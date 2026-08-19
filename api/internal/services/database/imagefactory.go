// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// ImageFactoryStore is the data-access interface for the image factory
// (design/0046, design/0047). Methods are grouped: catalog (read),
// catalog admin (write), known failures, configs, builds.
//
// All methods take ctx and return domain types from
// api/internal/imagefactory. Handlers depend on this interface (via the
// *database.Service concrete type) and tests inject fakes.
type ImageFactoryStore interface {
	// ── Platform config (singleton row) ───────────────────────────────
	GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error)
	SetPlatformConfig(ctx context.Context, pc imagefactory.PlatformConfig) error

	// ── Bases ─────────────────────────────────────────────────────────
	ListBases(ctx context.Context) ([]imagefactory.Base, error)
	GetBase(ctx context.Context, name, version string) (imagefactory.Base, error)
	UpsertBase(ctx context.Context, b imagefactory.Base) error
	// SeedUpsertBase is the boot-seed upsert: unlike UpsertBase it never
	// clears other defaults and never overwrites an existing row's
	// is_default (#936).
	SeedUpsertBase(ctx context.Context, b imagefactory.Base) error
	DeleteBase(ctx context.Context, name, version string) error

	// ── Extensions ────────────────────────────────────────────────────
	ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error)
	GetExtension(ctx context.Context, id string) (imagefactory.Extension, error)
	PublishExtension(ctx context.Context, e imagefactory.Extension) error
	RetireExtension(ctx context.Context, id string) error
	SetExtensionReviewRequested(ctx context.Context, id string, v bool) error

	// ── Known failures ───────────────────────────────────────────────
	ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error)
	GetKnownFailure(ctx context.Context, selectionHash, baseName string) (imagefactory.KnownFailure, error)
	RecordKnownFailure(ctx context.Context, kf imagefactory.KnownFailure) error
	SetKnownFailureRetriable(ctx context.Context, selectionHash, baseName string, retriable bool) error
	DeleteKnownFailure(ctx context.Context, selectionHash, baseName string) error
	ListRejectedConfigsForFailure(ctx context.Context, selectionHash, baseName string) ([]imagefactory.Config, error)

	// ── Configs ──────────────────────────────────────────────────────
	CreateConfig(ctx context.Context, c *imagefactory.Config) error
	CreateConfigAndBuild(ctx context.Context, c *imagefactory.Config, b *imagefactory.Build) error
	GetConfig(ctx context.Context, id string) (imagefactory.Config, error)
	GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error)
	ListConfigs(ctx context.Context, scope imagefactory.ConfigScope, ownerID, orgID *string) ([]imagefactory.Config, error)
	ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error)
	SetConfigStatus(ctx context.Context, id string, status imagefactory.ConfigStatus) error
	DeleteConfig(ctx context.Context, id string) error
	RenameConfig(ctx context.Context, id, newName string) error
	// GetLaunchableConfigByHash returns a Ready config matching the hash
	// and scope/owner filter, together with the image_ref of its
	// successful build. Used by the workspace launch path to resolve a
	// user-selected config hash to a concrete, pre-built image. Returns
	// ErrNotFound if the config doesn't exist, isn't Ready, or has no
	// successful build (the normal "not launchable yet" case).
	GetLaunchableConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, string, error)

	// ── Builds ───────────────────────────────────────────────────────
	GetBuild(ctx context.Context, id string) (imagefactory.Build, error)
	GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error)
	GetBuildByGHRunID(ctx context.Context, ghRunID int64) (imagefactory.Build, error)
	CreateBuild(ctx context.Context, b *imagefactory.Build) error
	MarkBuildSucceeded(ctx context.Context, id, imageRef, digest string) error
	MarkBuildFailed(ctx context.Context, id, failureReason, explanation string) error
	TransitionBuildSucceeded(ctx context.Context, buildID, configID, imageRef, digest string) error
	TransitionBuildFailed(ctx context.Context, buildID, configID string, kf imagefactory.KnownFailure) error
}

// Assert *Service satisfies the interface at compile time.
var _ ImageFactoryStore = (*Service)(nil)

// ErrNotFound is the store's not-found sentinel, returned by GetX methods
// when no row matches. Callers use errors.Is(err, database.ErrNotFound).
// Exported so handler-layer code can distinguish 404 from 500 without
// fragile string matching.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned for unique-constraint violations (e.g. rename
// collides with an existing name in the same scope).
var ErrConflict = errors.New("conflict")

// ── Platform config ─────────────────────────────────────────────────────

func (s *Service) GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error) {
	var archs []string
	err := s.DB.QueryRowContext(ctx,
		`SELECT architectures FROM image_factory_platform_config WHERE id = 1`,
	).Scan(stringArrayScan(&archs))
	if err != nil {
		return imagefactory.PlatformConfig{}, fmt.Errorf("get platform config: %w", err)
	}
	return imagefactory.PlatformConfig{Architectures: archs}, nil
}

func (s *Service) SetPlatformConfig(ctx context.Context, pc imagefactory.PlatformConfig) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_platform_config SET architectures = $1, updated_at = now() WHERE id = 1`,
		pgStringArray(pc.Architectures),
	)
	if err != nil {
		return fmt.Errorf("set platform config: %w", err)
	}
	return nil
}

// ── Bases ───────────────────────────────────────────────────────────────

func (s *Service) ListBases(ctx context.Context) ([]imagefactory.Base, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, version, image, tag, digest, is_default FROM image_factory_bases ORDER BY name, version`)
	if err != nil {
		return nil, fmt.Errorf("list bases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []imagefactory.Base
	for rows.Next() {
		var b imagefactory.Base
		if err := rows.Scan(&b.Name, &b.Version, &b.Image, &b.Tag, &b.Digest, &b.IsDefault); err != nil {
			return nil, fmt.Errorf("list bases scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Service) GetBase(ctx context.Context, name, version string) (imagefactory.Base, error) {
	var b imagefactory.Base
	err := s.DB.QueryRowContext(ctx,
		`SELECT name, version, image, tag, digest, is_default FROM image_factory_bases WHERE name = $1 AND version = $2`,
		name, version,
	).Scan(&b.Name, &b.Version, &b.Image, &b.Tag, &b.Digest, &b.IsDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Base{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Base{}, fmt.Errorf("get base: %w", err)
	}
	return b, nil
}

func (s *Service) UpsertBase(ctx context.Context, b imagefactory.Base) error {
	// #936: a default=true upsert clears every other default in the same
	// transaction — moving the platform default is ONE call, and the
	// two-default state (pills resolve highest-sorted, the create form's
	// picker takes the first — visibly divergent) cannot persist.
	if b.IsDefault {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("upsert base: begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_factory_bases SET is_default = FALSE, updated_at = now() WHERE is_default`); err != nil {
			return fmt.Errorf("upsert base: clear prior defaults: %w", err)
		}
		if err := upsertBaseOn(ctx, tx, b); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("upsert base: commit: %w", err)
		}
		return nil
	}
	return upsertBaseOn(ctx, s.DB, b)
}

// SeedUpsertBase is the seed path (#936): the default applies only to
// rows the seed INSERTS — an existing base keeps its runtime is_default,
// so a boot-time seed never reverts an operator's default move.
func (s *Service) SeedUpsertBase(ctx context.Context, b imagefactory.Base) error {
	// The seed's is_default applies only when it INSERTS the row AND no
	// default exists yet — seed-after-delete (the operator removed the
	// default row; the runtime default may live on another base) must not
	// mint a second default. The partial unique index
	// (000025) enforces this structurally; the NOT EXISTS guard keeps the
	// intent readable at the store layer and gives a better error than
	// the index violation would.
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO image_factory_bases (name, version, image, tag, digest, is_default, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6 AND NOT EXISTS (SELECT 1 FROM image_factory_bases WHERE is_default), now())
		 ON CONFLICT (name, version) DO UPDATE SET
		     image = EXCLUDED.image,
		     tag = EXCLUDED.tag,
		     digest = EXCLUDED.digest,
		     updated_at = now()`,
		b.Name, b.Version, b.Image, b.Tag, b.Digest, b.IsDefault,
	)
	if err != nil {
		return fmt.Errorf("seed upsert base: %w", err)
	}
	return nil
}

// upsertBaseOn is the plain (non-seed) upsert on the given executor.
func upsertBaseOn(ctx context.Context, db queryExecer, b imagefactory.Base) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO image_factory_bases (name, version, image, tag, digest, is_default, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (name, version) DO UPDATE SET
		     image = EXCLUDED.image,
		     tag = EXCLUDED.tag,
		     digest = EXCLUDED.digest,
		     is_default = EXCLUDED.is_default,
		     updated_at = now()`,
		b.Name, b.Version, b.Image, b.Tag, b.Digest, b.IsDefault,
	)
	if err != nil {
		return fmt.Errorf("upsert base: %w", err)
	}
	return nil
}

// queryExecer abstracts *sql.DB vs *sql.Tx for shared upsert SQL.
type queryExecer interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func (s *Service) DeleteBase(ctx context.Context, name, version string) error {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM image_factory_bases WHERE name = $1 AND version = $2`, name, version)
	if err != nil {
		return fmt.Errorf("delete base: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Extensions ──────────────────────────────────────────────────────────

func (s *Service) ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error) {
	q := `SELECT id, type, value, file_spec, supported_bases, retired, review_requested, description
	      FROM image_factory_extensions`
	if !includeRetired {
		q += ` WHERE retired = false`
	}
	q += ` ORDER BY id`
	rows, err := s.DB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []imagefactory.Extension
	for rows.Next() {
		e, err := scanExtension(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) GetExtension(ctx context.Context, id string) (imagefactory.Extension, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, type, value, file_spec, supported_bases, retired, review_requested, description
		 FROM image_factory_extensions WHERE id = $1`, id)
	e, err := scanExtension(row)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Extension{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Extension{}, fmt.Errorf("get extension: %w", err)
	}
	return e, nil
}

func (s *Service) PublishExtension(ctx context.Context, e imagefactory.Extension) error {
	var fileSpecJSON interface{}
	if e.FileSpec != nil {
		b, err := json.Marshal(e.FileSpec)
		if err != nil {
			return fmt.Errorf("publish extension: marshal file_spec: %w", err)
		}
		fileSpecJSON = b
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO image_factory_extensions (id, type, value, file_spec, supported_bases, retired, review_requested, description)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE SET
		     retired = false,
		     review_requested = EXCLUDED.review_requested,
		     description = EXCLUDED.description,
		     updated_at = now()`,
		e.ID, string(e.Type), e.Value, fileSpecJSON, pgStringArray(e.SupportedBases),
		e.Retired, e.ReviewRequested, e.Description,
	)
	if err != nil {
		return fmt.Errorf("publish extension: %w", err)
	}
	return nil
}

func (s *Service) RetireExtension(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_extensions SET retired = true, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("retire extension: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetExtensionReviewRequested(ctx context.Context, id string, v bool) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_extensions SET review_requested = $1, updated_at = now() WHERE id = $2`, v, id)
	if err != nil {
		return fmt.Errorf("set extension review_requested: %w", err)
	}
	return nil
}

// ── Known failures ──────────────────────────────────────────────────────

func (s *Service) ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT selection_hash, selection, base_name, explanation, failure_reason, detected_at, retriable
		 FROM image_factory_known_failures ORDER BY detected_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list known failures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []imagefactory.KnownFailure
	for rows.Next() {
		var kf imagefactory.KnownFailure
		kfSel := (*stringArray)(&kf.Selection)
		if err := rows.Scan(&kf.SelectionHash, kfSel, &kf.BaseName,
			&kf.Explanation, &kf.FailureReason, &kf.DetectedAt, &kf.Retriable); err != nil {
			return nil, fmt.Errorf("list known failures scan: %w", err)
		}
		out = append(out, kf)
	}
	return out, rows.Err()
}

func (s *Service) GetKnownFailure(ctx context.Context, selectionHash, baseName string) (imagefactory.KnownFailure, error) {
	var kf imagefactory.KnownFailure
	err := s.DB.QueryRowContext(ctx,
		`SELECT selection_hash, selection, base_name, explanation, failure_reason, detected_at, retriable
		 FROM image_factory_known_failures WHERE selection_hash = $1 AND base_name = $2`,
		selectionHash, baseName,
	).Scan(&kf.SelectionHash, (*stringArray)(&kf.Selection), &kf.BaseName,
		&kf.Explanation, &kf.FailureReason, &kf.DetectedAt, &kf.Retriable)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.KnownFailure{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.KnownFailure{}, fmt.Errorf("get known failure: %w", err)
	}
	return kf, nil
}

func (s *Service) RecordKnownFailure(ctx context.Context, kf imagefactory.KnownFailure) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO image_factory_known_failures (selection_hash, selection, base_name, explanation, failure_reason, detected_at, retriable)
		 VALUES ($1, $2, $3, $4, $5, now(), $6)
		 ON CONFLICT (selection_hash, base_name) DO UPDATE SET
		     selection = EXCLUDED.selection,
		     explanation = EXCLUDED.explanation,
		     failure_reason = EXCLUDED.failure_reason,
		     detected_at = now(),
		     retriable = EXCLUDED.retriable`,
		kf.SelectionHash, pgStringArray(kf.Selection), kf.BaseName, kf.Explanation, kf.FailureReason, kf.Retriable,
	)
	if err != nil {
		return fmt.Errorf("record known failure: %w", err)
	}
	return nil
}

func (s *Service) SetKnownFailureRetriable(ctx context.Context, selectionHash, baseName string, retriable bool) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_known_failures SET retriable = $1 WHERE selection_hash = $2 AND base_name = $3`,
		retriable, selectionHash, baseName)
	if err != nil {
		return fmt.Errorf("set known failure retriable: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteKnownFailure(ctx context.Context, selectionHash, baseName string) error {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM image_factory_known_failures WHERE selection_hash = $1 AND base_name = $2`,
		selectionHash, baseName)
	if err != nil {
		return fmt.Errorf("delete known failure: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) ListRejectedConfigsForFailure(ctx context.Context, selectionHash, baseName string) ([]imagefactory.Config, error) {
	// selection_hash is the config.hash (same preimage); base_name is the
	// config.base_name. Rejected configs matching both are the un-block ->
	// rebuild target set.
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+configColumns+`
		 FROM image_factory_configs
		 WHERE hash = $1 AND base_name = $2 AND status = 'rejected'`,
		selectionHash, baseName)
	if err != nil {
		return nil, fmt.Errorf("list rejected configs for failure: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanConfigs(rows)
}

// ── Configs ─────────────────────────────────────────────────────────────

const configColumns = `id, hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, org_id, status`

// CreateConfigAndBuild inserts both rows in a single transaction so a
// failure in either insert rolls back the other — no orphaned config at
// 'building' with no build, no build row with no config. The handler
// calls this after a successful dispatch (design/0046 #17).
func (s *Service) CreateConfigAndBuild(ctx context.Context, c *imagefactory.Config, b *imagefactory.Build) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create config+build: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rvJSONCfg, err := json.Marshal(c.ResolvedValues)
	if err != nil {
		return fmt.Errorf("create config+build: marshal config resolved_values: %w", err)
	}
	var ownerID, orgID interface{}
	if c.OwnerID != nil {
		ownerID = *c.OwnerID
	}
	if c.OrgID != nil {
		orgID = *c.OrgID
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, org_id, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		c.Hash, c.Name, pgStringArray(c.Selection), rvJSONCfg, c.BaseName, c.BaseVersion,
		string(c.Scope), ownerID, orgID, string(c.Status),
	).Scan(&c.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create config+build: insert config: %w", err)
	}

	rvJSONBuild, err := json.Marshal(b.ResolvedValues)
	if err != nil {
		return fmt.Errorf("create config+build: marshal build resolved_values: %w", err)
	}
	var triggeredBy interface{}
	if b.TriggeredBy != nil {
		triggeredBy = *b.TriggeredBy
	}
	b.ConfigID = c.ID
	var buildOrgID interface{}
	if b.OrgID != nil {
		buildOrgID = *b.OrgID
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO image_factory_builds
		     (id, config_id, hash, base_name, base_version, resolved_values, architectures,
		      status, gh_run_id, callback_token, triggered_by, scope, org_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		b.ID, b.ConfigID, b.Hash, b.BaseName, b.BaseVersion, rvJSONBuild, pgStringArray(b.Architectures),
		string(b.Status), b.GHRunID, b.CallbackToken, triggeredBy, string(b.Scope), buildOrgID,
	)
	if err != nil {
		return fmt.Errorf("create config+build: insert build: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create config+build: commit: %w", err)
	}
	return nil
}

func (s *Service) CreateConfig(ctx context.Context, c *imagefactory.Config) error {
	rvJSON, err := json.Marshal(c.ResolvedValues)
	if err != nil {
		return fmt.Errorf("create config: marshal resolved_values: %w", err)
	}
	var ownerID, orgID interface{}
	if c.OwnerID != nil {
		ownerID = *c.OwnerID
	}
	if c.OrgID != nil {
		orgID = *c.OrgID
	}
	err = s.DB.QueryRowContext(ctx,
		`INSERT INTO image_factory_configs (hash, name, selection, resolved_values, base_name, base_version, scope, owner_id, org_id, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		c.Hash, c.Name, pgStringArray(c.Selection), rvJSON, c.BaseName, c.BaseVersion,
		string(c.Scope), ownerID, orgID, string(c.Status),
	).Scan(&c.ID)
	if err != nil {
		// #936: scoped-name uniqueness violation maps to the typed
		// conflict so the handler returns 409 instead of an opaque 500.
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create config: %w", err)
	}
	return nil
}

func (s *Service) GetConfig(ctx context.Context, id string) (imagefactory.Config, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+configColumns+` FROM image_factory_configs WHERE id = $1`, id)
	c, err := scanConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Config{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Config{}, fmt.Errorf("get config: %w", err)
	}
	return c, nil
}

func (s *Service) GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error) {
	// Friendly-name scope: lookup is by (hash, scope, owner/org). The first
	// match wins. Used for decode + dedup-on-save checks.
	q := `SELECT ` + configColumns + ` FROM image_factory_configs WHERE hash = $1 AND scope = $2`
	args := []interface{}{hash, string(scope)}
	if scope == imagefactory.ScopeMember && ownerID != nil {
		q += ` AND owner_id = $3`
		args = append(args, *ownerID)
	} else if scope == imagefactory.ScopeOrg && orgID != nil {
		q += ` AND org_id = $3`
		args = append(args, *orgID)
	}
	q += ` LIMIT 1`
	row := s.DB.QueryRowContext(ctx, q, args...)
	c, err := scanConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Config{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Config{}, fmt.Errorf("get config by hash: %w", err)
	}
	return c, nil
}

// GetLaunchableConfigByHash implements ImageFactoryStore.GetLaunchableConfigByHash.
// It joins image_factory_configs with its successful build to return both the
// config and the image_ref in one round-trip, and enforces the Ready + scope
// constraints needed by the workspace launch path. The query filters:
//   - config.status = 'ready' (design/0046 #15 — only Ready configs are launchable)
//   - scope/owner/org match (authorization: caller must own the config)
//   - a joined build row with status='succeeded' AND image_ref <> ” exists
//
// All selected columns are qualified with `c.` (or `b.`) because config and
// build tables share column names (hash, base_name, base_version, etc.) — an
// unqualified SELECT would be ambiguous and fail at query time.
//
// The build's image_ref is what the controller's runtime_resolver will use
// verbatim as the pod image (any '/'-containing runtime value is a passthrough).
func (s *Service) GetLaunchableConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, string, error) {
	q := `SELECT c.id, c.hash, c.name, c.selection, c.resolved_values,
	             c.base_name, c.base_version, c.scope, c.owner_id, c.org_id, c.status,
	             b.image_ref
	      FROM image_factory_configs c
	      JOIN image_factory_builds b
	        ON b.hash = c.hash AND b.base_version = c.base_version
	           AND b.status = 'succeeded' AND b.image_ref <> ''
	      WHERE c.hash = $1 AND c.status = 'ready' AND c.scope = $2`
	args := []interface{}{hash, string(scope)}
	if scope == imagefactory.ScopeMember && ownerID != nil {
		if _, err := uuid.Parse(*ownerID); err != nil {
			return imagefactory.Config{}, "", ErrNotFound
		}
		q += ` AND c.owner_id = $3`
		args = append(args, *ownerID)
	} else if scope == imagefactory.ScopeOrg && orgID != nil {
		if _, err := uuid.Parse(*orgID); err != nil {
			return imagefactory.Config{}, "", ErrNotFound
		}
		q += ` AND c.org_id = $3`
		args = append(args, *orgID)
	}
	q += ` LIMIT 1`
	var imageRef string
	row := s.DB.QueryRowContext(ctx, q, args...)
	c, err := scanConfigWithImageRef(row, &imageRef)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Config{}, "", ErrNotFound
	}
	if err != nil {
		return imagefactory.Config{}, "", fmt.Errorf("get launchable config by hash: %w", err)
	}
	return c, imageRef, nil
}

// scanConfigWithImageRef scans a (config, image_ref) pair produced by the
// GetLaunchableConfigByHash join. Mirrors scanConfig exactly for the config
// columns (pq.Array for selection, json for resolved_values), then reads the
// extra b.image_ref column into *out.
func scanConfigWithImageRef(sc rowScanner, out *string) (imagefactory.Config, error) {
	var c imagefactory.Config
	var rvRaw []byte
	var scopeStr, statusStr string
	var ownerID, orgID sql.NullString
	sel := (*stringArray)(&c.Selection)
	if err := sc.Scan(&c.ID, &c.Hash, &c.Name, sel, &rvRaw,
		&c.BaseName, &c.BaseVersion, &scopeStr, &ownerID, &orgID, &statusStr,
		out); err != nil {
		return imagefactory.Config{}, err
	}
	c.Scope = imagefactory.ConfigScope(scopeStr)
	c.Status = imagefactory.ConfigStatus(statusStr)
	if ownerID.Valid {
		v := ownerID.String
		c.OwnerID = &v
	}
	if orgID.Valid {
		v := orgID.String
		c.OrgID = &v
	}
	if err := json.Unmarshal(rvRaw, &c.ResolvedValues); err != nil {
		return imagefactory.Config{}, fmt.Errorf("scan config with image ref: unmarshal resolved_values: %w", err)
	}
	return c, nil
}

func (s *Service) ListConfigs(ctx context.Context, scope imagefactory.ConfigScope, ownerID, orgID *string) ([]imagefactory.Config, error) {
	q := `SELECT ` + configColumns + ` FROM image_factory_configs WHERE scope = $1`
	args := []interface{}{string(scope)}
	if scope == imagefactory.ScopeMember && ownerID != nil {
		q += ` AND owner_id = $2`
		args = append(args, *ownerID)
	} else if scope == imagefactory.ScopeOrg && orgID != nil {
		q += ` AND org_id = $2`
		args = append(args, *orgID)
	}
	q += ` ORDER BY name`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list configs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanConfigs(rows)
}

// ListVisibleConfigs returns the configs a member can see: their own
// member-scope, their org's org-scope (if any), plus platform-scope.
func (s *Service) ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error) {
	var ownerVal, orgVal interface{}
	if ownerID != nil {
		ownerVal = *ownerID
	}
	if orgID != nil {
		orgVal = *orgID
	}
	q := `SELECT ` + configColumns + ` FROM image_factory_configs
	      WHERE scope = 'platform'
	         OR (scope = 'member' AND owner_id = $1)
	         OR (scope = 'org' AND org_id = $2)
	      ORDER BY scope, name`
	rows, err := s.DB.QueryContext(ctx, q, ownerVal, orgVal)
	if err != nil {
		return nil, fmt.Errorf("list visible configs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanConfigs(rows)
}

func (s *Service) SetConfigStatus(ctx context.Context, id string, status imagefactory.ConfigStatus) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_configs SET status = $1, updated_at = now() WHERE id = $2`,
		string(status), id)
	if err != nil {
		return fmt.Errorf("set config status: %w", err)
	}
	return nil
}

// DeleteConfig deletes a config row and its build history. Returns
// ErrNotFound if the config doesn't exist. Builds are deleted first (the FK
// has no ON DELETE CASCADE), then the config — both in a single tx so a
// partial delete can't leave orphaned rows.
func (s *Service) DeleteConfig(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete config: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM image_factory_builds WHERE config_id = $1`, id); err != nil {
		return fmt.Errorf("delete config: delete builds: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM image_factory_configs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete config: delete config: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete config: commit: %w", err)
	}
	return nil
}

// RenameConfig updates the friendly name. Returns ErrNotFound if the
// config doesn't exist, or ErrConflict (via pq unique violation) if the
// name collides within the same scope.
func (s *Service) RenameConfig(ctx context.Context, id, newName string) error {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_configs SET name = $1, updated_at = now() WHERE id = $2`,
		newName, id)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("rename config: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Builds ──────────────────────────────────────────────────────────────

const buildColumns = `id, config_id, hash, base_name, base_version, resolved_values, architectures, image_ref, digest, status, gh_run_id, callback_token, failure_reason, explanation, triggered_by, started_at, finished_at, scope, org_id`

func (s *Service) GetBuild(ctx context.Context, id string) (imagefactory.Build, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM image_factory_builds WHERE id = $1`, id)
	b, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Build{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Build{}, fmt.Errorf("get build: %w", err)
	}
	return b, nil
}

// GetInFlightOrSuccessfulBuild is the coalescing probe (design/0046 #16).
// Returns a successful build if one exists for (hash, base_version);
// otherwise an in-flight (dispatched) one; otherwise nil. Prefers success
// so a new config immediately links to a Ready build rather than waiting
// on an in-flight one that might fail.
func (s *Service) GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM image_factory_builds
		 WHERE hash = $1 AND base_version = $2 AND status IN ('succeeded', 'dispatched')
		 ORDER BY (CASE status WHEN 'succeeded' THEN 0 ELSE 1 END), started_at DESC
		 LIMIT 1`,
		hash, baseVersion)
	b, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get in-flight-or-successful build: %w", err)
	}
	return &b, nil
}

func (s *Service) GetBuildByGHRunID(ctx context.Context, ghRunID int64) (imagefactory.Build, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM image_factory_builds WHERE gh_run_id = $1`, ghRunID)
	b, err := scanBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return imagefactory.Build{}, ErrNotFound
	}
	if err != nil {
		return imagefactory.Build{}, fmt.Errorf("get build by gh_run_id: %w", err)
	}
	return b, nil
}

func (s *Service) CreateBuild(ctx context.Context, b *imagefactory.Build) error {
	rvJSON, err := json.Marshal(b.ResolvedValues)
	if err != nil {
		return fmt.Errorf("create build: marshal resolved_values: %w", err)
	}
	var triggeredBy interface{}
	if b.TriggeredBy != nil {
		triggeredBy = *b.TriggeredBy
	}
	err = s.DB.QueryRowContext(ctx,
		`INSERT INTO image_factory_builds
		     (config_id, hash, base_name, base_version, resolved_values, architectures,
		      status, gh_run_id, callback_token, triggered_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id, started_at`,
		b.ConfigID, b.Hash, b.BaseName, b.BaseVersion, rvJSON, pgStringArray(b.Architectures),
		string(b.Status), b.GHRunID, b.CallbackToken, triggeredBy,
	).Scan(&b.ID, &b.StartedAt)
	if err != nil {
		return fmt.Errorf("create build: %w", err)
	}
	return nil
}

func (s *Service) MarkBuildSucceeded(ctx context.Context, id, imageRef, digest string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_builds
		 SET status = 'succeeded', image_ref = $1, digest = $2, finished_at = now()
		 WHERE id = $3`,
		imageRef, digest, id)
	if err != nil {
		return fmt.Errorf("mark build succeeded: %w", err)
	}
	return nil
}

func (s *Service) MarkBuildFailed(ctx context.Context, id, failureReason, explanation string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE image_factory_builds
		 SET status = 'failed', failure_reason = $1, explanation = $2, finished_at = now()
		 WHERE id = $3`,
		failureReason, explanation, id)
	if err != nil {
		return fmt.Errorf("mark build failed: %w", err)
	}
	return nil
}

// TransitionBuildSucceeded atomically marks a build succeeded and its
// config ready. Single tx — no partial state if one write fails.
func (s *Service) TransitionBuildSucceeded(ctx context.Context, buildID, configID, imageRef, digest string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transition succeeded: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE image_factory_builds SET status = 'succeeded', image_ref = $1, digest = $2, finished_at = now() WHERE id = $3`,
		imageRef, digest, buildID); err != nil {
		return fmt.Errorf("transition succeeded: update build: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE image_factory_configs SET status = 'ready', updated_at = now() WHERE id = $1`,
		configID); err != nil {
		return fmt.Errorf("transition succeeded: update config: %w", err)
	}
	return tx.Commit()
}

// TransitionBuildFailed atomically marks a build failed, records the
// known failure, and flips the config to rejected. Single tx — no
// partial state.
func (s *Service) TransitionBuildFailed(ctx context.Context, buildID, configID string, kf imagefactory.KnownFailure) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("transition failed: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE image_factory_builds SET status = 'failed', failure_reason = $1, explanation = $2, finished_at = now() WHERE id = $3`,
		kf.FailureReason, kf.Explanation, buildID); err != nil {
		return fmt.Errorf("transition failed: update build: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO image_factory_known_failures (selection_hash, selection, base_name, explanation, failure_reason, detected_at, retriable)
		 VALUES ($1, $2, $3, $4, $5, now(), $6)
		 ON CONFLICT (selection_hash, base_name) DO UPDATE SET
		     explanation = EXCLUDED.explanation, failure_reason = EXCLUDED.failure_reason, detected_at = now(), retriable = EXCLUDED.retriable`,
		kf.SelectionHash, pgStringArray(kf.Selection), kf.BaseName, kf.Explanation, kf.FailureReason, kf.Retriable); err != nil {
		return fmt.Errorf("transition failed: insert known failure: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE image_factory_configs SET status = 'rejected', updated_at = now() WHERE id = $1`,
		configID); err != nil {
		return fmt.Errorf("transition failed: update config: %w", err)
	}
	return tx.Commit()
}

// ── scanners ────────────────────────────────────────────────────────────

// rowScanner abstracts *sql.Row and *sql.Rows for shared scan logic.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanExtension(sc rowScanner) (imagefactory.Extension, error) {
	var e imagefactory.Extension
	var typeStr string
	var fileSpecRaw []byte
	var supportedBases stringArray
	if err := sc.Scan(&e.ID, &typeStr, &e.Value, &fileSpecRaw, &supportedBases,
		&e.Retired, &e.ReviewRequested, &e.Description); err != nil {
		return imagefactory.Extension{}, err
	}
	e.Type = imagefactory.ExtensionType(typeStr)
	e.SupportedBases = supportedBases
	if len(fileSpecRaw) > 0 && string(fileSpecRaw) != "null" {
		var fs imagefactory.FileSpec
		if err := json.Unmarshal(fileSpecRaw, &fs); err != nil {
			return imagefactory.Extension{}, fmt.Errorf("scan extension: unmarshal file_spec: %w", err)
		}
		e.FileSpec = &fs
	}
	return e, nil
}

func scanConfig(sc rowScanner) (imagefactory.Config, error) {
	var c imagefactory.Config
	var rvRaw []byte
	var scopeStr, statusStr string
	var ownerID, orgID sql.NullString
	// Selection is a named slice type (type Selection []string); pq.Array
	// only special-cases *[]string, so cast at the scan boundary.
	sel := (*stringArray)(&c.Selection)
	if err := sc.Scan(&c.ID, &c.Hash, &c.Name, sel, &rvRaw,
		&c.BaseName, &c.BaseVersion, &scopeStr, &ownerID, &orgID, &statusStr); err != nil {
		return imagefactory.Config{}, err
	}
	c.Scope = imagefactory.ConfigScope(scopeStr)
	c.Status = imagefactory.ConfigStatus(statusStr)
	if ownerID.Valid {
		v := ownerID.String
		c.OwnerID = &v
	}
	if orgID.Valid {
		v := orgID.String
		c.OrgID = &v
	}
	if err := json.Unmarshal(rvRaw, &c.ResolvedValues); err != nil {
		return imagefactory.Config{}, fmt.Errorf("scan config: unmarshal resolved_values: %w", err)
	}
	return c, nil
}

func scanConfigs(rows *sql.Rows) ([]imagefactory.Config, error) {
	var out []imagefactory.Config
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("scan configs: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanBuild(sc rowScanner) (imagefactory.Build, error) {
	var b imagefactory.Build
	var rvRaw []byte
	var statusStr string
	var imageRef, digest, failureReason, explanation, callbackToken sql.NullString
	var ghRunID sql.NullInt64
	var triggeredBy sql.NullString
	var finishedAt sql.NullTime
	var scope, buildOrgID sql.NullString
	if err := sc.Scan(&b.ID, &b.ConfigID, &b.Hash, &b.BaseName, &b.BaseVersion,
		&rvRaw, stringArrayScan(&b.Architectures), &imageRef, &digest, &statusStr,
		&ghRunID, &callbackToken, &failureReason, &explanation, &triggeredBy,
		&b.StartedAt, &finishedAt, &scope, &buildOrgID); err != nil {
		return imagefactory.Build{}, err
	}
	b.Status = imagefactory.BuildStatus(statusStr)
	b.ImageRef = imageRef.String
	b.Digest = digest.String
	b.FailureReason = failureReason.String
	b.Explanation = explanation.String
	b.CallbackToken = callbackToken.String // populated on the struct even though not in JSON
	if ghRunID.Valid {
		v := ghRunID.Int64
		b.GHRunID = &v
	}
	if triggeredBy.Valid {
		v := triggeredBy.String
		b.TriggeredBy = &v
	}
	if finishedAt.Valid {
		v := finishedAt.Time
		b.FinishedAt = &v
	}
	b.Scope = imagefactory.ConfigScope(scope.String)
	if buildOrgID.Valid {
		v := buildOrgID.String
		b.OrgID = &v
	}
	if err := json.Unmarshal(rvRaw, &b.ResolvedValues); err != nil {
		return imagefactory.Build{}, fmt.Errorf("scan build: unmarshal resolved_values: %w", err)
	}
	return b, nil
}

// sqlStateError is satisfied by both drivers' error types (*pq.Error,
// *pgconn.PgError).
type sqlStateError interface {
	SQLState() string
}

// isUniqueViolation reports a 23505 unique-constraint violation from
// either Postgres driver (#936).
func isUniqueViolation(err error) bool {
	var se sqlStateError
	if errors.As(err, &se) {
		return se.SQLState() == "23505"
	}
	return false
}
