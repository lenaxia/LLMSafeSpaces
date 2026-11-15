// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// builtin_personas_unit_test.go — the shipped stage-persona
// vocabulary (2026-11-13 owner ask) pinned at its single source: the
// six built-ins' exact stage-intent names, their composable charters
// (shared implementer/reviewer cores + domain flavor), their clean
// render in board_get_config's provenance view, and the owner's
// EXPLICIT NON-GOAL — no stage enforcement anywhere: a reviewer-named
// role in in_progress (or an implementer-named role in in_review) is
// a legal, accepted assignment.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuiltinBoardRoles_ExactNamesAndSlugs — the stage-intent
// convention pinned VERBATIM: every name is "<seat>: <domain>", the
// in_progress seat spelled "implementer" and the in_review seat
// "reviewer", across all three domains. The names are the self-
// describing surface in the config UI and board_get_config output;
// the slugs are the stable stagePersonas reference form.
func TestBuiltinBoardRoles_ExactNamesAndSlugs(t *testing.T) {
	want := []struct{ slug, name string }{
		{"implementer-coding", "implementer: coding"},
		{"reviewer-coding", "reviewer: coding"},
		{"implementer-data-analysis", "implementer: data analysis"},
		{"reviewer-data-analysis", "reviewer: data analysis"},
		{"implementer-writing", "implementer: writing"},
		{"reviewer-writing", "reviewer: writing"},
	}
	roles := BuiltinBoardRoles()
	require.Len(t, roles, len(want))
	seen := map[string]bool{}
	for i, r := range roles {
		assert.Equal(t, want[i].slug, r.Slug)
		assert.Equal(t, want[i].name, r.Name)
		assert.NotEmpty(t, r.Description)
		require.False(t, seen[r.Slug], "slugs are unique (platform-scope seeding keys on them)")
		seen[r.Slug] = true
	}
	// The defensive copy: mutating the returned slice cannot touch the
	// single source.
	roles[0].Name = "mutated"
	assert.Equal(t, "implementer: coding", BuiltinBoardRoles()[0].Name)
}

// TestBuiltinBoardRoles_ChartersComposable — each charter is the
// shared seat core plus its domain flavor: every implementer charter
// carries the worker discipline (worktree off origin/main, Conventional
// Commits, no sleep-polling, report when blocked), every reviewer
// charter carries the verdict-channel discipline (execution evidence,
// board_record_verdict, findings with pointers, never merge), and the
// domain flavor lands ONLY where it matters (coding: tests; data:
// methodology + validity; writing: clarity/accuracy/structure).
func TestBuiltinBoardRoles_ChartersComposable(t *testing.T) {
	implementerCore := []string{
		"worktree off origin/main",
		"conventional commits",
		"No sleep-polling",
		"report it on the story instead of stalling",
	}
	reviewerCore := []string{
		"execution evidence",
		"board_record_verdict",
		"findings [{severity blocker|major|minor, pointer, criterionIndex}]",
		"Never merge, never push",
	}
	flavor := map[string][]string{
		"implementer-coding":        {"tests and lint before claiming done"},
		"reviewer-coding":           {"tests cover the change"},
		"implementer-data-analysis": {"methodology and data sources", "validate the data"},
		"reviewer-data-analysis":    {"methodology", "data's validity", "interpretation"},
		"implementer-writing":       {"Draft for the stated audience"},
		"reviewer-writing":          {"clarity, accuracy, and structure"},
	}
	for _, r := range BuiltinBoardRoles() {
		var core []string
		if strings.HasPrefix(r.Slug, "implementer") {
			core = implementerCore
		} else {
			core = reviewerCore
		}
		for _, sub := range core {
			assert.Contains(t, r.SystemPrompt, sub, "%s's charter carries the seat core", r.Slug)
		}
		for _, sub := range flavor[r.Slug] {
			assert.Contains(t, r.SystemPrompt, sub, "%s's charter carries its domain flavor", r.Slug)
		}
		assert.Less(t, len(r.SystemPrompt), 1200, "%s's charter stays short and composable", r.Slug)
	}
}

// TestBoardConfigView_RendersBuiltinRoleRefsCleanly — board_get_config
// renders the built-ins' references verbatim in the stagePersonas
// provenance view: a tier that selects them shows each stage's persona
// with its source, the bare slug untouched. The FRESH tier (nothing
// stored) is unchanged by the six: in_review still renders the default
// reviewer composition (source default_reviewer, ref "reviewer") and
// in_progress renders none — the built-ins are selectable vocabulary,
// never new defaults.
func TestBoardConfigView_RendersBuiltinRoleRefsCleanly(t *testing.T) {
	// A user tier selecting built-ins for both stages.
	sp := map[string]PersonaConfig{
		"in_progress": {RoleID: "implementer-coding"},
		"in_review":   {RoleID: "reviewer-coding"},
	}
	view := buildBoardConfigView("user", "", &BoardConfigOverride{StagePersonas: &sp}, nil)
	assert.Equal(t, ConfigSourceUserDefault, view.Sources[ConfigKeyStagePersonas])
	ip := view.Personas[StageInProgress]
	require.NotNil(t, ip.Persona)
	assert.Equal(t, PersonaSourceUserDefault, ip.Source)
	assert.Equal(t, "implementer-coding", ip.Persona.RoleID, "the built-in ref renders raw (the slug, verbatim)")
	assert.Equal(t, "implementer-coding", ip.Persona.Label())
	ir := view.Personas[StageInReview]
	require.NotNil(t, ir.Persona)
	assert.Equal(t, PersonaSourceUserDefault, ir.Source)
	assert.Equal(t, "reviewer-coding", ir.Persona.RoleID)
	// The wire form is the canonical bare string.
	b, err := json.Marshal(view.Personas[StageInProgress].Persona)
	require.NoError(t, err)
	assert.Equal(t, `"implementer-coding"`, string(b))

	// The same pair at the project tier (override precedence).
	pov := BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{
		"in_progress": {RoleID: "implementer-data-analysis"},
	}}
	view = buildBoardConfigView("project", "prj-1", &BoardConfigOverride{StagePersonas: &sp}, &pov)
	assert.Equal(t, PersonaSourceProjectOverride, view.Personas[StageInProgress].Source)
	assert.Equal(t, "implementer-data-analysis", view.Personas[StageInProgress].Persona.RoleID)
	assert.Equal(t, PersonaSourceNoneExplicit, view.Personas[StageInReview].Source,
		"the project map owns every stage it does not list — the built-in selection inherits nothing silently")

	// The FRESH tier: byte-identical default posture, the six changed
	// nothing (the default reviewer composition is not one of them).
	fresh := buildBoardConfigView("user", "", nil, nil)
	dr := fresh.Personas[StageInReview]
	assert.Equal(t, PersonaSourceDefaultReviewer, dr.Source)
	require.NotNil(t, dr.Persona)
	assert.Equal(t, DefaultReviewerRoleSlug, dr.Persona.RoleID)
	assert.Equal(t, PersonaSourceNone, fresh.Personas[StageInProgress].Source)
}

// TestNoStageEnforcement_AnyRoleInAnyStageAccepted — the owner's
// EXPLICIT NON-GOAL pinned: NO stage-enforcement checks exist. The
// built-ins' names convey stage intent ONLY — a reviewer-named role
// assigned to in_progress (and an implementer-named role assigned to
// in_review) decodes, validates, and would write without a whisper of
// stage-mismatch error.
func TestNoStageEnforcement_AnyRoleInAnyStageAccepted(t *testing.T) {
	// The decode arm: the stage key must be persona-eligible, but the
	// ROLE reference is never stage-checked — cross-seat assignments
	// decode fine.
	raw := []byte(`{"in_progress":"reviewer-coding","in_review":"implementer-coding"}`)
	m, err := decodePersonaMap(raw)
	require.NoError(t, err, "a reviewer-named role in in_progress decodes — the naming conveys intent, nothing enforces it")
	assert.Equal(t, PersonaConfig{RoleID: "reviewer-coding"}, m[StageInProgress])
	assert.Equal(t, PersonaConfig{RoleID: "implementer-coding"}, m[StageInReview])

	// The set-path belt: validatePersonaRoleRefs checks ONLY that each
	// reference resolves against the vocabulary — the (stage, role)
	// pairing is never validated. A vocabulary carrying all six
	// accepts the fully cross-seat map.
	vocab := []AgentRoleRef{{ID: "99999999-9999-9999-9999-999999999991", Slug: "reviewer", Name: "Reviewer", Scope: "platform"}}
	for _, def := range BuiltinBoardRoles() {
		vocab = append(vocab, AgentRoleRef{ID: "99999999-9999-9999-9999-9999999999" + def.Slug[:2], Slug: def.Slug, Name: def.Name, Scope: "platform"})
	}
	s := &Service{roles: dirFunc(func() ([]AgentRoleRef, error) { return vocab, nil })}
	cross := &BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{
		StageInProgress: {RoleID: "reviewer-writing"},    // reviewer seat in the worker stage
		StageInReview:   {RoleID: "implementer-writing"}, // implementer seat in the review stage
	}}
	err = s.validatePersonaRoleRefs(context.Background(), "user-1", cross)
	assert.NoError(t, err, "the cross-seat assignment is accepted — no stage enforcement, by owner ruling")

	// And the standard pairing validates identically (the intended use
	// is not specially treated either).
	standard := &BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{
		StageInProgress: {RoleID: "implementer-coding"},
		StageInReview:   {RoleID: "reviewer-coding"},
	}}
	assert.NoError(t, s.validatePersonaRoleRefs(context.Background(), "user-1", standard))
}

// dirFunc adapts a closure to the RoleDirectory seam for the pure
// validation belt (no DB, no store).
type dirFunc func() ([]AgentRoleRef, error)

func (f dirFunc) AgentRolesForUser(_ context.Context, _ string) ([]AgentRoleRef, error) {
	return f()
}
