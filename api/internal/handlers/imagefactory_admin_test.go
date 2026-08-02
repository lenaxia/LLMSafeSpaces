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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

type fakeAdminStore struct {
	platformCfg imagefactory.PlatformConfig
	bases       []imagefactory.Base
	extensions  []imagefactory.Extension
	failures    []imagefactory.KnownFailure
	err         error
}

func (f *fakeAdminStore) GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error) {
	return f.platformCfg, f.err
}
func (f *fakeAdminStore) SetPlatformConfig(ctx context.Context, pc imagefactory.PlatformConfig) error {
	f.platformCfg = pc
	return f.err
}
func (f *fakeAdminStore) ListBases(ctx context.Context) ([]imagefactory.Base, error) {
	return f.bases, f.err
}
func (f *fakeAdminStore) UpsertBase(ctx context.Context, b imagefactory.Base) error {
	f.bases = append(f.bases, b)
	return f.err
}
func (f *fakeAdminStore) DeleteBase(ctx context.Context, name, version string) error {
	for i, b := range f.bases {
		if b.Name == name && b.Version == version {
			f.bases = append(f.bases[:i], f.bases[i+1:]...)
			return nil
		}
	}
	return database.ErrNotFound
}
func (f *fakeAdminStore) ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error) {
	return f.extensions, f.err
}
func (f *fakeAdminStore) PublishExtension(ctx context.Context, e imagefactory.Extension) error {
	f.extensions = append(f.extensions, e)
	return f.err
}
func (f *fakeAdminStore) RetireExtension(ctx context.Context, id string) error {
	for i, e := range f.extensions {
		if e.ID == id {
			f.extensions[i].Retired = true
			return nil
		}
	}
	return database.ErrNotFound
}
func (f *fakeAdminStore) ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error) {
	return f.failures, f.err
}
func (f *fakeAdminStore) SetKnownFailureRetriable(ctx context.Context, hash, baseName string, retriable bool) error {
	for i, kf := range f.failures {
		if kf.SelectionHash == hash && kf.BaseName == baseName {
			f.failures[i].Retriable = retriable
			return nil
		}
	}
	return database.ErrNotFound
}
func (f *fakeAdminStore) DeleteKnownFailure(ctx context.Context, hash, baseName string) error {
	for i, kf := range f.failures {
		if kf.SelectionHash == hash && kf.BaseName == baseName {
			f.failures = append(f.failures[:i], f.failures[i+1:]...)
			return nil
		}
	}
	return database.ErrNotFound
}

func newAdminRouter(t *testing.T, store adminStore) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := NewImageFactoryAdminHandler(store)
	g := r.Group("/api/v1/admin/image-factory")
	g.GET("/platform-config", h.GetPlatformConfig)
	g.PUT("/platform-config", h.SetPlatformConfig)
	g.GET("/bases", h.ListBases)
	g.POST("/bases", h.UpsertBase)
	g.DELETE("/bases/:name/:version", h.DeleteBase)
	g.GET("/extensions", h.ListExtensions)
	g.POST("/extensions", h.PublishExtension)
	g.DELETE("/extensions/:id", h.RetireExtension)
	g.GET("/known-failures", h.ListKnownFailures)
	g.PUT("/known-failures/:hash/:baseName", h.SetKnownFailureRetriable)
	g.DELETE("/known-failures/:hash/:baseName", h.DeleteKnownFailure)
	return r
}

func adminJSON(t *testing.T, r http.Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdmin_PlatformConfig_Get(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{platformCfg: imagefactory.PlatformConfig{Architectures: []string{"linux/amd64"}}}
	r := newAdminRouter(t, store)
	w := adminJSON(t, r, "GET", "/api/v1/admin/image-factory/platform-config", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var pc imagefactory.PlatformConfig
	json.Unmarshal(w.Body.Bytes(), &pc)
	assert.Equal(t, []string{"linux/amd64"}, pc.Architectures)
}

func TestAdmin_PlatformConfig_Set(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{}
	r := newAdminRouter(t, store)
	w := adminJSON(t, r, "PUT", "/api/v1/admin/image-factory/platform-config", setPlatformConfigRequest{
		Architectures: []string{"linux/amd64", "linux/arm64"},
	})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, store.platformCfg.Architectures)
}

func TestAdmin_Bases_CRUD(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{}
	r := newAdminRouter(t, store)

	// Create
	w := adminJSON(t, r, "POST", "/api/v1/admin/image-factory/bases", upsertBaseRequest{
		Name: "bookworm", Version: "0.6.0", Image: "img", Tag: "0.6.0",
	})
	require.Equal(t, http.StatusCreated, w.Code)
	require.Len(t, store.bases, 1)

	// List
	w = adminJSON(t, r, "GET", "/api/v1/admin/image-factory/bases", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Delete
	w = adminJSON(t, r, "DELETE", "/api/v1/admin/image-factory/bases/bookworm/0.6.0", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, store.bases)

	// Delete again → 404
	w = adminJSON(t, r, "DELETE", "/api/v1/admin/image-factory/bases/bookworm/0.6.0", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdmin_Extensions_PublishRetire(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{}
	r := newAdminRouter(t, store)

	// Publish
	w := adminJSON(t, r, "POST", "/api/v1/admin/image-factory/extensions", publishExtensionRequest{
		ID: "ffmpeg", Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg",
		SupportedBases: []string{"bookworm"},
	})
	require.Equal(t, http.StatusCreated, w.Code)

	// List (include retired)
	w = adminJSON(t, r, "GET", "/api/v1/admin/image-factory/extensions", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Retire
	w = adminJSON(t, r, "DELETE", "/api/v1/admin/image-factory/extensions/ffmpeg", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, store.extensions[0].Retired)

	// Retire again → 404
	w = adminJSON(t, r, "DELETE", "/api/v1/admin/image-factory/extensions/ghost", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdmin_KnownFailures_ListToggleClear(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{
		failures: []imagefactory.KnownFailure{
			{SelectionHash: "s-x", BaseName: "bookworm", Retriable: true},
		},
	}
	r := newAdminRouter(t, store)

	// List
	w := adminJSON(t, r, "GET", "/api/v1/admin/image-factory/known-failures", nil)
	require.Equal(t, http.StatusOK, w.Code)

	// Toggle retriable
	w = adminJSON(t, r, "PUT", "/api/v1/admin/image-factory/known-failures/s-x/bookworm", setRetriableRequest{Retriable: false})
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, store.failures[0].Retriable)

	// Clear
	w = adminJSON(t, r, "DELETE", "/api/v1/admin/image-factory/known-failures/s-x/bookworm", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, store.failures)

	// Toggle again → 404
	w = adminJSON(t, r, "PUT", "/api/v1/admin/image-factory/known-failures/s-x/bookworm", setRetriableRequest{Retriable: true})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdmin_StoreError500(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{err: testError("db down")}
	r := newAdminRouter(t, store)
	w := adminJSON(t, r, "GET", "/api/v1/admin/image-factory/bases", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAdmin_NonAdminReturns404(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Simulate AdminGuard: non-admin gets 404 (not 403, to hide route).
	r.Use(func(c *gin.Context) {
		role := c.GetHeader("X-Test-Role")
		if role != "admin" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	})
	h := NewImageFactoryAdminHandler(&fakeAdminStore{})
	r.GET("/api/v1/admin/image-factory/bases", h.ListBases)

	// Non-admin → 404
	req := httptest.NewRequest("GET", "/api/v1/admin/image-factory/bases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Admin → 200
	req2 := httptest.NewRequest("GET", "/api/v1/admin/image-factory/bases", nil)
	req2.Header.Set("X-Test-Role", "admin")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestAdmin_PublishExtension_InvalidType422(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{}
	r := newAdminRouter(t, store)
	w := adminJSON(t, r, "POST", "/api/v1/admin/image-factory/extensions", publishExtensionRequest{
		ID: "bad", Type: "run", Value: "rm -rf /", SupportedBases: []string{"bookworm"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "type=run must be rejected")
}

func TestAdmin_PublishExtension_FileWithoutSpec422(t *testing.T) {
	t.Parallel()
	store := &fakeAdminStore{}
	r := newAdminRouter(t, store)
	w := adminJSON(t, r, "POST", "/api/v1/admin/image-factory/extensions", publishExtensionRequest{
		ID: "bad", Type: imagefactory.ExtensionTypeFile, Value: "x", SupportedBases: []string{"bookworm"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "file without fileSpec must be rejected")
}
