// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// mcp_secrets_resync_test.go — US-70.3 PR-4 contract tests for the
// `secrets_resync` platform MCP tool.
//
// Workspace resolution (the mechanism this file's names document): the
// platform MCP server IS agentd's in-pod /v1/mcp endpoint — the entry
// injected into agent-config.json as "llmsafespaces" points at
// http://127.0.0.1:<AgentdPort>/v1/mcp (injectAgentdMCPServer), gated
// by the workspace password (#847). Tools therefore execute INSIDE the
// workspace they serve: secrets_resync resolves its workspace by pod
// identity (WORKSPACE_ID / the pod itself), exactly like dev_preview_url,
// and triggers the workspace's OWN in-process conditional resync by
// loopback POST /v1/resync-secrets — the endpoint server.go mounts as
// "the notify-pull target ... and the secrets_resync MCP tool call[s]
// this". No session, no API-side hop: the pull the resync performs
// recomputes the manifest + mints drift server-side (conditional
// bootstrap) and applies it with the pod's rate limit respected.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resyncRecorder stands in for the pod's own /v1/resync-secrets endpoint.
type resyncRecorder struct {
	mu      sync.Mutex
	calls   []recordedResyncCall
	handler func(w http.ResponseWriter)
	srv     *httptest.Server
}

type recordedResyncCall struct {
	path    string
	auth    string
	bodyLen int
}

func startResyncRecorder(t *testing.T, handler func(w http.ResponseWriter)) *resyncRecorder {
	t.Helper()
	rec := &resyncRecorder{handler: handler}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, recordedResyncCall{
			path:    r.URL.Path,
			auth:    r.Header.Get("Authorization"),
			bodyLen: len(body),
		})
		rec.mu.Unlock()
		handler(w)
	}))
	t.Cleanup(rec.srv.Close)
	old := setResyncBaseURLForTest(rec.srv.URL)
	t.Cleanup(func() { setResyncBaseURLForTest(old) })
	return rec
}

func (r *resyncRecorder) dispatched() []recordedResyncCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]recordedResyncCall, len(r.calls))
	copy(cp, r.calls)
	return cp
}

// setResyncBaseURLForTest points the tool's loopback target at a test
// server and returns the previous value for restoration.
func setResyncBaseURLForTest(url string) string {
	old := resyncBaseURL()
	resyncBaseURLAtomic.Store(url)
	return old
}

// callResyncTool drives the MCP tool layer (the dispatcher the JSON-RPC
// tools/call method delegates to).
func callResyncTool(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	return callMCPTool(context.Background(), mcpTestPassword, "secrets_resync", args)
}

func decodeToolJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), "tool result must be a JSON object: %q", out)
	return m
}

// --- registration shape ---

// TestSecretsResync_Registration_NoInputSchema pins the 0050 finding-3
// integrity rule at the schema surface: the tool declares NO input
// properties and requires nothing — it must never accept credential
// material.
func TestSecretsResync_Registration_NoInputSchema(t *testing.T) {
	req, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	w := httptest.NewRecorder()
	r := mcpAuthedRequest(req)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)

	var tool map[string]any
	for _, raw := range tools {
		candidate := raw.(map[string]any)
		if candidate["name"] == "secrets_resync" {
			tool = candidate
		}
	}
	require.NotNil(t, tool, "secrets_resync must be registered in tools/list")

	schema := tool["inputSchema"].(map[string]any)
	props, ok := schema["properties"].(map[string]any)
	if ok {
		assert.Empty(t, props, "secrets_resync must declare NO input properties")
	}
	if required, present := schema["required"]; present {
		assert.Empty(t, required, "secrets_resync must require nothing")
	}
	assert.Equal(t, "object", schema["type"])
}

// TestSecretsResync_DescriptionGuidance pins the description contract
// (the only documentation an agent ever sees — same doctrine as
// TestMCPHandler_ToolDescriptionGuidance).
func TestSecretsResync_DescriptionGuidance(t *testing.T) {
	req, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(req))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)
	var desc string
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "secrets_resync" {
			desc = tool["description"].(string)
		}
	}
	require.NotEmpty(t, desc)
	for _, want := range []string{
		"missing or stale", // the trigger tonight's incident taught (#443 class)
		"never accepts",    // finding-3: no credential material, ever
		"credential",       // named explicitly
		"converged",        // the outcome field
		"retryAfterMs",     // rate-limit contract
	} {
		assert.Contains(t, desc, want)
	}
}

// --- behavior ---

// TestSecretsResync_ResolvesWorkspaceByPodIdentity_ReportsAppliedRev is
// the AC-11 happy path: the tool triggers THIS pod's resync (loopback,
// empty body, workspace-password Basic auth) and reports the revision
// the pod APPLIED.
func TestSecretsResync_ResolvesWorkspaceByPodIdentity_ReportsAppliedRev(t *testing.T) {
	rec := startResyncRecorder(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"7:mh-7:ch-7","restarted":false}`))
	})

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)

	calls := rec.dispatched()
	require.Len(t, calls, 1, "exactly one loopback resync dispatch")
	assert.Equal(t, "/v1/resync-secrets", calls[0].path, "the dispatch targets the pod's resync endpoint")
	assert.Equal(t, 0, calls[0].bodyLen, "the resync request carries NO body — the pod pulls from the API itself")
	assert.Equal(t, "Basic "+basicAuth(mcpTestPassword), calls[0].auth,
		"the loopback dispatch authenticates with the workspace password (§D1)")

	body := decodeToolJSON(t, out)
	assert.Equal(t, "applied", body["status"])
	assert.Equal(t, "7:mh-7:ch-7", body["appliedRev"])
	assert.Equal(t, true, body["converged"])
}

// TestSecretsResync_NotModifiedIsConverged: a 304-shaped no-op is
// convergence, reported with the pod's certified anchor.
func TestSecretsResync_NotModifiedIsConverged(t *testing.T) {
	startResyncRecorder(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"not_modified","appliedRev":"3:mh-3"}`))
	})

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)
	body := decodeToolJSON(t, out)
	assert.Equal(t, "not_modified", body["status"])
	assert.Equal(t, "3:mh-3", body["appliedRev"])
	assert.Equal(t, true, body["converged"])
}

// TestSecretsResync_ExtraArgumentsIgnored_NeverCredentialMaterial pins
// what the hand-rolled MCP dispatcher does with undeclared arguments:
// there is no schema-validation framework in agentd's JSON-RPC layer,
// so extra args are IGNORED — the tool reads none of them, and the
// dispatch stays a credential-free empty POST (0050 finding-3: a batch
// body can never be injected or synthesized through the tool).
func TestSecretsResync_ExtraArgumentsIgnored_NeverCredentialMaterial(t *testing.T) {
	rec := startResyncRecorder(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"not_modified","appliedRev":"1:mh-1"}`))
	})

	out, err := callResyncTool(t, map[string]any{
		"batch":   `{"entries":[{"plaintext":"sk-stolen"}]}`,
		"body":    `{"secrets":{"API_KEY":"sk-stolen"}}`,
		"secrets": []any{"API_KEY=sk-stolen"},
	})
	require.NoError(t, err, "extra arguments are ignored, never an error")

	calls := rec.dispatched()
	require.Len(t, calls, 1)
	assert.Equal(t, 0, calls[0].bodyLen, "no argument content may reach the resync dispatch")
	body := decodeToolJSON(t, out)
	assert.Equal(t, true, body["converged"])
}

// TestSecretsResync_RateLimitedRefusalIs429Shaped: the tool shares the
// per-workspace resync floor (I15) — the endpoint's 429 becomes a
// refusal carrying the retry budget.
func TestSecretsResync_RateLimitedRefusalIs429Shaped(t *testing.T) {
	startResyncRecorder(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited","retryAfterMs":1500}`))
	})

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err, "a rate-limited refusal is a tool result, not a tool error")
	body := decodeToolJSON(t, out)
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, float64(1500), body["retryAfterMs"])
}

// TestSecretsResync_RateLimitedWithoutRetryFieldFallsBackToFloor: an
// endpoint 429 without a retry hint falls back to the configured
// minimum interval (same process, same env).
func TestSecretsResync_RateLimitedWithoutRetryFieldFallsBackToFloor(t *testing.T) {
	startResyncRecorder(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":"rate_limited"}`))
	})

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)
	body := decodeToolJSON(t, out)
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, float64(resyncMinIntervalFromEnv().Milliseconds()), body["retryAfterMs"])
}

// TestSecretsResync_PlatformUnreachable_ConvergedFalseExpectedRevNoError
// is the pull-failure path (the unreachable-endpoint analog of the
// notify model): converged=false, the revision this pod still stands at
// is reported as expectedRev with pending=true, and the tool returns NO
// error — the reconcile loop converges the workspace later (I3).
func TestSecretsResync_PlatformUnreachable_ConvergedFalseExpectedRevNoError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", filepath.Join(dir, "secrets-env"))
	anchorPath := revAnchorPath(filepath.Join(dir, "secrets-env"))
	require.NoError(t, os.WriteFile(anchorPath, []byte(`{"rev":"5:mh-5","appliedSeq":5}`), 0o600))

	startResyncRecorder(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"failed","reason":"pull_failed"}`))
	})

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err, "a failed pull is a report, never a tool error")

	body := decodeToolJSON(t, out)
	assert.Equal(t, false, body["converged"])
	assert.Equal(t, "5:mh-5", body["expectedRev"],
		"the pod's anchored rev is the standing expectation until the server is reachable")
	assert.Equal(t, true, body["pending"])
	assert.Equal(t, "failed", body["status"])
	assert.Equal(t, "pull_failed", body["reason"])
}

// TestSecretsResync_TransportFailure_ConvergedFalseNoError: the
// endpoint itself unreachable degrades the same way as a failed pull.
func TestSecretsResync_TransportFailure_ConvergedFalseNoError(t *testing.T) {
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", filepath.Join(t.TempDir(), "secrets-env"))
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "shutdown", http.StatusInternalServerError)
	}))
	dead.Close() // immediately unreachable
	old := setResyncBaseURLForTest(dead.URL)
	t.Cleanup(func() { setResyncBaseURLForTest(old) })

	out, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)
	body := decodeToolJSON(t, out)
	assert.Equal(t, false, body["converged"])
	assert.Equal(t, true, body["pending"])
	assert.NotEmpty(t, body["reason"], "the failure carries a machine-readable reason")
}

// TestSecretsResync_SharesPerWorkspaceResyncFloor drives the REAL
// resync endpoint (I15 limiter included) behind the tool: two rapid
// tool calls → the first is admitted, the second refused 429-shaped.
func TestSecretsResync_SharesPerWorkspaceResyncFloor(t *testing.T) {
	env := newResyncTestEnv(t, &conditionalServer{status: http.StatusNotModified})
	handler := resyncSecretsHandler(resyncDeps{
		cfg:    env.cfg,
		apply:  applySecretsDeps{OpencodePassword: mcpTestPassword},
		apiURL: env.apiSrv.URL, workspaceID: "ws",
		tokenPath: filepath.Join(env.dir, "token"), batchPath: filepath.Join(env.dir, "secrets.json"),
		minInterval: time.Hour, // deterministic: anything within the hour is refused
		now:         env.fakeClock.t,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := setResyncBaseURLForTest(srv.URL)
	t.Cleanup(func() { setResyncBaseURLForTest(old) })

	first, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, true, decodeToolJSON(t, first)["converged"])

	second, err := callResyncTool(t, map[string]any{})
	require.NoError(t, err)
	body := decodeToolJSON(t, second)
	assert.Equal(t, "rate_limited", body["error"], "the loop-spamming agent gets the 429-shaped refusal")
	assert.Positive(t, body["retryAfterMs"].(float64))
}

// TestSecretsResync_FullJSONRPCDispatch pins the registration wiring:
// tools/call over the real mcpHandler reaches the tool.
func TestSecretsResync_FullJSONRPCDispatch(t *testing.T) {
	startResyncRecorder(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"2:mh-2:ch-2","restarted":true}`))
	})

	params, _ := json.Marshal(map[string]any{"name": "secrets_resync", "arguments": map[string]any{}})
	reqBody, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 77, Method: "tools/call", Params: params})
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(reqBody))

	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.NotEqual(t, true, result["isError"], "the call must not be an MCP error")

	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	body := decodeToolJSON(t, text)
	assert.Equal(t, "2:mh-2:ch-2", body["appliedRev"])
	assert.Equal(t, true, body["converged"])
}

// TestSecretsResync_BoundedWait pins that the loopback wait is bounded
// (≤ resync budget + margin): a hung endpoint cannot wedge the agent's
// tool call forever.
func TestSecretsResync_BoundedWait(t *testing.T) {
	release := make(chan struct{})
	rec := startResyncRecorder(t, func(w http.ResponseWriter) {
		<-release
		_, _ = w.Write([]byte(`{"status":"not_modified","appliedRev":"1:mh-1"}`))
	})
	_ = rec

	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = callResyncTool(t, map[string]any{})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the tool must not return while the endpoint hangs")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool must return once the endpoint responds")
	}
	require.NoError(t, err)
	assert.Equal(t, true, decodeToolJSON(t, out)["converged"])
}
