// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestSSETracker(onIdle SessionIdleCallback) *Tracker {
	return NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, onIdle)
}

// --- Nested format (opencode /event format) ---

func makeNestedEvent(eventType string, props map[string]interface{}) string {
	propsJSON, _ := json.Marshal(props)
	data, _ := json.Marshal(map[string]interface{}{
		"directory": "ws-1",
		"payload": map[string]interface{}{
			"type":       eventType,
			"properties": json.RawMessage(propsJSON),
		},
	})
	return string(data)
}

// makeSessionStatusEvent builds a real opencode flat-format session.status event.
// statusType is "idle" or "busy".
func makeSessionStatusEvent(sessionID, statusType string) string {
	data, _ := json.Marshal(map[string]interface{}{
		"type": "session.status",
		"properties": map[string]interface{}{
			"sessionID": sessionID,
			"status":    map[string]string{"type": statusType},
		},
	})
	return string(data)
}

// makeNestedSessionEvent builds a nested-format session.status event (legacy format).
// statusType is "idle" or "busy".
func makeNestedSessionEvent(statusType, sessionID string) string {
	return makeNestedEvent("session.status", map[string]interface{}{
		"sessionID": sessionID,
		"status":    map[string]string{"type": statusType},
	})
}

func makeNestedPartUpdatedEvent(sessionID, partType, text string) string {
	return makeNestedEvent("message.part.updated", map[string]interface{}{
		"sessionID": sessionID,
		"part": map[string]interface{}{
			"type": partType,
			"text": text,
		},
	})
}

func makeNestedMessageUpdatedEvent(sessionID string) string {
	return makeNestedEvent("message.updated", map[string]interface{}{
		"sessionID": sessionID,
		"info": map[string]interface{}{
			"id":   "msg-1",
			"role": "assistant",
		},
	})
}

// fakePWProvider implements interfaces.WorkspacePasswordProvider for tests.
type fakePWProvider struct {
	pw  string
	err error
}

func (f fakePWProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return f.pw, f.err
}

func TestSSETracker_ProcessEvent_Nested_IdleEvent(t *testing.T) {
	var mu sync.Mutex
	var calls []struct {
		sandboxID string
		sessionID string
	}
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		mu.Lock()
		calls = append(calls, struct {
			sandboxID string
			sessionID string
		}{sandboxID, sessionID})
		mu.Unlock()
	})

	tracker.processEvent("sb-1", makeNestedSessionEvent("idle", "sess-1"))

	mu.Lock()
	require.Len(t, calls, 1)
	assert.Equal(t, "sb-1", calls[0].sandboxID)
	assert.Equal(t, "sess-1", calls[0].sessionID)
	mu.Unlock()
}

func TestSSETracker_ProcessEvent_Nested_BusyEvent(t *testing.T) {
	var mu sync.Mutex
	var activeCalls []struct {
		sandboxID string
		sessionID string
	}
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetOnSessionActive(func(sandboxID, sessionID string) {
		mu.Lock()
		activeCalls = append(activeCalls, struct {
			sandboxID string
			sessionID string
		}{sandboxID, sessionID})
		mu.Unlock()
	})

	tracker.processEvent("sb-1", makeNestedSessionEvent("busy", "sess-1"))

	mu.Lock()
	require.Len(t, activeCalls, 1)
	assert.Equal(t, "sb-1", activeCalls[0].sandboxID)
	assert.Equal(t, "sess-1", activeCalls[0].sessionID)
	mu.Unlock()
}

func TestSSETracker_ProcessEvent_Nested_IgnoresNonSessionEvents(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tracker.processEvent("sb-1", makeNestedPartUpdatedEvent("sess-1", "text", "hello"))
	tracker.processEvent("sb-1", makeNestedMessageUpdatedEvent("sess-1"))
	tracker.processEvent("sb-1", makeNestedEvent("session.created", map[string]interface{}{"sessionID": "sess-1"}))

	assert.Equal(t, int32(0), atomic.LoadInt32(&called), "idle callback should not be called for non-session.status events")
}

func TestSSETracker_ProcessEvent_Nested_IgnoresBusyStatus(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	for _, status := range []string{"active", "running", "pending"} {
		tracker.processEvent("sb-1", makeNestedSessionEvent(status, "sess-1"))
	}
	assert.Equal(t, int32(0), atomic.LoadInt32(&called))
}

func TestSSETracker_ProcessEvent_Nested_IgnoresEmptySessionID(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tracker.processEvent("sb-1", makeNestedSessionEvent("idle", ""))
	tracker.processEvent("sb-1", makeNestedSessionEvent("busy", ""))
	assert.Equal(t, int32(0), atomic.LoadInt32(&called))
}

func TestSSETracker_ProcessEvent_Nested_IgnoresMalformedJSON(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tests := []struct {
		name string
		data string
	}{
		{"missing payload", `{"directory":"ws-1"}`},
		{"payload with no type", `{"directory":"ws-1","payload":{"properties":{}}}`},
		{"payload type empty", `{"directory":"ws-1","payload":{"type":"","properties":{}}}`},
		{"properties not an object", `{"directory":"ws-1","payload":{"type":"session.status","properties":"string"}}`},
		{"completely wrong structure", `{"foo":"bar"}`},
		{"empty object", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&called, 0)
			tracker.processEvent("sb-1", tt.data)
			assert.Equal(t, int32(0), atomic.LoadInt32(&called))
		})
	}
}

// --- RawEventCallback tests ---

func TestSSETracker_RawEventCallback_Nested_AllEventsForwarded(t *testing.T) {
	var mu sync.Mutex
	var rawEvents []struct {
		workspaceID string
		eventType   string
		rawData     string
	}

	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		rawEvents = append(rawEvents, struct {
			workspaceID string
			eventType   string
			rawData     string
		}{workspaceID, eventType, rawData})
		mu.Unlock()
	})

	partUpdatedData := makeNestedPartUpdatedEvent("sess-1", "text", "hello")
	messageUpdatedData := makeNestedMessageUpdatedEvent("sess-1")
	sessionIdleData := makeNestedSessionEvent("idle", "sess-1")
	sessionBusyData := makeNestedSessionEvent("busy", "sess-1")

	tracker.processEvent("sb-1", partUpdatedData)
	tracker.processEvent("sb-1", messageUpdatedData)
	tracker.processEvent("sb-1", sessionIdleData)
	tracker.processEvent("sb-1", sessionBusyData)

	mu.Lock()
	require.Len(t, rawEvents, 4)

	assert.Equal(t, "sb-1", rawEvents[0].workspaceID)
	assert.Equal(t, "message.part.updated", rawEvents[0].eventType)
	assert.Contains(t, rawEvents[0].rawData, "message.part.updated")
	assert.Contains(t, rawEvents[0].rawData, "hello")

	assert.Equal(t, "message.updated", rawEvents[1].eventType)

	assert.Equal(t, "session.status", rawEvents[2].eventType)
	assert.Equal(t, "session.status", rawEvents[3].eventType)
	mu.Unlock()
}

func TestSSETracker_RawEventCallback_FlatFormatStillWorks(t *testing.T) {
	var mu sync.Mutex
	var rawEvents []struct {
		workspaceID string
		eventType   string
	}

	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		rawEvents = append(rawEvents, struct {
			workspaceID string
			eventType   string
		}{workspaceID, eventType})
		mu.Unlock()
	})

	tracker.processEvent("sb-1", makeSessionStatusEvent("sess-1", "idle"))

	mu.Lock()
	require.Len(t, rawEvents, 1)
	assert.Equal(t, "sb-1", rawEvents[0].workspaceID)
	assert.Equal(t, "session.status", rawEvents[0].eventType)
	mu.Unlock()
}

func TestSSETracker_RawEventCallback_NotCalledWhenNil(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	// onRawEvent is nil by default — should not panic

	tracker.processEvent("sb-1", makeNestedPartUpdatedEvent("sess-1", "text", "hello"))
	tracker.processEvent("sb-1", makeNestedSessionEvent("idle", "sess-1"))
	tracker.processEvent("sb-1", `{"type":"session.status","session_id":"sess-1","status":"idle"}`)
}

func TestSSETracker_RawEventCallback_PartUpdatedRawDataContainsFullPayload(t *testing.T) {
	var capturedRawData string
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		capturedRawData = rawData
	})

	data := makeNestedPartUpdatedEvent("sess-1", "text", "hello world")
	tracker.processEvent("sb-1", data)

	// The raw data should include the directory and full payload
	assert.Contains(t, capturedRawData, `"directory"`)
	assert.Contains(t, capturedRawData, `"message.part.updated"`)
	assert.Contains(t, capturedRawData, `"sessionID"`)
	assert.Contains(t, capturedRawData, `"hello world"`)

	// Verify it's valid JSON that can be parsed back
	var parsed map[string]interface{}
	err := json.Unmarshal([]byte(capturedRawData), &parsed)
	assert.NoError(t, err)
	assert.Equal(t, "ws-1", parsed["directory"])
}

func TestSSETracker_RawEventCallback_CalledForAllNestedTypes(t *testing.T) {
	eventTypes := make(map[string]int)
	var mu sync.Mutex

	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		eventTypes[eventType]++
		mu.Unlock()
	})

	events := []string{
		"session.status",
		"session.created",
		"session.updated",
		"session.error",
		"message.part.updated",
		"message.updated",
		"message.error",
	}

	for _, et := range events {
		tracker.processEvent("sb-1", makeNestedEvent(et, map[string]interface{}{
			"sessionID": "sess-1",
		}))
	}

	mu.Lock()
	require.Len(t, eventTypes, len(events))
	for _, et := range events {
		assert.Equal(t, 1, eventTypes[et], "event type %s should be called once", et)
	}
	mu.Unlock()
}

// --- Existing flat-format tests ---

func TestSSETracker_ProcessEvent_SessionStatusIdle(t *testing.T) {
	var mu sync.Mutex
	var calls []struct {
		sandboxID string
		sessionID string
	}
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		mu.Lock()
		calls = append(calls, struct {
			sandboxID string
			sessionID string
		}{sandboxID, sessionID})
		mu.Unlock()
	})

	tracker.processEvent("sb-1", makeSessionStatusEvent("sess-1", "idle"))

	mu.Lock()
	require.Len(t, calls, 1)
	assert.Equal(t, "sb-1", calls[0].sandboxID)
	assert.Equal(t, "sess-1", calls[0].sessionID)
	mu.Unlock()
}

func TestSSETracker_ProcessEvent_IgnoresNonSessionStatus(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tests := []struct {
		name      string
		eventData string
	}{
		{"other event type", makeSessionStatusEvent("sess-1", "idle")}, // wrong type below
		{"output event", makeSessionStatusEvent("sess-1", "idle")},
		{"ping event", `{"type":"ping","properties":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&called, 0)
			// For non-session.status types, build them directly
			var data string
			switch tt.name {
			case "other event type":
				data = `{"type":"session.created","properties":{"sessionID":"sess-1","status":{"type":"idle"}}}`
			case "output event":
				data = `{"type":"session.output","properties":{"sessionID":"sess-1","status":{"type":"idle"}}}`
			case "ping event":
				data = `{"type":"ping","properties":{}}`
			}
			tracker.processEvent("sb-1", data)
			assert.Equal(t, int32(0), atomic.LoadInt32(&called))
		})
	}
}

func TestSSETracker_ProcessEvent_IgnoresBusyStatus(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tests := []struct {
		name   string
		status string
	}{
		{"busy", "busy"},
		{"active", "active"},
		{"running", "running"},
		{"pending", "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&called, 0)
			tracker.processEvent("sb-1", makeSessionStatusEvent("sess-1", tt.status))
			assert.Equal(t, int32(0), atomic.LoadInt32(&called))
		})
	}
}

func TestSSETracker_ProcessEvent_IgnoresMalformedJSON(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tests := []struct {
		name string
		data string
	}{
		{"not json", "this is not json"},
		{"broken json", `{"type":"session.status","session_id":`},
		{"random text", "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&called, 0)
			tracker.processEvent("sb-1", tt.data)
			assert.Equal(t, int32(0), atomic.LoadInt32(&called))
		})
	}
}

func TestSSETracker_ProcessEvent_IgnoresEmptyData(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	tests := []struct {
		name string
		data string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"newlines", "\n\n"},
		{"tabs and spaces", "\t  \t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&called, 0)
			tracker.processEvent("sb-1", tt.data)
			assert.Equal(t, int32(0), atomic.LoadInt32(&called))
		})
	}
}

func TestSSETracker_ProcessEvent_IgnoresIdleWithEmptySessionID(t *testing.T) {
	var called int32
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {
		atomic.AddInt32(&called, 1)
	})

	// Empty sessionID — should not call idle callback
	tracker.processEvent("sb-1", `{"type":"session.status","properties":{"sessionID":"","status":{"type":"idle"}}}`)
	assert.Equal(t, int32(0), atomic.LoadInt32(&called))
}

func TestSSETracker_EnsureWatching_CreatesSubscription(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	tracker.EnsureWatching("sb-1")
	assert.Equal(t, 1, tracker.SubscriptionCount())

	tracker.Stop()
}

func TestSSETracker_EnsureWatching_NoDuplicateSubscription(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	tracker.EnsureWatching("sb-1")
	tracker.EnsureWatching("sb-1")
	tracker.EnsureWatching("sb-1")
	assert.Equal(t, 1, tracker.SubscriptionCount())

	tracker.Stop()
}

func TestSSETracker_EnsureWatching_MultipleSandboxes(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	tracker.EnsureWatching("sb-1")
	tracker.EnsureWatching("sb-2")
	tracker.EnsureWatching("sb-3")
	assert.Equal(t, 3, tracker.SubscriptionCount())

	tracker.Stop()
}

func TestSSETracker_StopWatching_CancelsSubscription(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	tracker.EnsureWatching("sb-1")
	tracker.EnsureWatching("sb-2")
	assert.Equal(t, 2, tracker.SubscriptionCount())

	tracker.StopWatching("sb-1")
	assert.Equal(t, 1, tracker.SubscriptionCount())

	tracker.Stop()
}

func TestSSETracker_StopWatching_NonexistentSandbox(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})

	tracker.StopWatching("sb-nonexistent")
	assert.Equal(t, 0, tracker.SubscriptionCount())
}

func TestSSETracker_Stop_CancelsAllSubscriptions(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	tracker.EnsureWatching("sb-1")
	tracker.EnsureWatching("sb-2")
	tracker.EnsureWatching("sb-3")
	assert.Equal(t, 3, tracker.SubscriptionCount())

	tracker.Stop()
	assert.Equal(t, 0, tracker.SubscriptionCount())
}

func TestSSETracker_SubscriptionCount_ZeroInitially(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	assert.Equal(t, 0, tracker.SubscriptionCount())
}

func TestSSETracker_SubscriptionCount_AccurateAfterOperations(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	tracker.SetPasswordGetter(fakePWProvider{err: fmt.Errorf("test error")})

	assert.Equal(t, 0, tracker.SubscriptionCount())

	tracker.EnsureWatching("sb-1")
	assert.Equal(t, 1, tracker.SubscriptionCount())

	tracker.EnsureWatching("sb-2")
	assert.Equal(t, 2, tracker.SubscriptionCount())

	tracker.StopWatching("sb-1")
	assert.Equal(t, 1, tracker.SubscriptionCount())

	tracker.EnsureWatching("sb-3")
	assert.Equal(t, 2, tracker.SubscriptionCount())

	tracker.Stop()
	assert.Equal(t, 0, tracker.SubscriptionCount())
}

func TestSSETracker_SetPasswordGetter(t *testing.T) {
	tracker := newTestSSETracker(func(sandboxID, sessionID string) {})
	require.Nil(t, tracker.passwordGetter)

	getter := fakePWProvider{pw: "test-password"}
	tracker.SetPasswordGetter(getter)
	require.NotNil(t, tracker.passwordGetter)

	pw, err := tracker.passwordGetter.WorkspacePassword(context.Background(), "sb-1")
	assert.NoError(t, err)
	assert.Equal(t, "test-password", pw)
}

func TestSSETracker_Subscribe_ReceivesIdleEvent(t *testing.T) {
	var mu sync.Mutex
	var idleCalls []struct {
		sandboxID string
		sessionID string
	}

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-pw", pass)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Emit real opencode flat-format events
		eventStrings := []string{
			makeSessionStatusEvent("sess-1", "idle"),
			makeSessionStatusEvent("sess-2", "busy"),
			`{"type":"session.output","properties":{"sessionID":"sess-1"}}`,
			makeSessionStatusEvent("sess-3", "idle"),
		}
		for _, evtStr := range eventStrings {
			fmt.Fprintf(w, "data: %s\n\n", evtStr)
			flusher.Flush()
		}
		// Block until client disconnects so the scanner sees EOF
		<-r.Context().Done()
	}))
	defer sseServer.Close()

	tracker := NewTracker(
		&http.Client{
			Transport: &redirectTransport{server: sseServer},
		},
		&testLogger{},
		func(sandboxID, sessionID string) {
			mu.Lock()
			idleCalls = append(idleCalls, struct {
				sandboxID string
				sessionID string
			}{sandboxID, sessionID})
			mu.Unlock()
		},
	)
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(sandboxID string) string {
		return "10.0.0.1"
	})

	tracker.EnsureWatching("sb-1")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(idleCalls) == 2
	}, 5*time.Second, 50*time.Millisecond, "expected 2 idle callbacks")

	mu.Lock()
	assert.Equal(t, "sb-1", idleCalls[0].sandboxID)
	assert.Equal(t, "sess-1", idleCalls[0].sessionID)
	assert.Equal(t, "sb-1", idleCalls[1].sandboxID)
	assert.Equal(t, "sess-3", idleCalls[1].sessionID)
	mu.Unlock()

	tracker.Stop()
}

// --- Inference callback tests (session.updated wire format validated against opencode 1.15.12) ---
//
// Real wire format from opencode 1.15.12 /event stream (validated 2026-06-11):
//   {"id":"evt_...","type":"session.updated","properties":{
//     "sessionID":"ses_...",
//     "info":{
//       "id":"ses_...",
//       "cost":0,
//       "tokens":{"input":509911,"output":20861,"reasoning":41,"cache":{"read":9229154,"write":0}},
//       "model":{"id":"glm-5.1","providerID":"thekao cloud","variant":"default"}
//     }
//   }}
//
// NOTE: tokens and model are under properties.info, NOT at properties top-level.
// The previous implementation parsed properties.{id,model,tokens,cost} and always
// silently returned — the guard `p.ID == ""` was always true.

func makeSessionUpdatedEvent(sessionID string, info map[string]interface{}) string {
	data, _ := json.Marshal(map[string]interface{}{
		"id":   "evt_test",
		"type": "session.updated",
		"properties": map[string]interface{}{
			"sessionID": sessionID,
			"info":      info,
		},
	})
	return string(data)
}

func TestSSETracker_Inference_FiredOnSessionUpdatedWithOutputTokens(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		workspaceID  string
		modelID      string
		providerID   string
		inputTokens  int64
		outputTokens int64
		cost         float64
	}
	var calls []call

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(workspaceID, modelID, providerID string, inputTokens, outputTokens int64, costDollars float64) {
		mu.Lock()
		calls = append(calls, call{workspaceID, modelID, providerID, inputTokens, outputTokens, costDollars})
		mu.Unlock()
	})

	// First event: cumulative tokens from a session in progress
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
		"id":   "ses_abc",
		"cost": 0.0,
		"tokens": map[string]interface{}{
			"input": 1000, "output": 500,
			"reasoning": 0,
			"cache":     map[string]interface{}{"read": 0, "write": 0},
		},
		"model": map[string]interface{}{"id": "gpt-4o", "providerID": "openai", "variant": "default"},
	}))

	mu.Lock()
	require.Len(t, calls, 1, "first session.updated should fire inference callback")
	assert.Equal(t, "ws-1", calls[0].workspaceID)
	assert.Equal(t, "gpt-4o", calls[0].modelID)
	assert.Equal(t, "openai", calls[0].providerID)
	assert.Equal(t, int64(500), calls[0].outputTokens)
	mu.Unlock()
}

func TestSSETracker_Inference_DeltaOnSubsequentEvent(t *testing.T) {
	var mu sync.Mutex
	type call struct{ outputTokens int64 }
	var calls []call

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, outputTokens int64, _ float64) {
		mu.Lock()
		calls = append(calls, call{outputTokens})
		mu.Unlock()
	})

	// First event: 500 cumulative output tokens
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
		"id": "ses_abc", "cost": 0.0,
		"tokens": map[string]interface{}{"input": 1000, "output": 500, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	// Second event: 700 cumulative output tokens — delta is 200
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
		"id": "ses_abc", "cost": 0.0,
		"tokens": map[string]interface{}{"input": 1400, "output": 700, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	mu.Lock()
	require.Len(t, calls, 2)
	assert.Equal(t, int64(500), calls[0].outputTokens, "first event: full output count")
	assert.Equal(t, int64(200), calls[1].outputTokens, "second event: delta only")
	mu.Unlock()
}

func TestSSETracker_Inference_NoFiredWhenOutputTokensZero(t *testing.T) {
	var fired int32
	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) {
		atomic.AddInt32(&fired, 1)
	})

	// output=0 must not fire
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
		"id": "ses_abc", "cost": 0.0,
		"tokens": map[string]interface{}{"input": 100, "output": 0, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fired))
}

func TestSSETracker_Inference_NoFiredWhenNoNewTokens(t *testing.T) {
	var calls int32
	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) {
		atomic.AddInt32(&calls, 1)
	})

	// Same cumulative output twice — second must not fire
	for i := 0; i < 2; i++ {
		tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
			"id": "ses_abc", "cost": 0.0,
			"tokens": map[string]interface{}{"input": 1000, "output": 500, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
			"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
		}))
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "duplicate event must fire only once")
}

func TestSSETracker_Inference_NoFiredWhenMissingSessionID(t *testing.T) {
	var fired int32
	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, _ float64) {
		atomic.AddInt32(&fired, 1)
	})

	tracker.processEvent("ws-1", makeSessionUpdatedEvent("", map[string]interface{}{
		"id": "", "cost": 0.0,
		"tokens": map[string]interface{}{"input": 100, "output": 200, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))
	assert.Equal(t, int32(0), atomic.LoadInt32(&fired))
}

func TestSSETracker_Inference_CacheTokensIncludedInInputDelta(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		inputTokens  int64
		outputTokens int64
	}
	var calls []call

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, inputTokens, outputTokens int64, _ float64) {
		mu.Lock()
		calls = append(calls, call{inputTokens, outputTokens})
		mu.Unlock()
	})

	// Real opencode event: large cache.read component
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_abc", map[string]interface{}{
		"id": "ses_abc", "cost": 0.0,
		"tokens": map[string]interface{}{
			"input": 509911, "output": 20861, "reasoning": 41,
			"cache": map[string]interface{}{"read": 9229154, "write": 0},
		},
		"model": map[string]interface{}{"id": "glm-5.1", "providerID": "thekao cloud"},
	}))

	mu.Lock()
	require.Len(t, calls, 1)
	assert.Equal(t, int64(509911), calls[0].inputTokens)
	assert.Equal(t, int64(20861), calls[0].outputTokens)
	mu.Unlock()
}

func TestSSETracker_Inference_CostDeltaOnSubsequentEvent(t *testing.T) {
	var mu sync.Mutex
	type call struct {
		cost float64
	}
	var calls []call

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, cost float64) {
		mu.Lock()
		calls = append(calls, call{cost})
		mu.Unlock()
	})

	// First event: cumulative cost = 0.05
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_cost", map[string]interface{}{
		"id": "ses_cost", "cost": 0.05,
		"tokens": map[string]interface{}{"input": 1000, "output": 500, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	// Second event: cumulative cost = 0.10 — delta should be 0.05, not 0.10
	tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_cost", map[string]interface{}{
		"id": "ses_cost", "cost": 0.10,
		"tokens": map[string]interface{}{"input": 2000, "output": 700, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
		"model":  map[string]interface{}{"id": "gpt-4o", "providerID": "openai"},
	}))

	mu.Lock()
	require.Len(t, calls, 2)
	assert.InDelta(t, 0.05, calls[0].cost, 0.001, "first event: full cumulative cost")
	assert.InDelta(t, 0.05, calls[1].cost, 0.001, "second event: delta only, not cumulative")
	mu.Unlock()
}

func TestSSETracker_Inference_ZeroCostNoInflation(t *testing.T) {
	var totalCost float64
	var mu sync.Mutex

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnInference(func(_, _, _ string, _, _ int64, cost float64) {
		mu.Lock()
		totalCost += cost
		mu.Unlock()
	})

	// Three events all reporting cost=0 (self-hosted provider)
	for i, output := range []int{500, 700, 900} {
		tracker.processEvent("ws-1", makeSessionUpdatedEvent("ses_zero_cost", map[string]interface{}{
			"id": "ses_zero_cost", "cost": 0.0,
			"tokens": map[string]interface{}{"input": 1000, "output": output, "reasoning": 0, "cache": map[string]interface{}{"read": 0, "write": 0}},
			"model":  map[string]interface{}{"id": "glm-5.1", "providerID": "thekao cloud"},
		}))
		_ = i
	}

	mu.Lock()
	assert.Equal(t, 0.0, totalCost, "zero-cost provider must never accumulate cost")
}

func TestSSETracker_RawEventCallback_StepEnded(t *testing.T) {
	var mu sync.Mutex
	var rawCalls []struct {
		workspaceID string
		eventType   string
		rawData     string
	}

	tracker := newTestSSETracker(func(_, _ string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		rawCalls = append(rawCalls, struct {
			workspaceID string
			eventType   string
			rawData     string
		}{workspaceID, eventType, rawData})
		mu.Unlock()
	})

	stepEndedJSON := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`
	tracker.processEvent("ws-1", stepEndedJSON)

	mu.Lock()
	require.Len(t, rawCalls, 1)
	assert.Equal(t, "ws-1", rawCalls[0].workspaceID)
	assert.Equal(t, "session.next.step.ended", rawCalls[0].eventType)
	assert.Contains(t, rawCalls[0].rawData, `"ses_abc"`)
	assert.Contains(t, rawCalls[0].rawData, `"input":800`)
	mu.Unlock()
}

func TestSSETracker_Subscribe_ReceivesStepEndedViaSSE(t *testing.T) {
	var mu sync.Mutex
	var rawCalls []struct {
		workspaceID string
		eventType   string
		rawData     string
	}

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		stepEndedJSON := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_rt","tokens":{"input":5000,"output":200,"reasoning":0,"cache":{"read":1000,"write":500}}}}`
		fmt.Fprintf(w, "data: %s\n\n", stepEndedJSON)
		flusher.Flush()

		<-r.Context().Done()
	}))
	defer sseServer.Close()

	tracker := NewTracker(
		&http.Client{Transport: &redirectTransport{server: sseServer}, Timeout: 2 * time.Second},
		&testLogger{},
		func(_, _ string) {},
	)
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		rawCalls = append(rawCalls, struct {
			workspaceID string
			eventType   string
			rawData     string
		}{workspaceID, eventType, rawData})
		mu.Unlock()
	})
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(_ string) string { return "10.0.0.1" })

	tracker.EnsureWatching("ws-1")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(rawCalls) >= 1
	}, 5*time.Second, 50*time.Millisecond, "expected at least 1 rawEvent callback")

	mu.Lock()
	require.Len(t, rawCalls, 1)
	assert.Equal(t, "ws-1", rawCalls[0].workspaceID)
	assert.Equal(t, "session.next.step.ended", rawCalls[0].eventType)
	assert.Contains(t, rawCalls[0].rawData, `"ses_rt"`)
	assert.Contains(t, rawCalls[0].rawData, `"input":5000`)
	assert.Contains(t, rawCalls[0].rawData, `"read":1000`)
	assert.Contains(t, rawCalls[0].rawData, `"write":500`)
	mu.Unlock()

	tracker.Stop()
}

// TestSSETracker_Subscribe_LargeEventDoesNotDropConnection is the
// regression test for the API-side SSE scanner buffer bug (parallel
// to agentd #805). The tracker's scanner had a 64KB per-line buffer;
// large message.part.updated events would kill the scanner and cause
// the tracker to miss subsequent session.status:idle events.
func TestSSETracker_Subscribe_LargeEventDoesNotDropConnection(t *testing.T) {
	var mu sync.Mutex
	var idleCalls []string

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// 1. Session goes busy
		fmt.Fprintf(w, "data: %s\n\n", makeSessionStatusEvent("sess-big", "busy"))
		flusher.Flush()

		// 2. Large event (>64KB) — simulates a patch part with thousands
		// of files. Must NOT kill the scanner.
		largeFiles := strings.Repeat(`"file.go",`, 8000) // ~72KB
		largeEvent := `{"type":"message.part.updated","properties":{"sessionID":"sess-big","part":{"type":"patch","files":[` +
			strings.TrimSuffix(largeFiles, ",") + `]},"time":1234567890}}`
		require.Greater(t, len(largeEvent), 64*1024)
		fmt.Fprintf(w, "data: %s\n\n", largeEvent)
		flusher.Flush()

		// 3. Session goes idle — the event that was missed when the
		// scanner died on the large event.
		fmt.Fprintf(w, "data: %s\n\n", makeSessionStatusEvent("sess-big", "idle"))
		flusher.Flush()

		<-r.Context().Done()
	}))
	defer sseServer.Close()

	tracker := NewTracker(
		&http.Client{
			Transport: &redirectTransport{server: sseServer},
		},
		&testLogger{},
		func(sandboxID, sessionID string) {
			mu.Lock()
			idleCalls = append(idleCalls, sessionID)
			mu.Unlock()
		},
	)
	tracker.SetPasswordGetter(fakePWProvider{pw: "test-pw"})
	tracker.SetPodIPResolver(func(sandboxID string) string { return "10.0.0.1" })

	tracker.EnsureWatching("sb-1")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(idleCalls) > 0 && idleCalls[len(idleCalls)-1] == "sess-big"
	}, 5*time.Second, 50*time.Millisecond,
		"tracker must receive idle for sess-big AFTER the large event (scanner buffer regression)")

	tracker.Stop()
}

// #901 G3: last-event-age gauge — never-received workspaces report a
// large age (visible silence), recent events report near-zero.
func TestRefreshLastEventGauges(t *testing.T) {
	recordLastEvent("ws-recent")
	RefreshLastEventGauges([]string{"ws-recent", "ws-never"})

	age := promtestutil.ToFloat64(lastEventAgeGauge.WithLabelValues("ws-recent"))
	assert.InDelta(t, 0.0, age, 5.0, "recent event: age ~0")

	never := promtestutil.ToFloat64(lastEventAgeGauge.WithLabelValues("ws-never"))
	assert.GreaterOrEqual(t, never, 300.0, "never-received: the silence must be visible, not absent")
}

// #902: processEvent records upstream liveness for every event.
func TestProcessEvent_RecordsLastEvent(t *testing.T) {
	tr := NewTracker(nil, &testLogger{}, nil)
	lastEventMu.Lock()
	delete(lastEvent, "ws-rec")
	lastEventMu.Unlock()
	tr.processEvent("ws-rec", `{"type":"server.connected"}`)
	lastEventMu.Lock()
	_, ok := lastEvent["ws-rec"]
	ts := lastEvent["ws-rec"]
	lastEventMu.Unlock()
	assert.True(t, ok, "processEvent must record the event timestamp")
	assert.WithinDuration(t, time.Now(), ts, 2*time.Second)
}

// #906 review: the backoff reset pinned. connectAndRead ALWAYS returns
// non-nil, so the pre-fix reset-on-nil branch was dead code — a healthy
// long-lived connection that ended kept the maxed 30s backoff forever.
func TestBackoffAfterConnect(t *testing.T) {
	orig := healthyConnBackoffReset
	healthyConnBackoffReset = 100 * time.Millisecond
	t.Cleanup(func() { healthyConnBackoffReset = orig })

	// Healthy long-lived connection ended: earned a fast retry.
	assert.Equal(t, 2*time.Second, backoffAfterConnect(30*time.Second, 200*time.Millisecond),
		"a connection that lived past the healthy threshold must reset the backoff")
	// Short-lived failure: keep the current (possibly maxed) backoff.
	assert.Equal(t, 30*time.Second, backoffAfterConnect(30*time.Second, 10*time.Millisecond),
		"a quickly-failed connection must keep the current backoff")
	assert.Equal(t, 8*time.Second, backoffAfterConnect(8*time.Second, 50*time.Millisecond))
	// Boundary: exactly the threshold is NOT a reset (strictly greater).
	assert.Equal(t, 30*time.Second, backoffAfterConnect(30*time.Second, 100*time.Millisecond))
}

// #901 G1: per-workspace connected gauge + stale-series cleanup.
func TestTrackerConnectedGauge_SeriesDeletedOnStop(t *testing.T) {
	tr := NewTracker(nil, &testLogger{}, nil)
	setTrackerConnected("ws-g", true)
	assert.Equal(t, 1.0, promtestutil.ToFloat64(trackerConnectedGauge.WithLabelValues("ws-g")))

	deleteTrackerSeries("ws-g")
	// No series = ToFloat64 of a deleted label errors; assert via the
	// collector instead: gather and confirm absence.
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == "llmsafespaces_sse_tracker_connected" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					require.NotEqual(t, "ws-g", l.GetValue(),
						"StopWatching must delete the workspace's gauge series (stale series fired UpstreamSilent forever)")
				}
			}
		}
	}
	_ = tr
}

// TestTrackerConnectedGauge_InitializedAtArmTime (#906 review F2): a
// watch that has NEVER connected must already emit a connected series
// reading 0 — absent ≠ 0 in PromQL, and the never-connected class was
// the incident's alert-invisible shape. With init-at-arm + deletion on
// StopWatching, a PRESENT series implies armed.
func TestTrackerConnectedGauge_InitializedAtArmTime(t *testing.T) {
	tr := NewTracker(nil, &testLogger{}, nil)
	tr.EnsureWatching("ws-never-conn")

	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(trackerConnectedGauge.WithLabelValues("ws-never-conn")) == 0
	}, 2*time.Second, 10*time.Millisecond,
		"the connected series must exist at 0 the moment the watch is armed — before any connection attempt")

	// StopWatching removes it again (armed-implies-present invariant).
	tr.StopWatching("ws-never-conn")
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() == "llmsafespaces_sse_tracker_connected" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					require.NotEqual(t, "ws-never-conn", l.GetValue(),
						"stopped watch must delete its series")
				}
			}
		}
	}
}

// TestTracker_PodIPUnavailableCounter (#901 G11 / #906 r6): a watch
// whose workspace has no podIP (resume race) must tick the counter on
// every connect attempt — the resume-race visibility signal.
func TestTracker_PodIPUnavailableCounter(t *testing.T) {
	tr := NewTracker(nil, &testLogger{}, nil)
	tr.SetPodIPResolver(func(string) string { return "" })
	tr.SetPasswordGetter(fakePWProvider{})
	tr.EnsureWatching("ws-noip")

	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(podIPUnavailable.WithLabelValues("ws-noip")) >= 1
	}, 3*time.Second, 10*time.Millisecond,
		"empty-podIP connect attempts must tick pod_ip_unavailable_total")
}
