// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestRecordStepUsage_DeterministicKeys: the billing idempotency keys
// are a pure function of (workspace, message, seq) — two replicas
// consuming the same pod stream generate identical keys, so the
// usage_events unique constraint enforces exactly-once billing.
func TestRecordStepUsage_DeterministicKeys(t *testing.T) {
	h := newUsageBillingTestHandler(t)
	defer h.Stop()

	var inference int
	var keySets []map[string]bool
	var lastEvents []types.UsageEvent
	h.SetUsageBilling(
		func(modelID, providerID string, in, out int64, cost float64) {
			inference++
		},
		func(e types.UsageEvent) { lastEvents = append(lastEvents, e) },
	)
	keys := func() map[string]bool {
		k := map[string]bool{}
		for _, e := range lastEvents {
			k[e.IdempotencyKey] = true
		}
		keySets = append(keySets, k)
		lastEvents = nil
		return k
	}

	h.recordStepUsage("ws1", stepUsageHelper("msg_1", 7, 100, 40))
	first := keys()
	h.recordStepUsage("ws1", stepUsageHelper("msg_1", 7, 100, 40))
	// The second call models a second replica consuming the same pod
	// stream: the generated key SET must be identical — the DB's unique
	// constraint then turns the race into an at-most-once insert.
	second := keys()
	require.Equal(t, first, second)
	require.Contains(t, first, "tokens:ws1:msg_1:7:in")
	require.Contains(t, first, "tokens:ws1:msg_1:7:out")
	require.Equal(t, 2, inference)

	// A different step (message or seq) bills under different keys.
	h.recordStepUsage("ws1", stepUsageHelper("msg_2", 8, 10, 5))
	require.NotEqual(t, first, keys())
}

// TestRecordStepUsage_SkipsWithoutSinks: no owner or no metering sink →
// no records, no panic.
func TestRecordStepUsage_SkipsWithoutSinks(t *testing.T) {
	h := newUsageBillingTestHandler(t)
	defer h.Stop()

	var events []types.UsageEvent
	h.SetUsageBilling(nil, func(e types.UsageEvent) { events = append(events, e) })
	// No workspace owner resolvable (no broker) → dropped.
	h.recordStepUsage("ws-unknown", stepUsageHelper("m", 1, 10, 5))
	require.Empty(t, events)
}

func TestQuestionRequestFromABI(t *testing.T) {
	req := &abiv1.InputRequest{
		Id: "q1", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
		Question: "Go?", Header: "Confirm",
		Options:  []*abiv1.InputOption{{Label: "yes", Description: "y"}, {Label: "no"}},
		Multiple: true,
		Tool:     &abiv1.ToolRef{MessageId: "m1", CallId: "c1"},
	}
	q := questionRequestFromABI(req, "root1")
	require.Equal(t, "q1", q.ID)
	require.Equal(t, "s1", q.SessionID)
	require.Equal(t, "root1", q.RootSessionID)
	require.Len(t, q.Questions, 1)
	require.Equal(t, "Go?", q.Questions[0].Question)
	require.Len(t, q.Questions[0].Options, 2)
	require.True(t, q.Questions[0].Multiple)
	require.NotNil(t, q.Tool)
	require.Equal(t, "c1", q.Tool.CallID)
}

func TestPermissionRequestFromABI(t *testing.T) {
	req := &abiv1.InputRequest{
		Id: "p1", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_PERMISSION,
		Permission: "shell", Patterns: []string{"ls"}, Always: []string{"bash"},
	}
	p := permissionRequestFromABI(req, "")
	require.Equal(t, "p1", p.ID)
	require.Equal(t, "shell", p.Permission)
	require.Equal(t, []string{"ls"}, p.Patterns)
	require.Empty(t, p.RootSessionID)
}

func stepUsageHelper(messageID string, seq uint64, in, out int64) usagestream.Usage {
	return usagestream.Usage{
		SessionID: "s1", MessageID: messageID, Seq: seq,
		ModelID: "glm-5.3", ProviderID: "opencode",
		InputTokens: in, OutputTokens: out,
	}
}

// newUsageBillingTestHandler builds a minimal ProxyHandler for billing
// wiring tests (no K8s interaction needed on these paths).
func newUsageBillingTestHandler(t *testing.T) *ProxyHandler {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())
	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", http.DefaultClient, nil)
	require.NoError(t, err)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws1", "u1")
	return h
}

// --- US-69.11 gate-test seams -------------------------------------------------
//
// The usagestream consumer is a process-wide singleton on the handler
// (UsageStream). Handler-level tests that exercise the arming paths
// (onPhaseChange, Start, stateReconciler, StreamEvents) inject a consumer
// whose Client is a recording fake — connections, disconnects, and
// event pushes are all observable without a pod.

// gateTestClient fakes the abiclient stream one gate holds: it records
// each connection, captures the fold/applied-events callbacks, and lets
// the test push ABI events exactly as a pod stream would.
type gateTestClient struct {
	mu           sync.Mutex
	connected    chan struct{}
	disconnected chan struct{}
	onUpdate     func(*abiclient.SessionState)
	onEvent      func(evt *abiv1.Event, seq uint64)
	// minimal fold: BUSY/COMPACTING status events mark the session busy
	// (mirrors the reference client's folded-state publications).
	busy map[string]bool
}

func (g *gateTestClient) Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error {
	g.mu.Lock()
	g.onUpdate = onUpdate
	g.onEvent = abiclient.AppliedEventsOf(opts)
	g.mu.Unlock()
	// Snapshot-first: the protocol's stamp.
	onUpdate(&abiclient.SessionState{Seq: 0})
	if g.connected != nil {
		g.connected <- struct{}{}
	}
	<-ctx.Done()
	if g.disconnected != nil {
		g.disconnected <- struct{}{}
	}
	return context.Canceled
}

// apply pushes one ABI event through the captured callbacks (and the
// minimal fold), as the pod stream would.
func (g *gateTestClient) apply(seq uint64, evt *abiv1.Event) {
	g.mu.Lock()
	onUpdate, onEvent := g.onUpdate, g.onEvent
	g.mu.Unlock()
	if onEvent == nil || onUpdate == nil {
		return
	}
	if evt.GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS {
		if g.busy == nil {
			g.busy = map[string]bool{}
		}
		switch evt.GetStatus() {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY, abiv1.SessionStatus_SESSION_STATUS_COMPACTING:
			g.busy[evt.GetSessionId()] = true
		default:
			delete(g.busy, evt.GetSessionId())
		}
	}
	onEvent(evt, seq)
	sessions := map[string]*abiv1.SessionSnapshot{}
	for sid := range g.busy {
		sessions[sid] = &abiv1.SessionSnapshot{SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}
	}
	onUpdate(&abiclient.SessionState{Seq: seq, Sessions: sessions})
}

// newRecordingGateConsumer builds a consumer whose gates all share the
// recording client. Resolve never fails and is recorded per workspace
// (the observable for "which workspaces got armed"). IdleDrop is parked
// at an hour: tests tear gates down explicitly via Close.
func newRecordingGateConsumer(bridge usagestream.Bridge) (*usagestream.Consumer, *gateTestClient, *[]string) {
	fc := &gateTestClient{connected: make(chan struct{}, 32)}
	var mu sync.Mutex
	var resolved []string
	c := usagestream.New(usagestream.Config{
		Resolve: func(_ context.Context, workspaceID string) (string, string, error) {
			mu.Lock()
			resolved = append(resolved, workspaceID)
			mu.Unlock()
			return "http://pod", "pw", nil
		},
		NewClient: func(baseURL, password string) usagestream.Client { return fc },
		Bridge:    bridge,
		Logger:    &testLogger{},
		IdleDrop:  time.Hour,
		Retry:     10 * time.Millisecond,
	})
	return c, fc, &resolved
}

// injectUsageStream swaps the handler's process-wide consumer singleton
// for the test's and returns a restore func. Handler tests run
// sequentially, so the package-level once/ptr swap is safe.
func injectUsageStream(c *usagestream.Consumer) (restore func()) {
	savedPtr := usageStreamPtr
	usageStreamOnce = sync.Once{} //nolint:copylocks // test-only singleton swap; no gate goroutine touches the Once concurrently
	usageStreamPtr = nil
	usageStreamOnce.Do(func() { usageStreamPtr = c })
	return func() {
		usageStreamOnce = sync.Once{}
		usageStreamPtr = savedPtr
	}
}

// stubUsageStream injects a never-connecting consumer so handler paths
// that arm gates on Active phases (onPhaseChange etc.) stay hermetic in
// tests that don't care about the stream itself.
func stubUsageStream() func() {
	c, _, _ := newRecordingGateConsumer(nil)
	return injectUsageStream(c)
}
