package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// mockTriggerStore implements triggerStore for testing.
type mockTriggerStore struct {
	triggers  map[string]*wf.TriggerRow
	webhooks  map[string]*wf.WebhookRow
	workflows map[string]*wf.WorkflowRow
	fires     []*wf.TriggerFireRow
	createErr error
}

func newMockTriggerStore() *mockTriggerStore {
	return &mockTriggerStore{
		triggers:  make(map[string]*wf.TriggerRow),
		webhooks:  make(map[string]*wf.WebhookRow),
		workflows: make(map[string]*wf.WorkflowRow),
	}
}

func (m *mockTriggerStore) CreateTrigger(_ context.Context, row *wf.TriggerRow) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.triggers[row.ID] = row
	return nil
}

func (m *mockTriggerStore) ListTriggers(_ context.Context, ownerType, ownerID string) ([]*wf.TriggerRow, error) {
	var out []*wf.TriggerRow
	for _, r := range m.triggers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mockTriggerStore) GetTrigger(_ context.Context, ownerType, ownerID, triggerID string) (*wf.TriggerRow, error) {
	r, ok := m.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockTriggerStore) UpdateTrigger(_ context.Context, ownerType, ownerID, triggerID string, upd *wf.TriggerUpdate) (*wf.TriggerRow, error) {
	r, ok := m.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	if upd.Enabled != nil {
		r.Enabled = *upd.Enabled
	}
	if upd.AutoDisableAfter != nil {
		r.AutoDisableAfter = *upd.AutoDisableAfter
	}
	if upd.Description != nil {
		r.Description = *upd.Description
	}
	if upd.Prompt != nil {
		r.Prompt = *upd.Prompt
	}
	if upd.WorkspaceID != nil {
		wsID := *upd.WorkspaceID
		if wsID == "" {
			r.WorkspaceID = nil
		} else {
			r.WorkspaceID = &wsID
		}
	}
	if upd.WorkflowID != nil {
		wfID := *upd.WorkflowID
		if wfID == "" {
			r.WorkflowID = nil
		} else {
			r.WorkflowID = &wfID
		}
	}
	if upd.MemoryMode != nil {
		r.MemoryMode = *upd.MemoryMode
	}
	if upd.CaptureMode != nil {
		r.CaptureMode = *upd.CaptureMode
	}
	if upd.PreserveSession != nil {
		r.PreserveSession = *upd.PreserveSession
	}
	if upd.Name != nil {
		r.Name = *upd.Name
	}
	if upd.SourceConfig != nil {
		r.SourceConfig = upd.SourceConfig
	}
	if upd.InputFrom != nil {
		r.InputFrom = *upd.InputFrom
	}
	if upd.Input != nil {
		r.Input = upd.Input
	}
	if upd.NextFireAt != nil {
		r.NextFireAt = upd.NextFireAt
	}
	return r, nil
}

func (m *mockTriggerStore) DeleteTrigger(_ context.Context, ownerType, ownerID, triggerID string) error {
	r, ok := m.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return wf.ErrNotFound
	}
	delete(m.triggers, triggerID)
	delete(m.webhooks, r.ID)
	return nil
}

func (m *mockTriggerStore) CountTriggersByOwner(_ context.Context, ownerType, ownerID string) (int, error) {
	count := 0
	for _, r := range m.triggers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			count++
		}
	}
	return count, nil
}

func (m *mockTriggerStore) CreateWebhook(_ context.Context, row *wf.WebhookRow) error {
	m.webhooks[row.TriggerID] = row
	return nil
}

func (m *mockTriggerStore) GetWebhookByTriggerID(_ context.Context, triggerID string) (*wf.WebhookRow, error) {
	r, ok := m.webhooks[triggerID]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockTriggerStore) ListTriggerFires(_ context.Context, triggerID string, limit, offset int) ([]*wf.TriggerFireRow, error) {
	return m.fires, nil
}

func (m *mockTriggerStore) UpdateWebhookSecret(_ context.Context, triggerID string, secretCipher []byte, keyVersion int) error {
	hook, ok := m.webhooks[triggerID]
	if !ok {
		return wf.ErrNotFound
	}
	hook.SecretCipher = secretCipher
	hook.KeyVersion = keyVersion
	return nil
}

// GetWorkflow backs the 0059 input-mapping validation (V3/V4/V6):
// owner-scoped, schema-bearing rows live here.
func (m *mockTriggerStore) GetWorkflow(_ context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

// mockEncryptor implements triggerEncryptor.
type mockEncryptor struct{}

func (m *mockEncryptor) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return append([]byte("enc:"), plaintext...), nil
}

func setupTriggerRouter(t *testing.T, store triggerStore, quota workflowQuotaChecker, encrypt triggerEncryptor) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewUserTriggersHandler(store, quota, encrypt)
	group := r.Group("/api/v1/me/triggers")
	group.Use(func(c *gin.Context) { c.Set("userID", "test-user"); c.Next() })
	group.GET("", h.UserList)
	group.POST("", h.UserCreate)
	group.GET("/:id", h.UserGet)
	group.PUT("/:id", h.UserUpdate)
	group.DELETE("/:id", h.UserDelete)
	group.POST("/:id/rotate-secret", h.UserRotateWebhookSecret)
	group.GET("/:id/fires", h.UserListFires)
	return r
}

func doTriggerRequest(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestTriggerCreate_Cron(t *testing.T) {
	store := newMockTriggerStore()
	// The create path resolves workflowId at the handler (the 35597973572
	// contract ruling) — the happy-path target must exist.
	store.workflows["wf_123"] = &wf.WorkflowRow{ID: "wf_123", OwnerType: "user", OwnerID: "test-user"}
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "nightly-backup",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workflowId":   "wf_123",
	})
	require.Equal(t, 201, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "nightly-backup", resp["name"])
	assert.Equal(t, "cron", resp["sourceType"])
	assert.NotNil(t, resp["nextFireAt"])
}

func TestTriggerCreate_Webhook(t *testing.T) {
	store := newMockTriggerStore()
	store.workflows["wf_123"] = &wf.WorkflowRow{ID: "wf_123", OwnerType: "user", OwnerID: "test-user"}
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "github-hook",
		"sourceType":   "webhook",
		"sourceConfig": map[string]any{},
		"workflowId":   "wf_123",
	})
	require.Equal(t, 201, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	trigger := resp["trigger"].(map[string]any)
	triggerID := trigger["id"].(string)
	assert.Equal(t, "webhook", trigger["sourceType"])
	assert.NotEmpty(t, resp["webhookUrl"])

	hook, err := store.GetWebhookByTriggerID(context.Background(), triggerID)
	require.NoError(t, err)
	assert.NotEmpty(t, hook.SecretCipher)
	assert.Equal(t, "header", hook.IdempotencyMode)
}

func TestTriggerCreate_InvalidSourceType(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":       "test",
		"sourceType": "manual",
		"workflowId": "wf_123",
	})
	assert.Equal(t, 400, w.Code)
}

func TestTriggerCreate_CronMissingExpr(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "bad-cron",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "test",
	})
	assert.Equal(t, 400, w.Code)
}

func TestTriggerGet_Success(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	triggerID := created["id"].(string)

	w2 := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+triggerID, nil)
	require.Equal(t, 200, w2.Code)
}

func TestTriggerGet_NotFound(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/nonexistent", nil)
	assert.Equal(t, 404, w.Code)
}

func TestTriggerUpdate_EnableDisable(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	triggerID := created["id"].(string)

	disabled := false
	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"enabled": disabled,
	})
	require.Equal(t, 200, w2.Code)

	var updated map[string]any
	json.Unmarshal(w2.Body.Bytes(), &updated)
	assert.Equal(t, false, updated["enabled"])
}

func TestTriggerUpdate_InvalidAutoDisable(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	triggerID := created["id"].(string)

	zero := 0
	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"autoDisableAfter": zero,
	})
	assert.Equal(t, 400, w2.Code)
}

func TestTriggerDelete_Success(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	triggerID := created["id"].(string)

	w2 := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+triggerID, nil)
	assert.Equal(t, 200, w2.Code)

	w3 := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+triggerID, nil)
	assert.Equal(t, 404, w3.Code)
}

func TestTriggerDelete_NotFound(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/nonexistent", nil)
	assert.Equal(t, 404, w.Code)
}

func TestTriggerList(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	for _, name := range []string{"t1", "t2"} {
		doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
			"name": name, "sourceType": "cron",
			"sourceConfig": map[string]any{"expr": "0 * * * *"},
			"workspaceId":  "ws-1", "prompt": "test routine",
		})
	}

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers", nil)
	require.Equal(t, 200, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	triggers := resp["triggers"].([]any)
	assert.Len(t, triggers, 2)
}

// TestTriggerCreate_NonexistentWorkflow_Named400 pins run 35597973572's
// arbitration finding + the adjudicated contract ruling: a user-supplied
// nonexistent workflowId must answer a NAMED 400 at the handler — it
// previously fell through to the store insert and surfaced as an opaque
// 500 (FK shape, 4.5ms). The wording mirrors the in-family update-path
// precedent ("target workflow not found"); the FK remains the integrity
// anchor (ON DELETE SET NULL — the #1440 valid-then-deleted loud path).
func TestTriggerCreate_NonexistentWorkflow_Named400(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "ghost-target",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
		"workflowId":   "deadbeef-0000-4000-8000-000000000000",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "target workflow not found")
	// Nothing may be stored on the rejected path.
	assert.Empty(t, store.triggers)
}

// TestTriggerCreate_CronValidationErrorOutranksWorkflowCheck pins the
// validation ORDER: the cron expr is validated before the workflow
// existence check, so an invalid expr answers the cron error even with
// a ghost workflowId (R1a's contract).
func TestTriggerCreate_CronValidationErrorOutranksWorkflowCheck(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "bad-cron-ghost-wf",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "not-a-cron", "tz": "UTC"},
		"workflowId":   "deadbeef-0000-4000-8000-000000000000",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "invalid cron expr")
}

func TestTriggerCreate_StoreError(t *testing.T) {
	store := newMockTriggerStore()
	store.createErr = errors.New("DB down")
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	assert.Equal(t, 500, w.Code)
}

func TestGenerateWebhookSecret(t *testing.T) {
	s1 := generateWebhookSecret()
	s2 := generateWebhookSecret()
	assert.NotEqual(t, s1, s2, "secrets must be random")
	assert.True(t, len(s1) > 30, "secret too short: %s", s1)
	assert.Contains(t, s1, "whsec_")
}

func TestTriggerCreate_QuotaExceeded(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{"triggers.maxPerUser": 1}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	body := map[string]any{
		"name": "t1", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	}

	// First succeeds (count 0 < 1).
	w1 := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", body)
	require.Equal(t, 201, w1.Code)

	// Second fails (count 1 >= 1).
	w2 := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "t2", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	assert.Equal(t, 409, w2.Code)
}

func TestTriggerCreate_CronNextFireAt(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "cron-test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.NotNil(t, resp["nextFireAt"], "cron trigger must have next_fire_at set")
}

func TestTriggerUpdate_AutoDisableAfter(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "test", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *"},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	require.Equal(t, 201, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	triggerID := created["id"].(string)

	five := 5
	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"autoDisableAfter": five,
	})
	require.Equal(t, 200, w2.Code)

	var updated map[string]any
	json.Unmarshal(w2.Body.Bytes(), &updated)
	assert.Equal(t, float64(5), updated["autoDisableAfter"])
}

func TestTriggerCreate_NilEncryptorForWebhook(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, nil)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "wh-no-encrypt", "sourceType": "webhook",
		"sourceConfig": map[string]any{},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	assert.Equal(t, 500, w.Code)

	// Verify cleanup: trigger should NOT exist after webhook creation failure.
	triggers := store.triggers
	assert.Empty(t, triggers, "trigger should be cleaned up when webhook creation fails")
}

// failingEncryptor returns an error on Encrypt, simulating KEK failure.
type failingEncryptor struct{}

func (m *failingEncryptor) Encrypt(_ context.Context, _ []byte) ([]byte, error) {
	return nil, errors.New("KEK unavailable")
}

func TestTriggerCreate_WebhookEncryptFailure_Cleanup(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &failingEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "wh-encrypt-fail", "sourceType": "webhook",
		"sourceConfig": map[string]any{},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	assert.Equal(t, 500, w.Code)

	// Verify cleanup: trigger should NOT exist after encryption failure.
	assert.Empty(t, store.triggers, "trigger should be cleaned up when webhook encryption fails")
	assert.Empty(t, store.webhooks, "webhook should not exist after encryption failure")
}

// failingWebhookStore wraps mockTriggerStore and fails CreateWebhook.
type failingWebhookStore struct {
	*mockTriggerStore
}

func (m *failingWebhookStore) CreateWebhook(_ context.Context, _ *wf.WebhookRow) error {
	return errors.New("webhook table write failed")
}

func TestTriggerCreate_WebhookStoreFailure_Cleanup(t *testing.T) {
	base := newMockTriggerStore()
	store := &failingWebhookStore{mockTriggerStore: base}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "wh-store-fail", "sourceType": "webhook",
		"sourceConfig": map[string]any{},
		"workspaceId":  "ws-1", "prompt": "test routine",
	})
	assert.Equal(t, 500, w.Code)

	// Verify cleanup: trigger should NOT exist after webhook store failure.
	assert.Empty(t, base.triggers, "trigger should be cleaned up when CreateWebhook fails")
}

func TestTriggerRotateWebhookSecret_Success(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-wh"] = &wf.TriggerRow{
		ID: "trig-wh", OwnerType: "user", OwnerID: "test-user",
		SourceType: "webhook", Enabled: true,
	}
	store.webhooks["trig-wh"] = &wf.WebhookRow{
		ID: "hook-1", TriggerID: "trig-wh",
		SecretCipher: []byte("old-enc"), KeyVersion: 1,
	}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers/trig-wh/rotate-secret", nil)
	assert.Equal(t, 200, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["webhookSecret"])
	assert.Contains(t, resp["webhookSecret"].(string), "whsec_")

	updated := store.webhooks["trig-wh"]
	assert.NotEqual(t, []byte("old-enc"), updated.SecretCipher)
	assert.Contains(t, string(updated.SecretCipher), "enc:whsec_")
}

func TestTriggerRotateWebhookSecret_NotWebhook(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-cron"] = &wf.TriggerRow{
		ID: "trig-cron", OwnerType: "user", OwnerID: "test-user",
		SourceType: "cron", Enabled: true,
	}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers/trig-cron/rotate-secret", nil)
	assert.Equal(t, 400, w.Code)
}

func TestTriggerRotateWebhookSecret_NotFound(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers/nonexistent/rotate-secret", nil)
	assert.Equal(t, 404, w.Code)
}

// --- #1411: create-path cron validation + real first fire slot ---

func TestTriggerCreate_InvalidCronExpr(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "bad-cron", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "not-a-cron", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "invalid cron expr")
	assert.Empty(t, store.triggers, "invalid schedule must not be stored")
}

func TestTriggerCreate_InvalidTZ(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "bad-tz", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 * * * *", "tz": "Mars/Olympus_Mons"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "invalid tz")
}

func TestTriggerCreate_CronNextFireIsFirstOccurrence(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	before := time.Now().UTC().Add(time.Minute)
	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "cron-first-slot", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 201, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	raw, ok := resp["nextFireAt"].(string)
	require.True(t, ok, "nextFireAt present")
	next, err := time.Parse(time.RFC3339, raw)
	require.NoError(t, err)
	assert.True(t, next.After(before), "nextFireAt must be the first scheduled occurrence, never creation time (was %s)", raw)
	assert.Equal(t, 0, next.Minute())
	assert.Equal(t, 2, next.Hour())
}

// --- #1410: schedule changes recompute next_fire_at immediately ---

func TestTriggerUpdate_ScheduleChangeRecomputesNextFireAt(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "reschedule", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	triggerID := created["id"].(string)

	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"sourceConfig": map[string]any{"expr": "0 4 1 * *", "tz": "UTC"},
	})
	require.Equal(t, 200, w2.Code, "body: %s", w2.Body.String())

	var updated map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &updated))
	raw, ok := updated["nextFireAt"].(string)
	require.True(t, ok, "nextFireAt present after update")
	next, err := time.Parse(time.RFC3339, raw)
	require.NoError(t, err)
	assert.Equal(t, 4, next.Hour(), "nextFireAt must reflect the NEW schedule immediately")
	assert.Equal(t, 1, next.Day())
}

func TestTriggerUpdate_InvalidCronExprRejected(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "stay-valid", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	triggerID := created["id"].(string)

	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"sourceConfig": map[string]any{"expr": "every 5 minutes", "tz": "UTC"},
	})
	assert.Equal(t, 400, w2.Code)
	assert.Contains(t, w2.Body.String(), "invalid cron expr")
}

func TestTriggerUpdate_EnableStaleSlotRecomputes(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "sleeper", "sourceType": "cron", "enabled": false,
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	triggerID := created["id"].(string)

	// Simulate a long-disabled trigger: stored slot long in the past.
	stale := time.Now().UTC().Add(-48 * time.Hour)
	for _, row := range store.triggers {
		row.NextFireAt = &stale
	}

	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"enabled": true,
	})
	require.Equal(t, 200, w2.Code, "body: %s", w2.Body.String())

	var updated map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &updated))
	raw, ok := updated["nextFireAt"].(string)
	require.True(t, ok, "nextFireAt present after re-enable")
	next, err := time.Parse(time.RFC3339, raw)
	require.NoError(t, err)
	assert.True(t, next.After(time.Now().UTC()), "re-enabling a stale trigger resumes on the next future occurrence, not the stale slot")
	assert.Equal(t, 2, next.Hour())
}

func TestTriggerUpdate_NoopEnableKeepsFutureSlot(t *testing.T) {
	// enabled:true on an ALREADY-enabled trigger must never move the
	// slot — an imminent-but-unclaimed fire (the scheduler tick window)
	// would otherwise be silently pushed to the next occurrence
	// (review finding on #1410).
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "imminent", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "x",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	triggerID := created["id"].(string)

	// Put the stored slot just inside the claim window: due, but not yet
	// claimed by the scheduler.
	imminent := time.Now().UTC().Add(-2 * time.Second)
	for _, row := range store.triggers {
		row.NextFireAt = &imminent
	}

	w2 := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"enabled": true,
	})
	require.Equal(t, 200, w2.Code)

	for _, row := range store.triggers {
		require.NotNil(t, row.NextFireAt)
		assert.WithinDuration(t, imminent, *row.NextFireAt, time.Second,
			"no-op enable on an enabled trigger must not reschedule an imminent fire")
	}
}

// --- 0059: trigger input mapping validation (V1–V7) ---------------------------

func TestValidTriggerInputFrom(t *testing.T) {
	for _, ok := range []string{"envelope", "body", "mapped"} {
		assert.True(t, types.ValidTriggerInputFrom(ok), "%s must be valid", ok)
	}
	for _, bad := range []string{"", "Envelope", "json", "envelopes"} {
		assert.False(t, types.ValidTriggerInputFrom(bad), "%s must be invalid", bad)
	}
}

const schemaRequiringTopic = `{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`

func seedWorkflowWithSchema(t *testing.T, store *mockTriggerStore, id, schema string) {
	t.Helper()
	store.workflows[id] = &wf.WorkflowRow{
		ID: id, OwnerType: "user", OwnerID: "test-user",
		SpecJSON: json.RawMessage(`{}`), InputSchema: json.RawMessage(schema),
	}
}

// V1: input mapping is DAG-mode only — routine triggers keep the
// envelope-fed {{.input}} prompt contract.
func TestTriggerInputMapping_V1_RequiresWorkflowTarget(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "routine-body", "sourceType": "webhook",
		"sourceConfig": map[string]any{}, "workspaceId": "ws-1",
		"inputFrom": "body",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "input mapping requires a workflow target")

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "routine-static", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workspaceId": "ws-1",
		"input": map[string]any{"topic": "x"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "input mapping requires a workflow target")
	assert.Empty(t, store.triggers, "rejected wiring must not be stored")
}

// V2: inputFrom body requires a webhook source (cron envelopes carry no
// body key).
func TestTriggerInputMapping_V2_BodyRequiresWebhook(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "cron-body", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"inputFrom": "body",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "requires a webhook source")
}

// V3: an opted-in wiring must be able to reach its schema — a missing
// target workflow cannot be validated at all (distinct from the fire-time
// ghost, which stays loud for un-opted wiring).
func TestTriggerInputMapping_V3_MissingWorkflowCannotValidate(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "mapped-ghost", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-gone",
		"inputFrom": "mapped", "input": map[string]any{"topic": "x"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "target workflow not found")
}

// V4: mapped mode validates the static document against the workflow's
// inputSchema at wiring time.
func TestTriggerInputMapping_V4_MappedValidatesSchema(t *testing.T) {
	store := newMockTriggerStore()
	seedWorkflowWithSchema(t, store, "wf-schema", schemaRequiringTopic)
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "mapped-divergent", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-schema",
		"inputFrom": "mapped", "input": map[string]any{},
	})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "does not satisfy the workflow's inputSchema")

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "mapped-conforming", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-schema",
		"inputFrom": "mapped", "input": map[string]any{"topic": "nightly"},
	})
	require.Equal(t, 201, w.Code, w.Body.String())
}

// V5: the static overlay merges into an object base (envelope, or an
// object body) — it must itself be an object.
func TestTriggerInputMapping_V5_StaticMustBeObject(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "array-static", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"input": []any{1, 2},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "static input must be a JSON object in envelope/body modes")

	// mapped mode has no base to overlay — any JSON document is fine.
	seedWorkflowWithSchema(t, store, "wf-free", `{"type":"object"}`)
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "array-mapped", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-free",
		"inputFrom": "mapped", "input": []any{1, 2},
	})
	// The schema demands an object, so this 400s on V4 — not on V5.
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "does not satisfy the workflow's inputSchema")
}

func TestTriggerInputMapping_StaticSizeCap(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	huge := strings.Repeat("x", types.MaxTriggerStaticInputBytes)
	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "huge-static", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"input": map[string]any{"topic": huge},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "static-input cap")
}

func TestTriggerInputMapping_InvalidInputFrom(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "bad-mode", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"inputFrom": "template",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "invalid inputFrom")
}

// V6: new un-opted wiring against a schema requiring non-envelope
// properties is the #1425 mis-wiring — 400 naming the violation and the
// three remedies. Wiring whose schema only requires envelope keys passes;
// a nonexistent workflowId is rejected at the create contract (the
// 35597973572 ruling — the old "ghost stays creatable" arm only ever held
// in this mock: the real DB's workflow_id FK answered those creates with
// an opaque 500, so the R4 fixture was never creatable in production).
func TestTriggerInputMapping_V6_WiringGuard(t *testing.T) {
	store := newMockTriggerStore()
	seedWorkflowWithSchema(t, store, "wf-topic", schemaRequiringTopic)
	seedWorkflowWithSchema(t, store, "wf-envelope-only", `{"type":"object","required":["source","received_at"]}`)
	seedWorkflowWithSchema(t, store, "wf-webhook-keys", `{"type":"object","required":["body","headers"]}`)
	seedWorkflowWithSchema(t, store, "wf-schemaless", `null`)
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	// Hit: cron + required topic, no opt-in.
	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "miswired", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-topic",
	})
	require.Equal(t, 400, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "topic")
	assert.Contains(t, body, "set")
	assert.Contains(t, body, "body")
	assert.Contains(t, body, "relax")

	// Miss: schema requiring only envelope keys.
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "envelope-ok", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-envelope-only",
	})
	require.Equal(t, 201, w.Code, w.Body.String())

	// Miss: webhook + schema requiring body/headers (the webhook envelope
	// provides both).
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "webhook-ok", "sourceType": "webhook",
		"sourceConfig": map[string]any{}, "workflowId": "wf-webhook-keys",
	})
	require.Equal(t, 201, w.Code, w.Body.String())

	// Miss: schema-less workflow.
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "schemaless-ok", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-schemaless",
	})
	require.Equal(t, 201, w.Code, w.Body.String())

	// Ghost: missing workflow → the create contract rejects with the
	// named 400 (the 35597973572 ruling; previously creatable only in
	// this FK-less mock).
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "ghost-ok", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-gone",
	})
	require.Equal(t, 400, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "target workflow not found")

	// Opt-in suppresses the guard: static input satisfies the schema via
	// the overlay, so the wiring is creatable.
	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "opted-in", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-topic",
		"input": map[string]any{"topic": "nightly"},
	})
	require.Equal(t, 201, w.Code, w.Body.String())
}

// V7: only patches touching workflowId/input/inputFrom re-run the rules —
// against the post-patch merged view. A rename never trips V6; removing
// an opt-in re-exposes the wiring and re-trips the guard.
func TestTriggerInputMapping_V7_UpdateScope(t *testing.T) {
	store := newMockTriggerStore()
	seedWorkflowWithSchema(t, store, "wf-topic", schemaRequiringTopic)
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	// A legacy trigger: schema-bearing workflow wired pre-0059 (envelope
	// mode, no static input) — exactly what migration 000031 backfills.
	legacyID := "trig-legacy"
	wfID := "wf-topic"
	store.triggers[legacyID] = &wf.TriggerRow{
		ID: legacyID, OwnerType: "user", OwnerID: "test-user",
		Name: "legacy", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"0 2 * * *","tz":"UTC"}`),
		WorkflowID:   &wfID, InputFrom: "envelope",
		AutoDisableAfter: 10,
	}

	// Rename-only patch: re-runs NONE of V1–V6.
	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, map[string]any{
		"name": "legacy-renamed",
	})
	require.Equal(t, 200, w.Code, "rename-only patch must not re-trip V6: %s", w.Body.String())

	// Enable flip + schedule change: still none of V1–V6.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, map[string]any{
		"enabled": true, "sourceConfig": map[string]any{"expr": "0 3 * * *", "tz": "UTC"},
	})
	require.Equal(t, 200, w.Code, w.Body.String())

	// Retargeting the workflow key re-trips the guard against the new
	// target's schema.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, map[string]any{
		"workflowId": "wf-topic",
	})
	require.Equal(t, 400, w.Code, "a workflowId patch must re-run V6")
	assert.Contains(t, w.Body.String(), "relax")

	// Opting in via static input is allowed (V6 does not apply to
	// opted-in wiring).
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, map[string]any{
		"input": map[string]any{"topic": "nightly"},
	})
	require.Equal(t, 200, w.Code, w.Body.String())
	for _, row := range store.triggers {
		if row.ID == legacyID {
			require.NotNil(t, row.Input)
			assert.JSONEq(t, `{"topic":"nightly"}`, string(row.Input))
		}
	}

	// Removing the opt-in (input → JSON null) re-exposes the wiring and
	// re-trips the guard.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, json.RawMessage(`{"input":null}`))
	require.Equal(t, 400, w.Code, "clearing the opt-in must re-run V6")
	assert.Contains(t, w.Body.String(), "relax")

	// Clearing the workflow target while an opt-in remains trips V1.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+legacyID, map[string]any{
		"workflowId": "",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "input mapping requires a workflow target")
}

// V7 continued: a patch switching inputFrom to body on a stored cron
// trigger trips V2 against the stored (immutable) source type.
func TestTriggerInputMapping_V7_BodyOnCronUpdateRejected(t *testing.T) {
	store := newMockTriggerStore()
	store.workflows["wf-any"] = &wf.WorkflowRow{ID: "wf-any", OwnerType: "user", OwnerID: "test-user"}
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "plain-cron", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-any",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	triggerID := created["id"].(string)

	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+triggerID, map[string]any{
		"inputFrom": "body",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "requires a webhook source")
}

// Response contract: inputFrom is always reported (envelope default) and
// input echoes when set.
func TestTriggerInputMapping_ResponseFields(t *testing.T) {
	store := newMockTriggerStore()
	seedWorkflowWithSchema(t, store, "wf-1", `null`)
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "resp-default", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
	})
	require.Equal(t, 201, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "envelope", resp["inputFrom"])
	assert.NotContains(t, resp, "input")

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "resp-mapped", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"inputFrom": "body",
	})
	require.Equal(t, 400, w.Code) // body on cron — V2

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "resp-static", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *"}, "workflowId": "wf-1",
		"input": map[string]any{"topic": "x"},
	})
	require.Equal(t, 201, w.Code)
	resp = map[string]any{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "envelope", resp["inputFrom"])
	input, ok := resp["input"].(map[string]any)
	require.True(t, ok, "input must be echoed: %v", resp["input"])
	assert.Equal(t, "x", input["topic"])
}

// #1441: routine failure causes land in the result column
// (UpdateTriggerFireResult) — the fires response must expose it or
// routine failures are undiagnosable from outside.
func TestTriggerFires_ExposeRoutineResult(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-r"] = &wf.TriggerRow{ID: "trig-r", OwnerType: "user", OwnerID: "test-user", Name: "r", Enabled: true}
	store.fires = []*wf.TriggerFireRow{{
		ID: "fire-1", TriggerID: "trig-r", SourceType: "webhook",
		ActionType: "routine", Status: "failed",
		Result: json.RawMessage(`{"error":"agent call failed: deadline exceeded"}`),
	}}
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/trig-r/fires", nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "deadline exceeded", "the routine failure cause must be visible in the fires list")
	assert.Contains(t, w.Body.String(), `"result":`, "the result field is marshaled")
}

// The fires endpoint is owner-scoped like every sibling: another
// user's trigger UUID (which travels in shared webhook URLs) must 404.
func TestTriggerFires_OwnershipGuard(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-own"] = &wf.TriggerRow{ID: "trig-own", OwnerType: "user", OwnerID: "someone-else", Name: "x", Enabled: true}
	store.fires = []*wf.TriggerFireRow{{
		ID: "fire-x", TriggerID: "trig-own", SourceType: "webhook",
		ActionType: "routine", Status: "failed",
		Result: json.RawMessage(`{"error":"secret cause"}`),
	}}
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/trig-own/fires", nil)
	require.Equal(t, 404, w.Code, "cross-tenant fires read must 404")
	assert.NotContains(t, w.Body.String(), "secret cause")
}

// Edge: NULL result (pre-execution or errors_only fires) omits the
// field entirely — no "result":null noise, no empty object.
func TestTriggerFires_NullResultOmitted(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-n"] = &wf.TriggerRow{ID: "trig-n", OwnerType: "user", OwnerID: "test-user", Name: "n", Enabled: true}
	store.fires = []*wf.TriggerFireRow{{
		ID: "fire-n", TriggerID: "trig-n", SourceType: "webhook",
		ActionType: "routine", Status: "fired",
	}}
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/trig-n/fires", nil)
	require.Equal(t, 200, w.Code)
	assert.NotContains(t, w.Body.String(), `"result"`, "NULL result omits the key (omitempty)")
}

// Edge: captured SUCCESS output surfaces the same way as a failure
// cause (captureMode full stores the agent output in the same column).
func TestTriggerFires_CapturedSuccessVisible(t *testing.T) {
	store := newMockTriggerStore()
	store.triggers["trig-c"] = &wf.TriggerRow{ID: "trig-c", OwnerType: "user", OwnerID: "test-user", Name: "c", Enabled: true}
	store.fires = []*wf.TriggerFireRow{{
		ID: "fire-c", TriggerID: "trig-c", SourceType: "webhook",
		ActionType: "routine", Status: "delivered",
		Result: json.RawMessage(`{"response":"NIGHTLY-OK","session_id":""}`),
	}}
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/trig-c/fires", nil)
	require.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "NIGHTLY-OK", "captured success output is readable")
}
func TestTriggerUpdate_TargetPresenceGuard(t *testing.T) {
	newRouterWithDAGTrigger := func(t *testing.T) (*gin.Engine, *mockTriggerStore, string) {
		store := newMockTriggerStore()
		store.workflows["wf-1"] = &wf.WorkflowRow{ID: "wf-1", OwnerType: "user", OwnerID: "test-user"}
		r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
		w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
			"name": "dag", "sourceType": "cron",
			"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
			"workflowId":   "wf-1", "prompt": "x",
		})
		require.Equal(t, 201, w.Code, "body: %s", w.Body.String())
		var created map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
		return r, store, created["id"].(string)
	}

	t.Run("clearing workflowId alone is rejected", func(t *testing.T) {
		r, _, id := newRouterWithDAGTrigger(t)
		w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workflowId": ""})
		require.Equal(t, 400, w.Code, "body: %s", w.Body.String())
	})

	t.Run("clearing workflowId while naming a workspace is allowed", func(t *testing.T) {
		r, store, id := newRouterWithDAGTrigger(t)
		w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workflowId": "", "workspaceId": "ws-1"})
		require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
		row := store.triggers[id]
		require.NotNil(t, row.WorkspaceID)
		assert.Nil(t, row.WorkflowID)
	})

	t.Run("clearing workspaceId on a routine trigger is rejected", func(t *testing.T) {
		store := newMockTriggerStore()
		r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
		w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
			"name": "routine", "sourceType": "cron",
			"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
			"workspaceId":  "ws-1", "prompt": "x",
		})
		require.Equal(t, 201, w.Code)
		var created map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
		id := created["id"].(string)

		w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workspaceId": ""})
		require.Equal(t, 400, w.Code, "body: %s", w.Body.String())
		assert.Contains(t, w.Body.String(), "without an execution target")
	})

	t.Run("swapping workspace for workflow keeps a target", func(t *testing.T) {
		store := newMockTriggerStore()
		r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
		w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
			"name": "swap", "sourceType": "cron",
			"sourceConfig": map[string]any{"expr": "0 3 1 * *", "tz": "UTC"},
			"workspaceId":  "ws-1", "prompt": "x",
		})
		require.Equal(t, 201, w.Code)
		var created map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
		id := created["id"].(string)

		w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workspaceId": "", "workflowId": "wf-9"})
		require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
		row := store.triggers[id]
		require.NotNil(t, row.WorkflowID)
		assert.Nil(t, row.WorkspaceID)
	})
}

// #1442 round 2: pre-existing targetless rows (the #1440 incident
// population — FK SET NULL zombies) must stay EDITABLE for non-target
// patches: disable, rename. The guard evaluates only patches that touch
// a target field; an unconditional merged-view check would 400 every
// disable/rename of the incident population and strand them enabled.
func TestTriggerUpdate_TargetlessRowNonTargetPatchAccepted(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	// Seed a targetless enabled row directly (the zombie shape).
	id := "trig-zombie"
	store.triggers[id] = &wf.TriggerRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "test-user",
		Name: "zombie", Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}

	disabled := false
	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": disabled})
	require.Equal(t, 200, w.Code, "disarm-first must work on a targetless row: %s", w.Body.String())
	assert.False(t, store.triggers[id].Enabled)

	rename := "zombie-renamed"
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"name": rename})
	require.Equal(t, 200, w.Code, "rename must work on a targetless row: %s", w.Body.String())
	assert.Equal(t, rename, store.triggers[id].Name)

	// But a target-touching patch on a targetless row still 400s — the
	// API route to KEEPING it targetless stays closed.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workflowId": ""})
	assert.Equal(t, 400, w.Code, "target-touching patch on a targetless row must still reject")
	assert.Contains(t, w.Body.String(), "without an execution target")
}

// #1442 round 3: the REPAIR path — naming a new target on an
// already-targetless row (the remediation for the #1440 incident
// population) must be accepted. The guard computes workflowSet from the
// request alone precisely to permit this; a "currently-targetless ⇒
// reject any target patch" simplification would strand every zombie
// unrepairable.
func TestTriggerUpdate_TargetlessRowRepairAccepted(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})

	id := "trig-zombie-repair"
	store.triggers[id] = &wf.TriggerRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "test-user",
		Name: "zombie2", Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"workflowId": "wf-repair"})
	require.Equal(t, 200, w.Code, "repair patch must be accepted: %s", w.Body.String())
	row := store.triggers[id]
	require.NotNil(t, row.WorkflowID)
	assert.Equal(t, "wf-repair", *row.WorkflowID)
}

// --- #1449: org-scope trigger routes must resolve the TRIGGER id ---

// The production route shape /api/v1/orgs/:id/triggers/:triggerId shadowed
// the delegated helpers' c.Param("id") read with the ORG id — every org
// trigger GET/PUT/DELETE/fires/rotate 404'd for real triggers. The helpers
// now take the resource id as a parameter; the wrappers bind the segment
// their route actually carries.
func TestOrgTriggerRoutes_ResolveTriggerID(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupTriggerRouter(t, store, quota, &mockEncryptor{})
	h := NewOrgTriggersHandler(store, quota, &mockEncryptor{})
	org := r.Group("/api/v1/orgs/:id/triggers")
	org.GET("/:triggerId", h.OrgGet)
	org.PUT("/:triggerId", h.OrgUpdate)
	org.DELETE("/:triggerId", h.OrgDelete)
	org.GET("/:triggerId/fires", h.OrgListFires)

	store.triggers["trig-org"] = &wf.TriggerRow{
		ID: "trig-org", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "org-trigger", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}

	// GET: the trigger — not a 404 from looking up the ORG id as the trigger.
	w := doTriggerRequest(t, r, "GET", "/api/v1/orgs/org-7/triggers/trig-org", nil)
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "org-trigger")

	// PUT: renames the trigger (would 404 under the shadowing).
	newName := "org-trigger-2"
	w = doTriggerRequest(t, r, "PUT", "/api/v1/orgs/org-7/triggers/trig-org", map[string]any{"name": newName})
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, newName, store.triggers["trig-org"].Name)

	// Fires: lists (empty is fine — the ROUTE must not 404).
	w = doTriggerRequest(t, r, "GET", "/api/v1/orgs/org-7/triggers/trig-org/fires", nil)
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())

	// DELETE: removes the trigger.
	w = doTriggerRequest(t, r, "DELETE", "/api/v1/orgs/org-7/triggers/trig-org", nil)
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, store.triggers, "trig-org")

	// Cross-org scoping still fails closed: another org's id 404s.
	store.triggers["trig-org2"] = &wf.TriggerRow{
		ID: "trig-org2", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "scoped", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}
	w = doTriggerRequest(t, r, "GET", "/api/v1/orgs/org-OTHER/triggers/trig-org2", nil)
	assert.Equal(t, 404, w.Code, "owner-scoped lookup must still fail closed")
}

// #1449 r1: OrgRotateWebhookSecret — fixed but previously untested.
func TestOrgRotateWebhookSecret_RouteWorks(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)
	h := NewOrgTriggersHandler(store, quota, encrypt)
	org := r.Group("/api/v1/orgs/:id/triggers")
	org.POST("/:triggerId/rotate-secret", h.OrgRotateWebhookSecret)

	// Non-webhook org trigger: 400 (route resolves the TRIGGER — a 404
	// would be the :id shadowing).
	store.triggers["trig-org-cron"] = &wf.TriggerRow{
		ID: "trig-org-cron", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "org-cron", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}
	w := doTriggerRequest(t, r, "POST", "/api/v1/orgs/org-7/triggers/trig-org-cron/rotate-secret", nil)
	require.Equal(t, 400, w.Code, "body: %s", w.Body.String())

	// Webhook org trigger: 200 + one-time secret, store updated.
	store.triggers["trig-org-hook"] = &wf.TriggerRow{
		ID: "trig-org-hook", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "org-hook", Enabled: true, SourceType: "webhook",
		SourceConfig: json.RawMessage(`{}`),
	}
	require.NoError(t, store.CreateWebhook(context.Background(), &wf.WebhookRow{ID: "wh-org", TriggerID: "trig-org-hook", SecretCipher: []byte("old"), KeyVersion: 1}))
	w = doTriggerRequest(t, r, "POST", "/api/v1/orgs/org-7/triggers/trig-org-hook/rotate-secret", nil)
	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	var resp struct {
		WebhookSecret string `json:"webhookSecret"`
		WebhookURL    string `json:"webhookUrl"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp.WebhookSecret, "whsec_")
	assert.Equal(t, "/api/v1/hooks/trig-org-hook", resp.WebhookURL)
}

// seedRoutineTriggerRow inserts a routine trigger row directly (bypassing
// the create handler) so the update guard can be tested against every
// stored memoryMode/captureMode combination, including states create
// refuses to produce.
func seedRoutineTriggerRow(store *mockTriggerStore, id, memoryMode, captureMode string) {
	wsID := "ws-1"
	store.triggers[id] = &wf.TriggerRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "test-user",
		Name: "seeded", Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *","tz":"UTC"}`),
		WorkspaceID:  &wsID, Prompt: "p",
		MemoryMode: memoryMode, MemoryMaxRuns: 1, CaptureMode: captureMode,
		PreserveSession: types.PreserveNever, AutoDisableAfter: 10,
	}
}

// #1467: the update path must enforce the create-path cross-constraint
// memoryMode 'last_result' ⇒ captureMode 'full' (triggers.go create
// arm) on the POST-PATCH MERGED view — a patch may flip one side while
// leaving the other stored. Guard matrix: violating flips → 400 with
// the create-path error; valid flips and unaffected combos → 200.
func TestTriggerUpdate_MemoryCaptureCrossConstraint(t *testing.T) {
	const constraintErr = "memoryMode 'last_result' requires captureMode 'full'"
	tests := []struct {
		name        string
		seedMemory  string
		seedCapture string
		patch       map[string]any
		wantCode    int
		wantErr     string
	}{
		{"flip to last_result without full", types.MemoryNone, types.CaptureErrorsOnly,
			map[string]any{"memoryMode": types.MemoryLastResult}, 400, constraintErr},
		{"flip to last_result with full", types.MemoryNone, types.CaptureErrorsOnly,
			map[string]any{"memoryMode": types.MemoryLastResult, "captureMode": types.CaptureFull}, 200, ""},
		{"narrow capture under last_result", types.MemoryLastResult, types.CaptureFull,
			map[string]any{"captureMode": types.CaptureErrorsOnly}, 400, constraintErr},
		{"re-set full under last_result", types.MemoryLastResult, types.CaptureFull,
			map[string]any{"captureMode": types.CaptureFull}, 200, ""},
		{"loosen memory to none", types.MemoryLastResult, types.CaptureFull,
			map[string]any{"memoryMode": types.MemoryNone}, 200, ""},
		{"capture full on plain trigger", types.MemoryNone, types.CaptureErrorsOnly,
			map[string]any{"captureMode": types.CaptureFull}, 200, ""},
		{"both provided violating", types.MemoryNone, types.CaptureFull,
			map[string]any{"memoryMode": types.MemoryLastResult, "captureMode": types.CaptureErrorsOnly}, 400, constraintErr},
		{"both provided none and errors_only", types.MemoryLastResult, types.CaptureFull,
			map[string]any{"memoryMode": types.MemoryNone, "captureMode": types.CaptureErrorsOnly}, 200, ""},
		{"unrelated patch on valid row", types.MemoryNone, types.CaptureErrorsOnly,
			map[string]any{"prompt": "new prompt"}, 200, ""},
		// A legacy-invalid row (the exact state the missing guard let
		// through) stays editable: untouched state is not re-scanned, so
		// a repair patch can reach the store.
		{"unrelated patch on legacy-invalid row", types.MemoryLastResult, types.CaptureErrorsOnly,
			map[string]any{"prompt": "still editable"}, 200, ""},
		{"flip on legacy empty-string row without full", "", "",
			map[string]any{"memoryMode": types.MemoryLastResult}, 400, constraintErr},
		{"flip on legacy empty-string row with full", "", "",
			map[string]any{"memoryMode": types.MemoryLastResult, "captureMode": types.CaptureFull}, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockTriggerStore()
			seedRoutineTriggerRow(store, "trig-seed", tt.seedMemory, tt.seedCapture)
			r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})

			w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/trig-seed", tt.patch)
			require.Equal(t, tt.wantCode, w.Code, "body: %s", w.Body.String())

			if tt.wantErr != "" {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
				assert.Equal(t, tt.wantErr, resp["error"])
			}
			if tt.wantCode == 200 {
				row, err := store.GetTrigger(context.Background(), types.WorkflowOwnerUser, "test-user", "trig-seed")
				require.NoError(t, err)
				if m, ok := tt.patch["memoryMode"].(string); ok {
					assert.Equal(t, m, row.MemoryMode, "accepted patch must persist memoryMode")
				}
				if cm, ok := tt.patch["captureMode"].(string); ok {
					assert.Equal(t, cm, row.CaptureMode, "accepted patch must persist captureMode")
				}
			}
		})
	}
}

// The merged view needs the stored row, so existence precedes the
// cross-constraint: a violating patch on a missing trigger is a 404.
func TestTriggerUpdate_MemoryCaptureCrossConstraint_NotFound(t *testing.T) {
	store := newMockTriggerStore()
	r := setupTriggerRouter(t, store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/nope",
		map[string]any{"memoryMode": types.MemoryLastResult})
	assert.Equal(t, 404, w.Code, "body: %s", w.Body.String())
}
