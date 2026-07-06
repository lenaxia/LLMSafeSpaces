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

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakeTokenReviewer struct {
	username string
	err      error
	called   bool
	token    string
}

func (f *fakeTokenReviewer) Review(_ context.Context, token string) (string, error) {
	f.called = true
	f.token = token
	return f.username, f.err
}

type fakeBootstrapInjector struct {
	secrets []byte
	err     error
}

func (f *fakeBootstrapInjector) InjectSecretsForPodBootstrap(_ context.Context, _, _ string) ([]byte, error) {
	return f.secrets, f.err
}

type fakeBootstrapLookup struct {
	ws  *types.WorkspaceMetadata
	err error
}

func (f *fakeBootstrapLookup) GetWorkspace(_ context.Context, _ string) (*types.WorkspaceMetadata, error) {
	return f.ws, f.err
}

const testBootstrapNamespace = "llmsafespace"

func newTestBootstrapRouter(t *testing.T, reviewer *fakeTokenReviewer, injector *fakeBootstrapInjector, lookup *fakeBootstrapLookup) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(reviewer, injector, lookup, nil, testBootstrapNamespace)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	return r
}

func doBootstrap(t *testing.T, router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/pod-bootstrap", bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestPodBootstrap_ValidToken_ReturnsSecrets(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{secrets: []byte(`[{"type":"llm-provider","name":"test"}]`)}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", DefaultModel: "glm-5.2"}}

	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	w := doBootstrap(t, router, "valid-token", `{"workspaceID":"ws-abc"}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "valid-token", reviewer.token, "token reviewer must receive the raw bearer token")

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, string(resp.Secrets), "llm-provider")

	var wsCfg map[string]any
	require.NoError(t, json.Unmarshal(resp.WorkspaceConfig, &wsCfg))
	assert.Equal(t, "glm-5.2", wsCfg["defaultModel"])
}

func TestPodBootstrap_MissingAuthHeader_Returns401(t *testing.T) {
	router := newTestBootstrapRouter(t, &fakeTokenReviewer{}, &fakeBootstrapInjector{}, &fakeBootstrapLookup{})
	w := doBootstrap(t, router, "", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestPodBootstrap_TokenReviewError_Returns500(t *testing.T) {
	reviewer := &fakeTokenReviewer{err: context.DeadlineExceeded}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, &fakeBootstrapLookup{})
	w := doBootstrap(t, router, "some-token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPodBootstrap_SANameMismatch_Returns403(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-xyz"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPodBootstrap_SANotWorkspacePattern_Returns403(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:default"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPodBootstrap_WorkspaceNotFound_Returns404(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-ghost"}
	lookup := &fakeBootstrapLookup{ws: nil}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-ghost"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPodBootstrap_InjectorError_Returns500(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{err: context.DeadlineExceeded}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPodBootstrap_EmptySecrets_Returns200Empty(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{secrets: []byte(`[]`)}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "[]", string(resp.Secrets))
}

func TestPodBootstrap_NoDefaultModel_OmitsWorkspaceConfig(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{secrets: []byte(`[]`)}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", DefaultModel: ""}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp.WorkspaceConfig, "workspaceConfig must be omitted when no default model")
}

func TestPodBootstrap_LookupError_Returns500(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{err: context.DeadlineExceeded}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPodBootstrap_MissingWorkspaceID_Returns400(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, &fakeBootstrapLookup{})
	w := doBootstrap(t, router, "token", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestParseSAPrincipal(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantNS   string
		wantID   string
		wantOK   bool
	}{
		{"uuid", "system:serviceaccount:llmsafespace:workspace-550e8400-e29b-41d4-a716-446655440000", "llmsafespace", "550e8400-e29b-41d4-a716-446655440000", true},
		{"short", "system:serviceaccount:default:workspace-abc", "default", "abc", true},
		{"not workspace prefix", "system:serviceaccount:default:default", "", "", false},
		{"garbage", "not-a-valid-username", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, id, ok := parseSAPrincipal(tt.username)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantNS, ns)
			assert.Equal(t, tt.wantID, id)
		})
	}
}

// TestPodBootstrap_LogsUnderlyingError_OnInjectorFailure proves the
// observability gap surfaced by the 2026-06-24 production incident: when
// InjectSecrets fails (e.g. "DEK not available" for non-LLM
// user secrets at boot), the handler returns a generic 500 "secret
// preparation failed" with NO breadcrumb of the underlying cause. An
// operator inspecting API logs sees only the request lifecycle and the
// status code; they cannot tell whether the failure is a missing KEK, a
// DB error, a decrypt failure, or anything else without enabling debug.
//
// The handler MUST log the wrapped error at error level before returning.
// This is independent of any behavioral fix — even after the SOLID
// redesign in PR #2, the handler is the right place to emit diagnostics
// for internal-API 5xx responses.
func TestPodBootstrap_LogsUnderlyingError_OnInjectorFailure(t *testing.T) {
	log, logs := logger.NewObserved()

	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	sentinel := errors.New("get DEK for non-LLM secrets: DEK not available: session expired or not unlocked")
	injector := &fakeBootstrapInjector{err: sentinel}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(reviewer, injector, lookup, nil, testBootstrapNamespace)
	h.SetLogger(log)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	entries := logs.FilterMessageSnippet("secret preparation failed").All()
	require.GreaterOrEqual(t, len(entries), 1, "handler must log the failure with the underlying error; got logs: %+v", logs.All())

	entry := entries[0]
	require.Equal(t, "error", entry.Level.String(), "log level must be ERROR for a 5xx-causing failure")

	var sawWorkspaceID, sawErrorText bool
	for _, f := range entry.Context {
		if f.Key == "workspaceID" && f.String == "ws-abc" {
			sawWorkspaceID = true
		}
		// The wrapped error must be present so operators can diagnose the
		// underlying cause (here: DEK-not-available). zap's logger.Error
		// puts the error value in Field.Interface (type ErrorType=26),
		// not in Field.String. Match either, since the exact field
		// shape depends on logger usage.
		if f.String != "" && assertContains(f.String, "DEK not available") {
			sawErrorText = true
		}
		if errVal, ok := f.Interface.(error); ok && errVal != nil &&
			assertContains(errVal.Error(), "DEK not available") {
			sawErrorText = true
		}
	}
	assert.True(t, sawWorkspaceID, "log entry must include workspaceID for correlation; fields: %+v", entry.Context)
	assert.True(t, sawErrorText, "log entry must include the underlying error text; fields: %+v", entry.Context)
}

func assertContains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// TestPodBootstrap_AuthenticatedFalse_Returns401 (C1) — a token rejected by
// the apiserver (Authenticated=false) must return 401, not 500. This is a
// client error (invalid/expired/wrong-audience token), not a server fault.
func TestPodBootstrap_AuthenticatedFalse_Returns401(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "", err: errTokenNotAuthenticated}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, &fakeBootstrapLookup{})
	w := doBootstrap(t, router, "rejected-token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "Authenticated=false must be 401, not 500 (C1)")
}

// TestPodBootstrap_CrossNamespaceSA_Rejected (S1) — a valid token from a
// workspace-<id> SA in a DIFFERENT namespace must be rejected (403). An
// attacker with namespace-creation privileges must not be able to forge a
// workspace SA and extract another workspace's credentials.
func TestPodBootstrap_CrossNamespaceSA_Rejected(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:attacker-ns:workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, &fakeBootstrapInjector{}, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "cross-namespace SA must be rejected (S1)")
}
