// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package usagestream is the API's busy-gated consumer of a pod's ABI
// contract stream (US-69.11, design 0055 S3 + D1-B + the owner's billing
// disposition): while any session of a workspace is in flight, the API
// holds ONE subscription to the pod's event stream and derives —
//
//   - per-step usage (MESSAGE_END tokens/model) for billing/metering,
//     with deterministic idempotency keys so the DB's unique constraint
//     makes multi-replica billing exactly-once;
//   - the user-stream state bridge (SESSION_STATUS → session.status,
//     INPUT_REQUEST/RESOLVED → agent.question/permission) — Epic 28's
//     cross-workspace surface keeps its wire shape, with the source
//     swapped from the retired tracker's dialect translation;
//   - context-usage persistence and session-title persistence;
//   - agent-death detection (the subscription dying after delivering
//     frames).
//
// When the fold reports no busy session for the settle window, the gate
// drops the connection — on idle pods the API holds no pod streams
// (scale-to-zero; D1-B). The display path (/contract-events) is a
// separate, browser-refcounted consumer; this one is activity-gated.
package usagestream

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// Resolve yields the pod's ABI base URL + password for a workspace.
type Resolve func(ctx context.Context, workspaceID string) (baseURL, password string, err error)

// Client is the subset of abiclient.Client the consumer needs (the test
// seam).
type Client interface {
	Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error
	// GetSnapshot reads the authority's derived per-session truth
	// (#1602: the bridge overlays the #1574 BusyComponents instead of
	// the raw dialect status).
	GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error)
}

// NewClient builds a Client for a resolved pod endpoint.
type NewClient func(baseURL, password string) Client

// Usage is one billable step: the MESSAGE_END cost record plus the
// identity needed for a deterministic idempotency key.
type Usage struct {
	SessionID  string
	MessageID  string
	Seq        uint64
	ModelID    string
	ProviderID string

	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// Billing receives billable usage. Implementations build idempotency
// keys from (WorkspaceID, MessageID, Seq) — deterministic across
// replicas — and record metering + metrics.
type Billing interface {
	RecordUsage(workspaceID string, u Usage)
}

// BillingFunc adapts a function to Billing.
type BillingFunc func(workspaceID string, u Usage)

func (f BillingFunc) RecordUsage(workspaceID string, u Usage) { f(workspaceID, u) }

// Bridge receives the derived state changes the platform still owns:
// the Epic 28 user-stream event surface, context/title persistence, and
// agent-death notification.
type Bridge interface {
	// SessionStatus fires on BUSY (or COMPACTING) and IDLE transitions.
	SessionStatus(workspaceID, sessionID string, busy bool)
	// InputRequested / InputResolved carry the unified pending-input
	// lifecycle.
	InputRequested(workspaceID string, req *abiv1.InputRequest)
	InputResolved(workspaceID, sessionID, inputID string)
	// SessionTitle fires when a session update carries a title.
	SessionTitle(workspaceID, sessionID, title string)
	// ContextUsed carries the per-step context occupancy numerator
	// (input + cacheRead + cacheWrite).
	ContextUsed(workspaceID, sessionID string, used int64)
	// AgentDied fires when the pod stream dies after delivering frames.
	AgentDied(workspaceID string)
}

// BridgeWithInternal is the #1817 piece-3 optional extension: a bridge
// that carries the ADDITIVE harness id (agentd's internal_session_id)
// through its emissions so its phrase translation can detect an
// already-translated frame and avoid the double-translation phantom row
// (the refreshed-pod defect) on EVERY path — the busy emission, the
// resolved-input emission, and the context-usage write. Bridges that do
// not implement it fall back to the plain methods (the harness-keyed
// legacy path — still correct, just without the additive guard).
type BridgeWithInternal interface {
	Bridge
	SessionStatusWithInternal(workspaceID, sessionID, internalID string, busy bool)
	InputResolvedWithInternal(workspaceID, sessionID, internalID, inputID string)
	ContextUsedWithInternal(workspaceID, sessionID, internalID string, used int64)
}

// BridgeResync is the #1884 optional extension: a bridge that wants to
// be told when a snapshot seed lands that is NOT the gate's first — with
// design 0069 resume active, that means a reconnect the ring could NOT
// serve (cursor below low-water: a pod restart ate the ring, or too many
// frames were lost; or client-ahead: the cursor is newer than the
// authority's current seq) or the in-stream reseed redial. Frames since
// the previous seed may have been lost with every downstream surface
// healthy
// (the user stream never saw a drop, so its #1646/#1687 heals never
// fire); the notification is the cue for the handler layer to run its
// authority-flight reconciliation (the busy-snapshot handshake) so
// subscribers converge onto ground truth. A reconnect that RESUMED from
// the replay ring fires nothing — nothing was lost. Bridges that do not
// implement it are unaffected.
type BridgeResync interface {
	Bridge
	// StreamResynced fires once per NON-INITIAL snapshot seed (the
	// gate's first connection fires nothing — nothing was lost before
	// it). Not a diff: an agreeing fold still fires (browser-side
	// latches are state the gate cannot compare against).
	StreamResynced(workspaceID string)
}

// emitSessionStatus routes the busy emission through the additive seam
// when the bridge supports it, else the plain one. The ADDITIVE id is
// evt.GetInternalSessionId() — the RAW agentd field, EMPTY for legacy /
// heal-window / agentd-unresolved frames (wire session_id = the harness
// id, no internal). Passing the raw field (NOT consultSessionID, which
// falls back to the wire id) is what lets the bridge's guard distinguish
// an already-phrase-translated frame (internal present) from a legacy
// harness frame (internal empty) — the latter must still translate to the
// phrase. A legacy frame with an empty internal therefore passes
// internalID="" through the seam and the bridge translates normally
// (the heal-window / "never worse" path).
func (c *Consumer) emitSessionStatus(workspaceID string, evt *abiv1.Event, busy bool) {
	if b, ok := c.cfg.Bridge.(BridgeWithInternal); ok {
		b.SessionStatusWithInternal(workspaceID, evt.GetSessionId(), evt.GetInternalSessionId(), busy)
		return
	}
	c.cfg.Bridge.SessionStatus(workspaceID, evt.GetSessionId(), busy)
}

// emitInputResolved routes the resolved-input emission through the
// additive seam (carrying the RAW internal id so the bridge's translation
// can detect an already-translated frame — empty for legacy frames, which
// still translate normally) when the bridge supports it, else the plain one.
func (c *Consumer) emitInputResolved(workspaceID string, evt *abiv1.Event, inputID string) {
	if b, ok := c.cfg.Bridge.(BridgeWithInternal); ok {
		b.InputResolvedWithInternal(workspaceID, evt.GetSessionId(), evt.GetInternalSessionId(), inputID)
		return
	}
	c.cfg.Bridge.InputResolved(workspaceID, evt.GetSessionId(), inputID)
}

// emitContextUsed routes the context-usage write through the additive
// seam (carrying the RAW internal id so the bridge writes the row against
// the real session, not a minted phantom — empty for legacy frames, which
// key by the wire id as before) when the bridge supports it, else the
// plain one.
func (c *Consumer) emitContextUsed(workspaceID string, evt *abiv1.Event, used int64) {
	if b, ok := c.cfg.Bridge.(BridgeWithInternal); ok {
		b.ContextUsedWithInternal(workspaceID, evt.GetSessionId(), evt.GetInternalSessionId(), used)
		return
	}
	c.cfg.Bridge.ContextUsed(workspaceID, evt.GetSessionId(), used)
}

// Logger is the minimal seam (the API's LoggerInterface satisfies it).
type Logger interface {
	Warn(msg string, keysAndValues ...interface{})
}

// Default gate tuning.
const (
	DefaultIdleDrop = 30 * time.Second // all-idle settle window before the gate drops
	DefaultRetry    = 2 * time.Second  // reconnect backoff after a stream error

	// consultBudget bounds one authority busy-truth consult (#1602).
	// The serve is a projection read plus a coalesced lease gather —
	// milliseconds in health; the bound only caps the fold goroutine's
	// stall when a pod is slow, before the fail-open degrade takes over.
	consultBudget = 2 * time.Second
)

// Config wires the consumer. Resolve and NewClient are required; the
// rest may be nil (defaults apply).
type Config struct {
	Resolve   Resolve
	NewClient NewClient
	Billing   Billing
	Bridge    Bridge
	Logger    Logger

	// IdleDrop is the settle window the fold must report zero busy
	// sessions for before the gate closes (default 30s).
	IdleDrop time.Duration
	// Retry is the reconnect backoff after a stream error (default 2s).
	Retry time.Duration

	// OnGateChange is the optional gate-lifecycle hook (the metrics
	// seam — a prometheus gauge in the handler layer; the package
	// stays import-clean). Fires open=true when a workspace's gate is
	// armed and open=false when it drops (Close, idle-drop, or
	// teardown). May be nil.
	OnGateChange func(workspaceID string, open bool)

	// OnFrame is the optional per-frame activity hook (the seamless
	// lease's 000069 leg): it fires for EVERY applied frame — including
	// PART_START tool-part events the billing/bridge legs skip — because
	// its consumer (the board's assignee-liveness index) answers "did
	// this session do ANYTHING" with tool-call START as evidence (a
	// worker mid-command longer than the lease window is alive). Called
	// synchronously in the fold goroutine: implementations must be
	// non-blocking (a map stamp) and never panic the stream. May be nil.
	OnFrame FrameObserver
}

// FrameObserver receives every applied frame of a workspace's pod
// stream. workspaceID scopes the stream; evt is the ABI frame (session
// identity in evt.SessionId / evt.InternalSessionId — #1817: the wire
// id is the phrase on emission, the harness id rides Internal).
type FrameObserver func(workspaceID string, evt *abiv1.Event)

// Consumer owns one busy-gated pod subscription per workspace.
type Consumer struct {
	cfg    Config
	mu     sync.Mutex
	gates  map[string]*gate
	closed bool
}

type gate struct {
	cancel context.CancelFunc
	done   chan struct{}
	// ctx is the gate's lifecycle context (Open→Close/idle-drop); the
	// busy-truth consults derive from it so a torn-down gate cancels
	// any in-flight consult promptly.
	ctx context.Context

	mu        sync.Mutex
	busyNow   bool      // the last published fold state had a busy session
	idleSince time.Time // when the fold last reported all-idle (zero while busy)
	frames    bool      // any frame received on the current connection
	// client is the connection the consults ride AND the holder of the
	// resume fold (design 0069): rebuilt only when the resolved endpoint
	// changes — across stream retries the SAME client is reused so its
	// persisted fold turns every reconnect into a ring resume instead of
	// a full snapshot. nil before the first connect. The fold (and its
	// cursor) is in-memory: it dies with the gate, degrading the next
	// gate to today's snapshot-first connect — always sound.
	client Client
	// baseURL/password pin the endpoint the current client was built
	// for: a changed resolve (pod replaced, credential rotation) builds
	// a fresh client rather than presenting one pod's cursor to another
	// authority.
	baseURL, password string
	// derived is the consult-maintained busy truth per session (#1602):
	// the last authority answer the bridge published (or failed to
	// consult past). OR'd into busyNow so a silent-but-derived-busy
	// tool run holds the gate and the final idle is never missed.
	derived map[string]bool
	// bridged is the last busy truth EMITTED to the bridge per session
	// (#1666): the reconcile baseline. Event legs record here; the
	// reconnect/resync seed diffs the freshly-folded truth against it
	// so busy state lost across a silent stream death is re-emitted,
	// not semantically lost (with design 0069 the loss window shrinks
	// to the snapshot-fallback/reseed legs — a served resume loses
	// nothing and the baseline carries over untouched).
	bridged map[string]bool
	// seededFold marks the connection's first fold publication (the
	// snapshot frame) as not-yet-applied — the reconnect reconcile
	// rebuilds derived from it. With design 0069 a RESUMED connection
	// sets this directly (WithResumed): its first publication is a
	// continuing fold, not a seed, and must not run the seed branch.
	seededFold bool
	// snapshotSeeds counts the snapshot seeds this gate has applied
	// (#1884): the initial connect's seed and every subsequent one (a
	// reconnect's first fold, the in-stream reseed redial). A seed
	// beyond the first means frames since the previous seed may have
	// been lost — the bridge's StreamResynced cue.
	snapshotSeeds int
	// connections counts the gate's Stream dial attempts (#1884 fix).
	// The resync cue keys on this, NOT on snapshotSeeds: a connection
	// that dies BEFORE delivering its snapshot (the kill-test's
	// incomplete-envelope EOF) leaves no fold and no seed — the
	// fold-count rule then misclassified the next connection's healing
	// seed as the initial connect, and the notify never fired (~12%
	// of race-timing runs). The subscription lived server-side during
	// the dead connection, so frames in that window are lost whether
	// or not a fold landed — any fold on a connection beyond the first
	// is a resync.
	connections int
	// reseedArmed is set by the WithResynced callback (the in-stream
	// projection.reseeded redial) and consumed by the next fold: that
	// fold is a resync even on connection 1.
	reseedArmed bool
	// consultFails counts consecutive-or-not consult failures for the
	// rate-limited observability warn (first + every 50th).
	consultFails int
}

func New(cfg Config) *Consumer {
	if cfg.IdleDrop <= 0 {
		cfg.IdleDrop = DefaultIdleDrop
	}
	if cfg.Retry <= 0 {
		cfg.Retry = DefaultRetry
	}
	return &Consumer{cfg: cfg, gates: map[string]*gate{}}
}

// Open arms the workspace's gate (idempotent). The subscription starts
// immediately; activity keeps it alive.
func (c *Consumer) Open(workspaceID string) {
	c.mu.Lock()
	if _, ok := c.gates[workspaceID]; ok {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &gate{cancel: cancel, done: make(chan struct{}), ctx: ctx, idleSince: time.Now(), derived: map[string]bool{}, bridged: map[string]bool{}}
	c.gates[workspaceID] = g
	hook := c.cfg.OnGateChange
	c.mu.Unlock()
	//nolint:contextcheck // the gate's lifecycle is activity-driven
	// (Open/Close + idle-drop), not request-scoped.
	if hook != nil {
		hook(workspaceID, true)
	}
	go c.run(ctx, workspaceID, g)
}

// Close drops the workspace's gate (idempotent).
func (c *Consumer) Close(workspaceID string) {
	c.mu.Lock()
	g, ok := c.gates[workspaceID]
	if ok {
		delete(c.gates, workspaceID)
	}
	hook := c.cfg.OnGateChange
	c.mu.Unlock()
	if ok {
		if hook != nil {
			hook(workspaceID, false)
		}
		g.cancel()
		<-g.done
	}
}

// Gates reports the number of open gates (the scale-to-zero observable).
func (c *Consumer) Gates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.gates)
}

// HasGate reports whether the workspace's gate is currently armed —
// the re-arm reconciler's idempotence check (#1732: after an API roll
// the new pod re-arms gates for workspaces with live sessions; ones
// already armed are left alone).
func (c *Consumer) HasGate(workspaceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.gates[workspaceID]
	return ok
}

// CloseAll drops every gate (shutdown). Terminal: later Opens are
// no-ops, and any gate goroutine that slips past the close (a caller
// mid-Open) terminates on its next loop iteration.
func (c *Consumer) CloseAll() {
	c.mu.Lock()
	c.closed = true
	gates := make([]*gate, 0, len(c.gates))
	for _, g := range c.gates {
		gates = append(gates, g)
	}
	c.gates = map[string]*gate{}
	c.mu.Unlock()
	for _, g := range gates {
		g.cancel()
		<-g.done
	}
}

func (c *Consumer) run(ctx context.Context, workspaceID string, g *gate) {
	defer func() {
		// Self-drop (idle window elapsed or ctx canceled with the gate
		// still mapped): the map entry is ours to remove — fire the hook
		// exactly once (Close's delete wins the race by making the
		// identity check fail).
		c.mu.Lock()
		self := c.gates[workspaceID] == g
		if self {
			delete(c.gates, workspaceID)
		}
		hook := c.cfg.OnGateChange
		c.mu.Unlock()
		if self && hook != nil {
			hook(workspaceID, false)
		}
		g.cancel()
		close(g.done)
	}()

	// Idle watchdog: a quiet stream publishes no state, so the drop rule
	// needs its own clock.
	idleCheck := time.NewTicker(c.cfg.IdleDrop / 4)
	defer idleCheck.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-idleCheck.C:
				g.mu.Lock()
				idle := !g.busyNow && !g.idleSince.IsZero() && time.Since(g.idleSince) >= c.cfg.IdleDrop
				g.mu.Unlock()
				if idle {
					g.cancel()
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		if c.isClosed() {
			return
		}
		baseURL, password, err := c.resolve(ctx, workspaceID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.warn("usagestream: pod resolve failed", workspaceID, err)
			if !c.sleep(ctx, c.cfg.Retry) {
				return
			}
			continue
		}
		// Client reuse (design 0069): keep the SAME abiclient across
		// stream retries while the resolved endpoint is stable — its
		// persisted fold makes the retry present EventsRequest.since and
		// resume from the authority's replay ring instead of paying a
		// full snapshot (and losing the billing frames the snapshot
		// cannot carry). A changed endpoint (pod replaced, credential
		// rotation) rebuilds: a fresh fold, never a foreign cursor.
		g.mu.Lock()
		if g.client == nil || g.baseURL != baseURL || g.password != password {
			g.client = c.cfg.NewClient(baseURL, password)
			g.baseURL, g.password = baseURL, password
		}
		cl := g.client
		g.frames = false
		g.seededFold = false
		g.connections++
		g.mu.Unlock()
		err = cl.Stream(ctx, func(st *abiclient.SessionState) {
			c.onState(workspaceID, g, st)
		}, abiclient.WithResumed(func() {
			// Design 0069: this connection RESUMED from the fold's last
			// applied seq (ring replay) — no frames were lost, so the
			// snapshot-seeded reconciliation (derived rebuild, #1666
			// corrections, #1884 resync cue) must NOT fire: mark the
			// connection already-seeded so the first publication skips
			// the seed branch. The baselines stay valid by construction
			// (the fold is the same continuing fold); replayed events
			// re-fire the event legs (billing is idempotent by its
			// deterministic keys). This is the "no double-fire" pin: a
			// resumed reconnect runs NO authority flight.
			g.mu.Lock()
			g.seededFold = true
			g.mu.Unlock()
		}), abiclient.WithResynced(func() {
			// In-stream reseed: abiclient redials on projection.reseeded
			// WITHOUT returning, so run()'s per-connection reset never
			// fires — re-arm the snapshot reconcile here (review r1
			// finding 1: the stale-true lease on this path).
			//
			// abiclient fires this callback when a snapshot frame lands
			// — the initial connect and the resume FALLBACK included —
			// so arming the resync cue here unconditionally would
			// notify on every connect. The discriminator: a true
			// in-stream reseed is a SECOND snapshot inside one Stream
			// call, i.e. the gate has already folded on this connection
			// (seededFold true — set by the seed branch, by live
			// folding, or by WithResumed). Arm only then; the run-loop
			// redial path keys on connections.
			g.mu.Lock()
			inStream := g.seededFold
			g.seededFold = false
			if inStream {
				g.reseedArmed = true
			}
			g.mu.Unlock()
		}), abiclient.WithAppliedEvents(func(evt *abiv1.Event, seq uint64) { //nolint:contextcheck // the applied-events callback cannot carry the stream ctx (abiclient's seam is ctx-less by design); the busy-truth consult inside derives from the GATE's lifecycle ctx — canceled at teardown, budget-capped otherwise.
			c.onEvent(workspaceID, g, evt, seq)
		}))
		if ctx.Err() != nil {
			return
		}
		// The stream died on its own: surface death (only after frames —
		// a never-connected stream is a resolve/transport problem, not a
		// dead agent), then retry with backoff.
		g.mu.Lock()
		hadFrames := g.frames
		g.mu.Unlock()
		if hadFrames {
			c.cfg.Bridge.AgentDied(workspaceID)
		}
		c.warn("usagestream: pod stream ended", workspaceID, err)
		if !c.sleep(ctx, c.cfg.Retry) {
			return
		}
	}
}

// onState applies the idle-drop rule on every folded-state publication:
// the gate survives while the CURRENT fold has a busy session; once the
// fold reports all-idle, the settle window starts (busy again cancels
// it). A quiet-but-busy stream (long turn, no events) stays connected.
// #1602: the consult-maintained derived truth (g.derived) joins the
// OR — the raw fold reads all-idle during a silent tool run (the
// harness's intermediate idle while a bash tool executes), and a gate
// dropped there never sees the turn's final idle (the stuck-busy
// aggravator).
func (c *Consumer) onState(workspaceID string, g *gate, st *abiclient.SessionState) {
	busy := false
	for _, s := range st.Sessions {
		if s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_BUSY ||
			s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_COMPACTING {
			busy = true
			break
		}
	}
	var corrections []bridgeCorrection
	resyncSeed := false
	g.mu.Lock()
	if !g.seededFold {
		// Reconnect reconcile (#1602): the connection's first
		// publication is the snapshot frame — the authority's current
		// answer. Rebuild the flip baseline from it so a stale-true
		// derived entry (the previous connection's consult truth for a
		// session the pod has since moved past) cannot hold the gate
		// open forever; the snapshot status is busy-aware, and the
		// BusyComponents (carried since the clone fix) are the derived
		// truth verbatim.
		g.seededFold = true
		g.snapshotSeeds++
		// #1884: a seed on any connection beyond the first — or any
		// in-stream reseed, even on connection 1 — is a reconnect/reseed
		// snapshot: frames since the previous seed may have been lost
		// while every downstream surface stayed healthy. With design
		// 0069 a seed means the ring could NOT serve the cursor (the
		// resume fallback) or a reseed invalidated the fold — the loss
		// window is real exactly then; a served resume never reaches
		// this branch (WithResumed suppresses it). Keyed on
		// connections, not the fold count: a connection that dies
		// before its first snapshot holds the server-side subscription
		// without ever folding, so its successor's healing seed must
		// still fire the bridge's resync cue. Fire after the
		// corrections (both outside the lock).
		resyncSeed = g.connections > 1 || g.reseedArmed
		g.reseedArmed = false
		rebuilt := map[string]bool{}
		for sid, s := range st.Sessions {
			// #1817 piece 3: key the fold's derived baseline by the INTERNAL
			// (harness) id, consistent with consultBusy (which records under
			// consultSessionID = the internal id). On refreshed pods the
			// snapshot is phrase-keyed (SessionId=phrase, the harness id
			// additive) — keying the fold by the phrase would leave a stale
			// phrase entry that the idle clear (consultBusy, harness-keyed)
			// never removes, OR-ing the gate busy forever (the latch). Keying
			// by the harness id makes the snapshot and the consult the same
			// session; legacy/harness-keyed frames (internal empty) fall
			// through to the wire key.
			key := s.GetInternalSessionId()
			if key == "" {
				key = sid
			}
			if sessionSeedBusy(s) {
				rebuilt[key] = true
			}
		}
		g.derived = rebuilt
		// #1666: the fold knows, the bridge may not — the stream is
		// replay-less, so the reconnect seed is the ONLY chance to
		// correct a busy frame lost across a silent stream death (the
		// backend twin of #1646). Diff the freshly-seeded truth
		// against the last bridged emission per session and collect
		// corrections; the emissions run after the unlock, in order.
		corrections = c.reconcileBridgeLocked(g, st.Sessions)
	}
	if !busy {
		for _, v := range g.derived {
			if v {
				busy = true
				break
			}
		}
	}
	g.busyNow = busy
	if busy {
		g.idleSince = time.Time{}
	} else if g.idleSince.IsZero() {
		g.idleSince = time.Now()
	}
	g.mu.Unlock()
	for _, corr := range corrections {
		// #1817 piece 3: route the reconcile correction through the additive
		// seam (carrying the snapshot's internal/harness id) so a
		// refreshed-pod (phrase-keyed) correction does not re-register the
		// phrase (finding 3). Non-implementing bridges fall back to the
		// plain SessionStatus.
		if b, ok := c.cfg.Bridge.(BridgeWithInternal); ok {
			b.SessionStatusWithInternal(workspaceID, corr.sessionID, corr.internalID, corr.busy)
			continue
		}
		c.cfg.Bridge.SessionStatus(workspaceID, corr.sessionID, corr.busy)
	}
	// #1884: the non-initial seed's resync cue — the corrections above
	// heal only the busy DIFFS the gate can compute; the authority
	// flight the handler layer runs on this cue heals the rest (browser
	// latches, sessions absent from the fold). Non-implementing bridges
	// are unaffected.
	if resyncSeed {
		if b, ok := c.cfg.Bridge.(BridgeResync); ok {
			b.StreamResynced(workspaceID)
		}
	}
}

// bridgeCorrection is one reconcile emission: the session whose bridged
// truth disagrees with the fold, and the fold's answer.
type bridgeCorrection struct {
	sessionID  string
	internalID string
	busy       bool
}

// reconcileBridgeLocked diffs the freshly-seeded fold against the last
// bridged emission per session and returns the corrections in session
// order (deterministic across map iteration). Called with g.mu held;
// recordings happen under the same lock hold that decides, so each
// disagreement yields exactly one emission (idempotent across
// reconnects — an agreeing fold returns nothing, the quiet periods stay
// quiet). Sessions ABSENT from the fold get no emission: the reconcile
// asserts only truths the authority answered.
func (c *Consumer) reconcileBridgeLocked(g *gate, sessions map[string]*abiv1.SessionSnapshot) []bridgeCorrection {
	if c.cfg.Bridge == nil {
		return nil
	}
	var corrections []bridgeCorrection
	for sid, s := range sessions {
		fold := sessionSeedBusy(s)
		if g.bridged[sid] != fold {
			g.bridged[sid] = fold
			corrections = append(corrections, bridgeCorrection{
				// #1817 piece 3: carry the additive harness id (the
				// snapshot's internal_session_id) so the bridge's
				// translation can detect an already-translated (phrase)
				// frame and not re-register it (finding 3 — the
				// reconnect-correction phantom row).
				sessionID:  s.GetSessionId(),
				internalID: s.GetInternalSessionId(),
				busy:       fold,
			})
		}
	}
	sort.Slice(corrections, func(i, j int) bool { return corrections[i].sessionID < corrections[j].sessionID })
	return corrections
}

// sessionSeedBusy is the fold's busy truth for one snapshot session —
// the same seed rule the derived baseline rebuild uses (#1602): the
// authority's BusyComponents verbatim when present, else the busy-aware
// raw status.
func sessionSeedBusy(s *abiv1.SessionSnapshot) bool {
	if b := s.GetBusy(); b != nil {
		return b.GetBusy()
	}
	return s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_BUSY ||
		s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_COMPACTING
}

func (c *Consumer) onEvent(workspaceID string, g *gate, evt *abiv1.Event, seq uint64) {
	g.mu.Lock()
	g.frames = true
	g.mu.Unlock()

	// The activity hook fires for EVERY frame before the typed legs —
	// its observers key on raw frame arrival (PART_START included),
	// not on any derived state. Panic containment is the observer's
	// own contract; a nil observer (the default) costs one branch.
	if c.cfg.OnFrame != nil {
		c.cfg.OnFrame(workspaceID, evt)
	}

	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_MESSAGE_END:
		msg := evt.GetMessage()
		if msg == nil || msg.GetType() != abiv1.MessageType_MESSAGE_TYPE_ASSISTANT {
			return
		}
		cost := msg.GetCost()
		if cost == nil || (cost.GetInputTokens() == 0 && cost.GetOutputTokens() == 0 && cost.GetTotalTokens() == 0 && cost.GetCostUsd() == 0) {
			return
		}
		if c.cfg.Billing != nil {
			u := Usage{
				SessionID:        evt.GetSessionId(),
				MessageID:        msg.GetId(),
				Seq:              seq,
				InputTokens:      cost.GetInputTokens(),
				OutputTokens:     cost.GetOutputTokens(),
				ReasoningTokens:  cost.GetReasoningTokens(),
				CacheReadTokens:  cost.GetCacheReadTokens(),
				CacheWriteTokens: cost.GetCacheWriteTokens(),
				CostUSD:          cost.GetCostUsd(),
			}
			if m := msg.GetModel(); m != nil {
				u.ModelID = m.GetId()
				u.ProviderID = m.GetProvider()
			}
			c.cfg.Billing.RecordUsage(workspaceID, u)
		}
		if c.cfg.Bridge != nil {
			c.emitContextUsed(workspaceID, evt,
				cost.GetInputTokens()+cost.GetCacheReadTokens()+cost.GetCacheWriteTokens())
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		if c.cfg.Bridge == nil {
			return
		}
		switch evt.GetStatus() {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY, abiv1.SessionStatus_SESSION_STATUS_COMPACTING,
			abiv1.SessionStatus_SESSION_STATUS_IDLE, abiv1.SessionStatus_SESSION_STATUS_ERROR:
			// #1602: the emission carries the authority's DERIVED busy
			// truth (#1574 BusyComponents — read via GetSnapshot, never
			// recomputed here). The raw dialect status is streaming-only
			// semantics — its idle while a tool executes is the incident
			// this fixes. Consult failure (pod glitch, projection-
			// unknown session) fails open to the raw mapping: the pipe
			// goes legacy, never quiet.
			// #1722: ERROR joins the bridged set — an errored turn is
			// OVER, not busy (the evidence settle publishes SESSION_STATUS
			// ERROR for a harness-reported error verdict; the raw mapping
			// below must not read it as busy). The consult governs.
			busy := evt.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_BUSY ||
				evt.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_COMPACTING
			if derived, ok := c.consultBusy(workspaceID, g, consultSessionID(evt)); ok {
				busy = derived
			}
			c.emitSessionStatus(workspaceID, evt, busy)
			// #1666: the emission is the reconcile baseline — record it.
			g.mu.Lock()
			g.bridged[evt.GetSessionId()] = busy
			g.mu.Unlock()
		}
	// Busy-flip candidates that fire NO status event (a tool part
	// starting after the harness's idle, an errored turn, a message
	// starting): consult the derived truth and publish the FLIP only —
	// these legs never bridged before, so every-event emission would
	// spam the user stream.
	case abiv1.EventType_EVENT_TYPE_PART_START, abiv1.EventType_EVENT_TYPE_PART_END,
		abiv1.EventType_EVENT_TYPE_MESSAGE_START, abiv1.EventType_EVENT_TYPE_ERROR:
		if c.cfg.Bridge == nil || evt.GetSessionId() == "" {
			return
		}
		// #1817 piece 3: read the flip baseline under the CONSULT key (the
		// internal/harness id — consultBusy records there and onState seeds
		// there), not the wire (phrase) key. On refreshed pods the phrase
		// key would always miss (prev=false), re-emitting busy on every
		// candidate and breaking the "flip only" contract.
		consultKey := consultSessionID(evt)
		g.mu.Lock()
		prev := g.derived[consultKey]
		g.mu.Unlock()
		if derived, ok := c.consultBusy(workspaceID, g, consultKey); ok && derived != prev {
			c.emitSessionStatus(workspaceID, evt, derived)
			// #1666: the emission is the reconcile baseline — record it.
			g.mu.Lock()
			g.bridged[evt.GetSessionId()] = derived
			g.mu.Unlock()
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if c.cfg.Bridge != nil && evt.GetInput() != nil {
			c.cfg.Bridge.InputRequested(workspaceID, evt.GetInput())
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if c.cfg.Bridge != nil && evt.GetInput() != nil {
			c.emitInputResolved(workspaceID, evt, evt.GetInput().GetId())
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_UPDATED:
		if c.cfg.Bridge != nil {
			if s := evt.GetSession(); s != nil && s.GetTitle() != "" {
				c.cfg.Bridge.SessionTitle(workspaceID, s.GetId(), s.GetTitle())
			}
		}
	}
}

// consultSessionID is the id the busy consult keys the authority by: the
// harness id, carried ADDITIVELY on the emitted event's
// internal_session_id (the #1817/#1821 piece-1 emission puts the phrase
// on the wire session_id). The authority's GetSnapshot map is
// harness-id-keyed — a phrase key 404s and fails open to the raw status
// (the sporadic-busy this fixes). Legacy events without the additive
// field (or with it empty) fall back to the wire session_id — never
// worse than today.
func consultSessionID(evt *abiv1.Event) string {
	if id := evt.GetInternalSessionId(); id != "" {
		return id
	}
	return evt.GetSessionId()
}

// consultBusy asks the authority for one session's derived busy truth
// (the #1574 BusyComponents, read verbatim — the one definition, never
// recomputed here) and records the answer as the gate's flip baseline.
// ok=false means no answer (no connection yet, consult failure, or a
// Busy-less snapshot) — callers fail open to the raw status mapping,
// and the failure is OBSERVABLE: warn on the first failure and every
// 50th after (a silent degrade would reproduce the #1602 incident with
// no signal). Called from the fold goroutine (onEvent), so consults
// serialize in stream order; the budget caps the stall a slow pod can
// impose.
func (c *Consumer) consultBusy(workspaceID string, g *gate, sessionID string) (busy, ok bool) {
	if sessionID == "" {
		return false, false
	}
	g.mu.Lock()
	cl := g.client
	gateCtx := g.ctx
	g.mu.Unlock()
	if cl == nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(gateCtx, consultBudget)
	defer cancel()
	snap, err := cl.GetSnapshot(ctx, sessionID)
	if err != nil || snap.GetBusy() == nil {
		g.mu.Lock()
		g.consultFails++
		n := g.consultFails
		g.mu.Unlock()
		if c.cfg.Logger != nil && (n == 1 || n%50 == 0) {
			if err != nil {
				c.cfg.Logger.Warn("usagestream: busy-truth consult failed — failing open to raw status", "workspaceID", workspaceID, "sessionID", sessionID, "error", err)
			} else {
				c.cfg.Logger.Warn("usagestream: busy-truth consult returned no components — failing open to raw status", "workspaceID", workspaceID, "sessionID", sessionID)
			}
		}
		return false, false
	}
	busy = snap.GetBusy().GetBusy()
	g.mu.Lock()
	g.derived[sessionID] = busy
	g.mu.Unlock()
	return busy, true
}

// handleError is the death-detection seam for connections that failed
// before any state callback could run (tests and edge wiring).
func (c *Consumer) handleError(workspaceID string, hadFrames bool) {
	if hadFrames && c.cfg.Bridge != nil {
		c.cfg.Bridge.AgentDied(workspaceID)
	}
}

// resolve wraps the resolve seam with panic containment: this goroutine
// is a background consumer whose seams touch handler state — a panic
// there must degrade to a retry, never kill the process.
func (c *Consumer) resolve(ctx context.Context, workspaceID string) (baseURL, password string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("resolve panicked: %v", r)
		}
	}()
	return c.cfg.Resolve(ctx, workspaceID)
}

func (c *Consumer) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Consumer) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Consumer) warn(msg, workspaceID string, err error) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn(msg, "workspaceID", workspaceID, "error", err)
	}
}
