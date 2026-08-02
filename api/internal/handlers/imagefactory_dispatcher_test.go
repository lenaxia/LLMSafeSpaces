// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGHActionsDispatcher_Dispatch_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/owner/repo/actions/workflows/image-build.yml/dispatches", r.URL.Path)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		var payload ghDispatchPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, "develop", payload.Ref)
		assert.Equal(t, "build-1", payload.Inputs["build_id"])
		assert.Contains(t, payload.Inputs["dockerfile"], "FROM")
		assert.Contains(t, payload.Inputs["architectures"], "linux/amd64")

		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		apiToken:   "test-token",
		owner:      "owner",
		repo:       "repo",
		workflowID: "image-build.yml",
		ref:        "develop",
		client:     srv.Client(),
	}
	oldURL := dispatchURL
	dispatchURL = srv.URL + "/repos/%s/%s/actions/workflows/%s/dispatches"
	defer func() { dispatchURL = oldURL }()

	_, err := d.Dispatch(context.Background(), dispatchRequest{
		BuildID:       "build-1",
		CallbackURL:   "http://api/callback",
		CallbackToken: "tok",
		Hash:          "s-abc",
		BaseName:      "bookworm",
		BaseVersion:   "0.6.0",
		Architectures: []string{"linux/amd64"},
		Dockerfile:    "FROM base\n",
	})
	require.NoError(t, err)
}

func TestGHActionsDispatcher_Dispatch_GHError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		apiToken:   "bad-token",
		owner:      "owner",
		repo:       "repo",
		workflowID: "image-build.yml",
		client:     srv.Client(),
	}
	oldURL := dispatchURL
	dispatchURL = srv.URL + "/repos/%s/%s/actions/workflows/%s/dispatches"
	defer func() { dispatchURL = oldURL }()

	_, err := d.Dispatch(context.Background(), dispatchRequest{
		BuildID: "b", Dockerfile: "FROM x\n",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestJoinArchs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "linux/amd64", joinArchs([]string{"linux/amd64"}))
	assert.Equal(t, "linux/amd64,linux/arm64", joinArchs([]string{"linux/amd64", "linux/arm64"}))
	assert.Equal(t, "", joinArchs(nil))
}
