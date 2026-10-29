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

// persona_roles_unit_test.go — the role-first persona vocabulary's
// unit belts (owner ruling 2026-10-29: "we can use the agent roles the
// platform already has. Rather than redefining them that way they can
// be reused."). RED-first for RoleID on PersonaConfig, the bare-string
// role reference decode, and the name→role migration helpers.

// TestPersonaRoleRefValidation — the persona map accepts the role-first
// shapes (bare reference string, {roleId}, {roleId, instructions},
// {roleId, name}) AND the legacy {name, instructions}; the empty-both
// and oversize-reference shapes reject.
func TestPersonaRoleRefValidation(t *testing.T) {
	// Accepted role-first shapes.
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":"reviewer"}`)                                          // the compact wire form
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_progress":{"roleId":"build"}}`)                                // object, reference only
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"reviewer","instructions":"Adversarial."}}`) // override layer
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"`+strings.Repeat("a", PersonaRoleIDMax)+`"}}`)
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"11111111-1111-1111-1111-111111111111","name":"Reviewer"}}`) // id + display name

	// The legacy shape still decodes (backward compat, storage + wire).
	mustNotErr(t, ConfigKeyStagePersonas, `{"in_review":{"name":"auditor","instructions":"Verify."}}`)

	// Rejected shapes.
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":""}`, "requires a role reference (roleId) or a non-empty name")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{}}`, "requires a role reference (roleId) or a non-empty name")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"instructions":"neither role nor name"}}`, "requires a role reference (roleId) or a non-empty name")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"}}`, "role reference exceeds")
	mustReject(t, ConfigKeyStagePersonas, `{"in_review":{"roleId":"r","promt":"typo"}}`, "unknown field")
}

// TestPersonaRoleRefDecode — decode resolves the compact and object
// shapes to the same typed persona; the round-trip through storage
// rows preserves the reference.
func TestPersonaRoleRefDecode(t *testing.T) {
	raw := json.RawMessage(`{"in_review":"reviewer","in_progress":{"roleId":"build","instructions":"Ship it."}}`)
	m, err := decodePersonaMap(raw)
	require.NoError(t, err)
	assert.Equal(t, PersonaConfig{RoleID: "reviewer"}, m["in_review"])
	assert.Equal(t, PersonaConfig{RoleID: "build", Instructions: "Ship it."}, m["in_progress"])

	// Round-trip: encode → decode keeps the reference (the storage row
	// shape).
	o := &BoardConfigOverride{StagePersonas: &m}
	var row configRow
	for _, r := range o.rows() {
		if r.Key == ConfigKeyStagePersonas {
			row = r
		}
	}
	back, err := DecodeBoardConfigOverride(map[string]json.RawMessage{row.Key: row.Value})
	require.NoError(t, err)
	assert.Equal(t, m, *back.StagePersonas)
}

// TestResolveAgentRoleRef — resolution precedence: an explicit RoleID
// matches id or slug EXACTLY (an unresolvable explicit reference is
// never silently re-pointed at a name match); a name-only persona
// (the migration input) matches slug then name; nothing matches
// nothing.
func TestResolveAgentRoleRef(t *testing.T) {
	roles := []AgentRoleRef{
		{ID: "11111111-1111-1111-1111-111111111111", Slug: "reviewer", Name: "Code Reviewer", Scope: "platform"},
		{ID: "22222222-2222-2222-2222-222222222222", Slug: "build", Name: "Builder", Scope: "org"},
	}

	// Explicit references: id and slug both address the role.
	r := ResolveAgentRoleRef(PersonaConfig{RoleID: "11111111-1111-1111-1111-111111111111"}, roles)
	require.NotNil(t, r)
	assert.Equal(t, "reviewer", r.Slug)
	r = ResolveAgentRoleRef(PersonaConfig{RoleID: "reviewer"}, roles)
	require.NotNil(t, r)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", r.ID)

	// An unresolvable EXPLICIT reference stays unresolvable — a stored
	// "reviewer-2" roleId never falls back to a name/slug match.
	assert.Nil(t, ResolveAgentRoleRef(PersonaConfig{RoleID: "gone", Name: "reviewer"}, roles))

	// The migration's name→role synthesis: slug match, then name match.
	r = ResolveAgentRoleRef(PersonaConfig{Name: "reviewer"}, roles)
	require.NotNil(t, r)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", r.ID)
	r = ResolveAgentRoleRef(PersonaConfig{Name: "Builder"}, roles)
	require.NotNil(t, r)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", r.ID)

	// Nothing in, nothing out.
	assert.Nil(t, ResolveAgentRoleRef(PersonaConfig{}, roles))
	assert.Nil(t, ResolveAgentRoleRef(PersonaConfig{Name: "ghost"}, roles))
}

// TestMigratePersonaRoles — the stored-config migration's pure core:
// legacy name-only personas stamp the resolved role's id; unresolvable
// names keep the persona verbatim (the instructions survive as a
// role-less override); already-role-bearing personas are untouched
// (including slug references — canonicalization is the spawn-time
// applyPersonaRole's job, never a storage rewrite).
func TestMigratePersonaRoles(t *testing.T) {
	roles := []AgentRoleRef{
		{ID: "11111111-1111-1111-1111-111111111111", Slug: "reviewer", Name: "Code Reviewer"},
	}
	in := map[string]PersonaConfig{
		"in_review":   {Name: "reviewer", Instructions: "Verify."},     // resolves by slug
		"in_progress": {Name: "implementer", Instructions: "Ship it."}, // unresolvable — kept verbatim
	}
	out := MigratePersonaRoles(in, roles)
	assert.Equal(t, PersonaConfig{
		RoleID: "11111111-1111-1111-1111-111111111111", Name: "reviewer", Instructions: "Verify.",
	}, out["in_review"], "a resolvable name stamps the role id (name + instructions preserved)")
	assert.Equal(t, in["in_progress"], out["in_progress"], "an unresolvable name keeps the legacy persona verbatim")

	// A slug reference rides untouched (never rewritten in storage).
	slugRef := map[string]PersonaConfig{"in_review": {RoleID: "reviewer"}}
	assert.Equal(t, slugRef, MigratePersonaRoles(slugRef, roles))

	assert.Nil(t, MigratePersonaRoles(nil, roles))
}

// TestPersonaLabel — the non-empty identity the claim stamps render:
// name first, else the role reference (a role-first persona may carry
// no name at all).
func TestPersonaLabel(t *testing.T) {
	assert.Equal(t, "auditor", PersonaConfig{Name: "auditor", RoleID: "r1"}.Label())
	assert.Equal(t, "reviewer", PersonaConfig{RoleID: "reviewer"}.Label())
	assert.Equal(t, "", PersonaConfig{}.Label())
}
