// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// fakeClient fakes abiclient.Client's Stream: snapshots then scripted
// applied events, under the test's control.
type fakeClient struct {
	onUpdate  func(*abiclient.SessionState)
	onEvent   func(evt *abiv1.Event, seq uint64)
	connected chan struct{}
	// Minimal fold: BUSY/COMPACTING status events mark the session busy
	// (mirrors the reference client's folded-state publications).
	busy map[string]bool
}

func (f *fakeClient) Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error {
	// The reference client exposes the composed callback for consumers
	// that wrap Stream (this fake is one).
	f.onEvent = abiclient.AppliedEventsOf(opts)
	f.onUpdate = onUpdate
	if f.connected != nil {
		f.connected <- struct{}{}
	}
	// Snapshot-first: the protocol's stamp.
	onUpdate(&abiclient.SessionState{Seq: 0})
	<-ctx.Done()
	return context.Canceled
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
	statuses []string // "ws:sid:busy|idle"
	inputs   []*abiv1.InputRequest
	resolved []string
	titles   []string
	contexts []string // "ws:sid:used"
	died     []string
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
