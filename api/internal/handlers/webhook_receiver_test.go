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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// mockWebhookReceiverStore implements webhookReceiverStore.
type mockWebhookReceiverStore struct {
	webhooks      map[string]*wf.WebhookRow
	triggers      map[string]*wf.TriggerRow
	workflows     map[string]*wf.WorkflowRow
	delivered     map[string]bool
	fireCount     int
	runErr        error
	triggerFail   map[string]int
	disabled      map[string]bool
	recordedFires []*wf.TriggerFireRow
	recordedRuns  []*wf.WorkflowRunRow
}

func newMockWebhookReceiverStore() *mockWebhookReceiverStore {
	return &mockWebhookReceiverStore{
		webhooks:    make(map[string]*wf.WebhookRow),
		triggers:    make(map[string]*wf.TriggerRow),
		workflows:   make(map[string]*wf.WorkflowRow),
		delivered:   make(map[string]bool),
		triggerFail: make(map[string]int),
		disabled:    make(map[string]bool),
	}
}

func (m *mockWebhookReceiverStore) GetWebhookByTriggerID(_ context.Context, triggerID string) (*wf.WebhookRow, error) {
	for _, r := range m.webhooks {
		if r.TriggerID == triggerID {
			return r, nil
		}
	}
	return nil, wf.ErrNotFound
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
	m.recordedFires = append(m.recordedFires, fire)
	m.recordedRuns = append(m.recordedRuns, run)
	return nil
}

func (m *mockWebhookReceiverStore) CreateTriggerFire(_ context.Context, row *wf.TriggerFireRow) error {
	m.fireCount++
	m.recordedFires = append(m.recordedFires, row)
	return nil
}

func (m *mockWebhookReceiverStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
}

func (m *mockWebhookReceiverStore) DisableTrigger(_ context.Context, triggerID string) error {
	m.disabled[triggerID] = true
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

	req := httptest.NewRequest("POST", "/api/v1/hooks/trig-1", bytes.NewBufferString(body))
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

	req := httptest.NewRequest("POST", "/api/v1/hooks/trig-1", bytes.NewBufferString(`{}`))
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

	req := httptest.NewRequest("POST", "/api/v1/hooks/trig-1", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=invalidhex")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, 401, w.Code)
}

func TestWebhookReceiver_Dedup(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-dedup"] = &wf.WebhookRow{
		ID: "hook-dedup", TriggerID: "t1",
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
	req1 := httptest.NewRequest("POST", "/api/v1/hooks/t1", bytes.NewBufferString(body))
	req1.Header.Set("X-Hub-Signature-256", sig)
	req1.Header.Set("X-Request-ID", "delivery-1")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	assert.Equal(t, 202, w1.Code)

	// Second delivery with same dedup key.
	req2 := httptest.NewRequest("POST", "/api/v1/hooks/t1", bytes.NewBufferString(body))
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
	req := httptest.NewRequest("POST", "/api/v1/hooks/t1", bytes.NewBufferString(body))
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
	req := httptest.NewRequest("POST", "/api/v1/hooks/t-rl", bytes.NewBufferString(body))
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
	req := httptest.NewRequest("POST", "/api/v1/hooks/t-ok", bytes.NewBufferString(body))
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

// --- 0059: inputFrom mapping on the webhook fire path ------------------------

// setupBodyMode wires one webhook trigger (inputFrom:body) on a
// schema-bearing workflow and returns its router + store.
func setupBodyMode(t *testing.T, schema string) (*gin.Engine, *mockWebhookReceiverStore, string) {
	t.Helper()
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-body"] = &wf.WebhookRow{
		ID: "hook-body", TriggerID: "trig-body",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["trig-body"] = &wf.TriggerRow{
		ID: "trig-body", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-schema"), InputFrom: "body",
		AutoDisableAfter: 10,
	}
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		InputSchema: json.RawMessage(schema),
	}
	return setupWebhookRouter(t, store, &mockDecryptor{secret: secret}), store, secret
}

func postSigned(t *testing.T, r *gin.Engine, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/hooks/trig-body", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", makeHMAC([]byte(body), []byte(secret)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestWebhookReceiver_BodyModeRunInput: the posted payload becomes the
// run input at the TOP LEVEL — the #1419 fix.
func TestWebhookReceiver_BodyModeRunInput(t *testing.T) {
	r, store, secret := setupBodyMode(t, `{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`)

	w := postSigned(t, r, secret, `{"topic":"e2e","nested":{"a":1}}`)
	require.Equal(t, 202, w.Code)
	require.Len(t, store.recordedRuns, 1)
	assert.JSONEq(t, `{"topic":"e2e","nested":{"a":1}}`, string(store.recordedRuns[0].Input),
		"body mode: the posted document is the run input verbatim")
	// The audit row keeps the full raw envelope.
	assert.NotNil(t, store.recordedFires[0].InputEnvelope)
	assert.Equal(t, 0, store.triggerFail["trig-body"])
}

// TestWebhookReceiver_BodyModeValidationFailure: a violating body answers
// 202 (delivery succeeded — a non-2xx would make GitHub-style senders
// retry a permanently invalid payload), records a validation_error fire
// with typed violations only (no instance echo), creates NO run, and
// counts toward auto-disable.
func TestWebhookReceiver_BodyModeValidationFailure(t *testing.T) {
	r, store, secret := setupBodyMode(t, `{"type":"object","required":["topic"],"additionalProperties":false}`)

	const instanceValue = "SECRET-BODY-VALUE"
	w := postSigned(t, r, secret, `{"wrong":"`+instanceValue+`"}`)
	require.Equal(t, 202, w.Code)
	assert.Len(t, store.recordedRuns, 0, "no run may be created for a failing input")

	require.Len(t, store.recordedFires, 1)
	fire := store.recordedFires[0]
	assert.Equal(t, types.TriggerFireValidationError, fire.Status)
	payload := string(fire.ActionResult)
	assert.Contains(t, payload, `"code":"schema_mismatch"`)
	assert.Contains(t, payload, `"inputFrom":"body"`)
	assert.Contains(t, payload, `"/topic"`)
	assert.NotContains(t, payload, instanceValue, "actionResult must never echo instance values")
	assert.NotContains(t, payload, `"wrong"`, "additionalProperties names are instance-derived and must not appear")
	assert.Equal(t, 1, store.triggerFail["trig-body"])
}

// TestWebhookReceiver_BodyModeAutoDisables: the accounting honors
// auto_disable_after on validation failures.
func TestWebhookReceiver_BodyModeAutoDisables(t *testing.T) {
	r, store, secret := setupBodyMode(t, `{"type":"object","required":["topic"]}`)
	store.triggers["trig-body"].AutoDisableAfter = 1

	w := postSigned(t, r, secret, `{"wrong":true}`)
	require.Equal(t, 202, w.Code)
	assert.True(t, store.disabled["trig-body"], "auto_disable_after=1 must disable after one validation_error fire")
}

// TestWebhookReceiver_EnvelopeStaticMerge: envelope mode + static input
// merges the overlay onto the envelope (static wins) — schema-driven
// authoring works without reaching into input.body.*.
func TestWebhookReceiver_EnvelopeStaticMerge(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-merge"] = &wf.WebhookRow{
		ID: "hook-merge", TriggerID: "trig-merge",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["trig-merge"] = &wf.TriggerRow{
		ID: "trig-merge", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-schema"), InputFrom: "envelope",
		Input:            json.RawMessage(`{"topic":"nightly"}`),
		AutoDisableAfter: 10,
	}
	store.workflows["wf-schema"] = &wf.WorkflowRow{
		ID: "wf-schema", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		InputSchema: json.RawMessage(`{"type":"object","required":["topic"]}`),
	}
	r := setupWebhookRouter(t, store, &mockDecryptor{secret: secret})

	w := postSignedTo(t, r, "/api/v1/hooks/trig-merge", secret, `{"anything":"goes"}`)
	require.Equal(t, 202, w.Code)
	require.Len(t, store.recordedRuns, 1)
	var runInput map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(store.recordedRuns[0].Input, &runInput))
	assert.Equal(t, `"nightly"`, string(runInput["topic"]), "static overlay wins at the top level")
	assert.NotNil(t, runInput["body"], "the envelope stays reachable under its keys")
	assert.NotNil(t, runInput["headers"])
}

func postSignedTo(t *testing.T, r *gin.Engine, path, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", makeHMAC([]byte(body), []byte(secret)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestWebhookReceiver_NonJSONFallbackShape: a non-JSON body is wrapped in
// the epic's {raw, content_type} shape (documented behavior, not coerced)
// — visible in the envelope and in body-mode run input.
func TestWebhookReceiver_NonJSONFallbackShape(t *testing.T) {
	r, store, secret := setupBodyMode(t, `{"type":"object"}`)

	w := postSigned(t, r, secret, `topic=nightly&urgent=1`)
	require.Equal(t, 202, w.Code)
	require.Len(t, store.recordedRuns, 1)

	var runInput map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(store.recordedRuns[0].Input, &runInput))
	var raw string
	require.NoError(t, json.Unmarshal(runInput["raw"], &raw))
	assert.Equal(t, "topic=nightly&urgent=1", raw)
	ct, ok := runInput["content_type"]
	require.True(t, ok, "the fallback doc must carry content_type: %s", store.recordedRuns[0].Input)
	assert.NotEmpty(t, string(ct))
}

// TestWebhookReceiver_InvalidSchemaFailedFire pins the webhook-side
// non-compiling-schema branch (duplicated from the cron path): a workflow
// defect, not an input defect — failed fire with
// {"code":"invalid_input_schema"}, 202 to the sender, no run.
func TestWebhookReceiver_InvalidSchemaFailedFire(t *testing.T) {
	store := newMockWebhookReceiverStore()
	secret := "s"
	store.webhooks["hook-broken"] = &wf.WebhookRow{
		ID: "hook-broken", TriggerID: "trig-broken",
		SecretCipher: []byte("enc"), IdempotencyMode: types.WebhookIdempotencyDisabled,
	}
	store.triggers["trig-broken"] = &wf.TriggerRow{
		ID: "trig-broken", OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: "webhook",
		WorkflowID: strPtrWF("wf-broken"), InputFrom: "body",
		AutoDisableAfter: 10,
	}
	store.workflows["wf-broken"] = &wf.WorkflowRow{
		ID: "wf-broken", OwnerType: "user", OwnerID: "u1",
		SpecJSON: json.RawMessage(`{}`), TargetWorkspaceID: strPtrWF("ws-1"),
		InputSchema: json.RawMessage(`{"$ref":"#/definitions/missing"}`),
	}
	r := setupWebhookRouter(t, store, &mockDecryptor{secret: secret})

	w := postSignedTo(t, r, "/api/v1/hooks/trig-broken", secret, `{"topic":"x"}`)
	require.Equal(t, 202, w.Code, "delivery succeeded; the workflow's defect surfaces via trigger_fires")
	assert.Len(t, store.recordedRuns, 0)
	require.Len(t, store.recordedFires, 1)
	assert.Equal(t, types.TriggerFireFailed, store.recordedFires[0].Status)
	assert.JSONEq(t, `{"code":"invalid_input_schema"}`, string(store.recordedFires[0].ActionResult))
	assert.Equal(t, 1, store.triggerFail["trig-broken"])
}
