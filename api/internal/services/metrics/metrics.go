// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"fmt"
	"time"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

type Service struct {
	logger               pkginterfaces.LoggerInterface
	requestCounter       *prometheus.CounterVec
	requestDuration      *prometheus.HistogramVec
	responseSize         *prometheus.HistogramVec
	activeConnections    *prometheus.GaugeVec
	workspacesCreated    *prometheus.CounterVec
	workspacesTerminated *prometheus.CounterVec
	errorsTotal          *prometheus.CounterVec
	resourceUsage        *prometheus.GaugeVec
}

func New(log pkginterfaces.LoggerInterface) *Service {
	svc := &Service{
		logger: log.With("component", "metrics-service"),
	}

	svc.requestCounter = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "api_requests_total",
			Help: "Total number of API requests",
		},
		[]string{"method", "endpoint", "status"},
	)

	svc.requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "api_request_duration_seconds",
			Help:    "API request duration in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
		},
		[]string{"method", "endpoint"},
	)

	svc.responseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "api_response_size_bytes",
			Help:    "API response size in bytes",
			Buckets: prometheus.ExponentialBuckets(100, 10, 8),
		},
		[]string{"method", "endpoint"},
	)

	svc.activeConnections = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "api_active_connections",
			Help: "Number of active connections",
		},
		[]string{"type", "user_id"},
	)

	svc.workspacesCreated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workspaces_created_total",
			Help: "Total number of workspaces created",
		},
		[]string{"runtime", "user_id"},
	)

	svc.workspacesTerminated = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workspaces_terminated_total",
			Help: "Total number of workspaces terminated",
		},
		[]string{"runtime", "reason"},
	)

	svc.errorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "api_errors_total",
			Help: "Total number of API errors",
		},
		[]string{"type", "endpoint", "code"},
	)

	svc.resourceUsage = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workspace_resource_usage",
			Help: "Resource usage by workspaces",
		},
		[]string{"workspace_id", "resource_type"},
	)

	return svc
}

func (s *Service) Start() error {
	s.logger.Info("Starting metrics service")
	return nil
}

func (s *Service) Stop() error {
	s.logger.Info("Stopping metrics service")
	return nil
}

func (s *Service) RecordRequest(method, path string, status int, duration time.Duration, size int) {
	s.requestCounter.WithLabelValues(method, path, fmt.Sprintf("%d", status)).Inc()
	s.requestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
	s.responseSize.WithLabelValues(method, path).Observe(float64(size))
}

func (s *Service) RecordWorkspaceCreation(runtime, userID string) {
	s.workspacesCreated.WithLabelValues(runtime, userID).Inc()
}

func (s *Service) RecordWorkspaceTermination(runtime, reason string) {
	s.workspacesTerminated.WithLabelValues(runtime, reason).Inc()
}

func (s *Service) RecordError(errorType, endpoint, code string) {
	s.errorsTotal.WithLabelValues(errorType, endpoint, code).Inc()
}

func (s *Service) RecordResourceUsage(workspaceID string, cpu float64, memoryBytes int64) {
	s.resourceUsage.WithLabelValues(workspaceID, "cpu").Set(cpu)
	s.resourceUsage.WithLabelValues(workspaceID, "memory").Set(float64(memoryBytes))
}

func (s *Service) IncrementActiveConnections(connType, userID string) {
	s.activeConnections.WithLabelValues(connType, userID).Inc()
}

func (s *Service) DecrementActiveConnections(connType, userID string) {
	s.activeConnections.WithLabelValues(connType, userID).Dec()
}

// --- Epic 27b: Agent reload metrics ---

var (
	agentReloadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_agent_reload_total",
			Help: "Total agent reload operations",
		},
		[]string{"result", "drained"},
	)
	agentReloadDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_agent_reload_duration_ms",
			Help:    "Agent reload duration in milliseconds",
			Buckets: prometheus.ExponentialBuckets(100, 2, 12),
		},
		[]string{"drained"},
	)
	agentReloadDrainTimeouts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "llmsafespaces_agent_reload_drain_timeouts_total",
		Help: "Total drain timeout occurrences",
	})
	agentReloadBulkTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_agent_reload_bulk_total",
			Help: "Total bulk reload operations",
		},
		[]string{"outcome"},
	)
)

// RecordAgentReload records a reload operation result.
func (s *Service) RecordAgentReload(result string, durationMs int64, drained bool) {
	drainedStr := "false"
	if drained {
		drainedStr = "true"
	}
	agentReloadTotal.WithLabelValues(result, drainedStr).Inc()
	agentReloadDuration.WithLabelValues(drainedStr).Observe(float64(durationMs))
}

// RecordAgentReloadDrainTimeout records a drain timeout.
func (s *Service) RecordAgentReloadDrainTimeout(_ int64) {
	agentReloadDrainTimeouts.Inc()
}

// RecordAgentReloadBulk records a bulk reload operation.
func (s *Service) RecordAgentReloadBulk(total, succeeded, failed int) {
	outcome := "all_success"
	if failed > 0 && succeeded > 0 {
		outcome = "partial"
	} else if failed > 0 {
		outcome = "all_failed"
	}
	agentReloadBulkTotal.WithLabelValues(outcome).Inc()
}

// --- Billing, Metering and Operations Metrics (Epic 26+) ---

var (
	inferenceRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_inference_requests_total",
		Help: "Total inference requests (session.updated with output tokens).",
	}, []string{"model_id", "provider_id", "tier"})

	inferenceInputTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_inference_input_tokens_total",
		Help: "Total input tokens consumed.",
	}, []string{"model_id", "provider_id", "tier"})

	inferenceOutputTokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_inference_output_tokens_total",
		Help: "Total output tokens produced.",
	}, []string{"model_id", "provider_id", "tier"})

	inferenceCostDollarsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_inference_cost_dollars_total",
		Help: "Estimated inference cost in USD from opencode session metadata.",
	}, []string{"model_id", "provider_id", "tier"})

	modelSelectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_model_selections_total",
		Help: "Model selection events (PUT /model calls that succeeded).",
	}, []string{"model_id", "provider_id", "tier"})

	workspacePhaseTotalTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_workspace_phase_transitions_total",
		Help: "Workspace phase transitions observed by the API server CRD watcher.",
	}, []string{"from_phase", "to_phase"})
)

// RecordInference records a completed inference event at the fleet level.
// Labels are model_id, provider_id, tier only — workspace_id is intentionally
// omitted to keep cardinality bounded (O(models × providers × tiers) ≈ 3k series
// vs O(workspaces × models × providers × tiers) ≈ 30M at scale).
//
// For per-user/per-workspace billing granularity, use the metering service
// (api/internal/services/metering) which writes to the usage_events table.
func (s *Service) RecordInference(modelID, providerID string, inputTokens, outputTokens int64, costDollars float64) {
	tier := "paid"
	if providerID == "opencode-relay" {
		tier = "free"
	}
	inferenceRequestsTotal.WithLabelValues(modelID, providerID, tier).Inc()
	if inputTokens > 0 {
		inferenceInputTokensTotal.WithLabelValues(modelID, providerID, tier).Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		inferenceOutputTokensTotal.WithLabelValues(modelID, providerID, tier).Add(float64(outputTokens))
	}
	if costDollars > 0 {
		inferenceCostDollarsTotal.WithLabelValues(modelID, providerID, tier).Add(costDollars)
	}
}

// RecordModelSelection records a model selection event.
func (s *Service) RecordModelSelection(modelID, providerID string) {
	tier := "paid"
	if providerID == "opencode-relay" {
		tier = "free"
	}
	modelSelectionsTotal.WithLabelValues(modelID, providerID, tier).Inc()
}

// RecordWorkspacePhaseTransition records a workspace phase change observed by
// the CRD watcher. from_phase is the previous phase; to_phase is the new phase.
func RecordWorkspacePhaseTransition(fromPhase, toPhase string) {
	workspacePhaseTotalTransitions.WithLabelValues(fromPhase, toPhase).Inc()
}

// --- Authentication + Session Metrics ---

var (
	// authFailuresTotal counts authentication failures by reason.
	// Security signal: detect brute-force, expired tokens, compromised keys.
	authFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_auth_failures_total",
		Help: "Authentication failures by reason (invalid_token, expired, wrong_password, revoked).",
	}, []string{"reason"})

	// sessionDurationSeconds tracks how long inference sessions run.
	// Metering signal: distribution of session lengths for capacity planning.
	sessionDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llmsafespaces_session_duration_seconds",
		Help:    "Duration of opencode sessions from first message to idle (seconds).",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800},
	}, []string{})
)

// RecordAuthFailure records an authentication failure with the given reason.
// reason values: "invalid_token", "expired_token", "wrong_password", "revoked_token",
// "missing_token", "insufficient_scope".
func RecordAuthFailure(reason string) {
	authFailuresTotal.WithLabelValues(reason).Inc()
}

// RecordSessionCompleted records a completed inference session.
// durationSeconds is the elapsed time from first message to idle status.
// RecordSessionCompleted records a completed inference session duration.
// workspace_id is omitted from the histogram to keep cardinality bounded
// (O(workspaces × 9 buckets) = 90k series at 10k workspaces).
// Fleet-level P50/P99 is sufficient for capacity planning.
func (s *Service) RecordSessionCompleted(_ string, durationSeconds float64) {
	sessionDurationSeconds.WithLabelValues().Observe(durationSeconds)
}

// --- Dependency Health Metrics (US-12.12) ---

var (
	dbQueryDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_db_query_duration_seconds",
			Help:    "Database query latency by operation",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
		},
		[]string{"operation"},
	)
	dbErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_db_errors_total",
			Help: "Database errors by operation and error type",
		},
		[]string{"operation", "error_type"},
	)
	dbPoolActiveConns = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_db_pool_active_connections",
			Help: "In-use database pool connections",
		},
	)
	dbPoolIdleConns = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_db_pool_idle_connections",
			Help: "Idle database pool connections",
		},
	)
	dbPoolMaxConns = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_db_pool_max_connections",
			Help: "Maximum database pool connections",
		},
	)
	redisCommandDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_redis_command_duration_seconds",
			Help:    "Redis command latency",
			Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
		},
		[]string{"command"},
	)
	redisErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_redis_errors_total",
			Help: "Redis errors by command",
		},
		[]string{"command"},
	)
	authAttemptsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_auth_attempts_total",
			Help: "Authentication attempts by method and result",
		},
		[]string{"method", "result"},
	)
	authLockoutsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "llmsafespaces_auth_lockouts_total",
			Help: "Brute-force lockout triggers",
		},
	)
	dependencyUp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_dependency_up",
			Help: "Dependency health (1=up, 0=down)",
		},
		[]string{"dependency"},
	)
	serviceStartupDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmsafespaces_service_startup_duration_seconds",
			Help:    "Service startup duration",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
		[]string{"service"},
	)
	suspendedWorkspaces = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmsafespaces_workspaces_suspended_total",
			Help: "Currently suspended workspaces",
		},
	)
)

func RecordDBQueryDuration(operation string, d time.Duration) {
	dbQueryDuration.WithLabelValues(operation).Observe(d.Seconds())
}

func RecordDBError(operation, errorType string) {
	dbErrorsTotal.WithLabelValues(operation, errorType).Inc()
}

func RecordDBPoolStats(active, idle, max int) {
	dbPoolActiveConns.Set(float64(active))
	dbPoolIdleConns.Set(float64(idle))
	dbPoolMaxConns.Set(float64(max))
}

func RecordRedisCommandDuration(command string, d time.Duration) {
	redisCommandDuration.WithLabelValues(command).Observe(d.Seconds())
}

func RecordRedisError(command string) {
	redisErrorsTotal.WithLabelValues(command).Inc()
}

func RecordAuthAttempt(method, result string) {
	authAttemptsTotal.WithLabelValues(method, result).Inc()
}

func RecordAuthLockout() {
	authLockoutsTotal.Inc()
}

func RecordDependencyUp(dependency string, up bool) {
	v := 0.0
	if up {
		v = 1.0
	}
	dependencyUp.WithLabelValues(dependency).Set(v)
}

func RecordServiceStartupDuration(service string, d time.Duration) {
	serviceStartupDuration.WithLabelValues(service).Observe(d.Seconds())
}

func RecordSuspendedWorkspaceCount(count int) {
	suspendedWorkspaces.Set(float64(count))
}

// GatherDefault returns all metric families currently registered on the
// prometheus default registry. It exists so test code in sibling packages
// can assert on metrics without taking a direct prometheus dependency.
func GatherDefault() ([]*dto.MetricFamily, error) {
	return prometheus.DefaultGatherer.Gather()
}

var (
	quotaExceededTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmsafespaces_metering_quota_exceeded_total",
			Help: "Total quota enforcement triggers",
		},
		[]string{"event_type"},
	)
)

func RecordQuotaExceeded(eventType string) {
	quotaExceededTotal.WithLabelValues(eventType).Inc()
}

var (
	requestBufferTimeoutTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workspace_request_buffer_timeout_total",
			Help: "Total buffered requests that exceeded the buffer timeout",
		},
		[]string{"workspace_id"},
	)
	requestBufferWaitSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "workspace_request_buffer_wait_seconds",
			Help:    "Time buffered requests waited before being forwarded or timing out",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30},
		},
		[]string{"workspace_id"},
	)
	requestBufferSize = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "workspace_request_buffer_size",
			Help: "Current number of buffered requests per workspace",
		},
		[]string{"workspace_id"},
	)
	requestBufferFullTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workspace_request_buffer_full_total",
			Help: "Total requests rejected because the per-workspace buffer was full",
		},
		[]string{"workspace_id"},
	)
	// requestBufferGlobalBytes (C5) is the total body bytes currently held
	// across all workspaces' buffers. Single time series (no labels) — the
	// global cap is a per-replica budget. Alert when approaching
	// defaultGlobalBufferBytesCap (500MB).
	requestBufferGlobalBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "workspace_request_buffer_global_bytes",
		Help: "Total body bytes currently buffered across all workspaces (C5 global cap budget use)",
	})
	// requestBufferGlobalFullTotal (C5) counts requests rejected because the
	// global byte cap was reached (as opposed to the per-workspace cap).
	requestBufferGlobalFullTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "workspace_request_buffer_global_full_total",
			Help: "Total requests rejected because the global buffer byte cap was reached (C5)",
		},
		[]string{"workspace_id"},
	)

	// upstream5xxTotal counts 5xx responses returned by the upstream
	// opencode process for proxied requests. Emitted by both the streaming
	// proxy path (doProxy) and the non-streaming history path
	// (doHistoryRequest). Complements the api middleware's
	// api_requests_total{status} counter — that one records the API's
	// OUTBOUND status; this one records the UPSTREAM status. They differ
	// when the API wraps upstream errors (e.g. upstream 500 -> API 502)
	// or passes them through. See LLMSafeSpaces#488 for the incident this
	// exists to make debuggable: opencode returned 500 on all session
	// history reads due to a ConfigInvalidError, and there was no
	// server-side observability of that fact.
	//
	// The `path` label carries the OPENCODE-side path (e.g. `/session`,
	// `/session/:id/message`) with the session-ID replaced by `:id` so the
	// cardinality stays bounded. Callers must sanitize before passing.
	upstream5xxTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "api_upstream_5xx_total",
			Help: "Total upstream (opencode) 5xx responses observed by the proxy layer (LLMSafeSpaces#488)",
		},
		[]string{"workspace_id", "path", "upstream_status"},
	)
)

func RecordRequestBufferTimeout(workspaceID string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferTimeoutTotal.WithLabelValues(workspaceID).Inc()
}

func RecordRequestBufferFull(workspaceID string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferFullTotal.WithLabelValues(workspaceID).Inc()
}

func RecordRequestBufferWait(workspaceID string, d time.Duration) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferWaitSeconds.WithLabelValues(workspaceID).Observe(d.Seconds())
}

func SetRequestBufferSize(workspaceID string, n int) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferSize.WithLabelValues(workspaceID).Set(float64(n))
}

// DeleteRequestBufferMetrics removes the per-workspace gauge series for the
// request buffer when a workspace's queue drains. Only the gauge is cleaned
// up: the timeout/full counters and the wait histogram are cumulative across
// the workspace's lifetime, so deleting them on drain would under-count and
// lose history. The gauge is the only instantaneous (non-cumulative) metric
// and must be removed to prevent orphan-label cardinality growth as
// workspaces come and go.
func DeleteRequestBufferMetrics(workspaceID string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferSize.DeleteLabelValues(workspaceID)
}

// SetRequestBufferGlobalBytes (C5) reports the total body bytes currently
// buffered across all workspaces. Single time series (no labels).
func SetRequestBufferGlobalBytes(bytes int64) {
	requestBufferGlobalBytes.Set(float64(bytes))
}

// RecordRequestBufferGlobalFull (C5) increments the counter for requests
// rejected because the global byte cap was reached.
func RecordRequestBufferGlobalFull(workspaceID string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	requestBufferGlobalFullTotal.WithLabelValues(workspaceID).Inc()
}

// RecordUpstream5xx (LLMSafeSpaces#488) increments the counter for every
// upstream (opencode) 5xx response the proxy layer observes. Called by
// both doProxy (streaming proxy) and doHistoryRequest (non-streaming
// history fetch). See the counter definition for full rationale.
//
// path SHOULD carry the opencode-side path with the session-ID and any
// other high-cardinality segments replaced by placeholders (e.g. `:id`)
// before this is called — sanitizePathForMetric in
// api/internal/handlers/proxy_upstream_observability.go does that
// normalization for the current callers. workspaceID and status are
// labeled verbatim.
func RecordUpstream5xx(workspaceID, path, status string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	if path == "" {
		path = "unknown"
	}
	if status == "" {
		status = "0"
	}
	upstream5xxTotal.WithLabelValues(workspaceID, path, status).Inc()
}

// Upstream5xxCounter (LLMSafeSpaces#488) exposes the underlying
// CounterVec so tests can reset it between cases (Prometheus counters
// are process-global) and assert on labeled values. Not intended for
// production code paths — use RecordUpstream5xx.
func Upstream5xxCounter() *prometheus.CounterVec {
	return upstream5xxTotal
}

// --- worklog 0589 / #493: Pod-recreation auto-push metrics ---

var (
	// secretAutoPushTotal counts fire-and-forget push attempts triggered
	// by the workspace-status pod-identity detector. Outcome label
	// values are exhaustively enumerated: "success", "inject_failed",
	// "reload_failed", "no_pod". Operators alert on non-zero
	// {outcome!="success"} sustained rate.
	//
	// This is distinct from llmsafespaces_agent_reload_total (which
	// counts user-initiated dispose+restart via POST /agent/reload) —
	// the two paths have different observability requirements and
	// different SLOs. Combining them into one metric would prevent
	// operators from telling "user hit reload button 50x in an hour"
	// (support signal) from "50 workspaces auto-recovered after a node
	// failure" (infrastructure signal).
	secretAutoPushTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "api_secret_auto_push_total",
			Help: "Total auto-push attempts triggered by pod-identity transitions on workspace status polls.",
		},
		[]string{"outcome"},
	)
)

// RecordSecretAutoPush increments the counter for a single auto-push
// attempt outcome. outcome must be one of "success", "inject_failed",
// "reload_failed", "no_pod". Called from app.recordAutoPushOutcome
// (the process-wide callback used by wsAgentPusherAdapter, the sole
// emission point for this metric — see the adapter's doc for the
// rationale).
func RecordSecretAutoPush(outcome string) {
	if outcome == "" {
		outcome = "unknown"
	}
	secretAutoPushTotal.WithLabelValues(outcome).Inc()
}

// SecretAutoPushCounter exposes the underlying CounterVec so tests can
// reset it between cases and assert on labeled values.
func SecretAutoPushCounter() *prometheus.CounterVec {
	return secretAutoPushTotal
}
