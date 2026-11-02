// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// config.go — Task Board Config (#1832 item 6, part 1): the storage +
// read-time merge behind the board-wide config system.
//
// Model: a USER-SCOPED defaults map (board_config_defaults — the
// owner's board-wide defaults across ALL their projects; boards are
// user-level per design 0067 R7, so "board-wide" means the user's
// boards, NOT the instance) plus an optional PER-PROJECT override map
// (board_project_config_overrides), merged at READ time — an unset
// key in the override map inherits the user default, and a user key
// that was never written inherits the code default. A future INSTANCE
// tier (instance > user > project) is deliberately not built (owner
// ruling on PR #1840); see the migration 000041 header. Storage is
// per-key jsonb rows (the instance_settings shape), so per-project
// stays UI-only later and the key set can grow without a row-shape
// change (grow = key vocabulary + CHECK constraint migration).
//
// Initial keys: reviewColumn (off|manual|auto, default auto — owner
// ruling 2026-10-24 flipped the #1832 default manual→auto),
// autoAssignReviewer (bool, default true — same ruling; explicit
// stored opt-outs at either tier still win), defaultWorkerSlots
// (int > 0, default 2 — the platform's worker-slots default; it feeds
// CreateProject's fallback when the caller specifies none).
//
// Writes are REPLACE-semantics (the tags-REPLACE discipline): a PUT
// carries the full map; absent keys stop being overridden and revert to
// inheritance. Validation is layered: the adapter's key-gate decodes
// and rejects unknown keys/malformed values/null values (400), the
// service re-checks the typed override (belt), and the tables' CHECK
// constraints are the physical floor. Unknown keys are fail-closed on
// read too: a row that somehow drifted past the belts surfaces as an
// error, never a silently ignored value.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Board config keys (the wire vocabulary, camelCase).
const (
	ConfigKeyReviewColumn       = "reviewColumn"
	ConfigKeyAutoAssignReviewer = "autoAssignReviewer"
	ConfigKeyDefaultWorkerSlots = "defaultWorkerSlots"
	ConfigKeyStagePersonas      = "stagePersonas"
)

// ReviewColumn values: whether the review column shows at all and who
// moves rows through it. auto stores the preference ONLY — the review
// automation it gates is PR-B1/B2 (#1832 item 5), deliberately out of
// this storage's scope.
const (
	ReviewColumnOff    = "off"
	ReviewColumnManual = "manual"
	ReviewColumnAuto   = "auto"
)

// DefaultBoardWorkerSlots is the platform default for worker slots: the
// Go-side fallback in CreateProject (when the caller specifies none and
// the user's defaultWorkerSlots config key is unset) and the config
// key's code default. The 000036 SQL column default (DEFAULT 2) is
// pinned to the same value by migration convention — SQL cannot import
// the Go constant.
const DefaultBoardWorkerSlots = 2

// PersonaRoleIDMax bounds one persona's role reference (the #1837 P1b
// discipline: a cap on the stored text field). The reference rides the
// spawn dispatch (the opencode agentID) and must stay a short, opaque
// token — a UUID or slug, never a prompt.
const PersonaRoleIDMax = 128

// PersonaConfig is ONE stage's configured agent persona — a ROLE
// REFERENCE, nothing else (owner ruling 2026-10-29: "Board stages
// should ONLY ever have roles configured, there is no custom
// definition in the board itself. ROLES are the way to define
// behavior for board stages."). RoleID names a platform agent role
// (by id or slug); the referenced role is the single source of truth
// for the spawned worker's model, tool surface, and system prompt —
// including the reviewer's verdict-channel charter, which the platform
// seeds INTO the built-in reviewer role (see
// board.DefaultReviewerRoleSystemPrompt). The board layer defines no
// behavior of its own: name and instructions do not exist here, on
// the wire, or in storage.
//
// Wire/storage shapes: the BARE STRING ("in_review": "reviewer") is
// canonical; the {roleId} object decodes but carries nothing else.
// Anything else — the legacy {name, instructions} shape above all —
// rejects loud and never decodes (owner ruling: no personas were ever
// created, so nothing needs to migrate; a stored non-role shape is
// storage drift).
type PersonaConfig struct {
	// RoleID references the platform agent role (matched by id or
	// slug against the available set; canonicalized to the role id at
	// spawn resolution).
	RoleID string `json:"roleId,omitempty"`
}

// personaUnresolvedLabel is the display identity of a persona whose
// role reference is empty (the degradation shape applyPersonaRole
// never mints but Label must still render for). Never an authored
// value: decode rejects empty references.
const personaUnresolvedLabel = "(unresolved role)"

// MarshalJSON renders the CANONICAL wire/storage form: the bare role
// reference string.
func (p PersonaConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.RoleID)
}

// UnmarshalJSON admits exactly two shapes: the bare reference string
// (canonical) and the {roleId} object (decodable, carries nothing
// else). Unknown fields reject (the strict-vocabulary discipline — a
// name/instructions/typo key must never ride silently into storage).
func (p *PersonaConfig) UnmarshalJSON(raw []byte) error {
	var ref string
	if err := json.Unmarshal(raw, &ref); err == nil {
		p.RoleID = ref
		return nil
	}
	var obj struct {
		RoleID string `json:"roleId"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("a persona is a role reference — the bare id/slug string (canonical) or the {roleId} object; got %s", string(raw))
	}
	p.RoleID = obj.RoleID
	return nil
}

// AgentRoleRef is one selectable agent role in the persona vocabulary:
// the fields a persona's RoleID may carry and the display name the
// editor renders. ID and Slug are both addressable as RoleID; Name is
// the migration fallback (legacy personas matched by name).
type AgentRoleRef struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Scope string `json:"scope"` // "platform" | "org"
}

// RoleDirectory resolves the agent-role vocabulary a board owner may
// reference from stagePersonas (the platform's agent_roles surface —
// org-level + platform roles available to the user). Installed through
// WithRoleDirectory; nil keeps the persona seam role-blind (a
// role-bearing set REFUSES on set under a nil directory — the
// role-only codec admits role references only and validates them
// against this vocabulary when one is installed; there is no other
// persona shape to fall back to).
type RoleDirectory interface {
	// AgentRolesForUser lists the roles addressable by the user
	// (platform roles plus their org's when they belong to one).
	AgentRolesForUser(ctx context.Context, userID string) ([]AgentRoleRef, error)
}

// ResolveAgentRoleRef matches one role reference (id or slug) against
// the available roles: exact id match, then exact slug match. nil when
// nothing matches — an explicit reference that does not resolve NEVER
// silently re-points (the caller degrades; see applyPersonaRole).
func ResolveAgentRoleRef(ref string, roles []AgentRoleRef) *AgentRoleRef {
	if ref == "" {
		return nil
	}
	for i := range roles {
		if roles[i].ID == ref || roles[i].Slug == ref {
			return &roles[i]
		}
	}
	return nil
}

// personaEligibleStages lists the stages a persona can be configured
// for: exactly the stages a LIVE CLAIM can exist in (a persona worker
// holds a lease + claim token). in_progress (the worker grant / bounce
// re-claim) and in_review (the persona claim minted on entry). backlog
// is the claim pool itself, done is terminal (RC5), and pending_input
// retains the suspended worker's token with human-only exits — a
// persona key on those would render an "effective" config the engine
// never honors, the defaultWorkerSlots-at-project-tier shape, and is
// rejected instead (fail-closed vocabulary).
var personaEligibleStages = map[string]bool{
	StageInProgress: true,
	StageInReview:   true,
}

// BoardConfig is the effective merged configuration row (every key
// always present on the wire).
type BoardConfig struct {
	ReviewColumn       string                   `json:"reviewColumn"`
	AutoAssignReviewer bool                     `json:"autoAssignReviewer"`
	DefaultWorkerSlots int                      `json:"defaultWorkerSlots"`
	StagePersonas      map[string]PersonaConfig `json:"stagePersonas"`
	// StagePersonasSet reports whether an explicit stagePersonas key
	// was set at ANY merged tier (even an empty {} — the opt-out).
	// It governs the legacy-composition fallthrough: an explicit key
	// OWNS the stage mapping, so {} disables even the legacy
	// reviewColumn=auto + autoAssignReviewer reviewer persona (the
	// opt-out the defaults flip needs). On the wire it is the
	// SET-NESS surface: a REPLACE-tier client (the Settings tab)
	// carries the stored map back ONLY when set — an unset key sent
	// as {} would materialize an explicit opt-out and silently
	// suppress the flipped floor for every inheriting project.
	StagePersonasSet bool `json:"stagePersonasSet"`
}

// BoardConfigOverride is a PARTIAL config: nil fields are unset and
// inherit at merge time. On the wire an unset key is absent (omitempty),
// never null.
type BoardConfigOverride struct {
	ReviewColumn       *string `json:"reviewColumn,omitempty"`
	AutoAssignReviewer *bool   `json:"autoAssignReviewer,omitempty"`
	DefaultWorkerSlots *int    `json:"defaultWorkerSlots,omitempty"`
	// StagePersonas is one key whose value is an object (stage →
	// persona). Merge semantics are per-KEY like every other key: a set
	// stagePersonas object REPLACES the inherited map wholesale (an
	// absent key inherits; an explicitly empty object {} is the OPT-OUT
	// — it owns the stage mapping outright and disables even the legacy
	// auto-review composition's default reviewer persona for that
	// tier). This is the composition lever: a project override {}
	// disables an inherited reviewer persona however it was configured.
	StagePersonas *map[string]PersonaConfig `json:"stagePersonas,omitempty"`
}

// ProjectBoardConfig renders one project's config surfaces in a single
// read: the raw stored overrides plus the effective merged row.
type ProjectBoardConfig struct {
	ProjectID string              `json:"project_id"`
	Overrides BoardConfigOverride `json:"overrides"`
	Effective BoardConfig         `json:"effective"`
}

// Board config value sources — WHERE one effective key came from (the
// board_get_config source annotations). The vocabulary is ordered by
// the merge itself: a project override wins, then a user default,
// then the code default.
const (
	ConfigSourceCodeDefault     = "code_default"
	ConfigSourceUserDefault     = "user_default"
	ConfigSourceProjectOverride = "project_override"
)

// BoardPersonaView is one persona-eligible stage's rendered provenance
// in BoardConfigView: the source vocabulary of
// PersonaProvenanceForStage plus the persona that source renders
// (absent for the none shapes — none_explicit renders the
// explicit-map semantics, none renders nothing configured).
type BoardPersonaView struct {
	Source  string         `json:"source"`
	Persona *PersonaConfig `json:"persona,omitempty"`
}

// BoardConfigView is board_get_config's rendered shape at EITHER tier:
// the tier's raw override map, the effective merged row, per-key
// source annotations (code_default | user_default |
// project_override), and per-stage persona provenance — the read an
// agent (or the board UI) needs to see not just WHAT is effective but
// WHERE each piece came from and which tier to override.
type BoardConfigView struct {
	Scope     string                      `json:"scope"` // "user" | "project"
	ProjectID string                      `json:"project_id,omitempty"`
	Overrides BoardConfigOverride         `json:"overrides"`
	Effective BoardConfig                 `json:"effective"`
	Sources   map[string]string           `json:"sources"`
	Personas  map[string]BoardPersonaView `json:"personas"`
}

// DefaultBoardConfig returns the code defaults (owner ruling
// 2026-10-24 — "flip defaults for review to on"): auto review column
// with auto reviewer assignment, worker slots 2, no stage personas. A
// stored explicit opt-out at either tier (user defaults, project
// overrides — including an explicit stagePersonas key, which owns the
// stage mapping outright) still wins at the read-time merge — the
// flip changes only the inheritance floor. With the flip, the DEFAULT
// effective config renders the reviewer persona for in_review through
// the stage-persona mechanism's legacy-composition arm (auto-review
// on IS in_review having a reviewer persona via the generic config).
func DefaultBoardConfig() BoardConfig {
	return BoardConfig{
		ReviewColumn:       ReviewColumnAuto,
		AutoAssignReviewer: true,
		DefaultWorkerSlots: DefaultBoardWorkerSlots,
		StagePersonas:      map[string]PersonaConfig{},
	}
}

// BoardConfigKeys lists the known config keys (grow the vocabulary =
// add a key here, a struct field, and its validation).
func BoardConfigKeys() []string {
	return []string{ConfigKeyReviewColumn, ConfigKeyAutoAssignReviewer, ConfigKeyDefaultWorkerSlots, ConfigKeyStagePersonas}
}

// MergeBoardConfig overlays a partial override onto a base at read
// time: a set key wins, an unset (nil) key inherits the base value.
// nil/empty overlays render the base verbatim. stagePersonas replaces
// wholesale when set (per-KEY semantics — see BoardConfigOverride).
func MergeBoardConfig(base BoardConfig, o *BoardConfigOverride) BoardConfig {
	out := base
	if o == nil {
		return out
	}
	if o.ReviewColumn != nil {
		out.ReviewColumn = *o.ReviewColumn
	}
	if o.AutoAssignReviewer != nil {
		out.AutoAssignReviewer = *o.AutoAssignReviewer
	}
	if o.DefaultWorkerSlots != nil {
		out.DefaultWorkerSlots = *o.DefaultWorkerSlots
	}
	if o.StagePersonas != nil {
		m := make(map[string]PersonaConfig, len(*o.StagePersonas))
		for k, v := range *o.StagePersonas {
			m[k] = v
		}
		out.StagePersonas = m
		out.StagePersonasSet = true
	}
	return out
}

// ValidateBoardConfigKey validates one raw key/value pair: the key must
// be known and the value well-formed for its type (enum membership for
// reviewColumn, a real bool for autoAssignReviewer, an integral int > 0
// for defaultWorkerSlots). A JSON null is NEVER a valid value — an
// unset key is ABSENT from the map (omitempty), never null; null is
// rejected so it cannot masquerade as an explicit false/zero and flip
// inheritance semantics. Violations render GuardInput (the 400 class)
// — unknown keys name the known set, malformed values name the key.
func ValidateBoardConfigKey(key string, raw json.RawMessage) error {
	if string(raw) == "null" {
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must not be null — omit the key to inherit the next tier's value", key)}
	}
	switch key {
	case ConfigKeyReviewColumn:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must be a string (off|manual|auto), got %s", key, string(raw))}
		}
		switch v {
		case ReviewColumnOff, ReviewColumnManual, ReviewColumnAuto:
			return nil
		}
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must be one of off, manual, auto (got %q)", key, v)}
	case ConfigKeyAutoAssignReviewer:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must be a boolean, got %s", key, string(raw))}
		}
		return nil
	case ConfigKeyDefaultWorkerSlots:
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must be an integer > 0, got %s", key, string(raw))}
		}
		if v <= 0 {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s must be an integer > 0 (got %d)", key, v)}
		}
		return nil
	case ConfigKeyStagePersonas:
		if _, err := decodePersonaMap(raw); err != nil {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s: %s", key, err.Error())}
		}
		return nil
	default:
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("unknown config key %q (known: reviewColumn, autoAssignReviewer, defaultWorkerSlots, stagePersonas)", key)}
	}
}

// decodePersonaMap decodes + validates the stagePersonas value. The
// ROLE-ONLY shapes (owner ruling 2026-10-29: "Board stages should ONLY
// ever have roles configured, there is no custom definition in the
// board itself."): stage → bare role reference string ("in_review":
// "reviewer", canonical) or stage → the {roleId} object (decodable,
// carries nothing else). Nothing else decodes — the legacy {name,
// instructions} shape above all rejects loud (owner ruling: no
// personas were ever created, so there is nothing to migrate or
// keep decoding). Every stage key must be persona-eligible (a stage a
// live claim can exist in), every reference non-empty (trim-aware —
// parity with agentd's validate_file semantic check, one contract per
// shape), and no unknown fields inside the {roleId} object.
func decodePersonaMap(raw json.RawMessage) (map[string]PersonaConfig, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("must be an object mapping stage → role reference (id or slug), got %s", string(raw))
	}
	out := make(map[string]PersonaConfig, len(m))
	for stage, praw := range m {
		if !personaEligibleStages[stage] {
			return nil, fmt.Errorf("stage %q cannot hold a persona (persona-eligible stages: in_progress, in_review — backlog is the claim pool, done is terminal, pending_input keeps its suspended worker)", stage)
		}
		if string(praw) == "null" {
			return nil, fmt.Errorf("stage %q must not map to null — omit the stage key to inherit", stage)
		}
		var p PersonaConfig
		if err := json.Unmarshal(praw, &p); err != nil {
			return nil, fmt.Errorf("stage %q: %v", stage, err)
		}
		if strings.TrimSpace(p.RoleID) == "" {
			return nil, fmt.Errorf("stage %q persona requires a non-empty role reference (roleId — the id or slug of an available agent role)", stage)
		}
		if len(p.RoleID) > PersonaRoleIDMax {
			return nil, fmt.Errorf("stage %q persona role reference exceeds %d characters", stage, PersonaRoleIDMax)
		}
		out[stage] = p
	}
	return out, nil
}

// DecodeBoardConfigOverride turns a raw key/value map (adapter body or
// storage rows) into the typed partial override. Every pair must pass
// ValidateBoardConfigKey — unknown keys and malformed values reject;
// nothing outside the vocabulary can ride this seam into storage.
func DecodeBoardConfigOverride(m map[string]json.RawMessage) (*BoardConfigOverride, error) {
	out := &BoardConfigOverride{}
	for key, raw := range m {
		if err := ValidateBoardConfigKey(key, raw); err != nil {
			return nil, err
		}
		switch key {
		case ConfigKeyReviewColumn:
			var v string
			_ = json.Unmarshal(raw, &v)
			out.ReviewColumn = &v
		case ConfigKeyAutoAssignReviewer:
			var v bool
			_ = json.Unmarshal(raw, &v)
			out.AutoAssignReviewer = &v
		case ConfigKeyDefaultWorkerSlots:
			var v int
			_ = json.Unmarshal(raw, &v)
			out.DefaultWorkerSlots = &v
		case ConfigKeyStagePersonas:
			m, err := decodePersonaMap(raw)
			if err != nil {
				return nil, &GuardError{Code: GuardInput, Detail: fmt.Sprintf("%s: %s", key, err.Error())}
			}
			out.StagePersonas = &m
		}
	}
	return out, nil
}

// ValidateBoardConfigOverride re-checks a TYPED override (the service
// belt under the adapter's decode — a caller that bypassed the adapter
// still cannot land a malformed value).
func ValidateBoardConfigOverride(o *BoardConfigOverride) error {
	if o == nil {
		return nil
	}
	if o.ReviewColumn != nil {
		if err := ValidateBoardConfigKey(ConfigKeyReviewColumn, mustMarshalJSON(*o.ReviewColumn)); err != nil {
			return err
		}
	}
	if o.AutoAssignReviewer != nil {
		if err := ValidateBoardConfigKey(ConfigKeyAutoAssignReviewer, mustMarshalJSON(*o.AutoAssignReviewer)); err != nil {
			return err
		}
	}
	if o.DefaultWorkerSlots != nil {
		if err := ValidateBoardConfigKey(ConfigKeyDefaultWorkerSlots, mustMarshalJSON(*o.DefaultWorkerSlots)); err != nil {
			return err
		}
	}
	if o.StagePersonas != nil {
		if err := ValidateBoardConfigKey(ConfigKeyStagePersonas, mustMarshalJSON(*o.StagePersonas)); err != nil {
			return err
		}
	}
	return nil
}

// mustMarshalJSON marshals a value that cannot fail (string/bool/int).
// The impossible branch PANICS rather than returning "null": a future
// reusable caller must never silently persist a null (null is never a
// config value — an unset key is absent, and a silent null would flip
// inheritance semantics at the storage floor).
func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("board config: mustMarshalJSON(%T): unreachable for string/bool/int: %v", v, err))
	}
	return b
}

// configRow is one storage row (key + jsonb value), the
// instance_settings shape.
type configRow struct {
	Key   string
	Value json.RawMessage
}

// StorageDriftError marks a stored config row that failed validation
// on READ — server-side data drift (e.g. manual SQL past the write
// belts), never a client error: a GET must not render 400 for the
// server's own corrupt row, so this class rides the generic 500 arm.
type StorageDriftError struct {
	Scope  string // which map drifted ("user defaults" / "project overrides")
	Detail string
}

func (e *StorageDriftError) Error() string {
	return fmt.Sprintf("board config storage drift (%s): %s", e.Scope, e.Detail)
}

// decodeBoardConfigRows decodes stored rows fail-closed: any
// validation failure is drift, not caller input.
func decodeBoardConfigRows(scope string, raw map[string]json.RawMessage) (*BoardConfigOverride, error) {
	o, err := DecodeBoardConfigOverride(raw)
	if err != nil {
		return nil, &StorageDriftError{Scope: scope, Detail: err.Error()}
	}
	return o, nil
}

// rows encodes the partial override to storage rows: set keys only —
// unset keys store nothing, which IS the inheritance mechanism.
func (o *BoardConfigOverride) rows() []configRow {
	if o == nil {
		return nil
	}
	var out []configRow
	if o.ReviewColumn != nil {
		out = append(out, configRow{Key: ConfigKeyReviewColumn, Value: mustMarshalJSON(*o.ReviewColumn)})
	}
	if o.AutoAssignReviewer != nil {
		out = append(out, configRow{Key: ConfigKeyAutoAssignReviewer, Value: mustMarshalJSON(*o.AutoAssignReviewer)})
	}
	if o.DefaultWorkerSlots != nil {
		out = append(out, configRow{Key: ConfigKeyDefaultWorkerSlots, Value: mustMarshalJSON(*o.DefaultWorkerSlots)})
	}
	if o.StagePersonas != nil {
		out = append(out, configRow{Key: ConfigKeyStagePersonas, Value: mustMarshalJSON(*o.StagePersonas)})
	}
	return out
}

// --- Store readers -------------------------------------------------------------

// loadBoardConfigDefaults reads one user's board-wide defaults map
// (empty map when nothing was ever written — the code defaults cover
// it at merge time).
func (st *Store) loadBoardConfigDefaults(ctx context.Context, q Queryer, userID string) (map[string]json.RawMessage, error) {
	rows, err := q.Query(ctx, `SELECT key, value::text FROM board_config_defaults WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// loadBoardProjectConfigOverrides reads one project's override map
// (empty map = no overrides, full inheritance).
func (st *Store) loadBoardProjectConfigOverrides(ctx context.Context, q Queryer, projectID string) (map[string]json.RawMessage, error) {
	rows, err := q.Query(ctx, `SELECT key, value::text FROM board_project_config_overrides WHERE project_id=$1`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// replaceBoardConfigDefaults REPLACE-writes one user's board-wide
// defaults map in one transaction (absent keys revert to the code
// defaults). A transaction-scoped advisory lock on the scope key
// serializes concurrent REPLACE writers (DELETE+INSERT under
// concurrency otherwise races on the PK or interleaves disjoint keys
// into a union — last-locked writer wins the WHOLE map, which is the
// replace contract).
func (st *Store) replaceBoardConfigDefaults(ctx context.Context, userID string, rows []configRow) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "board_config_defaults:"+userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, $2, $3::jsonb)`,
			userID, r.Key, string(r.Value)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// replaceBoardProjectConfigOverrides REPLACE-writes one project's
// override map in one transaction (absent keys revert to inheritance).
// The same per-scope advisory-xact-lock discipline as the user map:
// concurrent writers serialize, last-locked writer wins the WHOLE map.
func (st *Store) replaceBoardProjectConfigOverrides(ctx context.Context, projectID string, rows []configRow) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "board_project_config_overrides:"+projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM board_project_config_overrides WHERE project_id=$1`, projectID); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, $2, $3::jsonb)`,
			projectID, r.Key, string(r.Value)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// --- Service seam ----------------------------------------------------------------

// rolesForUserErr resolves the available agent-role vocabulary through
// the installed directory (nil directory or nil user → nil roles —
// the role-blind posture). Its callers must distinguish a directory
// OUTAGE (a plain error — 500-class on the set path, retryable once
// the role service is reachable; a degrade-to-persona-less warn on
// the spawn path) from an unresolvable reference (GuardInput — 400,
// the caller's payload names a role the vocabulary never carried).
func (s *Service) rolesForUserErr(ctx context.Context, userID string) ([]AgentRoleRef, error) {
	if s.roles == nil || userID == "" {
		return nil, nil
	}
	return s.roles.AgentRolesForUser(ctx, userID)
}

// validatePersonaRoleRefs is the SET-path belt (board_set_config's
// role validation): every persona's role reference must resolve
// against the available set (id or slug). Error classes are kept
// DISTINCT — the caller's remedy differs: a role-blind deployment
// (no directory installed) refuses the payload (GuardInput: this
// deployment can never honor a reference); a directory read failure
// is a plain error (500-class: retry once the role service is
// reachable — the payload may be perfectly valid); an unresolvable
// reference is GuardInput (400: name a role the vocabulary carries).
func (s *Service) validatePersonaRoleRefs(ctx context.Context, userID string, in *BoardConfigOverride) error {
	if in == nil || in.StagePersonas == nil {
		return nil
	}
	var refs []string
	for _, p := range *in.StagePersonas {
		if p.RoleID != "" {
			refs = append(refs, p.RoleID)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if s.roles == nil {
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf(
			"stagePersonas carries role references (%s) but this deployment has no agent-role directory — install the role service, then set stagePersonas",
			strings.Join(refs, ", "))}
	}
	roles, err := s.rolesForUserErr(ctx, userID)
	if err != nil {
		return fmt.Errorf("agent-role directory read failed — the set was not written; retry once the role service is reachable: %w", err)
	}
	vocab := make([]string, 0, len(roles))
	for i := range roles {
		vocab = append(vocab, roles[i].Slug)
	}
	known := strings.Join(vocab, ", ")
	if known == "" {
		known = "none are available to this owner"
	}
	for _, ref := range refs {
		if ResolveAgentRoleRef(ref, roles) == nil {
			return &GuardError{Code: GuardInput, Detail: fmt.Sprintf(
				"stagePersonas role reference %q does not match an available agent role (available: %s)", ref, known)}
		}
	}
	return nil
}

// userBoardConfigOverride reads the acting user's stored board-wide
// defaults and decodes them (fail-closed on drift: an unknown key or
// malformed value surfacing from storage is an error, never a
// silently ignored value — under the role-only ruling a stored
// non-role persona shape is drift, full stop: no decode, no
// migration, the 500-class StorageDriftError names it).
func (s *Service) userBoardConfigOverride(ctx context.Context, userID string) (*BoardConfigOverride, error) {
	raw, err := s.store.loadBoardConfigDefaults(ctx, s.q(), userID)
	if err != nil {
		return nil, err
	}
	return decodeBoardConfigRows("user defaults", raw)
}

// GetUserBoardConfig renders the acting user's effective board-wide
// defaults: the code defaults overlaid with their stored user map
// (R7: boards are user-level; "board-wide" = the user's boards, NOT
// the instance — the defaults row is scoped to the owner). Guards once
// here like every other config seam: a user-scoped read needs a real
// acting user (uniform with the project read and both writes — an
// empty actor never silently reads the code defaults as if they were
// a resolved user's map).
func (s *Service) GetUserBoardConfig(ctx context.Context, actor Actor) (*BoardConfig, error) {
	if actor.UserID == "" {
		return nil, &GuardError{Code: GuardAuthz, Detail: "board config defaults are user-scoped — an authenticated acting user is required"}
	}
	o, err := s.userBoardConfigOverride(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	eff := MergeBoardConfig(DefaultBoardConfig(), o)
	return &eff, nil
}

// SetUserBoardConfig REPLACE-writes the acting user's board-wide
// defaults map (send the full set; absent keys revert to the code
// defaults). Guards live here (design 0067:37 — once, in the
// service): a user-scoped action needs a real acting user; the actor
// rides the seam stamped by the adapter (every mutating board
// action's discipline) and lands in the write's log line —
// board_events is the project-scoped audit log and carries no config
// vocabulary (see migration 000041 for the explicit deferral ruling).
func (s *Service) SetUserBoardConfig(ctx context.Context, actor Actor, in *BoardConfigOverride) (*BoardConfig, error) {
	if actor.UserID == "" {
		return nil, &GuardError{Code: GuardAuthz, Detail: "board config defaults are user-scoped — an authenticated acting user is required"}
	}
	if err := ValidateBoardConfigOverride(in); err != nil {
		return nil, err
	}
	if err := s.validatePersonaRoleRefs(ctx, actor.UserID, in); err != nil {
		return nil, err
	}
	if err := s.store.replaceBoardConfigDefaults(ctx, actor.UserID, in.rows()); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: user config defaults replaced",
		"user_id", actor.UserID, "actor.kind", actor.Kind, "actor.surface", actor.Surface,
		"keys", len(in.rows()))
	return s.GetUserBoardConfig(ctx, actor)
}

// decodeProjectOverrides loads + decodes one project's override map
// (fail-closed on drift, the user-tier read's discipline). The
// effectiveConfig seam and both config reads share it.
func (s *Service) decodeProjectOverrides(ctx context.Context, projectID string) (*BoardConfigOverride, error) {
	raw, err := s.store.loadBoardProjectConfigOverrides(ctx, s.q(), projectID)
	if err != nil {
		return nil, err
	}
	return decodeBoardConfigRows("project overrides", raw)
}

// buildBoardConfigView renders the source-annotated view from the two
// RAW override maps (either may be nil): effective row, per-key
// sources, per-stage persona provenance. Pure — the service seam only
// loads the maps, so the annotation logic is pinnable without a DB.
func buildBoardConfigView(scope, projectID string, u, p *BoardConfigOverride) BoardConfigView {
	eff := MergeBoardConfig(MergeBoardConfig(DefaultBoardConfig(), u), p)
	has := func(o *BoardConfigOverride, key string) bool {
		if o == nil {
			return false
		}
		switch key {
		case ConfigKeyReviewColumn:
			return o.ReviewColumn != nil
		case ConfigKeyAutoAssignReviewer:
			return o.AutoAssignReviewer != nil
		case ConfigKeyDefaultWorkerSlots:
			return o.DefaultWorkerSlots != nil
		case ConfigKeyStagePersonas:
			return o.StagePersonas != nil
		}
		return false
	}
	sources := make(map[string]string, len(BoardConfigKeys()))
	for _, key := range BoardConfigKeys() {
		switch {
		case has(p, key):
			sources[key] = ConfigSourceProjectOverride
		case has(u, key):
			sources[key] = ConfigSourceUserDefault
		default:
			sources[key] = ConfigSourceCodeDefault
		}
	}
	personas := make(map[string]BoardPersonaView, len(personaEligibleStages))
	for stage := range personaEligibleStages {
		v := BoardPersonaView{Source: PersonaProvenanceForStage(u, p, eff, stage)}
		switch v.Source {
		case PersonaSourceProjectOverride:
			persona := (*p.StagePersonas)[stage]
			v.Persona = &persona
		case PersonaSourceUserDefault:
			persona := (*u.StagePersonas)[stage]
			v.Persona = &persona
		case PersonaSourceDefaultReviewer:
			persona := DefaultReviewerPersona
			v.Persona = &persona
		}
		personas[stage] = v
	}
	out := BoardConfigView{
		Scope:     scope,
		Overrides: BoardConfigOverride{},
		Effective: eff,
		Sources:   sources,
		Personas:  personas,
	}
	if p != nil {
		out.ProjectID = projectID
		out.Overrides = *p
	} else if u != nil {
		out.Overrides = *u
	}
	return out
}

// GetBoardConfigView renders the source-annotated config view for
// EITHER tier: no project ref → the user tier (the board-wide
// defaults), a project ref → the project tier (the caller's workspace
// binding still applies on the agent surface — the project resolves
// through the same scoped read as GetProjectBoardConfig). The read
// perimeter is GetUserBoardConfig's own: a user-scoped read needs a
// real acting user.
func (s *Service) GetBoardConfigView(ctx context.Context, actor Actor, projectID, shortIDOrID string) (*BoardConfigView, error) {
	if actor.UserID == "" {
		return nil, &GuardError{Code: GuardAuthz, Detail: "board config is user-scoped — an authenticated acting user is required"}
	}
	u, err := s.userBoardConfigOverride(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	if pick(projectID, shortIDOrID) == "" {
		view := buildBoardConfigView("user", "", u, nil)
		return &view, nil
	}
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	// The #1747 workspace binding on the agent surfaces' project reads
	// (the board_list discipline: a foreign workspace's project is not
	// this pod's business); REST/external surfaces scope by ownership
	// alone.
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	ov, err := s.decodeProjectOverrides(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	view := buildBoardConfigView("project", p.ID, u, ov)
	return &view, nil
}

// GetProjectBoardConfig renders one project's config surfaces: the raw
// stored overrides plus the effective row (the acting user's
// board-wide defaults overlaid with the project's overrides —
// read-time merge, never materialized). The read perimeter is the
// service's own (design 0067:37: guards once, here): the project
// resolves scoped to the acting user, so a foreign project renders
// the scoped 404 (never disclosed) regardless of which adapter calls.
func (s *Service) GetProjectBoardConfig(ctx context.Context, actor Actor, projectID, shortIDOrID string) (*ProjectBoardConfig, error) {
	if actor.UserID == "" {
		return nil, &GuardError{Code: GuardAuthz, Detail: "board project config is user-scoped — an authenticated acting user is required"}
	}
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	// Torn-read tolerance (deliberate): the user map and the project
	// map load in two statements on separate pool connections, so a
	// write landing between them can render an effective row that
	// never existed at any single instant. Each map is individually
	// consistent (per-scope advisory locks), every rendered value was
	// really stored, and a cross-scope snapshot transaction would
	// couple the two independent lock domains for a cosmetic
	// guarantee — tolerated and pinned by the concurrency test's
	// cross-parity arms.
	u, err := s.userBoardConfigOverride(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	ov, err := s.decodeProjectOverrides(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return &ProjectBoardConfig{
		ProjectID: p.ID,
		Overrides: *ov,
		Effective: MergeBoardConfig(MergeBoardConfig(DefaultBoardConfig(), u), ov),
	}, nil
}

// SetProjectBoardConfig REPLACE-writes one project's override map
// (send the full set; absent keys revert to inheriting the user's
// board-wide default). Manager action: ownership (R7) + the #1747
// workspace binding on the agent surface, the UpdateProject
// discipline.
func (s *Service) SetProjectBoardConfig(ctx context.Context, actor Actor, projectID, shortIDOrID string, in *BoardConfigOverride) (*ProjectBoardConfig, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	if err := ValidateBoardConfigOverride(in); err != nil {
		return nil, err
	}
	if err := s.validatePersonaRoleRefs(ctx, p.UserID, in); err != nil {
		return nil, err
	}
	// defaultWorkerSlots is a USER-level key (it feeds CreateProject's
	// fallback): a project's slots are fixed at create, so a per-project
	// override would be inert yet render as "effective" — an
	// effective-map the engine does not honor. Rejected here rather
	// than displayed as a lie.
	if in != nil && in.DefaultWorkerSlots != nil {
		return nil, &GuardError{Code: GuardInput, Detail: "defaultWorkerSlots is a user-level key — set it in the board-wide defaults (PUT /api/v1/me/board/config); a project's worker slots are fixed at create"}
	}
	if err := s.store.replaceBoardProjectConfigOverrides(ctx, p.ID, in.rows()); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: project config overrides replaced",
		"project_id", p.ID, "actor.kind", actor.Kind, "actor.user_id", actor.UserID, "actor.surface", actor.Surface,
		"keys", len(in.rows()))
	return s.GetProjectBoardConfig(ctx, actor, p.ID, "")
}
