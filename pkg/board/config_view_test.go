// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// config_view_test.go — the source-annotated view (board_get_config's
// shape) pinned PURE: per-key sources follow the merge precedence
// (project override > user default > code default), per-stage persona
// provenance mirrors personaForStage's spawn-time resolution, and the
// default-reviewer arm renders the persona an inheriting project's
// in_review actually runs.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func TestBuildBoardConfigView_UserTier(t *testing.T) {
	view := buildBoardConfigView("user", "", nil, nil)
	require.Equal(t, "user", view.Scope)
	assert.Empty(t, view.ProjectID)
	// Nothing stored anywhere: every key renders its code-default
	// source, and the effective row IS the code defaults.
	for _, key := range BoardConfigKeys() {
		assert.Equal(t, ConfigSourceCodeDefault, view.Sources[key], key)
	}
	assert.Equal(t, DefaultBoardConfig(), view.Effective)
	// The legacy composition is live (auto + autoAssignReviewer, no
	// explicit map): in_review renders the default reviewer persona,
	// in_progress renders none.
	require.Equal(t, PersonaSourceDefaultReviewer, view.Personas[StageInReview].Source)
	require.NotNil(t, view.Personas[StageInReview].Persona)
	assert.Equal(t, DefaultReviewerPersona, *view.Personas[StageInReview].Persona)
	assert.Equal(t, PersonaSourceNone, view.Personas[StageInProgress].Source)
	assert.Nil(t, view.Personas[StageInProgress].Persona)
}

func TestBuildBoardConfigView_UserTierStoredMap(t *testing.T) {
	u := &BoardConfigOverride{
		ReviewColumn:       strp(ReviewColumnManual),
		AutoAssignReviewer: boolp(false),
		StagePersonas:      &map[string]PersonaConfig{StageInReview: {Name: "auditor", Instructions: "Verify."}},
	}
	view := buildBoardConfigView("user", "", u, nil)
	assert.Equal(t, ConfigSourceUserDefault, view.Sources[ConfigKeyReviewColumn])
	assert.Equal(t, ConfigSourceUserDefault, view.Sources[ConfigKeyAutoAssignReviewer])
	assert.Equal(t, ConfigSourceCodeDefault, view.Sources[ConfigKeyDefaultWorkerSlots], "never stored → code default")
	assert.Equal(t, ConfigSourceUserDefault, view.Sources[ConfigKeyStagePersonas])
	// The explicit map owns EVERY stage: in_review renders the stored
	// persona, in_progress renders none_explicit (visible, not silent).
	assert.Equal(t, PersonaSourceUserDefault, view.Personas[StageInReview].Source)
	require.NotNil(t, view.Personas[StageInReview].Persona)
	assert.Equal(t, "auditor", view.Personas[StageInReview].Persona.Name)
	assert.Equal(t, PersonaSourceNoneExplicit, view.Personas[StageInProgress].Source)
	assert.Nil(t, view.Personas[StageInProgress].Persona)
}

func TestBuildBoardConfigView_ProjectTierPrecedence(t *testing.T) {
	u := &BoardConfigOverride{
		ReviewColumn:       strp(ReviewColumnManual),
		AutoAssignReviewer: boolp(false),
		StagePersonas:      &map[string]PersonaConfig{StageInReview: {Name: "auditor"}},
	}
	p := &BoardConfigOverride{
		ReviewColumn: strp(ReviewColumnOff),
		StagePersonas: &map[string]PersonaConfig{
			StageInProgress: {Name: "builder"},
		},
	}
	view := buildBoardConfigView("project", "proj-1", u, p)
	require.Equal(t, "project", view.Scope)
	assert.Equal(t, "proj-1", view.ProjectID)
	// Per-key precedence: the project's reviewColumn wins, the user's
	// autoAssignReviewer (unset at the project) inherits, slots stay
	// code-default.
	assert.Equal(t, ConfigSourceProjectOverride, view.Sources[ConfigKeyReviewColumn])
	assert.Equal(t, ReviewColumnOff, view.Effective.ReviewColumn)
	assert.Equal(t, ConfigSourceUserDefault, view.Sources[ConfigKeyAutoAssignReviewer])
	assert.False(t, view.Effective.AutoAssignReviewer)
	assert.Equal(t, ConfigSourceCodeDefault, view.Sources[ConfigKeyDefaultWorkerSlots])
	// Personas: the project's explicit map owns every stage — its
	// in_progress renders, and its MISSING in_review renders
	// none_explicit even though the user tier lists one (the
	// wholesale-replace semantics made visible).
	assert.Equal(t, PersonaSourceProjectOverride, view.Personas[StageInProgress].Source)
	require.NotNil(t, view.Personas[StageInProgress].Persona)
	assert.Equal(t, "builder", view.Personas[StageInProgress].Persona.Name)
	assert.Equal(t, PersonaSourceNoneExplicit, view.Personas[StageInReview].Source)
	assert.Nil(t, view.Personas[StageInReview].Persona,
		"the project override map replaces the user map wholesale — the user-tier auditor never leaks through")
}

func TestBuildBoardConfigView_EmptyProjectOptOut(t *testing.T) {
	u := &BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{StageInReview: {Name: "auditor"}}}
	p := &BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{}}
	view := buildBoardConfigView("project", "proj-1", u, p)
	assert.Equal(t, ConfigSourceProjectOverride, view.Sources[ConfigKeyStagePersonas], "{} is an EXPLICIT override")
	// {} suppresses even the inherited persona: none_explicit, never a
	// fallthrough to the user tier.
	assert.Equal(t, PersonaSourceNoneExplicit, view.Personas[StageInReview].Source)
	assert.Nil(t, view.Personas[StageInReview].Persona)
}

func TestBuildBoardConfigView_CompositionDeadUnderUserMap(t *testing.T) {
	// The toggles are ON (code defaults) but a user-tier explicit map
	// exists without in_review: the composition arm is dead — the map
	// owns the stages (the none_explicit semantics, one tier up).
	u := &BoardConfigOverride{StagePersonas: &map[string]PersonaConfig{StageInProgress: {Name: "builder"}}}
	view := buildBoardConfigView("project", "proj-1", u, nil)
	assert.Equal(t, PersonaSourceNoneExplicit, view.Personas[StageInReview].Source)
	assert.Nil(t, view.Personas[StageInReview].Persona)
}
