// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package workflows

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// StoreIntegrationSuite exercises the workflows store against a real PostgreSQL.
// Gated by the "integration" build tag; skipped when TEST_DATABASE_URL is unreachable.
type StoreIntegrationSuite struct {
	suite.Suite
	pool  *pgxpool.Pool
	store *Store
}

func TestStoreIntegrationSuite(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	suite.Run(t, &StoreIntegrationSuite{pool: pool, store: NewStore(pool)})
}

func (s *StoreIntegrationSuite) SetupTest() {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		TRUNCATE TABLE trigger_fires, workflow_node_runs, workflow_runs, webhook_deliveries, webhooks, triggers, workflows
		CASCADE
	`)
	s.Require().NoError(err)
	// Ensure a test user exists (workspaces.user_id FKs to users.id).
	// Schema (migration 000001): id, username, email, password_hash, active,
	// role, created_at, updated_at, plan_id, status, email_verified.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('test-user', 'test-user', 'test@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	s.Require().NoError(err)
}

// assertJSONEqual compares two JSON byte slices structurally (PG re-serializes
// jsonb with whitespace differences, so byte-equality fails round-trips).
func assertJSONEqual(t *testing.T, expected string, actual json.RawMessage) {
	t.Helper()
	var exp, got any
	require.NoError(t, json.Unmarshal([]byte(expected), &exp))
	require.NoError(t, json.Unmarshal(actual, &got))
	assert.Equal(t, exp, got)
}

func (s *StoreIntegrationSuite) newWorkspaceID() string {
	// Schema (migration 000001): id, name, user_id, namespace, runtime,
	// security_level, storage_size, created_at, updated_at, deleted_at,
	// image_tag, agent_version, default_model, org_id. No 'image' or 'phase'
	// column (those are on the K8s CRD, not the DB row). user_id FKs to users.id.
	id := uuid.New().String()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		 VALUES ($1, 'test-user', 'test-ws', 'default', now(), now())
		 ON CONFLICT (id) DO NOTHING`, id)
	s.Require().NoError(err)
	return id
}

// --- Workflow CRUD ---------------------------------------------------------

func (s *StoreIntegrationSuite) TestWorkflowCRUD() {
	ctx := context.Background()
	id := uuid.New().String()
	now := time.Now()

	created := &WorkflowRow{
		ID: id, OwnerType: "user", OwnerID: "u1",
		Name: "my-workflow", Slug: "my-workflow",
		Description: "test wf", SpecYAML: "name: test\n",
		SpecJSON:  json.RawMessage(`{"name":"test"}`),
		Status:    "draft",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(s.T(), s.store.CreateWorkflow(ctx, created))

	got, err := s.store.GetWorkflow(ctx, "user", "u1", id)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "my-workflow", got.Name)
	assert.Equal(s.T(), "test wf", got.Description)
	// Compare JSON structurally — PG re-serializes jsonb with a space after ':'.
	assertJSONEqual(s.T(), `{"name":"test"}`, got.SpecJSON)
	assert.Equal(s.T(), "draft", got.Status)

	// Partial update: only status changes. All other fields nil → preserved.
	statusActive := "active"
	updated, err := s.store.UpdateWorkflow(ctx, "user", "u1", id, &WorkflowUpdate{Status: &statusActive})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "active", updated.Status)
	assert.Equal(s.T(), "my-workflow", updated.Name, "name should be preserved (nil in update)")
	assert.Equal(s.T(), "test wf", updated.Description, "description should be preserved (nil in update)")

	// Partial update: set description to empty string (clear it).
	emptyDesc := ""
	updated2, err := s.store.UpdateWorkflow(ctx, "user", "u1", id, &WorkflowUpdate{Description: &emptyDesc})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "", updated2.Description, "description should be cleared")
	assert.Equal(s.T(), "active", updated2.Status, "status preserved")

	// List.
	list, err := s.store.ListWorkflows(ctx, "user", "u1")
	require.NoError(s.T(), err)
	assert.Len(s.T(), list, 1)

	// Cross-scope isolation: org cannot read user workflow.
	_, err = s.store.GetWorkflow(ctx, "org", "o1", id)
	assert.ErrorIs(s.T(), err, ErrNotFound)

	// Count.
	n, err := s.store.CountWorkflowsByOwner(ctx, "user", "u1")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 1, n)

	// Delete.
	require.NoError(s.T(), s.store.DeleteWorkflow(ctx, "user", "u1", id))
	_, err = s.store.GetWorkflow(ctx, "user", "u1", id)
	assert.ErrorIs(s.T(), err, ErrNotFound)

	// Delete again → ErrNotFound.
	err = s.store.DeleteWorkflow(ctx, "user", "u1", id)
	assert.ErrorIs(s.T(), err, ErrNotFound)
}

// --- Trigger CRUD ----------------------------------------------------------

func (s *StoreIntegrationSuite) TestTriggerCRUD() {
	ctx := context.Background()
	id := uuid.New().String()
	now := time.Now()

	created := &TriggerRow{
		ID: id, OwnerType: "user", OwnerID: "u1",
		Name: "nightly-backup", Enabled: true,
		SourceType:       "cron",
		SourceConfig:     json.RawMessage(`{"expr":"0 2 * * *","tz":"UTC"}`),
		TargetType:       "run_workflow",
		TargetConfig:     json.RawMessage(`{"workflowId":"wf_1"}`),
		AutoDisableAfter: 10,
		NextFireAt:       &now,
		CreatedAt:        now, UpdatedAt: now,
	}
	require.NoError(s.T(), s.store.CreateTrigger(ctx, created))

	got, err := s.store.GetTrigger(ctx, "user", "u1", id)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "nightly-backup", got.Name)
	assert.Equal(s.T(), "cron", got.SourceType)
	assert.True(s.T(), got.Enabled)
	assert.Equal(s.T(), 10, got.AutoDisableAfter)

	// Update enabled + auto_disable_after via pointer fields (nil = preserve).
	enabledFalse := false
	autoDisable5 := 5
	updated, err := s.store.UpdateTrigger(ctx, "user", "u1", id, &TriggerUpdate{
		Enabled:          &enabledFalse,
		AutoDisableAfter: &autoDisable5,
	})
	require.NoError(s.T(), err)
	assert.False(s.T(), updated.Enabled)
	assert.Equal(s.T(), 5, updated.AutoDisableAfter)

	// source_type is NOT mutable — verify it stayed 'cron'.
	assert.Equal(s.T(), "cron", updated.SourceType)

	// Partial update preserving auto_disable_after (nil pointer): must NOT
	// trigger the CHECK(>=1) violation that the int-0 path would have.
	descNew := "updated description"
	updated2, err := s.store.UpdateTrigger(ctx, "user", "u1", id, &TriggerUpdate{Description: &descNew})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "updated description", updated2.Description)
	assert.Equal(s.T(), 5, updated2.AutoDisableAfter, "auto_disable_after preserved (nil pointer)")
}

// --- Webhook ---------------------------------------------------------------

func (s *StoreIntegrationSuite) TestWebhookCreateAndGet() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now()

	// Create the trigger first (webhooks FK to it).
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "github-hook", Enabled: true,
		SourceType: "webhook", SourceConfig: json.RawMessage(`{}`),
		TargetType: "run_workflow", TargetConfig: json.RawMessage(`{}`),
		AutoDisableAfter: 10, CreatedAt: now, UpdatedAt: now,
	}))

	hookID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateWebhook(ctx, &WebhookRow{
		ID: hookID, TriggerID: triggerID,
		SecretCipher: []byte("encrypted-secret-blob"), KeyVersion: 1,
		AllowedIPs:      []string{"192.168.1.0/24", "10.0.0.0/8"},
		IdempotencyMode: "header", IdempotencyHeader: "X-GitHub-Delivery",
		CreatedAt: now,
	}))

	// Get by trigger ID.
	got, err := s.store.GetWebhookByTriggerID(ctx, triggerID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), hookID, got.ID)
	assert.Equal(s.T(), []byte("encrypted-secret-blob"), got.SecretCipher)
	assert.Equal(s.T(), []string{"192.168.1.0/24", "10.0.0.0/8"}, []string(got.AllowedIPs))

	// Get by webhook ID (the public path used by POST /hooks/:id).
	got2, err := s.store.GetWebhook(ctx, hookID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), triggerID, got2.TriggerID)

	// Not found.
	_, err = s.store.GetWebhook(ctx, uuid.New().String())
	assert.ErrorIs(s.T(), err, ErrNotFound)
}

// --- Webhook delivery dedup ------------------------------------------------

func (s *StoreIntegrationSuite) TestWebhookDeliveryDedup() {
	ctx := context.Background()
	now := time.Now()

	// webhooks FK requires a parent trigger; webhook_deliveries FK requires a parent webhook.
	triggerID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "wh-for-dedup", Enabled: true, SourceType: "webhook",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 10,
		CreatedAt: now, UpdatedAt: now,
	}))
	hookID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateWebhook(ctx, &WebhookRow{
		ID: hookID, TriggerID: triggerID,
		SecretCipher: []byte("x"), KeyVersion: 1,
		IdempotencyMode: "header", IdempotencyHeader: "X-Request-ID",
		CreatedAt: now,
	}))

	// First delivery succeeds.
	err := s.store.RecordWebhookDelivery(ctx, hookID, "delivery-1")
	require.NoError(s.T(), err)

	// Second delivery with same dedup key → ErrDedupConflict.
	err = s.store.RecordWebhookDelivery(ctx, hookID, "delivery-1")
	assert.ErrorIs(s.T(), err, ErrDedupConflict)

	// Different dedup key succeeds.
	err = s.store.RecordWebhookDelivery(ctx, hookID, "delivery-2")
	require.NoError(s.T(), err)
}

// --- Workflow run single-in-flight -----------------------------------------

func (s *StoreIntegrationSuite) TestWorkflowRunSingleInFlight() {
	ctx := context.Background()
	wfID := uuid.New().String()
	wsID := s.newWorkspaceID()
	now := time.Now()

	// Create the workflow first (runs FK to it).
	require.NoError(s.T(), s.store.CreateWorkflow(ctx, &WorkflowRow{
		ID: wfID, OwnerType: "user", OwnerID: "u1",
		Name: "wf", Slug: "wf", SpecYAML: "x", SpecJSON: json.RawMessage(`{}`),
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}))

	// First queued run succeeds.
	run1 := &WorkflowRunRow{
		ID: uuid.New().String(), WorkflowID: wfID, WorkspaceID: wsID,
		SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(s.T(), s.store.CreateWorkflowRun(ctx, run1))

	// Second queued run for same workflow → ErrConcurrentRun (partial unique index).
	run2 := &WorkflowRunRow{
		ID: uuid.New().String(), WorkflowID: wfID, WorkspaceID: wsID,
		SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
		CreatedAt: now, UpdatedAt: now,
	}
	err := s.store.CreateWorkflowRun(ctx, run2)
	assert.ErrorIs(s.T(), err, ErrConcurrentRun)

	// After run1 reaches terminal state, a new run succeeds.
	require.NoError(s.T(), s.store.UpdateWorkflowRunStatus(ctx, run1.ID, "succeeded", nil, nil, json.RawMessage(`{"result":"done"}`)))

	// HasInFlightRun should now be false.
	inFlight, err := s.store.HasInFlightRun(ctx, wfID)
	require.NoError(s.T(), err)
	assert.False(s.T(), inFlight)

	run3 := &WorkflowRunRow{
		ID: uuid.New().String(), WorkflowID: wfID, WorkspaceID: wsID,
		SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(s.T(), s.store.CreateWorkflowRun(ctx, run3))
}

// --- ClaimQueuedRuns (FOR UPDATE SKIP LOCKED) ------------------------------

func (s *StoreIntegrationSuite) TestClaimQueuedRuns() {
	ctx := context.Background()
	wsID := s.newWorkspaceID()
	now := time.Now()

	for i := 0; i < 3; i++ {
		wfID := uuid.New().String()
		require.NoError(s.T(), s.store.CreateWorkflow(ctx, &WorkflowRow{
			ID: wfID, OwnerType: "user", OwnerID: "u1",
			Name: "wf-" + wfID[:8], Slug: "wf-" + wfID[:8],
			SpecYAML: "x", SpecJSON: json.RawMessage(`{}`), Status: "active",
			CreatedAt: now, UpdatedAt: now,
		}))
		require.NoError(s.T(), s.store.CreateWorkflowRun(ctx, &WorkflowRunRow{
			ID: uuid.New().String(), WorkflowID: wfID, WorkspaceID: wsID,
			SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now,
		}))
	}

	// Claim 2 — should get the 2 oldest.
	claimed, err := s.store.ClaimQueuedRuns(ctx, 2)
	require.NoError(s.T(), err)
	assert.Len(s.T(), claimed, 2)
	for _, r := range claimed {
		assert.Equal(s.T(), "running", r.Status)
		assert.NotNil(s.T(), r.StartedAt)
	}

	// Claim 2 again — should get only the 1 remaining.
	claimed2, err := s.store.ClaimQueuedRuns(ctx, 2)
	require.NoError(s.T(), err)
	assert.Len(s.T(), claimed2, 1)

	// Third claim returns empty.
	claimed3, err := s.store.ClaimQueuedRuns(ctx, 2)
	require.NoError(s.T(), err)
	assert.Empty(s.T(), claimed3)
}

// --- Node runs -------------------------------------------------------------

func (s *StoreIntegrationSuite) TestNodeRunCreateAndUpdate() {
	ctx := context.Background()
	wfID := uuid.New().String()
	wsID := s.newWorkspaceID()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateWorkflow(ctx, &WorkflowRow{
		ID: wfID, OwnerType: "user", OwnerID: "u1",
		Name: "wf", Slug: "wf", SpecYAML: "x", SpecJSON: json.RawMessage(`{}`),
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}))
	runID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateWorkflowRun(ctx, &WorkflowRunRow{
		ID: runID, WorkflowID: wfID, WorkspaceID: wsID,
		SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
		CreatedAt: now, UpdatedAt: now,
	}))

	nodeRunID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateNodeRun(ctx, &WorkflowNodeRunRow{
		ID: nodeRunID, WorkflowRunID: runID, NodeID: "start",
		NodeType: "script", Status: "running", Attempt: 0,
		Input: json.RawMessage(`{"x":1}`), StartedAt: now,
	}))

	// Update to succeeded with output.
	err := s.store.UpdateNodeRunStatus(ctx, nodeRunID, "succeeded",
		json.RawMessage(`{"y":2}`), nil, nil, nil)
	require.NoError(s.T(), err)

	// List and verify.
	nodes, err := s.store.ListNodeRuns(ctx, runID)
	require.NoError(s.T(), err)
	require.Len(s.T(), nodes, 1)
	assert.Equal(s.T(), "succeeded", nodes[0].Status)
	assertJSONEqual(s.T(), `{"y":2}`, nodes[0].Output)
	assert.NotNil(s.T(), nodes[0].FinishedAt)
}

// --- Circuit breaker helpers -----------------------------------------------

func (s *StoreIntegrationSuite) TestTriggerCircuitBreaker() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "cron-trigger", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 3,
		CreatedAt: now, UpdatedAt: now,
	}))

	// Simulate 3 failures.
	for i := 0; i < 3; i++ {
		n, err := s.store.IncrementTriggerFailures(ctx, triggerID)
		require.NoError(s.T(), err)
		assert.Equal(s.T(), i+1, n)
	}

	// Verify consecutive_failures.
	got, err := s.store.GetTrigger(ctx, "user", "u1", triggerID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, got.ConsecutiveFailures)

	// Disable (circuit breaker trips).
	require.NoError(s.T(), s.store.DisableTrigger(ctx, triggerID))

	// Reset on success.
	require.NoError(s.T(), s.store.ResetTriggerFailures(ctx, triggerID))
	got, err = s.store.GetTrigger(ctx, "user", "u1", triggerID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 0, got.ConsecutiveFailures)
}

// --- Due cron triggers -----------------------------------------------------

func (s *StoreIntegrationSuite) TestListDueCronTriggers() {
	ctx := context.Background()
	now := time.Now()
	past := now.Add(-5 * time.Minute)
	future := now.Add(1 * time.Hour)

	// Due trigger (next_fire_at in the past).
	dueID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: dueID, OwnerType: "user", OwnerID: "u1",
		Name: "due", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 10,
		NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
	}))

	// Not-due trigger (next_fire_at in the future).
	notDueID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: notDueID, OwnerType: "user", OwnerID: "u1",
		Name: "not-due", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 10,
		NextFireAt: &future, CreatedAt: now, UpdatedAt: now,
	}))

	// Disabled trigger (even if due).
	disabledID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: disabledID, OwnerType: "user", OwnerID: "u1",
		Name: "disabled", Enabled: false, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 10,
		NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
	}))

	due, err := s.store.ListDueCronTriggers(ctx, now, 10)
	require.NoError(s.T(), err)
	require.Len(s.T(), due, 1, "only the enabled due trigger should be returned")
	assert.Equal(s.T(), dueID, due[0].ID)
}

// --- Atomic fire-row + run-create ------------------------------------------

func (s *StoreIntegrationSuite) TestCreateWorkflowRunWithFire() {
	ctx := context.Background()
	wfID := uuid.New().String()
	wsID := s.newWorkspaceID()
	triggerID := uuid.New().String()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateWorkflow(ctx, &WorkflowRow{
		ID: wfID, OwnerType: "user", OwnerID: "u1",
		Name: "wf", Slug: "wf", SpecYAML: "x", SpecJSON: json.RawMessage(`{}`),
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "wh", Enabled: true, SourceType: "webhook",
		SourceConfig: json.RawMessage(`{}`), TargetType: "run_workflow",
		TargetConfig: json.RawMessage(`{}`), AutoDisableAfter: 10,
		CreatedAt: now, UpdatedAt: now,
	}))

	fireID := uuid.New().String()
	runID := uuid.New().String()
	err := s.store.CreateWorkflowRunWithFire(ctx,
		&TriggerFireRow{
			ID: fireID, TriggerID: triggerID, SourceType: "webhook",
			InputEnvelope: json.RawMessage(`{"body":{"x":1}}`),
			ActionType:    "run_workflow", Status: "fired", FiredAt: now,
		},
		&WorkflowRunRow{
			ID: runID, WorkflowID: wfID, WorkspaceID: wsID,
			SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
			TriggerID: &triggerID, CreatedAt: now, UpdatedAt: now,
		},
	)
	require.NoError(s.T(), err)

	// Run should have trigger_fire_id set.
	got, err := s.store.GetWorkflowRun(ctx, runID)
	require.NoError(s.T(), err)
	assert.NotNil(s.T(), got.TriggerFireID)
	assert.Equal(s.T(), fireID, *got.TriggerFireID)

	// Second fire for the same workflow → ErrConcurrentRun (single-in-flight).
	fireID2 := uuid.New().String()
	runID2 := uuid.New().String()
	err = s.store.CreateWorkflowRunWithFire(ctx,
		&TriggerFireRow{
			ID: fireID2, TriggerID: triggerID, SourceType: "webhook",
			ActionType: "run_workflow", Status: "fired", FiredAt: now,
		},
		&WorkflowRunRow{
			ID: runID2, WorkflowID: wfID, WorkspaceID: wsID,
			SpecSnapshot: json.RawMessage(`{}`), Status: "queued",
			TriggerID: &triggerID, CreatedAt: now, UpdatedAt: now,
		},
	)
	assert.ErrorIs(s.T(), err, ErrConcurrentRun)

	// Verify NO orphan fire row was written (atomic rollback).
	fires, err := s.store.ListTriggerFires(ctx, triggerID, 10, 0)
	require.NoError(s.T(), err)
	assert.Len(s.T(), fires, 1, "rejected fire should NOT have written an orphan row")
	assert.Equal(s.T(), fireID, fires[0].ID)
}
