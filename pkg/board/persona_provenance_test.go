// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// persona_provenance_test.go — pins PersonaProvenanceForStage (the
// display twin of personaForStage's spawn-time resolution: same
// precedence, named sources for the board_get_config annotations) and
// the frontend's DefaultReviewerPersona mirror (the BoardConfigPanels
// one-click import must carry the SAME charter preamble the engine
// composes — a drifted copy would silently hand the UX a different
// reviewer than the board spawns).

func personaMap(m map[string]PersonaConfig) *map[string]PersonaConfig { return &m }

// TestPersonaProvenanceForStage pins the precedence: project explicit
// map owns every stage; else user explicit map owns every stage; else
// the legacy composition renders the default reviewer for in_review;
// else none. The explicit-key-owns-all-stages semantics renders as
// none_explicit — visible, never silent.
func TestPersonaProvenanceForStage(t *testing.T) {
	eff := DefaultBoardConfig() // auto + autoAssignReviewer: composition armed
	user := &BoardConfigOverride{}
	proj := &BoardConfigOverride{}

	cases := []struct {
		name    string
		user    *BoardConfigOverride
		project *BoardConfigOverride
		eff     BoardConfig
		stage   string
		want    string
	}{
		{
			name: "no maps, composition armed: in_review runs the default reviewer",
			user: user, project: proj, eff: eff, stage: StageInReview,
			want: PersonaSourceDefaultReviewer,
		},
		{
			name: "no maps, composition armed: in_progress has none",
			user: user, project: proj, eff: eff, stage: StageInProgress,
			want: PersonaSourceNone,
		},
		{
			name: "no maps, composition disarmed (autoAssignReviewer off): none",
			user: user, project: proj,
			eff: BoardConfig{ReviewColumn: ReviewColumnAuto, AutoAssignReviewer: false}, stage: StageInReview,
			want: PersonaSourceNone,
		},
		{
			name: "project map lists the stage: project_override",
			user: user,
			project: &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
				StageInReview: {RoleID: "auditor"},
			})},
			eff: eff, stage: StageInReview,
			want: PersonaSourceProjectOverride,
		},
		{
			name: "project map owns the stages it does not list: none_explicit",
			user: &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
				StageInReview: {RoleID: "auditor"},
			})},
			project: &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
				StageInProgress: {RoleID: "builder"},
			})},
			eff: eff, stage: StageInReview,
			want: PersonaSourceNoneExplicit,
		},
		{
			name: "user map listed stage (no project map): user_default",
			user: &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
				StageInReview: {RoleID: "auditor"},
			})},
			project: proj, eff: eff, stage: StageInReview,
			want: PersonaSourceUserDefault,
		},
		{
			name: "user map owns the stages it does not list: none_explicit (composition dead)",
			user: &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
				StageInProgress: {RoleID: "builder"},
			})},
			project: proj, eff: eff, stage: StageInReview,
			want: PersonaSourceNoneExplicit,
		},
		{
			name: "nil maps: the composition arm answers",
			user: nil, project: nil, eff: eff, stage: StageInReview,
			want: PersonaSourceDefaultReviewer,
		},
		{
			name: "non-eligible stage: none",
			user: user, project: proj, eff: eff, stage: StageBacklog,
			want: PersonaSourceNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PersonaProvenanceForStage(tc.user, tc.project, tc.eff, tc.stage))
		})
	}
}

// TestPersonaProvenanceForStage_AgreesWithPersonaForStage: the display
// twin never disagrees with the spawn-time resolution — a stage the
// provenance says carries a persona must resolve one, and none/none_explicit
// must resolve nil.
func TestPersonaProvenanceForStage_AgreesWithPersonaForStage(t *testing.T) {
	user := &BoardConfigOverride{StagePersonas: personaMap(map[string]PersonaConfig{
		StageInReview: {RoleID: "auditor"},
	})}
	proj := &BoardConfigOverride{}
	effUser := MergeBoardConfig(DefaultBoardConfig(), user)
	for _, stage := range []string{StageInProgress, StageInReview} {
		src := PersonaProvenanceForStage(user, proj, effUser, stage)
		p := personaForStage(effUser, stage)
		// p.Name must not ride the format args raw: args evaluate
		// eagerly, and a nil p would panic before assert.Nil reports.
		found := "nil"
		if p != nil {
			found = p.RoleID
		}
		switch src {
		case PersonaSourceProjectOverride, PersonaSourceUserDefault, PersonaSourceDefaultReviewer:
			require.NotNil(t, p, "%s: provenance %q implies a persona, resolution says none", stage, src)
		case PersonaSourceNoneExplicit, PersonaSourceNone:
			assert.Nil(t, p, "%s: provenance %q implies no persona, resolution found %q", stage, src, found)
		}
	}
}
