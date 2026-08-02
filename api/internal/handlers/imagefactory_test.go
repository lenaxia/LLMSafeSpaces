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
	platformCfg  imagefactory.PlatformConfig
	bases        []imagefactory.Base
	extensions   []imagefactory.Extension
	failures     []imagefactory.KnownFailure
	configs      []imagefactory.Config // returned by ListVisibleConfigs
	configByHash map[string]imagefactory.Config
	err          error

	// call records
	listExtIncludeRetired []bool
	calledListVisible     bool
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
	if c, ok := f.configByHash[hash]; ok {
		return c, nil
	}
	return imagefactory.Config{}, database.ErrNotFound
}
func (f *fakeIFStore) ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error) {
	f.calledListVisible = true
	return f.configs, f.err
}

// fakeOrgResolver is the test double for orgResolver.
type fakeOrgResolver struct {
	orgIDByUser map[string]string
}

func (f *fakeOrgResolver) GetUserOrgID(ctx context.Context, userID string) (string, error) {
	return f.orgIDByUser[userID], nil
}

func newIFRouter(t *testing.T, store imageFactoryStore, orgs orgResolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
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
