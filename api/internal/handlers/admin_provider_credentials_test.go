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

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeCredentialStore implements handlers.CredentialStore for testing. It is
// shared by the admin, user, and org credential handler tests; each scopes its
// rows by (ownerType, ownerID) so a single fake backs all three handlers.
type fakeCredentialStore struct {
	creds     map[string]*secrets.CredentialRow // keyed by cred ID
	nextErr   error
	updateErr error // returned specifically by UpdateCredential
	createErr error // returned specifically by CreateCredential
}

func newFakeCredentialStore() *fakeCredentialStore {
	return &fakeCredentialStore{creds: make(map[string]*secrets.CredentialRow)}
}

// scopedCreds returns the credentials matching (ownerType, ownerID). The map is
// keyed by cred ID, so this filters by the row's embedded OwnerType/OwnerID.
func (f *fakeCredentialStore) scopedCreds(ownerType, ownerID string) []*secrets.CredentialRow {
	var out []*secrets.CredentialRow
	for _, c := range f.creds {
		if c.OwnerType == ownerType && c.OwnerID == ownerID {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeCredentialStore) CreateCredential(_ context.Context, ownerType, ownerID string, row *secrets.CredentialRow) error {
	if f.createErr != nil {
		err := f.createErr
		f.createErr = nil
		return err
	}
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return err
	}
	row.OwnerType = ownerType
	row.OwnerID = ownerID
	f.creds[row.ID] = row
	return nil
}

func (f *fakeCredentialStore) ListCredentials(_ context.Context, ownerType, ownerID string) ([]*secrets.CredentialRow, error) {
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return nil, err
	}
	return f.scopedCreds(ownerType, ownerID), nil
}

func (f *fakeCredentialStore) GetCredential(_ context.Context, ownerType, ownerID, id string) (*secrets.CredentialRow, error) {
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return nil, err
	}
	c, ok := f.creds[id]
	if !ok || c.OwnerType != ownerType || c.OwnerID != ownerID {
		return nil, nil
	}
	return c, nil
}

func (f *fakeCredentialStore) UpdateCredential(_ context.Context, ownerType, ownerID string, credID string, row *secrets.CredentialRow) error {
	if f.updateErr != nil {
		err := f.updateErr
		f.updateErr = nil
		return err
	}
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return err
	}
	row.ID = credID
	row.OwnerType = ownerType
	row.OwnerID = ownerID
	f.creds[credID] = row
	return nil
}

func (f *fakeCredentialStore) DeleteCredential(_ context.Context, ownerType, ownerID, id string) error {
	if f.nextErr != nil {
		err := f.nextErr
		f.nextErr = nil
		return err
	}
	c, ok := f.creds[id]
	if !ok || c.OwnerType != ownerType || c.OwnerID != ownerID {
		return pgx.ErrNoRows
	}
	delete(f.creds, id)
	return nil
}

// newFakeAdminCredStore returns a fakeCredentialStore for the admin handler
// tests (which historically used a dedicated fakeAdminCredStore). Kept as an
// alias for minimal diff against existing test bodies.
func newFakeAdminCredStore() *fakeCredentialStore { return newFakeCredentialStore() }

// mustStaticProv wraps a raw 32-byte key as a RootKeyProvider for handler
// tests (US-50.2: replaces the old AdminKeyDeriver callback).
func mustStaticProv(key []byte) secrets.RootKeyProvider {
	if key == nil {
		return nil
	}
	p, _ := secrets.NewStaticKeyProvider(key)
	return p
}

func setupAdminCredRouter(h *AdminProviderCredentialsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/admin/provider-credentials")
	g.POST("", h.Create)
	g.GET("", h.List)
	g.GET("/:id", h.Get)
	g.PUT("/:id", h.Update)
	g.DELETE("/:id", h.Delete)
	g.GET("/:id/models", h.ProbeModels)
	return r
}

func TestAdminProviderCredentials_Create_Success(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-anthropic", resp.Name)
	assert.Equal(t, "anthropic", resp.Slug)
	assert.NotEmpty(t, resp.ID)
	assert.Empty(t, resp.BaseURL) // not set
}

func TestAdminProviderCredentials_Create_MissingAPIKey(t *testing.T) {
	store := newFakeAdminCredStore()
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAdminProviderCredentials_Create_NilKEK(t *testing.T) {
	store := newFakeAdminCredStore()
	h := NewAdminProviderCredentialsHandler(store, nil)
	router := setupAdminCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestAdminProviderCredentials_List(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create one first.
	store.creds["id1"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform",
		ID: "id1", Name: "test", Kind: "openai", Slug: "openai",
		Ciphertext: mustEncrypt(t, kek, `{"kind":"openai","slug":"openai","apiKey":"sk-123"}`),
	}

	req, _ := http.NewRequest("GET", "/api/v1/admin/provider-credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var list []CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Len(t, list, 1)
	assert.Equal(t, "openai", list[0].Slug)
}

func TestAdminProviderCredentials_Get_NotFound(t *testing.T) {
	store := newFakeAdminCredStore()
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/admin/provider-credentials/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminProviderCredentials_Delete(t *testing.T) {
	store := newFakeAdminCredStore()
	store.creds["del-id"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform", ID: "del-id", Name: "x", Kind: "anthropic", Slug: "anthropic"}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/admin/provider-credentials/del-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, store.creds)
}

func TestAdminProviderCredentials_Update_Success(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	store.creds["upd-id"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform",
		ID: "upd-id", Name: "old", Kind: "anthropic", Slug: "anthropic",
		Ciphertext: mustEncrypt(t, kek, `{"kind":"anthropic","slug":"anthropic","apiKey":"old-key"}`),
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{"name":"new-name","kind":"anthropic","slug":"anthropic","apiKey":"new-key","baseURL":"https://custom.api"}`
	req, _ := http.NewRequest("PUT", "/api/v1/admin/provider-credentials/upd-id", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "new-name", resp.Name)
	// BaseURL is stored encrypted; not returned in response (verify via decrypt if needed)
}

func mustEncrypt(t *testing.T, kek []byte, plaintext string) []byte {
	t.Helper()
	ct, err := secrets.EncryptSecret(kek, []byte(plaintext))
	require.NoError(t, err)
	return ct
}

// TestAdminProviderCredentials_Delete_NotFound verifies L-1 fix: deleting a
// non-existent credential returns 404, not 204.
func TestAdminProviderCredentials_Delete_NotFound(t *testing.T) {
	store := newFakeAdminCredStore() // empty store — nothing to delete
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/admin/provider-credentials/missing-id", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestAdminProviderCredentials_Update_CorruptCiphertext_Returns500 verifies C-4 fix:
// attempting to rotate the API key when the existing ciphertext is unreadable returns 500
// with an actionable message, instead of silently encrypting a zeroed credential.
func TestAdminProviderCredentials_Update_CorruptCiphertext_Returns500(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i)
	}
	// Store a credential whose ciphertext was encrypted with a DIFFERENT key — simulates
	// the "unreadable ciphertext" scenario (wrong KEK, DB corruption, etc.).
	differentKEK := make([]byte, 32)
	store.creds["c1"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform",
		ID:         "c1",
		Name:       "test",
		Kind:       "openai",
		Slug:       "openai",
		Ciphertext: mustEncrypt(t, differentKEK, `{"kind":"openai","slug":"openai","apiKey":"original"}`),
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{"apiKey":"new-rotated-key"}` // triggers re-encrypt path
	req, _ := http.NewRequest("PUT", "/api/v1/admin/provider-credentials/c1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Must return 500 with an actionable error, NOT 200 with a zeroed credential.
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["error"], "unreadable")
}

// TestAdminProviderCredentials_Update_DuplicateProvider_Returns409 verifies M-4 fix:
// changing provider to one that already exists returns 409 Conflict, not 500.
func TestAdminProviderCredentials_Update_DuplicateProvider_Returns409(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	store.creds["c1"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform",
		ID: "c1", Name: "existing", Kind: "openai", Slug: "openai",
		Ciphertext: mustEncrypt(t, kek, `{"kind":"openai","slug":"openai","apiKey":"key1"}`),
	}
	// updateErr is consumed ONLY by UpdateCredential; GetCredential won't touch it.
	store.updateErr = &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{"name":"renamed"}`
	req, _ := http.NewRequest("PUT", "/api/v1/admin/provider-credentials/c1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestAdminProviderCredentials_AutoApply_NilStore_Returns503 verifies M-7 fix:
// all three auto-apply handlers return 503 when the store is nil.
func TestAdminProviderCredentials_AutoApply_NilStore_Returns503(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	// autoApplyStore is nil by default — do NOT call SetAutoApplyStore
	g := gin.New()
	g.POST("/api/v1/admin/provider-credentials/:id/auto-apply", h.CreateAutoApply)
	g.GET("/api/v1/admin/provider-credentials/:id/auto-apply", h.ListAutoApply)
	g.DELETE("/api/v1/admin/provider-credentials/:id/auto-apply/:targetType/:targetId", h.DeleteAutoApply)

	for _, tc := range []struct {
		method string
		url    string
		body   string
	}{
		{"POST", "/api/v1/admin/provider-credentials/c1/auto-apply", `{"targetType":"all"}`},
		{"GET", "/api/v1/admin/provider-credentials/c1/auto-apply", ""},
		{"DELETE", "/api/v1/admin/provider-credentials/c1/auto-apply/all/_", ""},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var bodyReader *bytes.Reader
			if tc.body != "" {
				bodyReader = bytes.NewReader([]byte(tc.body))
			} else {
				bodyReader = bytes.NewReader(nil)
			}
			req, _ := http.NewRequest(tc.method, tc.url, bodyReader)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, req)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "method %s should return 503", tc.method)
		})
	}
}

// TestAdminProviderCredentials_Create_ModelContextLimits verifies that
// modelContextLimits AND modelOutputLimits round-trip through create and
// appear in the response.
func TestAdminProviderCredentials_Create_ModelContextLimits(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{
		"name":"limits-test","kind":"openai","slug":"openai","apiKey":"sk-test",
		"baseURL":"https://example.com/v1",
		"modelAllowlist":["glm-5.1","gpt-4o"],
		"modelContextLimits":{"glm-5.1":200000,"gpt-4o":128000},
		"modelOutputLimits":{"glm-5.1":8192,"gpt-4o":16384}
	}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, []string{"glm-5.1", "gpt-4o"}, resp.ModelAllowlist)
	require.Equal(t, 200000, resp.ModelContextLimits["glm-5.1"])
	require.Equal(t, 128000, resp.ModelContextLimits["gpt-4o"])
	require.Equal(t, 8192, resp.ModelOutputLimits["glm-5.1"])
	require.Equal(t, 16384, resp.ModelOutputLimits["gpt-4o"])
}

// TestAdminProviderCredentials_Update_ModelContextLimits verifies that
// modelContextLimits AND modelOutputLimits can be updated independently via PUT.
func TestAdminProviderCredentials_Update_ModelContextLimits(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create first.
	createBody := `{"name":"c1","kind":"openai","slug":"openai","apiKey":"sk-orig","baseURL":"https://x.com/v1"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Update both limit maps.
	updateBody := `{
		"modelAllowlist":["glm-5.2"],
		"modelContextLimits":{"glm-5.2":1000000},
		"modelOutputLimits":{"glm-5.2":32768}
	}`
	req, _ = http.NewRequest("PUT", "/api/v1/admin/provider-credentials/"+created.ID,
		bytes.NewBufferString(updateBody))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var updated CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, []string{"glm-5.2"}, updated.ModelAllowlist)
	assert.Equal(t, 1000000, updated.ModelContextLimits["glm-5.2"])
	assert.Equal(t, 32768, updated.ModelOutputLimits["glm-5.2"])
}

// TestAdminProviderCredentials_ProbeModels_NoBaseURL verifies that the probe
// endpoint returns a graceful warning when the credential has no baseURL rather
// than attempting to probe a nil URL.
func TestAdminProviderCredentials_ProbeModels_NoBaseURL(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 2)
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create a credential without baseURL (native provider).
	createBody := `{"name":"native","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Probe models — must return 200 with a warning, not 500.
	req, _ = http.NewRequest("GET", "/api/v1/admin/provider-credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	assert.NotEmpty(t, probe.Warning, "no-baseURL credential must return a warning")
	assert.Empty(t, probe.Models, "no-baseURL credential must return empty model list")
}

// TestAdminProviderCredentials_ProbeModels_NotFound verifies 404 for unknown ID.
func TestAdminProviderCredentials_ProbeModels_NotFound(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/admin/provider-credentials/does-not-exist/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestAdminProviderCredentials_ProbeModels_WithBaseURL_CallsProvider verifies
// that when a credential has a baseURL, the probe endpoint attempts to contact
// the provider and returns a warning (not 500) when it fails.
func TestAdminProviderCredentials_ProbeModels_WithBaseURL_CallsProvider(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 3)
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create with a baseURL that won't be reachable in tests.
	createBody := `{"name":"custom","kind":"openai_compatible","slug":"custom","apiKey":"sk-test","baseURL":"http://localhost:19999/v1"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Probe — provider unreachable, must return 200 with warning.
	req, _ = http.NewRequest("GET", "/api/v1/admin/provider-credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	assert.NotEmpty(t, probe.Warning, "unreachable provider must return a warning")
}

// TestAdminProviderCredentials_ProbeModels_WithBaseURL_Success verifies that
// when a provider's /models endpoint is reachable and returns a valid list,
// the probe response includes those models with saved context limits merged in.
func TestAdminProviderCredentials_ProbeModels_WithBaseURL_Success(t *testing.T) {
	// Spin up a fake /models server.
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		assert.Equal(t, "Bearer sk-probe-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"},{"id":"glm-5.2"},{"id":"classifier"}]}`))
	}))
	defer fakeProvider.Close()

	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 4)
	}
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create with saved context AND output limits for two of the three models.
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
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Probe — should return all 3 models from the fake provider,
	// with saved context AND output limits pre-populated for glm-5.1 and glm-5.2.
	req, _ = http.NewRequest("GET", "/api/v1/admin/provider-credentials/"+created.ID+"/models", nil)
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
	assert.Equal(t, 0, byID["classifier"].ContextLimit, "classifier has no saved context limit")
	assert.Equal(t, 8192, byID["glm-5.1"].OutputLimit)
	assert.Equal(t, 16384, byID["glm-5.2"].OutputLimit)
	assert.Equal(t, 0, byID["classifier"].OutputLimit, "classifier has no saved output limit")
}

// TestAdminProviderCredentials_Response_NoOrgID verifies that admin credential
// responses do NOT include the orgId field (regression test for C1: the unified
// buildCredentialResponse leaked orgId:"_platform" into admin responses).
func TestAdminProviderCredentials_Response_NoOrgID(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	// Create a credential.
	createBody := `{"name":"test","kind":"openai","slug":"openai","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Assert the raw JSON does NOT contain "orgId".
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, hasOrgID := raw["orgId"]
	assert.False(t, hasOrgID, "admin response must not include orgId")

	// Also check List (which uses buildCredentialResponse).
	req2, _ := http.NewRequest("GET", "/api/v1/admin/provider-credentials", nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)
	var listRaw []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &listRaw))
	require.NotEmpty(t, listRaw)
	_, hasOrgIDList := listRaw[0]["orgId"]
	assert.False(t, hasOrgIDList, "admin List response must not include orgId")
}

// TestAdminProviderCredentials_Create_InvalidKind_400 asserts that a request
// with a kind value not in the enum is rejected at the handler boundary with
// HTTP 400 and a field-specific message — NOT a 500 from a downstream DB
// CHECK violation. The field tag in the response body lets the client
// highlight the offending input. (Epic 55 robustness fix.)
func TestAdminProviderCredentials_Create_InvalidKind_400(t *testing.T) {
	store := newFakeAdminCredStore()
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	// "custom" was the legacy SDK kind for OpenAI-compatible endpoints —
	// it is NOT in the post-Epic-55 enum. The handler must reject it.
	body := `{"name":"x","kind":"custom","slug":"x","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code,
		"invalid kind must surface as 400 from the handler boundary, not 500 from the DB CHECK")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "error", "response must carry an error message")
	assert.Equal(t, "kind", resp["field"], "response must identify the offending field")
}

// TestAdminProviderCredentials_Create_InvalidSlug_400 asserts the same
// boundary contract for slug values that don't match the slug-safe regex.
func TestAdminProviderCredentials_Create_InvalidSlug_400(t *testing.T) {
	store := newFakeAdminCredStore()
	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(make([]byte, 32)))
	router := setupAdminCredRouter(h)

	// Slugs with spaces, slashes, uppercase, or leading/trailing hyphens
	// are all invalid. Test the most likely accidental input: a slug with
	// a space (the original incident's "thekao cloud" credential).
	body := `{"name":"x","kind":"anthropic","slug":"has space","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/admin/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code,
		"invalid slug must surface as 400 from the handler boundary")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "error")
	assert.Equal(t, "slug", resp["field"])
}

// TestAdminProviderCredentials_Update_InvalidKind_400 asserts the validation
// also fires on the partial-update path.
func TestAdminProviderCredentials_Update_InvalidKind_400(t *testing.T) {
	store := newFakeAdminCredStore()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	// Seed an existing valid credential.
	existingPT, _ := json.Marshal(secrets.LLMProviderData{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-existing"})
	existingCT, err := secrets.EncryptSecret(kek, existingPT)
	require.NoError(t, err)
	store.creds["id1"] = &secrets.CredentialRow{
		OwnerType: "admin", OwnerID: "_platform", ID: "id1",
		Name: "n", Kind: "anthropic", Slug: "anthropic",
		Ciphertext: existingCT, KeyVersion: 1,
	}

	h := NewAdminProviderCredentialsHandler(store, mustStaticProv(kek))
	router := setupAdminCredRouter(h)

	body := `{"kind":"custom"}`
	req, _ := http.NewRequest("PUT", "/api/v1/admin/provider-credentials/id1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
