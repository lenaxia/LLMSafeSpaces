// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// --- #751 tests ---

// TestSSETracker_Inference_CostAsObject verifies that handleSessionUpdated
// doesn't drop the event when "cost" is an object (not a float). The old
// code declared Cost as float64 which silently failed on object shapes.
func TestSSETracker_Inference_CostAsObject(t *testing.T) {
	var mu sync.Mutex
	var fired bool

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) {
		mu.Lock()
		fired = true
		mu.Unlock()
	})

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_obj", map[string]interface{}{
		"id":   "ses_obj",
		"cost": map[string]interface{}{"total": 0.05},
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
		},
		"model": map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	mu.Lock()
	assert.True(t, fired, "inference callback must fire even when cost is an object")
	mu.Unlock()
}

// TestSSETracker_Inference_ProviderKeyFallback verifies that when the
// model object has "provider" (legacy) instead of "providerID", the
// callback still receives a non-empty providerID.
func TestSSETracker_Inference_ProviderKeyFallback(t *testing.T) {
	var mu sync.Mutex
	var providerID string

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, pid string, _, _ int64, _ float64) {
		mu.Lock()
		providerID = pid
		mu.Unlock()
	})

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_prov", map[string]interface{}{
		"id":   "ses_prov",
		"cost": 0.0,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
		},
		"model": map[string]interface{}{"id": "claude-3.5", "provider": "anthropic"},
	}))

	mu.Lock()
	assert.Equal(t, "anthropic", providerID, "provider must fall back to legacy 'provider' key")
	mu.Unlock()
}

// TestSSETracker_StopWatching_CleansUpMaps verifies that StopWatching
// removes per-session entries from all three billing maps.
func TestSSETracker_StopWatching_CleansUpMaps(t *testing.T) {
	tracker := newTestSSETracker(func(_, _ string) {})

	// Seed the maps directly.
	tracker.tokensMu.Lock()
	tracker.sessionTokenSeen["ws-1:ses_clean"] = 500
	tracker.sessionCostSeen["ws-1:ses_clean"] = 0.05
	tracker.tokensMu.Unlock()

	tracker.startTimeMu.Lock()
	tracker.sessionStartTime["ws-1:ses_clean"] = time.Now()
	tracker.startTimeMu.Unlock()

	tracker.StopWatching("ws-1")

	tracker.tokensMu.Lock()
	assert.NotContains(t, tracker.sessionTokenSeen, "ws-1:ses_clean", "tokenSeen must be cleaned")
	assert.NotContains(t, tracker.sessionCostSeen, "ws-1:ses_clean", "costSeen must be cleaned")
	tracker.tokensMu.Unlock()

	tracker.startTimeMu.Lock()
	assert.NotContains(t, tracker.sessionStartTime, "ws-1:ses_clean", "startTime must be cleaned")
	tracker.startTimeMu.Unlock()
}

// --- #751 parse error logging ---

func TestSSETracker_Inference_MalformedEvent_NoPanic(t *testing.T) {
	tracker := newTestSSETracker(func(_, _ string) {})

	raw := `{"type":"session.updated","properties":{"info":"not an object"}}`
	assert.NotPanics(t, func() {
		tracker.processEvent("ws-1", raw)
	})
}
