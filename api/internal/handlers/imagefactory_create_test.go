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

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// fakeDispatcher scripts the Dispatch outcome. Records calls so tests
// assert whether dispatch was invoked.
type fakeDispatcher struct {
	ghRunID     int64
	err         error
	called      bool
	lastReq     dispatchRequest
	cancelCalls []int64
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req dispatchRequest) (int64, error) {
	f.called = true
	f.lastReq = req
	return f.ghRunID, f.err
}

func (f *fakeDispatcher) Cancel(_ context.Context, ghRunID int64) error {
	f.cancelCalls = append(f.cancelCalls, ghRunID)
	return nil
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
	// Verify the rendered Dockerfile is passed in the dispatch request.
	assert.NotEmpty(t, disp.lastReq.Dockerfile, "dispatch must carry rendered Dockerfile")
	assert.Contains(t, disp.lastReq.Dockerfile, "FROM ", "Dockerfile must start with FROM")
	assert.Contains(t, disp.lastReq.Dockerfile, "ffmpeg", "Dockerfile must contain the apt package")
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

// TestIF_CreateConfig_DispatchFailureLogsError locks in the diagnostic
// behavior: the underlying dispatch error must be surfaced to the logger,
// not discarded into the generic 503. Regression for the hours-long
// blind-spot caused by a swallowed error.
func TestIF_CreateConfig_DispatchFailureLogsError(t *testing.T) {
	t.Parallel()
	store := s4Store()
	dispatchErr := errors.New("gh dispatch: unexpected status 403: forbidden")
	disp := &fakeDispatcher{err: dispatchErr}
	log := &captureIFLogger{}
	h := NewImageFactoryHandler(store, &fakeOrgResolver{})
	h.SetDispatcher(disp)
	h.SetLogger(log)
	r := newIFRouterForHandler(t, h)

	w := postConfigs(t, r, s4Body("fail-cfg", "ffmpeg"))
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Len(t, log.errors, 1, "dispatch failure must be logged exactly once")
	assert.Contains(t, log.errors[0].msg, "dispatch failed",
		"log message must identify the failure path")
	assert.ErrorIs(t, log.errors[0].err, dispatchErr,
		"the underlying wrapped error must be passed to the logger")
}

// captureIFLogger records Error calls for assertion. No-ops the other
// levels — satisfies pkginterfaces.LoggerInterface.
type captureIFLogger struct{ errors []ifLogEntry }
type ifLogEntry struct {
	msg string
	err error
}

func (l *captureIFLogger) Debug(string, ...interface{}) {}
func (l *captureIFLogger) Info(string, ...interface{})  {}
func (l *captureIFLogger) Warn(string, ...interface{})  {}
func (l *captureIFLogger) Error(msg string, err error, _ ...interface{}) {
	l.errors = append(l.errors, ifLogEntry{msg: msg, err: err})
}
func (l *captureIFLogger) Fatal(string, error, ...interface{})               {}
func (l *captureIFLogger) With(...interface{}) pkginterfaces.LoggerInterface { return l }
func (l *captureIFLogger) Sync() error                                       { return nil }

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
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)
	return newIFRouterForHandler(t, h)
}

// newIFRouterForHandler mounts a pre-constructed handler (e.g. with a
// capture logger wired) on a minimal router mirroring the real route group.
func newIFRouterForHandler(t *testing.T, h *ImageFactoryHandler) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	g := r.Group("/api/v1/image-factory")
	g.GET("/catalog", h.Catalog)
	g.GET("/configs", h.ListConfigs)
	g.GET("/configs/:hash", h.GetConfig)
	g.POST("/configs", h.CreateConfig)
	return r
}

// ── #936: scoped-name collisions surface as 409, not opaque 500s ──────

func TestIF_CreateConfig_NameConflict_CoalescedPath_409(t *testing.T) {
	t.Parallel()
	store := s4Store()
	store.existingBuild = &imagefactory.Build{Status: imagefactory.BuildSucceeded}
	store.createConfigErr = database.ErrConflict
	disp := &fakeDispatcher{ghRunID: 999}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("dup-name", "ffmpeg"))
	require.Equal(t, http.StatusConflict, w.Code, "coalesced-path collision must 409; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "dup-name", "the error names the colliding config")
	assert.Empty(t, disp.cancelCalls, "no dispatch fired on the coalesced path — nothing to cancel")
}

func TestIF_CreateConfig_NameConflict_FreshPath_409_AndCancelsDispatch(t *testing.T) {
	t.Parallel()
	store := s4Store() // no existingBuild → fresh path
	store.createBuildErr = database.ErrConflict
	disp := &fakeDispatcher{ghRunID: 4242}
	r := newIFRouterWithDispatcher(t, store, &fakeOrgResolver{}, disp)

	w := postConfigs(t, r, s4Body("dup-name", "ffmpeg"))
	require.Equal(t, http.StatusConflict, w.Code, "fresh-path collision must 409; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "dup-name")
	assert.True(t, disp.called, "dispatch fired before the failing insert (dispatch-before-commit)")
	// The recorded run ID (4242 from the fake) must be canceled.
	require.NotEmpty(t, disp.cancelCalls)
	assert.Equal(t, int64(4242), disp.cancelCalls[0])
}
