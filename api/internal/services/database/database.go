// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lib/pq"
)

// Service handles database operations
type Service struct {
	Logger *logger.Logger
	Config *config.Config
	DB     *sql.DB
}

func New(cfg *config.Config, log *logger.Logger) (*Service, error) {
	connString := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.User,
		cfg.Database.Password,
		cfg.Database.Database,
		cfg.Database.SSLMode,
	)

	// Open via stdlib.OpenDB so we can attach a pgx QueryTracer at the
	// driver layer. Every query — including those issued by the secrets
	// pgxpool — flows through the same tracer and emits
	// llmsafespaces_db_query_duration_seconds and
	// llmsafespaces_db_errors_total. Switching from sql.Open("pgx", …)
	// to OpenDB is required: the registered driver path has no hook for
	// per-connection configuration.
	connConfig, err := pgx.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database connection string: %w", err)
	}
	connConfig.Tracer = newQueryTracer()
	db := stdlib.OpenDB(*connConfig)

	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	svc := &Service{
		Logger: log,
		Config: cfg,
		DB:     db,
	}
	// Seed the connection-pool gauges so they appear in /metrics from
	// startup, not only after the first periodic poll.
	stats := db.Stats()
	metrics.RecordDBPoolStats(stats.InUse, stats.Idle, stats.MaxOpenConnections)
	return svc, nil
}

// Start starts the database service
func (s *Service) Start() error {
	s.Logger.Info("Database service started")
	return nil
}

// Stop stops the database service
func (s *Service) Stop() error {
	s.Logger.Info("Stopping database service")
	return s.DB.Close()
}

// Ensure Service implements the DatabaseService interface
var _ interfaces.DatabaseService = (*Service)(nil) // Compile-time interface check

// Ping checks the database connection
func (s *Service) Ping(ctx context.Context) error {
	return s.DB.PingContext(ctx)
}

// GetUser gets a user by ID
func (s *Service) GetUser(ctx context.Context, userID string) (*types.User, error) {
	query := `
        SELECT id, username, email, password_hash, created_at, updated_at, active, role, status, email_verified
        FROM users
        WHERE id = $1
    `

	var user types.User

	err := s.DB.QueryRowContext(ctx, query, userID).Scan(
		&user.ID,
		&user.Username,
		&user.Email,
		&user.PasswordHash,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.Active,
		&user.Role,
		&user.Status,
		&user.EmailVerified,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get user by ID: %w", err)
	}

	return &user, nil
}

// GetUserByEmail gets a user by email address
func (s *Service) GetUserByEmail(ctx context.Context, email string) (*types.User, error) {
	query := `
        SELECT id, username, email, password_hash, created_at, updated_at, active, role, status, email_verified
        FROM users 
        WHERE email = $1
    `

	var user types.User

	err := s.DB.QueryRowContext(ctx, query, email).Scan(
		&user.ID,
		&user.Username,
		&user.Email,
		&user.PasswordHash,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.Active,
		&user.Role,
		&user.Status,
		&user.EmailVerified,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get user by email: %w", err)
	}

	return &user, nil
}

// CountUsers returns the total number of users in the system. Used by the
// auth Register flow to detect a fresh installation (count == 0) and
// auto-promote the first user to admin so a brand-new install has at least
// one administrator.
func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count users: %w", err)
	}
	return count, nil
}

// CreateUser creates a new user
func (s *Service) CreateUser(ctx context.Context, user *types.User) error {
	now := time.Now()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if user.UpdatedAt.IsZero() {
		user.UpdatedAt = now
	}

	// G8 (Epic 17): atomically promote the very first registrant to
	// admin in the same SQL statement so the count-then-insert race
	// is impossible. The CTE counts users `BEFORE` insert; if zero,
	// the role is forced to 'admin' regardless of the caller-supplied
	// value. If non-zero, the caller-supplied role wins (typically
	// 'user'). Postgres serializes the count + insert under the
	// row-level locks of the unique index on (email).
	query := `
		WITH existing AS (
			SELECT COUNT(*) AS n FROM users
		)
		INSERT INTO users (id, username, email, password_hash, created_at, updated_at, active, role)
		SELECT $1, $2, $3, $4, $5, $6, $7,
		       CASE WHEN existing.n = 0 THEN 'admin' ELSE $8 END
		FROM existing
		RETURNING role`

	var assignedRole string
	err := s.DB.QueryRowContext(ctx, query,
		user.ID,
		user.Username,
		user.Email,
		user.PasswordHash,
		user.CreatedAt,
		user.UpdatedAt,
		user.Active,
		user.Role,
	).Scan(&assignedRole)
	if err != nil {
		return fmt.Errorf("failed to create user: %w", err)
	}
	user.Role = assignedRole // reflect the actual role written
	return nil
}

// UpdateUser updates specific fields on a user record. Only non-nil fields are applied.
func (s *Service) UpdateUser(ctx context.Context, userID string, updates types.UserUpdates) error {
	query := "UPDATE users SET updated_at = NOW()"
	args := []interface{}{}
	i := 0

	if updates.Username != nil {
		i++
		query += fmt.Sprintf(", username = $%d", i)
		args = append(args, *updates.Username)
	}
	if updates.Email != nil {
		i++
		query += fmt.Sprintf(", email = $%d", i)
		args = append(args, *updates.Email)
	}
	if updates.Active != nil {
		i++
		query += fmt.Sprintf(", active = $%d", i)
		args = append(args, *updates.Active)
	}
	if updates.Role != nil {
		i++
		query += fmt.Sprintf(", role = $%d", i)
		args = append(args, *updates.Role)
	}
	if updates.PasswordHash != nil {
		i++
		query += fmt.Sprintf(", password_hash = $%d", i)
		args = append(args, *updates.PasswordHash)
	}
	if updates.Status != nil {
		i++
		query += fmt.Sprintf(", status = $%d", i)
		args = append(args, string(*updates.Status))
	}
	if updates.EmailVerified != nil {
		i++
		query += fmt.Sprintf(", email_verified = $%d", i)
		args = append(args, *updates.EmailVerified)
	}

	if i == 0 {
		return nil
	}

	// Same pattern as credential_sets: WHERE clause is a literal
	// "WHERE id = $N"; user values bind via placeholders.
	query += fmt.Sprintf(" WHERE id = $%d", i+1) //nolint:gosec // G202: literal with placeholder bind
	args = append(args, userID)

	_, err := s.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to update user: %w", err)
	}

	return nil
}

// SetUserStatus sets the authoritative operational status of a user account
// (D19). status='suspended' blocks the user across all contexts via the auth
// middleware; 'active' restores access.
//
// F6 (US-43.19): the legacy `active` boolean is mirrored from `status`
// (active = (status='active')) so the two columns cannot drift apart. The auth
// middleware authorizes on `status`; Login historically checks `active`. Before
// this fix, any path that wrote `active` independently of `status` would leave
// the user blocked at Login but not at the middleware (or vice-versa). Keeping
// them in lockstep removes that divergence vector.
func (s *Service) SetUserStatus(ctx context.Context, userID string, status types.UserStatus) error {
	active := status == types.UserStatusActive
	_, err := s.DB.ExecContext(ctx,
		`UPDATE users SET status = $1, active = $2, updated_at = NOW() WHERE id = $3`,
		string(status), active, userID,
	)
	if err != nil {
		return fmt.Errorf("failed to set user status: %w", err)
	}
	return nil
}

// ListAllUsers returns every user for the platform-admin dashboard (US-43.18).
// The optional statusFilter narrows to a single UserStatus; an empty/nil filter
// returns all users. Each entry carries the user's single org membership
// (org_id/org_name) resolved via a LEFT JOIN — under single-org enforcement
// (D8) a user belongs to at most one org, so this adds no row fan-out.
//
// Password hashes and other sensitive columns are never selected. limit is
// clamped to [1, adminListMaxLimit]; offset defaults to 0.
func (s *Service) ListAllUsers(ctx context.Context, limit, offset int, statusFilter *string) ([]types.UserListEntry, *types.PaginationMetadata, error) {
	limit = clampAdminLimit(limit)
	if offset < 0 {
		offset = 0
	}

	var (
		countArgs    []interface{}
		countWhere   string
		listWhere    string
		listArgs     []interface{}
		statusArgIdx int
	)
	if statusFilter != nil && *statusFilter != "" {
		countArgs = append(countArgs, *statusFilter)
		countWhere = " WHERE status = $1"
		listArgs = append(listArgs, *statusFilter)
		statusArgIdx = 1
		listWhere = " WHERE u.status = $1"
	} else {
		listWhere = ""
	}

	var total int
	if err := s.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users"+countWhere,
		countArgs...,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count all users: %w", err)
	}

	pagination := &types.PaginationMetadata{
		Total: total, Start: offset, End: offset + limit, Limit: limit, Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}
	if total == 0 {
		return []types.UserListEntry{}, pagination, nil
	}

	limitIdx := statusArgIdx + 1
	offsetIdx := statusArgIdx + 2
	listArgs = append(listArgs, limit, offset)
	query := fmt.Sprintf( //nolint:gosec // G201: $N placeholder indexes only, no string interpolation of user input
		`SELECT u.id, u.email, u.role, u.status, u.created_at,
		        COALESCE(m.org_id::text, ''), COALESCE(o.name, '')
		 FROM users u
		 LEFT JOIN org_memberships m ON m.user_id = u.id
		 LEFT JOIN organizations o ON o.id = m.org_id AND o.deleted_at IS NULL%s
		 ORDER BY u.created_at DESC
		 LIMIT $%d OFFSET $%d`,
		listWhere, limitIdx, offsetIdx,
	)

	rows, err := s.DB.QueryContext(ctx, query, listArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("list all users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]types.UserListEntry, 0)
	for rows.Next() {
		var e types.UserListEntry
		if err := rows.Scan(&e.ID, &e.Email, &e.Role, &e.Status, &e.CreatedAt, &e.OrgID, &e.OrgName); err != nil {
			return nil, nil, fmt.Errorf("scan user list entry: %w", err)
		}
		if e.OrgID != "" {
			e.OrgCount = 1
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate user list entries: %w", err)
	}
	return out, pagination, nil
}

// DeleteUser deletes a user
func (s *Service) DeleteUser(ctx context.Context, userID string) error {
	query := `DELETE FROM users WHERE id = $1`

	_, err := s.DB.ExecContext(ctx, query, userID)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}

	return nil
}

// GetUserByAPIKey gets the user associated with an API key
func (s *Service) GetUserByAPIKey(ctx context.Context, apiKey string) (*types.User, error) {
	query := `
        SELECT u.id, u.username, u.email, u.created_at, u.updated_at, u.active, u.role, u.status, u.email_verified
        FROM users u
        JOIN api_keys k ON u.id = k.user_id
        WHERE k.key = $1 AND k.active = true
    `

	var user types.User

	err := s.DB.QueryRowContext(ctx, query, apiKey).Scan(
		&user.ID,
		&user.Username,
		&user.Email,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.Active,
		&user.Role,
		&user.Status,
		&user.EmailVerified,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get user by API key: %w", err)
	}

	return &user, nil
}

// CheckResourceOwnership checks if a user owns a resource
func (s *Service) CheckResourceOwnership(ctx context.Context, userID, resourceType, resourceID string) (bool, error) {
	var count int
	var query string

	switch resourceType {
	case "workspace":
		query = "SELECT COUNT(*) FROM workspaces WHERE id = $1 AND user_id = $2"
	default:
		return false, fmt.Errorf("unsupported resource type: %s", resourceType)
	}

	err := s.DB.QueryRowContext(ctx, query, resourceID, userID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check resource ownership: %w", err)
	}

	return count > 0, nil
}

// CheckPermission checks if a user has permission to perform an action on a resource
func (s *Service) CheckPermission(ctx context.Context, userID, resourceType, resourceID, action string) (bool, error) {
	var count int
	query := `
		SELECT COUNT(*) FROM permissions
		WHERE user_id = $1
		AND resource_type = $2
		AND (resource_id = $3 OR resource_id = '*')
		AND (action = $4 OR action = '*')
	`

	err := s.DB.QueryRowContext(ctx, query, userID, resourceType, resourceID, action).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check permission: %w", err)
	}

	return count > 0, nil
}

// GetWorkspace gets a workspace by ID.
func (s *Service) GetWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	if workspaceID == "" {
		return nil, nil
	}
	query := `
        SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at,
               COALESCE(w.default_model, '') AS default_model,
               COALESCE(s.pending_refresh, FALSE) AS agent_needs_refresh,
               s.last_credential_changed_at AS credentials_pending_since,
               w.org_id
        FROM workspaces w
        LEFT JOIN workspace_agent_state s ON s.workspace_id = w.id
        WHERE w.id = $1
    `
	var ws types.WorkspaceMetadata
	err := s.DB.QueryRowContext(ctx, query, workspaceID).Scan(
		&ws.ID,
		&ws.UserID,
		&ws.Name,
		&ws.Runtime,
		&ws.StorageSize,
		&ws.ImageTag,
		&ws.AgentVersion,
		&ws.CreatedAt,
		&ws.UpdatedAt,
		&ws.DefaultModel,
		&ws.AgentNeedsRefresh,
		&ws.CredentialsPendingSince,
		&ws.OrgID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get workspace: %w", err)
	}
	return &ws, nil
}

// CreateWorkspace inserts a new workspace record.
func (s *Service) CreateWorkspace(ctx context.Context, workspace *types.WorkspaceMetadata) error {
	query := `
        INSERT INTO workspaces (id, user_id, name, runtime, storage_size, org_id, created_at, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
    `
	now := time.Now()
	if workspace.CreatedAt.IsZero() {
		workspace.CreatedAt = now
	}
	if workspace.UpdatedAt.IsZero() {
		workspace.UpdatedAt = now
	}
	_, err := s.DB.ExecContext(ctx, query,
		workspace.ID,
		workspace.UserID,
		workspace.Name,
		workspace.Runtime,
		workspace.StorageSize,
		workspace.OrgID,
		workspace.CreatedAt,
		workspace.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create workspace: %w", err)
	}
	return nil
}

// UpdateWorkspace updates specific fields on a workspace record.
func (s *Service) UpdateWorkspace(ctx context.Context, workspaceID string, updates types.WorkspaceUpdates) error {
	query := "UPDATE workspaces SET updated_at = NOW()"
	args := []interface{}{}
	i := 0
	if updates.Name != nil {
		i++
		query += fmt.Sprintf(", name = $%d", i)
		args = append(args, *updates.Name)
	}
	if updates.DefaultModel != nil {
		i++
		query += fmt.Sprintf(", default_model = $%d", i)
		args = append(args, *updates.DefaultModel)
	}
	if i == 0 {
		return nil
	}
	query += fmt.Sprintf(" WHERE id = $%d", i+1) //nolint:gosec // G202: literal with placeholder bind
	args = append(args, workspaceID)
	_, err := s.DB.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to update workspace: %w", err)
	}
	return nil
}

// GetDefaultModel returns the workspace's configured default model, or "" if unset.
func (s *Service) GetDefaultModel(ctx context.Context, workspaceID string) (string, error) {
	var model sql.NullString
	err := s.DB.QueryRowContext(ctx, "SELECT default_model FROM workspaces WHERE id = $1", workspaceID).Scan(&model)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("get default model: %w", err)
	}
	return model.String, nil
}

// DeleteWorkspace removes a workspace record.
func (s *Service) DeleteWorkspace(ctx context.Context, workspaceID string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM workspaces WHERE id = $1", workspaceID)
	if err != nil {
		return fmt.Errorf("failed to delete workspace: %w", err)
	}
	return nil
}

// SyncWorkspaceVersionInfo updates image_tag and/or agent_version in the DB.
// Only non-empty values are written; passing an empty string for either field
// leaves the existing DB value untouched. This allows the CRD watcher to sync
// imageTag without clobbering agentVersion, which is sourced separately from
// agentd health checks.
func (s *Service) SyncWorkspaceVersionInfo(ctx context.Context, workspaceID, imageTag, agentVersion string) {
	if workspaceID == "" || (imageTag == "" && agentVersion == "") {
		return
	}
	var query string
	var args []any
	switch {
	case imageTag != "" && agentVersion != "":
		query = "UPDATE workspaces SET image_tag = $1, agent_version = $2, updated_at = NOW() WHERE id = $3 AND deleted_at IS NULL"
		args = []any{imageTag, agentVersion, workspaceID}
	case imageTag != "":
		query = "UPDATE workspaces SET image_tag = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL"
		args = []any{imageTag, workspaceID}
	default:
		query = "UPDATE workspaces SET agent_version = $1, updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL"
		args = []any{agentVersion, workspaceID}
	}
	_, err := s.DB.ExecContext(ctx, query, args...)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("failed to sync workspace version info to DB", err, "workspaceID", workspaceID)
		}
	}
}

// MarkWorkspaceDeleted soft-deletes a workspace by setting deleted_at and
// purges any user_secret_bindings rows pointing at it within a single
// transaction. The bindings table has no FK to workspaces.id (the column
// types differ historically) so a soft delete leaves orphan binding rows
// behind unless we clean up here explicitly. See Bug 11 in worklog 0085.
//
// The two writes are wrapped in a single transaction so an API-process
// crash between them cannot leave a soft-deleted workspace with orphan
// bindings (validator finding on Bug 11 follow-up).
func (s *Service) MarkWorkspaceDeleted(ctx context.Context, workspaceID string) {
	if workspaceID == "" {
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Error("failed to begin tx for workspace soft-delete", err, "workspaceID", workspaceID)
		}
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		"UPDATE workspaces SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL",
		workspaceID); err != nil {
		if s.Logger != nil {
			s.Logger.Error("failed to mark workspace deleted in DB", err, "workspaceID", workspaceID)
		}
		return
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM user_secret_bindings WHERE workspace_id = $1",
		workspaceID); err != nil {
		// Bindings DELETE failure rolls the entire transaction back:
		// neither the soft-delete nor the bindings purge land. The
		// caller's next reconcile retries from a clean state. We
		// prefer this over committing the soft-delete with orphan
		// bindings — the orphan rows are exactly the Bug-11 hazard
		// we are trying to eliminate.
		if s.Logger != nil {
			s.Logger.Warn("failed to delete user_secret_bindings for deleted workspace; rolling back entire tx",
				"workspaceID", workspaceID, "error", err.Error())
		}
		return
	}
	if err := tx.Commit(); err != nil {
		if s.Logger != nil {
			s.Logger.Error("failed to commit workspace soft-delete tx", err, "workspaceID", workspaceID)
		}
		return
	}
	committed = true
}

// ListWorkspaces lists workspaces owned by the user with pagination.
//
// Per Epic 43 decision D6, this returns only workspaces the user created
// (w.user_id = $1). The prior LEFT JOIN org_memberships + OR clause that let any
// org member see every other member's org workspace has been removed: members
// now see only their own workspaces. Org admins who need to see all org
// workspaces use the dedicated GET /orgs/:id/workspaces endpoint
// (OrgStore.ListOrgWorkspaces).
func (s *Service) ListWorkspaces(ctx context.Context, userID string, limit, offset int) ([]*types.WorkspaceMetadata, *types.PaginationMetadata, error) {
	// S1: filter out frozen workspaces — org-attributed workspaces where the
	// user is no longer a current member of the org (offboarded or org
	// soft-deleted). Personal workspaces (org_id IS NULL) are always shown.
	// The membership check mirrors IsOrgMember: joins organizations for the
	// deleted_at + status guards.
	membershipCondition := `
        AND (
            w.org_id IS NULL
            OR EXISTS (
                SELECT 1 FROM org_memberships m
                JOIN organizations o ON o.id = m.org_id
                WHERE m.org_id = w.org_id AND m.user_id = $1
                  AND o.deleted_at IS NULL AND o.status != 'suspended'
            )
        )`

	var total int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspaces w
		WHERE w.deleted_at IS NULL AND w.user_id = $1`+membershipCondition,
		userID,
	).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("failed to count workspaces: %w", err)
	}
	pagination := &types.PaginationMetadata{
		Total:  total,
		Start:  offset,
		End:    offset + limit,
		Limit:  limit,
		Offset: offset,
	}
	if pagination.End > total {
		pagination.End = total
	}
	if total == 0 {
		return []*types.WorkspaceMetadata{}, pagination, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
        SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version, w.created_at, w.updated_at,
               COALESCE(w.default_model, '') AS default_model,
               COALESCE(s.pending_refresh, FALSE) AS agent_needs_refresh,
               s.last_credential_changed_at AS credentials_pending_since,
               w.org_id
        FROM workspaces w
        LEFT JOIN workspace_agent_state s ON s.workspace_id = w.id
        WHERE w.deleted_at IS NULL AND w.user_id = $1`+membershipCondition+`
        ORDER BY w.created_at DESC
        LIMIT $2 OFFSET $3
    `, userID, limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	workspaces := make([]*types.WorkspaceMetadata, 0)
	for rows.Next() {
		var ws types.WorkspaceMetadata
		if err := rows.Scan(
			&ws.ID, &ws.UserID, &ws.Name, &ws.Runtime,
			&ws.StorageSize,
			&ws.ImageTag, &ws.AgentVersion,
			&ws.CreatedAt, &ws.UpdatedAt,
			&ws.DefaultModel,
			&ws.AgentNeedsRefresh, &ws.CredentialsPendingSince,
			&ws.OrgID,
		); err != nil {
			return nil, nil, fmt.Errorf("failed to scan workspace row: %w", err)
		}
		workspaces = append(workspaces, &ws)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("error iterating workspace rows: %w", err)
	}
	return workspaces, pagination, nil
}

func (s *Service) CountWorkspacesByUserAndOrg(ctx context.Context, userID, orgID string) (int, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces WHERE user_id = $1 AND org_id = $2 AND deleted_at IS NULL`,
		userID, orgID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count workspaces by user and org: %w", err)
	}
	return count, nil
}

// PurgeUserSecrets deletes every user-owned secret row for a user:
// provider_credentials (LLM provider keys) and user_secrets. It is
// called from the email password-reset flow to make the "your saved
// keys will be deleted" guarantee literal.
//
// The DEK reinitialisation that precedes this call already makes the
// old ciphertext cryptographically undecryptable; deleting the rows
// removes them outright and guarantees no future materialization can
// resurrect them. Both tables' dependents (workspace_credential_bindings,
// user_secret_bindings) reference the parent with ON DELETE CASCADE, so
// no orphaned binding rows remain. Rows deleted before this call by the
// DEK reinit's UPSERT (user_keys) are unaffected.
//
// Best-effort at the caller: a failure here does not undo the reset
// because the cryptographic erasure has already happened.
func (s *Service) PurgeUserSecrets(ctx context.Context, userID string) error {
	if _, err := s.DB.ExecContext(ctx,
		`DELETE FROM provider_credentials WHERE owner_type = 'user' AND owner_id = $1`,
		userID,
	); err != nil {
		return fmt.Errorf("delete user provider credentials: %w", err)
	}
	if _, err := s.DB.ExecContext(ctx,
		`DELETE FROM user_secrets WHERE user_id = $1`,
		userID,
	); err != nil {
		return fmt.Errorf("delete user secrets: %w", err)
	}
	return nil
}

func (s *Service) CountActiveWorkspacesByUserAndOrg(ctx context.Context, userID, orgID string) (int, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces w
		 WHERE w.user_id = $1 AND w.org_id = $2 AND w.deleted_at IS NULL
		 AND (
		   SELECT e.to_phase FROM workspace_lifecycle_events e
		   WHERE e.workspace_id = w.id
		   ORDER BY e.event_time DESC
		   LIMIT 1
		 ) = 'Active'`,
		userID, orgID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active workspaces by user and org: %w", err)
	}
	return count, nil
}

func (s *Service) CreateAPIKey(ctx context.Context, apiKey *types.APIKey) error {
	query := `
        INSERT INTO api_keys (id, user_id, key, name, active, created_at, expires_at, key_prefix, key_legacy,
                              decrypt_access, kek_salt, wrapped_dek, dek_synced, key_ciphertext, key_version, allowed_cidrs)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
    `
	var expiresAt interface{}
	if apiKey.ExpiresAt != nil {
		expiresAt = *apiKey.ExpiresAt
	}
	prefix := apiKey.Prefix
	if prefix == "" && len(apiKey.Key) >= 8 {
		prefix = apiKey.Key[:8]
	}
	_, err := s.DB.ExecContext(ctx, query,
		apiKey.ID,
		apiKey.UserID,
		apiKey.Key,
		apiKey.Name,
		apiKey.Active,
		apiKey.CreatedAt,
		expiresAt,
		prefix,
		apiKey.Legacy,
		apiKey.DecryptAccess,
		apiKey.KekSalt,
		apiKey.WrappedDEK,
		apiKey.DekSynced,
		apiKey.KeyCiphertext,
		apiKey.KeyVersion,
		toNullableStringArray(apiKey.AllowedCIDRs),
	)
	if err != nil {
		return fmt.Errorf("failed to create api key: %w", err)
	}
	return nil
}
func (s *Service) ListAPIKeys(ctx context.Context, userID string) ([]*types.APIKey, error) {
	query := `
        SELECT id, user_id, key, name, active, created_at, expires_at,
               COALESCE(decrypt_access, false), COALESCE(dek_synced, false),
               allowed_cidrs
        FROM api_keys
        WHERE user_id = $1
        ORDER BY created_at DESC
    `
	rows, err := s.DB.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	keys := make([]*types.APIKey, 0)
	for rows.Next() {
		var k types.APIKey
		var keyStr string
		var expiresAt sql.NullTime
		if err := rows.Scan(&k.ID, new(string), &keyStr, &k.Name, &k.Active, &k.CreatedAt, &expiresAt, &k.DecryptAccess, &k.DekSynced, pq.Array(&k.AllowedCIDRs)); err != nil {
			return nil, fmt.Errorf("failed to scan api key: %w", err)
		}
		k.Prefix = "lsp_"
		if expiresAt.Valid {
			k.ExpiresAt = &expiresAt.Time
		}
		keys = append(keys, &k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating api keys: %w", err)
	}
	return keys, nil
}

func (s *Service) GetAPIKey(ctx context.Context, userID, keyID string) (*types.APIKey, error) {
	query := `
        SELECT id, key, name, active, created_at, expires_at
        FROM api_keys
        WHERE id = $1 AND user_id = $2
    `
	var k types.APIKey
	var keyStr string
	var expiresAt sql.NullTime
	err := s.DB.QueryRowContext(ctx, query, keyID, userID).Scan(
		&k.ID, &keyStr, &k.Name, &k.Active, &k.CreatedAt, &expiresAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get api key: %w", err)
	}
	k.Prefix = "lsp_"
	if expiresAt.Valid {
		k.ExpiresAt = &expiresAt.Time
	}
	return &k, nil
}

func (s *Service) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1 AND user_id = $2", keyID, userID)
	if err != nil {
		return fmt.Errorf("failed to delete api key: %w", err)
	}
	return nil
}

func (s *Service) GetAPIKeyRecordByHash(ctx context.Context, keyHash string) (*types.APIKey, error) {
	query := `
		SELECT id, user_id, key, name, active, created_at, expires_at,
		       decrypt_access, kek_salt, wrapped_dek, dek_synced, key_ciphertext,
		       allowed_cidrs
		FROM api_keys
		WHERE key = $1 AND active = true
	`
	var k types.APIKey
	var expiresAt sql.NullTime
	var decryptAccess bool
	var kekSalt, wrappedDEK, keyCiphertext []byte
	var dekSynced bool

	err := s.DB.QueryRowContext(ctx, query, keyHash).Scan(
		&k.ID, &k.UserID, new(string), &k.Name, &k.Active, &k.CreatedAt, &expiresAt,
		&decryptAccess, &kekSalt, &wrappedDEK, &dekSynced, &keyCiphertext,
		pq.Array(&k.AllowedCIDRs),
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get api key by hash: %w", err)
	}
	k.DecryptAccess = decryptAccess
	k.KekSalt = kekSalt
	k.WrappedDEK = wrappedDEK
	k.DekSynced = dekSynced
	k.KeyCiphertext = keyCiphertext
	if expiresAt.Valid {
		t := expiresAt.Time
		k.ExpiresAt = &t
	}
	return &k, nil
}

func (s *Service) UpdateAPIKeyDEK(ctx context.Context, keyID string, wrappedDEK, kekSalt []byte, synced bool) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE api_keys SET wrapped_dek = $1, kek_salt = $2, dek_synced = $3 WHERE id = $4`,
		wrappedDEK, kekSalt, synced, keyID)
	if err != nil {
		return fmt.Errorf("failed to update api key DEK: %w", err)
	}
	return nil
}

func (s *Service) ListAPIKeysWithDecrypt(ctx context.Context, userID string) ([]*types.APIKey, error) {
	query := `
		SELECT id, user_id, key, name, active, created_at, expires_at,
		       decrypt_access, kek_salt, wrapped_dek, dek_synced, key_ciphertext, key_version
		FROM api_keys
		WHERE user_id = $1 AND decrypt_access = true AND active = true
		ORDER BY created_at DESC
	`
	rows, err := s.DB.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list api keys with decrypt: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []*types.APIKey
	for rows.Next() {
		var k types.APIKey
		var keyStr string
		var expiresAt sql.NullTime
		var kekSalt, wrappedDEK, keyCiphertext []byte
		var dekSynced bool

		if err := rows.Scan(
			&k.ID, new(string), &keyStr, &k.Name, &k.Active, &k.CreatedAt, &expiresAt,
			&k.DecryptAccess, &kekSalt, &wrappedDEK, &dekSynced, &keyCiphertext, &k.KeyVersion,
		); err != nil {
			return nil, fmt.Errorf("failed to scan api key: %w", err)
		}
		k.KekSalt = kekSalt
		k.WrappedDEK = wrappedDEK
		k.DekSynced = dekSynced
		k.KeyCiphertext = keyCiphertext
		if expiresAt.Valid {
			t := expiresAt.Time
			k.ExpiresAt = &t
		}
		keys = append(keys, &k)
	}
	return keys, rows.Err()
}

// --- Session Index DB methods (Phase A) ---

func (s *Service) ListSessionIndex(ctx context.Context, workspaceID string) ([]types.SessionListItem, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT session_id, title, parent_session_id, last_message_at, message_count,
		        last_seen_at,
		        (last_message_at IS NOT NULL
		         AND (last_seen_at IS NULL OR last_message_at > last_seen_at)) AS has_unread,
		        context_used
		 FROM session_index WHERE workspace_id = $1
		 ORDER BY last_message_at DESC NULLS LAST LIMIT 100`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]types.SessionListItem, 0)
	for rows.Next() {
		var item types.SessionListItem
		var title sql.NullString
		var parentID sql.NullString
		var lastMsg sql.NullTime
		var lastSeen sql.NullTime
		var contextUsed sql.NullInt64
		if err := rows.Scan(&item.ID, &title, &parentID, &lastMsg, &item.MessageCount, &lastSeen, &item.HasUnread, &contextUsed); err != nil {
			return nil, err
		}
		if title.Valid {
			item.Title = title.String
		}
		if parentID.Valid {
			item.ParentID = parentID.String
		}
		if lastMsg.Valid {
			t := lastMsg.Time
			item.LastMessageAt = &t
		}
		if lastSeen.Valid {
			t := lastSeen.Time
			item.LastSeenAt = &t
		}
		if contextUsed.Valid {
			v := contextUsed.Int64
			item.ContextUsed = &v
		}
		item.Status = "idle"
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) DeleteSessionIndex(ctx context.Context, workspaceID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM session_index WHERE workspace_id = $1`, workspaceID)
	return err
}

func (s *Service) DeleteSessionTree(ctx context.Context, workspaceID, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT session_id FROM session_index
			WHERE workspace_id = $1 AND session_id = $2
			UNION ALL
			SELECT si.session_id FROM session_index si
			INNER JOIN descendants d ON si.parent_session_id = d.session_id AND si.workspace_id = $1
		)
		DELETE FROM session_index
		WHERE workspace_id = $1 AND session_id IN (SELECT session_id FROM descendants)`, workspaceID, sessionID)
	return err
}

func (s *Service) UpsertSessionMessage(ctx context.Context, workspaceID, sessionID string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, last_message_at, message_count, updated_at)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   last_message_at = EXCLUDED.last_message_at,
		   message_count = session_index.message_count + 1,
		   updated_at = NOW()`, workspaceID, sessionID, at)
	return err
}

func (s *Service) UpsertSessionTitle(ctx context.Context, workspaceID, sessionID, title string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, title, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   title = EXCLUDED.title,
		   updated_at = NOW()`, workspaceID, sessionID, title)
	return err
}

// UpsertSessionContextUsed persists the prompt token count for the most recent
// LLM step in this session. Called by the API proxy on every
// session.next.step.ended SSE event. Idempotent: concurrent writes of the same
// value are safe (both replicas receive the same event and write the same data).
func (s *Service) UpsertSessionContextUsed(ctx context.Context, workspaceID, sessionID string, contextUsed int64) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, context_used, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   context_used = EXCLUDED.context_used,
		   updated_at = NOW()`, workspaceID, sessionID, contextUsed)
	return err
}

// UpsertSessionParent records (or refreshes) the parent_session_id for a
// session. Used to mirror opencode subagent (subtask) parent links into the
// sidebar's session_index so the UI can render the hierarchy without
// round-tripping the agent.
//
// Idempotent: passing the same parentID is a no-op. We deliberately do not
// guard against parentID changes — opencode never re-parents a session in
// practice, and an UPDATE-on-conflict path costs less than a SELECT-then-
// UPDATE round trip.
func (s *Service) UpsertSessionParent(ctx context.Context, workspaceID, sessionID, parentID string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, parent_session_id, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   parent_session_id = EXCLUDED.parent_session_id,
		   updated_at = NOW()`, workspaceID, sessionID, parentID)
	return err
}

func (s *Service) UpdateSessionLastSeen(ctx context.Context, workspaceID, sessionID string) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO session_index (workspace_id, session_id, last_seen_at, message_count, updated_at)
		 VALUES ($1, $2, NOW(), 0, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   last_seen_at = NOW(),
		   updated_at = NOW()`, workspaceID, sessionID)
	return err
}

// BeginTx starts a new database transaction. Used by handlers that need
// multi-statement atomicity (e.g., AgentReloadHandler's SELECT FOR UPDATE + UPSERT).
func (s *Service) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return s.DB.BeginTx(ctx, opts)
}

// MarkCredentialChanged flips a workspace into "credentials staged, reload needed" state.
// Uses a single auto-commit UPSERT (no external transaction parameter) because
// the binding write (PgSecretStore, pgxpool) and this write (*sql.DB) use
// incompatible connection pools — cross-pool transactions are impossible.
func (s *Service) MarkCredentialChanged(ctx context.Context, workspaceID string) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO workspace_agent_state
			(workspace_id, last_credential_changed_at, pending_refresh, updated_at)
		VALUES ($1, NOW(), TRUE, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
			last_credential_changed_at = NOW(),
			pending_refresh = TRUE,
			updated_at = NOW()
	`, workspaceID)
	if err != nil {
		return fmt.Errorf("mark credential changed: %w", err)
	}
	return nil
}

// GetLastCredentialChangedAt returns the most recent credential-changed
// timestamp for the workspace, or the zero time if no row exists.
func (s *Service) GetLastCredentialChangedAt(ctx context.Context, workspaceID string) (time.Time, error) {
	var t time.Time
	err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(last_credential_changed_at, '1970-01-01') FROM workspace_agent_state WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get last credential changed at: %w", err)
	}
	return t, nil
}

// MarkAgentReloaded clears pending_refresh after a successful dispose.
// Uses SELECT FOR UPDATE to serialize against concurrent MarkCredentialChanged.
// priorChangedAt is captured BEFORE dispose; if a new credential was staged
// during the dispose window, pending_refresh stays true.
// Returns the DB-clock timestamp written to last_agent_disposed_at.
func (s *Service) MarkAgentReloaded(ctx context.Context, tx *sql.Tx, workspaceID string, priorChangedAt time.Time) (time.Time, error) {
	var currentChangedAt time.Time
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(last_credential_changed_at, '1970-01-01')
		 FROM workspace_agent_state
		 WHERE workspace_id = $1
		 FOR UPDATE`,
		workspaceID,
	).Scan(&currentChangedAt)
	if err == sql.ErrNoRows {
		return time.Time{}, apierrors.ErrNoAgentStateRow
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("lock workspace_agent_state: %w", err)
	}

	// pending_refresh stays true if a credential was staged during dispose window.
	newPendingRefresh := currentChangedAt.After(priorChangedAt)

	var disposedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO workspace_agent_state
			(workspace_id, last_agent_disposed_at, pending_refresh, updated_at)
		VALUES ($1, NOW(), $2, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
			last_agent_disposed_at = NOW(),
			pending_refresh = $2,
			updated_at = NOW()
		RETURNING last_agent_disposed_at
	`, workspaceID, newPendingRefresh).Scan(&disposedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("mark agent reloaded: %w", err)
	}
	return disposedAt, nil
}

// GetLastSeenPodIdentity returns the pod-identity tuple last observed for
// the workspace by the API's status-read path, used to detect pod recreations
// so the auto-push of user-DEK secrets can fire.
//
// (name, startTime) form the tuple: both change on every pod recreation
// (the controller writes both in phase_creating.go when a pod becomes Active).
// An absent row or NULL columns surface as ("", zero-time, nil error) —
// callers treat this as "no observation yet" and MUST NOT trigger an
// auto-push on the first observation; they simply record the current
// identity so subsequent transitions can be detected. This avoids a
// spurious push on the initial status poll after deploy (existing
// workspaces have no row until the first observation is written).
func (s *Service) GetLastSeenPodIdentity(ctx context.Context, workspaceID string) (string, time.Time, error) {
	var name string
	var startTime time.Time
	err := s.DB.QueryRowContext(ctx,
		`SELECT COALESCE(last_seen_pod_name, ''),
		        COALESCE(last_seen_pod_start_time, '1970-01-01'::timestamptz)
		 FROM workspace_agent_state
		 WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&name, &startTime)
	if err == sql.ErrNoRows {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("get last seen pod identity: %w", err)
	}
	// A row can exist with NULL identity columns (e.g., created by
	// MarkCredentialChanged before this feature). The COALESCE above
	// substitutes ''/epoch; treat epoch as the zero identity.
	if startTime.Unix() == 0 {
		return name, time.Time{}, nil
	}
	return name, startTime, nil
}

// UpsertLastSeenPodIdentity records the currently-observed pod identity
// WITHOUT touching pending_refresh. Used on the initial observation
// (no prior row) so the API remembers the current pod without triggering
// an auto-push. Auto-push is triggered separately via
// MarkPodIdentityTransition.
func (s *Service) UpsertLastSeenPodIdentity(ctx context.Context, workspaceID, podName string, startTime time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO workspace_agent_state
			(workspace_id, last_seen_pod_name, last_seen_pod_start_time, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
			last_seen_pod_name = EXCLUDED.last_seen_pod_name,
			last_seen_pod_start_time = EXCLUDED.last_seen_pod_start_time,
			updated_at = NOW()
	`, workspaceID, podName, startTime)
	if err != nil {
		return fmt.Errorf("upsert last seen pod identity: %w", err)
	}
	return nil
}

// MarkPodIdentityTransition atomically records a NEW pod identity AND
// flips pending_refresh=TRUE with last_credential_changed_at=NOW(). The
// combined write is important: pending_refresh must be true BEFORE the
// caller fires the fire-and-forget auto-push goroutine so a concurrent
// GetWorkspace list-read (which surfaces agentNeedsRefresh) sees the
// pending state and the AgentReloadBanner appears as the fallback UX
// while the auto-push is in flight. On success the auto-push clears
// pending_refresh via MarkAgentReloaded; on failure it stays TRUE and
// the banner remains visible.
func (s *Service) MarkPodIdentityTransition(ctx context.Context, workspaceID, podName string, startTime time.Time) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO workspace_agent_state
			(workspace_id, last_seen_pod_name, last_seen_pod_start_time,
			 last_credential_changed_at, pending_refresh, updated_at)
		VALUES ($1, $2, $3, NOW(), TRUE, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
			last_seen_pod_name = EXCLUDED.last_seen_pod_name,
			last_seen_pod_start_time = EXCLUDED.last_seen_pod_start_time,
			last_credential_changed_at = NOW(),
			pending_refresh = TRUE,
			updated_at = NOW()
	`, workspaceID, podName, startTime)
	if err != nil {
		return fmt.Errorf("mark pod identity transition: %w", err)
	}
	return nil
}

// ListPendingReloadWorkspaces returns workspaces with pending_refresh=TRUE for the given user.
func (s *Service) ListPendingReloadWorkspaces(ctx context.Context, userID string) ([]*types.WorkspaceMetadata, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size, w.image_tag, w.agent_version,
		       w.created_at, w.updated_at,
		       COALESCE(w.default_model, '') AS default_model,
		       TRUE AS agent_needs_refresh,
		       s.last_credential_changed_at AS credentials_pending_since
		FROM workspaces w
		JOIN workspace_agent_state s ON s.workspace_id = w.id
		WHERE w.user_id = $1
		  AND w.deleted_at IS NULL
		  AND s.pending_refresh = TRUE
		ORDER BY w.created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list pending reload workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []*types.WorkspaceMetadata
	for rows.Next() {
		var ws types.WorkspaceMetadata
		if err := rows.Scan(
			&ws.ID, &ws.UserID, &ws.Name, &ws.Runtime,
			&ws.StorageSize, &ws.ImageTag, &ws.AgentVersion,
			&ws.CreatedAt, &ws.UpdatedAt,
			&ws.DefaultModel,
			&ws.AgentNeedsRefresh, &ws.CredentialsPendingSince,
		); err != nil {
			return nil, fmt.Errorf("scan pending reload workspace: %w", err)
		}
		items = append(items, &ws)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending reload workspaces: %w", err)
	}
	return items, nil
}

func toNullableStringArray(s []string) interface{} {
	if len(s) == 0 {
		return nil
	}
	return pq.Array(s)
}

func (s *Service) ListAllWorkspaceOwners(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_id FROM workspaces WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("failed to list workspace owners: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]string)
	for rows.Next() {
		var id, userID string
		if err := rows.Scan(&id, &userID); err != nil {
			return nil, fmt.Errorf("failed to scan workspace owner: %w", err)
		}
		result[id] = userID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return result, nil
}

type WorkspaceBillingRecord struct {
	ID          string
	UserID      string
	StorageSize string
}

func (s *Service) ListAllWorkspacesForBilling(ctx context.Context) ([]WorkspaceBillingRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, user_id, storage_size FROM workspaces WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("failed to list workspaces for billing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []WorkspaceBillingRecord
	for rows.Next() {
		var r WorkspaceBillingRecord
		if err := rows.Scan(&r.ID, &r.UserID, &r.StorageSize); err != nil {
			return nil, fmt.Errorf("failed to scan workspace billing record: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return records, nil
}
