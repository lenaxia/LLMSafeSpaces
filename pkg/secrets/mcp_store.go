// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- Epic 53: MCP server store (US-53.3) ---
//
// Pure data-access on mcp_servers / mcp_server_bindings / mcp_server_auto_apply.
// Crypto (encrypt/decrypt) is NOT done here — handlers encrypt before calling
// Create/Update; the injection pipeline decrypts after calling
// GetWorkspaceMCPServers. This mirrors the provider_credentials split exactly.

// MCPServerRow is the DB row shape for mcp_servers.
type MCPServerRow struct {
	ID         string
	OwnerType  string
	OwnerID    string
	Name       string
	Transport  string
	URL        string
	Command    string
	Args       []string
	TimeoutMs  *int
	Ciphertext []byte
	KeyVersion int
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// MCPServerBindingRow is a joined row from mcp_server_bindings + mcp_servers,
// used by the injection pipeline. Ciphertext is carried for downstream decrypt.
type MCPServerBindingRow struct {
	ServerID   string
	Name       string
	Transport  string
	URL        string
	Command    string
	Args       []string
	TimeoutMs  *int
	Ciphertext []byte
	OwnerType  string
	KeyVersion int
	Enabled    bool
	SourceType string
}

// CreateMCPServer inserts a row into mcp_servers. The caller supplies a
// pre-generated ID and pre-encrypted ciphertext.
func (s *PgSecretStore) CreateMCPServer(ctx context.Context, row *MCPServerRow) error {
	// json.Marshal(nil []string) returns []byte("null"), which Postgres
	// stores as JSONB null. The read path normalizes nil back to []string{},
	// so the roundtrip works. The column default '[]'::jsonb applies only
	// when the INSERT omits the column entirely (not when nil is passed).
	args, _ := json.Marshal(row.Args)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_servers (id, owner_type, owner_id, name, transport, url, command, args, timeout_ms, ciphertext, key_version, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`, row.ID, row.OwnerType, row.OwnerID, row.Name, row.Transport, row.URL, row.Command, args, row.TimeoutMs, row.Ciphertext, row.KeyVersion, row.Enabled, row.CreatedAt, row.UpdatedAt)
	return err
}

// ListMCPServers returns all MCP servers owned by (ownerType, ownerID),
// ordered by created_at ASC. Never decrypts — display fields only.
func (s *PgSecretStore) ListMCPServers(ctx context.Context, ownerType, ownerID string) ([]*MCPServerRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_type, owner_id, name, transport, url, command, args, timeout_ms, ciphertext, key_version, enabled, created_at, updated_at
		FROM mcp_servers WHERE owner_type = $1 AND owner_id = $2
		ORDER BY created_at ASC
	`, ownerType, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMCPServerRows(rows)
}

// GetMCPServer returns a single MCP server by ID scoped to (ownerType, ownerID),
// or nil if not found.
func (s *PgSecretStore) GetMCPServer(ctx context.Context, ownerType, ownerID, serverID string) (*MCPServerRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, owner_type, owner_id, name, transport, url, command, args, timeout_ms, ciphertext, key_version, enabled, created_at, updated_at
		FROM mcp_servers WHERE id = $1 AND owner_type = $2 AND owner_id = $3
	`, serverID, ownerType, ownerID)
	r, err := scanMCPServerRow(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// UpdateMCPServer updates an existing MCP server scoped to (ownerType, ownerID).
// Partial update: nil fields preserve existing values. Ciphertext is only
// rewritten when non-nil (secret rotation). Args is only rewritten when non-nil
// (a nil []string is passed as SQL NULL so the CASE preserves the existing value;
// passing JSON "null" would overwrite args with JSONB null — see review finding).
func (s *PgSecretStore) UpdateMCPServer(ctx context.Context, ownerType, ownerID, serverID string, row *MCPServerRow) error {
	var argsJSON any
	if row.Args != nil {
		argsJSON, _ = json.Marshal(row.Args)
	}
	return s.pool.QueryRow(ctx, `
		UPDATE mcp_servers
		SET name = COALESCE(NULLIF($4, ''), name),
		    url = COALESCE(NULLIF($5, ''), url),
		    command = COALESCE(NULLIF($6, ''), command),
		    args = CASE WHEN $7::jsonb IS NULL THEN args ELSE $7 END,
		    timeout_ms = CASE WHEN $8::int IS NULL THEN timeout_ms ELSE $8 END,
		    enabled = CASE WHEN $9::boolean IS NULL THEN enabled ELSE $9 END,
		    ciphertext = CASE WHEN $10::bytea IS NOT NULL THEN $10 ELSE ciphertext END,
		    key_version = CASE WHEN $10::bytea IS NOT NULL THEN $11 ELSE key_version END
		WHERE id = $1 AND owner_type = $2 AND owner_id = $3
		RETURNING updated_at
	`, serverID, ownerType, ownerID, row.Name, row.URL, row.Command, argsJSON, row.TimeoutMs, boolPtr(row.Enabled), row.Ciphertext, row.KeyVersion).Scan(&row.UpdatedAt)
}

// DeleteMCPServer deletes an MCP server by ID scoped to (ownerType, ownerID).
// FK cascades handle bindings + auto-apply. Returns pgx.ErrNoRows if not found.
func (s *PgSecretStore) DeleteMCPServer(ctx context.Context, ownerType, ownerID, serverID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1 AND owner_type = $2 AND owner_id = $3`, serverID, ownerType, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountMCPServersByOwner returns the number of MCP servers owned by
// (ownerType, ownerID). Used for plan-tier quota enforcement (user scope).
func (s *PgSecretStore) CountMCPServersByOwner(ctx context.Context, ownerType, ownerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM mcp_servers WHERE owner_type = $1 AND owner_id = $2`, ownerType, ownerID).Scan(&n)
	return n, err
}

// CountWorkspaceMCPServers returns the number of bound MCP servers for a
// workspace. Used for org quota enforcement (max_mcp_servers_per_workspace).
func (s *PgSecretStore) CountWorkspaceMCPServers(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM mcp_server_bindings b
		JOIN mcp_servers s ON s.id = b.server_id
		WHERE b.workspace_id = $1 AND s.enabled = true
	`, workspaceID).Scan(&n)
	return n, err
}

// GetWorkspaceOrgIDForMCP resolves the org_id of a workspace, used by the
// MCP bind quota check to look up max_mcp_servers_per_workspace. Returns ""
// for personal workspaces (no org quota applies — only plan-tier quota).
func (s *PgSecretStore) GetWorkspaceOrgIDForMCP(ctx context.Context, workspaceID string) (string, error) {
	var orgID *string
	err := s.pool.QueryRow(ctx, `SELECT org_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&orgID)
	if err != nil || orgID == nil {
		return "", err
	}
	return *orgID, nil
}

// GetWorkspaceUserIDForMCP resolves the user_id of a workspace, used by
// the MCP bind handler to verify the caller owns the target workspace.
func (s *PgSecretStore) GetWorkspaceUserIDForMCP(ctx context.Context, workspaceID string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `SELECT user_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&userID)
	if err != nil {
		return "", err
	}
	return userID, nil
}

// GetWorkspaceMCPServers returns all bound+enabled MCP servers for a workspace,
// carrying ciphertext for downstream decryption by the injection pipeline.
// Disabled servers are skipped (omitted, not emitted as the disable form).
func (s *PgSecretStore) GetWorkspaceMCPServers(ctx context.Context, workspaceID string) ([]MCPServerBindingRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.name, s.transport, s.url, s.command, s.args, s.timeout_ms,
		       s.ciphertext, s.owner_type, s.key_version, b.source_type
		FROM mcp_server_bindings b
		JOIN mcp_servers s ON s.id = b.server_id
		WHERE b.workspace_id = $1 AND s.enabled = true
		ORDER BY s.created_at ASC
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("query workspace mcp servers: %w", err)
	}
	defer rows.Close()

	var out []MCPServerBindingRow
	for rows.Next() {
		var r MCPServerBindingRow
		var argsJSON []byte
		if err := rows.Scan(&r.ServerID, &r.Name, &r.Transport, &r.URL, &r.Command, &argsJSON, &r.TimeoutMs, &r.Ciphertext, &r.OwnerType, &r.KeyVersion, &r.SourceType); err != nil {
			return nil, fmt.Errorf("scan mcp binding: %w", err)
		}
		if argsJSON != nil {
			_ = json.Unmarshal(argsJSON, &r.Args) //nolint:errcheck // DB CHECK guarantees valid JSON
		}
		if r.Args == nil {
			r.Args = []string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedWorkspaceMCPServers inserts MCP server bindings for a new workspace.
// Idempotent (ON CONFLICT DO NOTHING). Mirrors SeedWorkspaceCredentials.
//
// Seeding logic:
//   - Platform (target_type='all') rules always apply.
//   - User-scope (target_type='user') rules always apply — the workspace's
//     user_id is available regardless of org membership. Whether the user
//     is ALLOWED to create personal MCP servers is gated at CRUD time by
//     the allow_user_mcp_servers org policy, not at seed time.
//   - Org-scope (target_type='org') rules apply when orgID is non-empty.
func (s *PgSecretStore) SeedWorkspaceMCPServers(ctx context.Context, workspaceID, userID string, orgID *string) error {
	// Bind platform auto-apply rules (target_type='all').
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_server_bindings (server_id, workspace_id, source_type)
		SELECT maa.server_id, $1, 'auto'
		FROM mcp_server_auto_apply maa
		JOIN mcp_servers s ON s.id = maa.server_id AND s.enabled = true
		WHERE maa.target_type = 'all'
		ON CONFLICT (server_id, workspace_id) DO NOTHING
	`, workspaceID)
	if err != nil {
		return fmt.Errorf("seed mcp (platform rules): %w", err)
	}

	// Bind user-scope auto-apply rules (always — the user_id is on every
	// workspace row, personal or org-owned).
	_, err = s.pool.Exec(ctx, `
		INSERT INTO mcp_server_bindings (server_id, workspace_id, source_type)
		SELECT maa.server_id, $1, 'auto'
		FROM mcp_server_auto_apply maa
		JOIN mcp_servers s ON s.id = maa.server_id AND s.enabled = true
		WHERE maa.target_type = 'user' AND maa.target_id = $2
		ON CONFLICT (server_id, workspace_id) DO NOTHING
	`, workspaceID, userID)
	if err != nil {
		return fmt.Errorf("seed mcp (user rules): %w", err)
	}

	if orgID == nil || *orgID == "" {
		return nil
	}

	// Org workspace: bind org auto-apply rules.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO mcp_server_bindings (server_id, workspace_id, source_type)
		SELECT maa.server_id, $1, 'auto'
		FROM mcp_server_auto_apply maa
		JOIN mcp_servers s ON s.id = maa.server_id AND s.enabled = true
		WHERE maa.target_type = 'org' AND maa.target_id = $2
		ON CONFLICT (server_id, workspace_id) DO NOTHING
	`, workspaceID, *orgID)
	if err != nil {
		return fmt.Errorf("seed mcp (org rules): %w", err)
	}
	return nil
}

// BindMCPServerToWorkspace explicitly binds an MCP server to a workspace.
func (s *PgSecretStore) BindMCPServerToWorkspace(ctx context.Context, serverID, workspaceID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_server_bindings (server_id, workspace_id, source_type)
		VALUES ($1, $2, 'explicit')
		ON CONFLICT (server_id, workspace_id) DO UPDATE SET source_type = 'explicit'
	`, serverID, workspaceID)
	return err
}

// UnbindMCPServerFromWorkspace removes an explicit MCP server binding.
// Returns ErrAutoBindingProtected if the binding is auto-managed.
func (s *PgSecretStore) UnbindMCPServerFromWorkspace(ctx context.Context, serverID, workspaceID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM mcp_server_bindings
		WHERE server_id = $1 AND workspace_id = $2 AND source_type = 'explicit'
	`, serverID, workspaceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var sourceType string
		scanErr := s.pool.QueryRow(ctx, `
			SELECT source_type FROM mcp_server_bindings
			WHERE server_id = $1 AND workspace_id = $2
		`, serverID, workspaceID).Scan(&sourceType)
		if scanErr == pgx.ErrNoRows {
			return nil // already gone — idempotent
		}
		if scanErr == nil && sourceType == "auto" {
			return ErrAutoBindingProtected
		}
	}
	return nil
}

// BackfillMCPServerAutoApply binds an MCP server to all workspaces matching
// its auto-apply rules. Idempotent. Returns rows inserted.
//
// The workspace match uses workspaces.user_id (for target_type='user') and
// workspaces.org_id (for target_type='org') directly — both columns exist on
// the workspaces table. target_type='all' matches every workspace.
func (s *PgSecretStore) BackfillMCPServerAutoApply(ctx context.Context, serverID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_server_bindings (server_id, workspace_id, source_type)
		SELECT $1, w.id, 'auto'
		FROM workspaces w
		WHERE w.deleted_at IS NULL
		  AND EXISTS (
		    SELECT 1 FROM mcp_server_auto_apply maa
		    JOIN mcp_servers s ON s.id = maa.server_id AND s.enabled = true
		    WHERE maa.server_id = $1
		      AND (
		        maa.target_type = 'all'
		        OR (maa.target_type = 'user' AND maa.target_id = w.user_id)
		        OR (maa.target_type = 'org' AND maa.target_id = w.org_id::text)
		      )
		  )
		ON CONFLICT (server_id, workspace_id) DO NOTHING
	`, serverID)
	if err != nil {
		return 0, fmt.Errorf("backfill mcp auto-apply: %w", err)
	}
	return tag.RowsAffected(), nil
}

// --- Auto-apply CRUD ---

// MCPAutoApplyRule is a row from mcp_server_auto_apply.
type MCPAutoApplyRule struct {
	ServerID   string
	TargetType string
	TargetID   *string
}

// CreateMCPServerAutoApply inserts an auto-apply rule.
func (s *PgSecretStore) CreateMCPServerAutoApply(ctx context.Context, serverID, targetType string, targetID *string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_server_auto_apply (server_id, target_type, target_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, serverID, targetType, targetID)
	return err
}

// DeleteMCPServerAutoApply removes an auto-apply rule.
func (s *PgSecretStore) DeleteMCPServerAutoApply(ctx context.Context, serverID, targetType string, targetID *string) error {
	if targetID == nil {
		_, err := s.pool.Exec(ctx, `
			DELETE FROM mcp_server_auto_apply
			WHERE server_id = $1 AND target_type = $2 AND target_id IS NULL
		`, serverID, targetType)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM mcp_server_auto_apply
		WHERE server_id = $1 AND target_type = $2 AND target_id = $3
	`, serverID, targetType, *targetID)
	return err
}

// ListMCPServerAutoApply returns all auto-apply rules for a server.
func (s *PgSecretStore) ListMCPServerAutoApply(ctx context.Context, serverID string) ([]MCPAutoApplyRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT server_id, target_type, target_id
		FROM mcp_server_auto_apply WHERE server_id = $1
	`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MCPAutoApplyRule
	for rows.Next() {
		var r MCPAutoApplyRule
		if err := rows.Scan(&r.ServerID, &r.TargetType, &r.TargetID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- helpers ---

func scanMCPServerRows(rows pgx.Rows) ([]*MCPServerRow, error) {
	var out []*MCPServerRow
	for rows.Next() {
		r, err := scanMCPServerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type mcpRowScanner interface {
	Scan(dest ...any) error
}

func scanMCPServerRow(row mcpRowScanner) (*MCPServerRow, error) {
	var r MCPServerRow
	var argsJSON []byte
	if err := row.Scan(&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Transport, &r.URL, &r.Command, &argsJSON, &r.TimeoutMs, &r.Ciphertext, &r.KeyVersion, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if argsJSON != nil {
		_ = json.Unmarshal(argsJSON, &r.Args) //nolint:errcheck
	}
	if r.Args == nil {
		r.Args = []string{}
	}
	return &r, nil
}

// boolPtr converts a bool to *bool for SQL parameter use. Returns nil-safe
// pointer so COALESCE/CASE can distinguish "set" from "not changing".
func boolPtr(b bool) *bool { return &b }
