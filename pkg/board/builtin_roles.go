// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// builtin_roles.go — the platform's SHIPPED stage-persona vocabulary
// (2026-11-13 owner ask): boards reference agent roles from
// stagePersonas (the role-only model, owner ruling 2026-10-29), and a
// board author previously had to author custom roles before the board
// was useful. These six built-ins — an implementer/reviewer pair per
// domain (coding, data analysis, writing) — give every board a
// selectable floor. The app layer SEEDS them as platform-scope agent
// roles at startup (seedBuiltinBoardRoles, the same mechanism as the
// built-in reviewer); pkg/board owns the definitions as the single
// source, exactly like DefaultReviewerRoleSystemPrompt.
//
// NAMING (the stage-intent convention, owner's exact shape): every
// name is "<stage-seat>: <domain>" — "implementer: coding" is meant
// for in_progress (the worker), "reviewer: coding" for in_review.
// The names are the ONLY stage signal, deliberately: the owner's
// explicit non-goal ruling is NO stage-enforcement checks — any role
// may sit in any stage (a reviewer-named role in in_progress is a
// legal, unremarkable assignment; the set path validates the
// reference RESOLVES, never which stage it lands in).
//
// Charters are short and composable: one shared implementer core (do
// the work; worktree off origin/main; conventional commits; no
// sleep-polling; report when blocked) and one shared reviewer core
// (verify each acceptance criterion with execution evidence; verdict
// via board_record_verdict; findings with pointers; no drive-by
// merging), with domain flavor only where it matters.

import "strings"

// BuiltinBoardRole is one shipped role definition: the identity the
// seeded agent role carries plus the charter (its system prompt).
type BuiltinBoardRole struct {
	// Slug is the seeded role's slug — the canonical stagePersonas
	// reference form (bare, short, stable).
	Slug string
	// Name is the seeded role's display name — the stage-intent
	// convention "implementer: <domain>" / "reviewer: <domain>".
	Name string
	// Description is the seeded role's one-line description (what a
	// picker renders beside the name; marks the role as built-in).
	Description string
	// SystemPrompt is the role's charter — the system prompt the
	// spawned worker runs on.
	SystemPrompt string
}

// The implementer charter core: stage-appropriate worker discipline
// shared by all three domains' in_progress seats. Kept prose-light —
// the domain flavor constants append one or two sentences each.
const builtinImplementerCore = "You are the implementer for this story: do the work its charter asks, fully and honestly. " +
	"Work from a worktree off origin/main; use conventional commits. " +
	"No sleep-polling: run commands to completion or hand the work off — never idle-loop waiting. " +
	"If you are blocked or the ask is ambiguous, report it on the story instead of stalling. " +
	"When the work is done, move the story to in_review with evidence (PR, build, or doc link)."

// The reviewer charter core: the verdict-channel discipline shared by
// all three domains' in_review seats (mirrors the built-in reviewer's
// charter, the DefaultReviewerRoleSystemPrompt shape).
const builtinReviewerCore = "Verify EACH acceptance criterion against execution evidence — run the checks, read the artifacts, never trust the worker's summary alone. " +
	"Record ONE verdict via board_record_verdict: approve (requires the story's evidence URL and ALL criteria met — the Done guard applies) " +
	"or fail with findings [{severity blocker|major|minor, pointer, criterionIndex}] — every finding carries a pointer. " +
	"Never merge, never push. No drive-by fixes: bounce with findings instead."

// Domain flavor: appended to the matching core. One or two sentences
// where the domain actually changes the work.
const (
	builtinCodingImplementerFlavor  = " Run the available tests and lint before claiming done, and pin the behavior you touched with new or updated tests where the suite supports it."
	builtinCodingReviewerFlavor     = " Check that tests cover the change: new behavior pinned, regressions caught, and the suite green on the exact head."
	builtinDataImplementerFlavor    = " State your methodology and data sources up front; validate the data (provenance, coverage, known gaps) before interpreting it, and show the numbers you ran."
	builtinDataReviewerFlavor       = " Audit the methodology, the data's validity, and the interpretation: does the evidence actually support the conclusion, or merely fail to contradict it?"
	builtinWritingImplementerFlavor = " Draft for the stated audience; keep claims accurate and attributed; match the surrounding document's structure and voice."
	builtinWritingReviewerFlavor    = " Edit for clarity, accuracy, and structure: would the intended reader understand it, is every claim true, does it hold together."
)

// builtinBoardRoles is the shipped vocabulary, deterministic order
// (coding, data analysis, writing; implementer before reviewer per
// pair). The exact names are PINNED by test — they are the config
// UI's self-describing stage-intent surface.
var builtinBoardRoles = []BuiltinBoardRole{
	{
		Slug:         "implementer-coding",
		Name:         "implementer: coding",
		Description:  "Built-in board implementer for coding stories (in_progress seat): worktree off origin/main, conventional commits, tests before done.",
		SystemPrompt: builtinImplementerCore + builtinCodingImplementerFlavor,
	},
	{
		Slug:         "reviewer-coding",
		Name:         "reviewer: coding",
		Description:  "Built-in board code reviewer (in_review seat): criteria walk with execution evidence, tests/regression checks, verdict via board_record_verdict.",
		SystemPrompt: builtinReviewerCore + builtinCodingReviewerFlavor,
	},
	{
		Slug:         "implementer-data-analysis",
		Name:         "implementer: data analysis",
		Description:  "Built-in board analyst (in_progress seat): methodology and data sources stated, data validated before interpretation.",
		SystemPrompt: builtinImplementerCore + builtinDataImplementerFlavor,
	},
	{
		Slug:         "reviewer-data-analysis",
		Name:         "reviewer: data analysis",
		Description:  "Built-in board analytical QA (in_review seat): methodology, data validity, and interpretation audited against execution evidence.",
		SystemPrompt: builtinReviewerCore + builtinDataReviewerFlavor,
	},
	{
		Slug:         "implementer-writing",
		Name:         "implementer: writing",
		Description:  "Built-in board writer (in_progress seat): drafted for the stated audience, accurate and attributed.",
		SystemPrompt: builtinImplementerCore + builtinWritingImplementerFlavor,
	},
	{
		Slug:         "reviewer-writing",
		Name:         "reviewer: writing",
		Description:  "Built-in board editor (in_review seat): clarity, accuracy, and structure — verdict via board_record_verdict.",
		SystemPrompt: builtinReviewerCore + builtinWritingReviewerFlavor,
	},
}

// BuiltinBoardRoles returns the shipped stage-persona vocabulary
// (defensive copy — callers must not mutate the single source).
func BuiltinBoardRoles() []BuiltinBoardRole {
	out := make([]BuiltinBoardRole, len(builtinBoardRoles))
	copy(out, builtinBoardRoles)
	return out
}

// BuiltinBoardRoleBySlug resolves one shipped role by its slug (nil
// when the slug names no built-in) — the seed's lookup arm.
func BuiltinBoardRoleBySlug(slug string) *BuiltinBoardRole {
	for i := range builtinBoardRoles {
		if builtinBoardRoles[i].Slug == slug {
			r := builtinBoardRoles[i]
			return &r
		}
	}
	return nil
}

// BuiltinBoardRoleSlugs renders the shipped vocabulary's slugs (the
// "which built-ins exist" line for logs and error detail).
func BuiltinBoardRoleSlugs() string {
	slugs := make([]string, len(builtinBoardRoles))
	for i := range builtinBoardRoles {
		slugs[i] = builtinBoardRoles[i].Slug
	}
	return strings.Join(slugs, ", ")
}
