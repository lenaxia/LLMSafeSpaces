// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrapupCharterGoldenInput is the fixed fixture for the golden pin: values
// chosen so every substituted slot renders distinctly.
func wrapupCharterGoldenInput() WrapUpCharterInput {
	from := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 16, 12, 30, 5, 0, time.UTC)
	doneAt := time.Date(2026, 11, 16, 8, 15, 0, 0, time.UTC)
	return WrapUpCharterInput{
		Epic: &Epic{
			ID:          "b1b1b1b1-1111-4111-8111-b1b1b1b1b1b1",
			ShortID:     "epc-golden01",
			Title:       "Ship the golden epic",
			Outcome:     "The bar this epic exists to achieve — verbatim, multi-word.",
			ProjectName: "golden-project",
			CreatedAt:   from,
		},
		WindowFrom: from,
		WindowTo:   to,
		Stories: []WrapUpStoryRef{
			{ShortID: "tsk-golden01", Title: "First story", DoneAt: doneAt, Evidence: "https://ev.test/1"},
			{ShortID: "tsk-golden02", Title: "Second story", DoneAt: doneAt.Add(time.Hour), Evidence: ""},
		},
		WrapUpCount:    1, // second consecutive close
		ConsecutiveCap: 3,
	}
}

// TestRenderWrapUpCharterGolden pins the charter's EXACT block structure
// (§5): header, epic identity, outcome verbatim, the RFC 3339 coverage
// window pair, the story list with the evidence chain's rendered fallback,
// the aggregate-diff instruction (the reviewer computes the diff — the
// engine never runs git), THE REVIEW procedure (lenses-not-checklists,
// verdict-verb-only close, gap-first-dedupe, #1946 setCriteriaMet, the cap
// awareness line), and the lease-nudge trailer.
func TestRenderWrapUpCharterGolden(t *testing.T) {
	got := RenderWrapUpCharter(wrapupCharterGoldenInput())
	want := `Epic wrap-up review — holistic, delta-scoped, against the epic's outcome.

Epic: epc-golden01 "Ship the golden epic" (project golden-project)
Outcome (the bar this epic exists to achieve):
  The bar this epic exists to achieve — verbatim, multi-word.

COVERAGE WINDOW (UTC, both ends inclusive-bound):
  from 2026-11-15T09:00:00Z
  to   2026-11-16T12:30:05Z
Stories in this window (2):
  - tsk-golden01 "First story" — done 2026-11-16T08:15:00Z; evidence: https://ev.test/1
  - tsk-golden02 "Second story" — done 2026-11-16T09:15:00Z; evidence: (none recorded)
Aggregate diff: in the project's target workspace run, after fetching origin,
  git log --no-merges --since='2026-11-15T09:00:00Z' --until='2026-11-16T12:30:05Z' origin/main
  (plus each story's PR head where its evidence URL points at an unmerged PR).
  The window bounds the review; the story list is the union to read.

THE REVIEW (engine-owned procedure — the role owns judgment, never the reverse):
1. Read the aggregate diff as ONE change against the outcome above — coherence
   across stories, integration seams between them, dead/orphaned code,
   built-but-never-wired features.
2. LENSES, not checklists: evidence verification (PRs merged, issues closed)
   and wiring (registered/mounted/reachable; flag states are decisions, not
   defaults) run INSIDE the holistic read.
3. Close ONLY via board_record_wrapup_verdict (claim token from your envelope):
   - pass  {verdict:"pass",  reportUrl:"<the wrap-up report>"}   — the epic completes.
   - gaps  {verdict:"gaps",  reportUrl:"<report>", gapRefs:["tsk-…",…]} — the epic
     stays open. File gap stories FIRST via board_create_story (tag "gap"; DEDUPE
     against existing rows — board_list the epic before creating; a gap is a real
     story with real acceptance criteria, never a note). Then record the verdict
     referencing them.
4. Check off this row's criteria with board_update_story {setCriteriaMet} as you
   verify them (#1946: your own claimed row); the verdict requires ALL met +
   reportUrl (the Done guard applies).
5. Recursion awareness: this is consecutive wrap-up 2 of 3 for this epic; at the cap a human ack is required before another mints.

If a BOARD LEASE NUDGE arrives, call board_heartbeat ONCE immediately, then continue.
The claim token in your envelope authorizes in-flight writes for this task only.
`
	require.Equal(t, want, got, "the §5 charter block, byte-exact")
}

// TestRenderWrapUpCharterDeterministic — [2]: the renderer is a PURE
// function of (epic, window, stories, cap state): identical inputs render
// byte-identical charters (a re-claimed reviewer reads the same window —
// convergence, not drift), and distinct inputs never collide.
func TestRenderWrapUpCharterDeterministic(t *testing.T) {
	in := wrapupCharterGoldenInput()
	assert.Equal(t, RenderWrapUpCharter(in), RenderWrapUpCharter(in),
		"same input → byte-identical charter")

	// Mutations every field → distinct bytes (no hidden nondeterminism
	// papering over a stale slot).
	assert.NotEqual(t, RenderWrapUpCharter(in), RenderWrapUpCharter(wrapupCharterGoldenInputWith(func(m *WrapUpCharterInput) {
		m.WindowTo = m.WindowTo.Add(time.Minute)
	})))
	assert.NotEqual(t, RenderWrapUpCharter(in), RenderWrapUpCharter(wrapupCharterGoldenInputWith(func(m *WrapUpCharterInput) {
		m.Stories = m.Stories[:1]
	})))
	assert.NotEqual(t, RenderWrapUpCharter(in), RenderWrapUpCharter(wrapupCharterGoldenInputWith(func(m *WrapUpCharterInput) {
		m.WrapUpCount = 0
	})))

	// The empty window: zero stories renders an explicit (none) — the
	// charter stays well-formed for an empty delta.
	empty := wrapupCharterGoldenInputWith(func(m *WrapUpCharterInput) { m.Stories = nil })
	assert.Contains(t, RenderWrapUpCharter(empty), "Stories in this window (0):\n  (none)")
}

func wrapupCharterGoldenInputWith(mutate func(*WrapUpCharterInput)) WrapUpCharterInput {
	in := wrapupCharterGoldenInput()
	mutate(&in)
	return in
}

// TestWrapUpCriteriaSeedPins — the engine-seeded acceptance criteria are
// EXACTLY §5's three (reviewer-checkable via board_update_story
// {setCriteriaMet}; the verdict's Done-guard parity rides them).
func TestWrapUpCriteriaSeedPins(t *testing.T) {
	require.Equal(t, []AcceptanceCriterion{
		{Text: "The aggregate diff for the coverage window was read as a whole, against the epic's outcome, with the evidence and wiring lenses applied"},
		{Text: "A wrap-up report exists at a reachable URL and is referenced by reportUrl in the recorded verdict"},
		{Text: "The verdict was recorded via board_record_wrapup_verdict; on gaps, every gapRef resolves to a deduped gap-tagged story in this epic"},
	}, wrapUpCriteria())
}
