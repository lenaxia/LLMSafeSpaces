// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// errNoSuchSession models the authority's NotFound for projection-unknown
// sessions (the fail-open leg).
type errNoSuchSession string

func (e errNoSuchSession) Error() string { return "unknown session: " + string(e) }

// fakeClient fakes abiclient.Client's Stream: snapshots then scripted
// applied events, under the test's control.
type fakeClient struct {
	onUpdate  func(*abiclient.SessionState)
	onEvent   func(evt *abiv1.Event, seq uint64)
	connected chan struct{}
	// Minimal fold: BUSY/COMPACTING status events mark the session busy
	// (mirrors the reference client's folded-state publications).
	busy map[string]bool
	// #1602: scriptable GetSnapshot answers (the authority truth the
	// bridge consults).
	mu          sync.Mutex
	snapshots   map[string]*abiv1.SessionSnapshot
	snapshotErr error
	// kill (one-shot, closed by killStream) makes the CURRENT stream
	// return an error — the reconnect path.
	kill chan struct{}
	// resynced is the WithResynced callback (nil until the consumer
	// arms it) — fired by reseed to simulate the in-stream
	// projection.reseeded redial.
	resynced func()
	// seedSessions scripts the fold's snapshot content on (re)connect
	// (#1666: the busy truth that exists only in the NEXT connection's
	// snapshot — the frame died with the old one, no replay).
	seedSessions map[string]*abiv1.SessionSnapshot
}

// setSeedSessions scripts every subsequent connection's snapshot fold.
func (f *fakeClient) setSeedSessions(sessions map[string]*abiv1.SessionSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seedSessions = sessions
}

func (f *fakeClient) Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error {
	// The reference client exposes the composed callback for consumers
	// that wrap Stream (this fake is one).
	f.onEvent = abiclient.AppliedEventsOf(opts)
	f.onUpdate = onUpdate
	f.resynced = abiclient.ResyncedOf(opts)
	// Initialize the kill channel and snapshot the seed BEFORE signaling
	// connected. The test's requireOpen only waits for connected, then
	// scripts the next seed and calls killStream; if the kill channel were
	// created after the signal, killStream could observe nil and silently
	// miss (no reconnect → the 5s Eventually "never satisfied" flake).
	// Reading the seed first also makes the initial connection's snapshot
	// deterministic (the test scripts the RECONNECT's truth after open).
	f.mu.Lock()
	if f.kill == nil {
		f.kill = make(chan struct{})
	}
	kill := f.kill
	seed := f.seedSessions
	f.mu.Unlock()
	// Snapshot-first: the protocol's stamp (scripted per #1666 so a
	// reconnect can carry truth its predecessor's frames lost). Signal
	// `connected` only AFTER applying it: requireOpen then guarantees the
	// consumer has finished the initial onState before the test drives
	// events (otherwise the snapshot fold and a synchronous apply race on
	// the recording bridge).
	sessions := map[string]*abiv1.SessionSnapshot{}
	for sid, s := range seed {
		sessions[sid] = s
	}
	onUpdate(&abiclient.SessionState{Seq: 0, Sessions: sessions})
	if f.connected != nil {
		f.connected <- struct{}{}
	}
	select {
	case <-ctx.Done():
		return context.Canceled
	case <-kill:
		f.mu.Lock()
		f.kill = nil // one-shot: the reconnect watches a fresh channel
		f.mu.Unlock()
		return errors.New("stream killed by test")
	}
}

// killStream closes the current stream connection (one-shot).
func (f *fakeClient) killStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kill != nil {
		close(f.kill)
	}
}

func (f *fakeClient) apply(seq uint64, evt *abiv1.Event) {
	if evt.GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS {
		if f.busy == nil {
			f.busy = map[string]bool{}
		}
		switch evt.GetStatus() {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY, abiv1.SessionStatus_SESSION_STATUS_COMPACTING:
			f.busy[evt.GetSessionId()] = true
		default:
			delete(f.busy, evt.GetSessionId())
		}
	}
	f.onEvent(evt, seq)
	sessions := map[string]*abiv1.SessionSnapshot{}
	for sid := range f.busy {
		sessions[sid] = &abiv1.SessionSnapshot{SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}
	}
	f.onUpdate(&abiclient.SessionState{Seq: seq, Sessions: sessions})
}

type recordedUsage struct {
	workspaceID string
	usage       Usage
}

type recordedBridge struct {
	statuses  []string // "ws:sid:busy|idle"
	inputs    []*abiv1.InputRequest
	resolved  []string
	titles    []string
	contexts  []string // "ws:sid:used"
	died      []string
	statusInt []string // "ws:sid:internal" — the RAW internal id the consumer passed through the additive seam (#1817 r3 finding 1)
	resInt    []string // "ws:sid:internal" — resolved-input additive seam
	ctxInt    []string // "ws:sid:internal" — context-usage additive seam
}

func (r *recordedBridge) SessionStatus(workspaceID, sessionID string, busy bool) {
	r.statuses = append(r.statuses, workspaceID+":"+sessionID+":"+boolWord(busy))
}
func (r *recordedBridge) InputRequested(workspaceID string, req *abiv1.InputRequest) {
	r.inputs = append(r.inputs, req)
}
func (r *recordedBridge) InputResolved(workspaceID, sessionID, inputID string) {
	r.resolved = append(r.resolved, workspaceID+":"+sessionID+":"+inputID)
}
func (r *recordedBridge) SessionTitle(workspaceID, sessionID, title string) {
	r.titles = append(r.titles, workspaceID+":"+sessionID+":"+title)
}
func (r *recordedBridge) ContextUsed(workspaceID, sessionID string, used int64) {
	r.contexts = append(r.contexts, workspaceID+":"+sessionID+":"+fmtInt(used))
}
func (r *recordedBridge) AgentDied(workspaceID string) {
	r.died = append(r.died, workspaceID)
}

// BridgeWithInternal — records the RAW internal id the consumer threads
// through the additive seam. The #1817 r3 finding-1 pin asserts this is
// agentd's internal_session_id verbatim (EMPTY for a legacy frame), NOT the
// wire session id (which the consumer's consult fallback would substitute).
func (r *recordedBridge) SessionStatusWithInternal(workspaceID, sessionID, internalID string, busy bool) {
	r.statuses = append(r.statuses, workspaceID+":"+sessionID+":"+boolWord(busy))
	r.statusInt = append(r.statusInt, workspaceID+":"+sessionID+":"+internalID)
}
func (r *recordedBridge) InputResolvedWithInternal(workspaceID, sessionID, internalID, inputID string) {
	r.resolved = append(r.resolved, workspaceID+":"+sessionID+":"+inputID)
	r.resInt = append(r.resInt, workspaceID+":"+sessionID+":"+internalID)
}
func (r *recordedBridge) ContextUsedWithInternal(workspaceID, sessionID, internalID string, used int64) {
	r.contexts = append(r.contexts, workspaceID+":"+sessionID+":"+fmtInt(used))
	r.ctxInt = append(r.ctxInt, workspaceID+":"+sessionID+":"+internalID)
}

func boolWord(b bool) string {
	if b {
		return "busy"
	}
	return "idle"
}

func fmtInt(i int64) string {
	return strconv.FormatInt(i, 10)
}

func newTestConsumer(t *testing.T) (*Consumer, *fakeClient, *[]recordedUsage, *recordedBridge) {
	t.Helper()
	connected := make(chan struct{}, 4)
	fc := &fakeClient{connected: connected}
	var usage []recordedUsage
	br := &recordedBridge{}
	c := New(Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			return "http://pod", "pw", nil
		},
		NewClient: func(baseURL, password string) Client {
			return fc
		},
		Billing: BillingFunc(func(workspaceID string, u Usage) {
			usage = append(usage, recordedUsage{workspaceID: workspaceID, usage: u})
		}),
		Bridge:   br,
		Logger:   nopLogger{},
		IdleDrop: 50 * time.Millisecond,
		Retry:    5 * time.Millisecond,
	})
	return c, fc, &usage, br
}

func requireOpen(t *testing.T, c *Consumer, fc *fakeClient) {
	t.Helper()
	c.Open("ws1")
	select {
	case <-fc.connected:
	case <-time.After(2 * time.Second):
		t.Fatal("gate never connected")
	}
}

func TestOpenConnectsOnce(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	c.Open("ws1")
	<-fc.connected
	c.Open("ws1") // idempotent
	select {
	case <-fc.connected:
		t.Fatal("second connection opened for the same workspace")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestMessageEndBillsUsageWithContext(t *testing.T) {
	c, fc, usage, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.apply(7, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_MESSAGE_END,
		SessionId: "s1",
		MessageId: "msg_1",
		Message: &abiv1.Message{
			Id: "msg_1", SessionId: "s1", Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
			Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "opencode"},
			Cost:  &abiv1.Cost{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 10, CacheWriteTokens: 5, CostUsd: 0.002},
		},
	})

	require.Len(t, *usage, 1)
	u := (*usage)[0]
	require.Equal(t, "ws1", u.workspaceID)
	require.Equal(t, Usage{
		SessionID: "s1", MessageID: "msg_1", Seq: 7,
		ModelID: "glm-5.3", ProviderID: "opencode",
		InputTokens: 100, OutputTokens: 40, CacheReadTokens: 10, CacheWriteTokens: 5,
		CostUSD: 0.002,
	}, u.usage)

	// Context numerator = input + cacheRead + cacheWrite (per-step
	// occupancy — the same formula the old ContextUsageFromEvent used).
	require.Equal(t, []string{"ws1:s1:115"}, br.contexts)
}

func TestMessageEndSkipsZeroCostAndUserMessages(t *testing.T) {
	c, fc, usage, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.apply(1, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_END, SessionId: "s1", MessageId: "m0",
		Message: &abiv1.Message{Id: "m0", SessionId: "s1", Type: abiv1.MessageType_MESSAGE_TYPE_USER, Cost: &abiv1.Cost{InputTokens: 12}},
	})
	fc.apply(2, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_END, SessionId: "s1", MessageId: "m1",
		Message: &abiv1.Message{Id: "m1", SessionId: "s1", Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT},
	})

	require.Empty(t, *usage)
	require.Empty(t, br.contexts)
}

func TestSessionStatusBridgesBusyIdle(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_COMPACTING})
	fc.apply(3, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:busy", "ws1:s1:idle"}, br.statuses)
}

// TestAdditiveSeamPassesRawInternalID (#1817 piece 3, r3 finding 1): the
// additive seam must carry agentd's RAW internal_session_id — EMPTY for a
// legacy / heal-window / agentd-unresolved frame (wire session_id = the
// harness id) — NOT consultSessionID, which falls back to the wire id when
// the internal is empty. If the consumer passed the wire id, a legacy frame
// would reach the bridge with internalID==sessionID, the bridge's
// double-translation guard would read it as "already translated" and skip
// the phrase translation, silently re-breaking the heal-window join this PR
// claims to preserve. The recordedBridge implements BridgeWithInternal and
// records the exact internal id threaded through the seam.
func TestAdditiveSeamPassesRawInternalID(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	// LEGACY frame: wire session_id = the harness id, NO internal_session_id
	// (agentd un-refreshed / unresolved). The consumer must pass internal="".
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	// REFRESHED frame: wire = the phrase, internal = the harness id.
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "phrase-s2", InternalSessionId: "harness-s2", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})

	require.Equal(t, []string{"ws1:s1:", "ws1:phrase-s2:harness-s2"}, br.statusInt,
		"the additive seam must carry the RAW internal_session_id: EMPTY for a legacy frame (so the bridge still translates to the phrase), and the harness id for a refreshed frame — NOT the wire id (which the consult fallback would substitute and which the bridge's guard would misread as already-translated)")
}

func TestInputLifecycleBridges(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	req := &abiv1.InputRequest{Id: "q1", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Go?"}
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, SessionId: "s1", Input: req})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, SessionId: "s1", Input: &abiv1.InputRequest{Id: "q1", SessionId: "s1"}})

	require.Len(t, br.inputs, 1)
	require.Equal(t, "q1", br.inputs[0].GetId())
	require.Equal(t, []string{"ws1:s1:q1"}, br.resolved)
}

func TestSessionUpdatedBridgesTitle(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "new title"}})

	require.Equal(t, []string{"ws1:s1:new title"}, br.titles)
}

func TestStreamErrorAfterFramesSignalsDeathAndRetries(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	c.Open("ws1")
	<-fc.connected

	// Simulate the stream dying mid-connection after delivering frames:
	// cancel the gate's stream ctx by closing over it via Close+reopen is
	// heavyweight; instead drive the error path directly through the
	// consumer's error handling seam.
	c.handleError("ws1", true)

	require.Equal(t, []string{"ws1"}, br.died)
}

func TestIdleDropClosesGate(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	// No busy sessions ever observed: the settle window elapses and the
	// gate drops the connection (scale-to-zero).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Gates() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gate never dropped after idle")
}

func TestBusySessionHoldsGate(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	time.Sleep(150 * time.Millisecond) // > IdleDrop
	require.Equal(t, 1, c.Gates(), "busy session must hold the gate open")
}

func TestCloseCancelsGate(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	c.Close("ws1")
	require.Equal(t, 0, c.Gates())
}

type nopLogger struct{}

func (nopLogger) Warn(string, ...interface{}) {}

// TestGateChangeHook (US-69.11): the optional metrics seam fires
// open=true on arm and open=false exactly once per drop — whether the
// drop is an explicit Close or the idle window (scale-to-zero). The
// hook is the llmsafespaces_usage_stream_gates gauge's event source.
func TestGateChangeHook(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	events := make(chan bool, 8)
	c.cfg.OnGateChange = func(ws string, open bool) {
		require.Equal(t, "ws1", ws)
		events <- open
	}

	c.Open("ws1")
	<-fc.connected
	require.True(t, <-events, "arming the gate fires open")

	// Idempotent re-arm inside the idle window: no duplicate event
	// (newTestConsumer's IdleDrop is 50ms — stay well inside it).
	c.Open("ws1")
	select {
	case v := <-events:
		t.Fatalf("idempotent re-open must not re-fire, got %v", v)
	case <-time.After(20 * time.Millisecond):
	}

	// Idle window elapses (nothing ever busy): the gate self-drops →
	// close, exactly once.
	select {
	case open := <-events:
		require.False(t, open, "idle-drop fires close")
	case <-time.After(2 * time.Second):
		t.Fatal("idle-drop never fired the close hook")
	}
	require.Zero(t, c.Gates())

	// Re-arm, hold busy (no idle-drop race), then explicit Close: close
	// fires once (Close's delete wins; run's deferred identity check
	// must not double-fire).
	c.Open("ws1")
	<-fc.connected
	require.True(t, <-events, "re-arm fires open again")
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1",
		Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	c.Close("ws1")
	select {
	case open := <-events:
		require.False(t, open, "explicit Close fires close")
	case <-time.After(2 * time.Second):
		t.Fatal("Close never fired the close hook")
	}
	select {
	case v := <-events:
		t.Fatalf("double close event, got %v", v)
	case <-time.After(150 * time.Millisecond):
	}
}

// --- #1602: the session.status bridge carries the authority's derived
// busy truth (#1574 BusyComponents — read, never recompute) -----------

// setSnapshot scripts the fake's GetSnapshot answer for a session.
func (f *fakeClient) setSnapshot(sid string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshots == nil {
		f.snapshots = map[string]*abiv1.SessionSnapshot{}
	}
	f.snapshots[sid] = &abiv1.SessionSnapshot{SessionId: sid, Busy: &abiv1.BusyComponents{Busy: busy, Streaming: busy}}
}

// failSnapshot arms a permanent GetSnapshot failure (fail-open leg).
func (f *fakeClient) failSnapshot(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotErr = err
}

func (f *fakeClient) GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	if s, ok := f.snapshots[sessionID]; ok {
		return s, nil
	}
	return nil, errNoSuchSession(sessionID)
}

// TestRunningToolBridgesBusyTruth: THE acceptance row at the bridge
// level. The harness reports IDLE (streaming-only semantics) while the
// authority's components say busy (a bash tool mid-execution) — the
// bridge must publish BUSY, not the raw idle. The reverse leg: the
// harness's BUSY mark while the authority carved the session out
// (permission wait, nothing else in flight) publishes IDLE.
func TestRunningToolBridgesBusyTruth(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.setSnapshot("s1", true) // bash mid-execution: derived busy
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	require.Equal(t, []string{"ws1:s1:busy"}, br.statuses,
		"the intermediate idle must be overlaid with the authority's busy truth")

	fc.setSnapshot("s2", false) // permission wait: the carve-out leg
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s2", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s2:idle"}, br.statuses,
		"a tracker-side busy the authority does not back must publish idle")
}

// TestPartStartBridgesBusyFlip: busy flips that happen WITHOUT any
// status event (the tool part starts after the harness's idle) are the
// incident's silent class — the candidate consult catches the flip and
// publishes it. One emission per flip, not per event.
func TestPartStartBridgesBusyFlip(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.setSnapshot("s1", false)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	require.Equal(t, []string{"ws1:s1:idle"}, br.statuses)

	fc.setSnapshot("s1", true) // the tool starts: derived busy flips
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1",
		Part: &abiv1.Part{Id: "p1", Type: abiv1.PartType_PART_TYPE_TOOL}})
	fc.apply(3, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_DELTA, SessionId: "s1", PartId: "p1", Delta: "x"})
	fc.apply(4, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_END, SessionId: "s1", PartId: "p1",
		Part: &abiv1.Part{Id: "p1", Type: abiv1.PartType_PART_TYPE_TOOL}})
	require.Equal(t, []string{"ws1:s1:idle", "ws1:s1:busy"}, br.statuses,
		"one busy emission on the flip; the delta/end events that keep it busy must not re-emit")

	fc.setSnapshot("s1", false) // the turn truly ends
	fc.apply(5, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_END, SessionId: "s1", PartId: "p1",
		Part: &abiv1.Part{Id: "p1", Type: abiv1.PartType_PART_TYPE_TOOL}})
	require.Equal(t, []string{"ws1:s1:idle", "ws1:s1:busy", "ws1:s1:idle"}, br.statuses,
		"the flip back to idle publishes exactly once")
}

// TestErrorEventClearsBusyViaAuthority: an ERROR event is a candidate —
// the vetoed derivation (busy=false) reaches the bridge, clearing an
// indicator that today's raw path leaves stuck busy (ERROR status
// events were never bridged at all).
func TestErrorEventClearsBusyViaAuthority(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.setSnapshot("s1", true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	require.Equal(t, []string{"ws1:s1:busy"}, br.statuses)

	fc.setSnapshot("s1", false) // terminal ERROR vetoes busy in the derivation
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_ERROR, SessionId: "s1", Error: &abiv1.Error{Code: "turn.failed"}})
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle"}, br.statuses)
}

// TestConsultFailureFailsOpenToRawStatus: GetSnapshot unavailable (pod
// glitch, projection-unknown session) degrades to today's raw-status
// bridging — the pipe never goes quiet, it goes legacy.
func TestConsultFailureFailsOpenToRawStatus(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.failSnapshot(errors.New("pod unavailable"))
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle"}, br.statuses)

	// Non-status candidates during a failure publish nothing new.
	n := len(br.statuses)
	fc.apply(3, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1"})
	require.Len(t, br.statuses, n)
}

// TestDerivedBusyHoldsIdleGate: the idle-drop gate must not tear down
// the subscription while the DERIVED truth says busy — the raw fold
// reads all-idle during a silent tool run (the intermediate harness
// idle), and a drop there would miss the final idle entirely (the
// stuck-busy aggravator). When the derived truth goes idle, the settle
// window runs and the gate drops as designed.
func TestDerivedBusyHoldsIdleGate(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	// The harness went idle; the tool is still running (derived busy).
	fc.setSnapshot("s1", true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1"})

	// Long enough for the idle watchdog to have dropped an
	// un-OR'd gate several times over.
	time.Sleep(4 * c.cfg.IdleDrop)
	require.Equal(t, 1, c.Gates(), "derived busy must hold the gate open through the silent tool run")

	// The turn truly ends: the derived truth goes idle, the settle
	// window elapses, the gate drops (scale-to-zero preserved).
	fc.setSnapshot("s1", false)
	fc.apply(3, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_END, SessionId: "s1", PartId: "p1"})
	require.Eventually(t, func() bool { return c.Gates() == 0 }, 5*time.Second, 50*time.Millisecond,
		"all-idle derived truth must still drop the gate")
}

// TestReconnectReconcilesDerivedTruth: a stale-true derived entry (the
// consult truth from a PREVIOUS connection) must not hold the gate
// open forever — the reconnect's snapshot publication is the authority's
// current answer, and the flip baseline rebuilds from it. Without the
// reconcile, a pod that moved on (session now idle, no events coming)
// keeps one stream connection per workspace leased indefinitely — the
// scale-to-zero violation.
func TestReconnectReconcilesDerivedTruth(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.setSnapshot("s1", true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1"})
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, c.Gates(), "precondition: derived busy holds the gate")

	// The pod moves on: the session is idle now (the reconnect's fold
	// publishes all-idle; the consult answers not-busy).
	fc.setSnapshot("s1", false)
	fc.killStream()

	require.Eventually(t, func() bool { return c.Gates() == 0 },
		5*time.Second, 50*time.Millisecond,
		"a stale-true derived entry must reconcile against the reconnect snapshot — the gate must drop")
}

// reseed simulates abiclient's IN-STREAM projection.reseeded handling:
// the client redials, applies the fresh snapshot, fires the resynced
// callback, and republishes — WITHOUT Stream returning (the run loop
// and its per-connection resets never observe it).
func (f *fakeClient) reseed(sessions map[string]*abiv1.SessionSnapshot) {
	f.mu.Lock()
	f.busy = nil
	resynced := f.resynced
	f.mu.Unlock()
	if resynced != nil {
		resynced()
	}
	f.onUpdate(&abiclient.SessionState{Seq: 0, Sessions: sessions})
}

// TestInStreamReseedReconcilesDerivedTruth: the review-r1 gap — the
// reseed redial happens inside abiclient.Stream (no return, no run-loop
// reconnect), so the per-connection seededFold reset never fires. A
// stale-true derived entry across an in-stream reseed must still
// reconcile against the fresh snapshot — the no-stale-lease invariant
// holds on this path too.
func TestInStreamReseedReconcilesDerivedTruth(t *testing.T) {
	c, fc, _, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	fc.setSnapshot("s1", true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	fc.apply(2, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1"})
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, c.Gates(), "precondition: derived busy holds the gate")

	// The authority reseeds from a store that says idle — the fresh
	// snapshot (all idle, no Busy components) is the new truth.
	fc.setSnapshot("s1", false)
	fc.reseed(map[string]*abiv1.SessionSnapshot{})

	require.Eventually(t, func() bool { return c.Gates() == 0 },
		5*time.Second, 50*time.Millisecond,
		"an in-stream reseed's snapshot must rebuild the derived baseline — the gate must drop")
}

// recordingLogger captures warns (the fail-open observability row).
type recordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingLogger) Warn(msg string, keysAndValues ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

func (l *recordingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

// TestConsultFailureIsObservable: a persistently failing consult must
// not be silent — the fix's entire purpose is the truth overlay, and a
// silent degrade would reproduce the incident with no signal. Warn on
// the first failure, rate-limited afterwards (every 50th).
func TestConsultFailureIsObservable(t *testing.T) {
	connected := make(chan struct{}, 4)
	fc := &fakeClient{connected: connected}
	lg := &recordingLogger{}
	br := &recordedBridge{}
	c := New(Config{
		Resolve:   func(ctx context.Context, workspaceID string) (string, string, error) { return "http://pod", "pw", nil },
		NewClient: func(baseURL, password string) Client { return fc },
		Bridge:    br,
		Logger:    lg,
		IdleDrop:  time.Hour,
	})
	requireOpen(t, c, fc)

	fc.failSnapshot(errors.New("pod glitch"))
	for i := 1; i <= 3; i++ {
		fc.apply(uint64(i), &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	}
	require.Len(t, br.statuses, 3, "the raw fail-open mapping still publishes")
	require.Equal(t, 1, lg.count(), "first failure warns; the next two are rate-limited silent")

	for i := 4; i <= 52; i++ {
		fc.apply(uint64(i), &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	}
	require.Equal(t, 2, lg.count(), "the 51st failure warns again (every 50th)")
}

// --- #1817 piece 2: the consult keys the authority by the INTERNAL
// (harness) id, not the emitted phrase -------------------------------

// TestConsultResolvesPhraseToHarness: the emission carries the PHRASE as
// the wire session_id (the #1817/#1821 piece-1 translation) with the
// harness id riding ADDITIVELY on internal_session_id. The authority's
// GetSnapshot map is harness-id-keyed, so a consult that passed the
// phrase would 404 and fail open to the raw status — re-creating the
// sporadic-busy this fixes. The consult must resolve phrase→harness via
// the additive field.
func TestConsultResolvesPhraseToHarness(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	const phrase = "amber-river-7"
	const harness = "ses_harness_1817"
	fc.setSnapshot(harness, true)
	fc.apply(1, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId:         phrase,
		InternalSessionId: harness,
		Status:            abiv1.SessionStatus_SESSION_STATUS_IDLE,
	})
	require.Equal(t, []string{"ws1:" + phrase + ":busy"}, br.statuses,
		"the consult must key the authority by the harness id (internal_session_id) — a phrase key 404s and fails open to the raw idle")
}

// TestConsultLegacyEventFallsBackToSessionId: a pre-piece-1 event (no
// additive field) carries the harness id on the wire session_id. The
// consult must fall back to it, so legacy emitters keep consulting —
// never worse than today.
func TestConsultLegacyEventFallsBackToSessionId(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	const harness = "ses_legacy_1817"
	fc.setSnapshot(harness, true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: harness, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	require.Equal(t, []string{"ws1:" + harness + ":busy"}, br.statuses,
		"an event without internal_session_id must consult on the wire session_id")
}

// TestConsultEmptyInternalFallsBack: the additive field present but EMPTY
// (an un-refreshed emitter that set the field without a value) must fall
// back to the wire session_id, not consult an empty key.
func TestConsultEmptyInternalFallsBack(t *testing.T) {
	c, fc, _, br := newTestConsumer(t)
	requireOpen(t, c, fc)

	const harness = "ses_empty_internal_1817"
	fc.setSnapshot(harness, true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: harness, InternalSessionId: "", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	require.Equal(t, []string{"ws1:" + harness + ":busy"}, br.statuses,
		"an empty internal_session_id must fall back to the wire session_id, never consult an empty key")
}

// --- #1666: the reconnect/resync snapshot fold reconciles the BRIDGE
// onto the post-reconnect busy truth (the backend twin of #1646) ------

// newReconcileFakeConsumer wires the scripted fake with the
// mutex-guarded bridge (the reconnect path runs on the consumer's
// goroutine — the recorded bridge is for synchronous drives only).
func newReconcileFakeConsumer(t *testing.T) (*Consumer, *fakeClient, *syncBridge) {
	t.Helper()
	connected := make(chan struct{}, 4)
	fc := &fakeClient{connected: connected}
	br := &syncBridge{}
	c := New(Config{
		Resolve:   func(ctx context.Context, workspaceID string) (string, string, error) { return "http://pod", "pw", nil },
		NewClient: func(baseURL, password string) Client { return fc },
		Bridge:    br,
		IdleDrop:  time.Hour,
		Retry:     5 * time.Millisecond,
	})
	return c, fc, br
}

// TestReconnectSeedReconcilesBridgeTruth: the run-level reconnect row —
// the busy frame died with the old connection (no replay), so the truth
// arrives only via the NEXT connection's snapshot fold. The consumer's
// derived baseline already rebuilds from it (#1664); the BRIDGE must be
// reconciled onto it too — a never-heard session folding busy gets its
// busy emission at the reconnect seed, not never.
func TestReconnectSeedReconcilesBridgeTruth(t *testing.T) {
	c, fc, br := newReconcileFakeConsumer(t)
	requireOpen(t, c, fc)

	// The busy truth exists only in the next connection's snapshot.
	fc.setSeedSessions(map[string]*abiv1.SessionSnapshot{
		"s1": {SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY, Busy: &abiv1.BusyComponents{Busy: true}},
	})
	fc.killStream()

	require.Eventually(t, func() bool {
		got := br.snapshot()
		return len(got) == 1 && got[0] == "ws1:s1:busy"
	}, 5*time.Second, 10*time.Millisecond,
		"the reconnect's seeded fold must reconcile the bridge onto its busy truth")
}

// TestReseedSeedReconcilesBridgeFlip: the in-stream resync row, both
// disagreement directions, synchronously driven. A bridged-busy session
// whose fresh fold says idle gets the busy=false correction (the
// stuck-busy indicator's only cure on a quiet stream); an agreeing
// re-seed stays silent (a diff, not a spray).
func TestReseedSeedReconcilesBridgeFlip(t *testing.T) {
	c, fc, br := newReconcileFakeConsumer(t)
	requireOpen(t, c, fc)

	// The bridge heard busy (the consult-backed event leg).
	fc.setSnapshot("s1", true)
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	require.Equal(t, []string{"ws1:s1:busy"}, br.snapshot())

	// The fold's truth flips while the stream is dead; the resync's
	// snapshot carries the fresh (idle) truth.
	fc.setSnapshot("s1", false)
	fc.reseed(map[string]*abiv1.SessionSnapshot{
		"s1": {SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE, Busy: &abiv1.BusyComponents{Busy: false}},
	})
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle"}, br.snapshot(),
		"a bridged-busy session folding idle must be corrected to idle at the resync seed")

	// Agreement: the same truth re-seeded again emits nothing.
	fc.reseed(map[string]*abiv1.SessionSnapshot{
		"s1": {SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE, Busy: &abiv1.BusyComponents{Busy: false}},
	})
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle"}, br.snapshot(),
		"an agreeing fold must not re-emit — the reconcile fires only on disagreement")
}

// #1817 piece 3 (finding 2): the GATE LATCH. On refreshed pods the
// subscribe snapshot is phrase-keyed (SessionId=phrase, the harness id
// additive as internal_session_id). The consumer seeds g.derived from it;
// consultBusy (the idle clear) keys g.derived by the HARNESS id. If the
// fold keyed the snapshot by the phrase, the stale phrase entry would
// survive the harness-keyed idle clear and OR the gate busy forever —
// the idle-drop never fires (the stuck-busy leak). The fix keys the fold
// by the internal (harness) id so the snapshot and the consult are the
// same session. This pin: a phrase-keyed busy snapshot (harness
// additive) followed by a harness-keyed idle event must leave the gate
// NOT busy (no stale phrase entry).
func TestGateNoLatch_PhraseKeyedSnapshotThenIdle(t *testing.T) {
	const harnessID = "ses_latch_1817"
	const phraseID = "foxtrot-peak-2"

	c, fc, _, _ := newTestConsumer(t)
	// The refreshed-pod snapshot: phrase-keyed, the harness id additive.
	fc.setSeedSessions(map[string]*abiv1.SessionSnapshot{
		phraseID: {
			SessionId:         phraseID,
			InternalSessionId: harnessID,
			Status:            abiv1.SessionStatus_SESSION_STATUS_BUSY,
			Busy:              &abiv1.BusyComponents{Busy: true, Streaming: true},
		},
	})
	// The authority's truth is keyed by the HARNESS id (consultBusy).
	fc.setSnapshot(harnessID, true)
	requireOpen(t, c, fc)

	// The fold seeded the snapshot (phrase-keyed, busy). The derived
	// baseline must be keyed by the HARNESS id, not the phrase.
	g := c.gates["ws1"]
	g.mu.Lock()
	_, keyedByPhrase := g.derived[phraseID]
	_, keyedByHarness := g.derived[harnessID]
	g.mu.Unlock()
	require.False(t, keyedByPhrase,
		"the fold's derived baseline must NOT be keyed by the phrase — a phrase entry the harness-keyed idle clear can't remove is the latch")
	require.True(t, keyedByHarness,
		"the fold's derived baseline must be keyed by the harness id (the consult's key) so the idle clear reaches it")

	// Now the session goes idle: the authority says idle, the event is
	// phrase-keyed with the harness id additive. The consult clears the
	// harness-keyed entry, and the fold (all-idle) must drop the gate.
	fc.setSnapshot(harnessID, false)
	fc.apply(1, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId:         phraseID,
		InternalSessionId: harnessID,
		Status:            abiv1.SessionStatus_SESSION_STATUS_IDLE,
	})

	// The gate must NOT stay latched busy. Wait briefly for the fold to
	// recompute (the idle-drop settle window is 50ms in this fixture).
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return !g.busyNow
	}, 2*time.Second, 10*time.Millisecond,
		"the gate must drop to idle after the idle event — a stale phrase-keyed derived entry would hold it busy forever (the latch)")
}
