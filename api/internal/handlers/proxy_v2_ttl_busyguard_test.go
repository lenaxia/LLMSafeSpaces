// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// --- #744 Finding 1: busy-session guard ---

func TestV2StrandedRecovery_SkipsBusySession(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-idle")
	handler.v2Pending.add("ws-1", "ses-busy")

	busySessions := map[string]bool{"ses-busy": true}
	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", busySessions)

	assert.Equal(t, int32(1), atomic.LoadInt32(&wakeCount),
		"only the idle session must be woken; busy session must be skipped")
}

func TestV2StrandedRecovery_NilBusySet_WakesAll(t *testing.T) {
	var wakeCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/prompt") {
			atomic.AddInt32(&wakeCount, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending.add("ws-1", "ses-a")
	handler.v2Pending.add("ws-1", "ses-b")

	handler.wakeStrandedV2Sessions(context.Background(), "ws-1", nil)

	assert.Equal(t, int32(2), atomic.LoadInt32(&wakeCount),
		"nil busySet must wake all pending sessions (backward compat)")
}

// --- #744 Finding 2: TTL pruning ---

func TestV2PendingSessions_TTLPrunesStaleEntries(t *testing.T) {
	v := newV2PendingSessions()

	v.add("ws-1", "ses-fresh")
	v.add("ws-1", "ses-stale")

	// Manually age the stale entry past the TTL threshold.
	v.mu.Lock()
	e := v.data["ws-1"]["ses-stale"]
	e.lastAdded = time.Now().Add(-v2PendingTTL - time.Minute)
	v.data["ws-1"]["ses-stale"] = e
	v.mu.Unlock()

	// Trigger a read which prunes.
	sessions := v.sessionsForWorkspace("ws-1")

	assert.Len(t, sessions, 1, "stale entry must be pruned, fresh entry retained")
	assert.Contains(t, sessions, "ses-fresh")
	assert.False(t, v.has("ws-1", "ses-stale"), "stale entry must not be visible after prune")
	assert.True(t, v.has("ws-1", "ses-fresh"), "fresh entry must survive")
}

func TestV2PendingSessions_TTLPrunesOnRemove(t *testing.T) {
	v := newV2PendingSessions()

	v.add("ws-1", "ses-stale")
	v.add("ws-1", "ses-fresh")

	// Age the stale entry.
	v.mu.Lock()
	e := v.data["ws-1"]["ses-stale"]
	e.lastAdded = time.Now().Add(-v2PendingTTL - time.Minute)
	v.data["ws-1"]["ses-stale"] = e
	v.mu.Unlock()

	v.remove("ws-1", "ses-fresh")

	assert.False(t, v.has("ws-1", "ses-stale"), "stale entry must be pruned during remove")
	assert.False(t, v.has("ws-1", "ses-fresh"), "fresh entry removed normally")
}

func TestV2PendingSessions_TTLFreshEntrySurvives(t *testing.T) {
	v := newV2PendingSessions()
	v.add("ws-1", "ses-recent")

	// Entry is fresh (just added) — should survive reads.
	assert.True(t, v.has("ws-1", "ses-recent"))
	sessions := v.sessionsForWorkspace("ws-1")
	assert.Contains(t, sessions, "ses-recent")
}

// --- #744 Finding 3: nil-guard symmetry ---

func TestEnqueueV2_NilV2Pending_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_1","sessionID":"ses_1"}}`))
	}))
	defer srv.Close()

	_, handler := newV2TestHandler(t, srv)
	handler.v2Pending = nil

	assert.NotPanics(t, func() {
		if handler.v2Pending != nil {
			handler.v2Pending.add("ws-1", "ses-1")
		}
	})
}
