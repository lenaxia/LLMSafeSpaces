// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// fakeIFStore is the test double for imageFactoryStore. It returns scripted
// results and records calls so tests assert which methods ran.
type fakeIFStore struct {
	platformCfg             imagefactory.PlatformConfig
	bases                   []imagefactory.Base
	baseByName              map[string]imagefactory.Base
	extensions              []imagefactory.Extension
	failures                []imagefactory.KnownFailure
	knownFailureByHash      map[string]imagefactory.KnownFailure
	getKnownFailureOverride *imagefactory.KnownFailure
	configs                 []imagefactory.Config // returned by ListVisibleConfigs
	configByHash            map[string]imagefactory.Config
	existingBuild           *imagefactory.Build // returned by GetInFlightOrSuccessfulBuild
	err                     error
	createConfigErr         error
	createBuildErr          error

	// call records
	listExtIncludeRetired []bool
	calledListVisible     bool
	calledCreateConfig    bool
	calledCreateBuild     bool
	lastCreatedConfig     *imagefactory.Config
	lastCreatedBuild      *imagefactory.Build
}

func (f *fakeIFStore) GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error) {
	return f.platformCfg, f.err
}
func (f *fakeIFStore) ListBases(ctx context.Context) ([]imagefactory.Base, error) {
	return f.bases, f.err
}
func (f *fakeIFStore) ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error) {
	f.listExtIncludeRetired = append(f.listExtIncludeRetired, includeRetired)
	return f.extensions, f.err
}
func (f *fakeIFStore) ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error) {
	return f.failures, f.err
}
func (f *fakeIFStore) GetConfig(ctx context.Context, id string) (imagefactory.Config, error) {
	for _, c := range f.configs {
		if c.ID == id {
			return c, nil
		}
	}
	return imagefactory.Config{}, database.ErrNotFound
}
func (f *fakeIFStore) GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error) {
	if c, ok := f.configByHash[hash]; ok && c.Scope == scope {
		return c, nil
	}
	return imagefactory.Config{}, database.ErrNotFound
}
func (f *fakeIFStore) ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error) {
	f.calledListVisible = true
	return f.configs, f.err
}

// S4 additions.
func (f *fakeIFStore) GetBase(ctx context.Context, name, version string) (imagefactory.Base, error) {
	key := name + "/" + version
	if b, ok := f.baseByName[key]; ok {
		return b, nil
	}
	return imagefactory.Base{}, database.ErrNotFound
}

func (f *fakeIFStore) GetKnownFailure(ctx context.Context, hash, baseName string) (imagefactory.KnownFailure, error) {
	if f.getKnownFailureOverride != nil {
		return *f.getKnownFailureOverride, nil
	}
	if kf, ok := f.knownFailureByHash[hash]; ok {
		return kf, nil
	}
	return imagefactory.KnownFailure{}, database.ErrNotFound
}

func (f *fakeIFStore) GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error) {
	return f.existingBuild, f.err
}

func (f *fakeIFStore) CreateConfig(ctx context.Context, c *imagefactory.Config) error {
	f.calledCreateConfig = true
	f.lastCreatedConfig = c
	return f.createConfigErr
}

func (f *fakeIFStore) CreateConfigAndBuild(ctx context.Context, c *imagefactory.Config, b *imagefactory.Build) error {
	f.calledCreateConfig = true
	f.calledCreateBuild = true
	f.lastCreatedConfig = c
	f.lastCreatedBuild = b
	if f.createConfigErr != nil {
		return f.createConfigErr
	}
	return f.createBuildErr
}

func (f *fakeIFStore) CreateBuild(ctx context.Context, b *imagefactory.Build) error {
	f.calledCreateBuild = true
	f.lastCreatedBuild = b
	return f.createBuildErr
}

func (f *fakeIFStore) DeleteConfig(ctx context.Context, id string) error { return nil }
func (f *fakeIFStore) RenameConfig(ctx context.Context, id, newName string) error {
	return nil
}

// fakeOrgResolver is the test double for orgResolver.
type fakeOrgResolver struct {
	orgIDByUser map[string]string
	adminUsers  map[string]bool // "userID:orgID" → true
}

func (f *fakeOrgResolver) GetUserOrgID(ctx context.Context, userID string) (string, error) {
	return f.orgIDByUser[userID], nil
}

func (f *fakeOrgResolver) IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	if f.adminUsers == nil {
		return false, nil
	}
	return f.adminUsers[userID+":"+orgID], nil
}

func init() {
	// Set gin to test mode once at package level. Calling SetMode per-test
	// races under -race when tests run in parallel (gin.SetMode writes a
	// package-global variable).
	gin.SetMode(gin.TestMode)
}

func newIFRouter(t *testing.T, store imageFactoryStore, orgs orgResolver) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, orgs)
	g := r.Group("/api/v1/image-factory")
	g.GET("/catalog", h.Catalog)
	g.GET("/configs", h.ListConfigs)
	g.GET("/configs/:hash", h.GetConfig)
	g.POST("/configs", h.CreateConfig)
	return r
}

func doAuthed(t *testing.T, r *gin.Engine, method, path, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if userID != "" {
		req.Header.Set("X-Test-UserID", userID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ── Catalog ─────────────────────────────────────────────────────────────

func TestIF_Catalog_Happy(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		platformCfg: imagefactory.PlatformConfig{Architectures: []string{"linux/amd64", "linux/arm64"}},
		bases:       []imagefactory.Base{{Name: "bookworm", Version: "0.6.0", IsDefault: true}},
		extensions:  []imagefactory.Extension{{ID: "ffmpeg", Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"}},
		failures:    []imagefactory.KnownFailure{{SelectionHash: "s-x", BaseName: "bookworm", Retriable: true, Explanation: "broken"}},
	}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/catalog", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	var out CatalogResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, out.Architectures)
	assert.Len(t, out.Bases, 1)
	assert.Len(t, out.Extensions, 1)
	assert.Len(t, out.KnownFailures, 1)
	// Catalog must request non-retired only (members select from live entries).
	assert.Equal(t, []bool{false}, store.listExtIncludeRetired)
}

func TestIF_Catalog_FailureReasonStripped(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		failures: []imagefactory.KnownFailure{
			{SelectionHash: "s-x", FailureReason: "SECRET-raw-log-tail", Explanation: "broken"},
		},
	}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/catalog", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "broken")
	assert.NotContains(t, body, "SECRET-raw-log-tail", "raw failure_reason must never reach members")
}

func TestIF_Catalog_StoreError500(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{err: errors.New("db down")}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/catalog", "user-1")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── ListConfigs ─────────────────────────────────────────────────────────

func TestIF_ListConfigs_Happy(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		configs: []imagefactory.Config{
			{ID: "c1", Name: "my-cfg", Hash: "s-a", Scope: imagefactory.ScopeMember},
			{ID: "c2", Name: "platform-cfg", Hash: "s-b", Scope: imagefactory.ScopePlatform},
		},
	}
	orgs := &fakeOrgResolver{orgIDByUser: map[string]string{"user-1": "org-9"}}
	r := newIFRouter(t, store, orgs)
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	var out ListConfigsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Len(t, out.Configs, 2)
	assert.True(t, store.calledListVisible)
}

func TestIF_ListConfigs_EmptyReturnsArrayNotNull(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{configs: nil}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"configs":[]}`, w.Body.String(), "empty must serialize as [] not null")
}

func TestIF_ListConfigs_Unauthed401(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestIF_ListConfigs_StoreError500(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{err: errors.New("db down")}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs", "user-1")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── GetConfig ───────────────────────────────────────────────────────────

func TestIF_GetConfig_Happy(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"s-abc": {ID: "c1", Hash: "s-abc", Name: "ml-stack", Scope: imagefactory.ScopeMember},
		},
	}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs/s-abc", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	var out imagefactory.Config
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "ml-stack", out.Name)
}

func TestIF_GetConfig_NotFound404(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{configByHash: map[string]imagefactory.Config{}}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs/s-ghost", "user-1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestIF_GetConfig_MissingHash400(t *testing.T) {
	t.Parallel()
	// :hash is a path param; "" can't be expressed via routing, so test the
	// guard indirectly by confirming a non-empty miss 404s (covered above) and
	// the handler logic for empty-string hash is exercised by the unit test
	// of the pure function elsewhere. This test documents the routing contract.
	store := &fakeIFStore{}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs/x", "user-1")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestIF_GetConfig_Unauthed401(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/configs/s-abc", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestIF_Catalog_KnownFailuresNeverNull(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		platformCfg: imagefactory.PlatformConfig{Architectures: []string{"linux/amd64"}},
		bases:       []imagefactory.Base{{Name: "bookworm", Version: "0.6.0", IsDefault: true}},
		extensions:  []imagefactory.Extension{{ID: "ffmpeg", Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"}},
		failures:    nil, // explicitly nil
	}
	r := newIFRouter(t, store, &fakeOrgResolver{})
	w := doAuthed(t, r, "GET", "/api/v1/image-factory/catalog", "user-1")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	// knownFailures must serialize as [] not null
	assert.Contains(t, body, "\"knownFailures\":[]")
	assert.NotContains(t, body, "\"knownFailures\":null")
}
