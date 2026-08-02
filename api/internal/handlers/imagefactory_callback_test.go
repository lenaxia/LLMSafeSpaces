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

// fakeBuildStore is the test double for buildStore.
type fakeBuildStore struct {
	builds        map[string]imagefactory.Build
	succeedErr    error
	failErr       error
	lastKnownFail imagefactory.KnownFailure
}

func (f *fakeBuildStore) GetBuild(_ context.Context, id string) (imagefactory.Build, error) {
	if b, ok := f.builds[id]; ok {
		return b, nil
	}
	return imagefactory.Build{}, database.ErrNotFound
}
func (f *fakeBuildStore) TransitionBuildSucceeded(_ context.Context, buildID, configID, imageRef, digest string) error {
	if f.succeedErr != nil {
		return f.succeedErr
	}
	if b, ok := f.builds[buildID]; ok {
		b.Status = imagefactory.BuildSucceeded
		b.ImageRef = imageRef
		b.Digest = digest
		f.builds[buildID] = b
	}
	return nil
}
func (f *fakeBuildStore) TransitionBuildFailed(_ context.Context, buildID, configID string, kf imagefactory.KnownFailure) error {
	if f.failErr != nil {
		return f.failErr
	}
	if b, ok := f.builds[buildID]; ok {
		b.Status = imagefactory.BuildFailed
		b.FailureReason = kf.FailureReason
		b.Explanation = kf.Explanation
		f.builds[buildID] = b
	}
	f.lastKnownFail = kf
	return nil
}

func newCallbackRouter(t *testing.T, bs buildStore) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &ImageFactoryHandler{buildStore: bs, imageRepo: "ghcr.io/acme/ws"}
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)
	return r
}

func postCallback(t *testing.T, r http.Handler, buildID, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/internal/image-factory/builds/"+buildID+"/callback", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestIF_Callback_Succeeded(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", ConfigID: "c-1", Hash: "s-abc", BaseName: "bookworm", BaseVersion: "0.6.0",
			Status: imagefactory.BuildDispatched, CallbackToken: "tok-secret"},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok-secret", callbackRequest{Status: "succeeded", Digest: "sha256:ok"})
	require.Equal(t, http.StatusNoContent, w.Code)
	b := bs.builds["b-1"]
	assert.Equal(t, imagefactory.BuildSucceeded, b.Status)
	assert.Equal(t, "sha256:ok", b.Digest)
	assert.Contains(t, b.ImageRef, "s-abc-0.6.0")
}

func TestIF_Callback_Failed(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", ConfigID: "c-1", Hash: "s-abc", BaseName: "bookworm", BaseVersion: "0.6.0",
			Status: imagefactory.BuildDispatched, CallbackToken: "tok-secret",
			ResolvedValues: imagefactory.ResolvedValues{"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"}}},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok-secret", callbackRequest{Status: "failed", FailureReason: "apt: 404 not found"})
	require.Equal(t, http.StatusNoContent, w.Code)
	b := bs.builds["b-1"]
	assert.Equal(t, imagefactory.BuildFailed, b.Status)
	assert.Contains(t, b.FailureReason, "apt: 404")
	assert.NotEmpty(t, bs.lastKnownFail.Explanation, "known failure must be recorded with explanation")
	assert.True(t, bs.lastKnownFail.Retriable)
}

func TestIF_Callback_WrongToken403(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", Status: imagefactory.BuildDispatched, CallbackToken: "tok-secret"},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "wrong-token", callbackRequest{Status: "succeeded"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestIF_Callback_MissingToken403(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", Status: imagefactory.BuildDispatched, CallbackToken: "tok-secret"},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "", callbackRequest{Status: "succeeded"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestIF_Callback_IdempotentReplay(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", ConfigID: "c-1", Status: imagefactory.BuildSucceeded, CallbackToken: "tok-secret"},
	}}
	r := newCallbackRouter(t, bs)
	// Already terminal → 204 without re-transitioning.
	w := postCallback(t, r, "b-1", "tok-secret", callbackRequest{Status: "failed"})
	require.Equal(t, http.StatusNoContent, w.Code)
	b := bs.builds["b-1"]
	assert.Equal(t, imagefactory.BuildSucceeded, b.Status, "replay must not overwrite terminal state")
}

func TestIF_Callback_BuildNotFound404(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-ghost", "tok", callbackRequest{Status: "succeeded"})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestIF_Callback_InvalidBody400(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", Status: imagefactory.BuildDispatched, CallbackToken: "tok"},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok", map[string]string{"bad": "shape"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestIF_Callback_InvalidStatus400(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", Status: imagefactory.BuildDispatched, CallbackToken: "tok"},
	}}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok", callbackRequest{Status: "wat"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestIF_Callback_TokenFromBuildA_DoesNotWorkOnBuildB(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {ID: "b-1", Status: imagefactory.BuildDispatched, CallbackToken: "tok-A"},
		"b-2": {ID: "b-2", Status: imagefactory.BuildDispatched, CallbackToken: "tok-B"},
	}}
	r := newCallbackRouter(t, bs)
	// Token from build A must NOT work on build B.
	w := postCallback(t, r, "b-2", "tok-A", callbackRequest{Status: "succeeded"})
	assert.Equal(t, http.StatusForbidden, w.Code, "token from build A must not mutate build B")
}

func TestIF_Callback_SucceededStoreError500(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{
		builds: map[string]imagefactory.Build{
			"b-1": {ID: "b-1", ConfigID: "c-1", Hash: "s-abc", BaseName: "bookworm",
				BaseVersion: "0.6.0", Status: imagefactory.BuildDispatched, CallbackToken: "tok"},
		},
		succeedErr: testError("connection lost"),
	}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok", callbackRequest{Status: "succeeded"})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestIF_Callback_FailedStoreError500(t *testing.T) {
	t.Parallel()
	bs := &fakeBuildStore{
		builds: map[string]imagefactory.Build{
			"b-1": {ID: "b-1", ConfigID: "c-1", Hash: "s-abc", BaseName: "bookworm",
				BaseVersion: "0.6.0", Status: imagefactory.BuildDispatched, CallbackToken: "tok",
				ResolvedValues: imagefactory.ResolvedValues{"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"}}},
		},
		failErr: testError("FK constraint"),
	}
	r := newCallbackRouter(t, bs)
	w := postCallback(t, r, "b-1", "tok", callbackRequest{Status: "failed", FailureReason: "apt 404"})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
