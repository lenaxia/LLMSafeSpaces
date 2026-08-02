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

// TestIF_Callback_Failed_WithLLMExplainer exercises the full handler path:
// callback → explainFailure → LLM server → TransitionBuildFailed with the
// LLM-generated explanation + attribution → SetExtensionReviewRequested.
// This is the integration test the bot required — it goes through the
// real handler code, not just Explain() in isolation.
func TestIF_Callback_Failed_WithLLMExplainer(t *testing.T) {
	t.Parallel()
	// LLM test server returns an attributed explanation.
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner := explainResponse{
			Explanation:         "package ffmpegx does not exist in bookworm",
			AttributedExtension: "ffmpegx",
		}
		innerJSON, _ := json.Marshal(inner)
		resp := chatCompletionResponse{}
		resp.Choices = []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Role: "assistant", Content: string(innerJSON)}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer llmSrv.Close()

	rv := imagefactory.ResolvedValues{
		"ffmpegx": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpegx"},
	}
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {
			ID: "b-1", ConfigID: "c-1", Hash: "s-fail", BaseName: "bookworm",
			BaseVersion: "0.6.0", Status: imagefactory.BuildDispatched, CallbackToken: "tok",
			ResolvedValues: rv,
		},
	}}
	fr := &fakeExtensionReviewer{}

	r := gin.New()
	h := &ImageFactoryHandler{
		buildStore: bs,
		imageRepo:  "ghcr.io/ws",
		explainer:  NewLLMExplainer(LLMExplainerConfig{BaseURL: llmSrv.URL, Model: "m"}),
		adminStore: fr,
	}
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)

	w := postCallback(t, r, "b-1", "tok", callbackRequest{
		Status: "failed", FailureReason: "E: Unable to locate package ffmpegx",
	})
	require.Equal(t, http.StatusNoContent, w.Code)

	// The known_failure recorded by TransitionBuildFailed must contain the
	// LLM-generated explanation (not the fallback).
	assert.Contains(t, bs.lastKnownFail.Explanation, "ffmpegx")
	assert.Contains(t, bs.lastKnownFail.Explanation, "does not exist")
	assert.Equal(t, "E: Unable to locate package ffmpegx", bs.lastKnownFail.FailureReason)

	// The attributed extension must be flagged for review.
	assert.True(t, fr.called, "SetExtensionReviewRequested must be called for attributed extension")
	assert.Equal(t, "ffmpegx", fr.lastID)
	assert.True(t, fr.lastValue)
}

// TestIF_Callback_Failed_LLMDown_UsesFallback exercises the degradation
// path through the handler: LLM unreachable → fallback explanation, no
// attribution, no review flag.
func TestIF_Callback_Failed_LLMDown_UsesFallback(t *testing.T) {
	t.Parallel()
	rv := imagefactory.ResolvedValues{
		"ffmpeg": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"},
	}
	bs := &fakeBuildStore{builds: map[string]imagefactory.Build{
		"b-1": {
			ID: "b-1", ConfigID: "c-1", Hash: "s-fail2", BaseName: "bookworm",
			BaseVersion: "0.6.0", Status: imagefactory.BuildDispatched, CallbackToken: "tok",
			ResolvedValues: rv,
		},
	}}
	fr := &fakeExtensionReviewer{}

	r := gin.New()
	h := &ImageFactoryHandler{
		buildStore: bs,
		imageRepo:  "ghcr.io/ws",
		explainer:  NewLLMExplainer(LLMExplainerConfig{BaseURL: "http://127.0.0.1:1", Model: "m"}),
		adminStore: fr,
	}
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)

	w := postCallback(t, r, "b-1", "tok", callbackRequest{
		Status: "failed", FailureReason: "apt: 404",
	})
	require.Equal(t, http.StatusNoContent, w.Code)

	// Fallback explanation (not the LLM one).
	assert.Equal(t, "this combination failed to build; contact your administrator for details",
		bs.lastKnownFail.Explanation)
	// No attribution → no review flag.
	assert.False(t, fr.called, "review must NOT be flagged in degradation mode")
}

// fakeExtensionReviewer tracks SetExtensionReviewRequested calls.
type fakeExtensionReviewer struct {
	called    bool
	lastID    string
	lastValue bool
}

func (f *fakeExtensionReviewer) SetExtensionReviewRequested(_ context.Context, id string, v bool) error {
	f.called = true
	f.lastID = id
	f.lastValue = v
	return nil
}
