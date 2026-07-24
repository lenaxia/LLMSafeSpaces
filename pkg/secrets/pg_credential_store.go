// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetWorkspaceCredentials returns all credential bindings for a workspace,
// ordered by: (source_type='explicit') DESC, within_priority DESC, created_at ASC.
func (s *PgSecretStore) GetWorkspaceCredentials(ctx context.Context, workspaceID string) ([]CredentialBinding, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pc.id, pc.owner_type, pc.owner_id, pc.kind, pc.slug, pc.ciphertext,
		       pc.key_version, pc.model_allowlist, pc.model_context_limits, pc.model_output_limits, wcb.source_type, wcb.within_priority
		FROM workspace_credential_bindings wcb
		JOIN provider_credentials pc ON pc.id = wcb.credential_id
		WHERE wcb.workspace_id = $1
		ORDER BY (wcb.source_type = 'explicit') DESC, wcb.within_priority DESC, wcb.created_at ASC
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("query workspace credentials: %w", err)
	}
	defer rows.Close()

	var bindings []CredentialBinding
	for rows.Next() {
		var b CredentialBinding
		if err := rows.Scan(
			&b.ID, &b.OwnerType, &b.OwnerID, &b.Kind, &b.Slug, &b.Ciphertext,
			&b.KeyVersion, &b.ModelAllowlist, &b.ModelContextLimits, &b.ModelOutputLimits, &b.SourceType, &b.WithinPriority,
		); err != nil {
			return nil, fmt.Errorf("scan credential binding: %w", err)
		}
		if b.ModelContextLimits == nil {
			b.ModelContextLimits = map[string]int{}
		}
		if b.ModelOutputLimits == nil {
			b.ModelOutputLimits = map[string]int{}
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

// UpsertFreeTierCredential atomically upserts the platform free-tier
// opencode credential and its auto-apply rule in a single transaction.
func (s *PgSecretStore) UpsertFreeTierCredential(ctx context.Context, ciphertext []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var credID string
	err = tx.QueryRow(ctx, `
		INSERT INTO provider_credentials (owner_type, owner_id, name, kind, slug, ciphertext)
		VALUES ('admin', '_platform', 'opencode-free-tier', 'opencode', 'opencode-free-tier', $1)
		ON CONFLICT (owner_type, owner_id, slug)
		DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = now()
		RETURNING id
	`, ciphertext).Scan(&credID)
	if err != nil {
		return fmt.Errorf("upsert provider_credentials: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO credential_auto_apply (credential_id, target_type, within_priority)
		VALUES ($1, 'all', 0)
		ON CONFLICT DO NOTHING
	`, credID)
	if err != nil {
		return fmt.Errorf("upsert credential_auto_apply: %w", err)
	}

	return tx.Commit(ctx)
}

// SeedWorkspaceCredentials inserts credential bindings for a new workspace.
// Idempotent — uses ON CONFLICT DO NOTHING throughout.
//   - orgID nil: personal workspace — org auto-apply rules are not applied.
//   - orgID non-nil: org workspace — org auto-apply rules and all org credentials are bound.
//
// Issue #593 Option A: when the workspace owner has users.role='admin',
// every admin credential is bound unconditionally — including ones with
// no credential_auto_apply rule. Rationale: POST /admin/provider-credentials
// creates a credential but does not auto-create an auto-apply rule, so an
// admin's own workspaces would otherwise never see admin credentials they
// added via the admin UI without a second manual API call. The cascade is
// gated on role='admin' via a SQL EXISTS subquery so non-admin owners are
// unaffected (they only see admin credentials that have target_type='all'
// or target_type='user' auto-apply rules — same as before).
func (s *PgSecretStore) SeedWorkspaceCredentials(ctx context.Context, workspaceID, userID string, orgID *string) error {
	// Bind admin auto-apply rules (target_type='all' or 'user' matching userID).
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT caa.credential_id, $1, 'auto', caa.within_priority
		FROM credential_auto_apply caa
		WHERE caa.target_type = 'all'
		   OR (caa.target_type = 'user' AND caa.target_id = $2)
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("seed workspace credentials (admin rules): %w", err)
	}

	// Issue #593 Option A: when the workspace owner is an admin, bind
	// every admin credential unconditionally. Credentials already bound
	// by the auto-apply block above are skipped via ON CONFLICT. The
	// EXISTS subquery is the privilege gate: non-admin owners get zero
	// rows here, preserving the pre-fix behavior for them.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT pc.id, $1, 'auto', 0
		FROM provider_credentials pc
		WHERE pc.owner_type = 'admin'
		  AND EXISTS (SELECT 1 FROM users WHERE id = $2 AND role = 'admin')
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("seed workspace credentials (admin cascade): %w", err)
	}

	// Bind all personal credentials owned by this user.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT pc.id, $1, 'auto', 10
		FROM provider_credentials pc
		WHERE pc.owner_type = 'user' AND pc.owner_id = $2
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("seed workspace credentials (user creds): %w", err)
	}

	if orgID == nil || *orgID == "" {
		return nil
	}

	// Bind org auto-apply rules.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT caa.credential_id, $1, 'auto', caa.within_priority
		FROM credential_auto_apply caa
		JOIN provider_credentials pc ON pc.id = caa.credential_id
		  AND pc.owner_type = 'org' AND pc.owner_id = $2
		WHERE caa.target_type = 'org' AND caa.target_id = $2
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, workspaceID, *orgID)
	if err != nil {
		return fmt.Errorf("seed workspace credentials (org auto-apply rules): %w", err)
	}

	// Bind all org-owned credentials directly.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT pc.id, $1, 'auto', 5
		FROM provider_credentials pc
		WHERE pc.owner_type = 'org' AND pc.owner_id = $2
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, workspaceID, *orgID)
	if err != nil {
		return fmt.Errorf("seed workspace credentials (org creds): %w", err)
	}

	return nil
}

// BindCredentialToAllUserWorkspaces binds a user credential to every workspace
// owned by userID. Called when a user creates a new personal credential so that
// the invariant "all credentials bound to all workspaces" is maintained. Idempotent.
func (s *PgSecretStore) BindCredentialToAllUserWorkspaces(ctx context.Context, credentialID, userID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT $1, w.id, 'auto', 10
		FROM workspaces w
		WHERE w.user_id = $2
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`, credentialID, userID)
	if err != nil {
		return fmt.Errorf("bind credential to all user workspaces: %w", err)
	}
	return nil
}

// BackfillFreeTierBindings inserts workspace_credential_bindings for all
// existing workspaces that lack the free-tier opencode credential binding.
// Idempotent — uses ON CONFLICT DO NOTHING. Returns the number of rows inserted.
func (s *PgSecretStore) BackfillFreeTierBindings(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		SELECT pc.id, w.id, 'auto', 0
		FROM provider_credentials pc
		CROSS JOIN workspaces w
		WHERE pc.owner_type = 'admin' AND pc.owner_id = '_platform' AND pc.slug = 'opencode-free-tier'
		  AND NOT EXISTS (
		    SELECT 1 FROM workspace_credential_bindings wcb
		    WHERE wcb.credential_id = pc.id AND wcb.workspace_id = w.id
		  )
		ON CONFLICT (credential_id, workspace_id) DO NOTHING
	`)
	if err != nil {
		return 0, fmt.Errorf("backfill free-tier bindings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// HasUserProviderCredential returns true if the user owns a credential
// with the given slug.
func (s *PgSecretStore) HasUserProviderCredential(ctx context.Context, userID, slug string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM provider_credentials
			WHERE owner_type = 'user' AND owner_id = $1 AND slug = $2
		)
	`, userID, slug).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check user credential by slug: %w", err)
	}
	return exists, nil
}

// CredentialRow is the DB row shape for all provider credential types.
// Maps to the provider_credentials table; owner_type discriminates the owner
// scope ("admin", "user", "org") and owner_id the concrete owner
// ("_platform", a user id, or an org id). Defined here to avoid an import
// cycle (handlers → secrets → handlers).
//
// Epic 55 identity model:
//   - Kind is the SDK-class enum (openai, anthropic, openai_compatible, ...).
//     Multiple credentials of the same Kind can exist per owner.
//   - Slug is the per-owner unique identity AND the literal provider-map key
//     in agent-config.json. Slug-safe regex enforced by the DB CHECK.
//   - Name is the free-form display label shown in the UI.
type CredentialRow struct {
	ID                 string
	OwnerType          string
	OwnerID            string
	Name               string // display label, free-form
	Kind               string // SDK-class enum
	Slug               string // per-owner unique identity; reaches opencode as providerID
	Ciphertext         []byte
	KeyVersion         int
	ModelAllowlist     []string
	ModelContextLimits map[string]int // model_id → context window size in tokens
	ModelOutputLimits  map[string]int // model_id → max response tokens
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateCredential inserts a provider credential scoped by (ownerType, ownerID).
// The caller supplies a pre-generated ID (uuid.New().String()), matching the
// admin/user pattern; the DB DEFAULT gen_random_uuid() is only a fallback.
func (s *PgSecretStore) CreateCredential(ctx context.Context, ownerType, ownerID string, row *CredentialRow) error {
	if row.ModelContextLimits == nil {
		row.ModelContextLimits = map[string]int{}
	}
	if row.ModelOutputLimits == nil {
		row.ModelOutputLimits = map[string]int{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO provider_credentials (id, owner_type, owner_id, name, kind, slug, ciphertext, key_version, model_allowlist, model_context_limits, model_output_limits, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, row.ID, ownerType, ownerID, row.Name, row.Kind, row.Slug, row.Ciphertext, row.KeyVersion, row.ModelAllowlist, row.ModelContextLimits, row.ModelOutputLimits, row.CreatedAt, row.UpdatedAt)
	return err
}

// ListCredentials returns all credentials owned by (ownerType, ownerID),
// ordered by created_at ASC.
func (s *PgSecretStore) ListCredentials(ctx context.Context, ownerType, ownerID string) ([]*CredentialRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_type, owner_id, name, kind, slug, ciphertext, key_version, model_allowlist, model_context_limits, model_output_limits, created_at, updated_at
		FROM provider_credentials WHERE owner_type = $1 AND owner_id = $2
		ORDER BY created_at ASC
	`, ownerType, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*CredentialRow
	for rows.Next() {
		var r CredentialRow
		if err := rows.Scan(&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Kind, &r.Slug, &r.Ciphertext, &r.KeyVersion, &r.ModelAllowlist, &r.ModelContextLimits, &r.ModelOutputLimits, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if r.ModelContextLimits == nil {
			r.ModelContextLimits = map[string]int{}
		}
		if r.ModelOutputLimits == nil {
			r.ModelOutputLimits = map[string]int{}
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetCredential returns a single credential by ID scoped to (ownerType, ownerID),
// or nil if not found. Filtering on both owner_type AND owner_id preserves the
// L-4 defensive multi-admin safety of the former admin path.
func (s *PgSecretStore) GetCredential(ctx context.Context, ownerType, ownerID, credID string) (*CredentialRow, error) {
	var r CredentialRow
	err := s.pool.QueryRow(ctx, `
		SELECT id, owner_type, owner_id, name, kind, slug, ciphertext, key_version, model_allowlist, model_context_limits, model_output_limits, created_at, updated_at
		FROM provider_credentials WHERE id = $1 AND owner_type = $2 AND owner_id = $3
	`, credID, ownerType, ownerID).Scan(&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Kind, &r.Slug, &r.Ciphertext, &r.KeyVersion, &r.ModelAllowlist, &r.ModelContextLimits, &r.ModelOutputLimits, &r.CreatedAt, &r.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.ModelContextLimits == nil {
		r.ModelContextLimits = map[string]int{}
	}
	if r.ModelOutputLimits == nil {
		r.ModelOutputLimits = map[string]int{}
	}
	return &r, nil
}

// UpdateCredential updates an existing credential scoped to (ownerType, ownerID).
//
// It uses COALESCE for model_allowlist, model_context_limits, and
// model_output_limits so a nil value means "don't change this column" — the
// org handler relies on this: a nil modelContextLimits/modelOutputLimits must
// reach the DB as SQL NULL so COALESCE preserves the existing column value.
// An empty slice/map is a valid "clear the column" value and is written as-is.
// Do NOT normalize nil → {} here (it would convert a "don't change" into a
// "clear all" via COALESCE).
//
// For the admin handler (which allows provider changes and re-encrypts up-front),
// the caller passes the fully-resolved row with non-nil fields; COALESCE is a
// no-op there because the caller always supplies concrete values.
//
// updated_at is read back via RETURNING (M-8 fix); the DB trigger sets it to now().
func (s *PgSecretStore) UpdateCredential(ctx context.Context, ownerType, ownerID, credID string, row *CredentialRow) error {
	return s.pool.QueryRow(ctx, `
		UPDATE provider_credentials
		SET name = COALESCE(NULLIF($4, ''), name),
		    kind = COALESCE(NULLIF($5, ''), kind),
		    slug = COALESCE(NULLIF($6, ''), slug),
		    ciphertext = CASE WHEN $7::bytea IS NOT NULL THEN $7 ELSE ciphertext END,
		    key_version = $8,
		    model_allowlist = COALESCE($9, model_allowlist),
		    model_context_limits = COALESCE($10, model_context_limits),
		    model_output_limits = COALESCE($11, model_output_limits)
		WHERE id = $1 AND owner_type = $2 AND owner_id = $3
		RETURNING updated_at
	`, credID, ownerType, ownerID, row.Name, row.Kind, row.Slug, row.Ciphertext, row.KeyVersion, row.ModelAllowlist, row.ModelContextLimits, row.ModelOutputLimits).Scan(&row.UpdatedAt)
}

// DeleteCredential deletes a credential by ID scoped to (ownerType, ownerID).
// FK cascades handle bindings. Returns pgx.ErrNoRows if no row was deleted so
// callers can distinguish 404 (L-1 fix).
func (s *PgSecretStore) DeleteCredential(ctx context.Context, ownerType, ownerID, credID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id = $1 AND owner_type = $2 AND owner_id = $3`, credID, ownerType, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CreateAutoApply inserts an auto-apply rule.
func (s *PgSecretStore) CreateAutoApply(ctx context.Context, credentialID, targetType string, targetID *string, priority int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO credential_auto_apply (credential_id, target_type, target_id, within_priority)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, credentialID, targetType, targetID, priority)
	return err
}

// DeleteAutoApply removes an auto-apply rule.
func (s *PgSecretStore) DeleteAutoApply(ctx context.Context, credentialID, targetType string, targetID *string) error {
	if targetID == nil {
		_, err := s.pool.Exec(ctx, `
			DELETE FROM credential_auto_apply
			WHERE credential_id = $1 AND target_type = $2 AND target_id IS NULL
		`, credentialID, targetType)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM credential_auto_apply
		WHERE credential_id = $1 AND target_type = $2 AND target_id = $3
	`, credentialID, targetType, *targetID)
	return err
}

// AutoApplyRule is a row from credential_auto_apply (exported for handler use).
type AutoApplyRule struct {
	CredentialID string
	TargetType   string
	TargetID     *string
	Priority     int
}

// ListAutoApply returns all auto-apply rules for a credential.
func (s *PgSecretStore) ListAutoApply(ctx context.Context, credentialID string) ([]AutoApplyRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT credential_id, target_type, target_id, within_priority
		FROM credential_auto_apply WHERE credential_id = $1
	`, credentialID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AutoApplyRule
	for rows.Next() {
		var r AutoApplyRule
		if err := rows.Scan(&r.CredentialID, &r.TargetType, &r.TargetID, &r.Priority); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BindCredentialToWorkspace explicitly binds a credential to a workspace.
func (s *PgSecretStore) BindCredentialToWorkspace(ctx context.Context, credentialID, workspaceID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workspace_credential_bindings (credential_id, workspace_id, source_type, within_priority)
		VALUES ($1, $2, 'explicit', 0)
		ON CONFLICT (credential_id, workspace_id) DO UPDATE SET source_type = 'explicit'
	`, credentialID, workspaceID)
	return err
}

// UnbindCredentialFromWorkspace removes an EXPLICIT credential binding.
// Returns ErrAutoBindingProtected if the binding is auto-managed (H-1 fix).
func (s *PgSecretStore) UnbindCredentialFromWorkspace(ctx context.Context, credentialID, workspaceID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM workspace_credential_bindings
		WHERE credential_id = $1 AND workspace_id = $2 AND source_type = 'explicit'
	`, credentialID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "already gone" (idempotent OK) from "auto-binding" (protected).
		var sourceType string
		scanErr := s.pool.QueryRow(ctx, `
			SELECT source_type FROM workspace_credential_bindings
			WHERE credential_id = $1 AND workspace_id = $2
		`, credentialID, workspaceID).Scan(&sourceType)
		if scanErr == pgx.ErrNoRows {
			return nil // Already gone — idempotent.
		}
		if scanErr == nil && sourceType == "auto" {
			return ErrAutoBindingProtected
		}
	}
	return nil
}

// GetCredentialBindings returns workspace IDs the credential is bound to,
// scoped to workspaces owned by ownerID.
func (s *PgSecretStore) GetCredentialBindings(ctx context.Context, credentialID, ownerID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wcb.workspace_id
		FROM workspace_credential_bindings wcb
		JOIN workspaces w ON w.id = wcb.workspace_id
		WHERE wcb.credential_id = $1
		  AND w.user_id = $2
		ORDER BY wcb.workspace_id
	`, credentialID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, rows.Err()
}

// GetCredentialBindingsWithSource returns workspace IDs and source type for bindings,
// scoped to workspaces owned by ownerID (M-1 fix: allows UI to distinguish auto vs explicit).
func (s *PgSecretStore) GetCredentialBindingsWithSource(ctx context.Context, credentialID, ownerID string) ([]CredentialBindingInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT wcb.workspace_id, wcb.source_type
		FROM workspace_credential_bindings wcb
		JOIN workspaces w ON w.id = wcb.workspace_id
		WHERE wcb.credential_id = $1
		  AND w.user_id = $2
		ORDER BY wcb.workspace_id
	`, credentialID, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CredentialBindingInfo
	for rows.Next() {
		var b CredentialBindingInfo
		if err := rows.Scan(&b.WorkspaceID, &b.SourceType); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if out == nil {
		out = []CredentialBindingInfo{}
	}
	return out, rows.Err()
}
