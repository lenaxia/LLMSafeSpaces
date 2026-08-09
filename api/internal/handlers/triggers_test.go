package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTriggerStore implements triggerStore for testing.
type mockTriggerStore struct {
	triggers  map[string]*wf.TriggerRow
	webhooks  map[string]*wf.WebhookRow
	createErr error
}

func newMockTriggerStore() *mockTriggerStore {
	return &mockTriggerStore{triggers: make(map[string]*wf.TriggerRow), webhooks: make(map[string]*wf.WebhookRow)}
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
	return []*wf.TriggerFireRow{}, nil
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
