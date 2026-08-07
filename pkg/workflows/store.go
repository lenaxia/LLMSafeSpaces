// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package workflows contains the storage layer for Epic 64 (triggers & workflows).
//
// Pure data-access on the seven Epic 64 tables. Crypto (encrypt/decrypt of
// webhook HMAC secrets) is NOT done here — handlers encrypt before calling
// CreateWebhook; the webhook receiver decrypts after calling GetWebhook.
// This mirrors the pkg/secrets/mcp_store.go split exactly.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Get/Update/Delete when the row does not exist
// (or the caller's owner scope does not match). Handlers map this to 404.
var ErrNotFound = errors.New("workflow: not found")

// ErrConcurrentRun is returned by CreateWorkflowRun when the single-in-flight
// partial unique index (uq_workflow_run_single_inflight) rejects the insert
// because another queued/running run exists for the same workflow_id.
// Handlers map this to 409 "already running".
var ErrConcurrentRun = errors.New("workflow: a run is already in flight for this workflow")

// ErrDedupConflict is returned by RecordWebhookDelivery when the
// (webhook_id, dedup_key) UNIQUE constraint rejects a duplicate delivery.
// Handlers map this to 200 "duplicate" (idempotent re-delivery).
var ErrDedupConflict = errors.New("workflow: duplicate webhook delivery")

// Store is the Postgres-backed data-access layer for Epic 64. Construct once
// at app boot via NewStore; share across handlers. Methods are safe for
// concurrent use (pgxpool serializes connections internally).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store backed by the given pgxpool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// --- DB row shapes ----------------------------------------------------------

// WorkflowRow is the DB row shape for workflows.
type WorkflowRow struct {
	ID                string
	OwnerType         string
	OwnerID           string
	Name              string
	Slug              string
	Description       string
	SpecYAML          string
	SpecJSON          json.RawMessage
	InputSchema       json.RawMessage
	TargetWorkspaceID *string
	Status            string
	Defaults          json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TriggerRow is the DB row shape for triggers.
type TriggerRow struct {
	ID                  string
	OwnerType           string
	OwnerID             string
	Name                string
	Description         string
	Enabled             bool
	SourceType          string
	SourceConfig        json.RawMessage
	TargetType          string
	TargetConfig        json.RawMessage
	ConsecutiveFailures int
	AutoDisableAfter    int
	LastFiredAt         *time.Time
	NextFireAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// WebhookRow is the DB row shape for webhooks. SecretCipher is the encrypted
// HMAC secret (crypto envelope); callers must decrypt before verifying signatures.
type WebhookRow struct {
	ID                string
	TriggerID         string
	SecretCipher      []byte
	KeyVersion        int
	AllowedIPs        []string
	IdempotencyMode   string
	IdempotencyHeader string
	CreatedAt         time.Time
}

// WorkflowRunRow is the DB row shape for workflow_runs.
type WorkflowRunRow struct {
	ID            string
	WorkflowID    string
	SpecSnapshot  json.RawMessage
	Input         json.RawMessage
	Output        json.RawMessage
	Status        string
	ErrorCode     *string
	Error         json.RawMessage
	TriggerID     *string
	TriggerFireID *string
	WorkspaceID   string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// WorkflowNodeRunRow is the DB row shape for workflow_node_runs.
type WorkflowNodeRunRow struct {
	ID            string
	WorkflowRunID string
	NodeID        string
	NodeType      string
	Status        string
	Attempt       int
	Input         json.RawMessage
	Output        json.RawMessage
	Branch        *string
	ErrorCode     *string
	Error         json.RawMessage
	StartedAt     time.Time
	FinishedAt    *time.Time
}

// TriggerFireRow is the DB row shape for trigger_fires.
type TriggerFireRow struct {
	ID            string
	TriggerID     string
	SourceType    string
	InputEnvelope json.RawMessage
	ActionType    string
	ActionResult  json.RawMessage
	Status        string
	FiredAt       time.Time
	CompletedAt   *time.Time
}

// WorkflowUpdate carries only the fields a partial update may change. Pointer
// fields: nil means "keep existing". This mirrors the API DTO pattern
// (UpdateWorkflowRequest) so the handler can pass fields through directly.
// The zero-value WorkflowUpdate preserves every field (no changes).
type WorkflowUpdate struct {
	Name              *string
	Slug              *string
	Description       *string
	SpecYAML          *string
	SpecJSON          json.RawMessage
	InputSchema       json.RawMessage
	TargetWorkspaceID *string // nil = keep; non-nil "" = clear (set NULL); non-nil "uuid" = set
	Status            *string
	Defaults          json.RawMessage
}

// TriggerUpdate carries only the fields a partial update may change. Pointer
// fields: nil means "keep existing". source_type is NOT in this struct — it's
// immutable after create (the source defines the trigger's identity).
type TriggerUpdate struct {
	Name             *string
	Description      *string
	Enabled          *bool
	SourceConfig     json.RawMessage
	TargetType       *string
	TargetConfig     json.RawMessage
	AutoDisableAfter *int
}

// --- Workflow CRUD ----------------------------------------------------------

// CreateWorkflow inserts a row into workflows. The caller supplies a
// pre-generated UUID. spec_json must already be parsed + validated (US-64.4).
func (s *Store) CreateWorkflow(ctx context.Context, row *WorkflowRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workflows (id, owner_type, owner_id, name, slug, description, spec_yaml, spec_json, input_schema, target_workspace_id, status, defaults, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, ''), $7, $8, $9, $10, COALESCE($11, 'draft'), COALESCE($12, '{}'::jsonb), $13, $14)
	`, row.ID, row.OwnerType, row.OwnerID, row.Name, row.Slug, row.Description,
		row.SpecYAML, row.SpecJSON, nullableJSON(row.InputSchema), nullableStrPtr(row.TargetWorkspaceID),
		row.Status, nullableJSON(row.Defaults), row.CreatedAt, row.UpdatedAt)
	return err
}

// GetWorkflow returns a single workflow by ID scoped to (ownerType, ownerID),
// or ErrNotFound if not found / wrong scope.
func (s *Store) GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*WorkflowRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, owner_type, owner_id, name, slug, description, spec_yaml, spec_json, input_schema, target_workspace_id, status, defaults, created_at, updated_at
		FROM workflows WHERE id = $1 AND owner_type = $2 AND owner_id = $3
	`, workflowID, ownerType, ownerID)
	r, err := scanWorkflowRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListWorkflows returns all workflows owned by (ownerType, ownerID), ordered
// by created_at ASC. Never decrypts — display fields only.
func (s *Store) ListWorkflows(ctx context.Context, ownerType, ownerID string) ([]*WorkflowRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_type, owner_id, name, slug, description, spec_yaml, spec_json, input_schema, target_workspace_id, status, defaults, created_at, updated_at
		FROM workflows WHERE owner_type = $1 AND owner_id = $2
		ORDER BY created_at ASC
	`, ownerType, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkflowRows(rows)
}

// UpdateWorkflow updates an existing workflow scoped to (ownerType, ownerID).
// Partial update via WorkflowUpdate pointer fields: nil preserves existing
// values. Returns the updated row or ErrNotFound.
func (s *Store) UpdateWorkflow(ctx context.Context, ownerType, ownerID, workflowID string, upd *WorkflowUpdate) (*WorkflowRow, error) {
	row := &WorkflowRow{}
	err := s.pool.QueryRow(ctx, `
		UPDATE workflows
		SET name = COALESCE($4, name),
		    slug = COALESCE($5, slug),
		    description = COALESCE($6, description),
		    spec_yaml = COALESCE($7, spec_yaml),
		    spec_json = CASE WHEN $8::jsonb IS NOT NULL THEN $8 ELSE spec_json END,
		    input_schema = CASE WHEN $9::jsonb IS NOT NULL THEN $9 ELSE input_schema END,
		    target_workspace_id = CASE WHEN $10::boolean IS NULL THEN target_workspace_id
		                               WHEN $10::boolean = false THEN NULL
		                               ELSE $11::uuid END,
		    status = COALESCE($12, status),
		    defaults = CASE WHEN $13::jsonb IS NOT NULL THEN $13 ELSE defaults END
		WHERE id = $1 AND owner_type = $2 AND owner_id = $3
		RETURNING id, owner_type, owner_id, name, slug, description, spec_yaml, spec_json, input_schema, target_workspace_id, status, defaults, created_at, updated_at
	`,
		workflowID, ownerType, ownerID,
		upd.Name, upd.Slug, upd.Description, upd.SpecYAML,
		nullableJSON(upd.SpecJSON), nullableJSON(upd.InputSchema),
		targetWorkspaceUpdateFlag(upd.TargetWorkspaceID),  // $10: NULL=keep, false=clear, true=set
		targetWorkspaceUpdateValue(upd.TargetWorkspaceID), // $11: the uuid (or NULL)
		upd.Status,
		nullableJSON(upd.Defaults),
	).Scan(
		&row.ID, &row.OwnerType, &row.OwnerID, &row.Name, &row.Slug, &row.Description,
		&row.SpecYAML, &row.SpecJSON, &row.InputSchema, &row.TargetWorkspaceID,
		&row.Status, &row.Defaults, &row.CreatedAt, &row.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return row, err
}

// targetWorkspaceUpdateFlag returns the boolean flag for the CASE: NULL=keep,
// false=clear, true=set. Mirrors the three-state intent of *string.
func targetWorkspaceUpdateFlag(s *string) any {
	if s == nil {
		return nil // keep
	}
	if *s == "" {
		return false // clear
	}
	return true // set
}

// targetWorkspaceUpdateValue returns the uuid value (or nil) for the SET case.
func targetWorkspaceUpdateValue(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// DeleteWorkflow deletes a workflow by ID scoped to (ownerType, ownerID). FK
// cascades handle workflow_runs (ON DELETE CASCADE). Returns ErrNotFound if
// the workflow doesn't exist or the caller's scope doesn't match.
func (s *Store) DeleteWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1 AND owner_type = $2 AND owner_id = $3`, workflowID, ownerType, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountWorkflowsByOwner returns the number of workflows owned by (ownerType, ownerID).
// Used for quota enforcement (workflows.maxPerUser / workflows.maxPerOrg).
func (s *Store) CountWorkflowsByOwner(ctx context.Context, ownerType, ownerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflows WHERE owner_type = $1 AND owner_id = $2`, ownerType, ownerID).Scan(&n)
	return n, err
}

// --- Trigger CRUD -----------------------------------------------------------

// CreateTrigger inserts a row into triggers. The caller supplies a pre-generated
// UUID. For webhook triggers, an accompanying webhooks row is created via
// CreateWebhook in the same transaction by the handler (US-64.5).
func (s *Store) CreateTrigger(ctx context.Context, row *TriggerRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO triggers (id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, ''), COALESCE($6, true), $7, COALESCE($8, '{}'::jsonb), $9, COALESCE($10, '{}'::jsonb), COALESCE($11, 0), COALESCE($12, 10), $13, $14, $15, $16)
	`, row.ID, row.OwnerType, row.OwnerID, row.Name, row.Description, row.Enabled,
		row.SourceType, nullableJSON(row.SourceConfig), row.TargetType, nullableJSON(row.TargetConfig),
		row.ConsecutiveFailures, row.AutoDisableAfter, row.LastFiredAt, row.NextFireAt,
		row.CreatedAt, row.UpdatedAt)
	return err
}

// GetTrigger returns a single trigger by ID scoped to (ownerType, ownerID),
// or ErrNotFound.
func (s *Store) GetTrigger(ctx context.Context, ownerType, ownerID, triggerID string) (*TriggerRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at
		FROM triggers WHERE id = $1 AND owner_type = $2 AND owner_id = $3
	`, triggerID, ownerType, ownerID)
	r, err := scanTriggerRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// GetTriggerByID returns a trigger by its UUID without owner scoping. Used by
// the webhook receiver where the trigger_id is authoritative (unguessable UUID).
func (s *Store) GetTriggerByID(ctx context.Context, triggerID string) (*TriggerRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at
		FROM triggers WHERE id = $1
	`, triggerID)
	r, err := scanTriggerRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListTriggers returns all triggers owned by (ownerType, ownerID), ordered by created_at ASC.
func (s *Store) ListTriggers(ctx context.Context, ownerType, ownerID string) ([]*TriggerRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at
		FROM triggers WHERE owner_type = $1 AND owner_id = $2
		ORDER BY created_at ASC
	`, ownerType, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTriggerRows(rows)
}

// UpdateTrigger updates an existing trigger scoped to (ownerType, ownerID).
// source_type is NOT mutable (not in TriggerUpdate). Partial update via pointer
// fields: nil preserves existing values. Returns the updated row or ErrNotFound.
func (s *Store) UpdateTrigger(ctx context.Context, ownerType, ownerID, triggerID string, upd *TriggerUpdate) (*TriggerRow, error) {
	row := &TriggerRow{}
	err := s.pool.QueryRow(ctx, `
		UPDATE triggers
		SET name = COALESCE($4, name),
		    description = COALESCE($5, description),
		    enabled = COALESCE($6, enabled),
		    source_config = CASE WHEN $7::jsonb IS NOT NULL THEN $7 ELSE source_config END,
		    target_type = COALESCE($8, target_type),
		    target_config = CASE WHEN $9::jsonb IS NOT NULL THEN $9 ELSE target_config END,
		    auto_disable_after = COALESCE($10, auto_disable_after)
		WHERE id = $1 AND owner_type = $2 AND owner_id = $3
		RETURNING id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at
	`,
		triggerID, ownerType, ownerID,
		upd.Name, upd.Description, upd.Enabled,
		nullableJSON(upd.SourceConfig), upd.TargetType, nullableJSON(upd.TargetConfig),
		upd.AutoDisableAfter,
	).Scan(
		&row.ID, &row.OwnerType, &row.OwnerID, &row.Name, &row.Description, &row.Enabled,
		&row.SourceType, &row.SourceConfig, &row.TargetType, &row.TargetConfig,
		&row.ConsecutiveFailures, &row.AutoDisableAfter, &row.LastFiredAt, &row.NextFireAt,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return row, err
}

// DeleteTrigger deletes a trigger by ID scoped to (ownerType, ownerID). FK
// cascades handle webhooks + trigger_fires. Returns ErrNotFound.
func (s *Store) DeleteTrigger(ctx context.Context, ownerType, ownerID, triggerID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM triggers WHERE id = $1 AND owner_type = $2 AND owner_id = $3`, triggerID, ownerType, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountTriggersByOwner returns the number of triggers owned by (ownerType, ownerID).
func (s *Store) CountTriggersByOwner(ctx context.Context, ownerType, ownerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM triggers WHERE owner_type = $1 AND owner_id = $2`, ownerType, ownerID).Scan(&n)
	return n, err
}

// --- Webhook (one row per webhook trigger) ----------------------------------

// CreateWebhook inserts a row into webhooks. The caller supplies a pre-encrypted
// SecretCipher (KEK for org scope, session-DEK for user scope — exact mcp_servers
// crypto envelope pattern). trigger_id has a 1:1 UNIQUE constraint.
func (s *Store) CreateWebhook(ctx context.Context, row *WebhookRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhooks (id, trigger_id, secret_cipher, key_version, allowed_ips, idempotency_mode, idempotency_header, created_at)
		VALUES ($1, $2, $3, COALESCE($4, 1), COALESCE($5, ARRAY[]::text[]), COALESCE($6, 'header'), COALESCE($7, 'X-Request-ID'), $8)
	`, row.ID, row.TriggerID, row.SecretCipher, row.KeyVersion,
		toNullableStringArray(row.AllowedIPs), row.IdempotencyMode, row.IdempotencyHeader, row.CreatedAt)
	return err
}

// GetWebhookByTriggerID returns the webhook config for a trigger, or ErrNotFound.
// Used by the webhook receiver to fetch the HMAC secret + IP allowlist.
func (s *Store) GetWebhookByTriggerID(ctx context.Context, triggerID string) (*WebhookRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, trigger_id, secret_cipher, key_version, allowed_ips, idempotency_mode, idempotency_header, created_at
		FROM webhooks WHERE trigger_id = $1
	`, triggerID)
	r, err := scanWebhookRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// GetWebhook returns a webhook by its own ID (the public webhook_id in the
// receiver URL path). Used by POST /api/v1/hooks/:webhook_id.
func (s *Store) GetWebhook(ctx context.Context, webhookID string) (*WebhookRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, trigger_id, secret_cipher, key_version, allowed_ips, idempotency_mode, idempotency_header, created_at
		FROM webhooks WHERE id = $1
	`, webhookID)
	r, err := scanWebhookRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// --- Webhook deliveries (idempotency) ---------------------------------------

// RecordWebhookDelivery inserts a dedup row. Returns ErrDedupConflict if a row
// with the same (webhook_id, dedup_key) already exists — the caller returns
// 200 "duplicate" in that case. The unique constraint name is
// webhook_deliveries_webhook_dedup_uniq.
func (s *Store) RecordWebhookDelivery(ctx context.Context, webhookID, dedupKey string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (id, webhook_id, dedup_key, delivered_at)
		VALUES (gen_random_uuid(), $1, $2, now())
		ON CONFLICT (webhook_id, dedup_key) DO NOTHING
	`, webhookID, dedupKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrDedupConflict
	}
	return nil
}

// --- Workflow runs (execution state machine) -------------------------------

// CreateWorkflowRun inserts a new run row. The single-in-flight partial unique
// index (uq_workflow_run_single_inflight) may reject the insert with a unique
// violation — mapped to ErrConcurrentRun so the handler can return 409 +
// Retry-After. Callers should use CreateWorkflowRunTx (with the fire row) for
// webhook-triggered runs; this method is for manual runs.
func (s *Store) CreateWorkflowRun(ctx context.Context, row *WorkflowRunRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workflow_runs (id, workflow_id, spec_snapshot, input, output, status, error_code, error, trigger_id, trigger_fire_id, workspace_id, started_at, finished_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, row.ID, row.WorkflowID, row.SpecSnapshot, nullableJSON(row.Input), nullableJSON(row.Output),
		row.Status, nullableStrPtr(row.ErrorCode), nullableJSON(row.Error),
		nullableStrPtr(row.TriggerID), nullableStrPtr(row.TriggerFireID), row.WorkspaceID,
		row.StartedAt, row.FinishedAt, row.CreatedAt, row.UpdatedAt)
	if isUniqueViolation(err) {
		return ErrConcurrentRun
	}
	return err
}

// CreateWorkflowRunWithFire atomically inserts a trigger_fires row AND a
// workflow_runs row in a single transaction. If the run insert hits the
// single-in-flight unique violation, the whole tx rolls back (no orphan
// fire row claiming "fired" with no run). The caller then commits a separate
// trigger_fires row with status='skipped' + reason='already_running' and
// returns 409 + Retry-After.
//
// This is the v1 correctness pattern for webhook fire-row + run-create atomicity
// (design Edge Case 4 / Webhook Security atomicity bullet).
func (s *Store) CreateWorkflowRunWithFire(ctx context.Context, fire *TriggerFireRow, run *WorkflowRunRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, input_envelope, action_type, action_result, status, fired_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, fire.ID, fire.TriggerID, fire.SourceType, nullableJSON(fire.InputEnvelope),
		fire.ActionType, nullableJSON(fire.ActionResult), fire.Status, fire.FiredAt, fire.CompletedAt)
	if err != nil {
		return fmt.Errorf("insert trigger_fire: %w", err)
	}

	run.TriggerFireID = &fire.ID
	_, err = tx.Exec(ctx, `
		INSERT INTO workflow_runs (id, workflow_id, spec_snapshot, input, output, status, error_code, error, trigger_id, trigger_fire_id, workspace_id, started_at, finished_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, run.ID, run.WorkflowID, run.SpecSnapshot, nullableJSON(run.Input), nullableJSON(run.Output),
		run.Status, nullableStrPtr(run.ErrorCode), nullableJSON(run.Error),
		nullableStrPtr(run.TriggerID), nullableStrPtr(run.TriggerFireID), run.WorkspaceID,
		run.StartedAt, run.FinishedAt, run.CreatedAt, run.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConcurrentRun
		}
		return fmt.Errorf("insert workflow_run: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// ClaimQueuedRuns selects up to limit queued runs and atomically marks them
// running, returning the claimed rows. Uses FOR UPDATE SKIP LOCKED so multiple
// controller replicas (or multiple goroutines) can claim concurrently without
// contention. The reconciler polls this on each tick.
func (s *Store) ClaimQueuedRuns(ctx context.Context, limit int) ([]*WorkflowRunRow, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE workflow_runs
		SET status = 'running', started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id IN (
			SELECT id FROM workflow_runs
			WHERE status = 'queued'
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, workflow_id, spec_snapshot, input, output, status, error_code, error, trigger_id, trigger_fire_id, workspace_id, started_at, finished_at, created_at, updated_at
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkflowRunRows(rows)
}

// GetWorkflowRun returns a single run by ID, or ErrNotFound. Not scoped — run
// IDs are unguessable UUIDs and the caller has already authorized via the
// workflow's owner scope.
func (s *Store) GetWorkflowRun(ctx context.Context, runID string) (*WorkflowRunRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, workflow_id, spec_snapshot, input, output, status, error_code, error, trigger_id, trigger_fire_id, workspace_id, started_at, finished_at, created_at, updated_at
		FROM workflow_runs WHERE id = $1
	`, runID)
	r, err := scanWorkflowRunRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListWorkflowRuns returns runs for a workflow, paginated by created_at DESC.
// limit must be > 0; offset is the number of rows to skip.
func (s *Store) ListWorkflowRuns(ctx context.Context, workflowID string, limit, offset int) ([]*WorkflowRunRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workflow_id, spec_snapshot, input, output, status, error_code, error, trigger_id, trigger_fire_id, workspace_id, started_at, finished_at, created_at, updated_at
		FROM workflow_runs WHERE workflow_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, workflowID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkflowRunRows(rows)
}

// UpdateWorkflowRunStatus sets status + terminal fields. On a terminal
// transition (succeeded/failed/canceled/timed_out), finished_at is set.
// error_code + error are nullable (null on success).
func (s *Store) UpdateWorkflowRunStatus(ctx context.Context, runID, status string, errorCode *string, errMsg json.RawMessage, output json.RawMessage) error {
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow_runs
		SET status = $2,
		    error_code = $3,
		    error = $4,
		    output = CASE WHEN $5::jsonb IS NOT NULL THEN $5 ELSE output END,
		    finished_at = CASE WHEN $2 IN ('succeeded','failed','canceled','timed_out') THEN COALESCE(finished_at, now()) ELSE finished_at END,
		    updated_at = now()
		WHERE id = $1
		RETURNING id
	`, runID, status, nullableStrPtr(errorCode), nullableJSON(errMsg), nullableJSON(output)).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// HasInFlightRun reports whether a non-terminal run exists for the workflow.
// Used for fast-path rejection at the API layer (the partial unique index is
// the authoritative gate; this is an early-reject optimization).
func (s *Store) HasInFlightRun(ctx context.Context, workflowID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM workflow_runs WHERE workflow_id = $1 AND status IN ('queued','running'))
	`, workflowID).Scan(&exists)
	return exists, err
}

// --- Workflow node runs (per-node state) ------------------------------------

// CreateNodeRun inserts a workflow_node_runs row.
func (s *Store) CreateNodeRun(ctx context.Context, row *WorkflowNodeRunRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO workflow_node_runs (id, workflow_run_id, node_id, node_type, status, attempt, input, output, branch, error_code, error, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, row.ID, row.WorkflowRunID, row.NodeID, row.NodeType, row.Status, row.Attempt,
		nullableJSON(row.Input), nullableJSON(row.Output), nullableStrPtr(row.Branch),
		nullableStrPtr(row.ErrorCode), nullableJSON(row.Error), row.StartedAt, row.FinishedAt)
	return err
}

// UpdateNodeRunStatus sets status + terminal fields. On a terminal transition,
// finished_at is set. branch is set only on condition nodes (matched edge handle).
func (s *Store) UpdateNodeRunStatus(ctx context.Context, nodeRunID, status string, output json.RawMessage, branch *string, errorCode *string, errMsg json.RawMessage) error {
	err := s.pool.QueryRow(ctx, `
		UPDATE workflow_node_runs
		SET status = $2,
		    output = CASE WHEN $3::jsonb IS NOT NULL THEN $3 ELSE output END,
		    branch = CASE WHEN $4::text IS NOT NULL THEN $4 ELSE branch END,
		    error_code = $5,
		    error = $6,
		    finished_at = CASE WHEN $2 IN ('succeeded','failed','skipped') THEN COALESCE(finished_at, now()) ELSE finished_at END
		WHERE id = $1
		RETURNING id
	`, nodeRunID, status, nullableJSON(output), nullableStrPtr(branch),
		nullableStrPtr(errorCode), nullableJSON(errMsg)).Scan(&nodeRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ListNodeRuns returns all node runs for a workflow run, ordered by started_at ASC.
func (s *Store) ListNodeRuns(ctx context.Context, workflowRunID string) ([]*WorkflowNodeRunRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, workflow_run_id, node_id, node_type, status, attempt, input, output, branch, error_code, error, started_at, finished_at
		FROM workflow_node_runs WHERE workflow_run_id = $1
		ORDER BY started_at ASC
	`, workflowRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRunRows(rows)
}

// --- Trigger fires (audit log) ----------------------------------------------

// CreateTriggerFire inserts a trigger_fires row.
func (s *Store) CreateTriggerFire(ctx context.Context, row *TriggerFireRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, input_envelope, action_type, action_result, status, fired_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, row.ID, row.TriggerID, row.SourceType, nullableJSON(row.InputEnvelope),
		row.ActionType, nullableJSON(row.ActionResult), row.Status, row.FiredAt, row.CompletedAt)
	return err
}

// ListTriggerFires returns recent fires for a trigger, paginated by fired_at DESC.
func (s *Store) ListTriggerFires(ctx context.Context, triggerID string, limit, offset int) ([]*TriggerFireRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, trigger_id, source_type, input_envelope, action_type, action_result, status, fired_at, completed_at
		FROM trigger_fires WHERE trigger_id = $1
		ORDER BY fired_at DESC
		LIMIT $2 OFFSET $3
	`, triggerID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTriggerFireRows(rows)
}

// IncrementTriggerFailures increments consecutive_failures and returns the new
// value. If the new value crosses auto_disable_after, the caller sets enabled=false.
func (s *Store) IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		UPDATE triggers SET consecutive_failures = consecutive_failures + 1, updated_at = now()
		WHERE id = $1 RETURNING consecutive_failures
	`, triggerID).Scan(&n)
	return n, err
}

// ResetTriggerFailures resets consecutive_failures to 0 (called on first success).
func (s *Store) ResetTriggerFailures(ctx context.Context, triggerID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE triggers SET consecutive_failures = 0, updated_at = now() WHERE id = $1
	`, triggerID)
	return err
}

// DisableTrigger sets enabled=false (called when the circuit breaker trips).
func (s *Store) DisableTrigger(ctx context.Context, triggerID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE triggers SET enabled = false, updated_at = now() WHERE id = $1`, triggerID)
	return err
}

// UpdateTriggerFireTimestamps sets last_fired_at + advances next_fire_at for
// cron triggers after a fire. nextFireAt may be nil to clear it.
func (s *Store) UpdateTriggerFireTimestamps(ctx context.Context, triggerID string, lastFiredAt time.Time, nextFireAt *time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE triggers SET last_fired_at = $2, next_fire_at = $3, updated_at = now() WHERE id = $1
	`, triggerID, lastFiredAt, nextFireAt)
	return err
}

// ListDueCronTriggers returns enabled cron triggers whose next_fire_at <= now,
// ordered by next_fire_at ASC. Used by the scheduler goroutine (US-64.9).
func (s *Store) ListDueCronTriggers(ctx context.Context, now time.Time, limit int) ([]*TriggerRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_type, owner_id, name, description, enabled, source_type, source_config, target_type, target_config, consecutive_failures, auto_disable_after, last_fired_at, next_fire_at, created_at, updated_at
		FROM triggers
		WHERE source_type = 'cron' AND enabled = true AND next_fire_at IS NOT NULL AND next_fire_at <= $1
		ORDER BY next_fire_at ASC
		LIMIT $2
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTriggerRows(rows)
}

// --- Scanner helpers --------------------------------------------------------

func scanWorkflowRow(row pgx.Row) (*WorkflowRow, error) {
	var r WorkflowRow
	err := row.Scan(
		&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Slug, &r.Description,
		&r.SpecYAML, &r.SpecJSON, &r.InputSchema, &r.TargetWorkspaceID,
		&r.Status, &r.Defaults, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanWorkflowRows(rows pgx.Rows) ([]*WorkflowRow, error) {
	var out []*WorkflowRow
	for rows.Next() {
		r, err := scanWorkflowRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanTriggerRow(row pgx.Row) (*TriggerRow, error) {
	var r TriggerRow
	err := row.Scan(
		&r.ID, &r.OwnerType, &r.OwnerID, &r.Name, &r.Description, &r.Enabled,
		&r.SourceType, &r.SourceConfig, &r.TargetType, &r.TargetConfig,
		&r.ConsecutiveFailures, &r.AutoDisableAfter, &r.LastFiredAt, &r.NextFireAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanTriggerRows(rows pgx.Rows) ([]*TriggerRow, error) {
	var out []*TriggerRow
	for rows.Next() {
		r, err := scanTriggerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanWebhookRow(row pgx.Row) (*WebhookRow, error) {
	var r WebhookRow
	err := row.Scan(
		&r.ID, &r.TriggerID, &r.SecretCipher, &r.KeyVersion,
		&r.AllowedIPs, &r.IdempotencyMode, &r.IdempotencyHeader, &r.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanWorkflowRunRow(row pgx.Row) (*WorkflowRunRow, error) {
	var r WorkflowRunRow
	err := row.Scan(
		&r.ID, &r.WorkflowID, &r.SpecSnapshot, &r.Input, &r.Output,
		&r.Status, &r.ErrorCode, &r.Error, &r.TriggerID, &r.TriggerFireID,
		&r.WorkspaceID, &r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanWorkflowRunRows(rows pgx.Rows) ([]*WorkflowRunRow, error) {
	var out []*WorkflowRunRow
	for rows.Next() {
		r, err := scanWorkflowRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanNodeRunRow(row pgx.Row) (*WorkflowNodeRunRow, error) {
	var r WorkflowNodeRunRow
	err := row.Scan(
		&r.ID, &r.WorkflowRunID, &r.NodeID, &r.NodeType, &r.Status, &r.Attempt,
		&r.Input, &r.Output, &r.Branch, &r.ErrorCode, &r.Error,
		&r.StartedAt, &r.FinishedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanNodeRunRows(rows pgx.Rows) ([]*WorkflowNodeRunRow, error) {
	var out []*WorkflowNodeRunRow
	for rows.Next() {
		r, err := scanNodeRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanTriggerFireRow(row pgx.Row) (*TriggerFireRow, error) {
	var r TriggerFireRow
	err := row.Scan(
		&r.ID, &r.TriggerID, &r.SourceType, &r.InputEnvelope,
		&r.ActionType, &r.ActionResult, &r.Status, &r.FiredAt, &r.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanTriggerFireRows(rows pgx.Rows) ([]*TriggerFireRow, error) {
	var out []*TriggerFireRow
	for rows.Next() {
		r, err := scanTriggerFireRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- PG helpers -------------------------------------------------------------

// nullableJSON returns nil for empty RawMessage so pgx sends SQL NULL; otherwise
// the raw bytes (which are valid JSON by construction).
func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// nullableStrPtr dereferences a *string for SQL, returning nil for nil.
func nullableStrPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// toNullableStringArray converts a []string to a pgx-compatible value for text[]
// columns. Empty slice → nil (SQL NULL). pgx v5 handles []string natively for both
// encode and decode of text[] columns.
func toNullableStringArray(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}

// isUniqueViolation reports whether err is a Postgres unique_violation (SQLSTATE 23505).
// Used by CreateWorkflowRun to map the single-in-flight index violation to ErrConcurrentRun.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
