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
// unit belts, ROLE-ONLY (owner ruling 2026-10-29, round 2): values are
// agent-role references (the bare id/slug string canonical, the
// {roleId} object decodable, nothing else) and the legacy
// {name, instructions} shape rejects loud — no decode, no migration
// (owner ruling: no personas were ever created).

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
// eligible stages + bounded role references accepted (both admitted
// shapes); ineligible stages, nulls, empties, oversize, the legacy
// name/instructions shape, and unknown nested fields reject.
func TestStagePersonasValidation(t *testing.T) {
	// Accepted shapes — the two role-reference forms, nothing else.
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":"reviewer"}`)           // the bare string (canonical)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_progress":{"roleId":"build"}}`) // the {roleId} object
	mustNotErr(t, ConfigKeyStagePersonas, `{}`)                                 // the explicit opt-out override
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":"`+strings.Repeat("a", PersonaRoleIDMax)+`"}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"`+strings.Repeat("a", PersonaRoleIDMax)+`"}}`)

	// Rejected shapes — ineligible stages (no live claim can exist).
	for _, stage := range []string{"backlog", "done", "pending_input", "some_arbitrary_stage"} {
		mustReject(t, ConfigKeyStagePersonas,
			`{"`+stage+`":"reviewer"}`, "cannot hold a persona")
	}
	// Nulls (the never-a-value rule), empties, bounds.
	mustReject(t, ConfigKeyStagePersonas, `null`, "must not be null")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":null}`, "must not map to null")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":""}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":"   "}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":""}}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"}`, "exceeds")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"}}`, "exceeds")

	// The legacy shapes are GONE (owner ruling: no migration, no
	// compat decode — a name/instructions payload fails loud).
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"reviewer"}}`, "role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"instructions":"Verify."}}`, "role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"r","instructions":"override"}}`, "role reference")
	// Unknown fields in the {roleId} object (the strict vocabulary at
	// the nested level — a typo must never ride silently).
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"r","promt":"typo"}}`, "role reference")
	// Non-object maps.
	mustReject(t, ConfigKeyStagePersonas, `["in_review"]`, "must be an object")
	mustReject(t, ConfigKeyStagePersonas, `"nope"`, "must be an object")
	mustReject(t, ConfigKeyStagePersonas, `5`, "must be an object")
}

// TestStagePersonasDecodeRoundTrip — storage rows decode back to the
// typed partial override (set keys only; absent = inherit). The stored
// form is the CANONICAL bare string: both admitted shapes normalize
// through PersonaConfig's codec.
func TestStagePersonasDecodeRoundTrip(t *testing.T) {
	in := map[string]PersonaConfig{
		"in_review":   {RoleID: "reviewer"},
		"in_progress": {RoleID: "implementer"},
	}
	o := &BoardConfigOverride{StagePersonas: &in}
	rows := o.rows()
	found := false
	for _, r := range rows {
		if r.Key == ConfigKeyStagePersonas {
			found = true
			// The stored row is the canonical bare-string map.
			assert.JSONEq(t, `{"in_review":"reviewer","in_progress":"implementer"}`, string(r.Value))
			back, err := DecodeBoardConfigOverride(map[string]json.RawMessage{r.Key: r.Value})
			require.NoError(t, err)
			require.NotNil(t, back.StagePersonas)
			assert.Equal(t, in, *back.StagePersonas)
		}
	}
	assert.True(t, found, "stagePersonas encodes to a config row")
	// The {roleId} object decodes to the same reference (one shape,
	// one value — the object is admission, not meaning).
	back, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
		ConfigKeyStagePersonas: json.RawMessage(`{"in_review":{"roleId":"reviewer"}}`),
	})
	require.NoError(t, err)
	require.NotNil(t, back.StagePersonas)
	assert.Equal(t, PersonaConfig{RoleID: "reviewer"}, (*back.StagePersonas)[StageInReview])
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
		"in_review": {RoleID: "inherited-reviewer"},
	}

	// Absent key inherits.
	got := MergeBoardConfig(base, &BoardConfigOverride{})
	assert.Equal(t, base.StagePersonas, got.StagePersonas)

	// Set key replaces wholesale — a partial map does NOT per-stage
	// merge (per-KEY semantics).
	repl := map[string]PersonaConfig{"in_progress": {RoleID: "implementer"}}
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
// (the built-in reviewer role reference) for in_review (existing
// stored configs keep their meaning — auto-review on IS in_review
// having a reviewer persona via the generic config); everything else
// is nil (today's behavior).
func TestPersonaForStageResolution(t *testing.T) {
	// Explicit entry — arbitrary stage, arbitrary role reference (the
	// flag is what the MERGE sets; direct construction models the same
	// shape).
	eff := DefaultBoardConfig()
	eff.StagePersonas = map[string]PersonaConfig{"in_review": {RoleID: "auditor"}}
	eff.StagePersonasSet = true
	if p := personaForStage(eff, StageInReview); p == nil || p.RoleID != "auditor" {
		t.Fatalf("explicit in_review persona must win, got %+v", p)
	}
	eff.StagePersonas = map[string]PersonaConfig{"in_progress": {RoleID: "implementer"}}
	if p := personaForStage(eff, StageInProgress); p == nil || p.RoleID != "implementer" {
		t.Fatalf("explicit in_progress persona must win, got %+v", p)
	}

	// Legacy composition: auto + true → the default reviewer persona
	// (the built-in reviewer role reference).
	eff = DefaultBoardConfig()
	eff.ReviewColumn = ReviewColumnAuto
	eff.AutoAssignReviewer = true
	if p := personaForStage(eff, StageInReview); p == nil || p.RoleID != DefaultReviewerPersona.RoleID {
		t.Fatalf("auto+true must render the default reviewer persona, got %+v", p)
	}

	// The legacy gate's non-firing arms → nil. NOTE: the plain
	// auto+true arm is now the CODE DEFAULT (the owner ruling's flip,
	// #1939) and renders the default reviewer persona — it moved to
	// the firing side below; these arms are the explicit non-firing
	// overrides over the flipped floor.
	for _, tc := range []BoardConfig{
		{ReviewColumn: ReviewColumnAuto, AutoAssignReviewer: false},
		{ReviewColumn: ReviewColumnManual, AutoAssignReviewer: true},
		{ReviewColumn: ReviewColumnOff, AutoAssignReviewer: true},
	} {
		if p := personaForStage(tc, StageInReview); p != nil {
			t.Fatalf("non-firing config %+v must resolve no persona", tc)
		}
	}

	// The flipped default floor itself fires (the composition: the
	// defaults flip IS in_review having a reviewer persona).
	flipped := DefaultBoardConfig()
	if p := personaForStage(flipped, StageInReview); p == nil || p.RoleID != DefaultReviewerPersona.RoleID {
		t.Fatalf("the flipped default (auto+true) must render the reviewer persona, got %+v", p)
	}

	// An explicit stagePersonas key beats the legacy composition —
	// INCLUDING the empty-object opt-out (round-2 review finding: {}
	// must own the stage mapping outright, or the defaults flip could
	// never be opted out per-project).
	eff = DefaultBoardConfig()
	eff.ReviewColumn = ReviewColumnAuto
	eff.AutoAssignReviewer = true
	eff.StagePersonas = map[string]PersonaConfig{"in_review": {RoleID: "custom-auditor"}}
	eff.StagePersonasSet = true
	if p := personaForStage(eff, StageInReview); p == nil || p.RoleID != "custom-auditor" {
		t.Fatalf("explicit persona must beat the legacy composition, got %+v", p)
	}
	empty := map[string]PersonaConfig{}
	eff.StagePersonas = empty
	if p := personaForStage(eff, StageInReview); p != nil {
		t.Fatalf("the explicit {} opt-out must disable even the legacy composition, got %+v", p)
	}
	// An explicit map that does not list the stage also suppresses the
	// legacy fallthrough (the explicit key owns the mapping wholesale).
	eff.StagePersonas = map[string]PersonaConfig{"in_progress": {RoleID: "implementer"}}
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

// TestDefaultBoardConfigPersonaPosture — after the #1939 defaults
// flip: the code default carries NO explicit personas but the flipped
// legacy floor (auto+true) renders the reviewer persona for
// in_review; the EXPLICIT {} opt-out owns the mapping and renders
// none anywhere (the per-project/per-user disable lever).
func TestDefaultBoardConfigPersonaPosture(t *testing.T) {
	eff := DefaultBoardConfig()
	assert.Empty(t, eff.StagePersonas)
	assert.False(t, eff.StagePersonasSet)
	if p := personaForStage(eff, StageInReview); p == nil || p.RoleID != DefaultReviewerPersona.RoleID {
		t.Fatalf("the flipped floor renders the reviewer persona for in_review, got %+v", p)
	}
	if p := personaForStage(eff, StageInProgress); p != nil {
		t.Fatalf("the floor is in_review-only, got %+v", p)
	}
	// The explicit opt-out: merged from an explicit {} override.
	eff = MergeBoardConfig(DefaultBoardConfig(), &BoardConfigOverride{
		StagePersonas: &map[string]PersonaConfig{},
	})
	for _, stage := range []string{StageBacklog, StageInProgress, StageInReview, StageDone, StagePendingInput} {
		assert.Nil(t, personaForStage(eff, stage), "the explicit {} opt-out resolves no persona for %s", stage)
	}
}
