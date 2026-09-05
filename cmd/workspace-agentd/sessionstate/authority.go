// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sessionstate is the pod-local session state authority (design
// 0055, Epic 69 US-69.2): a module-sealed subsystem that owns seq
// assignment (I1), the store-reseeded projection (I3/I4), the stamped
// snapshot + ordered event fanout (I2), the durable seq cursor on the
// platform/ PVC subPath (I9 prep), and the authenticated ABI surface
// (I8). It is dialect-free machinery: the harness event parser and store
// reader are injected seams (US-69.3 grows the opencode implementations).
//
// Sealing rules (enforced by TestModuleSealDependencies): no imports of
// agentd supervision or any cmd/ package; callers reach the authority only
// through this package's exported API.
package sessionstate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// ErrNoStore is returned by Reseed when no store reader is wired: the
// projection cannot be rebuilt from truth, so reseed fails loudly instead
// of silently trusting the (possibly stale) in-memory state.
var ErrNoStore = errors.New("sessionstate: no store reader wired")

// ReseedReason classifies why a projection reseed ran.
type ReseedReason int

const (
	ReseedReasonBoot ReseedReason = iota
	ReseedReasonGenerationChange
	// ReseedReasonStallWake (US-69.12): the stall watchdog reseeded
	// because an admitted row crossed the promotion deadline — the
	// store refresh is the wake (events completing the turn promote or
	// turn-end the row; a still-missing promotion escalates via the
	// stalled-entries/wake-failure alerts).
	ReseedReasonStallWake
)

func (r ReseedReason) proto() abiv1.ReseedReason {
	if r == ReseedReasonGenerationChange {
		return abiv1.ReseedReason_RESEED_REASON_GENERATION_CHANGE
	}
	return abiv1.ReseedReason_RESEED_REASON_BOOT
}

// EventParser translates one raw harness SSE payload into a contract
// event. ok=false means the payload carries nothing projectable (dropped,
// counted); err signals a decode failure of a payload that CLAIMED to be
// projectable (wire drift — logged, never fatal). Implementations must be
// safe for concurrent use.
type EventParser interface {
	Parse(raw []byte) (evt *abiv1.Event, ok bool, err error)
}

// StoreReader snapshots the harness store's session truth — the reseed
// source (I3/I4). Implementations must respect ctx deadlines; a hung store
// must surface as ctx error, never block forever.
type StoreReader interface {
	SessionStates(ctx context.Context) (map[string]SessionSeed, error)
}

// RateLimitConfig bounds per-session mutating-op rates (I8).
type RateLimitConfig struct {
	Burst        int
	RefillPerSec float64
}

const (
	defaultBurst        = 20
	defaultRefillPerSec = 5
)

// Config wires the authority. PlatformDir holds the durable seq cursor
// (platform/ PVC subPath in production; any dir in tests).
type Config struct {
	PlatformDir string
	Parser      EventParser
	Store       StoreReader
	Passwords   []string
	RateLimit   RateLimitConfig
	Logger      *zap.Logger
	// ABIVersion is reported in the capability report snapshot frame.
	ABIVersion string
	// Capabilities, when set, is the capability report served on the
	// snapshot frame (provenance, supported actions, supported part
	// kinds). Built by the wiring layer from boot-time facts (0053 pins,
	// versions, D3 part limits) — the report is STATIC per boot so the
	// hot path never touches the harness (M3.1).
	Capabilities *abiv1.CapabilityReport
	// Admitter (US-69.7) is the delivery-admission seam; nil DISABLES
	// the ledger (Deliver returns NotSupported) — the authority owns
	// nothing dialect-specific.
	Admitter Admitter
	// Actor (US-69.9) is the typed-actions seam; nil DISABLES the Act op
	// (NotSupported) — the wiring layer injects the opencode executor.
	Actor Actor
	// Wake (US-69.12) is the stall-recovery seam: agentd's best-effort
	// nudge when an admitted row crosses the promotion deadline
	// (checkStalls fires it once per row). Nil = no-op wake (stalls
	// still surface via metrics/alerts; wake failures count only when a
	// wake exists).
	Wake func(ctx context.Context, sessionID string) error
	// FastCursor disables per-event fsync (scenario harnesses replaying
	// event BURSTS; durability is covered by the fault-injection suite).
	FastCursor bool
}

// StateSnapshot is an atomically-stamped view of the projection (I1: the
// state is exactly the fold of events 1..Seq).
type StateSnapshot struct {
	Seq      uint64
	Sessions map[string]*SessionView
}

type subscriber struct {
	ch     chan *abiv1.StreamFrame
	cancel context.CancelFunc
}

// Authority is the module-sealed session state authority. All mutable
// state lives under mu: seq assignment, the projection, and fanout
// registration share one lock so the stamp is atomic by construction.
type Authority struct {
	cfg    Config
	logger *zap.Logger

	mu  sync.Mutex
	seq uint64
	// lastSeqAt is when the projection last advanced (the seq-stall
	// signal's clock, R5/US-69.12).
	lastSeqAt time.Time
	sessions  map[string]*sessionRecord
	subs      map[*subscriber]struct{}
	buffering bool
	pending   [][]byte

	reseedMu sync.Mutex
	cursor   *seqCursor
	limiter  *sessionLimiter

	ledger  *deliveryLedger
	deliver *deliveryDriver

	// sessionLocks is the per-session single-flight domain shared by
	// delivery admission and Act (US-69.9 sole-writer serialization:
	// actions serialize against in-flight delivery on the same lock).
	sessionLocks   map[string]*sync.Mutex
	sessionLocksMu sync.Mutex

	droppedEvents     int64
	parserFailures    int64
	panicsContained   int64 // atomic — parseContained runs under a.mu on the reseed flush path; a lock here deadlocks
	customValveEvents int64
}

// New constructs the authority and loads the durable seq cursor from
// PlatformDir. It does NOT reseed — callers run Reseed(ReseedBoot) once the
// store reader is reachable (boot ordering is the caller's decision).
func New(cfg Config) (*Authority, error) {
	if cfg.Parser == nil {
		return nil, errors.New("sessionstate: parser is required")
	}
	if len(cfg.Passwords) == 0 {
		return nil, errors.New("sessionstate: at least one auth password is required (I8: no unauthenticated routes)")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.RateLimit.Burst <= 0 {
		cfg.RateLimit.Burst = defaultBurst
	}
	if cfg.RateLimit.RefillPerSec <= 0 {
		cfg.RateLimit.RefillPerSec = defaultRefillPerSec
	}
	dir := cfg.PlatformDir
	if dir == "" {
		dir = DefaultPlatformDir
	}
	cursor, err := openSeqCursor(dir, cfg.FastCursor)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: seq cursor: %w", err)
	}
	var ledger *deliveryLedger
	var driver *deliveryDriver
	if cfg.Admitter != nil {
		ledger, err = openDeliveryLedger(filepath.Join(dir, "ledger.wal"))
		if err != nil {
			return nil, fmt.Errorf("sessionstate: ledger: %w", err)
		}
	}
	if cfg.ABIVersion == "" {
		cfg.ABIVersion = "1"
	}
	a := &Authority{
		cfg:          cfg,
		logger:       logger,
		seq:          cursor.last(),
		sessions:     map[string]*sessionRecord{},
		subs:         map[*subscriber]struct{}{},
		cursor:       cursor,
		limiter:      newSessionLimiter(cfg.RateLimit),
		ledger:       ledger,
		sessionLocks: map[string]*sync.Mutex{},
	}
	if cfg.Admitter != nil {
		// The driver joins the authority's per-session single-flight —
		// the SAME lock domain Act executes under (US-69.9: actions and
		// admissions serialize per session; M1 sole writer).
		driver = newDeliveryDriver(ledger, cfg.Admitter, cfg, a.sessionLock)
		a.deliver = driver
	}
	return a, nil
}

// DefaultPlatformDir is the platform/ PVC subPath mount (pod_builder).
// Overridable for tests via Config.PlatformDir.
const DefaultPlatformDir = "/platform"

// sessionLock returns the session's single-flight mutex — the sole-writer
// domain admissions (US-69.7) and actions (US-69.9) share.
func (a *Authority) sessionLock(sessionID string) *sync.Mutex {
	a.sessionLocksMu.Lock()
	defer a.sessionLocksMu.Unlock()
	m, ok := a.sessionLocks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		a.sessionLocks[sessionID] = m
	}
	return m
}

// Ingest applies one raw harness event. It is the recover wall: parser
// panics and decode failures are contained (counted + logged), never
// propagated — agentd must not die from projection input. During a reseed
// the payload is buffered instead of applied (I3 ordering).
func (a *Authority) Ingest(raw []byte) {
	evt, ok, err := a.parseContained(raw)
	if err != nil {
		// #1291 r5: a payload that CLAIMED projectability but failed to
		// decode returns (nil, true, err) from the parser — err governs,
		// NEVER ok: applyLocked(nil) was a live SIGSEGV on the production
		// hot path (properties-shape drift is the expected drift class).
		a.mu.Lock()
		a.parserFailures++
		a.mu.Unlock()
		a.logger.Warn("sessionstate: parser rejected payload claiming to be projectable", zap.Error(err))
		return
	}
	if !ok {
		a.mu.Lock()
		a.droppedEvents++
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	if a.buffering {
		a.pending = append(a.pending, raw)
		a.mu.Unlock()
		return
	}
	a.applyLocked(evt)
	a.mu.Unlock()
}

// parseContained runs the injected parser behind a recover wall.
// panicsContained increments WITHOUT a.mu: the reseed flush path calls
// parseContained while HOLDING a.mu (authority.go:352+), so taking the
// lock here deadlocks the flush. The counter stays atomic instead —
// read-side synchronization (Metrics) may read a torn instant on 32-bit
// builds; acceptable for a best-effort ops counter, and the deadlock is
// not.
func (a *Authority) parseContained(raw []byte) (evt *abiv1.Event, ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			atomic.AddInt64(&a.panicsContained, 1)
			a.logger.Error("sessionstate: parser panic contained by recover wall", zap.Any("panic", r))
			evt, ok, err = nil, false, nil
		}
	}()
	return a.cfg.Parser.Parse(raw)
}

// applyLocked assigns the next seq (fsync-persisting the cursor BEFORE
// publication — I1: a published seq is never reused after crash) and
// updates the projection + fanout. mu must be held.
func (a *Authority) applyLocked(evt *abiv1.Event) {
	next := a.seq + 1
	if err := a.cursor.persist(next); err != nil {
		// Never publish a seq that could be reused after a crash: a cursor
		// write failure drops the event instead (visible, counted) —
		// integrity over availability for ordering.
		a.logger.Error("sessionstate: seq cursor persist failed — dropping event", zap.Uint64("seq", next), zap.Error(err))
		return
	}
	a.seq = next
	a.lastSeqAt = time.Now()
	a.projectLocked(evt)
	frame := &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: next, Event: evt}}}
	a.fanoutLocked(frame)
}

func (a *Authority) projectLocked(evt *abiv1.Event) {
	a.applyContractLocked(evt)
	if a.deliver != nil {
		a.deliver.observeEvent(evt)
	}
}

func (a *Authority) fanoutLocked(frame *abiv1.StreamFrame) {
	for sub := range a.subs {
		select {
		case sub.ch <- frame:
		default:
			// M3.4 backpressure: a slow consumer is dropped — it resyncs
			// with a fresh stamped snapshot on reconnect.
			sub.cancel()
			delete(a.subs, sub)
		}
	}
}

// Reseed runs the ordered, race-free reseed procedure (design 0055 M1):
// quiesce → buffer inbound → reseed from the store → emit
// projection.reseeded (consuming a seq) → flush buffered → resume live.
// Serialized by reseedMu; the store read happens OUTSIDE mu so ingest and
// stream reads stay hot (M3.1).
func (a *Authority) Reseed(ctx context.Context, reason ReseedReason) error {
	a.reseedMu.Lock()
	defer a.reseedMu.Unlock()

	if a.cfg.Store == nil {
		return ErrNoStore
	}
	seeds, err := a.cfg.Store.SessionStates(ctx)
	if err != nil {
		return fmt.Errorf("sessionstate: store read during reseed: %w", err)
	}

	a.mu.Lock()
	a.buffering = true
	a.mu.Unlock()
	// quiesce point: everything ingested from here on buffers.

	flush := func() {
		a.mu.Lock()
		a.buffering = false
		pending := a.pending
		a.pending = nil
		for _, raw := range pending {
			if evt, ok, err := a.parseContained(raw); err != nil {
				a.parserFailures++
			} else if ok {
				a.applyLocked(evt)
			}
		}
		a.mu.Unlock()
	}

	a.mu.Lock()
	next := a.seq + 1
	if err := a.cursor.persist(next); err != nil {
		a.mu.Unlock()
		flush()
		return fmt.Errorf("sessionstate: reseed seq persist: %w", err)
	}
	a.seq = next
	a.lastSeqAt = time.Now()
	a.sessions = make(map[string]*sessionRecord, len(seeds))
	for id, seed := range seeds {
		a.sessions[id] = seedLocked(seed)
	}
	frame := &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Reseeded{Reseeded: &abiv1.ReseedNotice{Seq: next, Reason: reason.proto()}}}
	a.fanoutLocked(frame)
	a.mu.Unlock()

	flush()
	return nil
}

// State returns the atomically-stamped projection snapshot.
func (a *Authority) State() StateSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]*SessionView, len(a.sessions))
	for k, v := range a.sessions {
		out[k] = v.view()
	}
	return StateSnapshot{Seq: a.seq, Sessions: out}
}

// capabilityReportLocked builds the snapshot frame's capability report at
// the current story stage. US-69.4 replaces this with the full provenance
// wiring (0054 predicate); the frame shape is stable from day one.
// capabilityReport serves the wiring-provided static report (provenance,
// supported actions, supported part kinds — US-69.4); the fallback covers
// bare constructions (tests) with the D3 opencode part limits.
func (a *Authority) capabilityReport() *abiv1.CapabilityReport {
	if a.cfg.Capabilities != nil {
		return a.cfg.Capabilities
	}
	return &abiv1.CapabilityReport{
		Provenance:             abiv1.Provenance_PROVENANCE_PLATFORM_PINNED,
		SupportedActions:       nil,
		SupportedDeliveryParts: []abiv1.DeliveryPartKind{abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT},
		AbiVersion:             a.cfg.ABIVersion,
	}
}

// Stream subscribes to the ordered frame stream. The mandatory connection
// ordering (I2): (1) register the per-connection buffer, (2) capture the
// stamped snapshot under the projection lock, (3) flush buffered frames
// with seq > stamp, (4) go live. Canceling the returned func tears the
// subscription down.
func (a *Authority) Stream(ctx context.Context) (<-chan *abiv1.StreamFrame, func(), error) {
	subCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sub := &subscriber{ch: make(chan *abiv1.StreamFrame, streamBuffer), cancel: cancel}

	a.mu.Lock()
	stamp := a.seq
	pod := &abiv1.PodSnapshot{Sessions: a.podSnapshotsLocked()}
	a.subs[sub] = struct{}{} // (1) subscribe BEFORE snapshot capture
	a.mu.Unlock()            // (2) stamp captured under the same lock hold

	snap := &abiv1.SnapshotFrame{
		AtSeq:        stamp,
		Snapshot:     pod,
		Capabilities: a.capabilityReport(),
	}
	select {
	case sub.ch <- &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Snapshot{Snapshot: snap}}:
	case <-subCtx.Done():
		a.dropSub(sub)
		return nil, func() {}, subCtx.Err()
	}
	// (3)+(4) live pump: buffered channel carries everything published
	// after registration; slow consumers are dropped by fanoutLocked.
	out := make(chan *abiv1.StreamFrame, streamBuffer)
	go func() {
		defer close(out)
		for {
			select {
			case f := <-sub.ch:
				select {
				case out <- f:
				case <-subCtx.Done():
					return
				}
			case <-subCtx.Done():
				return
			}
		}
	}()
	return out, func() { a.dropSub(sub) }, nil
}

func (a *Authority) dropSub(sub *subscriber) {
	sub.cancel()
	a.mu.Lock()
	delete(a.subs, sub)
	a.mu.Unlock()
}

// Close persists the final cursor and releases resources.
func (a *Authority) Close() error {
	if a.ledger != nil {
		_ = a.ledger.close()
	}
	return a.cursor.close()
}

// ReplayUnresolvedDeliveries drives admission for ledgered rows after a
// crash/suspend resume (M2 pod-death path: exactly-once per attempt —
// admitted rows are skipped by construction, I6).
func (a *Authority) ReplayUnresolvedDeliveries(ctx context.Context) {
	if a.deliver != nil {
		a.deliver.replayUnresolved(ctx)
	}
}

// KillForTest simulates SIGKILL semantics for the kill-9 fault-injection
// test: resources are abandoned WITHOUT graceful cleanup (no cursor close,
// no subscriber teardown) exactly as a hard process death would.
func (a *Authority) KillForTest() {}

// Metrics exposes module counters for ops_metrics wiring (US-69.12).
type Metrics struct {
	DroppedEvents     int64
	ParserFailures    int64
	PanicsContained   int64
	CustomValveEvents int64
	Subscribers       int
	BufferedPending   int
	// SecondsSinceSeqAdvance is how long since the projection last moved
	// (the R5 starvation signal: seq stalled N seconds while the pod
	// runs). Initialized at construction; a reseed counts as advance.
	SecondsSinceSeqAdvance float64
	// LedgerDepths is the per-state row count (the ledger funnel).
	// Nil when no ledger is wired.
	LedgerDepths map[string]int64
	// StalledEntries is the current stalled-row count (the #1119 class,
	// visible).
	StalledEntries int64
	// OldestPromotionStallSeconds is the age of the oldest
	// admitted-unpromoted row (0 when none).
	OldestPromotionStallSeconds float64
}

func (a *Authority) Metrics() Metrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := Metrics{
		DroppedEvents:               a.droppedEvents,
		ParserFailures:              a.parserFailures,
		PanicsContained:             a.panicsContained,
		CustomValveEvents:           a.customValveEvents,
		Subscribers:                 len(a.subs),
		BufferedPending:             len(a.pending),
		SecondsSinceSeqAdvance:      time.Since(a.lastSeqAt).Seconds(),
		LedgerDepths:                nil,
		StalledEntries:              0,
		OldestPromotionStallSeconds: 0,
	}
	if a.ledger != nil {
		m.LedgerDepths = a.ledger.depths()
		m.StalledEntries = a.ledger.stalledCount()
		m.OldestPromotionStallSeconds = a.ledger.oldestAdmittedAge(time.Now())
	}
	return m
}

// InFlightDeliveries is the ledger's unresolved delivery count
// (ledgered + admitted + stalled) — design 0055 M4's flip-gate drain
// signal. Promoted/turn-ended/failed rows are resolved from the flip's
// perspective (failed is terminal per attempt; the outbox owns retries).
func (a *Authority) InFlightDeliveries() int64 {
	m := a.Metrics()
	var n int64
	for _, state := range []string{"ledgered", "admitted", "stalled"} {
		n += m.LedgerDepths[state]
	}
	return n
}

// StallStats is one stall-watchdog pass's outcome (US-69.12).
type StallStats struct {
	// Stalled is the number of rows moved admitted→stalled THIS pass.
	Stalled int
	// WakeFailures is the number of wake attempts that errored THIS pass
	// (the escalation signal: wake-only recovery failing).
	WakeFailures int
}

// CheckStalls runs one stall-detection pass: admitted rows older than the
// promotion deadline move to stalled (fsync'd), and the configured Wake
// fires once per newly-stalled row. No-op without a ledger.
func (a *Authority) CheckStalls(ctx context.Context) StallStats {
	if a.ledger == nil {
		return StallStats{}
	}
	wake := a.cfg.Wake
	if wake == nil {
		wake = func(context.Context, string) error { return nil }
	}
	stalled, failures := a.ledger.checkStalls(ctx, wake, time.Now())
	return StallStats{Stalled: stalled, WakeFailures: failures}
}

// SetStoreForTest swaps the store reader (fault-injection support).
func (a *Authority) SetStoreForTest(s StoreReader) { a.cfg.Store = s }

// PlatformDir reports the durable-cursor directory (suspend/resume
// diagnostics and the S1 scenario harness's restart scenarios).
func (a *Authority) PlatformDir() string { return a.cfg.PlatformDir }

// SetParserForTest swaps the parser seam (the #1291 r6 restart-mid-turn
// e2e: a fresh translator instance models the process restarting with an
// empty tool-call memo).
func (a *Authority) SetParserForTest(p EventParser) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Parser = p
}

// ParserFailuresForTest exposes the parser-failure counter (the #1291
// r5 shape-drift accounting pin).
func (a *Authority) ParserFailuresForTest() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parserFailures
}

// IngestForTest applies a contract event directly, bypassing the parser
// seam (fault-injection + comparator support: the projection fold is
// exercised without dialect fixtures).
func (a *Authority) IngestForTest(evt *abiv1.Event) {
	a.mu.Lock()
	if a.buffering {
		a.pending = append(a.pending, nil)
		a.mu.Unlock()
		return
	}
	a.applyLocked(evt)
	a.mu.Unlock()
}

const streamBuffer = 256

// SetStallDeadlineForTest shrinks the promotion-stall deadline (the
// fault-injection harness makes a row cross it without waiting 10m).
func (a *Authority) SetStallDeadlineForTest(d time.Duration) {
	if a.ledger != nil {
		a.ledger.mu.Lock()
		a.ledger.deadline = d
		a.ledger.mu.Unlock()
	}
}
