// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// #1850: the reap guard's spawn-window grace. DefaultReapGuard
// admitted EVERY message_count==0 row — but a freshly created
// create_session worker keeps count 0 through its whole first turn
// (the prompt delivery is a pod-local POST the API never sees), so
// two harness-list misses inside that window reaped the LIVE row and
// orphaned the phrase minted at creation (the re-phrase defect). The
// guard must refuse rows younger than the grace window; aged rows and
// rows without creation-time information keep the pre-#1850 verdict.
//
// Known effect (accepted, documented in the #1850 PR): the grace also
// shelters genuinely stillborn rows (probe shells, spawn crashes) for
// the window's duration — they converge one grace period later, which
// the #1340 reaper's ghost-hygiene purpose tolerates.

func TestDefaultReapGuard_GraceWindow(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-30 * time.Second)
	aged := now.Add(-reapGraceWindow).Add(-time.Minute)

	cases := []struct {
		name    string
		row     types.SessionListItem
		wantAdm bool
	}{
		{
			name:    "fresh count-zero row is sheltered (spawn window)",
			row:     types.SessionListItem{InternalID: "ses_fresh", MessageCount: 0, CreatedAt: &fresh},
			wantAdm: false,
		},
		{
			name:    "aged count-zero row is admitted (ghost hygiene)",
			row:     types.SessionListItem{InternalID: "ses_ghost", MessageCount: 0, CreatedAt: &aged},
			wantAdm: true,
		},
		{
			name:    "fresh history-bearing row still refused (count guard)",
			row:     types.SessionListItem{InternalID: "ses_live", MessageCount: 4, CreatedAt: &fresh},
			wantAdm: false,
		},
		{
			name:    "nil CreatedAt keeps the pre-#1850 verdict (hand-built rows)",
			row:     types.SessionListItem{InternalID: "ses_legacy", MessageCount: 0},
			wantAdm: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantAdm, DefaultReapGuard(tc.row))
		})
	}
}

// TestPlanReconciliation_FreshRowSurvivesTwoMisses is the #1850
// scenario at the plan tier: the exact reaper pass that orphaned the
// five reproduced workers — count-zero row, harness list missing it
// twice (threshold 2) — must keep the row while it is inside the
// grace window, and reap it once aged.
func TestPlanReconciliation_FreshRowSurvivesTwoMisses(t *testing.T) {
	now := time.Now()
	freshAt := now.Add(-time.Minute)
	agedAt := now.Add(-reapGraceWindow).Add(-2*time.Minute - time.Minute)

	lister := func() (map[string]bool, error) { return map[string]bool{}, nil }

	// Fresh row: two consecutive misses at threshold 2 — the plan
	// would have removed it pre-#1850; the guard now shelters it. The
	// row lands in keptForOperator (the orchestration layer pins kept
	// rows' counters at threshold — r2 finding — so the operator
	// signal repeats every pass until the row ages past the grace).
	fresh := []types.SessionListItem{{InternalID: "ses_worker", ID: "amber-trout-glides", MessageCount: 0, CreatedAt: &freshAt}}
	remove, kept, miss := PlanReconciliation(fresh, lister, map[string]int{"ses_worker": 1}, 2, DefaultReapGuard)
	assert.Empty(t, remove, "a fresh spawn-window row must never be reaped")
	assert.Len(t, kept, 1, "the sheltered row is the kept-for-operator signal")
	assert.NotContains(t, miss, "ses_worker", "the plan carries no counter for kept rows (the handler pins it)")

	// Aged row: the same plan reaps it (ghost hygiene converges, one
	// grace period later).
	aged := []types.SessionListItem{{InternalID: "ses_ghost", ID: "fallow-mist-rises", MessageCount: 0, CreatedAt: &agedAt}}
	remove, kept, _ = PlanReconciliation(aged, lister, map[string]int{"ses_ghost": 1}, 2, DefaultReapGuard)
	assert.Equal(t, []string{"ses_ghost"}, remove)
	assert.Empty(t, kept)
}
