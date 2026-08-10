// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Integration tests for the workflows store. Skips gracefully when
// TEST_DATABASE_URL is unreachable, so it's safe to run in normal CI
// (the test compiles — catching SQL drift — but skips without a live PG).
// Set TEST_DATABASE_URL to a real PG to run the full suite.

package workflows

import (
	"context"
	"encoding/json"
	"fmt"
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
	// OnMissingWorkspace was not set (zero value "") — COALESCE(NULLIF(...))
	// must apply the DB default 'abort'.
	assert.Equal(s.T(), "abort", got.OnMissingWorkspace, "empty OnMissingWorkspace must default to 'abort'")

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
		WorkflowID:       nil,
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
	// MemoryMode/CaptureMode/PreserveSession were not set (zero value "") —
	// COALESCE(NULLIF(...)) must apply the DB defaults.
	assert.Equal(s.T(), "none", got.MemoryMode, "empty MemoryMode must default to 'none'")
	assert.Equal(s.T(), "errors_only", got.CaptureMode, "empty CaptureMode must default to 'errors_only'")
	assert.Equal(s.T(), "never", got.PreserveSession, "empty PreserveSession must default to 'never'")
	assert.Equal(s.T(), 1, got.MemoryMaxRuns, "zero MemoryMaxRuns must default to 1")

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
		WorkflowID:       nil,
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
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
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
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 3,
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

// TestUpdateTrigger_ReEnableResetsFailures verifies that re-enabling a
// disabled trigger resets consecutive_failures to 0. Without this, a trigger
// disabled at 15 failures would immediately re-disable on the first failed
// fire after re-enable (15 >= auto_disable_after). The CASE clause in
// UpdateTrigger detects enabled=false→true and resets.
func (s *StoreIntegrationSuite) TestUpdateTrigger_ReEnableResetsFailures() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "disabled-trigger", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		CreatedAt: now, UpdatedAt: now,
	}))

	// Accumulate failures and disable.
	for i := 0; i < 15; i++ {
		_, err := s.store.IncrementTriggerFailures(ctx, triggerID)
		require.NoError(s.T(), err)
	}
	require.NoError(s.T(), s.store.DisableTrigger(ctx, triggerID))

	got, err := s.store.GetTrigger(ctx, "user", "u1", triggerID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 15, got.ConsecutiveFailures)
	assert.False(s.T(), got.Enabled)

	// Re-enable — must reset consecutive_failures to 0.
	enabledTrue := true
	updated, err := s.store.UpdateTrigger(ctx, "user", "u1", triggerID, &TriggerUpdate{
		Enabled: &enabledTrue,
	})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 0, updated.ConsecutiveFailures,
		"re-enabling must reset consecutive_failures")
	assert.True(s.T(), updated.Enabled)

	// Verify persisted.
	got2, err := s.store.GetTrigger(ctx, "user", "u1", triggerID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 0, got2.ConsecutiveFailures)
	assert.True(s.T(), got2.Enabled)
}

// TestUpdateTrigger_EnableStaysTrue_DoesNotReset verifies that updating an
// already-enabled trigger does NOT reset the failure counter. The CASE clause
// only fires on the enabled=false→true transition.
func (s *StoreIntegrationSuite) TestUpdateTrigger_EnableStaysTrue_DoesNotReset() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "active-trigger", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		CreatedAt: now, UpdatedAt: now,
	}))

	// Accumulate some failures (but don't disable).
	for i := 0; i < 5; i++ {
		_, err := s.store.IncrementTriggerFailures(ctx, triggerID)
		require.NoError(s.T(), err)
	}

	// Update an unrelated field while enabled stays true.
	newName := "renamed"
	updated, err := s.store.UpdateTrigger(ctx, "user", "u1", triggerID, &TriggerUpdate{
		Name: &newName,
	})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 5, updated.ConsecutiveFailures,
		"failures must NOT reset when enabled stays true")
}

// TestUpdateTrigger_DisableViaUpdate_PreservesFailures verifies that explicitly
// disabling via UpdateTrigger (Enabled=&false) does NOT reset the counter.
// The CASE clause only resets on the false->true transition, never on true->false.
func (s *StoreIntegrationSuite) TestUpdateTrigger_DisableViaUpdate_PreservesFailures() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now()

	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "disable-via-update", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		CreatedAt: now, UpdatedAt: now,
	}))

	for i := 0; i < 7; i++ {
		_, err := s.store.IncrementTriggerFailures(ctx, triggerID)
		require.NoError(s.T(), err)
	}

	disabledFalse := false
	updated, err := s.store.UpdateTrigger(ctx, "user", "u1", triggerID, &TriggerUpdate{
		Enabled: &disabledFalse,
	})
	require.NoError(s.T(), err)
	assert.False(s.T(), updated.Enabled)
	assert.Equal(s.T(), 7, updated.ConsecutiveFailures,
		"failures must NOT reset on explicit disable via UpdateTrigger")
}

// --- Due cron triggers -----------------------------------------------------

func (s *StoreIntegrationSuite) TestClaimDueCronTriggers() {
	ctx := context.Background()
	now := time.Now()
	past := now.Add(-5 * time.Minute)
	future := now.Add(1 * time.Hour)

	// Due trigger (next_fire_at in the past).
	dueID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: dueID, OwnerType: "user", OwnerID: "u1",
		Name: "due", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
	}))

	// Not-due trigger (next_fire_at in the future).
	notDueID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: notDueID, OwnerType: "user", OwnerID: "u1",
		Name: "not-due", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		NextFireAt: &future, CreatedAt: now, UpdatedAt: now,
	}))

	// Disabled trigger (even if due).
	disabledID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: disabledID, OwnerType: "user", OwnerID: "u1",
		Name: "disabled", Enabled: false, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
	}))

	claimedNextFire := now.Add(15 * time.Minute)
	due, err := s.store.ClaimDueCronTriggers(ctx, now, 10, func(t *TriggerRow) time.Time {
		return claimedNextFire
	})
	require.NoError(s.T(), err)
	require.Len(s.T(), due, 1, "only the enabled due trigger should be claimed")
	assert.Equal(s.T(), dueID, due[0].ID)

	// Claim must have advanced last_fired_at + next_fire_at atomically.
	got, err := s.store.GetTrigger(ctx, "user", "u1", dueID)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), got.LastFiredAt)
	require.NotNil(s.T(), got.NextFireAt)
	assert.WithinDuration(s.T(), now, *got.LastFiredAt, time.Second,
		"last_fired_at should be ~now")
	assert.WithinDuration(s.T(), claimedNextFire, *got.NextFireAt, time.Second,
		"next_fire_at should be advanced to the claimed value")

	// Second claim should return nothing — the trigger is no longer due.
	due2, err := s.store.ClaimDueCronTriggers(ctx, now, 10, func(t *TriggerRow) time.Time {
		return claimedNextFire
	})
	require.NoError(s.T(), err)
	assert.Empty(s.T(), due2, "trigger should not be re-claimed after advance")
}

// TestClaimDueCronTriggers_SkipLocked verifies that concurrent transactions
// do not claim the same trigger row. This is the regression test for the
// 15/10 circuit-breaker overshoot: with N API replicas, each scheduler tick
// SELECTed the same due trigger and fired concurrently, inflating the failure
// counter past auto_disable_after before any replica observed the disable.
func (s *StoreIntegrationSuite) TestClaimDueCronTriggers_SkipLocked() {
	ctx := context.Background()
	now := time.Now()
	past := now.Add(-5 * time.Minute)

	triggerID := uuid.New().String()
	require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Name: "concurrent", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
		NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
	}))

	// Open two transactions concurrently (simulating two API replicas).
	tx1, err := s.pool.Begin(ctx)
	require.NoError(s.T(), err)
	defer func() { _ = tx1.Rollback(ctx) }()

	// tx1 claims the trigger.
	row := tx1.QueryRow(ctx, `
		SELECT `+triggerSelectColumns+`
		FROM triggers
		WHERE source_type = 'cron' AND enabled = true AND next_fire_at <= $1
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, now)
	claimed, err := scanTriggerRow(row)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), triggerID, claimed.ID)

	// tx2 should get zero rows — the row is locked by tx1.
	tx2, err := s.pool.Begin(ctx)
	require.NoError(s.T(), err)
	defer func() { _ = tx2.Rollback(ctx) }()

	rows, err := tx2.Query(ctx, `
		SELECT id FROM triggers
		WHERE source_type = 'cron' AND enabled = true AND next_fire_at <= $1
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, now)
	require.NoError(s.T(), err)
	count := 0
	for rows.Next() {
		count++
	}
	rows.Close()
	assert.Equal(s.T(), 0, count, "concurrent tx must not claim the locked trigger")
}

// TestClaimDueCronTriggers_ConcurrentMethodCalls verifies that two concurrent
// calls to the actual ClaimDueCronTriggers method return disjoint results.
// Unlike the SkipLocked test (which uses raw SQL to validate the FOR UPDATE
// SKIP LOCKED invariant), this test guards the implementation directly: if
// someone removed FOR UPDATE SKIP LOCKED from ClaimDueCronTriggers, this test
// would fail because both goroutines would claim all triggers.
func (s *StoreIntegrationSuite) TestClaimDueCronTriggers_ConcurrentMethodCalls() {
	ctx := context.Background()
	now := time.Now()
	past := now.Add(-5 * time.Minute)

	// Create multiple due triggers so there's contention.
	triggerIDs := make([]string, 4)
	for i := range triggerIDs {
		triggerIDs[i] = uuid.New().String()
		require.NoError(s.T(), s.store.CreateTrigger(ctx, &TriggerRow{
			ID: triggerIDs[i], OwnerType: "user", OwnerID: "u1",
			Name: fmt.Sprintf("concurrent-method-%d", i), Enabled: true, SourceType: "cron",
			SourceConfig: json.RawMessage(`{}`), WorkflowID: nil, AutoDisableAfter: 10,
			NextFireAt: &past, CreatedAt: now, UpdatedAt: now,
		}))
	}

	type claimResult struct {
		ids []string
		err error
	}
	resultCh := make(chan claimResult, 2)

	// Two goroutines claiming concurrently (simulating two API replicas).
	for i := 0; i < 2; i++ {
		go func() {
			claimed, err := s.store.ClaimDueCronTriggers(ctx, now, 10, func(t *TriggerRow) time.Time {
				return now.Add(15 * time.Minute)
			})
			ids := make([]string, len(claimed))
			for j, t := range claimed {
				ids[j] = t.ID
			}
			resultCh <- claimResult{ids: ids, err: err}
		}()
	}

	results := make([]claimResult, 2)
	for i := range results {
		results[i] = <-resultCh
		require.NoError(s.T(), results[i].err)
	}

	// Every trigger must be claimed exactly once across both goroutines.
	allClaimed := append(results[0].ids, results[1].ids...)
	assert.Len(s.T(), allClaimed, 4, "all 4 triggers should be claimed across both goroutines")

	seen := make(map[string]int)
	for _, id := range allClaimed {
		seen[id]++
	}
	for id, count := range seen {
		assert.Equal(s.T(), 1, count, "trigger %s claimed %d times (must be exactly 1)", id, count)
	}
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
		SourceConfig: json.RawMessage(`{}`), WorkflowID: strPtr(wfID), AutoDisableAfter: 10,
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

func (s *StoreIntegrationSuite) TestGetLastRoutineResult() {
	ctx := context.Background()
	triggerID := uuid.New().String()

	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO triggers (id, owner_type, owner_id, name, enabled, source_type, source_config, memory_mode, capture_mode, preserve_session, auto_disable_after, created_at, updated_at)
		VALUES ($1, 'user', 'test-user', 'test-routine', true, 'cron', '{}'::jsonb, 'last_result', 'full', 'never', 10, $2, $3)
	`, triggerID, now, now)
	s.Require().NoError(err)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, action_type, status, fired_at, result)
		VALUES ($1, $2, 'cron', 'routine', 'delivered', $3, '{"summary":"previous run result"}'::jsonb)
	`, uuid.New().String(), triggerID, now.Add(-1*time.Hour))
	s.Require().NoError(err)

	result, err := s.store.GetLastRoutineResult(ctx, triggerID)
	s.Require().NoError(err)
	s.Require().NotNil(result)
	s.Contains(string(result), "previous run result")
}

func (s *StoreIntegrationSuite) TestGetLastRoutineResult_EmptyOnNoDelivered() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO triggers (id, owner_type, owner_id, name, enabled, source_type, source_config, memory_mode, capture_mode, preserve_session, auto_disable_after, created_at, updated_at)
		VALUES ($1, 'user', 'test-user', 'test-routine2', true, 'cron', '{}'::jsonb, 'last_result', 'full', 'never', 10, $2, $3)
	`, triggerID, now, now)
	s.Require().NoError(err)

	_, err = s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, action_type, status, fired_at)
		VALUES ($1, $2, 'cron', 'routine', 'fired', $3)
	`, uuid.New().String(), triggerID, now)
	s.Require().NoError(err)

	result, err := s.store.GetLastRoutineResult(ctx, triggerID)
	s.Require().NoError(err)
	s.Nil(result, "should return nil for non-delivered fires")
}

func (s *StoreIntegrationSuite) TestListPendingRoutineFires() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	wsID := s.newWorkspaceID()
	now := time.Now().UTC()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO triggers (id, owner_type, owner_id, name, enabled, source_type, source_config,
			workspace_id, prompt, memory_mode, capture_mode, preserve_session, auto_disable_after, created_at, updated_at)
		VALUES ($1, 'user', 'test-user', 'test-pending', true, 'cron', '{}'::jsonb,
			$4, 'test prompt', 'none', 'full', 'never', 10, $2, $3)
	`, triggerID, now, now, wsID)
	s.Require().NoError(err)

	fireID := uuid.New().String()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, action_type, status, fired_at)
		VALUES ($1, $2, 'cron', 'routine', 'fired', $3)
	`, fireID, triggerID, now)
	s.Require().NoError(err)

	fires, err := s.store.ListPendingRoutineFires(ctx, 10)
	s.Require().NoError(err)
	s.Require().Len(fires, 1)
	s.Equal(fireID, fires[0].ID)

	_, err = s.pool.Exec(ctx, `
		UPDATE trigger_fires SET status = 'delivered', result = '{"ok":true}'::jsonb WHERE id = $1
	`, fireID)
	s.Require().NoError(err)

	fires, err = s.store.ListPendingRoutineFires(ctx, 10)
	s.Require().NoError(err)
	s.Empty(fires, "delivered fire should not be pending")
}

// TestTriggerFiresUUIDColumn_RejectsNonUUIDIDs is the integration regression
// for the "Test1we" production bug: the cron scheduler previously generated
// fire IDs as fmt.Sprintf("fire-%s-%d", triggerID, unix), which Postgres
// rejected with SQLSTATE 22P02 because trigger_fires.id is `uuid NOT NULL`
// (migration 000016:327). Every cron tick silently dropped the fire row, the
// trigger looked fired (last_fired_at advanced) but no agent invocation ran.
//
// This test pins the schema-side invariant: the column rejects the old shape
// and accepts a real UUID. The unit tests in api/internal/workflows
// (TestScheduler_FireAndRunIDs_AreUUIDs, TestReconciler_NodeRunID_IsUUID)
// cover the engine side.
func (s *StoreIntegrationSuite) TestTriggerFiresUUIDColumn_RejectsNonUUIDIDs() {
	ctx := context.Background()
	triggerID := uuid.New().String()
	now := time.Now().UTC()

	_, err := s.pool.Exec(ctx, `
		INSERT INTO triggers (id, owner_type, owner_id, name, enabled, source_type, source_config,
			memory_mode, capture_mode, preserve_session, auto_disable_after, created_at, updated_at)
		VALUES ($1, 'user', 'test-user', 'test-uuid-regress', true, 'cron', '{}'::jsonb,
			'none', 'full', 'never', 10, $2, $3)
	`, triggerID, now, now)
	s.Require().NoError(err)

	// Old buggy shape — what the scheduler used to produce.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, action_type, status, fired_at)
		VALUES ($1, $2, 'cron', 'routine', 'fired', $3)
	`, "fire-"+triggerID+"-1786260973", triggerID, now)
	s.Require().Error(err, "non-UUID id must be rejected by trigger_fires.id uuid column")

	pgErr, ok := err.(interface{ SQLState() string })
	s.Require().True(ok, "error should be a PG error with SQLState")
	s.Equal("22P02", pgErr.SQLState(), "expected invalid_text_representation (22P02)")

	// New correct shape — what the scheduler produces after the fix.
	fireID := uuid.New().String()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO trigger_fires (id, trigger_id, source_type, action_type, status, fired_at)
		VALUES ($1, $2, 'cron', 'routine', 'fired', $3)
	`, fireID, triggerID, now)
	s.Require().NoError(err, "UUID id must be accepted")

	fires, err := s.store.ListTriggerFires(ctx, triggerID, 10, 0)
	s.Require().NoError(err)
	s.Len(fires, 1)
	s.Equal(fireID, fires[0].ID)
}
