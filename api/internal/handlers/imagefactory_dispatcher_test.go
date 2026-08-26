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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGHActionsDispatcher_Dispatch_Success(t *testing.T) {
	// GitHub's workflow_dispatch endpoint returns 204 No Content on success
	// (not 201 Created). This is the actual production behavior.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/repos/owner/repo/actions/workflows/image-build.yml/dispatches", r.URL.Path)
		require.Equal(t, "Bearer fake-installation-token", r.Header.Get("Authorization"))

		var payload ghDispatchPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, "develop", payload.Ref)
		assert.Equal(t, "build-1", payload.Inputs["build_id"])
		assert.Contains(t, payload.Inputs["dockerfile"], "FROM")
		assert.Contains(t, payload.Inputs["architectures"], "linux/amd64")

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID:          "123",
		privateKey:     "",
		owner:          "owner",
		repo:           "repo",
		workflowID:     "image-build.yml",
		ref:            "develop",
		client:         srv.Client(),
		cachedToken:    "fake-installation-token",
		cachedTokenExp: time.Now().Add(1 * time.Hour),
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
		BaseVersion:   "0.20.1",
		Architectures: []string{"linux/amd64"},
		Dockerfile:    "FROM base\n",
	})
	require.NoError(t, err)
}

// TestGHActionsDispatcher_Dispatch_Accepts201 verifies the dispatcher also
// accepts 201 Created (defensive — GitHub returns 204 today, but accepting
// both guards against future API changes).
func TestGHActionsDispatcher_Dispatch_Accepts201(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID: "123", owner: "o", repo: "r", workflowID: "w.yml", ref: "main",
		client:         srv.Client(),
		cachedToken:    "tok",
		cachedTokenExp: time.Now().Add(1 * time.Hour),
	}
	oldURL := dispatchURL
	dispatchURL = srv.URL + "/repos/%s/%s/actions/workflows/%s/dispatches"
	defer func() { dispatchURL = oldURL }()

	_, err := d.Dispatch(context.Background(), dispatchRequest{Dockerfile: "FROM x\n"})
	require.NoError(t, err, "201 Created must be accepted as success")
}

func TestGHActionsDispatcher_Dispatch_GHError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID:          "123",
		privateKey:     "",
		owner:          "owner",
		repo:           "repo",
		workflowID:     "image-build.yml",
		ref:            "main",
		client:         srv.Client(),
		cachedToken:    "fake-installation-token",
		cachedTokenExp: time.Now().Add(1 * time.Hour),
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

func TestGHActionsDispatcher_TokenCaching(t *testing.T) {
	// NOT t.Parallel() — overrides ghBaseURL global.
	t.Parallel()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/repos/owner/repo/installation" {
			json.NewEncoder(w).Encode(map[string]any{"id": 99999})
			return
		}
		if r.URL.Path == "/app/installations/99999/access_tokens" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test_token_" + string(rune('0'+callCount))})
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID:      "123",
		privateKey: testRSAKey,
		owner:      "owner",
		repo:       "repo",
		workflowID: "test.yml",
		ref:        "main",
		client:     srv.Client(),
	}
	oldGH := ghBaseURL
	ghBaseURL = srv.URL
	defer func() { ghBaseURL = oldGH }()

	// First call: mints token (2 API calls: installation + access_token)
	token1, err := d.getInstallationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_test_token_2", token1, "token from access_token call (install + access + dispatch)")

	// Second call: cached (0 additional API calls)
	token2, err := d.getInstallationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, token1, token2, "cached token must be returned")
	assert.Equal(t, 2, callCount, "installation lookup must not be repeated")
}

const testRSAKey = `-----BEGIN PRIVATE KEY-----
MIIEuwIBADANBgkqhkiG9w0BAQEFAASCBKUwggShAgEAAoIBAQCxM9LUdQTxPukk
Tm4yRV1uPzmydeCx2p1qXC7sLxTECjD1ZGWaYxplxaG/zetlBk6qdnBDBt3G2D2J
iDO2RPUoam8uChhOJV6ARedhKpvLNcIBIJZzZDypIh8sDvbocbOdNxvw+Ztmh7YU
MH3qabvuRUwv7/EjdZKQjZR9cbBfplP44sZRgZPU9ut3s9eKsjdkCWg7YVzYNmNY
fNpdcddfnXtT/4Qao1V8Pk8WHuGS8dnY06W5FyWPNnX8HFJenYN4vBUOP+3t9IDx
nQvopj+/909vDTSQ8EvqNNkovSANm1gSvxKVG9WK/ILlerxVc0iY49+bAV+H48uD
bSJ7Xwg3AgMBAAECgf8QGcB3XetD8qoz2a7hxH5YHPD+picjS9zUfOj8N/+/7r98
tJPhvmYVQSpamHE7aQMY6LXlZbOI/Hzwy96oP21mkcMqhktO/L6kssx3716vJROk
P6g4eWxpFC8ye7m0Lj4+OrIeESvjIsCw0171pdSvFWw2PEEIS2fFVgL5mSrTp8lq
nf6gghjSN0epTIyJG+L7ycMyqVlXR/ZXX3Z2zSTJS09bDuq1hnaEdiVLuFc+a13f
oFUmaZNxIUM7PefJFpfi993Ke/fI6oMfVciv5IFBOa3mP6LAV7hQfrJ1DUa0xVmF
Jy3GEK9wSH9X83y2qAzmwTAGpuSZIfAcHH8KPhkCgYEA9Ij6WtrD6fouwgjQ4oqf
r++xw49lFSQc5zV5puIif/DC6Ki3bmbaH348Nyp+9UpvitDYbzAYJ6rb0Z7xXmvQ
8QQOS/2wXbCSq5HHrGs7Q7oqTaJEEV6potULt+IkalALzgiy2ohm82p207d1Gmvc
+HWozVdNNV+CyINDt+VoyuMCgYEAuYKwPkjBTyGPfGRcO6Y6PtBhghuGBEf5jV0I
qW6WiyDn2PInzzshBA6n4Balpc+bPw1UnERyZc2SIPt2uwPslCp+/YVBYwEksWgr
zU3Da5ezFJ3jRNirZqSj/G+sVarrB0kmLPm+VgK35vCGlZ4Axiv15RvNqv0el3+o
AZFm6Z0CgYBO1m6opgktwSwcAI2fzAOJzGRqYSu8siTjYfkzlYp75xpfui1RWbWP
G7q8KmY+HN5zSbvNtRrEhzBRl8XHpEj7u0wEseiPfCL9T4Wpj/TOdBG5b8w0MWnN
hpQ9l5oX8HCt314SWJGgfr2KqoYFm6rlK8HdWf0ZbQ6UKMXHXx328wKBgQCnOYSw
EJuZPnJ+umVeK+ETYHqVc0QitdLiOHwnZ5XzQq1cpiV2rCF968wut5uI1ZVniBe+
agEJff79FlEYElh/07L3y9h+a7hs56+ceT3wziXTLuSA2iPf+ggM9YnPC6yju6/b
GSIXnIm0dxuK4YxnF5eoeKC0Q0oBXUTQbQbtDQKBgGdda3yHO+nXQbt5PES6WiUZ
QCaje7fPR04qxWguWrz5Z/n58WwVrNzx/Q83JjXs3vc6x2HXhRoumqRbddZntHBJ
6kIljGTnmKDiUD7HwW8fVs08QzF83AO02pBC28Z0sRlePZ39iFgDPAd1sAMgAzW1
LPxUGo39LjLnfvu6VboF
-----END PRIVATE KEY-----`

func TestGHActionsDispatcher_GetInstallationID_NotFound(t *testing.T) {
	// NOT t.Parallel() — overrides ghBaseURL global.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID:      "123",
		privateKey: testRSAKey,
		owner:      "ghost",
		repo:       "repo",
		client:     srv.Client(),
	}
	oldGH := ghBaseURL
	ghBaseURL = srv.URL
	defer func() { ghBaseURL = oldGH }()

	jwt, err := d.mintAppJWT()
	require.NoError(t, err)
	_, err = d.getInstallationID(context.Background(), jwt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestGHActionsDispatcher_CreateInstallationToken_Unauthorized(t *testing.T) {
	// NOT t.Parallel() — overrides ghBaseURL global.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Forbidden"}`)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{
		appID:      "123",
		privateKey: testRSAKey,
		client:     srv.Client(),
	}
	oldGH := ghBaseURL
	ghBaseURL = srv.URL
	defer func() { ghBaseURL = oldGH }()

	jwt, err := d.mintAppJWT()
	require.NoError(t, err)
	_, err = d.createInstallationToken(context.Background(), jwt, 99999)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestGHActionsDispatcher_MintAppJWT_InvalidKey(t *testing.T) {
	t.Parallel()
	d := &ghActionsDispatcher{
		appID:      "123",
		privateKey: "not-a-real-key",
	}
	_, err := d.mintAppJWT()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse private key")
}

func TestGHActionsDispatcher_Dispatch_TokenMintFailure(t *testing.T) {
	t.Parallel()
	d := &ghActionsDispatcher{
		appID:      "123",
		privateKey: "invalid",
		owner:      "owner",
		repo:       "repo",
		workflowID: "test.yml",
		ref:        "main",
		client:     &http.Client{},
	}
	// No cached token — will try to mint and fail.
	_, err := d.Dispatch(context.Background(), dispatchRequest{
		BuildID: "b", Dockerfile: "FROM x\n",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get token")
	assert.Contains(t, err.Error(), "parse private key")
}

// ── Cancel (#936) ──────────────────────────────────────────────────────

func TestGHActionsDispatcher_Cancel_RunIDZero_NoOp(t *testing.T) {
	// Today's production shape: Dispatch returns 0 (workflow_dispatch
	// yields no run ID), so Cancel(0) must silently no-op — no HTTP call.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	d := &ghActionsDispatcher{client: srv.Client()}
	require.NoError(t, d.Cancel(context.Background(), 0))
	require.NoError(t, d.Cancel(context.Background(), -1))
	assert.False(t, called, "runID<=0 must not hit the API")
}

func TestGHActionsDispatcher_Cancel_Accepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repos/owner/repo/actions/runs/4242/cancel", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	oldCU := cancelURL
	cancelURL = srv.URL + "/repos/%s/%s/actions/runs/%d/cancel"
	defer func() { cancelURL = oldCU }()
	d := &ghActionsDispatcher{owner: "owner", repo: "repo", client: srv.Client(), cachedToken: "tok", cachedTokenExp: time.Now().Add(time.Hour)}
	require.NoError(t, d.Cancel(context.Background(), 4242))
}

func TestGHActionsDispatcher_Cancel_AlreadyCompleted_409_IsSuccess(t *testing.T) {
	// 409 = the run already finished; nothing to cancel — not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	oldCU := cancelURL
	cancelURL = srv.URL + "/repos/%s/%s/actions/runs/%d/cancel"
	defer func() { cancelURL = oldCU }()
	d := &ghActionsDispatcher{owner: "owner", repo: "repo", client: srv.Client(), cachedToken: "tok", cachedTokenExp: time.Now().Add(time.Hour)}
	require.NoError(t, d.Cancel(context.Background(), 4242))
}

func TestGHActionsDispatcher_Cancel_UnexpectedStatus_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"bad token"}`))
	}))
	defer srv.Close()

	oldCU := cancelURL
	cancelURL = srv.URL + "/repos/%s/%s/actions/runs/%d/cancel"
	defer func() { cancelURL = oldCU }()
	d := &ghActionsDispatcher{owner: "owner", repo: "repo", client: srv.Client(), cachedToken: "tok", cachedTokenExp: time.Now().Add(time.Hour)}
	err := d.Cancel(context.Background(), 4242)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}
