// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
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
