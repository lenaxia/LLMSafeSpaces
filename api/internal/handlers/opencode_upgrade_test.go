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
	mu            sync.Mutex
	titles        map[string]string // key: "workspaceID/sessionID"
	contextUsed   map[string]*int64 // key: "workspaceID/sessionID"
	deletedTree   map[string]bool   // key: "workspaceID/sessionID" (DeleteSession recording)
	failDelete    bool              // DeleteSession returns an error when set
	failList      bool              // ListByWorkspace returns an error when set
	rebuiltCounts map[string]int    // key: "workspaceID/sessionID" (#1481 count rebuilds)
	rows          map[string][]types.SessionListItem
}

func newMockSessionIndex() *mockSessionIndex {
	return &mockSessionIndex{
		titles:      make(map[string]string),
		contextUsed: make(map[string]*int64),
		deletedTree: make(map[string]bool),
		rows:        make(map[string][]types.SessionListItem),
	}
}

func (m *mockSessionIndex) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.titles[workspaceID+"/"+sessionID] = title
	return nil
}

func (m *mockSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (m *mockSessionIndex) ListByWorkspace(_ context.Context, workspaceID string) ([]types.SessionListItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failList {
		return nil, assert.AnError
	}
	return append([]types.SessionListItem{}, m.rows[workspaceID]...), nil
}

// seedRow / seedRowCounted are test helpers for reconciliation scenarios.
func (m *mockSessionIndex) seedRow(workspaceID, sessionID string) {
	m.seedRowCounted(workspaceID, sessionID, 0)
}

func (m *mockSessionIndex) seedRowCounted(workspaceID, sessionID string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[workspaceID] = append(m.rows[workspaceID], types.SessionListItem{ID: sessionID, Title: sessionID, MessageCount: count})
}
func (m *mockSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error { return nil }
func (m *mockSessionIndex) DeleteSession(_ context.Context, workspaceID, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete {
		return assert.AnError
	}
	m.deletedTree[workspaceID+"/"+sessionID] = true
	return nil
}
func (m *mockSessionIndex) RebuildMessageCount(_ context.Context, workspaceID, sessionID string, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rebuiltCounts == nil {
		m.rebuiltCounts = make(map[string]int)
	}
	m.rebuiltCounts[workspaceID+"/"+sessionID] = count
	return nil
}

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
