// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1340 S5b: the reconciliation ORCHESTRATION (the pure decision matrix
// lives in sessionindex.PlanReconciliation's own tests). These pins
// prove the wiring: index rows → harness list → wsstate counters →
// DeleteSession after N consecutive absences; harness errors never reap.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func reconcileTestHandler(t *testing.T, harnessSessions map[string]bool, harnessErr error) (*ProxyHandler, *mockSessionIndex) {
	t.Helper()
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			if harnessErr != nil {
				return nil, harnessErr
			}
			out := make([]session.Session, 0, len(harnessSessions))
			for id := range harnessSessions {
				out = append(out, session.Session{ID: id})
			}
			return out, nil
		},
	}
	return h, idx
}

func runReconcile(t *testing.T, h *ProxyHandler, workspaceID string) {
	t.Helper()
	h.runSessionIndexReconciliation(workspaceID)
}

func TestSessionIndexReconcile_ReapsAfterTwoConsecutiveAbsences(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_alive")
	idx.seedRow("ws-1", "ses_ghost") // count 0: guard admits

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "first absence increments, never reaps")
	assert.Equal(t, map[string]int{"ses_ghost": 1},
		h.state().GetReconcileMisses(context.Background(), "ws-1"))

	runReconcile(t, h, "ws-1")
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "second consecutive absence reaps the count-zero ghost")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_alive")
	assert.Empty(t, h.state().GetReconcileMisses(context.Background(), "ws-1"),
		"reaped + present rows leave no counters behind")
}

// The triage safety guard: a history-bearing row the harness reports
// gone is NEVER auto-deleted — kept for operator review (the read path
// still answers the typed gone-state, so the UX self-heals).
func TestSessionIndexReconcile_HistoryBearingGhostKeptForOperator(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	idx.seedRowCounted("ws-1", "ses_vanished", 12)
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_vanished": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "message_count>0 rows are never auto-deleted")
	// The kept row's counter pins AT the threshold (r2): every
	// subsequent pass re-detects it — the operator signal is
	// consistent, never the r2-found 1→2→1 oscillation.
	assert.Equal(t, map[string]int{"ses_vanished": reconcileMissThreshold},
		h.state().GetReconcileMisses(context.Background(), "ws-1"))
}

func TestSessionIndexReconcile_PresenceResetsMiss(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_flap": true}, nil)
	idx.seedRow("ws-1", "ses_flap")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_flap": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "a present session never reaps regardless of prior misses")
	assert.Empty(t, h.state().GetReconcileMisses(context.Background(), "ws-1"))
}

func TestSessionIndexReconcile_HarnessErrorNeverReaps(t *testing.T) {
	h, idx := reconcileTestHandler(t, nil, fmt.Errorf("pod unreachable"))
	idx.seedRow("ws-1", "ses_maybe")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_maybe": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "an unreachable pod is not evidence of deletion")
	assert.Equal(t, map[string]int{"ses_maybe": 1},
		h.state().GetReconcileMisses(context.Background(), "ws-1"), "counters frozen on error")
}

func TestSessionIndexReconcile_CadenceGate(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h, idx := reconcileTestHandler(t, nil, nil)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, nil
		},
	}
	// Rows must exist or runSessionIndexReconciliation returns before
	// the adapter call (the empty-index fast path).
	idx.seedRow("ws-1", "ses_a")
	idx.seedRow("ws-2", "ses_b")

	ctx := context.Background()
	h.ReconcileSessionIndex(ctx, "ws-1")
	h.ReconcileSessionIndex(ctx, "ws-1")
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	}, 3*time.Second, 50*time.Millisecond, "the cross-replica turn claim must coalesce bursts")

	// The claim is per workspace: a different workspace still runs.
	h.ReconcileSessionIndex(ctx, "ws-2")
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, 3*time.Second, 50*time.Millisecond)
}

func TestSessionIndexReconcile_EmptyIndexSkipsHarnessCall(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil) // lister would reap everything
	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree)
}

// r4: the pass's delete-failure leg — the pass CONTINUES (no abort,
// later deletions still run) and every disposition is counted.
func TestSessionIndexReconcile_DeleteFailedContinuesAndCounts(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_alive")          // present
	idx.seedRowCounted("ws-1", "ses_dies", 0) // guard-admitted ghost
	idx.seedRowCounted("ws-1", "ses_kept", 9) // history-bearing ghost
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{
		"ses_dies": 1, "ses_kept": 1,
	})

	reapedBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped"))
	keptBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator"))
	failedBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("delete_failed"))

	idx.failDelete = true
	runReconcile(t, h, "ws-1")

	assert.Empty(t, idx.deletedTree, "the failing delete removed nothing")
	// The pass continued through the failure: the kept_for_operator
	// disposition was still recorded in the SAME pass.
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("delete_failed")), failedBefore,
		"the delete failure must be counted")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator")), keptBefore,
		"the pass must continue past a failed deletion")

	// Recover: the same pass next window reaps cleanly and counts it.
	idx.failDelete = false
	runReconcile(t, h, "ws-1")
	assert.True(t, idx.deletedTree["ws-1/ses_dies"], "a later pass must still reap")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped")), reapedBefore)
}
