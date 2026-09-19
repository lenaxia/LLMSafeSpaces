// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1481 (the #754 fold-in via #1340/#1479): the reconcile pass rebuilds
// message_count for PRESENT sessions from the harness ground truth —
// the duplicate SSE event that double-incremented the incremental
// counter is repaired within one convergence window instead of
// persisting forever. Budgeted (K sessions/pass, oldest-touched first),
// drift-only writes, incremental RecordMessage stays as the
// between-passes approximation.

import (
	"context"
	"errors"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// countTestHandler wires a reconcile handler whose harness lists the
// given sessions and counts them per counts (absent = walk error).
func countTestHandler(t *testing.T, harnessSessions []string, counts map[string]int, walkErrs map[string]error) (*ProxyHandler, *mockSessionIndex, *[]string) {
	t.Helper()
	idx := newMockSessionIndex()
	var counted []string
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			out := make([]session.Session, 0, len(harnessSessions))
			for _, id := range harnessSessions {
				out = append(out, session.Session{ID: id})
			}
			return out, nil
		},
		countMessagesFn: func(_ context.Context, _, _, sessionID string) (int, error) {
			counted = append(counted, sessionID)
			if err, ok := walkErrs[sessionID]; ok {
				return 0, err
			}
			return counts[sessionID], nil
		},
	}
	return h, idx, &counted
}

func seedRowCountedAt(t *testing.T, idx *mockSessionIndex, workspaceID, sessionID string, count int, lastMessageAt time.Time) {
	t.Helper()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.rows[workspaceID] = append(idx.rows[workspaceID], types.SessionListItem{
		ID: sessionID, Title: sessionID, MessageCount: count, LastMessageAt: &lastMessageAt,
	})
}

// TestSessionIndexReconcile_RebuildsDriftedCount — the headline #754
// row: a duplicate SSE event double-incremented the row (7 recorded,
// 5 real); one reconcile pass repairs it to the harness ground truth.
func TestSessionIndexReconcile_RebuildsDriftedCount(t *testing.T) {
	now := time.Now().UTC()
	h, idx, _ := countTestHandler(t,
		[]string{"ses_drift"},
		map[string]int{"ses_drift": 5}, nil)
	seedRowCountedAt(t, idx, "ws-1", "ses_drift", 7, now)

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_rebuilt"))
	runReconcile(t, h, "ws-1")

	require.Equal(t, map[string]int{"ws-1/ses_drift": 5}, idx.rebuiltCounts,
		"the drifted count is rebuilt to the harness-walked ground truth")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_rebuilt")))
}

// TestSessionIndexReconcile_CountMatchWritesNothing — a converged row
// stays write-free (the DB DISTINCT guard's handler-side twin: no
// rebuild call, count_unchanged metric).
func TestSessionIndexReconcile_CountMatchWritesNothing(t *testing.T) {
	now := time.Now().UTC()
	h, idx, _ := countTestHandler(t,
		[]string{"ses_ok"},
		map[string]int{"ses_ok": 5}, nil)
	seedRowCountedAt(t, idx, "ws-1", "ses_ok", 5, now)

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_unchanged"))
	runReconcile(t, h, "ws-1")

	assert.Empty(t, idx.rebuiltCounts, "a matching count must not be rewritten")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_unchanged")))
}

// TestSessionIndexReconcile_CountBudgetOldestFirst — the per-pass budget
// walks the OLDEST-touched present rows first (steady-state convergence
// for every row within ceil(N/K) passes); younger rows wait for later
// passes.
func TestSessionIndexReconcile_CountBudgetOldestFirst(t *testing.T) {
	now := time.Now().UTC()
	ids := []string{"ses_new1", "ses_old1", "ses_mid1", "ses_old2", "ses_new2"}
	counts := map[string]int{}
	for _, id := range ids {
		counts[id] = 99
	}
	h, idx, counted := countTestHandler(t, ids, counts, nil)
	ages := map[string]time.Duration{
		"ses_old1": 90 * time.Minute,
		"ses_old2": 60 * time.Minute,
		"ses_mid1": 30 * time.Minute,
		"ses_new1": 5 * time.Minute,
		"ses_new2": 1 * time.Minute,
	}
	for _, id := range ids {
		seedRowCountedAt(t, idx, "ws-1", id, 1, now.Add(-ages[id]))
	}

	runReconcile(t, h, "ws-1")

	assert.Equal(t, []string{"ses_old1", "ses_old2", "ses_mid1"}, *counted,
		"the budget takes the three oldest-touched present rows, oldest first")
	assert.Len(t, idx.rebuiltCounts, 3)
}

// TestSessionIndexReconcile_CountWalkErrorSkipsRow — a walk failure
// (including the typed gone error mid-walk) skips the row, never fails
// the pass, and later budget rows still run.
func TestSessionIndexReconcile_CountWalkErrorSkipsRow(t *testing.T) {
	now := time.Now().UTC()
	h, idx, counted := countTestHandler(t,
		[]string{"ses_err1", "ses_ok1"},
		map[string]int{"ses_ok1": 4},
		map[string]error{"ses_err1": agent.ErrSessionNotFound})
	seedRowCountedAt(t, idx, "ws-1", "ses_err1", 9, now.Add(-2*time.Minute))
	seedRowCountedAt(t, idx, "ws-1", "ses_ok1", 9, now.Add(-1*time.Minute))

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_walk_error"))
	runReconcile(t, h, "ws-1")

	assert.Equal(t, []string{"ses_err1", "ses_ok1"}, *counted, "both rows were walked")
	assert.Equal(t, map[string]int{"ws-1/ses_ok1": 4}, idx.rebuiltCounts,
		"the failed walk skipped its row; the healthy row still rebuilt")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_walk_error")))
}

// TestSessionIndexReconcile_AbsentRowsNeverCounted — the rebuild walks
// PRESENT sessions only; ghost candidates go through the miss/reap
// machinery, never the counter.
func TestSessionIndexReconcile_AbsentRowsNeverCounted(t *testing.T) {
	now := time.Now().UTC()
	h, idx, counted := countTestHandler(t,
		[]string{"ses_alive"},
		map[string]int{"ses_alive": 2}, nil)
	seedRowCountedAt(t, idx, "ws-1", "ses_alive", 2, now)
	seedRowCountedAt(t, idx, "ws-1", "ses_ghost", 9, now)

	runReconcile(t, h, "ws-1")

	assert.Equal(t, []string{"ses_alive"}, *counted, "the absent row is a reap candidate, never a count walk")
}

var errCountTestHarness = errors.New("harness list failed")

// TestSessionIndexReconcile_HarnessErrorSkipsRebuildToo — when the
// lister errors, Plan never reaps AND the rebuild never walks (no
// count calls at all).
func TestSessionIndexReconcile_HarnessErrorSkipsRebuildToo(t *testing.T) {
	now := time.Now().UTC()
	idx := newMockSessionIndex()
	var counted []string
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return nil, errCountTestHarness
		},
		countMessagesFn: func(_ context.Context, _, _, sessionID string) (int, error) {
			counted = append(counted, sessionID)
			return 0, nil
		},
	}
	seedRowCountedAt(t, idx, "ws-1", "ses_a", 5, now)

	runReconcile(t, h, "ws-1")

	assert.Empty(t, counted, "no harness list, no count walks")
	assert.Empty(t, idx.rebuiltCounts)
}
