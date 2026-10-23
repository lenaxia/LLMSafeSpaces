// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1880 direction 3 — the listing serve-through: for the drift class
// (rows whose message stream never traversed the API proxy — spawn-
// created sessions invisible to the harness LIST, exactly the #1452
// gap the #1865 reap-veto patched on the reap side), the sidebar's
// session list serves the HARNESS-walked count authoritatively and
// persists it through the rebuild seam. The reconcile pass cannot
// converge these rows (its candidate set is present[]-derived), so the
// read path is the only convergence surface that reaches them.

import (
	"context"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// listingCountTestHandler wires a handler whose harness counts sessions
// per counts (absent = walk error), mirroring countTestHandler.
func listingCountTestHandler(t *testing.T, counts map[string]int, walkErrs map[string]error) (*ProxyHandler, *mockSessionIndex, *[]string) {
	t.Helper()
	idx := newMockSessionIndex()
	var counted []string
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
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

func driftRow(internalID string, age time.Duration) types.SessionListItem {
	created := time.Now().UTC().Add(-age)
	return types.SessionListItem{
		ID: internalID, InternalID: internalID,
		MessageCount: 0, CreatedAt: &created,
	}
}

// TestListingCount_ServesHarnessTruthForDriftRow — the headline #1880
// row, straight from the live evidence: index 0, harness 389, row aged
// past the spawn grace. One listing pass serves 389 on the wire AND
// persists it through the rebuild seam (the serve-through truth lands
// in the index, so every other count consumer converges too).
func TestListingCount_ServesHarnessTruthForDriftRow(t *testing.T) {
	h, idx, counted := listingCountTestHandler(t,
		map[string]int{"ses_profound": 389}, nil)
	sessions := []types.SessionListItem{driftRow("ses_profound", 2*time.Hour)}

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_served"))
	h.ServeHarnessMessageCounts(context.Background(), "ws-1", sessions)

	require.Equal(t, []string{"ses_profound"}, *counted,
		"the drift row is walked by ID — the list-visibility gap never applies to by-ID reads")
	assert.Equal(t, 389, sessions[0].MessageCount,
		"the listing response carries the harness truth, not the drifted index value")
	require.Equal(t, map[string]int{"ws-1/ses_profound": 389}, idx.rebuiltCounts,
		"the walked count is persisted through the rebuild seam (self-healing)")
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_served")))
}

// TestListingCount_WritesInternalIDNotPhrase — the #1756/#1881
// discipline on the new write path: the rebuild persist is keyed by
// the harness id. A phrase-keyed write would mint a ghost row while
// the real row never converges.
func TestListingCount_WritesInternalIDNotPhrase(t *testing.T) {
	h, idx, counted := listingCountTestHandler(t,
		map[string]int{"ses_drift": 42}, nil)
	sessions := []types.SessionListItem{{
		ID: "amber-falcon-glides", InternalID: "ses_drift",
		MessageCount: 0, CreatedAt: ptrTime(time.Now().UTC().Add(-2 * time.Hour)),
	}}

	h.ServeHarnessMessageCounts(context.Background(), "ws-1", sessions)

	require.Equal(t, []string{"ses_drift"}, *counted)
	require.Equal(t, map[string]int{"ws-1/ses_drift": 42}, idx.rebuiltCounts,
		"the persist is keyed by the harness id, never the phrase")
	require.NotContains(t, idx.rebuiltCounts, "ws-1/amber-falcon-glides")
}

// TestListingCount_WalkErrorServesIndexValueNoPersist — a failed walk
// (dead pod, gone session) serves the index value (never worse than
// today), persists nothing, and counts the outcome loudly: the silence
// is exactly why the drift went unnoticed.
func TestListingCount_WalkErrorServesIndexValueNoPersist(t *testing.T) {
	h, idx, counted := listingCountTestHandler(t, nil,
		map[string]error{"ses_gone": agent.ErrSessionNotFound})
	sessions := []types.SessionListItem{driftRow("ses_gone", 2*time.Hour)}

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error"))
	h.ServeHarnessMessageCounts(context.Background(), "ws-1", sessions)

	require.Equal(t, []string{"ses_gone"}, *counted)
	assert.Equal(t, 0, sessions[0].MessageCount,
		"a failed walk serves the index value — never worse than the pre-fix wire")
	assert.Empty(t, idx.rebuiltCounts)
	assert.Equal(t, before+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error")))
}

// TestListingCount_OrdersOldestTouchedFirst — budget fairness mirrors
// the rebuild pass: never-walked rows first, then oldest walk. With 5
// eligible rows and budget 3, the FIRST THREE in slice order walk
// (never-walked ties break stably).
func TestListingCount_OrdersOldestTouchedFirst(t *testing.T) {
	counts := map[string]int{}
	ids := []string{"ses_a", "ses_b", "ses_c", "ses_d", "ses_e"}
	for _, id := range ids {
		counts[id] = 7
	}
	h, _, counted := listingCountTestHandler(t, counts, nil)
	sessions := make([]types.SessionListItem, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, driftRow(id, time.Hour))
	}

	h.ServeHarnessMessageCounts(context.Background(), "ws-1", sessions)

	assert.Equal(t, []string{"ses_a", "ses_b", "ses_c"}, *counted,
		"never-walked rows walk first, budget-capped at 3, stable order")
}

// TestListingCount_RotatesBudgetAcrossPasses — the rotation pin: a
// second pass (fresh listing, tracker warm from the first) must reach
// the rows the budget skipped. The cooldown composes with rotation:
// the first pass's converged rows back off (their walked count now
// matches the served count), freeing the ENTIRE budget for the
// never-walked tail. Without rotation the same three rows would be
// walked on every request forever and rows 4+ never converge.
func TestListingCount_RotatesBudgetAcrossPasses(t *testing.T) {
	counts := map[string]int{}
	ids := []string{"ses_a", "ses_b", "ses_c", "ses_d", "ses_e"}
	for _, id := range ids {
		counts[id] = 7
	}
	h, _, counted := listingCountTestHandler(t, counts, nil)
	sessions := make([]types.SessionListItem, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, driftRow(id, time.Hour))
	}
	ctx := context.Background()
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)
	require.Equal(t, []string{"ses_a", "ses_b", "ses_c"}, *counted,
		"pass one takes the first three (never-walked ties, stable order)")
	// Second listing: the first three now carry walked counts — the
	// cooldown backs them off and the budget rotates to the tail.
	for i := range sessions {
		sessions[i].MessageCount = 7
	}
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)

	require.Len(t, *counted, 5, "two budgeted passes walk five rows total")
	assert.Equal(t, []string{"ses_d", "ses_e"}, (*counted)[3:],
		"the second pass's whole budget reaches the rows the first skipped")

	// Phase 3 isolates the ORDERING rule deterministically: backdate
	// ONLY the first pass's rows (one shared instant), keep d,e hot
	// (cooldown), and introduce two fresh never-walked rows — the sort
	// must put NEVER-WALKED first (f, g) ahead of the stale-walked a,
	// with the budget capping at three. Without the ordering rule,
	// slice order walks a, b, c and the tail starves.
	expired := time.Now().Add(-2 * countServeCooldown)
	tr := h.countServeState()
	tr.mu.Lock()
	for _, id := range ids[:3] {
		if _, ok := tr.lastAt["ws-1/"+id]; ok {
			tr.lastAt["ws-1/"+id] = expired
		}
	}
	tr.mu.Unlock()
	sessions = append(sessions, driftRow("ses_f", time.Hour), driftRow("ses_g", time.Hour))
	*counted = nil
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)
	assert.Equal(t, []string{"ses_f", "ses_g", "ses_a"}, *counted,
		"never-walked rows outrank stale-walked ones; the budget caps the pass")
}

// TestListingCount_CooldownBacksOffConvergedRows — a row whose last
// walk already converged (walked count == served count) backs off
// within the cooldown window: steady-state empty sessions (legitimately
// 0) and freshly-served drift rows alike stop consuming budget. Expiry
// re-arms the walk (bounded staleness for the never-traversed class).
func TestListingCount_CooldownBacksOffConvergedRows(t *testing.T) {
	h, _, counted := listingCountTestHandler(t,
		map[string]int{"ses_empty": 0, "ses_growing": 500}, nil)
	ctx := context.Background()
	sessions := []types.SessionListItem{driftRow("ses_empty", time.Hour), driftRow("ses_growing", time.Hour)}
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)
	require.Equal(t, []string{"ses_empty", "ses_growing"}, *counted)
	require.Equal(t, 500, sessions[1].MessageCount, "truth served on pass one")

	// Immediate re-listing: both rows' walks converged (0==0, 500==500)
	// — neither re-walks inside the cooldown.
	*counted = nil
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)
	assert.Empty(t, *counted,
		"converged rows back off within the cooldown window — the budget is for UNconverged truth")

	// Expiry re-arms: backdate the tracker past the cooldown (one
	// shared instant — per-key Now() noise would make the walk order
	// nondeterministic) and the rows walk again (bounded staleness,
	// never a permanent blind spot).
	tr := h.countServeState()
	expired := time.Now().Add(-2 * countServeCooldown)
	tr.mu.Lock()
	for k := range tr.lastAt {
		tr.lastAt[k] = expired
	}
	tr.mu.Unlock()
	h.ServeHarnessMessageCounts(ctx, "ws-1", sessions)
	assert.ElementsMatch(t, []string{"ses_empty", "ses_growing"}, *counted,
		"past the cooldown the rows re-walk — live truth keeps flowing")
}

// TestListingCount_SkipsFreshGraceAndConvergedRows — the scope fence:
// fresh spawns (inside reapGraceWindow — their 0 is legitimate, the
// #1881 belt owns them), converged rows (count>0 AND last_message_at
// set — the proxy-traversed class), and archived rows never walk.
// Companion/mutation-verify pin: remove the gate → reddens.
func TestListingCount_SkipsFreshGraceAndConvergedRows(t *testing.T) {
	h, _, counted := listingCountTestHandler(t,
		map[string]int{"ses_any": 99, "ses_conv": 99, "ses_arch": 99}, nil)
	fresh := driftRow("ses_any", 5*time.Minute)
	lastMsg := time.Now().UTC().Add(-time.Hour)
	converged := types.SessionListItem{
		ID: "ses_conv", InternalID: "ses_conv",
		MessageCount: 5, LastMessageAt: &lastMsg,
	}
	archived := driftRow("ses_arch", 2*time.Hour)
	archived.Archived = true

	h.ServeHarnessMessageCounts(context.Background(), "ws-1",
		[]types.SessionListItem{fresh, converged, archived})

	assert.Empty(t, *counted,
		"fresh-grace, converged, and archived rows are outside the serve-through scope")
}
