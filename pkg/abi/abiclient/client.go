// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package abiclient is the reference Go consumer of the harness ABI
// (design 0055 M1, US-69.4): it implements the client side of the
// stamped-snapshot sync protocol — apply in order, discard seq ≤ S,
// re-snapshot on projection.reseeded. The S1 comparator (US-69.5) and the
// API's shadow consumer (S2) share this implementation so the discard rule
// exists exactly once.
//
// Design 0069 adds the resume half: the client PERSISTS its fold (and the
// fold's last applied seq) across connections and, on every reconnect,
// presents that seq as EventsRequest.since. A served cursor replays the
// authority's retained frames (ring replay) through the same discard rule —
// no gap, no duplicate application, by construction; an unservable
// cursor (below the ring's low-water mark, ahead of its current seq,
// or an older agentd that ignores the field) degrades to today's
// snapshot-first protocol, which the client
// recognizes by the SnapshotFrame that opens the connection.
//
// This is a consumer of generated wire types — no hand-written wire
// structs (TestNoHandWrittenWire governs the surface path).
package abiclient

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// Client talks to one pod's ABI surface (agentd :4097 behind Basic auth —
// supply an http.Client whose transport injects the credential).
//
// Concurrency contract: one Client is used by ONE goroutine (the fold is
// single-threaded by design — the discard rule owns ordering). The states
// handed to Stream's onUpdate callback are immutable snapshots (deep
// copies) safe to retain or read from any goroutine.
//
// Design 0069: the fold is CLIENT-held and survives across Stream calls
// and their internal redials — that persistence is what makes the resume
// cursor truthful ("I hold exactly the fold of events 1..Seq") on every
// reconnect. The cursor is in-memory only: it lives as long as the Client
// (for the API's usagestream consumer, as long as the workspace's gate
// holds its client — an API roll or a gate re-arm starts from a snapshot,
// which is always sound). The durable variant (persisting the cursor in
// the API's ws_state store so it survives API rolls, the
// api/internal/services/wsstate/store.go precedent) was judged not worth
// the coupling: a lost cursor degrades to one snapshot, never to loss.
type Client struct {
	svc abiconnect.HarnessABIServiceClient
	// state is the persistent fold (lazily seeded by Stream). Its Seq IS
	// the resume cursor presented as EventsRequest.since.
	state *SessionState
	// foldSeeded marks that a snapshot has seeded the fold at least once —
	// only then does a fold exist whose Seq is a sound resume claim.
	foldSeeded bool
	// needSnapshot marks that a ReseedNotice invalidated the fold: the
	// next connection must re-snapshot (EventsRequest WITHOUT since —
	// replaying post-reseed deltas onto a pre-reseed fold would diverge,
	// and replaying the notice itself would loop the redial).
	needSnapshot bool
}

func New(httpClient *http.Client, baseURL string, opts ...connect.ClientOption) *Client {
	return &Client{svc: abiconnect.NewHarnessABIServiceClient(httpClient, baseURL, opts...)}
}

// SessionState is the client-side fold: the pod snapshot plus live events
// with seq > Seq applied. Equivalent to the server projection's view by
// construction (TestDiscardRulePropertyFuzz).
type SessionState struct {
	Seq      uint64
	Sessions map[string]*abiv1.SessionSnapshot
}

func newState() *SessionState {
	return &SessionState{Sessions: map[string]*abiv1.SessionSnapshot{}}
}

// clone returns an immutable deep copy (handed to callbacks; the live fold
// keeps mutating the original).
func (s *SessionState) clone() *SessionState {
	out := &SessionState{Seq: s.Seq, Sessions: make(map[string]*abiv1.SessionSnapshot, len(s.Sessions))}
	for k, v := range s.Sessions {
		out.Sessions[k] = cloneSessionSnapshot(v)
	}
	return out
}

// GetSnapshot fetches one session's authoritative snapshot (I12) — a
// projection read plus ONE pod-local lease-refresh gather (#1310: pending
// asks are leases re-verified on serve; coalesced, TTL-bounded, and
// degraded-served on failure).
func (c *Client) GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
	resp, err := c.svc.GetSnapshot(ctx, connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Deliver submits a delivery entry (op 1). Idempotence is "no second
// row", not "state frozen": a duplicate ack carries the row's CURRENT
// state (which may have advanced) — the delivery_op_test semantics.
func (c *Client) Deliver(ctx context.Context, req *abiv1.DeliveryRequest) (*abiv1.DeliveryAck, error) {
	resp, err := c.svc.Deliver(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// syncIdleWindow is how long Sync waits for further frames before
// declaring the fold current (frames arrive promptly; live pods emit
// continuously during turns — the window only needs to outlast a burst).
const syncIdleWindow = 150 * time.Millisecond

// Sync opens the events stream, applies the snapshot frame, and drains
// frames until the stream goes idle — a consistent one-shot state fetch.
// Cancel ctx to stop early.
func (c *Client) Sync(ctx context.Context) (*SessionState, error) {
	st, _, err := c.syncInner(ctx, syncIdleWindow)
	return st, err
}

func (c *Client) syncInner(ctx context.Context, idle time.Duration) (*SessionState, *abiv1.CapabilityReport, error) {
	s, err := c.svc.Events(ctx, connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = s.Close() }()
	st := newState()
	var rep *abiv1.CapabilityReport
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	for {
		if idleTimer == nil {
			idleTimer = time.NewTimer(idle)
			idleC = idleTimer.C
		} else {
			idleTimer.Reset(idle)
		}
		type frameOrErr struct {
			f   *abiv1.StreamFrame
			ok  bool
			err error
		}
		recv := make(chan frameOrErr, 1)
		go func() {
			f, ok := func() (*abiv1.StreamFrame, bool) {
				if s.Receive() {
					return s.Msg(), true
				}
				return nil, false
			}()
			recv <- frameOrErr{f, ok, nil}
		}()
		select {
		case r := <-recv:
			if !r.ok {
				if err := s.Err(); err != nil {
					return nil, nil, err
				}
				return st, rep, nil
			}
			if rep == nil {
				if snap := r.f.GetSnapshot(); snap != nil {
					applySnapshot(st, snap)
					rep = snap.GetCapabilities()
					continue
				}
			}
			if rr := r.f.GetReseeded(); rr != nil {
				fresh, frep, err := c.syncInner(ctx, idle)
				if err != nil {
					return nil, nil, err
				}
				return fresh, frep, nil
			}
			applyEvent(st, r.f.GetEvent())
		case <-idleC:
			return st, rep, nil
		case <-ctx.Done():
			return st, rep, ctx.Err()
		}
	}
}

// firstFrameBudget bounds how long a SNAPSHOT-MODE (re)connect may take to
// deliver its snapshot frame — the handler sends it immediately, so a
// silent window means a wedged connection; the client drops it and
// reconnects instead of stalling the fold forever. Design 0069 resume
// connections take NO budget: a served-but-caught-up cursor legitimately
// hears nothing until the next live event (empty replay + idle pod), which
// is indistinguishable from silence — the steady-state posture (idle pods
// legitimately emit nothing for minutes) extends to the resumed
// connection's first frame.
const firstFrameBudget = 2 * time.Second

type frameResult struct {
	f   *abiv1.StreamFrame
	ok  bool
	err error
}

// receiveOne reads one frame with a first-frame budget; budget=false for
// steady-state frames (idle pods legitimately emit nothing for minutes).
func receiveOne(ctx context.Context, s *connect.ServerStreamForClient[abiv1.StreamFrame], budget bool) frameResult {
	recv := make(chan frameResult, 1)
	go func() {
		if s.Receive() {
			recv <- frameResult{f: s.Msg(), ok: true}
		} else {
			recv <- frameResult{ok: false}
		}
	}()
	if !budget {
		select {
		case r := <-recv:
			return r
		case <-ctx.Done():
			return frameResult{err: ctx.Err()}
		}
	}
	timer := time.NewTimer(firstFrameBudget)
	defer timer.Stop()
	select {
	case r := <-recv:
		return r
	case <-timer.C:
		_ = s.Close()
		return frameResult{err: errText("no snapshot frame within budget")}
	case <-ctx.Done():
		return frameResult{err: ctx.Err()}
	}
}

// StreamOption tunes Stream's callbacks (US-69.11).
type StreamOption func(*streamOptions)

type streamOptions struct {
	appliedEvents func(evt *abiv1.Event, seq uint64)
	resynced      func()
	resumed       func()
}

// AppliedEventsOf composes opts into the single applied-events callback
// they configure (nil when none). Consumers that wrap Stream with their
// own event dispatch (the API's usagestream consumer and its test fake)
// read the seam through this instead of re-implementing option plumbing.
func AppliedEventsOf(opts []StreamOption) func(evt *abiv1.Event, seq uint64) {
	var so streamOptions
	for _, o := range opts {
		o(&so)
	}
	return so.appliedEvents
}

// WithResynced registers a callback fired whenever the fold is rebuilt
// from a fresh snapshot frame — the initial connect, the honest
// fallback (the resume cursor was below the ring's low-water mark,
// ahead of its current seq, or the serving agentd predates design
// 0069 and ignored since), and the in-stream projection.reseeded
// redial (Stream handles the reseed internally without returning, so
// run-loop-level reconnect hooks never see it). Fired BEFORE the
// onUpdate publication of the rebuilt state, so consumers can re-arm
// snapshot-based reconciliation. The #1602 stale-truth reconcile is the
// consumer. A connection that RESUMED fires WithResumed instead — nothing
// was lost, so snapshot-seeded reconciliation must stay quiet.
func WithResynced(fn func()) StreamOption {
	return func(o *streamOptions) { o.resynced = fn }
}

// ResyncedOf composes opts into the single resynced callback they
// configure (nil when none) — the WithResynced counterpart of
// AppliedEventsOf for consumers that wrap Stream (test fakes).
func ResyncedOf(opts []StreamOption) func() {
	var so streamOptions
	for _, o := range opts {
		o(&so)
	}
	return so.resynced
}

// WithResumed registers a callback fired when a (re)connect RESUMES from
// the client's last applied seq (design 0069 ring replay) instead of
// re-snapshotting — the server honored EventsRequest.since and the stream
// opened with replayed/live frames, no SnapshotFrame. Fired once per such
// connection, BEFORE the first onUpdate publication of that connection, so
// consumers can suppress snapshot-seeded reconciliation (busy corrections,
// resync cues): the fold is the same continuing fold and nothing was lost.
// A connection that falls back fires WithResynced instead.
func WithResumed(fn func()) StreamOption {
	return func(o *streamOptions) { o.resumed = fn }
}

// ResumedOf composes opts into the single resumed callback they configure
// (nil when none) — the WithResumed counterpart of AppliedEventsOf for
// consumers that wrap Stream (test fakes).
func ResumedOf(opts []StreamOption) func() {
	var so streamOptions
	for _, o := range opts {
		o(&so)
	}
	return so.resumed
}

// WithAppliedEvents registers a callback invoked for every event the
// fold ACCEPTED (post discard rule), in stream order, with its seq —
// exactly-once-per-seq delivery across reconnects. Design 0069 extends the
// guarantee across the reconnect itself: replayed frames (ring resume)
// fire here exactly like live ones, which is the billing-recovery path —
// a MESSAGE_END dropped in a disconnect window is redelivered by the
// replay and billed exactly once (the consumer's idempotency keys absorb
// any redelivery). Consumers that need event payloads (usage metering,
// state bridges) use this instead of re-implementing the fold.
func WithAppliedEvents(fn func(evt *abiv1.Event, seq uint64)) StreamOption {
	return func(o *streamOptions) { o.appliedEvents = fn }
}

// Stream keeps the folded state current: it applies the snapshot, then live
// events, invoking onUpdate after each applied frame. On
// projection.reseeded (mandatory re-snapshot) it transparently reconnects.
// Runs until ctx is canceled; returns the ctx error.
//
// Design 0069 resume: the fold lives on the Client and every reconnect —
// the internal redials (reseed, wedge, server-side slow-consumer drop) and
// later Stream calls on the same Client — presents the fold's Seq as
// EventsRequest.since. A served cursor replays the authority's retained
// frames seq > since through the same discard rule (no gap, no duplicate,
// by construction); an unservable cursor (or an agentd without ring
// support) degrades to the snapshot-first protocol, recognized by the
// SnapshotFrame that opens the connection.
func (c *Client) Stream(ctx context.Context, onUpdate func(*SessionState), opts ...StreamOption) error {
	var so streamOptions
	for _, o := range opts {
		o(&so)
	}
	if c.state == nil {
		c.state = newState()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req := &abiv1.EventsRequest{}
		resuming := false
		if c.foldSeeded && !c.needSnapshot {
			// Resume attempt: the fold's Seq is the truthful cursor ("I
			// hold exactly the fold of events 1..Seq"). The server either
			// replays > since (first frame NOT a snapshot) or falls back
			// honestly (first frame a snapshot); both are handled below.
			since := c.state.Seq
			req.Since = &since
			resuming = true
		}
		s, err := c.svc.Events(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		seeded := false
		wedged := false
		receiveDone := false
		for {
			receiveDone = false // per-call: only THIS call's completion counts
			// The first-frame budget applies only while a SNAPSHOT is
			// owed (the handler sends it immediately). A resume
			// connection owes nothing: silence is legitimate (caught-up
			// cursor, idle pod) — steady-state posture, see
			// firstFrameBudget.
			r := receiveOne(ctx, s, !seeded && !resuming)
			if r.err != nil {
				if ctx.Err() == nil {
					wedged = true // budget/protocol stall: reconnect with pause
				}
				break
			}
			// The receiver goroutine completed (buffered result): the
			// stream is idle now — s.Err() is safe. Never concurrent with
			// an in-flight Receive (connect streams are not goroutine-safe).
			receiveDone = true
			if !r.ok {
				break
			}
			f := r.f
			if !seeded {
				if snap := f.GetSnapshot(); snap != nil {
					// Snapshot-first connection: the initial connect, the
					// post-reseed redial, or the honest fallback (cursor
					// below the ring's low-water mark, ahead of its
					// current seq, / pre-0069 agentd).
					// applySnapshot REPLACES the fold content and re-stamps
					// Seq — the discard rule stays exact across the rebuild.
					applySnapshot(c.state, snap)
					c.foldSeeded = true
					c.needSnapshot = false
					seeded = true
					if so.resynced != nil {
						so.resynced()
					}
					if onUpdate != nil {
						onUpdate(c.state.clone())
					}
					continue
				}
				if resuming {
					// Resume honored (design 0069): the stream opened with
					// a replayed/live frame — the fold continues where it
					// left off, no rebuild, no snapshot reconciliation.
					seeded = true
					if so.resumed != nil {
						so.resumed()
					}
				} else {
					// A non-snapshot first frame violates the protocol;
					// treat as wedged and reconnect.
					_ = s.Close()
					break
				}
			}
			if rr := f.GetReseeded(); rr != nil {
				// Mandatory re-snapshot (I3): the fold is invalidated —
				// drop the connection and reconnect WITHOUT since (the
				// next connection's snapshot re-stamps the cursor past
				// this notice; replaying post-reseed deltas onto the
				// pre-reseed fold would diverge it, and replaying the
				// notice again would loop the redial forever). Frames
				// replayed BEFORE the notice were already applied — that
				// is the billing window the resume closes for good.
				c.needSnapshot = true
				_ = s.Close()
				break
			}
			seqed := f.GetEvent()
			applyEvent(c.state, seqed)
			if so.appliedEvents != nil && seqed != nil && seqed.GetEvent() != nil {
				so.appliedEvents(seqed.GetEvent(), seqed.GetSeq())
			}
			if onUpdate != nil {
				onUpdate(c.state.clone())
			}
		}
		if receiveDone {
			if err := s.Err(); err != nil && ctx.Err() == nil {
				return err
			}
		}
		if wedged {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// Capabilities fetches the pod's capability report (rides the events
// stream's snapshot frame — there is deliberately no sixth op).
func (c *Client) Capabilities(ctx context.Context) (*abiv1.CapabilityReport, error) {
	_, rep, err := c.syncInner(ctx, syncIdleWindow)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, connect.NewError(connect.CodeInternal, errText("stream closed without a snapshot frame"))
	}
	return rep, nil
}

// Act issues a typed action (surface-gated until US-69.9; undeclared
// actions return typed NotSupported via connect error details).
func (c *Client) Act(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	resp, err := c.svc.Act(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// applySnapshot REPLACES the fold with the stamped snapshot's content
// (rebuild semantics — a snapshot is the full authority answer, not a
// merge): Seq takes the stamp, and the sessions map is rebuilt from
// scratch. The discard rule then drops any subsequent event seq ≤ stamp,
// which is what keeps snapshot-mode overlap exact across rebuilds.
func applySnapshot(st *SessionState, snap *abiv1.SnapshotFrame) {
	st.Seq = snap.GetAtSeq()
	st.Sessions = make(map[string]*abiv1.SessionSnapshot, len(snap.GetSnapshot().GetSessions()))
	for _, s := range snap.GetSnapshot().GetSessions() {
		st.Sessions[s.GetSessionId()] = cloneSessionSnapshot(s)
	}
}

// applyEvent implements the client discard rule: events with seq ≤ the
// snapshot stamp (or already applied) are dropped; the rest fold.
func applyEvent(st *SessionState, seqed *abiv1.SequencedEvent) {
	if seqed == nil {
		return
	}
	if seqed.Seq <= st.Seq {
		return // discard ≤ S (duplicate application direction)
	}
	st.Seq = seqed.Seq
	evt := seqed.GetEvent()
	if evt == nil {
		return
	}
	sid := evt.GetSessionId()
	snap := st.Sessions[sid]
	if snap == nil {
		// A session observed before any status event is UNKNOWN — the
		// same convention as the server projection (never UNSPECIFIED:
		// the zero value is not a valid status). Carry the additive
		// internal_session_id from the event so a freshly-seen session's
		// snapshot does not drop the "never dropped" harness id (#1817 r4
		// finding 5 — the clone preserves it; a synthesized snapshot must
		// too).
		snap = &abiv1.SessionSnapshot{
			SessionId:         sid,
			Status:            abiv1.SessionStatus_SESSION_STATUS_UNKNOWN,
			InternalSessionId: evt.GetInternalSessionId(),
		}
		st.Sessions[sid] = snap
	}
	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		snap.Status = evt.GetStatus()
		if evt.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_IDLE {
			// Turn over: the server projection clears in-flight parts on
			// idle — the fold must mirror it or reconnects show stale
			// parts.
			snap.InFlightParts = nil
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if in := evt.GetInput(); in != nil {
			snap.PendingInputs = append(snap.PendingInputs, in)
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if in := evt.GetInput(); in != nil {
			next := snap.PendingInputs[:0]
			for _, p := range snap.PendingInputs {
				if p.GetId() != in.GetId() {
					next = append(next, p)
				}
			}
			snap.PendingInputs = next
		}
	case abiv1.EventType_EVENT_TYPE_PART_START, abiv1.EventType_EVENT_TYPE_PART_END:
		if p := evt.GetPart(); p != nil {
			upsertPart(snap, p)
		}
	case abiv1.EventType_EVENT_TYPE_PART_DELTA:
		pid := evt.GetPartId()
		for i, p := range snap.InFlightParts {
			if p.GetId() == pid {
				snap.InFlightParts[i].Payload = &abiv1.Part_Text{Text: p.GetText() + evt.GetDelta()}
			}
		}
	}
}

func upsertPart(snap *abiv1.SessionSnapshot, p *abiv1.Part) {
	for i, existing := range snap.InFlightParts {
		if existing.GetId() == p.GetId() {
			snap.InFlightParts[i] = p
			return
		}
	}
	snap.InFlightParts = append(snap.InFlightParts, p)
}

// cloneBusy deep-copies the authority-derived components (nil-safe —
// proto.Clone on a nil message returns an untyped nil the caller cannot
// assert back).
func cloneBusy(b *abiv1.BusyComponents) *abiv1.BusyComponents {
	if b == nil {
		return nil
	}
	return proto.Clone(b).(*abiv1.BusyComponents)
}

func cloneSessionSnapshot(s *abiv1.SessionSnapshot) *abiv1.SessionSnapshot {
	out := &abiv1.SessionSnapshot{
		SessionId:     s.GetSessionId(),
		Status:        s.GetStatus(),
		QueueDepth:    s.GetQueueDepth(),
		InFlightParts: append([]*abiv1.Part(nil), s.GetInFlightParts()...),
		PendingInputs: append([]*abiv1.InputRequest(nil), s.GetPendingInputs()...),
		// The #1574 derived busy truth is authority-computed and rides
		// the snapshot verbatim — consumers must read it, never
		// recompute it. Dropping it here served busy-status sessions
		// with nil components (#1602).
		Busy: cloneBusy(s.GetBusy()),
		// #1817 piece 3: the additive harness id (the phrase-keyed
		// snapshot's internal_session_id) must survive the fold or the
		// "never dropped" contract is broken and the consumer can't
		// key its derived baseline by the harness id.
		InternalSessionId: s.GetInternalSessionId(),
	}
	return out
}

type errText string

func (e errText) Error() string { return string(e) }
