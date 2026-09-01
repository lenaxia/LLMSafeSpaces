// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
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

// --- session index mock ---
//
// US-69.11: the persistTitleFromEvent / persistContextFromEvent dialect
// parsing tests (opencode session.updated JSON of several wire
// generations) were deleted with the tracker — the ABI consumer now
// delivers titles and context usage as structured fields, so there is
// no dialect left to pin (the ABI decode is pinned in
// api/internal/services/usagestream tests). What survives is the
// HANDLER contract: bridge-provided titles/usage reach the session
// index; deleted sessions are skipped (see also proxy_test.go).

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

// --- usage bridge persistence tests (US-69.11) ---

func newBridgeIndexHandler(t *testing.T) (*ProxyHandler, *mockSessionIndex) {
	t.Helper()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(newMockSessionIndex())
	si := env.handler.sessionIndex.(*mockSessionIndex)
	t.Cleanup(stubUsageStream())
	return env.handler, si
}

func TestBridgeSessionTitle_Persists(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).SessionTitle("ws-1", "ses_123", "Hello World")

	assert.Equal(t, "Hello World", si.titles["ws-1/ses_123"])
}

func TestBridgeSessionTitle_EmptyTitleOrSession_Skipped(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).SessionTitle("ws-1", "ses_123", "")
	(&usageBridge{h: h}).SessionTitle("ws-1", "", "Orphan")

	assert.Empty(t, si.titles)
}

func TestBridgeContextUsed_Persists(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_abc", 1050)

	v := si.contextUsed["ws-1/ses_abc"]
	require.NotNil(t, v, "bridge-provided usage must be stored")
	assert.Equal(t, int64(1050), *v)
}

func TestBridgeContextUsed_NonPositiveSkipped(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_abc", 0)
	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_abc", -5)

	assert.Nil(t, si.contextUsed["ws-1/ses_abc"], "non-positive usage must not write")
}

func TestBridgeContextUsed_OverwritesPreviousValue(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_1", 5000)
	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_1", 61500)

	v := si.contextUsed["ws-1/ses_1"]
	require.NotNil(t, v)
	assert.Equal(t, int64(61500), *v, "latest step overwrites previous contextUsed")
}

func TestBridgeContextUsed_MultipleSessionsTrackedIndependently(t *testing.T) {
	h, si := newBridgeIndexHandler(t)

	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_1", 5000)
	(&usageBridge{h: h}).ContextUsed("ws-1", "ses_2", 80000)

	assert.Equal(t, int64(5000), *si.contextUsed["ws-1/ses_1"])
	assert.Equal(t, int64(80000), *si.contextUsed["ws-1/ses_2"])
}

func TestBridge_NilSessionIndex_NoPanic(t *testing.T) {
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(stubUsageStream())

	assert.NotPanics(t, func() {
		(&usageBridge{h: env.handler}).SessionTitle("ws-1", "s", "t")
		(&usageBridge{h: env.handler}).ContextUsed("ws-1", "s", 10)
	})
}
