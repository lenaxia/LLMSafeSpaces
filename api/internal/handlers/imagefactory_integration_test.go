// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
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

// TestIF_RoundTrip_CreateThenCallback exercises the full write-path vertical
// slice: POST /configs (novel dispatch) → POST /callback (succeeded). Verifies
// the config+build are committed atomically, the dispatch fires, and the
// callback transitions both rows to ready+succeeded.
//
// Note: in production, CreateConfigAndBuild and the callback share the same
// *database.Service. The test uses two separate fakes (fakeIFStore +
// fakeBuildStore), so after step 1 we mirror the build into the buildStore
// to simulate the shared-DB state.
func TestIF_RoundTrip_CreateThenCallback(t *testing.T) {
	t.Parallel()

	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 42}
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, &fakeOrgResolver{})
	h.SetDispatcher(disp)
	h.SetBuildStore(bs, "ghcr.io/acme/ws", "/internal/image-factory/builds")
	r.POST("/api/v1/image-factory/configs", h.CreateConfig)
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)

	// Step 1: POST /configs — novel dispatch.
	cfgBody, _ := json.Marshal(createConfigRequest{
		Name: "rt-cfg", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	req1 := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Test-UserID", "user-1")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	require.Equal(t, http.StatusCreated, w1.Code)
	assert.True(t, disp.called, "dispatch must fire")

	// Verify config + build were committed atomically (both fakes recorded).
	assert.True(t, store.calledCreateConfig, "config committed")
	assert.True(t, store.calledCreateBuild, "build committed")
	cfg := store.lastCreatedConfig
	require.NotNil(t, cfg)
	assert.Equal(t, imagefactory.StatusBuilding, cfg.Status)
	build := store.lastCreatedBuild
	require.NotNil(t, build)
	assert.Equal(t, int64(42), *build.GHRunID)

	// Mirror the build into the buildStore (simulates shared-DB state).
	bs.builds[build.ID] = *build

	// Step 2: POST /callback — succeeded.
	callbackBody, _ := json.Marshal(callbackRequest{
		Status: "succeeded", Digest: "sha256:built-image",
	})
	req2 := httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+build.ID+"/callback",
		bytes.NewReader(callbackBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+build.CallbackToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusNoContent, w2.Code)

	// Verify the callback transitioned the build to succeeded.
	updated := bs.builds[build.ID]
	assert.Equal(t, imagefactory.BuildSucceeded, updated.Status)
	assert.Equal(t, "sha256:built-image", updated.Digest)
	assert.Contains(t, updated.ImageRef, "s-")
}

// TestIF_RoundTrip_CreateThenCallbackFailed exercises the failure path:
// POST /configs (novel) → POST /callback (failed) → known_failure recorded.
func TestIF_RoundTrip_CreateThenCallbackFailed(t *testing.T) {
	t.Parallel()

	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 99}

	// Seed the buildStore with the build that CreateConfigAndBuild would
	// have committed (the fake dispatcher can't write to the buildStore,
	// so we simulate the post-commit state).
	build := &imagefactory.Build{
		ID:            "b-roundtrip-fail",
		ConfigID:      "c-roundtrip-fail",
		Hash:          "s-xxx",
		BaseName:      "bookworm",
		BaseVersion:   "0.6.0",
		Status:        imagefactory.BuildDispatched,
		CallbackToken: "tok-rtf",
		ResolvedValues: imagefactory.ResolvedValues{
			"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"},
		},
	}
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-roundtrip-fail": *build,
	}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &ImageFactoryHandler{
		store:      store,
		orgs:       &fakeOrgResolver{},
		dispatcher: disp,
		buildStore: bs,
		imageRepo:  "ghcr.io/acme/ws",
	}
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)

	// POST callback — failed.
	callbackBody, _ := json.Marshal(callbackRequest{
		Status: "failed", FailureReason: "E: unable to locate package ffmpegx",
	})
	req := httptest.NewRequest("POST",
		"/internal/image-factory/builds/b-roundtrip-fail/callback",
		bytes.NewReader(callbackBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-rtf")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	updated := bs.builds["b-roundtrip-fail"]
	assert.Equal(t, imagefactory.BuildFailed, updated.Status)
	assert.Contains(t, updated.FailureReason, "unable to locate package")
	assert.NotEmpty(t, bs.lastKnownFail.Explanation, "known failure recorded with explanation")
	assert.True(t, bs.lastKnownFail.Retriable)
}

// TestIF_RoundTrip_CoalesceThenReady exercises: POST /configs (coalesce onto
// succeeded) → config immediately ready, no dispatch.
func TestIF_RoundTrip_CoalesceThenReady(t *testing.T) {
	t.Parallel()

	store := s4Store()
	store.existingBuild = &imagefactory.Build{Status: imagefactory.BuildSucceeded}
	disp := &fakeDispatcher{ghRunID: 1}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, &fakeOrgResolver{})
	h.SetDispatcher(disp)
	r.POST("/api/v1/image-factory/configs", h.CreateConfig)

	cfgBody, _ := json.Marshal(createConfigRequest{
		Name: "coalesce-cfg", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	assert.False(t, disp.called, "dispatch must NOT fire when coalescing")
	assert.Equal(t, imagefactory.StatusReady, store.lastCreatedConfig.Status,
		"coalescing onto succeeded → config immediately ready")
}

// TestIF_CreateConfig_StoreErrorOnCommit verifies that if the atomic
// CreateConfigAndBuild fails (e.g. DB constraint), the handler returns 500
// and does NOT leave the user with a partial response. This is the
// regression test for the atomicity fix.
func TestIF_CreateConfig_StoreErrorOnCommit(t *testing.T) {
	t.Parallel()

	store := s4Store()
	store.createConfigErr = errors.New("DB constraint violation")
	disp := &fakeDispatcher{ghRunID: 1}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, &fakeOrgResolver{})
	h.SetDispatcher(disp)
	r.POST("/api/v1/image-factory/configs", h.CreateConfig)

	cfgBody, _ := json.Marshal(createConfigRequest{
		Name: "err-cfg", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"commit failure must surface as 500, not 201")
}
