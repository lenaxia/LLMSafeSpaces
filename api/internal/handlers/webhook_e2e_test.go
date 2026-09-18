// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// webhookE2EStore is ONE store shared by the real TriggersHandler and the
// real WebhookReceiverHandler — the exact seam the URL/row-ID bug lived
// in: create advertises a URL, the receiver must resolve it. Neither
// handler sees a fake of the other.
type webhookE2EStore struct {
	triggers     map[string]*wf.TriggerRow
	webhooksByID map[string]*wf.WebhookRow
	workflows    map[string]*wf.WorkflowRow
	delivered    map[string]bool
	fires        []*wf.TriggerFireRow
	runs         []*wf.WorkflowRunRow
	triggerFail  map[string]int
	disabled     map[string]bool
}

func newWebhookE2EStore() *webhookE2EStore {
	return &webhookE2EStore{
		triggers:     make(map[string]*wf.TriggerRow),
		webhooksByID: make(map[string]*wf.WebhookRow),
		workflows:    make(map[string]*wf.WorkflowRow),
		delivered:    make(map[string]bool),
		triggerFail:  make(map[string]int),
		disabled:     make(map[string]bool),
	}
}

// symmetricCipher makes create's encrypt and the receiver's decrypt
// round-trip in-test (production uses the KEK).
type symmetricCipher struct{}

func (symmetricCipher) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}
func (symmetricCipher) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

// --- triggerStore (subset used by create + rotate) --------------------------

func (m *webhookE2EStore) CreateTrigger(_ context.Context, row *wf.TriggerRow) error {
	m.triggers[row.ID] = row
	return nil
}
func (m *webhookE2EStore) ListTriggers(_ context.Context, ownerType, ownerID string) ([]*wf.TriggerRow, error) {
	var out []*wf.TriggerRow
	for _, r := range m.triggers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *webhookE2EStore) GetTrigger(_ context.Context, ownerType, ownerID, triggerID string) (*wf.TriggerRow, error) {
	r, ok := m.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}
func (m *webhookE2EStore) UpdateTrigger(_ context.Context, ownerType, ownerID, triggerID string, upd *wf.TriggerUpdate) (*wf.TriggerRow, error) {
	r, ok := m.triggers[triggerID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}
func (m *webhookE2EStore) DeleteTrigger(_ context.Context, ownerType, ownerID, triggerID string) error {
	delete(m.triggers, triggerID)
	return nil
}
func (m *webhookE2EStore) CountTriggersByOwner(_ context.Context, ownerType, ownerID string) (int, error) {
	n := 0
	for _, r := range m.triggers {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			n++
		}
	}
	return n, nil
}
func (m *webhookE2EStore) CreateWebhook(_ context.Context, row *wf.WebhookRow) error {
	m.webhooksByID[row.ID] = row
	return nil
}
func (m *webhookE2EStore) GetWebhookByTriggerID(_ context.Context, triggerID string) (*wf.WebhookRow, error) {
	for _, r := range m.webhooksByID {
		if r.TriggerID == triggerID {
			return r, nil
		}
	}
	return nil, wf.ErrNotFound
}
func (m *webhookE2EStore) UpdateWebhookSecret(_ context.Context, triggerID string, secretCipher []byte, keyVersion int) error {
	for _, r := range m.webhooksByID {
		if r.TriggerID == triggerID {
			r.SecretCipher = secretCipher
			r.KeyVersion = keyVersion
			return nil
		}
	}
	return wf.ErrNotFound
}
func (m *webhookE2EStore) ListTriggerFires(_ context.Context, triggerID string, limit, offset int) ([]*wf.TriggerFireRow, error) {
	return m.fires, nil
}

// --- webhookReceiverStore ---------------------------------------------------

func (m *webhookE2EStore) GetTriggerByID(_ context.Context, triggerID string) (*wf.TriggerRow, error) {
	r, ok := m.triggers[triggerID]
	if !ok {
		return nil, wf.ErrNotFound
	}
	return r, nil
}
func (m *webhookE2EStore) GetWorkflow(_ context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}
func (m *webhookE2EStore) RecordWebhookDelivery(_ context.Context, webhookID, dedupKey string) error {
	key := webhookID + ":" + dedupKey
	if m.delivered[key] {
		return wf.ErrDedupConflict
	}
	m.delivered[key] = true
	return nil
}
func (m *webhookE2EStore) CreateWorkflowRunWithFire(_ context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error {
	m.fires = append(m.fires, fire)
	m.runs = append(m.runs, run)
	return nil
}
func (m *webhookE2EStore) CreateTriggerFire(_ context.Context, row *wf.TriggerFireRow) error {
	m.fires = append(m.fires, row)
	return nil
}

// #1412 accounting, reused by validation_error fires (0059 D3).
func (m *webhookE2EStore) IncrementTriggerFailures(_ context.Context, triggerID string) (int, error) {
	m.triggerFail[triggerID]++
	return m.triggerFail[triggerID], nil
}
func (m *webhookE2EStore) DisableTrigger(_ context.Context, triggerID string) error {
	m.disabled[triggerID] = true
	return nil
}

// compile-time: the one store satisfies both handler surfaces.
var _ triggerStore = (*webhookE2EStore)(nil)
var _ webhookReceiverStore = (*webhookE2EStore)(nil)

// TestWebhookE2E_AdvertisedURLDelivers pins the composed contract this
// seam broke: the URL create/rotate hands out, signed, POSTed to the
// receiver, delivers — and the unhappy paths (bad signature, unknown
// id, duplicate delivery) fail closed without side effects.
func TestWebhookE2E_AdvertisedURLDelivers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newWebhookE2EStore()
	store.workflows["wf-e2e"] = &wf.WorkflowRow{
		ID: "wf-e2e", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-e2e",
		SpecJSON: json.RawMessage(`{"nodes":[],"edges":[]}`), TargetWorkspaceID: strPtrWF("ws-e2e"),
	}

	// Owner-side router: real TriggersHandler (create + rotate).
	owner := gin.New()
	th := NewUserTriggersHandler(store, nil, symmetricCipher{})
	g := owner.Group("/api/v1/me/triggers")
	g.Use(func(c *gin.Context) { c.Set("userID", "user-e2e"); c.Next() })
	g.POST("", th.UserCreate)
	g.POST("/:id/rotate-secret", th.UserRotateWebhookSecret)

	// 1. Create a webhook trigger; take the advertised URL VERBATIM.
	createBody := `{"name":"e2e-hook","sourceType":"webhook","sourceConfig":{"method":"POST"},"workflowId":"wf-e2e"}`
	w := httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/me/triggers", bytes.NewReader([]byte(createBody))))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created struct {
		Trigger    map[string]any `json:"trigger"`
		WebhookURL string         `json:"webhookUrl"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.WebhookURL, "create must advertise a hook URL")
	triggerID, _ := created.Trigger["id"].(string)
	require.NotEmpty(t, triggerID)

	// 2. Rotate: the owner's only path to a signing secret.
	w = httptest.NewRecorder()
	owner.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/me/triggers/"+triggerID+"/rotate-secret", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var rotated struct {
		WebhookSecret string `json:"webhookSecret"`
		WebhookURL    string `json:"webhookUrl"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rotated))
	require.Equal(t, created.WebhookURL, rotated.WebhookURL, "rotate advertises the SAME URL as create")

	// 3. Receiver router: real handler, SAME store. POST the advertised
	//    URL verbatim with a valid HMAC signature.
	recv := setupWebhookRouter(t, store, symmetricCipher{})
	payload := `{"event":"e2e"}`
	sig := "sha256=" + hex.EncodeToString(hmacSHA256([]byte(payload), []byte(rotated.WebhookSecret)))
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, created.WebhookURL, bytes.NewReader([]byte(payload)))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Request-ID", "e2e-delivery-1")
	recv.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code, "advertised URL + valid signature must deliver: %s", w.Body.String())
	require.Len(t, store.fires, 1, "fire row recorded")
	require.Len(t, store.runs, 1, "workflow run queued via CreateWorkflowRunWithFire")
	assert.Equal(t, "run_workflow", store.fires[0].ActionType)
	assert.Equal(t, "wf-e2e", store.runs[0].WorkflowID)

	// 4a. Bad signature → 401, no new fire.
	bad := httptest.NewRequest(http.MethodPost, created.WebhookURL, bytes.NewReader([]byte(payload)))
	bad.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(hmacSHA256([]byte(payload), []byte("wrong"))))
	bad.Header.Set("X-Request-ID", "e2e-delivery-2")
	w = httptest.NewRecorder()
	recv.ServeHTTP(w, bad)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Len(t, store.fires, 1, "rejected signature must not record a fire")

	// 4b. Unknown id → 404.
	w = httptest.NewRecorder()
	recv.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+triggerID+"0000", bytes.NewReader([]byte(payload))))
	assert.Equal(t, http.StatusNotFound, w.Code)

	// 4c. Duplicate delivery (same X-Request-ID) → 200 duplicate, no new fire.
	w = httptest.NewRecorder()
	dup := httptest.NewRequest(http.MethodPost, created.WebhookURL, bytes.NewReader([]byte(payload)))
	dup.Header.Set("X-Hub-Signature-256", sig)
	dup.Header.Set("X-Request-ID", "e2e-delivery-1")
	recv.ServeHTTP(w, dup)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "duplicate")
	assert.Len(t, store.fires, 1, "dedup must not double-fire")
}

func hmacSHA256(body, key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return mac.Sum(nil)
}
