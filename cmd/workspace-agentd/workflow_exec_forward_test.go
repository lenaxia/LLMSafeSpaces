// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workflow_exec_forward_test.go — #1878 sidecar-mode pins: a script
// node dispatched at the workflow mux executes in the WORKSPACE
// container (forwarded over the control socket to the supervisor's
// session-env engine), failures ride the error envelope loudly, and a
// dead supervisor is script_env_unavailable naming the control socket —
// never a silent success.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The exact failed shape from #1878's evidence (the one-node python
// workflow that died on script_env_unavailable in the sidecar),
// executed against a forwarded mux: the script must now run in the
// supervisor's session environment and its sentinel must land.
func TestWorkflowExecute_ScriptNodeForwardedToWorkspaceEnv(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh — shim executors unavailable")
	}
	sentinelDir, _, delta := scriptExecTestEnv(t)

	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	srv.scriptExec = newScriptExecEngine(func() []string { return productionChildEnv(delta) })
	go srv.serve()
	fwd := (&controlClient{addr: srv.addr(), timeout: 2 * time.Second, version: controlProtocolVersion}).ScriptExec

	handler := `import json, os
def handler(input):
    with open(os.path.join(os.environ["SENTINEL_DIR"], "sentinel-mux-1878.log"), "w") as f:
        f.write("fired\n")
    return {"ran": True, "token": bool(os.environ.get("GH_TOKEN"))}
`
	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":` + jsonString(t, handler) + `},"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandlerForward(fwd, mcpTestPassword)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handler status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Output    json.RawMessage `json:"output"`
		ErrorCode string          `json:"errorCode"`
		Detail    string          `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("forwarded script node must execute in the workspace env (#1878), got %s: %s", resp.ErrorCode, resp.Detail)
	}
	if _, err := os.Stat(filepath.Join(sentinelDir, "sentinel-mux-1878.log")); err != nil {
		t.Fatalf("the sentinel must land on the workspace filesystem through the forwarded path: %v", err)
	}
	if !strings.Contains(string(resp.Output), `"ran":true`) {
		t.Fatalf("unexpected output: %s", string(resp.Output))
	}
}

// Loud failures through the forwarded path: the supervisor's
// script_failed (exit N + stderr) reaches the workflow error envelope
// verbatim — the silent-delivered regression class cannot recur.
func TestWorkflowExecute_ScriptForwardMapsFailureLoudly(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	srv.scriptExec = newScriptExecEngine(func() []string { return nil })
	go srv.serve()
	fwd := (&controlClient{addr: srv.addr(), timeout: 2 * time.Second, version: controlProtocolVersion}).ScriptExec

	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":"def handler(input):\n    raise RuntimeError('probe-boom-1878')"},"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandlerForward(fwd, mcpTestPassword)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("error codes ride HTTP 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ErrorCode string `json:"errorCode"`
		Detail    string `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != "script_failed" {
		t.Fatalf("expected script_failed, got %q (%q)", resp.ErrorCode, resp.Detail)
	}
	if !strings.Contains(resp.Detail, "probe-boom-1878") {
		t.Fatalf("the script's stderr must surface in the detail (loud failure), got %q", resp.Detail)
	}
}

// A dead supervisor (sidecar up, supervisor restarting) is a LOUD
// terminal script_env_unavailable naming the control socket — the
// #1878 silent-delivered class, closed.
func TestWorkflowExecute_ScriptForwardTransportFailureLoud(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	fwd := (&controlClient{addr: addr, timeout: 500 * time.Millisecond, version: controlProtocolVersion}).ScriptExec

	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":"def handler(input):\n    return {}"},"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	w := httptest.NewRecorder()
	workflowExecuteHandlerForward(fwd, mcpTestPassword)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("error codes ride HTTP 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ErrorCode string `json:"errorCode"`
		Detail    string `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != "script_env_unavailable" {
		t.Fatalf("transport failure must surface script_env_unavailable, got %q (%q)", resp.ErrorCode, resp.Detail)
	}
	if !strings.Contains(resp.Detail, "control socket") || !strings.Contains(resp.Detail, "workspace container") {
		t.Fatalf("the detail must name the control socket and the workspace container, got %q", resp.Detail)
	}
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal handler: %v", err)
	}
	return string(b)
}

// The forwarded path honors the mux's per-request timeout: the
// supervisor's script_timeout maps to the same 504 script_timeout
// envelope the local path uses (executor parity).
func TestWorkflowExecute_ScriptForwardTimeoutMaps504(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	srv.scriptExec = &scriptExecEngine{childEnv: func() []string { return nil }, exec: (&fakeScriptExec{hangCtx: true}).exec}
	go srv.serve()
	fwd := (&controlClient{addr: srv.addr(), timeout: 2 * time.Second, version: controlProtocolVersion}).ScriptExec

	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":"def handler(input):\n    return {}"},"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	req.SetBasicAuth("opencode", mcpTestPassword)
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	workflowExecuteHandlerForward(fwd, mcpTestPassword)(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("timeout must map to 504, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "script_timeout") {
		t.Fatalf("expected script_timeout envelope, got %s", w.Body.String())
	}
}
