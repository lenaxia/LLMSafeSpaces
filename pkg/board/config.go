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

// PersonaNameMax and PersonaInstructionsMax bound one persona config's
// text fields (the #1837 P1b discipline: caps on every stored text
// field). The name rides log lines and event payloads; the
// instructions ride the spawned charter verbatim.
const (
	PersonaNameMax         = 64
	PersonaInstructionsMax = 4096
)

// PersonaConfig is ONE stage's configured agent persona (the
// stage-persona claim-then-spawn mechanism): when a task ENTERS a
// stage carrying one, the engine claims the row (existing lease +
// claim-token semantics) and spawns a fresh session whose charter
// renders this persona. Stage-generic by construction — any persona
// name, any persona-bearing stage; nothing is hardwired to a
// particular persona or stage.
type PersonaConfig struct {
	Name string `json:"name"`
	// Instructions is the persona's charter preamble (optional: an
	// explicit persona config may rely on the generic scaffold alone).
	Instructions string `json:"instructions,omitempty"`
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

// UserBoardConfig is the user tier's twin of ProjectBoardConfig: the
// raw stored defaults plus the effective merged row. The REST surface
// renders only the effective row (its contract, pinned by openapi);
// this surfaces read exists for the agent seam's source annotations
// (inherited vs override needs the RAW map, which the merged row
// cannot disclose — a user-set value equal to the code default is
// indistinguishable in the merge).
type UserBoardConfig struct {
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

// decodePersonaMap decodes + validates the stagePersonas value: a JSON
// object stage → {name, instructions?}. Every stage key must be
// persona-eligible (a stage a live claim can exist in), every persona
// name non-empty and bounded, instructions bounded, and no unknown
// fields inside a persona object (the strict-vocabulary discipline at
// the nested level; DisallowUnknownFields — an accidental
// "instructions"-typo must never ride silently into storage).
func decodePersonaMap(raw json.RawMessage) (map[string]PersonaConfig, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("must be an object mapping stage → {name, instructions?}, got %s", string(raw))
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
		pdec := json.NewDecoder(bytes.NewReader(praw))
		pdec.DisallowUnknownFields()
		if err := pdec.Decode(&p); err != nil {
			return nil, fmt.Errorf("stage %q persona must be an object {name, instructions?}: %v", stage, err)
		}
		if p.Name == "" {
			return nil, fmt.Errorf("stage %q persona requires a non-empty name", stage)
		}
		if len(p.Name) > PersonaNameMax {
			return nil, fmt.Errorf("stage %q persona name exceeds %d characters", stage, PersonaNameMax)
		}
		if len(p.Instructions) > PersonaInstructionsMax {
			return nil, fmt.Errorf("stage %q persona instructions exceed %d characters", stage, PersonaInstructionsMax)
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

// userBoardConfigOverride reads the acting user's stored board-wide
// defaults and decodes them (fail-closed on drift: an unknown key or
// malformed value surfacing from storage is an error, never a
// silently ignored value).
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
	if err := s.store.replaceBoardConfigDefaults(ctx, actor.UserID, in.rows()); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: user config defaults replaced",
		"user_id", actor.UserID, "actor.kind", actor.Kind, "actor.surface", actor.Surface,
		"keys", len(in.rows()))
	return s.GetUserBoardConfig(ctx, actor)
}

// GetUserBoardConfigSurfaces renders the user tier's TWO surfaces —
// the raw stored defaults map and the effective merged row — the
// user-tier twin of GetProjectBoardConfig. Same guards as the
// effective-only read (the read perimeter does not widen because the
// raw map rides along: still the acting user's own row).
func (s *Service) GetUserBoardConfigSurfaces(ctx context.Context, actor Actor) (*UserBoardConfig, error) {
	if actor.UserID == "" {
		return nil, &GuardError{Code: GuardAuthz, Detail: "board config defaults are user-scoped — an authenticated acting user is required"}
	}
	o, err := s.userBoardConfigOverride(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	return &UserBoardConfig{
		Overrides: *o,
		Effective: MergeBoardConfig(DefaultBoardConfig(), o),
	}, nil
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
	rawOv, err := s.store.loadBoardProjectConfigOverrides(ctx, s.q(), p.ID)
	if err != nil {
		return nil, err
	}
	ov, err := decodeBoardConfigRows("project overrides", rawOv)
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
	rawOv, err := s.store.loadBoardProjectConfigOverrides(ctx, s.q(), p.ID)
	if err != nil {
		return nil, err
	}
	ov, err := decodeBoardConfigRows("project overrides", rawOv)
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
