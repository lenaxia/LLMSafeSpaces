// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// config.go — Task Board Config (#1832 item 6, part 1): the storage +
// read-time merge behind the board-wide config system.
//
// Model: a GLOBAL defaults map (board_config_defaults) plus an optional
// PER-PROJECT override map (board_project_config_overrides), merged at
// READ time — an unset key in the override map inherits the global
// default, and a global key that was never written inherits the code
// default. Storage is per-key jsonb rows (the instance_settings shape),
// so per-project stays UI-only later and the key set can grow without a
// row-shape change (grow = key vocabulary + CHECK constraint migration).
//
// Initial keys: reviewColumn (off|manual|auto, default manual — PR-B1/B2
// own any review automation; this stores the preference only),
// autoAssignReviewer (bool, default false), defaultWorkerSlots (int > 0,
// default 2 — the platform's existing worker-slots default).
//
// Writes are REPLACE-semantics (the tags-REPLACE discipline): a PUT
// carries the full map; absent keys stop being overridden and revert to
// inheritance. Validation is layered: the adapter's key-gate decodes
// and rejects unknown keys/malformed values (400), the service re-checks
// the typed override (belt), and the tables' CHECK constraints are the
// physical floor. Unknown keys are fail-closed on read too: a row that
// somehow drifted past the belts surfaces as an error, never a silently
// ignored value.

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
// create-project fallback, the 000036 column default, and the config
// key's code default — one constant now, three call sites before.
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

// DefaultBoardConfig returns the code defaults (#1832 item 6): manual
// review column, no auto reviewer assignment, worker slots 2.
func DefaultBoardConfig() BoardConfig {
	return BoardConfig{
		ReviewColumn:       ReviewColumnManual,
		AutoAssignReviewer: false,
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
// for defaultWorkerSlots). Violations render GuardInput (the 400 class)
// — unknown keys name the known set, malformed values name the key.
func ValidateBoardConfigKey(key string, raw json.RawMessage) error {
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
func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// configRow is one storage row (key + jsonb value), the
// instance_settings shape.
type configRow struct {
	Key   string
	Value json.RawMessage
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

// loadBoardConfigDefaults reads the global defaults map (empty map when
// nothing was ever written — the code defaults cover it at merge time).
func (st *Store) loadBoardConfigDefaults(ctx context.Context, q Queryer) (map[string]json.RawMessage, error) {
	rows, err := q.Query(ctx, `SELECT key, value::text FROM board_config_defaults`)
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

// replaceBoardConfigDefaults REPLACE-writes the global defaults map in
// one transaction (absent keys revert to the code defaults).
func (st *Store) replaceBoardConfigDefaults(ctx context.Context, rows []configRow) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM board_config_defaults`); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `
			INSERT INTO board_config_defaults (key, value) VALUES ($1, $2::jsonb)`,
			r.Key, string(r.Value)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// replaceBoardProjectConfigOverrides REPLACE-writes one project's
// override map in one transaction (absent keys revert to inheritance).
func (st *Store) replaceBoardProjectConfigOverrides(ctx context.Context, projectID string, rows []configRow) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
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

// globalBoardConfig reads the stored global defaults and decodes them
// (fail-closed on drift: an unknown key surfacing from storage is an
// error, never a silently ignored value).
func (s *Service) globalBoardConfigOverride(ctx context.Context) (*BoardConfigOverride, error) {
	raw, err := s.store.loadBoardConfigDefaults(ctx, s.q())
	if err != nil {
		return nil, err
	}
	return DecodeBoardConfigOverride(raw)
}

// GetGlobalBoardConfig renders the effective global defaults: the code
// defaults overlaid with the stored global map.
func (s *Service) GetGlobalBoardConfig(ctx context.Context) (*BoardConfig, error) {
	o, err := s.globalBoardConfigOverride(ctx)
	if err != nil {
		return nil, err
	}
	eff := MergeBoardConfig(DefaultBoardConfig(), o)
	return &eff, nil
}

// SetGlobalBoardConfig REPLACE-writes the global defaults map (send the
// full set; absent keys revert to the code defaults). The route is
// admin-guarded (instance-scope config, the /admin/settings
// discipline); the actor rides the seam stamped by the adapter (every
// mutating board action's discipline) and lands in the write's log
// line — board_events is the project-scoped audit log and carries no
// config vocabulary (see migration 000041).
func (s *Service) SetGlobalBoardConfig(ctx context.Context, actor Actor, in *BoardConfigOverride) (*BoardConfig, error) {
	if err := ValidateBoardConfigOverride(in); err != nil {
		return nil, err
	}
	if err := s.store.replaceBoardConfigDefaults(ctx, in.rows()); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: global config defaults replaced",
		"actor.kind", actor.Kind, "actor.user_id", actor.UserID, "actor.surface", actor.Surface,
		"keys", len(in.rows()))
	return s.GetGlobalBoardConfig(ctx)
}

// GetProjectBoardConfig renders one project's config surfaces: the raw
// stored overrides plus the effective row (global effective overlaid
// with the project's overrides — read-time merge, never materialized).
// The read perimeter is the adapter's (ownerProject, the GetProject
// discipline).
func (s *Service) GetProjectBoardConfig(ctx context.Context, projectID, shortIDOrID string) (*ProjectBoardConfig, error) {
	p, err := s.store.resolveProject(ctx, s.q(), "", pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	g, err := s.globalBoardConfigOverride(ctx)
	if err != nil {
		return nil, err
	}
	rawOv, err := s.store.loadBoardProjectConfigOverrides(ctx, s.q(), p.ID)
	if err != nil {
		return nil, err
	}
	ov, err := DecodeBoardConfigOverride(rawOv)
	if err != nil {
		return nil, err
	}
	return &ProjectBoardConfig{
		ProjectID: p.ID,
		Overrides: *ov,
		Effective: MergeBoardConfig(MergeBoardConfig(DefaultBoardConfig(), g), ov),
	}, nil
}

// SetProjectBoardConfig REPLACE-writes one project's override map
// (send the full set; absent keys revert to inheriting the global
// default). Manager action: ownership (R7) + the #1747 workspace
// binding on the agent surface, the UpdateProject discipline.
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
	if err := s.store.replaceBoardProjectConfigOverrides(ctx, p.ID, in.rows()); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "board: project config overrides replaced",
		"project_id", p.ID, "actor.kind", actor.Kind, "actor.user_id", actor.UserID, "actor.surface", actor.Surface,
		"keys", len(in.rows()))
	return s.GetProjectBoardConfig(ctx, p.ID, "")
}
