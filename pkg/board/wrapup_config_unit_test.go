// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wrapup_config_unit_test.go — the wrapUpReviewer config slot's unit
// belts (epic wrap-up review, low-level design §6 / high-level D8):
// one key, BOTH tiers (user default + project override; deliberately
// NO per-epic reviewer override — the epic's own wrapUpReview flag is
// the only per-epic control, owner ruling 2026-11-15). Values are bare
// role refs through the SAME PersonaConfig codec discipline as
// stagePersonas; an unresolved ref refuses the whole set-time write
// (validatePersonaRoleRefs). No set-ness flag: a bare-string key with
// a non-empty default has no opt-out semantics — empty means "default"
// on the effective row, and un-setting is key ABSENCE (the REPLACE
// discipline).

// wrapupDir is the RoleDirectory double for set-belt unit pins.
type wrapupDir struct {
	roles []AgentRoleRef
	err   error
}

func (d *wrapupDir) AgentRolesForUser(context.Context, string) ([]AgentRoleRef, error) {
	return d.roles, d.err
}

var (
	wrapupRoleFix = AgentRoleRef{ID: "22222222-2222-2222-2222-222222222222", Slug: "wrapup-reviewer", Name: "Wrap-up reviewer", Scope: "platform"}
	customRoleFix = AgentRoleRef{ID: "33333333-3333-3333-3333-333333333333", Slug: "custom", Name: "Custom", Scope: "org"}
)

// TestWrapUpReviewerDefaultSlot — the default floor: the seeded
// wrapup-reviewer role's slug seeds DefaultBoardConfig (every tier
// inherits it until an explicit write lands), and the default persona
// is a bare role reference — no behavior of its own (the role-only
// ruling; the charter lives in the SEEDED role's system prompt).
func TestWrapUpReviewerDefaultSlot(t *testing.T) {
	assert.Equal(t, "wrapup-reviewer", DefaultWrapUpReviewerRoleSlug)
	assert.Equal(t, PersonaConfig{RoleID: DefaultWrapUpReviewerRoleSlug}, DefaultWrapUpReviewerPersona)
	assert.Equal(t, DefaultWrapUpReviewerRoleSlug, DefaultBoardConfig().WrapUpReviewer,
		"the code default seeds the wrap-up reviewer persona's role reference")
	// The persona's wire form is the bare slug (the codec's canonical
	// shape — nothing else exists).
	b, err := json.Marshal(DefaultWrapUpReviewerPersona)
	require.NoError(t, err)
	assert.Equal(t, `"wrapup-reviewer"`, string(b))
}

// TestWrapUpReviewerKeyValidation — the slot decodes exactly the
// PersonaConfig role-reference shapes (the one-contract-per-shape
// discipline): the bare string is canonical, the {roleId} object
// decodes, null/empty/oversize/legacy shapes reject loud.
func TestWrapUpReviewerKeyValidation(t *testing.T) {
	mustNotErr(t, ConfigKeyWrapUpReviewer, `"reviewer"`)                                        // canonical bare ref
	mustNotErr(t, ConfigKeyWrapUpReviewer, `{"roleId":"custom"}`)                               // the {roleId} object
	mustNotErr(t, ConfigKeyWrapUpReviewer, `{"roleId":"22222222-2222-2222-2222-222222222222"}`) // by id
	mustNotErr(t, ConfigKeyWrapUpReviewer, `"`+strings.Repeat("a", PersonaRoleIDMax)+`"`)

	// null is never a value — an unset key is ABSENT, not null.
	mustReject(t, ConfigKeyWrapUpReviewer, `null`, "must not be null")
	// The trim-aware non-empty rule (parity with the persona map's
	// semantic check).
	mustReject(t, ConfigKeyWrapUpReviewer, `""`, "non-empty role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `"   "`, "non-empty role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{}`, "non-empty role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"roleId":""}`, "non-empty role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"roleId":"   "}`, "non-empty role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `"`+strings.Repeat("a", PersonaRoleIDMax+1)+`"`, "exceeds")
	// The legacy persona shapes never decode (role-only ruling — a
	// stored non-role shape is drift, nothing migrates).
	mustReject(t, ConfigKeyWrapUpReviewer, `{"name":"auditor","instructions":"Verify."}`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"name":"auditor"}`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"instructions":"Verify."}`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"roleId":"r","instructions":"x"}`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `{"roleId":"r","promt":"typo"}`, "role reference")
	// Non-reference types never decode.
	mustReject(t, ConfigKeyWrapUpReviewer, `17`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `true`, "role reference")
	mustReject(t, ConfigKeyWrapUpReviewer, `[]`, "role reference")

	// The known-key list names the new key (an unknown-key refusal must
	// teach the caller the vocabulary).
	err := ValidateBoardConfigKey("wrapUpReviwer", json.RawMessage(`"reviewer"`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrapUpReviewer")
}

// TestWrapUpReviewerMerge — inherit/override exactly like reviewColumn:
// a set key wins at its tier, an unset key inherits the base, and the
// empty effective value means "default" (the floor is non-empty; only
// drift can render empty, and resolution treats it as default).
func TestWrapUpReviewerMerge(t *testing.T) {
	base := DefaultBoardConfig()
	assert.Equal(t, DefaultWrapUpReviewerRoleSlug, base.WrapUpReviewer)

	custom := "custom"
	ov := &BoardConfigOverride{WrapUpReviewer: &custom}
	merged := MergeBoardConfig(base, ov)
	assert.Equal(t, "custom", merged.WrapUpReviewer, "a set override wins")

	merged = MergeBoardConfig(base, &BoardConfigOverride{})
	assert.Equal(t, DefaultWrapUpReviewerRoleSlug, merged.WrapUpReviewer, "an unset key inherits the base")
	merged = MergeBoardConfig(base, nil)
	assert.Equal(t, DefaultWrapUpReviewerRoleSlug, merged.WrapUpReviewer, "a nil overlay is the base verbatim")
}

// TestWrapUpReviewerDecodeAndRows — the raw map decodes into the typed
// override, the typed override re-validates, and the storage row is
// the CANONICAL bare string (the round-trip shape the CHECK-grown key
// physically stores).
func TestWrapUpReviewerDecodeAndRows(t *testing.T) {
	o, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
		"wrapUpReviewer": json.RawMessage(`"custom"`),
	})
	require.NoError(t, err)
	require.NotNil(t, o.WrapUpReviewer)
	assert.Equal(t, "custom", *o.WrapUpReviewer)

	// The {roleId} object decodes to the same typed value.
	o2, err := DecodeBoardConfigOverride(map[string]json.RawMessage{
		"wrapUpReviewer": json.RawMessage(`{"roleId":"custom"}`),
	})
	require.NoError(t, err)
	require.NotNil(t, o2.WrapUpReviewer)
	assert.Equal(t, "custom", *o2.WrapUpReviewer)

	// Unknown key never rides the seam.
	_, err = DecodeBoardConfigOverride(map[string]json.RawMessage{
		"wrapUpReviwer": json.RawMessage(`"custom"`),
	})
	require.Error(t, err)

	require.NoError(t, ValidateBoardConfigOverride(o))
	bad := "  "
	assert.Error(t, ValidateBoardConfigOverride(&BoardConfigOverride{WrapUpReviewer: &bad}),
		"the typed belt re-checks the trim-aware non-empty rule")

	rows := o.rows()
	require.Len(t, rows, 1)
	assert.Equal(t, ConfigKeyWrapUpReviewer, rows[0].Key)
	assert.Equal(t, `"custom"`, string(rows[0].Value), "storage is the canonical bare-string ref")
}

// TestWrapUpReviewerKeySetPin — the wire vocabulary is EXACTLY five
// keys, user+project tiers only: the pin that no epic-level reviewer
// override key exists (owner ruling 2026-11-15 — the epic's own
// wrapUpReview flag is the only per-epic control; a per-epic reviewer
// would have to be a key here, and none is).
func TestWrapUpReviewerKeySetPin(t *testing.T) {
	assert.ElementsMatch(t, []string{
		ConfigKeyReviewColumn,
		ConfigKeyAutoAssignReviewer,
		ConfigKeyDefaultWorkerSlots,
		ConfigKeyStagePersonas,
		ConfigKeyWrapUpReviewer,
	}, BoardConfigKeys())
}

// TestWrapUpReviewerSetRoleRefBelt — validatePersonaRoleRefs gains the
// key: an unresolved wrapUpReviewer reference refuses the WHOLE write,
// on the same vocabulary check stagePersonas rides; a resolvable
// reference (id or slug) passes; a role-bearing set with NO directory
// installed refuses (never store a reference this deployment can never
// honor).
func TestWrapUpReviewerSetRoleRefBelt(t *testing.T) {
	ctx := context.Background()
	healthy := &wrapupDir{roles: []AgentRoleRef{wrapupRoleFix, customRoleFix}}

	// Resolvable refs pass — by slug and by id.
	svc := &Service{roles: healthy}
	assert.NoError(t, svc.validatePersonaRoleRefs(ctx, "u1", &BoardConfigOverride{WrapUpReviewer: strptr("custom")}))
	assert.NoError(t, svc.validatePersonaRoleRefs(ctx, "u1", &BoardConfigOverride{WrapUpReviewer: strptr("22222222-2222-2222-2222-222222222222")}))

	// An unresolved ref refuses, naming the key it rode in on.
	err := (&Service{roles: healthy}).validatePersonaRoleRefs(ctx, "u1", &BoardConfigOverride{WrapUpReviewer: strptr("ghost")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrapUpReviewer")
	assert.Contains(t, err.Error(), "ghost")
	guard, ok := err.(*GuardError)
	require.True(t, ok, "the unresolved ref renders GuardInput (400), got %T", err)
	assert.Equal(t, GuardInput, guard.Code)

	// The refusal covers a write carrying BOTH keys when either ref is
	// stale — the whole write refuses, not just the offending key.
	both := &BoardConfigOverride{
		WrapUpReviewer: strptr("custom"),
		StagePersonas:  &map[string]PersonaConfig{"in_review": {RoleID: "ghost"}},
	}
	err = (&Service{roles: healthy}).validatePersonaRoleRefs(ctx, "u1", both)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stagePersonas")

	both = &BoardConfigOverride{
		WrapUpReviewer: strptr("ghost"),
		StagePersonas:  &map[string]PersonaConfig{"in_review": {RoleID: "custom"}},
	}
	err = (&Service{roles: healthy}).validatePersonaRoleRefs(ctx, "u1", both)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrapUpReviewer")

	// Role-blind deployment: a wrapUpReviewer-bearing set refuses.
	err = (&Service{}).validatePersonaRoleRefs(ctx, "u1", &BoardConfigOverride{WrapUpReviewer: strptr("custom")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no agent-role directory")

	// A directory outage is a plain error (500-class, retryable — never
	// a silent partial write).
	err = (&Service{roles: &wrapupDir{err: assert.AnError}}).validatePersonaRoleRefs(ctx, "u1", &BoardConfigOverride{WrapUpReviewer: strptr("custom")})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "does not match", "the outage is not an unresolvable-ref 400")
}

// strptr is the local *string helper.
func strptr(s string) *string { return &s }

// TestBuildBoardConfigView_WrapUpReviewer — the slot renders with
// ConfigSource* annotations at BOTH tiers: code_default (the seeded
// floor), user_default, project_override — precedence project > user >
// code, the sibling keys' discipline.
func TestBuildBoardConfigView_WrapUpReviewer(t *testing.T) {
	// Nothing stored anywhere: the seeded floor renders as code_default
	// at both tiers, and the effective row carries the slug.
	v := buildBoardConfigView("user", "", nil, nil)
	assert.Equal(t, ConfigSourceCodeDefault, v.Sources[ConfigKeyWrapUpReviewer])
	assert.Equal(t, DefaultWrapUpReviewerRoleSlug, v.Effective.WrapUpReviewer)
	assert.Empty(t, v.Overrides.WrapUpReviewer)

	// A user default set: user_default at the user tier AND at a
	// project tier that does not override it.
	custom := "custom"
	u := &BoardConfigOverride{WrapUpReviewer: &custom}
	v = buildBoardConfigView("user", "", u, nil)
	assert.Equal(t, ConfigSourceUserDefault, v.Sources[ConfigKeyWrapUpReviewer])
	assert.Equal(t, "custom", v.Effective.WrapUpReviewer)
	require.NotNil(t, v.Overrides.WrapUpReviewer)
	assert.Equal(t, "custom", *v.Overrides.WrapUpReviewer)

	v = buildBoardConfigView("project", "p1", u, &BoardConfigOverride{})
	assert.Equal(t, ConfigSourceUserDefault, v.Sources[ConfigKeyWrapUpReviewer],
		"an empty project map with no wrapUpReviewer key inherits the user default")
	assert.Equal(t, "custom", v.Effective.WrapUpReviewer)

	// A project override wins over the user default.
	reviewer := "reviewer"
	p := &BoardConfigOverride{WrapUpReviewer: &reviewer}
	v = buildBoardConfigView("project", "p1", u, p)
	assert.Equal(t, ConfigSourceProjectOverride, v.Sources[ConfigKeyWrapUpReviewer])
	assert.Equal(t, "reviewer", v.Effective.WrapUpReviewer)

	// The user tier stays untouched by the project override.
	v = buildBoardConfigView("user", "", u, nil)
	assert.Equal(t, ConfigSourceUserDefault, v.Sources[ConfigKeyWrapUpReviewer])
}

// TestDefaultWrapUpReviewerRoleSystemPrompt — the JUDGMENT-half charter
// verbatim from the design (§6): the holistic aggregate-diff read, the
// lenses-inside-the-read rule, the reviewer-owns-judgment /
// procedure-not-yours-to-change split, the gap discipline, and the
// never-merge rule. The procedure (verdict verb, gap schema, the cap)
// is ENGINE-side (D8/D9 sibling stories) — the seeded prompt carries
// NO procedure the engine does not enforce, so a custom role cannot
// opt out by editing prose.
func TestDefaultWrapUpReviewerRoleSystemPrompt(t *testing.T) {
	p := DefaultWrapUpReviewerRoleSystemPrompt
	for _, sub := range []string{
		"You are the wrap-up reviewer for an epic",
		"aggregate diff of everything the epic shipped, as one change",
		"epic's stated outcome",
		"Coherence across stories",
		"integration seams",
		"dead or orphaned code",
		"built but never wired",
		"lenses inside that read",
		"never a checklist replacing it",
		"You own the judgment",
		"is not yours to change",
		"File gaps as real stories, deduplicated",
		"never merge, never push",
	} {
		assert.Contains(t, p, sub)
	}
	// The engine-owns-procedure pin: the prompt names the procedure as
	// NOT the reviewer's to change and carries none of the engine's
	// vocabulary — no verdict verb, no gap-ref schema, no tool call
	// instructions. Procedure lives engine-side; a role edit cannot
	// bypass it.
	for _, banned := range []string{
		"board_record_wrapup_verdict",
		"gapRefs",
		"wrapup_ack",
		"consecutive",
	} {
		assert.NotContains(t, p, banned, "the seeded prompt carries no engine procedure (D8/D9): %s is engine-side", banned)
	}
	assert.Less(t, len(p), PersonaInstructionsMaxLegacy, "the charter stays a prompt, not a document")
}
