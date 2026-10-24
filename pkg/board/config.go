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

// BoardConfig is the effective merged configuration row (every key
// always present on the wire).
type BoardConfig struct {
	ReviewColumn       string `json:"reviewColumn"`
	AutoAssignReviewer bool   `json:"autoAssignReviewer"`
	DefaultWorkerSlots int    `json:"defaultWorkerSlots"`
}

// BoardConfigOverride is a PARTIAL config: nil fields are unset and
// inherit at merge time. On the wire an unset key is absent (omitempty),
// never null.
type BoardConfigOverride struct {
	ReviewColumn       *string `json:"reviewColumn,omitempty"`
	AutoAssignReviewer *bool   `json:"autoAssignReviewer,omitempty"`
	DefaultWorkerSlots *int    `json:"defaultWorkerSlots,omitempty"`
}

// ProjectBoardConfig renders one project's config surfaces in a single
// read: the raw stored overrides plus the effective merged row.
type ProjectBoardConfig struct {
	ProjectID string              `json:"project_id"`
	Overrides BoardConfigOverride `json:"overrides"`
	Effective BoardConfig         `json:"effective"`
}

// DefaultBoardConfig returns the code defaults (owner ruling
// 2026-10-24 — "flip defaults for review to on"): auto review column
// with auto reviewer assignment, worker slots 2. A stored explicit
// opt-out at either tier (user defaults, project overrides) still wins
// at the read-time merge — the flip changes only the inheritance floor.
func DefaultBoardConfig() BoardConfig {
	return BoardConfig{
		ReviewColumn:       ReviewColumnAuto,
		AutoAssignReviewer: true,
		DefaultWorkerSlots: DefaultBoardWorkerSlots,
	}
}

// BoardConfigKeys lists the known config keys (grow the vocabulary =
// add a key here, a struct field, and its validation).
func BoardConfigKeys() []string {
	return []string{ConfigKeyReviewColumn, ConfigKeyAutoAssignReviewer, ConfigKeyDefaultWorkerSlots}
}

// MergeBoardConfig overlays a partial override onto a base at read
// time: a set key wins, an unset (nil) key inherits the base value.
// nil/empty overlays render the base verbatim.
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
	default:
		return &GuardError{Code: GuardInput, Detail: fmt.Sprintf("unknown config key %q (known: reviewColumn, autoAssignReviewer, defaultWorkerSlots)", key)}
	}
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
