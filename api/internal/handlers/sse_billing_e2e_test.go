// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"go.uber.org/zap"

	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"

	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestE2E_SSETracker_InferencePipeline_CostAsObject verifies the full
// pipeline from SSE event arrival through tracker dispatch to the inference
// callback when cost arrives as an object (opencode 1.18.10 wire shape).
// This proves the wiring is correct end-to-end, not just the tracker's
// isolated parsing logic.
func TestE2E_SSETracker_InferencePipeline_CostAsObject(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-billing", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-billing")
	env.setupPasswordWithT(t, "ws-billing", "test-password")
	env.setupWorkspaceWithT(t, "ws-billing", 5)

	tracker := sse.NewTracker(env.handler.httpClient, env.log, func(_, _ string) {})
	env.handler.sseTracker = tracker

	type inferenceCall struct {
		modelID      string
		providerID   string
		inputTokens  int64
		outputTokens int64
		costDollars  float64
	}
	var mu sync.Mutex
	var calls []inferenceCall

	tracker.SetOnInference(func(workspaceID, modelID, providerID string, inputTokens, outputTokens int64, costDollars float64) {
		mu.Lock()
		calls = append(calls, inferenceCall{
			modelID:      modelID,
			providerID:   providerID,
			inputTokens:  inputTokens,
			outputTokens: outputTokens,
			costDollars:  costDollars,
		})
		mu.Unlock()
	})
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)

	// Send session.updated with cost as an object (potential 1.18.10 wire shape).
	// In ocCost, "cost" is CostUSD (dollar amount), not "total" (token count).
	// Uses legacy "provider" key (1.15.x wire shape).
	tracker.ProcessEvent("ws-billing", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_billing",
			"info": {
				"id": "ses_billing",
				"cost": {"cost": 0.042},
				"tokens": {"input": 5000, "output": 1200},
				"model": {"id": "glm-5.2", "provider": "thekaocloud"}
			}
		}
	}`)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, calls, 1, "inference callback must fire exactly once")
	c := calls[0]
	assert.Equal(t, "glm-5.2", c.modelID, "model ID must be parsed from info.model.id")
	assert.Equal(t, "thekaocloud", c.providerID, "provider must fall back to legacy 'provider' key")
	assert.Equal(t, int64(5000), c.inputTokens, "first event bills full input")
	assert.Equal(t, int64(1200), c.outputTokens, "output delta must equal full output on first event")
	assert.Greater(t, c.costDollars, 0.0, "cost must be non-zero (parsed from object)")
}

// TestE2E_SSETracker_InferencePipeline_SecondEventDedup verifies that a
// second session.updated event for the same session bills only the output
// delta (not re-billing input tokens). This is the dedup logic that #759
// flagged as causing double-billing on pod restart — the in-memory state
// must correctly track previous output.
func TestE2E_SSETracker_InferencePipeline_SecondEventDedup(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tracker := sse.NewTracker(env.handler.httpClient, env.log, func(_, _ string) {})
	env.handler.sseTracker = tracker

	type inferenceCall struct {
		inputTokens  int64
		outputTokens int64
	}
	var mu sync.Mutex
	var calls []inferenceCall

	tracker.SetOnInference(func(_, _, _ string, inputTokens, outputTokens int64, _ float64) {
		mu.Lock()
		calls = append(calls, inferenceCall{inputTokens, outputTokens})
		mu.Unlock()
	})
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)

	// First event: bills full input + output.
	tracker.ProcessEvent("ws-dedup", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_dedup",
			"info": {
				"id": "ses_dedup",
				"tokens": {"input": 10000, "output": 500},
				"model": {"id": "gpt-4o", "providerID": "openai"}
			}
		}
	}`)

	// Second event: cumulative tokens — must bill only output delta, NOT input.
	tracker.ProcessEvent("ws-dedup", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_dedup",
			"info": {
				"id": "ses_dedup",
				"tokens": {"input": 10000, "output": 800},
				"model": {"id": "gpt-4o", "providerID": "openai"}
			}
		}
	}`)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, calls, 2, "inference callback must fire twice")
	assert.Equal(t, int64(10000), calls[0].inputTokens, "first event bills full input")
	assert.Equal(t, int64(500), calls[0].outputTokens, "first event bills full output")
	assert.Equal(t, int64(0), calls[1].inputTokens, "second event must NOT re-bill input (dedup)")
	assert.Equal(t, int64(300), calls[1].outputTokens, "second event bills output delta only (800-500)")
}

// TestE2E_PhaseChangeSuspend_CleansBillingMaps verifies that when a
// workspace transitions to Suspended, the handler calls StopWatching which
// cleans the per-session billing maps. Without this, suspended workspaces
// accumulate entries forever (the #751 F2 memory leak, validated end-to-end
// through the real proxy_events.go path).
func TestE2E_PhaseChangeSuspend_CleansBillingMaps(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-clean", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-clean")
	env.setupPasswordWithT(t, "ws-clean", "test-password")
	env.setupWorkspaceWithT(t, "ws-clean", 5)

	tracker := sse.NewTracker(env.handler.httpClient, env.log, func(_, _ string) {})
	env.handler.sseTracker = tracker
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)
	tracker.SetPasswordGetter(env.handler)
	tracker.SetPodIPResolver(func(string) string { return "10.0.0.1" })

	// Seed billing state by sending a session.updated event.
	// The decoder above is load-bearing: without it the metering guard
	// gates the seed out, the maps start empty, and the post-suspend
	// emptiness assertions below pass vacuously (mutation-verified in
	// #949 round 3).
	tracker.ProcessEvent("ws-clean", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_clean",
			"info": {
				"id": "ses_clean",
				"cost": 0.01,
				"tokens": {"input": 1000, "output": 500},
				"model": {"id": "gpt-4o", "providerID": "openai"}
			}
		}
	}`)

	// Verify maps are populated.
	tracker.ProcessEvent("ws-clean", `{
		"type": "session.status",
		"properties": {
			"sessionID": "ses_clean",
			"status": {"type": "busy"}
		}
	}`)

	// Simulate a suspend phase change — the handler must call StopWatching.
	wsObj := makeWorkspaceCRDWithStatus("ws-clean", "10.0.0.1", string(v1.WorkspacePhaseActive), "")
	wsObj.Status.Phase = v1.WorkspacePhaseSuspended
	env.handler.onPhaseChange(wsObj)

	// After StopWatching, the billing maps must be empty for this workspace.
	// Access the tracker's internal state to verify.
	require.False(t, tracker.IsWatching("ws-clean"),
		"tracker must stop watching after suspend phase change")

	// Verify the billing maps are actually cleaned — not just the subscription.
	// sessionTokenSeen and sessionCostSeen are keyed by "workspaceID:sessionID".
	tokenSeen, costSeen, startTimeEntries := tracker.GetBillingState("ws-clean")
	assert.Empty(t, tokenSeen, "sessionTokenSeen must be empty after StopWatching")
	assert.Empty(t, costSeen, "sessionCostSeen must be empty after StopWatching")
	assert.Empty(t, startTimeEntries, "sessionStartTime must be empty after StopWatching")
}

// TestE2E_PhaseChangeActive_StopThenEnsure_Cycle exercises the handler-level
// Creating→Active transition path (proxy_events.go:93-98). A real subscribe
// goroutine is started via EnsureWatching, then onPhaseChange calls
// StopWatching→EnsureWatching. Verifies billing maps are cleared and the
// WaitGroup drains the old goroutine before the new subscription starts.
func TestE2E_PhaseChangeActive_StopThenEnsure_Cycle(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-cycle", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-cycle")
	env.setupPasswordWithT(t, "ws-cycle", "test-password")
	env.setupWorkspaceWithT(t, "ws-cycle", 5)

	tracker := sse.NewTracker(env.handler.httpClient, env.log, func(_, _ string) {})
	env.handler.sseTracker = tracker
	tracker.SetPasswordGetter(env.handler)
	tracker.SetPodIPResolver(func(string) string { return "10.0.0.1" })
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) {})
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)

	// Start a real subscription first so StopWatching has a goroutine to drain.
	tracker.EnsureWatching("ws-cycle")

	// Seed billing state.
	tracker.ProcessEvent("ws-cycle", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_cycle",
			"info": {
				"id": "ses_cycle",
				"cost": 0.01,
				"tokens": {"input": 1000, "output": 500},
				"model": {"id": "gpt-4o", "providerID": "openai"}
			}
		}
	}`)

	tokens, _, _ := tracker.GetBillingState("ws-cycle")
	require.NotEmpty(t, tokens, "billing state must exist before cycle")

	// Simulate a Creating→Active transition through the handler — this calls
	// StopWatching (draining the real goroutine via WaitGroup) then EnsureWatching.
	wsObj := makeWorkspaceCRDWithStatus("ws-cycle", "10.0.0.1", string(v1.WorkspacePhaseActive), "")
	env.handler.onPhaseChange(wsObj)

	tokens, costs, startTimes := tracker.GetBillingState("ws-cycle")
	assert.Empty(t, tokens, "old billing state cleared by StopWatching")
	assert.Empty(t, costs, "old cost state cleared by StopWatching")
	assert.Empty(t, startTimes, "old startTime cleared by StopWatching")
	assert.True(t, tracker.IsWatching("ws-cycle"), "tracker watching after EnsureWatching")

	tracker.StopWatching("ws-cycle")
}

// TestE2E_SSETracker_RealFixture_1_18_10_SessionUpdated verifies the tracker
// against a session.updated event built from the REAL opencode 1.18.10
// session_get fixture (pkg/agent/opencode/testdata/session_get_1_18_10.json).
// Key: cost is a plain int (0), NOT an object — confirms cost-as-object path
// is defensive code that doesn't fire with real 1.18.10 data.
func TestE2E_SSETracker_RealFixture_1_18_10_SessionUpdated(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	tracker := sse.NewTracker(env.handler.httpClient, env.log, func(_, _ string) {})
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)
	env.handler.sseTracker = tracker

	var mu sync.Mutex
	type inferenceCall struct {
		modelID      string
		providerID   string
		inputTokens  int64
		outputTokens int64
	}
	var calls []inferenceCall

	tracker.SetOnInference(func(_, modelID, providerID string, inputTokens, outputTokens int64, _ float64) {
		mu.Lock()
		calls = append(calls, inferenceCall{modelID, providerID, inputTokens, outputTokens})
		mu.Unlock()
	})

	tracker.ProcessEvent("ws-real", `{
		"id": "evt_test",
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_test01KKKKKKKKKKKKKKKKKK",
			"info": {
				"id": "ses_test01KKKKKKKKKKKKKKKKKK",
				"cost": 0,
				"tokens": {
					"input": 4868893,
					"output": 330410,
					"reasoning": 35310,
					"cache": {"read": 761649152, "write": 0}
				},
				"model": {
					"id": "glm-5.2",
					"providerID": "thekaocloud",
					"variant": "default"
				}
			}
		}
	}`)

	mu.Lock()
	defer mu.Unlock()

	require.NotEmpty(t, calls, "inference callback must fire on real 1.18.10 wire shape")
	c := calls[0]
	assert.Equal(t, "glm-5.2", c.modelID)
	assert.Equal(t, "thekaocloud", c.providerID)
	assert.Equal(t, int64(4868893), c.inputTokens)
	assert.Equal(t, int64(330410), c.outputTokens)
}
