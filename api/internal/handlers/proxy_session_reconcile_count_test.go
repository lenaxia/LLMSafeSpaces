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

// TestSessionIndexReconcile_TruncatedWalkNeverPersistsUndercount — the
// round-1 minor finding, pinned: a ceiling-exhausted walk returns the
// truncation sentinel; the rebuild skips the row (count_walk_error)
// rather than persisting a floor as authoritative ground truth.
func TestSessionIndexReconcile_TruncatedWalkNeverPersistsUndercount(t *testing.T) {
	now := time.Now().UTC()
	h, idx, _ := countTestHandler(t,
		[]string{"ses_big"},
		nil,
		map[string]error{"ses_big": agent.ErrMessageCountTruncated})
	seedRowCountedAt(t, idx, "ws-1", "ses_big", 22000, now)

	runReconcile(t, h, "ws-1")

	assert.Empty(t, idx.rebuiltCounts,
		"a truncated walk is a floor — persisting it would authoritatively undercount a possibly-correct row")
}

// #1756 post-union finding 5 pin: RebuildMessageCount is a row-creating
// upsert keyed by the id passed — it MUST receive the harness id
// (InternalID). Passing the phrase (row.ID) minted a ghost row keyed by
// the phrase while the real row never converged. The row here is
// minted (ID=phrase, InternalID=harness) so a phrase-keyed write lands
// in rebuiltCounts under the phrase and the assertion fails.
// TestSessionIndexReconcile_RebuildWritesInternalIDNotPhrase — see above.
func TestSessionIndexReconcile_RebuildWritesInternalIDNotPhrase(t *testing.T) {
	now := time.Now().UTC()
	h, idx, _ := countTestHandler(t,
		[]string{"ses_drift"},
		map[string]int{"ses_drift": 5}, nil)
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "amber-falcon-glides", InternalID: "ses_drift",
		MessageCount: 7, LastMessageAt: &now,
	})
	idx.mu.Unlock()

	runReconcile(t, h, "ws-1")

	require.Equal(t, map[string]int{"ws-1/ses_drift": 5}, idx.rebuiltCounts,
		"the rebuild write must be keyed by the harness id, never the phrase")
	require.NotContains(t, idx.rebuiltCounts, "ws-1/amber-falcon-glides",
		"a phrase-keyed rebuild would mint a ghost row")
}

// TestSessionIndexReconcile_PresentMissIsLoud — the #1880 convergence
// hole made visible: an aged count-zero row the harness LIST omits
// (the #1452 list-visibility gap — live spawn-created sessions) is
// excluded from the rebuild candidates FOREVER. That exclusion was
// silent; now it carries a metric + Warn so a drifting workspace is
// diagnosable from the outside. History-bearing absent rows stay with
// the operator/ghost machinery (kept_for_operator / miss counters —
// NOT this metric).
func TestSessionIndexReconcile_PresentMissIsLoud(t *testing.T) {
	now := time.Now().UTC()
	h, idx, counted := countTestHandler(t,
		[]string{"ses_alive"},
		map[string]int{"ses_alive": 2}, nil)
	seedRowCountedAt(t, idx, "ws-1", "ses_alive", 2, now)
	// The drift row: aged count-zero, list-invisible (spawn class).
	aged := now.Add(-2 * time.Hour)
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "ses_spawn", InternalID: "ses_spawn",
		MessageCount: 0, LastMessageAt: nil, CreatedAt: &aged,
	})
	// A history-bearing absent row: NOT drift-suspect — the ghost
	// machinery owns it, the present-miss metric must not fire.
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "ses_ghost_hist", InternalID: "ses_ghost_hist",
		MessageCount: 30, LastMessageAt: &now,
	})
	idx.mu.Unlock()

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_not_present"))
	runReconcile(t, h, "ws-1")

	assert.Equal(t, []string{"ses_alive"}, *counted,
		"the absent drift row is still never walked by the rebuild (present-only by contract)")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_not_present")),
		"the list-invisible count-zero row's exclusion is counted — the silence was the diagnosis gap")
}

// TestSessionIndexReconcile_NoHarnessListCountsSkipLoud — when the
// harness list fails, the whole count rebuild silently returns
// (presentSet nil). One loud outcome per pass so "rebuild not
// converging" is attributable to list failures from metrics alone.
func TestSessionIndexReconcile_NoHarnessListCountsSkipLoud(t *testing.T) {
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

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_pass_no_list"))
	runReconcile(t, h, "ws-1")

	assert.Empty(t, counted, "no harness list, no count walks")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_pass_no_list")),
		"the pass-level skip is visible in metrics")
}

// TestSessionIndexReconcile_CountPassRanDenominator — review r2, the
// issue's third skip class ("the pass not being scheduled for this
// workspace at all"): a listing-driven workspace whose count leg
// actually EXECUTES must count count_pass_ran. That counter is the
// denominator — sidebar traffic with walk/skip outcomes but zero
// count_pass_ran means the pass is never scheduled; every outcome
// without it was invisible (the issue's "rows sat at 0 for 1–5h while
// the workspace was actively open").
func TestSessionIndexReconcile_CountPassRanDenominator(t *testing.T) {
	now := time.Now().UTC()
	h, _, _ := countTestHandler(t,
		[]string{"ses_alive"},
		map[string]int{"ses_alive": 2}, nil)
	seedRowCountedAt(t, h.sessionIndex.(*mockSessionIndex), "ws-1", "ses_alive", 2, now)

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_pass_ran"))
	runReconcile(t, h, "ws-1")

	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_pass_ran")),
		"a pass that reaches the count leg with a live present-set counts the pass-ran denominator")
}

// TestSessionIndexReconcile_RebuildRotatesThroughNullGroup — review
// r2, thread Finding 2 (rebuild stationarity): the candidate sort keys
// on last_message_at, but UpsertSessionMessageCount never writes that
// column — the whole count-0/NULL drift group sorts as one stable
// block, so the budget re-walked the SAME first-K rows every pass
// forever and rows K+1..N were never walked at all (7 of the issue's
// 10 evidence rows). The rotation fix: when candidates exceed the
// budget, the walk window starts at (passIndex × K) mod N — the pass
// index is a wall-clock bucket of the reconcile cadence (stateless
// and cross-replica-consistent under the cadence claim; injected here
// via countPassIndexOverride). Every candidate walks within
// ceil(N/K) passes; ≤budget workspaces keep the deterministic
// oldest-first walk.
func TestSessionIndexReconcile_RebuildRotatesThroughNullGroup(t *testing.T) {
	ids := []string{"ses_drift1", "ses_drift2", "ses_drift3",
		"ses_drift4", "ses_drift5", "ses_drift6"}
	counts := map[string]int{}
	for _, id := range ids {
		counts[id] = 9
	}
	h, idx, counted := countTestHandler(t, ids, counts, nil)
	idx.mu.Lock()
	for _, id := range ids {
		idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
			ID: id, InternalID: id, MessageCount: 0, LastMessageAt: nil,
		})
	}
	idx.mu.Unlock()

	window0 := int64(0)
	h.countPassIndexOverride = &window0
	runReconcile(t, h, "ws-1")
	assert.Equal(t, ids[0:3], *counted,
		"window zero walks the first three rows of the stable NULL group")

	window1 := int64(1)
	h.countPassIndexOverride = &window1
	runReconcile(t, h, "ws-1")
	assert.Equal(t, ids[3:6], (*counted)[3:],
		"window one MUST advance past the budget — the starved tail walks (the stationarity defect re-walked ids[0:3] here forever)")

	require.Len(t, idx.rebuiltCounts, 6,
		"two rotated passes converge the whole six-row drift group")
	for _, id := range ids {
		assert.Equal(t, 9, idx.rebuiltCounts["ws-1/"+id])
	}
}
