// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// mockEnvService is a minimal WorkspaceEnvService fake for unit testing.
// Each method stores call arguments and returns caller-configured results.
type mockEnvService struct {
	secrets        map[string]*secrets.SecretResponse // keyed by name
	bindings       map[string][]secrets.BoundSecret   // keyed by workspaceID
	nextID         int
	createErr      error
	updateErr      error
	deleteErr      error
	getByNameErr   error
	addBindingsErr error
	getBindingsErr error

	lastCreateReq    secrets.CreateSecretRequest
	lastUpdateID     string
	lastUpdateReq    secrets.UpdateSecretRequest
	lastDeleteID     string
	lastBindWks      string
	lastBindIDs      []string
	createCallCount  int
	updateCallCount  int
	deleteCallCount  int
	addBindCallCount int
}

func newMockEnvService() *mockEnvService {
	return &mockEnvService{
		secrets:  make(map[string]*secrets.SecretResponse),
		bindings: make(map[string][]secrets.BoundSecret),
	}
}

func (m *mockEnvService) GetSecretByName(_ context.Context, _, name string) (*secrets.SecretResponse, error) {
	if m.getByNameErr != nil {
		return nil, m.getByNameErr
	}
	return m.secrets[name], nil
}

func (m *mockEnvService) CreateSecret(_ context.Context, _, _ string, _ []byte, req secrets.CreateSecretRequest) (*secrets.SecretResponse, error) {
	m.createCallCount++
	m.lastCreateReq = req
	if m.createErr != nil {
		return nil, m.createErr
	}
	m.nextID++
	resp := &secrets.SecretResponse{
		ID:   "secret-" + itoa(m.nextID),
		Name: req.Name,
		Type: req.Type,
	}
	m.secrets[req.Name] = resp
	return resp, nil
}

func (m *mockEnvService) UpdateSecret(_ context.Context, _, _ string, _ []byte, secretID string, req secrets.UpdateSecretRequest) error {
	m.updateCallCount++
	m.lastUpdateID = secretID
	m.lastUpdateReq = req
	return m.updateErr
}

func (m *mockEnvService) DeleteSecret(_ context.Context, _, secretID string) error {
	m.deleteCallCount++
	m.lastDeleteID = secretID
	for name, s := range m.secrets {
		if s.ID == secretID {
			delete(m.secrets, name)
		}
	}
	return m.deleteErr
}

func (m *mockEnvService) AddBindings(_ context.Context, _, workspaceID string, secretIDs []string) (secrets.BindingsMutationResult, error) {
	m.addBindCallCount++
	m.lastBindWks = workspaceID
	m.lastBindIDs = secretIDs
	if m.addBindingsErr != nil {
		return secrets.BindingsMutationResult{}, m.addBindingsErr
	}
	return secrets.BindingsMutationResult{}, nil
}

func (m *mockEnvService) GetBindings(_ context.Context, _, workspaceID string) (*secrets.BindingsResponse, error) {
	if m.getBindingsErr != nil {
		return nil, m.getBindingsErr
	}
	return &secrets.BindingsResponse{Bindings: m.bindings[workspaceID]}, nil
}

// itoa is a minimal int→string to avoid importing strconv in the mock.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// setupEnvRouter creates a Gin engine with WorkspaceEnvHandler wired to
// the three env routes, using a mock auth middleware that injects a fixed
// userID + sessionID into the gin context.
func setupEnvRouter(svc WorkspaceEnvService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "sess-1")
		c.Next()
	})
	h := NewWorkspaceEnvHandler(svc)
	r.PUT("/workspaces/:id/env", h.SetWorkspaceEnv)
	r.GET("/workspaces/:id/env", h.GetWorkspaceEnv)
	r.DELETE("/workspaces/:id/env/:name", h.DeleteWorkspaceEnv)
	return r
}

// doEnvRequest is a test helper for the env tests (wraps httptest).
// The package-level doRequest in orgs_test.go has a different body-arg
// convention; this one is self-contained for these table-driven tests.
func doEnvRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- SetWorkspaceEnv ---

func TestSetWorkspaceEnv_CreatesNewVars(t *testing.T) {
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar","BAZ":"qux"}}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d (body=%s)", w.Code, w.Body.String())
	}
	if svc.createCallCount != 2 {
		t.Errorf("create calls: want 2, got %d", svc.createCallCount)
	}
	if svc.addBindCallCount != 1 {
		t.Errorf("addBindings calls: want 1, got %d", svc.addBindCallCount)
	}
	if len(svc.lastBindIDs) != 2 {
		t.Errorf("bind IDs: want 2, got %d", len(svc.lastBindIDs))
	}
	if svc.lastBindWks != "ws-1" {
		t.Errorf("bind workspace: want ws-1, got %s", svc.lastBindWks)
	}
}

func TestSetWorkspaceEnv_UpdatesExistingVar(t *testing.T) {
	svc := newMockEnvService()
	svc.secrets["ws-1-env-foo"] = &secrets.SecretResponse{ID: "existing-1", Name: "ws-1-env-foo"}
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"new-value"}}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d (body=%s)", w.Code, w.Body.String())
	}
	if svc.createCallCount != 0 {
		t.Errorf("create calls: want 0 (should update), got %d", svc.createCallCount)
	}
	if svc.updateCallCount != 1 {
		t.Fatalf("update calls: want 1, got %d", svc.updateCallCount)
	}
	if svc.lastUpdateID != "existing-1" {
		t.Errorf("update ID: want existing-1, got %s", svc.lastUpdateID)
	}
	if svc.lastUpdateReq.Value != "new-value" {
		t.Errorf("update value: want new-value, got %s", svc.lastUpdateReq.Value)
	}
}

func TestSetWorkspaceEnv_MixedCreateAndUpdate(t *testing.T) {
	svc := newMockEnvService()
	svc.secrets["ws-1-env-existing"] = &secrets.SecretResponse{ID: "old-1", Name: "ws-1-env-existing"}
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"EXISTING":"v1","NEWVAR":"v2"}}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d (body=%s)", w.Code, w.Body.String())
	}
	if svc.updateCallCount != 1 {
		t.Errorf("update calls: want 1, got %d", svc.updateCallCount)
	}
	if svc.createCallCount != 1 {
		t.Errorf("create calls: want 1, got %d", svc.createCallCount)
	}
}

func TestSetWorkspaceEnv_InvalidJSON(t *testing.T) {
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", `{"vars":}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", w.Code)
	}
}

func TestSetWorkspaceEnv_EmptyVarsMap_NoOpSuccess(t *testing.T) {
	// An empty (non-nil) vars map satisfies binding:"required" and is a
	// valid no-op: no secrets to create, AddBindings with empty slice.
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", `{"vars":{}}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: want 204 (empty vars is a no-op), got %d", w.Code)
	}
	if svc.createCallCount != 0 {
		t.Errorf("create calls: want 0, got %d", svc.createCallCount)
	}
}

func TestSetWorkspaceEnv_CreateFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.createErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar"}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "FOO") {
		t.Errorf("error should include failing var name, got: %s", w.Body.String())
	}
}

func TestSetWorkspaceEnv_UpdateFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.secrets["ws-1-env-foo"] = &secrets.SecretResponse{ID: "ex-1", Name: "ws-1-env-foo"}
	svc.updateErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar"}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestSetWorkspaceEnv_GetByNameFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.getByNameErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar"}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestSetWorkspaceEnv_AddBindingsFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.addBindingsErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar"}}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestSetWorkspaceEnv_NoAuth_Returns401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWorkspaceEnvHandler(newMockEnvService())
	r.PUT("/workspaces/:id/env", h.SetWorkspaceEnv)

	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env",
		`{"vars":{"FOO":"bar"}}`)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
}

// --- GetWorkspaceEnv ---

func TestGetWorkspaceEnv_ReturnsEnvVarNamesOnly(t *testing.T) {
	svc := newMockEnvService()
	svc.bindings["ws-1"] = []secrets.BoundSecret{
		{SecretID: "s1", Name: "ws-1-env-database_url", Type: secrets.SecretTypeEnvSecret},
		{SecretID: "s2", Name: "ws-1-env-api_key", Type: secrets.SecretTypeEnvSecret},
		{SecretID: "s3", Name: "my-git-token", Type: secrets.SecretTypeGitCredential},
	}
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "GET", "/workspaces/ws-1/env", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	var resp struct {
		Vars []string `json:"vars"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Vars) != 2 {
		t.Fatalf("vars: want 2 (env-secrets only), got %d (%v)", len(resp.Vars), resp.Vars)
	}
}

func TestGetWorkspaceEnv_NoBindings_ReturnsEmptyArray(t *testing.T) {
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "GET", "/workspaces/ws-1/env", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	var resp struct {
		Vars []string `json:"vars"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Vars == nil || len(resp.Vars) != 0 {
		t.Errorf("vars: want non-nil empty array, got %v", resp.Vars)
	}
}

func TestGetWorkspaceEnv_GetBindingsFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.getBindingsErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "GET", "/workspaces/ws-1/env", "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestGetWorkspaceEnv_NoAuth_Returns401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWorkspaceEnvHandler(newMockEnvService())
	r.GET("/workspaces/:id/env", h.GetWorkspaceEnv)

	w := doEnvRequest(r, "GET", "/workspaces/ws-1/env", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
}

// --- DeleteWorkspaceEnv ---

func TestDeleteWorkspaceEnv_Success(t *testing.T) {
	svc := newMockEnvService()
	svc.secrets["ws-1-env-foo"] = &secrets.SecretResponse{ID: "s-1", Name: "ws-1-env-foo"}
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "DELETE", "/workspaces/ws-1/env/foo", "")

	if w.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", w.Code)
	}
	if svc.deleteCallCount != 1 {
		t.Fatalf("delete calls: want 1, got %d", svc.deleteCallCount)
	}
	if svc.lastDeleteID != "s-1" {
		t.Errorf("delete ID: want s-1, got %s", svc.lastDeleteID)
	}
}

func TestDeleteWorkspaceEnv_NotFound_Returns404(t *testing.T) {
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "DELETE", "/workspaces/ws-1/env/nonexistent", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", w.Code)
	}
}

func TestDeleteWorkspaceEnv_GetByNameFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.getByNameErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "DELETE", "/workspaces/ws-1/env/foo", "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestDeleteWorkspaceEnv_DeleteFails_Returns500(t *testing.T) {
	svc := newMockEnvService()
	svc.secrets["ws-1-env-foo"] = &secrets.SecretResponse{ID: "s-1", Name: "ws-1-env-foo"}
	svc.deleteErr = errors.New("db down")
	r := setupEnvRouter(svc)

	w := doEnvRequest(r, "DELETE", "/workspaces/ws-1/env/foo", "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500, got %d", w.Code)
	}
}

func TestDeleteWorkspaceEnv_NoAuth_Returns401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWorkspaceEnvHandler(newMockEnvService())
	r.DELETE("/workspaces/:id/env/:name", h.DeleteWorkspaceEnv)

	w := doEnvRequest(r, "DELETE", "/workspaces/ws-1/env/foo", "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", w.Code)
	}
}

// --- Compile-time interface conformance ---

// Compile-time proof that *secrets.SecretService satisfies
// WorkspaceEnvService. If the interface drifts, this fails at build time.
var _ WorkspaceEnvService = (*secrets.SecretService)(nil)

// --- G37: env-var name blocklist ---

// TestSetWorkspaceEnv_RejectsBlockedNames verifies that the API rejects
// dangerous env-var names BEFORE touching the secret store. The blocklist
// (LD_PRELOAD, PATH, PYTHONPATH, etc.) prevents a workspace owner from
// setting vars that would compromise every process spawned in the pod.
//
// Regression: before G37 the handler accepted these names verbatim.
// Setting LD_PRELOAD would cause every subsequent exec in the pod to load
// the attacker's .so; setting PATH would redirect opencode/git/ssh
// lookups; setting BASH_ENV would source an attacker-controlled file on
// every non-interactive bash invocation. The pod's single UID means
// this is container-escape-equivalent in practice.
func TestSetWorkspaceEnv_RejectsBlockedNames(t *testing.T) {
	blocked := []string{
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"PATH",
		"PYTHONPATH",
		"NODE_OPTIONS",
		"BASH_ENV",
		"IFS",
		"HOME",
		"RUBYOPT",
		"PERL5OPT",
		"JAVA_TOOL_OPTIONS",
		"DYLD_INSERT_LIBRARIES",
	}
	for _, name := range blocked {
		t.Run(name, func(t *testing.T) {
			svc := newMockEnvService()
			r := setupEnvRouter(svc)

			body := `{"vars":{"` + name + `":"evil-value"}}`
			w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("blocked name %q: want 400, got %d (body=%s)", name, w.Code, w.Body.String())
			}
			if svc.createCallCount != 0 {
				t.Errorf("blocked name %q: create was called %d times; want 0 (reject before store)", name, svc.createCallCount)
			}
			if svc.updateCallCount != 0 {
				t.Errorf("blocked name %q: update was called %d times; want 0", name, svc.updateCallCount)
			}
		})
	}
}

// TestSetWorkspaceEnv_RejectsBlockedNamesCaseInsensitive confirms the
// blocklist is case-insensitive (ld.so accepts ld_preload on some
// glibc versions; the gate must catch the lowercase form too).
func TestSetWorkspaceEnv_RejectsBlockedNamesCaseInsensitive(t *testing.T) {
	for _, name := range []string{"ld_preload", "Path", "pythonpath", "node_options"} {
		t.Run(name, func(t *testing.T) {
			svc := newMockEnvService()
			r := setupEnvRouter(svc)
			body := `{"vars":{"` + name + `":"evil-value"}}`
			w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("case-insensitive blocklist miss for %q: want 400, got %d", name, w.Code)
			}
		})
	}
}

// TestSetWorkspaceEnv_RejectsInvalidPOSIXNames covers the regex half
// (non-blocklist rejections). A name that doesn't match
// [A-Za-z_][A-Za-z0-9_]* is rejected with the same 400 shape.
func TestSetWorkspaceEnv_RejectsInvalidPOSIXNames(t *testing.T) {
	for _, name := range []string{"1STARTS_WITH_DIGIT", "HAS-SPACE", "HAS.DOT", "HAS SPACE"} {
		t.Run(name, func(t *testing.T) {
			svc := newMockEnvService()
			r := setupEnvRouter(svc)
			body := `{"vars":{"` + name + `":"value"}}`
			w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("invalid POSIX name %q: want 400, got %d", name, w.Code)
			}
		})
	}
}

// TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply confirms the
// fail-fast contract: if ANY name in the batch is invalid, the entire
// request is rejected with no writes to the secret store. Without this
// invariant a user who typos one name in a 10-var batch would silently
// create 9 secrets and have to figure out which one was rejected.
func TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply(t *testing.T) {
	svc := newMockEnvService()
	r := setupEnvRouter(svc)

	body := `{"vars":{"FOO":"ok","LD_PRELOAD":"evil","BAR":"also-ok"}}`
	w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", body)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("mixed batch with one bad name: want 400, got %d (body=%s)", w.Code, w.Body.String())
	}
	if svc.createCallCount != 0 {
		t.Errorf("partial apply: create was called %d times; want 0 (atomic reject)", svc.createCallCount)
	}
	if svc.addBindCallCount != 0 {
		t.Errorf("partial apply: addBindings was called %d times; want 0", svc.addBindCallCount)
	}
}

// TestSetWorkspaceEnv_AcceptsLocaleNames confirms locale env vars are
// NOT on the blocklist. LANG, LC_ALL, TZ, etc. are commonly set for
// legitimate localization and don't execute code. Regression guard.
func TestSetWorkspaceEnv_AcceptsLocaleNames(t *testing.T) {
	for _, name := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TZ", "LANGUAGE"} {
		t.Run(name, func(t *testing.T) {
			svc := newMockEnvService()
			r := setupEnvRouter(svc)
			body := `{"vars":{"` + name + `":"en_US.UTF-8"}}`
			w := doEnvRequest(r, "PUT", "/workspaces/ws-1/env", body)
			if w.Code != http.StatusNoContent {
				t.Errorf("locale name %q: want 204, got %d (body=%s)", name, w.Code, w.Body.String())
			}
		})
	}
}
