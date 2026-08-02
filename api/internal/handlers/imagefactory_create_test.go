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
	store.getKnownFailureOverride = &imagefactory.KnownFailure{
		Retriable: false, Explanation: "permanently blocked",
	}
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("blocked-cfg", "ffmpeg"))
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.False(t, disp.called, "dispatch must NOT be called for a permanently blocked combo")
	assert.False(t, store.calledCreateConfig)
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
	assert.Equal(t, http.StatusBadRequest, w.Code,
		"empty array fails gin binding (selection has binding:required)")
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
