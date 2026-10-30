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

// persona_roles_unit_test.go — the ROLE-ONLY persona vocabulary's unit
// belts (owner ruling 2026-10-29, round 2: "Board stages should ONLY
// ever have roles configured, there is no custom definition in the
// board itself. ROLES are the way to define behavior for board
// stages."). A persona IS a role reference: the bare id/slug string is
// canonical, the {roleId} object decodes, the legacy
// name/instructions shape and the name→role migration are GONE (owner
// ruling: no personas were ever created — nothing to migrate).

// TestPersonaRoleRefValidation — the persona map admits exactly the
// two role-reference shapes; empty (trim-aware), oversize, and the
// legacy shapes reject loud.
func TestPersonaRoleRefValidation(t *testing.T) {
	// Accepted shapes — role references, nothing else.
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":"reviewer"}`)           // the canonical bare string
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_progress":{"roleId":"build"}}`) // the {roleId} object
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":"`+strings.Repeat("a", PersonaRoleIDMax)+`"}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"11111111-1111-1111-1111-111111111111"}}`)

	// Rejected shapes — the trim-aware non-empty rule (whitespace-only
	// is the empty slot; parity with agentd's validate_file semantic
	// check, one contract per shape).
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":""}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":"   "}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{}}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":""}}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"   "}}`, "non-empty role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"}}`, "exceeds")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"}`, "exceeds")

	// The legacy shapes are GONE — no decode, no compat (owner ruling:
	// no personas were ever created; a stored non-role shape is drift).
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"auditor","instructions":"Verify."}}`, "role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"name":"reviewer"}}`, "role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"instructions":"Verify."}}`, "role reference")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"r","instructions":"override"}}`, "role reference")
	// Unknown fields in the {roleId} object (the strict vocabulary at
	// the nested level — a typo must never ride silently).
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"r","promt":"typo"}}`, "role reference")
}

// TestPersonaRoleRefDecode — decode resolves the bare string and the
// {roleId} object to the SAME typed persona; the storage round-trip
// preserves the reference in the canonical form.
func TestPersonaRoleRefDecode(t *testing.T) {
	raw := json.RawMessage(`{"in_review":"reviewer","in_progress":{"roleId":"build"}}`)
	m, err := decodePersonaMap(raw)
	require.NoError(t, err)
	assert.Equal(t, PersonaConfig{RoleID: "reviewer"}, m["in_review"])
	assert.Equal(t, PersonaConfig{RoleID: "build"}, m["in_progress"])

	// Round-trip: encode → decode keeps the reference; the storage row
	// is the canonical bare-string map (both shapes normalize).
	o := &BoardConfigOverride{StagePersonas: &m}
	var row configRow
	for _, r := range o.rows() {
		if r.Key == ConfigKeyStagePersonas {
			row = r
		}
	}
	assert.JSONEq(t, `{"in_review":"reviewer","in_progress":"build"}`, string(row.Value),
		"the canonical storage form is the bare reference string")
	back, err := DecodeBoardConfigOverride(map[string]json.RawMessage{row.Key: row.Value})
	require.NoError(t, err)
	assert.Equal(t, m, *back.StagePersonas)
}

// TestPersonaConfigCodec — PersonaConfig's codec pins the wire
// contract: MarshalJSON renders the bare reference (canonical),
// UnmarshalJSON admits the bare string and the {roleId} object and
// refuses everything else (including the legacy name shape).
func TestPersonaConfigCodec(t *testing.T) {
	b, err := json.Marshal(PersonaConfig{RoleID: "reviewer"})
	require.NoError(t, err)
	assert.Equal(t, `"reviewer"`, string(b), "the canonical wire form is the bare string")

	for _, raw := range []string{`"reviewer"`, `{"roleId":"reviewer"}`} {
		var p PersonaConfig
		require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
		assert.Equal(t, PersonaConfig{RoleID: "reviewer"}, p, raw)
	}
	for _, raw := range []string{`{"name":"reviewer"}`, `{"roleId":"r","instructions":"x"}`, `5`, `true`} {
		var p PersonaConfig
		require.Error(t, json.Unmarshal([]byte(raw), &p), "the non-role shape must refuse: %s", raw)
	}
}

// TestResolveAgentRoleRef — resolution: an explicit reference (the
// ONLY persona shape) matches id or slug EXACTLY; an unresolvable
// reference is never silently re-pointed; nothing in, nothing out.
func TestResolveAgentRoleRef(t *testing.T) {
	roles := []AgentRoleRef{
		{ID: "11111111-1111-1111-1111-111111111111", Slug: "reviewer", Name: "Code Reviewer", Scope: "platform"},
		{ID: "22222222-2222-2222-2222-222222222222", Slug: "build", Name: "Builder", Scope: "org"},
	}

	// References: id and slug both address the role.
	r := ResolveAgentRoleRef("11111111-1111-1111-1111-111111111111", roles)
	require.NotNil(t, r)
	assert.Equal(t, "reviewer", r.Slug)
	r = ResolveAgentRoleRef("reviewer", roles)
	require.NotNil(t, r)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", r.ID)
	r = ResolveAgentRoleRef("Builder", roles)
	require.Nil(t, r, "the DISPLAY name is not an addressable reference — id or slug only")

	// An unresolvable EXPLICIT reference stays unresolvable — a stored
	// "reviewer-2" never falls back to a fuzzy match.
	assert.Nil(t, ResolveAgentRoleRef("gone", roles))
	assert.Nil(t, ResolveAgentRoleRef("reviewer ", roles), "matching is exact, not trimmed")
	assert.Nil(t, ResolveAgentRoleRef("", roles))
	assert.Nil(t, ResolveAgentRoleRef("reviewer", nil))
}

// TestPersonaLabel — the display/stamp identity the claim mint renders:
// the role reference (the only authored value); the empty shape renders
// the unresolved placeholder.
func TestPersonaLabel(t *testing.T) {
	assert.Equal(t, "reviewer", PersonaConfig{RoleID: "reviewer"}.Label())
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", PersonaConfig{RoleID: "11111111-1111-1111-1111-111111111111"}.Label())
	assert.Equal(t, personaUnresolvedLabel, PersonaConfig{}.Label())
}

// TestDefaultReviewerPersonaIsRoleRefOnly — the default reviewer
// composition is a ROLE REFERENCE to the built-in reviewer agent role
// (slug form): no name, no instructions — the verdict-channel charter
// lives in the role's system prompt, never a board-layer persona.
func TestDefaultReviewerPersonaIsRoleRefOnly(t *testing.T) {
	assert.Equal(t, PersonaConfig{RoleID: DefaultReviewerRoleSlug}, DefaultReviewerPersona)
	assert.Equal(t, "reviewer", DefaultReviewerRoleSlug)
	// The persona carries NO behavior of its own: the codec's wire
	// form is the bare slug.
	b, err := json.Marshal(DefaultReviewerPersona)
	require.NoError(t, err)
	assert.Equal(t, `"reviewer"`, string(b))
}

// TestDefaultReviewerRoleSystemPrompt — the built-in reviewer role's
// system prompt is the verdict-channel charter VERBATIM: the
// board_record_verdict contract, the criteria walk (board_update_task
// setCriteriaMet), the Done guard, and the never-merge rule. The
// platform seeds this prose into the agent-roles table at startup
// (seedBuiltinReviewerRole) — this pin is the single source's
// tripwire: the constant may not drift away from the charter's
// load-bearing clauses.
func TestDefaultReviewerRoleSystemPrompt(t *testing.T) {
	p := DefaultReviewerRoleSystemPrompt
	for _, sub := range []string{
		"Verify EACH acceptance criterion adversarially",
		"board_record_verdict",
		"verdict=approve",
		"ALL criteria met",
		"verdict=fail with findings",
		"Never merge, never push",
		"board_update_task {taskId, claimToken, setCriteriaMet:{index, met}}",
		"board_move_stage→done completes the review",
	} {
		assert.Contains(t, p, sub)
	}
	assert.Less(t, len(p), PersonaInstructionsMaxLegacy, "the charter stays a prompt, not a document")
}

// PersonaInstructionsMaxLegacy is the retired cap's value kept as a
// sanity bound for the seeded prose (the config key's own cap is gone
// with the instructions field; this is a tripwire, not a wire limit).
const PersonaInstructionsMaxLegacy = 4096
