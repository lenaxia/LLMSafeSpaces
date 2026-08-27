// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metering

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	channelCapacity   = 4096
	batchSize         = 100
	flushInterval     = 1 * time.Second
	maxDLQRetries     = 5
	dlqReaperInterval = 60 * time.Second
	insertTimeout     = 5 * time.Second
	// exportMaxAttempts bounds the same-cycle retry of failed billing
	// export groups (#758). Idempotency keys bind the export window, so
	// a same-window retry is safe at Stripe's idempotency layer.
	exportMaxAttempts = 3
	exportRetryDelay  = 200 * time.Millisecond
)

var (
	metricEventsRecorded = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_events_recorded_total",
			Help: "Total usage events written to DB",
		},
		[]string{"event_type", "source"},
	)
	metricEventsFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_events_failed_total",
			Help: "Total usage events that failed to write to DB",
		},
		[]string{"event_type", "error_type"},
	)
	metricEventsDropped = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_events_dropped_total",
			Help: "Total usage events dropped (buffer full or DLQ also failed)",
		},
	)
	metricBatchDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_metering_batch_write_duration_seconds",
			Help:    "DB batch flush latency",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		},
	)
	metricDLQSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_metering_dlq_size",
			Help: "Current number of unresolved DLQ entries",
		},
	)
	metricDLQDead = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_dlq_dead_total",
			Help: "Total DLQ entries that exhausted retries",
		},
	)
	metricReconciliationCatchup = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_reconciliation_catchup_total",
			Help: "Total compute gap-fill events emitted by reconciliation",
		},
	)
	metricExportLag = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_metering_export_lag_seconds",
			Help: "Seconds since last successful export",
		},
	)
	metricExportFailures = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_export_failures_total",
			Help: "Total billing export cycles that failed and held the cursor (#758)",
		},
	)
)

type serviceMetrics struct {
	written atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64
}

type WorkspacePhaseChecker func() map[string]string

type WorkspaceBillingProvider interface {
	ListAllWorkspacesForBilling(ctx context.Context) ([]database.WorkspaceBillingRecord, error)
}

// UsageReporter reports metered usage to an external billing provider (e.g.
// Stripe). nil means usage is not reported externally (dev/noop mode).
type UsageReporter interface {
	ReportUsage(ctx context.Context, events []billing.UsageExportEvent) ([]int64, error)
}

type Service struct {
	logger     pkginterfaces.LoggerInterface
	config     *config.Config
	db         *sql.DB
	dbSvc      interfaces.DatabaseService
	billingSvc WorkspaceBillingProvider
	usageRpt   UsageReporter

	// billCtx is the parent for direct billing writes (buffer-full and
	// shutdown-race fallbacks). Never canceled: these writes must
	// outlive request and shutdown contexts (#766).
	billCtx context.Context

	ch       chan UsageEvent
	done     chan struct{}
	stopCtx  context.Context
	stopFn   context.CancelFunc
	stopOnce sync.Once
	closed   atomic.Bool

	activePhases WorkspacePhaseChecker

	metrics serviceMetrics
}

func New(cfg *config.Config, log pkginterfaces.LoggerInterface, db *sql.DB) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	return &Service{
		logger:  log.With("component", "metering"),
		config:  cfg,
		db:      db,
		billCtx: context.Background(),
		ch:      make(chan UsageEvent, channelCapacity),
		done:    make(chan struct{}),
		stopCtx: ctx,
		stopFn:  cancel,
	}, nil
}

func (s *Service) SetActivePhasesChecker(fn WorkspacePhaseChecker) {
	s.activePhases = fn
}

func (s *Service) SetDatabaseService(dbSvc interfaces.DatabaseService) {
	s.dbSvc = dbSvc
}

func (s *Service) SetBillingProvider(p WorkspaceBillingProvider) {
	s.billingSvc = p
}

func (s *Service) SetUsageReporter(r UsageReporter) {
	s.usageRpt = r
}

func (s *Service) Start() error {
	s.logger.Info("Starting metering service")
	go s.run()
	go s.dlqReaperLoop()
	go s.computeReconciliationLoop()
	go s.storageMeteringLoop()
	go s.exportLoop()
	s.logger.Info("Metering service started")
	return nil
}

func (s *Service) Stop() error {
	s.stopOnce.Do(func() {
		s.logger.Info("Stopping metering service",
			"written", s.metrics.written.Load(),
			"failed", s.metrics.failed.Load(),
			"dropped", s.metrics.dropped.Load(),
		)
		s.closed.Store(true)
		close(s.ch)
		<-s.done
		s.stopFn()
		s.logger.Info("Metering service stopped")
	})
	return nil
}

// Record enqueues a usage event for async batch write. Every path is
// lossless (#766): a full buffer falls back to a synchronous direct
// write; events racing Stop's channel close (the send panics) are
// salvaged by the same direct write inside the recover handler; events
// arriving after Stop are also written directly.
func (s *Service) Record(event UsageEvent) {
	if event.EventType == "" {
		return
	}
	if s.closed.Load() {
		s.recordDirect(event)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// Send on a closed channel: Stop raced this Record. The
			// event never entered the buffer — write it directly.
			s.recordDirect(event)
		}
	}()
	select {
	case s.ch <- event:
	default:
		s.recordDirect(event)
	}
}

// recordDirect writes one event synchronously with its own bounded
// timeout — the lossless back-pressure path for buffer-full, stop-race,
// and post-stop events. A DB failure here is counted as a drop and
// surfaced (never silent).
func (s *Service) recordDirect(event UsageEvent) {
	// billCtx is a dedicated base context for direct billing writes: a
	// billing write must not inherit (and die with) a caller's cancellable
	// request context, and must keep working during shutdown (post-stop
	// and stop-race events) unlike stopCtx.
	ctx, cancel := context.WithTimeout(s.billCtx, insertTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, insertEventSQL, s.eventArgs(event)...); err != nil {
		s.metrics.dropped.Add(1)
		metricEventsDropped.Inc()
		s.logger.Error("Metering direct write failed, dropping event", err,
			"event_type", event.EventType,
			"workspace_id", event.WorkspaceID,
		)
		return
	}
	s.metrics.written.Add(1)
	metricEventsRecorded.WithLabelValues(event.EventType, event.Source).Inc()
}

func (s *Service) RecordLifecycleEvent(ctx context.Context, workspaceID, ownerID string, ownerType OwnerType, fromPhase, toPhase, resourceTier string, eventTime time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_lifecycle_events (workspace_id, owner_id, owner_type, from_phase, to_phase, resource_tier, event_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		ownerID,
		string(ownerType),
		fromPhase,
		toPhase,
		resourceTier,
		eventTime,
	)
	if err != nil {
		s.logger.Error("Failed to record lifecycle event", err,
			"workspace_id", workspaceID,
			"from_phase", fromPhase,
			"to_phase", toPhase,
		)
		return fmt.Errorf("record lifecycle event: %w", err)
	}
	return nil
}

func (s *Service) GetUsage(ctx context.Context, owner BillingOwner, from, to time.Time) (*UsageReport, error) {
	report := &UsageReport{
		OwnerID:     owner.ID,
		OwnerType:   owner.Type,
		PeriodFrom:  from,
		PeriodTo:    to,
		Totals:      make(map[string]int64),
		ByWorkspace: make(map[string]map[string]int64),
		ByDay:       make(map[string]map[string]int64),
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT event_type, COALESCE(workspace_id, ''), DATE(event_time) as day, SUM(quantity)
		FROM usage_events
		WHERE owner_id = $1 AND owner_type = $2 AND event_time >= $3 AND event_time < $4
		GROUP BY event_type, workspace_id, DATE(event_time)`,
		owner.ID, string(owner.Type), from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var eventType, wsID string
		var day time.Time
		var total int64
		if err := rows.Scan(&eventType, &wsID, &day, &total); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}
		report.Totals[eventType] += total
		dayStr := day.Format("2006-01-02")
		if report.ByDay[dayStr] == nil {
			report.ByDay[dayStr] = make(map[string]int64)
		}
		report.ByDay[dayStr][eventType] += total
		if wsID != "" {
			if report.ByWorkspace[wsID] == nil {
				report.ByWorkspace[wsID] = make(map[string]int64)
			}
			report.ByWorkspace[wsID][eventType] += total
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return report, nil
}

func (s *Service) GetUsageByWorkspace(ctx context.Context, owner BillingOwner, workspaceID string, from, to time.Time) (*UsageReport, error) {
	report := &UsageReport{
		OwnerID:     owner.ID,
		OwnerType:   owner.Type,
		PeriodFrom:  from,
		PeriodTo:    to,
		Totals:      make(map[string]int64),
		ByWorkspace: make(map[string]map[string]int64),
		ByDay:       make(map[string]map[string]int64),
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT event_type, DATE(event_time) as day, SUM(quantity)
		FROM usage_events
		WHERE owner_id = $1 AND owner_type = $2 AND workspace_id = $3 AND event_time >= $4 AND event_time < $5
		GROUP BY event_type, DATE(event_time)`,
		owner.ID, string(owner.Type), workspaceID, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query workspace usage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var eventType string
		var day time.Time
		var total int64
		if err := rows.Scan(&eventType, &day, &total); err != nil {
			return nil, fmt.Errorf("scan workspace usage: %w", err)
		}
		report.Totals[eventType] += total
		dayStr := day.Format("2006-01-02")
		if report.ByDay[dayStr] == nil {
			report.ByDay[dayStr] = make(map[string]int64)
		}
		report.ByDay[dayStr][eventType] += total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return report, nil
}

func (s *Service) GetQuotaStatus(ctx context.Context, owner BillingOwner) ([]QuotaStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT event_type, period_type, max_quantity FROM usage_limits
		WHERE owner_id = $1 AND owner_type = $2`, owner.ID, string(owner.Type))
	if err != nil {
		return nil, fmt.Errorf("query quota limits: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var statuses []QuotaStatus
	for rows.Next() {
		var qs QuotaStatus
		if err := rows.Scan(&qs.EventType, &qs.PeriodType, &qs.Limit); err != nil {
			return nil, fmt.Errorf("scan quota limit: %w", err)
		}
		var qerr error
		qs.Used, qs.Remaining, qs.ResetsAt, _, qerr = s.getQuotaUsage(ctx, owner, qs.EventType, qs.PeriodType, qs.Limit)
		if qerr != nil {
			return nil, qerr
		}
		statuses = append(statuses, qs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return statuses, nil
}

func (s *Service) getQuotaUsage(ctx context.Context, owner BillingOwner, eventType, periodType string, limit int64) (used, remaining int64, resetsAt time.Time, periodKey string, err error) {
	now := time.Now().UTC()
	var query string
	var args []interface{}
	switch periodType {
	case "daily":
		periodKey = now.Format("2006-01-02")
		resetsAt = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		query = `SELECT COALESCE(SUM(quantity), 0) FROM usage_events
		 WHERE owner_id = $1 AND owner_type = $2 AND event_type = $3
		 AND period = $4`
		args = []interface{}{owner.ID, string(owner.Type), eventType, periodKey}
	case "monthly":
		periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		periodKey = periodStart.Format("2006-01-02")
		resetsAt = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		query = `SELECT COALESCE(SUM(quantity), 0) FROM usage_events
		 WHERE owner_id = $1 AND owner_type = $2 AND event_type = $3
		 AND period >= $4 AND period < $5`
		args = []interface{}{owner.ID, string(owner.Type), eventType, periodStart, resetsAt}
	default:
		periodKey = "lifetime"
		resetsAt = time.Time{}
		query = `SELECT COALESCE(SUM(quantity), 0) FROM usage_events
		 WHERE owner_id = $1 AND owner_type = $2 AND event_type = $3`
		args = []interface{}{owner.ID, string(owner.Type), eventType}
	}

	if qerr := s.db.QueryRowContext(ctx, query, args...).Scan(&used); qerr != nil {
		s.logger.Error("Failed to query quota usage", qerr,
			"owner_id", owner.ID,
			"event_type", eventType,
			"period_type", periodType,
		)
		return 0, limit, resetsAt, periodKey, fmt.Errorf("query quota usage: %w", qerr)
	}

	remaining = limit - used
	if remaining < 0 {
		remaining = 0
	}
	return used, remaining, resetsAt, periodKey, nil
}

func (s *Service) CheckQuota(ctx context.Context, owner BillingOwner, eventType string) (bool, int64, error) {
	var maxQty int64
	var periodType string
	err := s.db.QueryRowContext(ctx, `
		SELECT max_quantity, period_type FROM usage_limits
		WHERE owner_id = $1 AND owner_type = $2 AND event_type = $3
		ORDER BY period_type LIMIT 1`, owner.ID, string(owner.Type), eventType,
	).Scan(&maxQty, &periodType)
	if err == sql.ErrNoRows {
		return true, 0, nil
	}
	if err != nil {
		return true, 0, fmt.Errorf("check quota: %w", err)
	}

	_, remaining, _, _, qerr := s.getQuotaUsage(ctx, owner, eventType, periodType, maxQty)
	if qerr != nil {
		return true, 0, fmt.Errorf("check quota: %w", qerr)
	}
	return remaining > 0, remaining, nil
}

func (s *Service) ExportUsage(ctx context.Context) (int, error) {
	var lastID int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(last_exported_id, 0) FROM billing_export_cursor WHERE provider = 'noop'`).Scan(&lastID)
	if err == sql.ErrNoRows {
		lastID = 0
	} else if err != nil {
		return 0, fmt.Errorf("get export cursor: %w", err)
	}

	var maxID int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_events WHERE id > $1`, lastID).Scan(&maxID)
	if err != nil {
		return 0, fmt.Errorf("get max event id: %w", err)
	}
	if maxID <= lastID {
		return 0, nil
	}

	// US-43.17: If a usage reporter is configured (StripeProvider), aggregate
	// usage events by customer + event type and report to Stripe Metered.
	//
	// #758: a reporter failure HOLDS the cursor and surfaces the error.
	// The next export cycle retries the same window with identical
	// idempotency keys — already-reported groups dedupe at Stripe's
	// idempotency layer, only the failed groups consume new records.
	// Advancing the cursor here was permanent revenue loss for the
	// failed window. A persistent outage stalls the export pointer, not
	// usage_events growth (the table is the source of truth either way).
	if s.usageRpt != nil {
		if err := s.exportToStripe(ctx, lastID, maxID); err != nil {
			metricExportFailures.Inc()
			metricExportLag.Inc()
			s.logger.Error("Stripe usage export failed, cursor held", err,
				"cursor", lastID,
				"window_max", maxID,
			)
			return 0, fmt.Errorf("stripe export failed, cursor held at %d: %w", lastID, err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO billing_export_cursor (provider, last_exported_id, last_exported_at)
		VALUES ('noop', $1, NOW())
		ON CONFLICT (provider) DO UPDATE SET last_exported_id = $1, last_exported_at = NOW()`, maxID)
	if err != nil {
		return 0, fmt.Errorf("update export cursor: %w", err)
	}

	exported := int(maxID - lastID)
	return exported, nil
}

// exportToStripe aggregates usage events between lastID and maxID by customer +
// event type, resolves the Stripe customer id from billing_accounts, and
// reports the totals to Stripe Metered Billing. Each aggregation group gets a
// deterministic idempotency key binding the full [lastID, maxID] window so
// same-window retries (within this cycle, or the next cycle after a held
// cursor) do not double-report. Failed groups retry up to exportMaxAttempts;
// groups that already reported successfully are not re-attempted (#758).
//
// Usage events are recorded per-user (owner_type='user'); billing accounts are
// per-org (owner_type='org'). The JOIN resolves user → org via org_memberships,
// picking the user's earliest membership deterministically (a user in multiple
// orgs is billed to their first-joined org). Users without an org membership
// produce no billing rows (external_customer_id=”) and are skipped.
func (s *Service) exportToStripe(ctx context.Context, lastID, maxID int64) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ue.event_type, SUM(ue.quantity), MAX(ue.event_time),
		       COALESCE(ba.external_customer_id, '')
		FROM usage_events ue
		LEFT JOIN LATERAL (
			SELECT om.org_id
			FROM org_memberships om
			WHERE om.user_id = ue.owner_id
			ORDER BY om.created_at ASC
			LIMIT 1
		) primary_org ON true
		LEFT JOIN billing_accounts ba
		  ON ba.owner_id = primary_org.org_id
		 AND ba.owner_type = 'org'
		 AND ba.provider = 'stripe'
		WHERE ue.id > $1 AND ue.id <= $2
		GROUP BY ue.event_type, ba.external_customer_id
	`, lastID, maxID)
	if err != nil {
		return fmt.Errorf("query usage events for export: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var events []billing.UsageExportEvent
	for rows.Next() {
		var e billing.UsageExportEvent
		var timestamp string
		if err := rows.Scan(&e.EventType, &e.Quantity, &timestamp, &e.ExternalCustomerID); err != nil {
			return fmt.Errorf("scan usage event for export: %w", err)
		}
		e.Timestamp = timestamp
		if e.ExternalCustomerID == "" {
			continue
		}
		// Deterministic per customer+type+window: BOTH bounds make the key
		// stable across same-window retries and distinct across windows.
		e.IdempotencyKey = fmt.Sprintf("meter-%s-%s-%d-%d", e.ExternalCustomerID, e.EventType, lastID, maxID)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage events for export: %w", err)
	}

	if len(events) == 0 {
		return nil
	}

	return s.reportWithRetry(ctx, events)
}

// reportWithRetry reports export groups, consuming ReportUsage's
// per-group success markers: only groups after the first failure are
// retried, with unchanged idempotency keys, so a partial Stripe outage
// costs at most exportMaxAttempts calls per still-failing group and
// never re-reports a succeeded one.
func (s *Service) reportWithRetry(ctx context.Context, events []billing.UsageExportEvent) error {
	pending := events
	var lastErr error
	for attempt := 1; attempt <= exportMaxAttempts; attempt++ {
		if len(pending) == 0 {
			return nil
		}
		ids, err := s.usageRpt.ReportUsage(ctx, pending)
		if err == nil {
			return nil
		}
		lastErr = err
		// ids covers every group that no longer needs reporting: the
		// successfully reported prefix plus any skipped-no-meter entries
		// before the first failure. A nil ids (reporter failed before
		// any progress) retries the whole set.
		if len(ids) < len(pending) {
			pending = pending[len(ids):]
		}
		s.logger.Warn("Billing export partial failure, retrying failed groups", err,
			"reported_so_far", len(events)-len(pending),
			"pending", len(pending),
			"attempt", attempt,
		)
		if attempt < exportMaxAttempts {
			select {
			case <-time.After(exportRetryDelay):
			case <-ctx.Done():
				return fmt.Errorf("report usage to billing provider: %w", lastErr)
			}
		}
	}
	return fmt.Errorf("report usage to billing provider after %d attempts: %w", exportMaxAttempts, lastErr)
}

func (s *Service) run() {
	defer close(s.done)

	batch := make([]UsageEvent, 0, batchSize)
	flushTimer := time.NewTimer(flushInterval)
	defer flushTimer.Stop()

	for {
		select {
		case event, ok := <-s.ch:
			if !ok {
				if len(batch) > 0 {
					s.flushBatch(batch)
				}
				return
			}
			batch = append(batch, event)
			if len(batch) >= batchSize {
				s.flushBatch(batch)
				batch = batch[:0]
				if !flushTimer.Stop() {
					select {
					case <-flushTimer.C:
					default:
					}
				}
				flushTimer.Reset(flushInterval)
			}
		case <-flushTimer.C:
			if len(batch) > 0 {
				s.flushBatch(batch)
				batch = batch[:0]
			}
			flushTimer.Reset(flushInterval)
		}
	}
}

const insertEventSQL = `INSERT INTO usage_events (idempotency_key, owner_id, owner_type, actor_id, workspace_id,
	event_type, event_subtype, quantity, resource_tier, region, metadata, request_context,
	source, event_time, period)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (idempotency_key) DO NOTHING`

func (s *Service) flushBatch(events []UsageEvent) {
	if len(events) == 0 {
		return
	}

	start := time.Now()

	ctx, cancel := context.WithTimeout(s.stopCtx, insertTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.handleFlushFailure(events, fmt.Errorf("begin tx: %w", err))
		return
	}

	for _, evt := range events {
		args := s.eventArgs(evt)
		_, err := tx.ExecContext(ctx, insertEventSQL, args...)
		if err != nil {
			_ = tx.Rollback()
			s.handleFlushFailure(events, fmt.Errorf("exec insert: %w", err))
			return
		}
	}

	if err := tx.Commit(); err != nil {
		s.handleFlushFailure(events, fmt.Errorf("commit: %w", err))
		return
	}

	duration := time.Since(start)
	metricBatchDuration.Observe(duration.Seconds())

	for _, evt := range events {
		s.metrics.written.Add(1)
		metricEventsRecorded.WithLabelValues(evt.EventType, evt.Source).Inc()
	}
}

func (s *Service) handleFlushFailure(events []UsageEvent, flushErr error) {
	for _, evt := range events {
		s.metrics.failed.Add(1)
		metricEventsFailed.WithLabelValues(evt.EventType, "batch_write").Inc()
	}

	s.logger.Error("Metering batch write failed, attempting DLQ", flushErr,
		"event_count", len(events),
	)

	ctx, cancel := context.WithTimeout(s.stopCtx, insertTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logger.Error("DLQ write also failed: begin tx", err)
		s.dropAll(events)
		return
	}

	const dlqInsertSQL = `INSERT INTO usage_events_dlq (payload, error_message) VALUES ($1, $2)`

	for _, evt := range events {
		payload, _ := json.Marshal(evt)
		if _, err := tx.ExecContext(ctx, dlqInsertSQL, payload, flushErr.Error()); err != nil {
			_ = tx.Rollback()
			s.logger.Error("DLQ write also failed: exec", err)
			s.dropAll(events)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		s.logger.Error("DLQ write also failed: commit", err)
		s.dropAll(events)
	}
}

func (s *Service) dropAll(events []UsageEvent) {
	count := uint64(len(events))
	s.metrics.dropped.Add(count)
	for i := 0; i < len(events); i++ {
		metricEventsDropped.Inc()
	}
}

func (s *Service) dlqReaperLoop() {
	ticker := time.NewTicker(dlqReaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.reapDLQ(ctx); err != nil {
				s.logger.Error("DLQ reaper run failed", err)
			}
			cancel()
		}
	}
}

func (s *Service) reapDLQ(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, payload, error_message, retry_count, first_failed_at, last_retried_at, resolved_at, resolution
		FROM usage_events_dlq
		WHERE resolved_at IS NULL AND retry_count < $1
		ORDER BY last_retried_at ASC NULLS FIRST LIMIT 50`, maxDLQRetries)
	if err != nil {
		return fmt.Errorf("query DLQ: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var entry DLQEntry
		if err := rows.Scan(
			&entry.ID, &entry.Payload, &entry.ErrorMessage, &entry.RetryCount,
			&entry.FirstFailedAt, &entry.LastRetriedAt, &entry.ResolvedAt, &entry.Resolution,
		); err != nil {
			s.logger.Error("DLQ scan error", err)
			continue
		}

		var evt UsageEvent
		if err := json.Unmarshal(entry.Payload, &evt); err != nil {
			s.logger.Error("DLQ unmarshal error", err, "dlq_id", entry.ID)
			continue
		}

		insertErr := s.insertSingleEvent(ctx, evt)
		if insertErr == nil {
			_, err := s.db.ExecContext(ctx,
				`UPDATE usage_events_dlq SET resolved_at=NOW(), resolution='reprocessed' WHERE id=$1`,
				entry.ID,
			)
			if err != nil {
				s.logger.Error("DLQ mark reprocessed failed", err, "dlq_id", entry.ID)
			}
			s.metrics.written.Add(1)
			metricEventsRecorded.WithLabelValues(evt.EventType, evt.Source).Inc()
		} else if entry.RetryCount+1 >= maxDLQRetries {
			_, err := s.db.ExecContext(ctx,
				`UPDATE usage_events_dlq SET retry_count=$1, last_retried_at=NOW(), resolution='dead' WHERE id=$2`,
				entry.RetryCount+1, entry.ID,
			)
			if err != nil {
				s.logger.Error("DLQ mark dead failed", err, "dlq_id", entry.ID)
			}
			metricDLQDead.Inc()
		} else {
			_, err := s.db.ExecContext(ctx,
				`UPDATE usage_events_dlq SET retry_count=retry_count+1, last_retried_at=NOW() WHERE id=$1`,
				entry.ID,
			)
			if err != nil {
				s.logger.Error("DLQ increment retry failed", err, "dlq_id", entry.ID)
			}
		}
	}

	if err := rows.Err(); err != nil {
		s.logger.Error("DLQ reaper rows iteration error", err)
	}

	s.updateDLQGauge(ctx)
	return nil
}

func (s *Service) insertSingleEvent(ctx context.Context, evt UsageEvent) error {
	args := s.eventArgs(evt)
	_, err := s.db.ExecContext(ctx, insertEventSQL, args...)
	return err
}

func (s *Service) eventArgs(evt UsageEvent) []interface{} {
	metadata, _ := json.Marshal(evt.Metadata)
	requestCtx, _ := json.Marshal(evt.RequestContext)
	var idempotencyKey interface{} = evt.IdempotencyKey
	if evt.IdempotencyKey == "" {
		idempotencyKey = nil
	}
	return []interface{}{
		idempotencyKey,
		evt.Owner.ID,
		string(evt.Owner.Type),
		evt.ActorID,
		evt.WorkspaceID,
		evt.EventType,
		evt.EventSubtype,
		evt.Quantity,
		evt.ResourceTier,
		evt.Region,
		metadata,
		requestCtx,
		evt.Source,
		evt.EventTime,
		evt.EventTime,
	}
}

func (s *Service) updateDLQGauge(ctx context.Context) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_events_dlq WHERE resolved_at IS NULL`,
	).Scan(&count)
	if err != nil {
		s.logger.Error("DLQ gauge query failed", err)
		return
	}
	metricDLQSize.Set(float64(count))
}

func (s *Service) computeReconciliationLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.reconcileComputeTime(ctx); err != nil {
				s.logger.Error("Compute time reconciliation failed", err)
			}
			cancel()
		}
	}
}

func (s *Service) reconcileComputeTime(ctx context.Context) error {
	if s.activePhases == nil || s.dbSvc == nil {
		return nil
	}

	activePhases := s.activePhases()
	if len(activePhases) == 0 {
		return nil
	}

	owners, err := s.dbSvc.ListAllWorkspaceOwners(ctx)
	if err != nil {
		return fmt.Errorf("list workspace owners: %w", err)
	}

	now := time.Now()
	var totalEmitted int

	for workspaceID, phase := range activePhases {
		if phase != "Active" {
			continue
		}
		userID, exists := owners[workspaceID]
		if !exists {
			continue
		}

		enteredActiveAt, err := s.getLastActiveTransition(ctx, workspaceID)
		if err != nil {
			s.logger.Error("Failed to get last active transition", err, "workspace_id", workspaceID)
			continue
		}
		if enteredActiveAt.IsZero() {
			continue
		}

		lastComputeTime, err := s.getLastComputeEvent(ctx, workspaceID)
		if err != nil {
			s.logger.Error("Failed to get last compute event", err, "workspace_id", workspaceID)
			continue
		}

		startTime := enteredActiveAt
		if lastComputeTime.After(startTime) {
			startTime = lastComputeTime
		}

		gap := now.Sub(startTime)
		if gap < 45*time.Second {
			continue
		}

		// nolint:contextcheck // Record's interface carries no ctx; its
		// direct-write fallback derives from the service's billCtx.
		emitted := s.emitComputeBuckets(workspaceID, userID, startTime, now)
		totalEmitted += emitted
	}

	if totalEmitted > 0 {
		metricReconciliationCatchup.Add(float64(totalEmitted))
		s.logger.Info("Compute time reconciliation completed",
			"events_emitted", totalEmitted,
		)
	}

	return nil
}

func (s *Service) getLastActiveTransition(ctx context.Context, workspaceID string) (time.Time, error) {
	var eventTime time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT event_time FROM workspace_lifecycle_events
		 WHERE workspace_id = $1 AND to_phase = 'Active'
		 ORDER BY event_time DESC LIMIT 1`, workspaceID,
	).Scan(&eventTime)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return eventTime, err
}

func (s *Service) getLastComputeEvent(ctx context.Context, workspaceID string) (time.Time, error) {
	var eventTime time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT event_time FROM usage_events
		 WHERE workspace_id = $1 AND event_type = 'compute_seconds'
		 ORDER BY event_time DESC LIMIT 1`, workspaceID,
	).Scan(&eventTime)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return eventTime, err
}

func (s *Service) emitComputeBuckets(workspaceID, ownerID string, startTime, now time.Time) int {
	alignedStart := startTime.Truncate(15 * time.Second)
	if alignedStart.Before(startTime) {
		alignedStart = alignedStart.Add(15 * time.Second)
	}

	var count int
	for t := alignedStart; t.Before(now); t = t.Add(15 * time.Second) {
		end := t.Add(15 * time.Second)
		if end.After(now) {
			end = now
		}
		quantity := int64(end.Sub(t).Seconds())
		if quantity <= 0 {
			continue
		}

		s.Record(UsageEvent{
			IdempotencyKey: fmt.Sprintf("compute:%s:%d", workspaceID, t.Unix()),
			Owner:          BillingOwner{ID: ownerID, Type: OwnerTypeUser},
			ActorID:        ownerID,
			WorkspaceID:    workspaceID,
			EventType:      "compute_seconds",
			EventSubtype:   "active",
			Quantity:       quantity,
			Source:         "reconciliation",
			EventTime:      t,
		})
		count++
	}
	return count
}

func (s *Service) storageMeteringLoop() {
	for {
		next := nextMidnightUTC()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.stopCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(s.stopCtx, 30*time.Second)
			if err := s.recordStorageBytes(ctx); err != nil {
				s.logger.Error("Storage metering failed", err)
			}
			cancel()
		}
	}
}

func (s *Service) recordStorageBytes(ctx context.Context) error {
	if s.billingSvc == nil {
		return nil
	}
	records, err := s.billingSvc.ListAllWorkspacesForBilling(ctx)
	if err != nil {
		return fmt.Errorf("list workspaces for billing: %w", err)
	}
	now := time.Now()
	dateStr := now.Format("2006-01-02")
	for _, r := range records {
		bytes, perr := parseStorageSize(r.StorageSize)
		if perr != nil {
			s.logger.Warn("Skipping workspace with unparseable storage size",
				"workspace_id", r.ID,
				"storage_size", r.StorageSize,
			)
			continue
		}
		if bytes <= 0 {
			continue
		}
		// nolint:contextcheck // Record's interface carries no ctx; its
		// direct-write fallback derives from the service's billCtx.
		s.Record(UsageEvent{
			IdempotencyKey: fmt.Sprintf("storage:%s:%s", r.ID, dateStr),
			Owner:          BillingOwner{ID: r.UserID, Type: OwnerTypeUser},
			ActorID:        r.UserID,
			WorkspaceID:    r.ID,
			EventType:      "storage_bytes",
			EventSubtype:   "pvc",
			Quantity:       bytes,
			Source:         "cron",
			EventTime:      now,
		})
	}
	return nil
}

func parseStorageSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var value float64
	var unit string
	n, err := fmt.Sscanf(s, "%f%s", &value, &unit)
	if n < 2 || err != nil {
		return 0, fmt.Errorf("unparseable storage size: %q", s)
	}
	switch unit {
	case "Ki":
		return int64(value * 1024), nil
	case "Mi":
		return int64(value * 1024 * 1024), nil
	case "Gi":
		return int64(value * 1024 * 1024 * 1024), nil
	case "Ti":
		return int64(value * 1024 * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown storage unit %q in size %q", unit, s)
	}
}

func nextMidnightUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
}

func (s *Service) exportLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := s.ExportUsage(ctx)
			cancel()
			if err != nil {
				s.logger.Error("Billing export failed", err)
			} else {
				// Only a successful export clears the lag gauge — a
				// failure resetting it to 0 would mask held exports.
				metricExportLag.Set(0)
			}
		}
	}
}

var _ interfaces.MeteringRecorder = (*Service)(nil)
