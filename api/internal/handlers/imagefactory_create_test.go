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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// fakeDispatcher scripts the Dispatch outcome. Records calls so tests
// assert whether dispatch was invoked.
type fakeDispatcher struct {
	ghRunID int64
	err     error
	called  bool
	lastReq dispatchRequest
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req dispatchRequest) (int64, error) {
	f.called = true
	f.lastReq = req
	return f.ghRunID, f.err
}

func s4Store() *fakeIFStore {
	b := imagefactory.Base{Name: "bookworm", Version: "0.6.0", Image: "img", Tag: "0.6.0", IsDefault: true}
	return &fakeIFStore{
		platformCfg: imagefactory.PlatformConfig{Architectures: []string{"linux/amd64"}},
		bases:       []imagefactory.Base{b},
		baseByName:  map[string]imagefactory.Base{"bookworm/0.6.0": b},
		extensions: []imagefactory.Extension{
			{ID: "ffmpeg", Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg", SupportedBases: []string{"bookworm"}},
		},
		knownFailureByHash: map[string]imagefactory.KnownFailure{},
	}
}

func s4Body(name string, sel ...string) []byte {
	b, _ := json.Marshal(createConfigRequest{
		Name:      name,
		Selection: sel,
		BaseName:  "bookworm",
	})
	return b
}

func postConfigs(t *testing.T, r http.Handler, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIF_CreateConfig_NovelDispatches(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("novel-cfg", "ffmpeg"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.True(t, disp.called, "dispatch must be called for a novel build")
	assert.True(t, store.calledCreateConfig, "config must be committed")
	assert.True(t, store.calledCreateBuild, "build must be committed")
	assert.Equal(t, imagefactory.StatusBuilding, store.lastCreatedConfig.Status)
	assert.Equal(t, int64(999), *store.lastCreatedBuild.GHRunID)
}

func TestIF_CreateConfig_CoalesceOnSucceeded(t *testing.T) {
	t.Parallel()
	store := s4Store()
	store.existingBuild = &imagefactory.Build{Status: imagefactory.BuildSucceeded}
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("coalesce-cfg", "ffmpeg"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.False(t, disp.called, "dispatch must NOT be called when a succeeded build exists")
	assert.True(t, store.calledCreateConfig)
	assert.False(t, store.calledCreateBuild, "no new build row when coalescing")
	assert.Equal(t, imagefactory.StatusReady, store.lastCreatedConfig.Status,
		"coalescing onto a succeeded build → config is ready")
}

func TestIF_CreateConfig_CoalesceOnInFlight(t *testing.T) {
	t.Parallel()
	store := s4Store()
	store.existingBuild = &imagefactory.Build{Status: imagefactory.BuildDispatched}
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("coalesce-cfg", "ffmpeg"))
	require.Equal(t, http.StatusCreated, w.Code)
	assert.False(t, disp.called, "dispatch must NOT be called when an in-flight build exists")
	assert.True(t, store.calledCreateConfig)
	assert.False(t, store.calledCreateBuild)
	assert.Equal(t, imagefactory.StatusBuilding, store.lastCreatedConfig.Status)
}

func TestIF_CreateConfig_DispatchFailureNoCommit(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{err: errors.New("GH Actions down")}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("fail-cfg", "ffmpeg"))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.True(t, disp.called, "dispatch must be attempted")
	assert.False(t, store.calledCreateConfig, "config must NOT be committed on dispatch failure")
	assert.False(t, store.calledCreateBuild, "build must NOT be committed on dispatch failure")
}

func TestIF_CreateConfig_KnownFailureNotRetriable(t *testing.T) {
	t.Parallel()
	store := s4Store()
	store.knownFailureByHash = map[string]imagefactory.KnownFailure{
		// We don't know the exact hash, but we can pre-seed a known failure
		// that matches. Since the fake returns by-hash, we need to compute
		// the hash the handler will compute. But the handler computes it
		// internally. Easier: make GetKnownFailure always return a non-
		// retriable failure.
	}
	// Override GetKnownFailure to always return non-retriable.
	store2 := store
	store2.err = nil
	// We can't override a single method on the struct; instead use a wrapper.
	wrapped := &nonRetriableStore{inner: store2}
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, wrapped, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("blocked-cfg", "ffmpeg"))
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.False(t, disp.called, "dispatch must NOT be called for a permanently blocked combo")
	assert.False(t, wrapped.inner.calledCreateConfig)
}

func TestIF_CreateConfig_NoDispatcher503(t *testing.T) {
	t.Parallel()
	store := s4Store()
	r := newIFRouter(t, store, &fakeOrgResolver{}) // no dispatcher wired

	w := postConfigs(t, r, s4Body("no-disp-cfg", "ffmpeg"))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestIF_CreateConfig_InvalidBody(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, []byte("{not json"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestIF_CreateConfig_EmptySelection(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("empty-cfg"))
	// Empty selection passes binding (it's present as []), but fails validation.
	assert.True(t, w.Code == http.StatusUnprocessableEntity || w.Code == http.StatusBadRequest,
		"empty selection should be rejected, got %d", w.Code)
}

func TestIF_CreateConfig_UnknownExtension(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("bad-ext", "ghost-extension"))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.False(t, disp.called)
}

func TestIF_CreateConfig_Unauthed(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs",
		bytes.NewReader(s4Body("cfg", "ffmpeg")))
	req.Header.Set("Content-Type", "application/json")
	// no X-Test-UserID → unauthenticated
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── helpers for S4 tests ─────────────────────────────────────────────────

func newIFRouterWithDispatcher(t *testing.T, store imageFactoryStore, orgs orgResolver, disp buildDispatcher) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)
	g := r.Group("/api/v1/image-factory")
	g.GET("/catalog", h.Catalog)
	g.GET("/configs", h.ListConfigs)
	g.GET("/configs/:hash", h.GetConfig)
	g.POST("/configs", h.CreateConfig)
	return r
}

// nonRetriableStore wraps a fakeIFStore and makes GetKnownFailure always
// return a non-retriable failure (for the known-failure test).
type nonRetriableStore struct {
	inner *fakeIFStore
}

func (n *nonRetriableStore) GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error) {
	return n.inner.GetPlatformConfig(ctx)
}
func (n *nonRetriableStore) ListBases(ctx context.Context) ([]imagefactory.Base, error) {
	return n.inner.ListBases(ctx)
}
func (n *nonRetriableStore) GetBase(ctx context.Context, name, version string) (imagefactory.Base, error) {
	return n.inner.GetBase(ctx, name, version)
}
func (n *nonRetriableStore) ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error) {
	return n.inner.ListExtensions(ctx, includeRetired)
}
func (n *nonRetriableStore) ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error) {
	return n.inner.ListKnownFailures(ctx)
}
func (n *nonRetriableStore) GetKnownFailure(ctx context.Context, hash, baseName string) (imagefactory.KnownFailure, error) {
	return imagefactory.KnownFailure{Retriable: false, Explanation: "permanently blocked"}, nil
}
func (n *nonRetriableStore) GetConfig(ctx context.Context, id string) (imagefactory.Config, error) {
	return n.inner.GetConfig(ctx, id)
}
func (n *nonRetriableStore) GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error) {
	return n.inner.GetConfigByHash(ctx, hash, scope, ownerID, orgID)
}
func (n *nonRetriableStore) ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error) {
	return n.inner.ListVisibleConfigs(ctx, ownerID, orgID)
}
func (n *nonRetriableStore) CreateConfig(ctx context.Context, c imagefactory.Config) error {
	return n.inner.CreateConfig(ctx, c)
}
func (n *nonRetriableStore) GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error) {
	return n.inner.GetInFlightOrSuccessfulBuild(ctx, hash, baseVersion)
}
func (n *nonRetriableStore) CreateBuild(ctx context.Context, b imagefactory.Build) error {
	return n.inner.CreateBuild(ctx, b)
}

// compile-time check that nonRetriableStore satisfies imageFactoryStore.
var _ imageFactoryStore = (*nonRetriableStore)(nil)
