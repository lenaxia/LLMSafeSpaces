// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- stripVerboseQuery tests ---

func TestStripVerboseQuery_StripsVerbose(t *testing.T) {
	result := stripVerboseQuery("verbose=true&limit=10")
	assert.Equal(t, "limit=10", result)
}

func TestStripVerboseQuery_StripsWorkspace(t *testing.T) {
	result := stripVerboseQuery("workspace=ws_abc&limit=10")
	assert.Equal(t, "limit=10", result)
}

func TestStripVerboseQuery_StripsDirectory(t *testing.T) {
	result := stripVerboseQuery("directory=%2Fhome%2Fuser&limit=10")
	assert.Equal(t, "limit=10", result)
}

func TestStripVerboseQuery_StripsAllThree(t *testing.T) {
	result := stripVerboseQuery("verbose=true&workspace=ws_1&directory=/tmp&limit=5&offset=0")
	// Remaining params preserved (order may vary due to map iteration)
	assert.Contains(t, result, "limit=5")
	assert.Contains(t, result, "offset=0")
	assert.NotContains(t, result, "verbose")
	assert.NotContains(t, result, "workspace")
	assert.NotContains(t, result, "directory")
}

func TestStripVerboseQuery_PreservesOtherParams(t *testing.T) {
	result := stripVerboseQuery("limit=10&offset=0&search=hello")
	assert.Contains(t, result, "limit=10")
	assert.Contains(t, result, "offset=0")
	assert.Contains(t, result, "search=hello")
}

func TestStripVerboseQuery_EmptyString(t *testing.T) {
	assert.Equal(t, "", stripVerboseQuery(""))
}

func TestStripVerboseQuery_OnlyStrippedParams(t *testing.T) {
	result := stripVerboseQuery("verbose=true&workspace=ws_1&directory=/tmp")
	assert.Equal(t, "", result)
}

// --- persistTitleFromEvent tests ---

type mockSessionIndex struct {
	mu          sync.Mutex
	titles      map[string]string // key: "workspaceID/sessionID"
	contextUsed map[string]*int64 // key: "workspaceID/sessionID"
}

func newMockSessionIndex() *mockSessionIndex {
	return &mockSessionIndex{
		titles:      make(map[string]string),
		contextUsed: make(map[string]*int64),
	}
}

func (m *mockSessionIndex) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.titles[workspaceID+"/"+sessionID] = title
	return nil
}

func (m *mockSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (m *mockSessionIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (m *mockSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error  { return nil }
func (m *mockSessionIndex) DeleteSession(_ context.Context, _, _ string) error   { return nil }
func (m *mockSessionIndex) UpsertParent(_ context.Context, _, _, _ string) error { return nil }
func (m *mockSessionIndex) UpsertContextUsed(_ context.Context, workspaceID, sessionID string, contextUsed int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := contextUsed
	m.contextUsed[workspaceID+"/"+sessionID] = &v
	return nil
}
func (m *mockSessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error { return nil }
func (m *mockSessionIndex) Start() error                                        { return nil }
func (m *mockSessionIndex) Stop() error                                         { return nil }

var _ interfaces.SessionIndexService = (*mockSessionIndex)(nil)

func TestPersistTitleFromEvent_V115FlatFormat(t *testing.T) {
	// v1.15 format: {"id":"evt_...","type":"session.updated","properties":{"sessionID":"ses_123","info":{"id":"ses_123","title":"My Title",...}}}
	event := `{"id":"evt_abc","type":"session.updated","properties":{"sessionID":"ses_123","info":{"id":"ses_123","title":"Hello World","slug":"hello-world"}}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-1", event)

	assert.Equal(t, "Hello World", mock.titles["ws-1/ses_123"])
}

func TestPersistTitleFromEvent_V127FlatFormat(t *testing.T) {
	// v1.2.27 format: {"type":"session.updated","properties":{"info":{"id":"ses_456","title":"Old Format"}}}
	event := `{"type":"session.updated","properties":{"info":{"id":"ses_456","title":"Old Format"}}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-2", event)

	assert.Equal(t, "Old Format", mock.titles["ws-2/ses_456"])
}

func TestPersistTitleFromEvent_MissingTitle(t *testing.T) {
	event := `{"id":"evt_abc","type":"session.updated","properties":{"sessionID":"ses_123","info":{"id":"ses_123"}}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-1", event)

	assert.Empty(t, mock.titles)
}

func TestPersistTitleFromEvent_MissingInfoID(t *testing.T) {
	event := `{"id":"evt_abc","type":"session.updated","properties":{"sessionID":"ses_123","info":{"title":"No ID"}}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-1", event)

	assert.Empty(t, mock.titles)
}

func TestPersistTitleFromEvent_MalformedJSON(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-1", "not json at all")

	assert.Empty(t, mock.titles)
}

func TestPersistTitleFromEvent_EmptyProperties(t *testing.T) {
	event := `{"id":"evt_abc","type":"session.updated","properties":{}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock}
	h.persistTitleFromEvent("ws-1", event)

	assert.Empty(t, mock.titles)
}

type testSSEEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

func newTestSSETracker(onIdle sse.SessionIdleCallback) *sse.Tracker {
	return sse.NewTracker(&http.Client{Timeout: 2 * time.Second}, &testLogger{}, onIdle)
}

func TestSSETracker_ProcessEvent_V115Format_ParsesIDField(t *testing.T) {
	var mu sync.Mutex
	var capturedRawData string

	tracker := newTestSSETracker(func(workspaceID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		capturedRawData = rawData
		mu.Unlock()
	})

	// v1.15 format with id field
	event := `{"id":"evt_01jw123","type":"session.status","properties":{"sessionID":"ses_abc","status":{"type":"idle"}}}`
	tracker.ProcessEvent("ws-1", event)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, event, capturedRawData)

	// Verify the event was parsed correctly (id field didn't break parsing)
	var parsed testSSEEvent
	require.NoError(t, json.Unmarshal([]byte(event), &parsed))
	assert.Equal(t, "evt_01jw123", parsed.ID)
	assert.Equal(t, "session.status", parsed.Type)
}

func TestSSETracker_ProcessEvent_V115Format_SessionIdleStillWorks(t *testing.T) {
	var mu sync.Mutex
	var idleCalls []string

	tracker := newTestSSETracker(func(workspaceID, sessionID string) {
		mu.Lock()
		idleCalls = append(idleCalls, sessionID)
		mu.Unlock()
	})

	// v1.15 format with id field — should still trigger idle callback
	event := `{"id":"evt_01jw456","type":"session.status","properties":{"sessionID":"ses_xyz","status":{"type":"idle"}}}`
	tracker.ProcessEvent("ws-1", event)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, idleCalls, 1)
	assert.Equal(t, "ses_xyz", idleCalls[0])
}

func TestSSETracker_ProcessEvent_V115Format_HeartbeatIgnored(t *testing.T) {
	var mu sync.Mutex
	var eventTypes []string

	tracker := newTestSSETracker(func(workspaceID, sessionID string) {})
	tracker.SetOnRawEvent(func(workspaceID, eventType, rawData string) {
		mu.Lock()
		eventTypes = append(eventTypes, eventType)
		mu.Unlock()
	})

	// v1.15 heartbeat format
	event := `{"id":"evt_hb001","type":"server.heartbeat","properties":{}}`
	tracker.ProcessEvent("ws-1", event)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, eventTypes, 1)
	assert.Equal(t, "server.heartbeat", eventTypes[0])
}

// --- persistContextFromEvent wiring tests ---
//
// Translation (wire shapes → ContextUsage math) is owned by the opencode
// adapter and pinned in pkg/agent/opencode tests, including golden-fixture
// coverage. These tests pin the HANDLER contract: adapter-provided usage
// reaches the session index; everything else is skipped.

func TestPersistContextFromEvent_AdapterUsage_Persisted(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_abc": 1050}))

	event := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc","tokens":{"input":800,"output":400,"reasoning":100,"cache":{"read":200,"write":50}}}}`
	h.persistContextFromEvent("ws-1", "session.next.step.ended", event)

	v := mock.contextUsed["ws-1/ses_abc"]
	require.NotNil(t, v, "adapter-provided usage must be stored")
	assert.Equal(t, int64(1050), *v)
}

func TestPersistContextFromEvent_NewShapeEvent_Persisted(t *testing.T) {
	// The 1.18.x wire shape: usage inside step-finish parts of
	// message.part.updated events (suffixed). The handler must not care —
	// the adapter decides what carries usage.
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_new": 3590}))

	event := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_new","part":{"type":"step-finish","tokens":{"input":2310,"cache":{"read":1280,"write":0}}}}}`
	h.persistContextFromEvent("ws-1", "message.part.updated.1", event)

	v := mock.contextUsed["ws-1/ses_new"]
	require.NotNil(t, v)
	assert.Equal(t, int64(3590), *v)
}

func TestPersistContextFromEvent_ZeroUsage_Persisted(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_xyz": 0}))

	event := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_xyz","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}`
	h.persistContextFromEvent("ws-1", "session.next.step.ended", event)

	v := mock.contextUsed["ws-1/ses_xyz"]
	require.NotNil(t, v, "zero usage is real usage — must be stored")
	assert.Equal(t, int64(0), *v)
}

func TestPersistContextFromEvent_AdapterSaysNoUsage_NoWrite(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(nil))

	event := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc"}}`
	h.persistContextFromEvent("ws-1", "session.next.step.ended", event)

	assert.Nil(t, mock.contextUsed["ws-1/ses_abc"], "ok=false must not write")
}

func TestPersistContextFromEvent_MalformedJSON_NoWrite(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_abc": 1}))

	h.persistContextFromEvent("ws-1", "session.next.step.ended", "not json at all")

	assert.Empty(t, mock.contextUsed, "malformed JSON must not write")
}

func TestPersistContextFromEvent_NilAdapter_NoWriteNoPanic(t *testing.T) {
	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}

	event := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc","tokens":{"input":800}}}`
	assert.NotPanics(t, func() { h.persistContextFromEvent("ws-1", "session.next.step.ended", event) })
	assert.Empty(t, mock.contextUsed)
}

func TestPersistContextFromEvent_NilSessionIndex_NoPanic(t *testing.T) {
	h := &ProxyHandler{sessionIndex: nil}
	event := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_abc","tokens":{"input":800}}}`
	assert.NotPanics(t, func() { h.persistContextFromEvent("ws-1", "session.next.step.ended", event) })
}

func TestOnRawEvent_UsageEvent_PersistsThroughFullPath(t *testing.T) {
	// Integration: onRawEvent → persistContextFromEvent → session index,
	// for BOTH wire generations (legacy standalone event and part-update).
	legacy := `{"type":"session.next.step.ended","properties":{"sessionID":"ses_old","tokens":{"input":500,"cache":{"read":100,"write":25}}}}`
	modern := `{"id":"evt1","type":"message.part.updated.1","properties":{"sessionID":"ses_new","part":{"type":"step-finish","tokens":{"input":900,"cache":{"read":100,"write":0}}}}}`

	mock := newMockSessionIndex()
	h := &ProxyHandler{sessionIndex: mock, logger: &testLogger{}}
	h.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_old": 625, "ses_new": 1000}))

	h.onRawEvent("ws-1", "session.next.step.ended", legacy)
	h.onRawEvent("ws-1", "message.part.updated.1", modern)

	assert.Equal(t, int64(625), *mock.contextUsed["ws-1/ses_old"])
	assert.Equal(t, int64(1000), *mock.contextUsed["ws-1/ses_new"])
}
