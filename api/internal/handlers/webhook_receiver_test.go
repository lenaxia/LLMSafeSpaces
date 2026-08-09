package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/assert"
)

// mockWebhookReceiverStore implements webhookReceiverStore.
type mockWebhookReceiverStore struct {
	webhooks  map[string]*wf.WebhookRow
	triggers  map[string]*wf.TriggerRow
	workflows map[string]*wf.WorkflowRow
	delivered map[string]bool
	fireCount int
	runErr    error
}

func newMockWebhookReceiverStore() *mockWebhookReceiverStore {
	return &mockWebhookReceiverStore{
		webhooks:  make(map[string]*wf.WebhookRow),
		triggers:  make(map[string]*wf.TriggerRow),
		workflows: make(map[string]*wf.WorkflowRow),
		delivered: make(map[string]bool),
	}
}

func (m *mockWebhookReceiverStore) GetWebhook(_ context.Context, id string) (*wf.WebhookRow, error) {
	r, ok := m.webhooks[id]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockWebhookReceiverStore) GetTrigger(_ context.Context, _, _, id string) (*wf.TriggerRow, error) {
	r, ok := m.triggers[id]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockWebhookReceiverStore) GetTriggerByID(_ context.Context, id string) (*wf.TriggerRow, error) {
	r, ok := m.triggers[id]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockWebhookReceiverStore) GetWorkflow(_ context.Context, _, _, id string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[id]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockWebhookReceiverStore) RecordWebhookDelivery(_ context.Context, webhookID, dedupKey string) error {
	key := webhookID + ":" + dedupKey
	if m.delivered[key] {
		return wf.ErrDedupConflict
	}
	m.delivered[key] = true
	return nil
}

func (m *mockWebhookReceiverStore) CreateWorkflowRunWithFire(_ context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error {
	if m.runErr != nil {
		return m.runErr
	}
	m.fireCount++
	return nil
}

func (m *mockWebhookReceiverStore) CreateTriggerFire(_ context.Context, row *wf.TriggerFireRow) error {
	m.fireCount++
	return nil
}

// mockDecryptor implements webhookDecryptor.
type mockDecryptor struct{ secret string }

func (m *mockDecryptor) Decrypt(_ context.Context, _ []byte) ([]byte, error) {
	return []byte(m.secret), nil
}

func setupWebhookRouter(t *testing.T, store webhookReceiverStore, decrypt webhookDecryptor) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWebhookReceiverHandler(store, decrypt, 1<<20)
	r.POST("/api/v1/hooks/:webhookId", h.HandleWebhook)
	return r
}

func makeHMAC(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookReceiver_ValidSignature(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "test-secret"

	hookID := "hook-1"
	triggerID := "trig-1"
	store.webhooks[hookID] = &wf.WebhookRow{
		ID: hookID, TriggerID: triggerID,
		SecretCipher: []byte("encrypted"), KeyVersion: 1,
		IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers[triggerID] = &wf.TriggerRow{
		ID: triggerID, OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
	}

	r := setupWebhookRouter(t, store, &mockDecryptor{secret: secret})

	body := `{"event":"push","ref":"main"}`
	sig := makeHMAC([]byte(body), []byte(secret))

	req := httptest.NewRequest("POST", "/api/v1/hooks/"+hookID, bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 202, w.Code)
	assert.Equal(t, 1, store.fireCount)
}

func TestWebhookReceiver_MissingSignature(t *testing.T) {
	store := newMockWebhookReceiverStore()
	store.webhooks["hook-1"] = &wf.WebhookRow{
		ID: "hook-1", TriggerID: "trig-1",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["trig-1"] = &wf.TriggerRow{
		ID: "trig-1", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}

	r := setupWebhookRouter(t, store, &mockDecryptor{secret: "s"})

	req := httptest.NewRequest("POST", "/api/v1/hooks/hook-1", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 401, w.Code)
}

func TestWebhookReceiver_InvalidSignature(t *testing.T) {
	store := newMockWebhookReceiverStore()
	store.webhooks["hook-1"] = &wf.WebhookRow{
		ID: "hook-1", TriggerID: "trig-1",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["trig-1"] = &wf.TriggerRow{
		ID: "trig-1", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}

	r := setupWebhookRouter(t, store, &mockDecryptor{secret: "real-secret"})

	req := httptest.NewRequest("POST", "/api/v1/hooks/hook-1", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidhex")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 401, w.Code)
}

func TestWebhookReceiver_Dedup(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	hookID := "hook-dedup"
	store.webhooks[hookID] = &wf.WebhookRow{
		ID: hookID, TriggerID: "t1",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyHeader,
		IdempotencyHeader: "X-Request-ID",
	}
	store.triggers["t1"] = &wf.TriggerRow{
		ID: "t1", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		OwnerType: "user", OwnerID: "u1",
	}

	r := setupWebhookRouter(t, store, &mockDecryptor{secret: secret})

	body := `{"event":"push"}`
	sig := makeHMAC([]byte(body), []byte(secret))

	// First delivery.
	req1 := httptest.NewRequest("POST", "/api/v1/hooks/"+hookID, bytes.NewBufferString(body))
	req1.Header.Set("X-Hub-Signature-256", sig)
	req1.Header.Set("X-Request-ID", "delivery-1")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	assert.Equal(t, 202, w1.Code)

	// Second delivery with same dedup key.
	req2 := httptest.NewRequest("POST", "/api/v1/hooks/"+hookID, bytes.NewBufferString(body))
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-Request-ID", "delivery-1")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, 200, w2.Code)
	assert.Contains(t, w2.Body.String(), "duplicate")
}

func TestWebhookReceiver_WebhookNotFound(t *testing.T) {
	store := newMockWebhookReceiverStore()
	r := setupWebhookRouter(t, store, &mockDecryptor{secret: "s"})

	req := httptest.NewRequest("POST", "/api/v1/hooks/nonexistent", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 404, w.Code)
}

func TestWebhookReceiver_ConcurrentRun(t *testing.T) {
	store := newMockWebhookReceiverStore()
	store.runErr = wf.ErrConcurrentRun
	secret := "s"
	store.webhooks["hook-1"] = &wf.WebhookRow{
		ID: "hook-1", TriggerID: "t1",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["t1"] = &wf.TriggerRow{
		ID: "t1", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		OwnerType: "user", OwnerID: "u1",
	}

	r := setupWebhookRouter(t, store, &mockDecryptor{secret: secret})

	body := `{}`
	sig := makeHMAC([]byte(body), []byte(secret))
	req := httptest.NewRequest("POST", "/api/v1/hooks/hook-1", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 409, w.Code)
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
}

func TestVerifyHMAC(t *testing.T) {
	body := []byte(`{"test":true}`)
	secret := []byte("my-secret")
	sig := makeHMAC(body, secret)

	assert.True(t, verifyHMAC(body, secret, sig))
	assert.False(t, verifyHMAC(body, []byte("wrong"), sig))
	assert.False(t, verifyHMAC(body, secret, "sha256=bad"))
	assert.False(t, verifyHMAC(body, secret, "no-prefix"))
}

func TestIPInAllowlist(t *testing.T) {
	assert.True(t, ipInAllowlist("192.168.1.5", []string{"192.168.1.0/24"}))
	assert.False(t, ipInAllowlist("10.0.0.1", []string{"192.168.1.0/24"}))
	assert.True(t, ipInAllowlist("10.0.0.1", []string{"10.0.0.0/8"}))
	assert.False(t, ipInAllowlist("invalid", []string{"10.0.0.0/8"}))
}

func TestCheckTimestampSkew(t *testing.T) {
	now := time.Now().Unix()
	assert.True(t, checkTimestampSkew(fmt.Sprintf("%d", now)))
	assert.False(t, checkTimestampSkew("0"))
	assert.False(t, checkTimestampSkew("not-a-number"))
}

func strPtrWF(s string) *string { return &s }

// --- Rate limiting tests ---

type mockRateChecker struct {
	allow bool
	calls int
}

func (m *mockRateChecker) Allow(_ string, _ float64, _ int) bool {
	m.calls++
	return m.allow
}

func setupWebhookRouterWithRateLimit(t *testing.T, store webhookReceiverStore, decrypt webhookDecryptor, rc RateChecker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWebhookReceiverHandler(store, decrypt, 1<<20)
	h.SetRateChecker(rc, 10, 20)
	r.POST("/api/v1/hooks/:webhookId", h.HandleWebhook)
	return r
}

func TestWebhookReceiver_RateLimited(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-rl"] = &wf.WebhookRow{
		ID: "hook-rl", TriggerID: "t-rl",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["t-rl"] = &wf.TriggerRow{
		ID: "t-rl", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}

	rc := &mockRateChecker{allow: false}
	r := setupWebhookRouterWithRateLimit(t, store, &mockDecryptor{secret: secret}, rc)

	body := `{}`
	sig := makeHMAC([]byte(body), []byte(secret))
	req := httptest.NewRequest("POST", "/api/v1/hooks/hook-rl", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 429, w.Code)
	assert.Equal(t, "30", w.Header().Get("Retry-After"))
	assert.Equal(t, 0, store.fireCount)
}

func TestWebhookReceiver_RateLimitAllows(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-ok"] = &wf.WebhookRow{
		ID: "hook-ok", TriggerID: "t-ok",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["t-ok"] = &wf.TriggerRow{
		ID: "t-ok", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-1"),
	}
	store.workflows["wf-1"] = &wf.WorkflowRow{
		ID: "wf-1", SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		OwnerType: "user", OwnerID: "u1",
	}

	rc := &mockRateChecker{allow: true}
	r := setupWebhookRouterWithRateLimit(t, store, &mockDecryptor{secret: secret}, rc)

	body := `{}`
	sig := makeHMAC([]byte(body), []byte(secret))
	req := httptest.NewRequest("POST", "/api/v1/hooks/hook-ok", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 202, w.Code)
	assert.Equal(t, 1, rc.calls)
}

// --- Hash idempotency tests ---

func TestComputeHashDedupKey_SameBodySameWindow(t *testing.T) {
	body := []byte(`{"event":"push"}`)
	ts := "1700000000"

	key1 := computeHashDedupKey(body, ts)
	key2 := computeHashDedupKey(body, ts)
	assert.Equal(t, key1, key2, "same body + same timestamp window = same key")
}

func TestComputeHashDedupKey_DifferentBodyDifferentKey(t *testing.T) {
	ts := "1700000000"
	key1 := computeHashDedupKey([]byte(`{"a":1}`), ts)
	key2 := computeHashDedupKey([]byte(`{"a":2}`), ts)
	assert.NotEqual(t, key1, key2, "different body = different key")
}

func TestComputeHashDedupKey_SameBodyDifferentWindowDifferentKey(t *testing.T) {
	body := []byte(`{"event":"push"}`)
	key1 := computeHashDedupKey(body, "1700000000")
	key2 := computeHashDedupKey(body, "1700000300")
	assert.NotEqual(t, key1, key2, "same body, different 5-min window = different key")
}

func TestComputeHashDedupKey_SameBodyAdjacentTimestampsSameWindow(t *testing.T) {
	body := []byte(`{"event":"push"}`)
	// 1700000001 and 1700000099 both floor to window 1699999800
	key1 := computeHashDedupKey(body, "1700000001")
	key2 := computeHashDedupKey(body, "1700000099")
	assert.Equal(t, key1, key2, "timestamps within same 5-min window = same key")
}
