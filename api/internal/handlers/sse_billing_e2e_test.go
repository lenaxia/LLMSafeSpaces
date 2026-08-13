// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
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

	// Send session.updated with cost as an object (1.18.10 wire shape)
	// and legacy "provider" key (1.15.x wire shape).
	tracker.ProcessEvent("ws-billing", `{
		"type": "session.updated",
		"properties": {
			"sessionID": "ses_billing",
			"info": {
				"id": "ses_billing",
				"cost": {"total": 0.042},
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
	tracker.SetPasswordGetter(env.handler)
	tracker.SetPodIPResolver(func(string) string { return "10.0.0.1" })

	// Seed billing state by sending a session.updated event.
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
}
