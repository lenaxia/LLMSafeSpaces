// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOrgCredStore implements both CredentialStore and the org binding/auto-apply
// store for testing. It stores ciphertext verbatim and tracks call counts so
// tests can assert side effects.
type fakeOrgCredStore struct {
	creds            map[string]*secrets.CredentialRow
	createErr        error
	updateErr        error
	getErr           error
	bindErr          error
	createCalls      int
	updateCalls      int
	bindCalls        int
	getCalls         int
	getFailOnAttempt int // when >0, GetCredential fails on this call number (1-indexed)
	lastCreateCT     []byte
	lastUpdateCT     []byte
	lastUpdateKV     int
	lastUpdateName   *string
}

func newFakeOrgCredStore() *fakeOrgCredStore {
	return &fakeOrgCredStore{creds: make(map[string]*secrets.CredentialRow)}
}

func (f *fakeOrgCredStore) CreateCredential(_ context.Context, ownerType, ownerID string, row *secrets.CredentialRow) error {
	f.createCalls++
	if f.createErr != nil {
		return f.createErr
	}
	row.OwnerType = ownerType
	row.OwnerID = ownerID
	f.lastCreateCT = row.Ciphertext
	f.creds[row.ID] = row
	return nil
}

func (f *fakeOrgCredStore) ListCredentials(_ context.Context, ownerType, ownerID string) ([]*secrets.CredentialRow, error) {
	out := make([]*secrets.CredentialRow, 0, len(f.creds))
	for _, c := range f.creds {
		if c.OwnerType == ownerType && c.OwnerID == ownerID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeOrgCredStore) GetCredential(_ context.Context, _, _, credID string) (*secrets.CredentialRow, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getFailOnAttempt > 0 && f.getCalls == f.getFailOnAttempt {
		return nil, context.Canceled
	}
	c, ok := f.creds[credID]
	if !ok {
		return nil, nil
	}
	return c, nil
}

func (f *fakeOrgCredStore) UpdateCredential(_ context.Context, _, _, credID string, row *secrets.CredentialRow) error {
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	c, ok := f.creds[credID]
	if !ok {
		return nil
	}
	// Mirror the production COALESCE semantics: a nil field on the update row
	// means "don't change"; a non-nil field overwrites.
	if row.Name != "" {
		c.Name = row.Name
		f.lastUpdateName = &row.Name
	}
	if row.Kind != "" {
		c.Kind = row.Kind
	}
	if row.Slug != "" {
		c.Slug = row.Slug
	}
	if row.Ciphertext != nil {
		c.Ciphertext = row.Ciphertext
		c.KeyVersion = row.KeyVersion
		f.lastUpdateCT = row.Ciphertext
		f.lastUpdateKV = row.KeyVersion
	}
	if row.ModelAllowlist != nil {
		c.ModelAllowlist = row.ModelAllowlist
	}
	if row.ModelContextLimits != nil {
		c.ModelContextLimits = row.ModelContextLimits
	}
	if row.ModelOutputLimits != nil {
		c.ModelOutputLimits = row.ModelOutputLimits
	}
	return nil
}

func (f *fakeOrgCredStore) DeleteCredential(_ context.Context, _, _, credID string) error {
	if _, ok := f.creds[credID]; !ok {
		return pgx.ErrNoRows
	}
	delete(f.creds, credID)
	return nil
}

func (f *fakeOrgCredStore) BindCredentialToAllOrgWorkspaces(_ context.Context, _, _ string) error {
	f.bindCalls++
	return f.bindErr
}

func (f *fakeOrgCredStore) CreateOrgAutoApply(_ context.Context, _, _ string, _ int) error {
	return nil
}
func (f *fakeOrgCredStore) ListOrgAutoApply(_ context.Context, _ string) ([]*secrets.AutoApplyRule, error) {
	return nil, nil
}
func (f *fakeOrgCredStore) DeleteOrgAutoApply(_ context.Context, _, _ string) error { return nil }

func setupOrgCredRouter(h *OrgCredentialsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/orgs/:id/credentials")
	g.POST("", h.Create)
	g.GET("", h.List)
	g.PUT("/:credID", h.Update)
	g.DELETE("/:credID", h.Delete)
	g.GET("/:credID/models", h.ProbeModels)
	return r
}

// TestOrgCredentials_Create_Success verifies the happy path: a request with a
// valid apiKey is encrypted with the org KEK (derived from "org-credentials"),
// stored, bound to org workspaces, and returns 201.
func TestOrgCredentials_Create_Success(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"team-anthropic","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "team-anthropic", resp.Name)
	assert.Equal(t, "anthropic", resp.Slug)
	assert.Equal(t, "org-1", resp.OrgID)
	assert.NotEmpty(t, resp.ID)

	require.Equal(t, 1, store.createCalls, "credential must be stored")
	require.Equal(t, 1, store.bindCalls, "credential must be bound to org workspaces")
	require.NotEmpty(t, store.lastCreateCT, "stored ciphertext must be non-empty")

	// The stored ciphertext must decrypt back to the original provider data.
	// US-57.1: round-trip via provider.Decrypt (not raw DecryptSecret) because
	// provider output now carries the lkms:v1: prefix for CompositeProvider
	// dispatch — DecryptSecret does not understand the prefix.
	pd, err := provider.Decrypt(context.Background(), store.lastCreateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "anthropic", decoded.Kind)
	assert.Equal(t, "anthropic", decoded.Slug)
	assert.Equal(t, "sk-ant-123", decoded.APIKey)
}

// TestOrgCredentials_Create_NilKEK_503 verifies that when the server KEK is
// unavailable (nil deriver), Create returns 503 and does NOT store anything.
// This anchors the fail-closed contract: never store plaintext or encrypt with
// a nil key.
func TestOrgCredentials_Create_NilKEK_503(t *testing.T) {
	store := newFakeOrgCredStore()
	provider := secrets.RootKeyProvider(nil)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"openai","slug":"openai","apiKey":"sk-1"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, store.createCalls, "nothing must be stored when KEK is nil")
}

func TestOrgCredentials_Create_MissingAPIKey_400(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"openai","slug":"openai"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, store.createCalls)
}

// TestOrgCredentials_Create_BindFails_Returns201WithWarning verifies that a
// bind failure (e.g. no workspaces yet) does not fail the whole create — the
// credential is still stored, and the response carries a bindWarning. This is
// the contract in org_credentials.go:106-112.
func TestOrgCredentials_Create_BindFails_Returns201WithWarning(t *testing.T) {
	store := newFakeOrgCredStore()
	store.bindErr = context.DeadlineExceeded
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"openai","slug":"openai","apiKey":"sk-1"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "credential must still be created on bind failure")
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.BindWarning, "bind failure must surface a warning")
}

// TestOrgCredentials_Update_APIKeyRotation_Success verifies the re-encryption
// path: an existing credential (encrypted with org KEK) is decrypted, its
// apiKey is replaced, and the result is re-encrypted and stored with an
// incremented key version.
func TestOrgCredentials_Update_APIKeyRotation_Success(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	// Seed an existing credential encrypted with the org KEK.
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "old-key"}) //nolint:gosec
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "old-name", Kind: "anthropic", Slug: "anthropic",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"apiKey":"rotated-key"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, 1, store.updateCalls, "credential must be updated")
	require.Equal(t, 2, store.lastUpdateKV, "key version must increment from 1 to 2")
	require.NotEmpty(t, store.lastUpdateCT, "re-encrypted ciphertext must be stored")
	require.NotEqual(t, existingCT, store.lastUpdateCT, "ciphertext must change after rotation")

	// Decrypt the stored ciphertext and confirm the API key rotated while
	// other fields survived the round trip.
	pd, err := provider.Decrypt(context.Background(), store.lastUpdateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "rotated-key", decoded.APIKey)
	assert.Equal(t, "anthropic", decoded.Kind, "kind must survive rotation")
	assert.NotEmpty(t, decoded.Slug, "slug must survive rotation")
}

// TestOrgCredentials_Update_NilKEK_503 verifies that rotating the API key when
// the server KEK is unavailable returns 503 and does NOT corrupt the stored
// credential. This is the fail-closed contract for the re-encrypt path.
func TestOrgCredentials_Update_NilKEK_503(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "old"}) //nolint:gosec
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	provider := secrets.RootKeyProvider(nil)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"apiKey":"new"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, 0, store.updateCalls, "nothing must be written when KEK is nil")
	// Existing credential must be untouched.
	require.Equal(t, existingCT, store.creds["cred-1"].Ciphertext, "stored ciphertext must not be corrupted")
}

func TestOrgCredentials_Update_NotFound_404(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"new"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/missing", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, store.updateCalls)
}

// TestOrgCredentials_Update_NameOnly_NoReEncrypt verifies that updating only
// metadata (name) without an apiKey does NOT re-encrypt the ciphertext or bump
// the key version. The handler may still derive the KEK for read-only baseURL
// display decryption (which never writes ciphertext). This anchors the
// conditional-re-encryption contract in org_credentials.go.
func TestOrgCredentials_Update_NameOnly_NoReEncrypt(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "kept"}) //nolint:gosec
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "old", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 3,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"renamed"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, store.updateCalls)
	require.Nil(t, store.lastUpdateCT, "no re-encryption (ciphertext write) when apiKey absent")
	require.Equal(t, "renamed", store.creds["cred-1"].Name)
	assert.Equal(t, 3, store.creds["cred-1"].KeyVersion, "key version must not change without re-encryption")
}

// TestOrgCredentials_Update_NameOnly_PreservesLimits verifies that a metadata-
// only update (name change) does NOT wipe model_context_limits. Regression test
// for the critical COALESCE bug: the unified UpdateCredential had a nil→{}
// normalization that converted "don't change" into "clear all".
func TestOrgCredentials_Update_NameOnly_PreservesLimits(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "kept"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID:                 "cred-1",
		OwnerType:          "org",
		OwnerID:            "org-1",
		Name:               "old",
		Kind:               "openai",
		Slug:               "openai",
		Ciphertext:         existingCT,
		KeyVersion:         1,
		ModelAllowlist:     []string{"glm-5.1"},
		ModelContextLimits: map[string]int{"glm-5.1": 200000},
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"renamed"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "renamed", store.creds["cred-1"].Name)
	assert.Equal(t, []string{"glm-5.1"}, store.creds["cred-1"].ModelAllowlist, "model_allowlist must be preserved on name-only update")
	assert.Equal(t, map[string]int{"glm-5.1": 200000}, store.creds["cred-1"].ModelContextLimits, "model_context_limits must be preserved on name-only update")
}

// TestOrgCredentials_Update_CorruptCiphertext_500 verifies that rotating the
// API key against a credential whose ciphertext is unreadable returns 500 (not
// 200 with a zeroed credential). Mirrors the admin-credential C-4 fix.
func TestOrgCredentials_Update_CorruptCiphertext_500(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	// Ciphertext was encrypted with a DIFFERENT key — simulates DB corruption
	// or a KEK rotation that lost the old key.
	differentKEK := make([]byte, 32)
	corruptCT, err := secrets.EncryptSecret(differentKEK,
		[]byte(`{"kind":"openai","slug":"openai","apiKey":"original"}`))
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Kind: "openai", Slug: "openai",
		Ciphertext: corruptCT,
		KeyVersion: 1,
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"apiKey":"rotated"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 0, store.updateCalls, "corrupt ciphertext must not be written back")
}

// --- ProbeModels (B-1) ---

// TestOrgCredentials_ProbeModels_NotFound verifies 404 for an unknown credID.
func TestOrgCredentials_ProbeModels_NotFound(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials/missing/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestOrgCredentials_ProbeModels_NilKEK_503 verifies that probing when the
// server KEK is unavailable returns 503 (fail-closed — cannot decrypt).
func TestOrgCredentials_ProbeModels_NilKEK_503(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "sk-x", BaseURL: "http://localhost:19998/v1"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	h := NewOrgCredentialsHandler(store, store, nil, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials/cred-1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestOrgCredentials_ProbeModels_NoBaseURL verifies a graceful warning (200)
// when the credential has no baseURL (native provider).
func TestOrgCredentials_ProbeModels_NoBaseURL(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 2)
	}
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	createBody := `{"name":"native","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	req, _ = http.NewRequest("GET", "/api/v1/orgs/org-1/credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	assert.NotEmpty(t, probe.Warning, "no-baseURL credential must return a warning")
	assert.Empty(t, probe.Models)
}

// TestOrgCredentials_ProbeModels_Success verifies that with a reachable fake
// provider, the probe returns the model list with saved context limits merged.
func TestOrgCredentials_ProbeModels_Success(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		assert.Equal(t, "Bearer sk-probe-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"},{"id":"glm-5.2"},{"id":"classifier"}]}`))
	}))
	defer fakeProvider.Close()

	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 4)
	}
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	createBody, _ := json.Marshal(map[string]interface{}{
		"name":               "thekao",
		"kind":               "openai_compatible",
		"slug":               "thekao-cloud",
		"apiKey":             "sk-probe-key",
		"baseURL":            fakeProvider.URL + "/v1",
		"modelAllowlist":     []string{"glm-5.1", "glm-5.2"},
		"modelContextLimits": map[string]int{"glm-5.1": 200000, "glm-5.2": 1000000},
		"modelOutputLimits":  map[string]int{"glm-5.1": 8192, "glm-5.2": 16384},
	})
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	req, _ = http.NewRequest("GET", "/api/v1/orgs/org-1/credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))

	assert.Empty(t, probe.Warning)
	require.Len(t, probe.Models, 3)
	byID := map[string]ProbeModelEntry{}
	for _, m := range probe.Models {
		byID[m.ID] = m
	}
	assert.Equal(t, 200000, byID["glm-5.1"].ContextLimit)
	assert.Equal(t, 1000000, byID["glm-5.2"].ContextLimit)
	assert.Equal(t, 0, byID["classifier"].ContextLimit, "unsaved model has no context limit")
	assert.Equal(t, 8192, byID["glm-5.1"].OutputLimit)
	assert.Equal(t, 16384, byID["glm-5.2"].OutputLimit)
	assert.Equal(t, 0, byID["classifier"].OutputLimit, "unsaved model has no output limit")
}

// --- List (B-2): camelCase keys + baseURL extraction ---

// TestOrgCredentials_List_CamelCaseAndBaseURL verifies that the List response
// uses camelCase JSON keys (fixing the latent PascalCase serialization bug) and
// that baseURL is extracted from each credential's ciphertext via decryption.
func TestOrgCredentials_List_CamelCaseAndBaseURL(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 5)
	}

	// Seed two credentials: one with a baseURL, one without.
	for _, tc := range []struct {
		id, baseURL string
		limits      map[string]int
	}{
		{"cred-a", "https://api.example.com/v1", map[string]int{"glm-5.1": 200000}},
		{"cred-b", "", nil},
	} {
		pd := secrets.LLMProviderData{Kind: "openai_compatible", Slug: "custom", APIKey: "sk-" + tc.id, BaseURL: tc.baseURL}
		plain, _ := json.Marshal(pd)
		ct, err := secrets.EncryptSecret(kek, plain)
		require.NoError(t, err)
		store.creds[tc.id] = &secrets.CredentialRow{
			ID: tc.id, OwnerType: "org", OwnerID: "org-1", Name: tc.id, Kind: "openai_compatible", Slug: "custom", ModelAllowlist: []string{}, ModelContextLimits: tc.limits,
			Ciphertext: ct,
			KeyVersion: 1,
		}
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Unmarshal into a generic map to assert the raw JSON keys are camelCase
	// (not Go struct field names like "ModelAllowlist").
	var raw []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw, 2)

	// Every entry must expose camelCase keys.
	keys := map[string]bool{}
	for _, entry := range raw {
		for k := range entry {
			keys[k] = true
		}
	}
	for _, want := range []string{"id", "orgId", "name", "kind", "slug", "baseURL", "modelAllowlist", "modelContextLimits", "modelOutputLimits", "createdAt", "updatedAt"} {
		assert.True(t, keys[want], "List JSON must include camelCase key %q (got %v)", want, keys)
	}
	for _, forbidden := range []string{"ID", "OrgID", "Provider", "ModelAllowlist", "ModelContextLimits", "ModelOutputLimits", "CreatedAt"} {
		assert.False(t, keys[forbidden], "List JSON must NOT include PascalCase key %q", forbidden)
	}

	// Typed decode to verify baseURL extraction per credential.
	var typed []CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &typed))
	byID := map[string]CredentialResponse{}
	for _, c := range typed {
		byID[c.ID] = c
	}
	assert.Equal(t, "https://api.example.com/v1", byID["cred-a"].BaseURL, "baseURL must be decrypted for cred-a")
	assert.Equal(t, "", byID["cred-b"].BaseURL, "cred-b has no baseURL")
	assert.Equal(t, 200000, byID["cred-a"].ModelContextLimits["glm-5.1"])
}

// TestOrgCredentials_List_Empty verifies the empty-list contract returns [] not null.
func TestOrgCredentials_List_Empty(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "[]", w.Body.String(), "empty list must serialize as []")
}

// --- Create / Update (B-3): full responses ---

// TestOrgCredentials_Create_FullResponse verifies that Create returns the full
// CredentialResponse (not the old sparse {id,orgId,name,provider}), including
// modelAllowlist, modelContextLimits, baseURL, and timestamps.
func TestOrgCredentials_Create_FullResponse(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 6)
	}
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	createBody, _ := json.Marshal(map[string]interface{}{
		"name":               "thekao",
		"kind":               "openai_compatible",
		"slug":               "thekao-cloud",
		"apiKey":             "sk-x",
		"baseURL":            "https://api.example.com/v1",
		"modelAllowlist":     []string{"glm-5.1"},
		"modelContextLimits": map[string]int{"glm-5.1": 200000},
	})
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "org-1", resp.OrgID)
	assert.Equal(t, "thekao", resp.Name)
	assert.Equal(t, "thekao-cloud", resp.Slug)
	assert.Equal(t, "https://api.example.com/v1", resp.BaseURL, "Create response must echo baseURL")
	assert.Equal(t, []string{"glm-5.1"}, resp.ModelAllowlist)
	assert.Equal(t, 200000, resp.ModelContextLimits["glm-5.1"])
	assert.NotEmpty(t, resp.CreatedAt, "Create response must include createdAt")
	assert.NotEmpty(t, resp.UpdatedAt, "Create response must include updatedAt")
}

// TestOrgCredentials_Update_FullResponse verifies that Update returns the full
// CredentialResponse (not the old sparse {id,message}) after a metadata-only
// update (no re-encryption).
func TestOrgCredentials_Update_FullResponse(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 7)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "kept", BaseURL: "https://api.openai.com/v1"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "old", Kind: "openai", Slug: "openai", ModelAllowlist: []string{}, ModelContextLimits: map[string]int{},
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"renamed","modelAllowlist":["gpt-4o"],"modelContextLimits":{"gpt-4o":128000}}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "cred-1", resp.ID)
	assert.Equal(t, "renamed", resp.Name)
	assert.Equal(t, "openai", resp.Slug)
	assert.Equal(t, "https://api.openai.com/v1", resp.BaseURL, "Update response must decrypt baseURL")
	assert.Equal(t, []string{"gpt-4o"}, resp.ModelAllowlist)
	assert.Equal(t, 128000, resp.ModelContextLimits["gpt-4o"])
}

// TestOrgCredentials_Update_BaseURLOnly_Persists verifies that updating baseURL
// WITHOUT an apiKey still re-encrypts and persists the new baseURL. Regression
// test for the silent-drop bug (old condition was `req.APIKey != nil` only).
func TestOrgCredentials_Update_BaseURLOnly_Persists(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 8)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "kept", BaseURL: "https://old.example.com/v1"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "k", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"baseURL":"https://new.example.com/v1"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, 1, store.updateCalls, "Update must be called")
	require.NotNil(t, store.lastUpdateCT, "ciphertext must be re-encrypted on baseURL-only update")
	require.Equal(t, 2, store.lastUpdateKV, "key version must increment")

	// Decrypt and verify the new baseURL persisted while apiKey survived.
	pd, err := provider.Decrypt(context.Background(), store.lastUpdateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "https://new.example.com/v1", decoded.BaseURL, "new baseURL must persist")
	assert.Equal(t, "kept", decoded.APIKey, "apiKey must survive baseURL-only update")

	// Response must reflect the new baseURL.
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "https://new.example.com/v1", resp.BaseURL)
}

// TestOrgCredentials_Create_GetFails_GracefulFallback verifies that when the
// post-create GetCredential fails, Create still returns 201 with a minimal
// response (the credential was stored).
func TestOrgCredentials_Create_GetFails_GracefulFallback(t *testing.T) {
	store := newFakeOrgCredStore()
	store.getFailOnAttempt = 1 // first GetCredential call (post-create) fails
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"openai","slug":"openai","apiKey":"sk-1"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, "credential was stored; must still return 201")
	require.Equal(t, 1, store.createCalls, "credential must be created")
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.ID, "fallback response must still carry the ID")
	assert.Equal(t, "org-1", resp.OrgID)
}

// TestOrgCredentials_Update_GetFails_GracefulFallback verifies that when the
// post-update GetCredential fails, Update still returns 200 with a minimal
// response (the metadata update was persisted).
func TestOrgCredentials_Update_GetFails_GracefulFallback(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "k"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "old", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}
	store.getFailOnAttempt = 2 // 1st Get (existing) succeeds; 2nd Get (post-update) fails

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"renamed"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "update was persisted; must still return 200")
	require.Equal(t, 1, store.updateCalls, "Update must be called")
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "cred-1", resp.ID, "fallback response must still carry the ID")
}

// TestOrgCredentials_Update_APIKeyAndBaseURL_Combined verifies that updating
// both apiKey and baseURL in a single request re-encrypts once with both
// fields applied (covers the combined-change path the reviewer requested).
func TestOrgCredentials_Update_APIKeyAndBaseURL_Combined(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 9)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "old-key", BaseURL: "https://old.example.com/v1"})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1", Name: "k", Kind: "openai", Slug: "openai",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"apiKey":"new-key","baseURL":"https://new.example.com/v1"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, store.lastUpdateCT, "must re-encrypt once for both fields")
	require.Equal(t, 2, store.lastUpdateKV, "single key-version increment for the combined update")

	pd, err := provider.Decrypt(context.Background(), store.lastUpdateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "new-key", decoded.APIKey)
	assert.Equal(t, "https://new.example.com/v1", decoded.BaseURL)
}

// TestOrgCredentials_List_PartialDecryptFailure_NonFatal verifies that when
// one credential's ciphertext is unreadable (corrupt / wrong key), List still
// returns all rows with that row's baseURL omitted rather than failing the
// whole response. This is the non-fatal-decrypt contract.
func TestOrgCredentials_List_PartialDecryptFailure_NonFatal(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 10)
	}
	// cred-good: encrypted with the matching KEK → decrypts, baseURL extracted.
	goodPlain, _ := json.Marshal(secrets.LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "k1", BaseURL: "https://good.example.com/v1"})
	goodCT, err := secrets.EncryptSecret(kek, goodPlain)
	require.NoError(t, err)
	store.creds["cred-good"] = &secrets.CredentialRow{
		ID: "cred-good", OwnerType: "org", OwnerID: "org-1", Name: "good", Kind: "openai", Slug: "openai",
		Ciphertext: goodCT,
		KeyVersion: 1,
	}
	// cred-corrupt: ciphertext encrypted with a DIFFERENT key → decrypt fails.
	otherKEK := make([]byte, 32)
	corruptCT, err := secrets.EncryptSecret(otherKEK, []byte(`{"kind":"openai","slug":"openai","apiKey":"k2","baseURL":"https://corrupt.example.com/v1"}`))
	require.NoError(t, err)
	store.creds["cred-corrupt"] = &secrets.CredentialRow{
		ID: "cred-corrupt", OwnerType: "org", OwnerID: "org-1", Name: "corrupt", Kind: "openai", Slug: "openai",
		Ciphertext: corruptCT,
		KeyVersion: 1,
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "partial decrypt failure must not fail the whole list")
	var resp []CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	byID := map[string]CredentialResponse{}
	for _, r := range resp {
		byID[r.ID] = r
	}
	assert.Equal(t, "https://good.example.com/v1", byID["cred-good"].BaseURL, "good credential must have baseURL")
	assert.Equal(t, "", byID["cred-corrupt"].BaseURL, "corrupt credential must omit baseURL but still appear")
	assert.Equal(t, "corrupt", byID["cred-corrupt"].Name, "corrupt credential metadata must still be returned")
}

// TestOrgCredentials_List_OrderIsNewestFirst verifies that List returns
// credentials in DESCENDING created_at order (newest first). Regression test
// for C2: the unified ListCredentials returns ASC, which flipped the org list
// from newest-first to oldest-first.
func TestOrgCredentials_List_OrderIsNewestFirst(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)

	// Seed two creds with distinct timestamps.
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		id    string
		hours int
	}{
		{"cred-old", 0},
		{"cred-new", 1},
	} {
		ts := base.Add(time.Duration(tc.hours) * time.Hour)
		store.creds[tc.id] = &secrets.CredentialRow{
			ID:        tc.id,
			OwnerType: "org",
			OwnerID:   "org-1",
			Name:      tc.id,
			Kind:      "openai",
			Slug:      "openai",
			CreatedAt: ts,
			UpdatedAt: ts,
		}
		_ = i
	}

	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/orgs/org-1/credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp []CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	assert.Equal(t, "cred-new", resp[0].ID, "newest credential must be first (DESC order)")
	assert.Equal(t, "cred-old", resp[1].ID, "oldest credential must be last")
}

// TestOrgCredentials_Delete_NotFound_Returns204 verifies that deleting a
// non-existent credential returns 204 (idempotent), not 500. Regression test
// for C3: the unified DeleteCredential returns pgx.ErrNoRows on 0 rows.
func TestOrgCredentials_Delete_NotFound_Returns204(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	h := NewOrgCredentialsHandler(store, store, mustStaticProv(kek), &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/orgs/org-1/credentials/does-not-exist", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code, "deleting a missing credential must be idempotent (204)")
}

// TestOrgCredentials_Update_SlugRename_PropagatesToCiphertext is the Epic 55
// regression guard for the stale-ciphertext-on-rename bug. When the org
// handler updates a credential's slug, the encrypted LLMProviderData blob
// MUST be re-encrypted with the new slug — otherwise the blob carries the
// OLD slug, and on injection the materialize path (which keys agent-config.json
// by pd.Slug pulled from the decrypted blob) emits the OLD slug as the
// provider-map key. The wire format never sees the rename.
//
// Trace pre-fix:
//  1. PUT /orgs/:id/credentials/:id with {"slug":"new-slug"}.
//  2. Handler updates row.Slug column but skips re-encrypt (the condition
//     was `req.APIKey != nil || req.BaseURL != nil`).
//  3. Ciphertext still decrypts to LLMProviderData{Slug:"old-slug",...}.
//  4. InjectSecrets -> buildSecretsJSON sets Name: pd.Slug = "old-slug".
//  5. agent-config.json provider map has the old key.
//
// The fix mirrors the admin handler: include Kind/Slug in the re-encrypt
// condition.
func TestOrgCredentials_Update_SlugRename_PropagatesToCiphertext(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	// Seed: an existing org credential whose stored ciphertext encodes
	// slug="old-slug" inside the LLMProviderData blob.
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{
		Kind:   "openai_compatible",
		Slug:   "old-slug",
		APIKey: "sk-unchanged",
	})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1",
		Name:       "thekao cloud",
		Kind:       "openai_compatible",
		Slug:       "old-slug",
		Ciphertext: existingCT,
		KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	// Rename the slug; do NOT touch apiKey or baseURL.
	body := `{"slug":"new-slug"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	// The DB row's slug column must reflect the rename.
	require.Equal(t, "new-slug", store.creds["cred-1"].Slug, "row column slug must be renamed")

	// Critical: the encrypted blob must ALSO carry the new slug. Otherwise
	// the materialize path (which reads pd.Slug from the decrypted blob to
	// key agent-config.json) emits the OLD slug.
	require.NotEmpty(t, store.lastUpdateCT, "ciphertext must be rewritten on slug rename")
	require.NotEqual(t, existingCT, store.lastUpdateCT,
		"ciphertext must change when slug is renamed — the slug lives INSIDE the encrypted blob")

	pd, err := provider.Decrypt(context.Background(), store.lastUpdateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "new-slug", decoded.Slug,
		"decrypted slug must be the renamed value — this is what reaches opencode as providerID")
	assert.Equal(t, "openai_compatible", decoded.Kind, "kind survives the rename")
	assert.Equal(t, "sk-unchanged", decoded.APIKey, "apiKey survives the rename")
}

// TestOrgCredentials_Update_KindChange_PropagatesToCiphertext is the same
// regression for the Kind field. Kind also lives inside the encrypted blob
// (LLMProviderData.Kind) and is read out during materialization.
func TestOrgCredentials_Update_KindChange_PropagatesToCiphertext(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{
		Kind:   "openai_compatible",
		Slug:   "stable-slug",
		APIKey: "sk-stable",
	})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1",
		Name: "x", Kind: "openai_compatible", Slug: "stable-slug",
		Ciphertext: existingCT, KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"kind":"anthropic"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "anthropic", store.creds["cred-1"].Kind, "row column kind must change")

	require.NotEmpty(t, store.lastUpdateCT, "ciphertext must be rewritten on kind change")
	pd, err := provider.Decrypt(context.Background(), store.lastUpdateCT)
	require.NoError(t, err)
	var decoded secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(pd, &decoded))
	assert.Equal(t, "anthropic", decoded.Kind, "decrypted kind must be the new value")
	assert.Equal(t, "stable-slug", decoded.Slug, "slug survives the kind change")
}

// TestOrgCredentials_Create_InvalidKind_400 — boundary validation for the
// org handler. The kind value "custom" was the legacy SDK kind for
// OpenAI-compatible endpoints; Epic 55 replaces it with "openai_compatible"
// and the validator must reject the old name.
func TestOrgCredentials_Create_InvalidKind_400(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"custom","slug":"x","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code,
		"invalid kind must surface as 400 from the handler boundary, not 500 from the DB CHECK")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "kind", resp["field"])
}

// TestOrgCredentials_Create_InvalidSlug_400 — same for slug.
func TestOrgCredentials_Create_InvalidSlug_400(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"name":"x","kind":"anthropic","slug":"has space","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/orgs/org-1/credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "slug", resp["field"])
}

// TestOrgCredentials_Update_InvalidKind_400 — validation also fires on the
// partial-update path.
func TestOrgCredentials_Update_InvalidKind_400(t *testing.T) {
	store := newFakeOrgCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	existingPlaintext, _ := json.Marshal(secrets.LLMProviderData{
		Kind: "openai_compatible", Slug: "valid-slug", APIKey: "sk-existing",
	})
	existingCT, err := secrets.EncryptSecret(kek, existingPlaintext)
	require.NoError(t, err)
	store.creds["cred-1"] = &secrets.CredentialRow{
		ID: "cred-1", OwnerType: "org", OwnerID: "org-1",
		Name: "x", Kind: "openai_compatible", Slug: "valid-slug",
		Ciphertext: existingCT, KeyVersion: 1,
	}

	provider := mustStaticProv(kek)
	h := NewOrgCredentialsHandler(store, store, provider, &mockOrgAuthService{userID: "admin-1"})
	router := setupOrgCredRouter(h)

	body := `{"kind":"custom"}`
	req, _ := http.NewRequest("PUT", "/api/v1/orgs/org-1/credentials/cred-1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
