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
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
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
	entry   *secrets.BatchEntry
	degrade *secrets.BuildDegrade
	err     error

	manifestHash   string
	manifestErr    error
	currentSeq     int64
	currentHash    string
	currentOK      bool
	currentErr     error
	buildCalled    bool
	manifestCalled bool
}

func (f *fakeBootstrapInjector) BuildWorkspaceBatch(_ context.Context, _, _ string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	f.buildCalled = true
	if f.err != nil {
		return nil, nil, f.err
	}
	batch := &secrets.Batch{}
	if f.entry != nil {
		batch.Entries = append(batch.Entries, *f.entry)
	}
	return batch, f.degrade, nil
}

func (f *fakeBootstrapInjector) ManifestFor(_ context.Context, _, _ string) (string, error) {
	f.manifestCalled = true
	return f.manifestHash, f.manifestErr
}

func (f *fakeBootstrapInjector) CurrentRevision(_ context.Context, _ string) (int64, string, bool, error) {
	return f.currentSeq, f.currentHash, f.currentOK, f.currentErr
}

// legacyOnlyInjector implements ONLY BuildWorkspaceBatch — the
// mixed-fleet injector that predates the manifest seam (no promoted
// methods: it is its own struct). A v2 request against it must still
// receive a consumable (legacy-shaped) response, without reaching for
// the absent seam.
type legacyOnlyInjector struct {
	entry *secrets.BatchEntry
}

func (f *legacyOnlyInjector) BuildWorkspaceBatch(_ context.Context, _, _ string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	batch := &secrets.Batch{}
	if f.entry != nil {
		batch.Entries = append(batch.Entries, *f.entry)
	}
	return batch, nil, nil
}

type fakeBootstrapLookup struct {
	ws  *types.WorkspaceMetadata
	err error
}

func (f *fakeBootstrapLookup) GetWorkspace(_ context.Context, _ string) (*types.WorkspaceMetadata, error) {
	return f.ws, f.err
}

const testBootstrapNamespace = "llmsafespace"

func newTestBootstrapRouter(t *testing.T, reviewer *fakeTokenReviewer, injector bootstrapInjector, lookup *fakeBootstrapLookup) *gin.Engine {
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
	injector := &fakeBootstrapInjector{entry: &secrets.BatchEntry{
		SecretID: "sec-1", Version: 1, Type: secrets.SecretTypeLLMProvider, Name: "test", Value: "k",
	}}
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
	injector := &fakeBootstrapInjector{}
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
	injector := &fakeBootstrapInjector{}
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
// the batch build fails, the handler returns a generic 500 "secret
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

// ---------------------------------------------------------------------------
// Allowed-external-directories delivery (instance setting → bootstrap response).
//
// The handler reads workspace.allowedExternalDirectories from instance settings
// and returns it in the bootstrap response so agentd can pre-approve /tmp/* (and
// other operator-configured paths) as external_directory allow-rules. This is
// the API-side half of the "stop prompting for /tmp" feature.
//
// Contracts (each test turns red if its fix is reverted):
//  1. Settings reader returns dirs → response carries them verbatim.
//  2. Settings error → response omits the field (non-fatal degradation).
//  3. No settings reader wired → response omits the field.
// ---------------------------------------------------------------------------

// fakeSettingsReader is a minimal SettingsReader for bootstrap handler tests.
type fakeSettingsReader struct {
	strings    []string
	stringsErr error
}

func (f *fakeSettingsReader) GetBool(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (f *fakeSettingsReader) GetInt(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (f *fakeSettingsReader) GetString(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeSettingsReader) GetStrings(_ context.Context, _ string) ([]string, error) {
	return f.strings, f.stringsErr
}

// TestPodBootstrap_ReturnsAllowedExternalDirectories verifies the handler
// propagates the instance setting into the response when a settings reader
// is wired and returns a value.
func TestPodBootstrap_ReturnsAllowedExternalDirectories(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	sr := &fakeSettingsReader{strings: []string{"/tmp/*", "/var/cache/*"}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(reviewer, injector, lookup, nil, testBootstrapNamespace)
	h.SetSettingsReader(sr)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, []string{"/tmp/*", "/var/cache/*"}, resp.AllowedExternalDirectories,
		"allowed-external-directories must propagate from settings into the response verbatim")
}

// TestPodBootstrap_SettingsError_OmitsField verifies a settings read failure
// is non-fatal — the response omits allowedExternalDirectories and the pod
// still boots (agents prompt for /tmp/* as before).
func TestPodBootstrap_SettingsError_OmitsField(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	sr := &fakeSettingsReader{stringsErr: errors.New("settings unavailable")}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(reviewer, injector, lookup, nil, testBootstrapNamespace)
	h.SetSettingsReader(sr)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp.AllowedExternalDirectories,
		"settings error must degrade gracefully — field omitted, boot still succeeds")
}

// TestPodBootstrap_NoSettingsReader_OmitsField verifies that without a wired
// settings reader (nil), the field is omitted. Backward-compat for handlers
// constructed before the field existed.
func TestPodBootstrap_NoSettingsReader_OmitsField(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}

	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp.AllowedExternalDirectories,
		"no settings reader wired → field must be omitted")
}

// ---------------------------------------------------------------------------
// US-70.2 Part 2: conditional pull negotiation.
//
// Contract-versioned clients (contractVersion=2) present their last
// manifest hash; unchanged → 304 (zero decrypts, ETag "<seq>:<hash>"),
// changed → 200 with secrets as the revisioned ENVELOPE. Requests
// without contract fields must be byte-shape-identical to the legacy
// response (W15 mixed-fleet pin).
// ---------------------------------------------------------------------------

func v2BatchEntry() *secrets.BatchEntry {
	return &secrets.BatchEntry{
		SecretID: "sec-1", Version: 2, Type: secrets.SecretTypeEnvSecret, Name: "db", Value: "v",
		Metadata: json.RawMessage(`{"var_name":"DB"}`),
	}
}

// TestPodBootstrap_LegacyRequest_ByteShapeIdentical pins the W15
// mixed-fleet contract: a request with NO contract fields must produce a
// byte-identical response body to today's — secrets as the legacy bare
// array, same envelope fields, same order.
func TestPodBootstrap_LegacyRequest_ByteShapeIdentical(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{entry: v2BatchEntry()}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", DefaultModel: "glm-5.2"}}
	sr := &fakeSettingsReader{strings: []string{"/tmp/*"}}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(reviewer, injector, lookup, nil, testBootstrapNamespace)
	h.SetSettingsReader(sr)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "token", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	batch := &secrets.Batch{Entries: []secrets.BatchEntry{*v2BatchEntry()}}
	want, err := json.Marshal(bootstrapAPIResponse{
		Secrets:                    secrets.LegacyBatchJSON(*batch),
		WorkspaceConfig:            rawMustJSON(t, types.WorkspaceConfig{DefaultModel: "glm-5.2"}),
		AllowedExternalDirectories: []string{"/tmp/*"},
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), w.Body.String(),
		"legacy request must render the exact legacy response shape")
	assert.False(t, injector.manifestCalled, "legacy requests never touch the manifest seam")

	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, string(secrets.LegacyBatchJSON(*batch)), string(resp.Secrets),
		"legacy secrets payload is the bare array, byte-identical")
}

// TestPodBootstrap_v2_FirstPull_ReturnsEnvelope: no client hash → 200
// with secrets as the revisioned envelope (entries + revision), other
// response fields unchanged.
func TestPodBootstrap_v2_FirstPull_ReturnsEnvelope(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{entry: v2BatchEntry(), manifestHash: "m1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", DefaultModel: "glm-5.2"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc","contractVersion":2}`)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Secrets         json.RawMessage `json:"secrets"`
		WorkspaceConfig json.RawMessage `json:"workspaceConfig"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var envelope struct {
		Entries   []secrets.BatchEntry  `json:"entries"`
		Revision  secrets.BatchRevision `json:"revision"`
		RawSecret json.RawMessage
	}
	require.NoError(t, json.Unmarshal(resp.Secrets, &envelope))
	require.Len(t, envelope.Entries, 1)
	assert.Equal(t, "sec-1", envelope.Entries[0].SecretID)
	assert.Equal(t, "db", envelope.Entries[0].Name)
	assert.Equal(t, "v", envelope.Entries[0].Value)

	var wsCfg map[string]any
	require.NoError(t, json.Unmarshal(resp.WorkspaceConfig, &wsCfg))
	assert.Equal(t, "glm-5.2", wsCfg["defaultModel"], "workspaceConfig still delivered on the v2 path")
}

// TestPodBootstrap_v2_UnchangedManifest_304: the client's hash equals
// the current manifest hash → 304, empty body, ETag "<seq>:<hash>", and
// the builder (the decrypt tier) is never invoked.
func TestPodBootstrap_v2_UnchangedManifest_304(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{
		entry:        v2BatchEntry(),
		manifestHash: "m-current",
		currentSeq:   7, currentHash: "m-current", currentOK: true,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"m-current"}`)

	require.Equal(t, http.StatusNotModified, w.Code)
	assert.Empty(t, w.Body.String(), "304 carries an empty body")
	assert.Equal(t, `"7:m-current"`, w.Header().Get("ETag"))
	assert.False(t, injector.buildCalled, "the 304 path must not build (decrypt) the batch")
	assert.True(t, injector.manifestCalled)
}

// TestPodBootstrap_v2_ChangedHash_200Envelope: a differing client hash
// (or none) gets the full envelope rebuild.
func TestPodBootstrap_v2_ChangedHash_200Envelope(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{
		entry:        v2BatchEntry(),
		manifestHash: "m-new",
		currentSeq:   8, currentHash: "m-new", currentOK: true,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"m-stale"}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, injector.buildCalled, "changed manifest must rebuild the batch")

	var envelope struct {
		Entries []secrets.BatchEntry `json:"entries"`
	}
	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NoError(t, json.Unmarshal(resp.Secrets, &envelope))
	assert.Len(t, envelope.Entries, 1)
}

// TestPodBootstrap_v2_ManifestError_500: a manifest-tier failure is the
// same 500 class as a builder failure.
func TestPodBootstrap_v2_ManifestError_500(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{manifestErr: context.DeadlineExceeded}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token", `{"workspaceID":"ws-abc","contractVersion":2}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestPodBootstrap_v2_RevisionRowError_500: the stored-revision read
// backing the ETag failing is a 500, not a fabricated ETag.
func TestPodBootstrap_v2_RevisionRowError_500(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{
		manifestHash: "m",
		currentErr:   context.DeadlineExceeded,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"m"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestPodBootstrap_v2_HashMatchesButNoRevisionRow_Builds: a client hash
// matching the rows with NO stored revision row cannot be ETagged — the
// handler must fall through to the 200 build instead of fabricating a
// seq (DB-as-single-writer).
func TestPodBootstrap_v2_HashMatchesButNoRevisionRow_Builds(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{
		entry:        v2BatchEntry(),
		manifestHash: "m",
		currentOK:    false,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"m"}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, injector.buildCalled, "no stored seq ⇒ the build path mints one")
}

// TestPodBootstrap_v2_S1Mismatch_403: identity checks precede
// negotiation — a cross-namespace SA with contract fields still 403s
// without touching the manifest seam.
func TestPodBootstrap_v2_S1Mismatch_403(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:attacker-ns:workspace-ws-abc"}
	injector := &fakeBootstrapInjector{manifestHash: "m"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"m"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, injector.manifestCalled, "S1 precedes the manifest seam")
}

// TestPodBootstrap_v2_NoManifestSource_StillEnvelope: an injector
// without the manifest seam (mixed fleet inside the API process) cannot
// 304, but the v2 render contract is unaffected — the envelope is
// served and the absent seam is never reached.
func TestPodBootstrap_v2_NoManifestSource_StillEnvelope(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:llmsafespace:workspace-ws-abc"}
	injector := &legacyOnlyInjector{entry: v2BatchEntry()}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"whatever"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var envelope struct {
		Entries []secrets.BatchEntry `json:"entries"`
	}
	var resp bootstrapAPIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NoError(t, json.Unmarshal(resp.Secrets, &envelope))
	assert.Len(t, envelope.Entries, 1, "v2 renders the envelope regardless of the 304 seam")
}

func rawMustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
