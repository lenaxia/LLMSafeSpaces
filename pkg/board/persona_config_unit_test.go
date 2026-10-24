// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// persona_config_unit_test.go — the stagePersonas config vocabulary's
// unit belts (RED-first for the generalized stage-persona
// claim-then-spawn mechanism, owner directive 2026-10-24).

func mustNotErr(t *testing.T, key string, raw string) {
	t.Helper()
	if err := ValidateBoardConfigKey(key, json.RawMessage(raw)); err != nil {
		t.Fatalf("ValidateBoardConfigKey(%s, %s) unexpectedly rejected: %v", key, raw, err)
	}
}

func mustReject(t *testing.T, key string, raw string, wantSub string) {
	t.Helper()
	err := ValidateBoardConfigKey(key, json.RawMessage(raw))
	if err == nil {
		t.Fatalf("ValidateBoardConfigKey(%s, %s) unexpectedly accepted", key, raw)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("rejection %v does not name %q", err, wantSub)
	}
}

// TestStagePersonasValidation — the persona map's validation matrix:
// eligible stages + bounded name/instructions accepted; ineligible
// stages, nulls, empties, oversize, and unknown nested fields reject.
func TestStagePersonasValidation(t *testing.T) {
	// Accepted shapes.
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"name":"reviewer"}}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_progress":{"name":"implementer","instructions":"Ship it."}}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{}`) // the explicit opt-out override
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"name":"`+strings.Repeat("a", PersonaNameMax)+`"}}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"name":"r","instructions":"`+strings.Repeat("a", PersonaInstructionsMax)+`"}}`)

	// Rejected shapes — ineligible stages (no live claim can exist).
	for _, stage := range []string{"backlog", "done", "pending_input", "some_arbitrary_stage"} {
		mustReject(t, ConfigKeyStagePersonas,
			`{"`+stage+`":{"name":"p"}}`, "cannot hold a persona")
	}
	// Nulls (the never-a-value rule), empties, bounds, unknown fields.
	mustReject(t, ConfigKeyStagePersonas, `null`, "must not be null")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":null}`, "must not map to null")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":""}}`, "non-empty name")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"instructions":"no name"}}`, "non-empty name")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"`+strings.Repeat("a", PersonaNameMax+1)+`"}}`, "name exceeds")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"r","instructions":"`+strings.Repeat("a", PersonaInstructionsMax+1)+`"}}`, "instructions exceed")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"r","promt":"typo"}}`, "unknown field")
	mustReject(t, ConfigKeyStagePersonas, `["in_review"]`, "must be an object")
	mustReject(t, ConfigKeyStagePersonas, `"nope"`, "must be an object")
}

// TestStagePersonasDecodeRoundTrip — storage rows decode back to the
// typed partial override (set keys only; absent = inherit).
func TestStagePersonasDecodeRoundTrip(t *testing.T) {
	in := map[string]PersonaConfig{
		"in_review":   {Name: "auditor", Instructions: "Verify adversarially."},
		"in_progress": {Name: "implementer"},
	}
	o := &BoardConfigOverride{StagePersonas: &in}
	rows := o.rows()
	found := false
	for _, r := range rows {
		if r.Key == ConfigKeyStagePersonas {
			found = true
			back, err := DecodeBoardConfigOverride(map[string]json.RawMessage{r.Key: r.Value})
			require.NoError(t, err)
			require.NotNil(t, back.StagePersonas)
			assert.Equal(t, in, *back.StagePersonas)
		}
	}
	assert.True(t, found, "stagePersonas encodes to a config row")
	// Absent key: nothing stored, override nil (inheritance).
	o2, err := DecodeBoardConfigOverride(map[string]json.RawMessage{})
	require.NoError(t, err)
	assert.Nil(t, o2.StagePersonas)
}

// TestStagePersonasMergeSemantics — per-KEY merge: a set map replaces
// wholesale (the explicit {} is the opt-out override), an absent key
// inherits the base map.
func TestStagePersonasMergeSemantics(t *testing.T) {
	base := DefaultBoardConfig()
	base.StagePersonas = map[string]PersonaConfig{
		"in_review": {Name: "inherited-reviewer"},
	}

	// Absent key inherits.
	got := MergeBoardConfig(base, &BoardConfigOverride{})
	assert.Equal(t, base.StagePersonas, got.StagePersonas)

	// Set key replaces wholesale — a partial map does NOT per-stage
	// merge (per-KEY semantics).
	repl := map[string]PersonaConfig{"in_progress": {Name: "implementer"}}
	got = MergeBoardConfig(base, &BoardConfigOverride{StagePersonas: &repl})
	assert.Equal(t, repl, got.StagePersonas)

	// The explicit empty object is the opt-out (no personas) — and the
	// merge carries the explicit-set flag (the legacy-composition
	// suppressor) through every tier.
	empty := map[string]PersonaConfig{}
	got = MergeBoardConfig(base, &BoardConfigOverride{StagePersonas: &empty})
	assert.Empty(t, got.StagePersonas)
	assert.True(t, got.StagePersonasSet, "the {} opt-out is an EXPLICIT set — it owns the stage mapping")

	// Absent key: neither the map nor the flag inherit a set state.
	got = MergeBoardConfig(base, &BoardConfigOverride{ReviewColumn: strPtr("auto")})
	assert.False(t, got.StagePersonasSet)
}

// TestPersonaForStageResolution — the effective persona resolution:
// an explicit stagePersonas entry wins on ANY eligible stage; the
// legacy auto-review composition renders the default reviewer persona
// for in_review (existing stored configs keep their meaning —
// auto-review on IS in_review having a reviewer persona via the
// generic config); everything else is nil (today's behavior).
func TestPersonaForStageResolution(t *testing.T) {
	// Explicit entry — arbitrary stage, arbitrary persona (the flag is
	// what the MERGE sets; direct construction models the same shape).
	eff := DefaultBoardConfig()
	eff.StagePersonas = map[string]PersonaConfig{"in_review": {Name: "auditor"}}
	eff.StagePersonasSet = true
	if p := personaForStage(eff, StageInReview); p == nil || p.Name != "auditor" {
		t.Fatalf("explicit in_review persona must win, got %+v", p)
	}
	eff.StagePersonas = map[string]PersonaConfig{"in_progress": {Name: "implementer"}}
	if p := personaForStage(eff, StageInProgress); p == nil || p.Name != "implementer" {
		t.Fatalf("explicit in_progress persona must win, got %+v", p)
	}

	// Legacy composition: auto + true → the default reviewer persona.
	eff = DefaultBoardConfig()
	eff.ReviewColumn = ReviewColumnAuto
	eff.AutoAssignReviewer = true
	if p := personaForStage(eff, StageInReview); p == nil || p.Name != DefaultReviewerPersona.Name {
		t.Fatalf("auto+true must render the default reviewer persona, got %+v", p)
	}

	// The legacy gate's other arms and the shipped defaults → nil.
	for _, tc := range []BoardConfig{
		{ReviewColumn: ReviewColumnAuto, AutoAssignReviewer: false},
		{ReviewColumn: ReviewColumnManual, AutoAssignReviewer: true},
		{ReviewColumn: ReviewColumnOff, AutoAssignReviewer: true},
		DefaultBoardConfig(),
	} {
		if p := personaForStage(tc, StageInReview); p != nil {
			t.Fatalf("non-firing config %+v must resolve no persona", tc)
		}
	}

	// An explicit stagePersonas key beats the legacy composition —
	// INCLUDING the empty-object opt-out (round-2 review finding: {}
	// must own the stage mapping outright, or the defaults flip could
	// never be opted out per-project).
	eff = DefaultBoardConfig()
	eff.ReviewColumn = ReviewColumnAuto
	eff.AutoAssignReviewer = true
	eff.StagePersonas = map[string]PersonaConfig{"in_review": {Name: "custom-auditor"}}
	eff.StagePersonasSet = true
	if p := personaForStage(eff, StageInReview); p == nil || p.Name != "custom-auditor" {
		t.Fatalf("explicit persona must beat the legacy composition, got %+v", p)
	}
	empty := map[string]PersonaConfig{}
	eff.StagePersonas = empty
	if p := personaForStage(eff, StageInReview); p != nil {
		t.Fatalf("the explicit {} opt-out must disable even the legacy composition, got %+v", p)
	}
	// An explicit map that does not list the stage also suppresses the
	// legacy fallthrough (the explicit key owns the mapping wholesale).
	eff.StagePersonas = map[string]PersonaConfig{"in_progress": {Name: "implementer"}}
	if p := personaForStage(eff, StageInReview); p != nil {
		t.Fatalf("an explicit map owns its stages — unlisted stages get no legacy fallthrough, got %+v", p)
	}

	// The legacy composition never leaks onto other stages.
	eff = DefaultBoardConfig()
	eff.ReviewColumn = ReviewColumnAuto
	eff.AutoAssignReviewer = true
	if p := personaForStage(eff, StageInProgress); p != nil {
		t.Fatalf("legacy composition is in_review-only, got %+v", p)
	}
}

// TestDefaultBoardConfigNoPersonas — the code default carries NO
// personas: the mechanism is entirely opt-in (back-compat floor).
func TestDefaultBoardConfigNoPersonas(t *testing.T) {
	eff := DefaultBoardConfig()
	assert.Empty(t, eff.StagePersonas)
	for _, stage := range []string{StageBacklog, StageInProgress, StageInReview, StageDone, StagePendingInput} {
		assert.Nil(t, personaForStage(eff, stage), "default config resolves no persona for %s", stage)
	}
}
