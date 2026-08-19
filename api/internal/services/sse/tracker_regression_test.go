// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"

	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
		"cost": map[string]interface{}{"cost": 0.05},
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

// TestSSETracker_StopWatching_CleansStartTimeViaRealDispatch verifies that
// sessionStartTime entries created through the real dispatchProperties path
// (session.status=busy event → tracker writes composite key) are cleaned by
// StopWatching. This catches the keying mismatch bug: production code wrote
// bare session-ID keys while StopWatching used workspaceID: prefix matching.
func TestSSETracker_StopWatching_CleansStartTimeViaRealDispatch(t *testing.T) {
	tracker := newTestSSETracker(func(_, _ string) {})

	// Send a busy event through the real dispatch path — this writes to
	// sessionStartTime using whatever key the code actually uses.
	tracker.processEvent("ws-key-test", makeSessionStatusEvent("ses_keytest", "busy"))

	// Verify the entry exists.
	tracker.startTimeMu.Lock()
	assert.NotEmpty(t, tracker.sessionStartTime, "sessionStartTime must have entries after busy event")
	tracker.startTimeMu.Unlock()

	// StopWatching must clean ALL sessionStartTime entries for this workspace.
	tracker.StopWatching("ws-key-test")

	// Use assert.Empty, not NotContains — a bare session-ID key like
	// "ses_keytest" wouldn't contain "ws-key-test" and NotContains would
	// be a false pass on the unfixed (bare-key) code.
	tracker.startTimeMu.Lock()
	assert.Empty(t, tracker.sessionStartTime,
		"sessionStartTime must be empty after StopWatching (all entries for ws-key-test cleaned)")
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

// --- #751 F1c: silent failure paths must log (TDD failing tests first) ---

// newCapturingTracker builds a tracker wired to a capturingLogger so tests
// can assert on warn output. Returns both.
func newCapturingTracker() (*Tracker, *capturingLogger) {
	log := &capturingLogger{}
	tracker := NewTracker(&http.Client{Timeout: 2 * time.Second}, log, func(_, _ string) {})
	tracker.SetMeteringDecoder(agentoc.NewAdapter(nil, nil, zap.NewNop()).MeteringFromEvent)
	return tracker, log
}

// TestSSETracker_Inference_EmptyModelID_LogsWarn verifies that when a
// session.updated event parses but has an empty model.ID, the tracker
// emits a warn log instead of silently returning. This is the billing
// observability gap — without a log, operators cannot detect drift.
func TestSSETracker_Inference_EmptyModelID_LogsWarn(t *testing.T) {
	tracker, log := newCapturingTracker()
	var fired bool
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) { fired = true })

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_nomodel", map[string]interface{}{
		"id":   "ses_nomodel",
		"cost": 0.01,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
		},
		"model": map[string]interface{}{"id": ""},
	}))

	assert.False(t, fired, "inference must not fire on empty model")
	warns := log.Warns()
	assert.NotEmpty(t, warns, "empty model.ID must emit a warn log (currently silent)")
}

// TestSSETracker_Inference_ZeroOutput_LogsWarn verifies that when a
// session.updated event has zero output tokens, the tracker emits a
// warn log instead of silently returning.
func TestSSETracker_Inference_ZeroOutput_LogsWarn(t *testing.T) {
	tracker, log := newCapturingTracker()
	var fired bool
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) { fired = true })

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_nooutput", map[string]interface{}{
		"id":   "ses_nooutput",
		"cost": 0.0,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 0,
		},
		"model": map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	assert.False(t, fired, "inference must not fire on zero output")
	warns := log.Warns()
	assert.NotEmpty(t, warns, "zero output tokens must emit a warn log (currently silent)")
}

// TestSSETracker_Inference_EmptyID_LogsWarn verifies that when a
// session.updated event has an empty info.id, the tracker emits a
// warn log instead of silently returning.
func TestSSETracker_Inference_EmptyID_LogsWarn(t *testing.T) {
	tracker, log := newCapturingTracker()
	var fired bool
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) { fired = true })

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_noid", map[string]interface{}{
		"id":   "",
		"cost": 0.01,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
		},
		"model": map[string]interface{}{"id": "gpt-4o"},
	}))

	assert.False(t, fired, "inference must not fire on empty session ID")
	warns := log.Warns()
	assert.NotEmpty(t, warns, "empty info.id must emit a warn log (currently silent)")
}

// --- #751 F3: StopWatching must wait for the goroutine to exit (reconnect race) ---

// TestSSETracker_StopWatching_NoEventsAfterReturn verifies that after
// StopWatching returns, the subscribe goroutine has fully exited and no
// stale events can fire callbacks. The server sends a burst of 200 events
// before blocking; without a WaitGroup, buffered events in the HTTP
// response can still be processed by the scanner after cancel() fires but
// before the goroutine exits — mutating maps resurrected by stale writes.
func TestSSETracker_StopWatching_NoEventsAfterReturn(t *testing.T) {
	var activeCount atomic.Int64

	// Gate: server holds the gate open while sending the burst, then blocks.
	// This forces the scanner to have buffered events ready when cancel fires.
	sentBurst := make(chan struct{})

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Send a tight burst of 200 busy events with no delay.
		for i := 0; i < 200; i++ {
			fmt.Fprintf(w, "data: %s\n\n", makeSessionStatusEvent("sess-race", "busy"))
		}
		flusher.Flush()
		close(sentBurst)

		// Block until client disconnects.
		<-r.Context().Done()
	}))
	defer sseServer.Close()

	tracker := NewTracker(
		&http.Client{Transport: &redirectTransport{server: sseServer}},
		&testLogger{},
		func(_, _ string) {},
	)
	tracker.SetOnSessionActive(func(_, _ string) {
		activeCount.Add(1)
	})
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(string) string { return "10.0.0.1" })

	tracker.EnsureWatching("ws-race")

	// Wait for the burst to be sent by the server.
	select {
	case <-sentBurst:
	case <-time.After(5 * time.Second):
		t.Fatal("server never sent burst")
	}

	// Give the scanner a moment to buffer some events.
	time.Sleep(20 * time.Millisecond)

	// StopWatching must block until the goroutine exits. After it returns,
	// no more onSessionActive callbacks should fire.
	tracker.StopWatching("ws-race")

	countAtStop := activeCount.Load()

	// Wait to see if a stale goroutine delivers more events.
	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, countAtStop, activeCount.Load(),
		"no events should be processed after StopWatching returns (reconnect race — "+
			"goroutine may still be draining buffered events)")
}

// --- Real opencode 1.18.10 wire shape validation ---
// Fixture: pkg/agent/opencode/testdata/session_get_1_18_10.json
// Finding: cost is a plain int (0), NOT an object. The cost-as-object path
// is defensive — doesn't fire with real 1.18.10 data but must not break.

func TestSSETracker_RealWire_1_18_10_CostAsInt(t *testing.T) {
	var mu sync.Mutex
	var fired bool
	var costVal float64

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, cost float64) {
		mu.Lock()
		fired = true
		costVal = cost
		mu.Unlock()
	})

	// Real 1.18.10 shape: cost is a plain number, model has providerID.
	// Use non-zero (42) to detect parse regressions — cost=0 is tautological
	// (0.0 is the default float64 value).
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_real", map[string]interface{}{
		"id":   "ses_real",
		"cost": 42,
		"tokens": map[string]interface{}{
			"input": 4868893, "output": 330410,
		},
		"model": map[string]interface{}{
			"id": "glm-5.2", "providerID": "thekaocloud", "variant": "default",
		},
	}))

	mu.Lock()
	assert.True(t, fired, "must fire on real 1.18.10 wire shape (cost as plain int)")
	assert.Equal(t, 42.0, costVal, "cost must be 42.0 from plain int input")
	mu.Unlock()
}

func TestSSETracker_RealWire_1_18_10_NonZeroCostAsFloat(t *testing.T) {
	var mu sync.Mutex
	var costVal float64

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, cost float64) {
		mu.Lock()
		costVal = cost
		mu.Unlock()
	})

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_real2", map[string]interface{}{
		"id":   "ses_real2",
		"cost": 0.042,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
		},
		"model": map[string]interface{}{
			"id": "glm-5.2", "providerID": "thekaocloud",
		},
	}))

	mu.Lock()
	assert.Equal(t, 0.042, costVal, "cost must parse correctly as plain float64")
	mu.Unlock()
}

// TestSSETracker_Inference_SuffixedEventTypeDispatch pins the drift-fragile
// dispatch behavior at the TRACKER layer: metering events must fire
// regardless of type-name surface (live wire unsuffixed, event store
// version-suffixed), and near-miss types must not fire. Reverting the
// decoder routing to an exact "session.updated" string match would fail here.
func TestSSETracker_Inference_SuffixedEventTypeDispatch(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		fires     bool
	}{
		{"unsuffixed (live wire)", "session.updated", true},
		{"version-suffixed (store surface)", "session.updated.1", true},
		{"non-numeric suffix is not a version", "session.updated.foo", false},
		{"status is not metering", "session.status", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker, _ := newCapturingTracker()
			var fired bool
			tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) { fired = true })

			props, _ := json.Marshal(map[string]interface{}{
				"sessionID": "ses_x",
				"info": map[string]interface{}{
					"id":     "ses_x",
					"cost":   0.01,
					"tokens": map[string]interface{}{"input": 100, "output": 50},
					"model":  map[string]interface{}{"id": "m", "providerID": "p"},
				},
			})
			data, _ := json.Marshal(map[string]interface{}{
				"id":         "evt_t",
				"type":       tt.eventType,
				"properties": json.RawMessage(props),
			})
			tracker.processEvent("ws-1", string(data))

			assert.Equal(t, tt.fires, fired, "eventType %q", tt.eventType)
		})
	}
}

// TestSSETracker_Inference_MalformedCost_WarnsAndBillsAtZero pins the
// CostMalformed POLICY at the tracker layer: warn + onInference fires with
// cost 0 — never silently dropped, never a decode error.
func TestSSETracker_Inference_MalformedCost_WarnsAndBillsAtZero(t *testing.T) {
	tracker, log := newCapturingTracker()
	var mu sync.Mutex
	var cost float64
	var fired bool
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, costDollars float64) {
		mu.Lock()
		fired, cost = true, costDollars
		mu.Unlock()
	})

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_badcost", map[string]interface{}{
		"id":     "ses_badcost",
		"cost":   "not-a-number",
		"tokens": map[string]interface{}{"input": 1000, "output": 500},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	mu.Lock()
	assert.True(t, fired, "malformed cost must NOT drop the billing event")
	assert.Zero(t, cost, "malformed cost bills at 0")
	mu.Unlock()

	var found bool
	for _, w := range log.Warns() {
		if strings.Contains(w, "cost field is neither number nor object") {
			found = true
		}
	}
	assert.True(t, found, "malformed cost must warn (drift signal)")
}

// TestSSETracker_NonUsageEvents_NoWarnNoFire pins the dispatch contract
// that a5ba84be regressed: non-usage events (the majority of stream
// traffic — part-updates are dominant) must neither fire inference nor
// emit any warn. A spurious warn per event floods logs and drowns the
// real drift signals (empty-ID, undecodable, malformed-cost).
func TestSSETracker_NonUsageEvents_NoWarnNoFire(t *testing.T) {
	events := []struct {
		name      string
		eventType string
		body      string
	}{
		{"part update", "message.part.updated", `{"sessionID":"ses_x","part":{"type":"text","text":"hi"}}`},
		{"suffixed part update", "message.part.updated.1", `{"sessionID":"ses_x","part":{"type":"step-finish","tokens":{"input":10,"cache":{"read":0,"write":0}}}}`},
		{"session status", "session.status", `{"sessionID":"ses_x","status":{"type":"busy"}}`},
		{"message updated", "message.updated", `{"sessionID":"ses_x","info":{"id":"msg_1"}}`},
	}
	for _, tt := range events {
		t.Run(tt.name, func(t *testing.T) {
			tracker, log := newCapturingTracker() // real adapter decoder
			var fired bool
			tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) { fired = true })

			props, _ := json.Marshal(json.RawMessage(tt.body))
			data, _ := json.Marshal(map[string]interface{}{
				"id": "evt_t", "type": tt.eventType, "properties": props,
			})
			tracker.processEvent("ws-1", string(data))

			assert.False(t, fired, "non-usage event must not fire inference")
			assert.Empty(t, log.Warns(), "non-usage event must not warn (regression: a5ba84be)")
		})
	}
}
