// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeUserCredStore struct {
	creds      map[string]*secrets.CredentialRow
	bindings   map[string][]string // credID -> []wsID
	autoBinds  map[string]bool     // wsID -> true if auto-bound (for protection test)
	nextErr    error
	bindAllErr error
}

func newFakeUserCredStore() *fakeUserCredStore {
	return &fakeUserCredStore{
		creds:     make(map[string]*secrets.CredentialRow),
		bindings:  make(map[string][]string),
		autoBinds: make(map[string]bool),
	}
}

func (f *fakeUserCredStore) CreateCredential(_ context.Context, ownerType, ownerID string, row *secrets.CredentialRow) error {
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

func (f *fakeUserCredStore) ListCredentials(_ context.Context, ownerType, ownerID string) ([]*secrets.CredentialRow, error) {
	var out []*secrets.CredentialRow
	for _, c := range f.creds {
		if c.OwnerType == ownerType && c.OwnerID == ownerID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeUserCredStore) GetCredential(_ context.Context, ownerType, ownerID, id string) (*secrets.CredentialRow, error) {
	c, ok := f.creds[id]
	if !ok || c.OwnerType != ownerType || c.OwnerID != ownerID {
		return nil, nil
	}
	return c, nil
}

func (f *fakeUserCredStore) UpdateCredential(_ context.Context, ownerType, ownerID string, credID string, row *secrets.CredentialRow) error {
	row.ID = credID
	row.OwnerType = ownerType
	row.OwnerID = ownerID
	f.creds[credID] = row
	return nil
}

func (f *fakeUserCredStore) DeleteCredential(_ context.Context, ownerType, ownerID, id string) error {
	c, ok := f.creds[id]
	if !ok || c.OwnerType != ownerType || c.OwnerID != ownerID {
		return pgx.ErrNoRows
	}
	delete(f.creds, id)
	return nil
}

func (f *fakeUserCredStore) BindCredentialToWorkspace(_ context.Context, credID, wsID string) error {
	f.bindings[credID] = append(f.bindings[credID], wsID)
	return nil
}

func (f *fakeUserCredStore) UnbindCredentialFromWorkspace(_ context.Context, credID, wsID string) error {
	if f.autoBinds[wsID] {
		return secrets.ErrAutoBindingProtected
	}
	orig := f.bindings[credID]
	filtered := orig[:0]
	for _, id := range orig {
		if id != wsID {
			filtered = append(filtered, id)
		}
	}
	f.bindings[credID] = filtered
	return nil
}

func (f *fakeUserCredStore) GetCredentialBindings(_ context.Context, credID, _ string) ([]string, error) {
	ids := f.bindings[credID]
	if ids == nil {
		return []string{}, nil
	}
	return ids, nil
}

func (f *fakeUserCredStore) GetCredentialBindingsWithSource(_ context.Context, credID, _ string) ([]secrets.CredentialBindingInfo, error) {
	ids := f.bindings[credID]
	out := make([]secrets.CredentialBindingInfo, len(ids))
	for i, id := range ids {
		sourceType := "explicit"
		if f.autoBinds[id] {
			sourceType = "auto"
		}
		out[i] = secrets.CredentialBindingInfo{WorkspaceID: id, SourceType: sourceType}
	}
	return out, nil
}

func (f *fakeUserCredStore) BindCredentialToAllUserWorkspaces(_ context.Context, credID, _ string) error {
	_ = credID
	if f.bindAllErr != nil {
		return f.bindAllErr
	}
	return nil
}

type fakeKeyStore struct {
	version int
}

func (f *fakeKeyStore) GetUserKey(_ context.Context, _ string) (*secrets.UserKeyRecord, error) {
	return &secrets.UserKeyRecord{KeyVersion: f.version}, nil
}
func (f *fakeKeyStore) CreateUserKey(_ context.Context, _ *secrets.UserKeyRecord) error { return nil }
func (f *fakeKeyStore) UpdateWrappedDEK(_ context.Context, _ string, _ []byte, _ []byte, _ int) error {
	return nil
}
func (f *fakeKeyStore) UpdateWrappedDEKRecovery(_ context.Context, _ string, _ []byte, _ []byte) error {
	return nil
}

func setupUserCredRouter(h *UserProviderCredentialsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "sess-1")
		c.Next()
	})
	g := r.Group("/api/v1/provider-credentials")
	g.POST("", h.Create)
	g.GET("", h.List)
	g.GET("/:id", h.Get)
	g.GET("/:id/models", h.ProbeModels)
	g.DELETE("/:id", h.Delete)
	g.GET("/:id/bindings", h.ListBindings)
	g.POST("/:id/bind/:workspaceId", h.Bind)
	g.DELETE("/:id/bind/:workspaceId", h.Unbind)
	return r
}

// mockCredStateWriter captures MarkCredentialChanged calls for testing.
type mockCredStateWriter struct {
	fn func(ctx context.Context, wsID string) error
}

func (m *mockCredStateWriter) MarkCredentialChanged(ctx context.Context, wsID string) error {
	if m.fn != nil {
		return m.fn(ctx, wsID)
	}
	return nil
}

func TestUserProviderCredentials_Create_Success(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	keys := secrets.NewKeyService(nil, nil) // won't be used directly
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     keys,
		keyStore: &fakeKeyStore{version: 1},
	}
	// Override keys to use our fake DEK getter — inject via a patched KeyService isn't easy,
	// so we test through the handler by mocking at the store level.
	// Actually, the handler calls h.keys.GetDEK which needs a real KeyService.
	// Let's test the full handler with a working KeyService + DEK cache.
	dekCache := &testDEKCacheForHandler{}
	dekCache.cache = map[string][]byte{"sess-1": dek}
	keyService := secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache)
	h.keys = keyService
	h.keyStore = &fakeKeyStore{version: 1}

	router := setupUserCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)
	var resp CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-anthropic", resp.Name)
	assert.Equal(t, "anthropic", resp.Slug)
}

func TestUserProviderCredentials_Create_MissingAPIKey(t *testing.T) {
	store := newFakeUserCredStore()
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUserProviderCredentials_Create_EmptyProvider(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"test","kind":"  ","slug":"  ","apiKey":"key"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUserProviderCredentials_Create_Duplicate(t *testing.T) {
	store := newFakeUserCredStore()
	store.nextErr = &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"test","kind":"anthropic","slug":"anthropic","apiKey":"key"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestUserProviderCredentials_List(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var list []CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Len(t, list, 1)
}

func TestUserProviderCredentials_Get_NotFound(t *testing.T) {
	store := newFakeUserCredStore()
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/nonexistent", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserProviderCredentials_Get_WrongOwner(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "other-user", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserProviderCredentials_Bind_OwnershipCheck(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		wsOwnerCheck: func(_ context.Context, _, _ string) error {
			return errors.New("not owned")
		},
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials/c1/bind/ws-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserProviderCredentials_Bind_CredentialNotOwned(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "other-user", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials/c1/bind/ws-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserProviderCredentials_Bind_Success(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		wsOwnerCheck: func(_ context.Context, _, _ string) error {
			return nil // owned
		},
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials/c1/bind/ws-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, store.bindings["c1"], "ws-1")
}

func TestUserProviderCredentials_Delete(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/provider-credentials/c1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, store.creds)
}

func TestUserProviderCredentials_ListBindings_ReturnsWorkspaceIDs(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	store.bindings["c1"] = []string{"ws-1", "ws-2"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1/bindings", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		WorkspaceIds []string `json:"workspaceIds"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"ws-1", "ws-2"}, resp.WorkspaceIds)
}

// TestUserProviderCredentials_ListBindings_JSONShape_CamelCase is a regression
// test for the CredentialBindingInfo PascalCase serialization bug.
// CredentialBindingInfo had no json struct tags, causing encoding/json to emit
// WorkspaceID/SourceType (PascalCase) instead of workspaceId/sourceType.
// The frontend TypeScript type expects camelCase; PascalCase caused the binding
// panel to show every workspace as "Bind" regardless of actual binding state.
func TestUserProviderCredentials_ListBindings_JSONShape_CamelCase(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	store.bindings["c1"] = []string{"ws-explicit"}
	// ws-auto is in autoBinds so GetCredentialBindingsWithSource returns sourceType="auto"
	store.autoBinds["ws-auto"] = true
	store.bindings["c1"] = append(store.bindings["c1"], "ws-auto")
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1/bindings", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Parse as raw map to verify exact JSON key names.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))

	// Flat array key must be camelCase "workspaceIds", not "WorkspaceIds".
	assert.Contains(t, raw, "workspaceIds", "flat array key must be camelCase workspaceIds")
	assert.NotContains(t, raw, "WorkspaceIds", "PascalCase WorkspaceIds must not appear in response")

	// Bindings array key must be "bindings".
	assert.Contains(t, raw, "bindings", "bindings key must be present")

	// Each binding object must use camelCase keys.
	var bindings []json.RawMessage
	require.NoError(t, json.Unmarshal(raw["bindings"], &bindings))
	require.NotEmpty(t, bindings)

	for _, b := range bindings {
		var bindingMap map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &bindingMap))
		assert.Contains(t, bindingMap, "workspaceId", "binding key must be camelCase workspaceId")
		assert.Contains(t, bindingMap, "sourceType", "binding key must be camelCase sourceType")
		assert.NotContains(t, bindingMap, "WorkspaceID", "PascalCase WorkspaceID must not appear")
		assert.NotContains(t, bindingMap, "SourceType", "PascalCase SourceType must not appear")
	}

	// Verify sourceType values are correct.
	bindingByWs := map[string]string{}
	for _, b := range bindings {
		var bindingObj struct {
			WorkspaceId string `json:"workspaceId"`
			SourceType  string `json:"sourceType"`
		}
		require.NoError(t, json.Unmarshal(b, &bindingObj))
		bindingByWs[bindingObj.WorkspaceId] = bindingObj.SourceType
	}
	assert.Equal(t, "explicit", bindingByWs["ws-explicit"])
	assert.Equal(t, "auto", bindingByWs["ws-auto"])
}

func TestUserProviderCredentials_ListBindings_EmptyWhenNoneBound(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1/bindings", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		WorkspaceIds []string `json:"workspaceIds"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.WorkspaceIds)
}

func TestUserProviderCredentials_ListBindings_NotFoundForWrongOwner(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "other-user", Name: "test", Kind: "openai", Slug: "openai"}
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1/bindings", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUserProviderCredentials_Unbind_RemovesBinding(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	store.bindings["c1"] = []string{"ws-1", "ws-2"}
	h := &UserProviderCredentialsHandler{
		store:        store,
		bindings:     store,
		wsOwnerCheck: func(_ context.Context, _, _ string) error { return nil },
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/provider-credentials/c1/bind/ws-1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.NotContains(t, store.bindings["c1"], "ws-1")
	assert.Contains(t, store.bindings["c1"], "ws-2")
}

// TestUserProviderCredentials_Unbind_RejectsAutoBinding verifies H-1 fix:
// auto-bindings (seeded by SeedWorkspaceCredentials) return 409, not 204.
func TestUserProviderCredentials_Unbind_RejectsAutoBinding(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	store.bindings["c1"] = []string{"ws-auto"}
	store.autoBinds["ws-auto"] = true // simulate auto-bound
	h := &UserProviderCredentialsHandler{
		store:        store,
		bindings:     store,
		wsOwnerCheck: func(_ context.Context, _, _ string) error { return nil },
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/provider-credentials/c1/bind/ws-auto", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestUserProviderCredentials_Delete_NotifiesBoundWorkspaces verifies C-3 fix:
// deleting a credential marks all previously-bound workspaces as credential-changed.
func TestUserProviderCredentials_Delete_NotifiesBoundWorkspaces(t *testing.T) {
	store := newFakeUserCredStore()
	store.creds["c1"] = &secrets.CredentialRow{ID: "c1", OwnerType: "user", OwnerID: "user-1", Name: "test", Kind: "openai", Slug: "openai"}
	store.bindings["c1"] = []string{"ws-1", "ws-2"}

	notified := make(map[string]bool)
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		credStateWriter: &mockCredStateWriter{fn: func(ctx context.Context, wsID string) error {
			notified[wsID] = true
			return nil
		}},
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/provider-credentials/c1", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.True(t, notified["ws-1"], "ws-1 should be notified")
	assert.True(t, notified["ws-2"], "ws-2 should be notified")
}

// TestUserProviderCredentials_Create_Returns207OnBindFailure verifies C-2 fix:
// if BindCredentialToAllUserWorkspaces fails, Create returns 207 not 201.
func TestUserProviderCredentials_Create_Returns207OnBindFailure(t *testing.T) {
	store := newFakeUserCredStore()
	store.bindAllErr = errors.New("db timeout")
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"my-openai","kind":"openai","slug":"openai","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusMultiStatus, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp, "bindWarning")
	assert.Contains(t, resp, "credential")
}

// testDEKCacheForHandler is a minimal DEKCache for handler tests.
type testDEKCacheForHandler struct {
	cache map[string][]byte
}

func (c *testDEKCacheForHandler) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	if c.cache == nil {
		c.cache = make(map[string][]byte)
	}
	c.cache[sessionID] = dek
	return nil
}

func (c *testDEKCacheForHandler) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	dek, ok := c.cache[sessionID]
	if !ok {
		return nil, secrets.ErrDEKUnavailable
	}
	return dek, nil
}

func (c *testDEKCacheForHandler) EvictDEK(_ context.Context, _ string) error { return nil }

// setupUserCredRouterUnauthenticated builds the credential routes without the
// auth middleware that injects userID/sessionID, simulating an unauthenticated
// request. Every handler must return 401 rather than touching the store.
func setupUserCredRouterUnauthenticated(h *UserProviderCredentialsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1/provider-credentials")
	g.POST("", h.Create)
	g.GET("", h.List)
	g.GET("/:id", h.Get)
	g.DELETE("/:id", h.Delete)
	g.GET("/:id/bindings", h.ListBindings)
	g.POST("/:id/bind/:workspaceId", h.Bind)
	g.DELETE("/:id/bind/:workspaceId", h.Unbind)
	return r
}

// TestUserProviderCredentials_AuthGuards_Return401 verifies that every endpoint
// enforces authentication and returns 401 (never reaching the store) when the
// caller has no authenticated session. Guards against a regression that drops
// the extractAuth guard from any handler.
func TestUserProviderCredentials_AuthGuards_Return401(t *testing.T) {
	store := newFakeUserCredStore()
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	router := setupUserCredRouterUnauthenticated(h)

	createBody := bytes.NewBufferString(`{"kind":"anthropic","slug":"anthropic","apiKey":"k"}`)
	cases := []struct {
		name   string
		method string
		path   string
		body   *bytes.Buffer
	}{
		{"Create", http.MethodPost, "/api/v1/provider-credentials", createBody},
		{"List", http.MethodGet, "/api/v1/provider-credentials", nil},
		{"Get", http.MethodGet, "/api/v1/provider-credentials/c1", nil},
		{"Delete", http.MethodDelete, "/api/v1/provider-credentials/c1", nil},
		{"ListBindings", http.MethodGet, "/api/v1/provider-credentials/c1/bindings", nil},
		{"Bind", http.MethodPost, "/api/v1/provider-credentials/c1/bind/ws-1", nil},
		{"Unbind", http.MethodDelete, "/api/v1/provider-credentials/c1/bind/ws-1", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *bytes.Buffer
			if tc.body != nil {
				body = tc.body
			} else {
				body = bytes.NewBuffer(nil)
			}
			req, _ := http.NewRequest(tc.method, tc.path, body)
			if tc.body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code, "%s must return 401 when unauthenticated", tc.name)
		})
	}
}

// TestUserProviderCredentials_Create_RequiresSessionID verifies that Create in
// particular rejects requests that carry a userID but no sessionID (the other
// handlers only require userID, so this guards the Create-specific check).
func TestUserProviderCredentials_Create_RequiresSessionID(t *testing.T) {
	store := newFakeUserCredStore()
	h := &UserProviderCredentialsHandler{store: store, bindings: store}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1") // userID present, sessionID absent
		c.Next()
	})
	r.POST("/api/v1/provider-credentials", h.Create)

	body := `{"kind":"anthropic","slug":"anthropic","apiKey":"k"}`
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, store.creds, "no credential should be persisted without a session")
}

// TestUserProviderCredentials_Create_DEKUnavailable_ReturnsActionableError
// is the regression test for issue #593 Option C: when GetDEK returns
// ErrDEKUnavailable (typical for API-key auth without decrypt_access),
// the response must carry an actionable message pointing the caller at
// the two recovery paths — a password-authenticated session OR an API
// key created with decrypt_access=true. The previous response was an
// opaque {"error":"encryption unavailable"} that gave the caller no way
// to know what to do next.
func TestUserProviderCredentials_Create_DEKUnavailable_ReturnsActionableError(t *testing.T) {
	store := newFakeUserCredStore()
	// Empty cache: GetDEK("sess-1") misses and falls through to
	// rehydrate, which returns ErrDEKUnavailable because no JWT
	// session store is wired.
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"my-anthropic","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-123"}`
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"DEK-unavailable is an auth/permission condition (403), not a service outage (503)")
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	msg, ok := resp["error"]
	require.True(t, ok, "response must include an error field")
	assert.Contains(t, msg, "decryptAccess",
		"error must point the caller at the decryptAccess=true API-key path (matches the CreateAPIKeyRequest JSON field)")
	assert.Contains(t, msg, "password",
		"error must point the caller at the password-session path")
	assert.NotEqual(t, "encryption unavailable", msg,
		"the opaque pre-fix message must be replaced with actionable guidance")
	assert.Empty(t, store.creds, "no credential should be persisted when the DEK is unavailable")
}

// TestUserProviderCredentials_ProbeModels_NotFound verifies 404 for unknown credential.
func TestUserProviderCredentials_ProbeModels_NotFound(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/does-not-exist/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestUserProviderCredentials_ProbeModels_DEKUnavailable_ReturnsActionableError
// extends the issue #593 Option C fix to the probe endpoint. Without this,
// an API-key-only caller hitting GET /provider-credentials/:id/models sees
// the same opaque {"error":"encryption unavailable"} as the Create endpoint
// had pre-fix — same root cause, same recovery path, same fix.
func TestUserProviderCredentials_ProbeModels_DEKUnavailable_ReturnsActionableError(t *testing.T) {
	store := newFakeUserCredStore()
	// Stash a credential row so we get past the 404 and into the decrypt path.
	store.creds["c1"] = &secrets.CredentialRow{
		ID:         "c1",
		OwnerType:  "user",
		OwnerID:    "user-1",
		Kind:       "openai",
		Slug:       "openai",
		Ciphertext: []byte("anything"),
	}
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{}} // empty → DEK unavailable
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("GET", "/api/v1/provider-credentials/c1/models", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"DEK-unavailable on probe must be 403, matching the Create endpoint fix")
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["error"], "decryptAccess",
		"probe error must point at the decryptAccess=true path")
	assert.NotEqual(t, "encryption unavailable", resp["error"])
}

// TestUserProviderCredentials_ProbeModels_NoBaseURL verifies graceful warning
// when the credential has no baseURL (native provider).
func TestUserProviderCredentials_ProbeModels_NoBaseURL(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i + 5)
	}
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	// Create a credential without baseURL.
	body := `{"name":"native","kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	// Probe models — must return 200 with warning, not 500.
	req, _ = http.NewRequest("GET", "/api/v1/provider-credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	assert.NotEmpty(t, probe.Warning, "no-baseURL credential must return a warning")
	assert.Empty(t, probe.Models)
}

// TestUserProviderCredentials_ProbeModels_WithBaseURL_Success verifies that
// when a user credential has a baseURL, the probe endpoint fetches models
// from the provider and merges saved context limits.
func TestUserProviderCredentials_ProbeModels_WithBaseURL_Success(t *testing.T) {
	fakeProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.1"},{"id":"glm-5.2"}]}`))
	}))
	defer fakeProvider.Close()

	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i + 6)
	}
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache),
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	createBody, _ := json.Marshal(map[string]interface{}{
		"name":               "thekao",
		"kind":               "openai_compatible",
		"slug":               "thekao-cloud",
		"apiKey":             "sk-probe-key",
		"baseURL":            fakeProvider.URL + "/v1",
		"modelAllowlist":     []string{"glm-5.1"},
		"modelContextLimits": map[string]int{"glm-5.1": 200000},
	})
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var created CredentialResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	req, _ = http.NewRequest("GET", "/api/v1/provider-credentials/"+created.ID+"/models", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var probe ProbeModelsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &probe))
	assert.Empty(t, probe.Warning)
	require.Len(t, probe.Models, 2)
	byID := map[string]ProbeModelEntry{}
	for _, m := range probe.Models {
		byID[m.ID] = m
	}
	assert.Equal(t, 200000, byID["glm-5.1"].ContextLimit, "saved context limit must be populated")
	assert.Equal(t, 0, byID["glm-5.2"].ContextLimit, "unsaved model has no context limit")
}

// TestUserProviderCredentials_Delete_NotFound_Returns204 verifies that deleting
// a non-existent credential returns 204 (idempotent), not 500. Regression test
// for C3: the unified DeleteCredential returns pgx.ErrNoRows on 0 rows; the
// user delete handler must treat this as success to preserve the old behavior.
func TestUserProviderCredentials_Delete_NotFound_Returns204(t *testing.T) {
	store := newFakeUserCredStore()
	// Seed nothing — the credential doesn't exist.
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
	}
	router := setupUserCredRouter(h)

	req, _ := http.NewRequest("DELETE", "/api/v1/provider-credentials/does-not-exist", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code, "deleting a missing credential must be idempotent (204)")
}

// TestUserProviderCredentials_Create_InvalidKind_400 — boundary validation
// for the user handler. See AdminProviderCredentials_Create_InvalidKind_400
// for rationale (Epic 55 robustness fix).
func TestUserProviderCredentials_Create_InvalidKind_400(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	keyService := secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache)
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     keyService,
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"x","kind":"custom","slug":"x","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code,
		"invalid kind must surface as 400 from the handler boundary, not 500 from the DB CHECK")
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "kind", resp["field"])
}

// TestUserProviderCredentials_Create_InvalidSlug_400 — boundary validation
// for slug.
func TestUserProviderCredentials_Create_InvalidSlug_400(t *testing.T) {
	store := newFakeUserCredStore()
	dek := make([]byte, 32)
	dekCache := &testDEKCacheForHandler{cache: map[string][]byte{"sess-1": dek}}
	keyService := secrets.NewKeyService(&fakeKeyStore{version: 1}, dekCache)
	h := &UserProviderCredentialsHandler{
		store:    store,
		bindings: store,
		keys:     keyService,
		keyStore: &fakeKeyStore{version: 1},
	}
	router := setupUserCredRouter(h)

	body := `{"name":"x","kind":"anthropic","slug":"has space","apiKey":"sk-test"}`
	req, _ := http.NewRequest("POST", "/api/v1/provider-credentials", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "slug", resp["field"])
}
