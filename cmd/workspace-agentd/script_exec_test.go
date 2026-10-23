// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// script_exec_test.go — #1878 born-green pins for the new seams: the
// supervisor-side script_exec engine (scripts run in the WORKSPACE
// container's session environment), the control-socket method, and the
// sidecar mux's forwarding path.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/workflows/scriptwrap"
)

// fakeScriptExec is the exec seam substitute: scripts the (output,
// stderr, exitCode, err) tuple without touching an interpreter — for
// rows about MAPPING, not execution.
type fakeScriptExec struct {
	out     json.RawMessage
	stderr  string
	exit    int
	err     error
	hangCtx bool
	envSeen []string
}

func (f *fakeScriptExec) exec(ctx context.Context, _ scriptwrap.Language, _ string, _ any, env []string) (json.RawMessage, string, int, error) {
	f.envSeen = env
	if f.hangCtx {
		<-ctx.Done()
		return nil, "", -1, context.DeadlineExceeded
	}
	return f.out, f.stderr, f.exit, f.err
}

// scriptExecTestEnv builds the PRODUCTION child-env construction aimed
// at a test scratch dir: a secrets-env file (the #1455 override seam)
// plus a spawn delta (the sidecar-mux pull the supervisor composes in
// production). The returned sentinel dir stands in for the workspace
// filesystem.
func scriptExecTestEnv(t *testing.T) (sentinelDir, shimDir string, delta map[string]string) {
	t.Helper()
	sentinelDir = t.TempDir()
	shimDir = t.TempDir()

	// The credential-belt simulation: gh + app-git-token shims on PATH.
	// buildEnvFrom preserves the parent PATH, so a test-process PATH
	// prepend reaches the script child exactly as the image's mise/PATH
	// layout does in production.
	for _, tool := range []string{"gh", "app-git-token"} {
		p := filepath.Join(shimDir, tool)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho shim\n"), 0o755); err != nil {
			t.Fatalf("write %s shim: %v", tool, err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The secrets-env file (single-container coordinate) — what
	// buildEnvFrom sources.
	secrets := t.TempDir()
	secretPath := filepath.Join(secrets, "secrets-env")
	if err := os.WriteFile(secretPath, []byte("WT1878_SECRETS_PROBE=file-sourced\n"), 0o600); err != nil {
		t.Fatalf("write secrets-env: %v", err)
	}
	t.Setenv("LLMSAFESPACES_SECRETS_ENV_PATH", secretPath)

	// The spawn delta (sidecar-mux pull coordinate) — what
	// parentPlusDelta composes on top in the supervisor.
	delta = map[string]string{
		"WT1878_DELTA_PROBE": "delta-sourced",
		"GH_TOKEN":           "ghp-test-1878",
		"SENTINEL_DIR":       sentinelDir,
	}
	return sentinelDir, shimDir, delta
}

// productionChildEnv is the supervisor's script_child construction,
// mirrored here so the row exercises the REAL composition order.
func productionChildEnv(delta map[string]string) []string {
	return parentPlusDelta(opencodeBaseEnv(), delta)
}

// The #1878 probe scenario at the code level: a dispatched script runs
// in the workspace container's SESSION environment — the sentinel file
// LANDS on the filesystem, the interpreter resolves, and the gh/belt
// credential helpers resolve via PATH (shimmed) + env (delta-sourced
// GH_TOKEN), exactly as they would for a session bash child.
func TestScriptExecEngine_WorkspaceEnvSentinelAndBelt(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh — shim executors unavailable")
	}
	sentinelDir, shimDir, delta := scriptExecTestEnv(t)

	engine := newScriptExecEngine(func() []string { return productionChildEnv(delta) })
	handler := `import json, os, shutil
def handler(input):
    with open(os.path.join(os.environ["SENTINEL_DIR"], "sentinel-1878.log"), "w") as f:
        f.write("sentinel\n")
    return {
        "secretsProbe": os.environ.get("WT1878_SECRETS_PROBE"),
        "deltaProbe": os.environ.get("WT1878_DELTA_PROBE"),
        "gh": shutil.which("gh"),
        "belt": shutil.which("app-git-token"),
        "ghToken": bool(os.environ.get("GH_TOKEN")),
    }
`
	outcome, werr := engine.run(context.Background(), map[string]any{
		"language": "python", "handler": handler, "timeout_ms": 30000,
	})
	if werr != nil {
		t.Fatalf("script failed: %s: %s", werr.Code, werr.Message)
	}
	if _, err := os.Stat(filepath.Join(sentinelDir, "sentinel-1878.log")); err != nil {
		t.Fatalf("the sentinel file must land on the workspace filesystem (#1878 probe): %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(outcome.Output), &result); err != nil {
		t.Fatalf("output not JSON: %v (%s)", err, outcome.Output)
	}
	if result["secretsProbe"] != "file-sourced" {
		t.Errorf("script child must see the secrets-env-sourced env, got %v", result["secretsProbe"])
	}
	if result["deltaProbe"] != "delta-sourced" {
		t.Errorf("script child must see the spawn-delta env, got %v", result["deltaProbe"])
	}
	if gh, _ := result["gh"].(string); !strings.HasPrefix(gh, shimDir) {
		t.Errorf("gh must resolve from PATH in the session env, got %q", result["gh"])
	}
	if belt, _ := result["belt"].(string); !strings.HasPrefix(belt, shimDir) {
		t.Errorf("app-git-token (the belt) must resolve from PATH in the session env, got %q", result["belt"])
	}
	if result["ghToken"] != true {
		t.Errorf("GH_TOKEN from the spawn delta must be visible to the script child, got %v", result["ghToken"])
	}
}

// Loud failures: a failing script's stderr rides the closed enum —
// exit>0 maps to script_failed "exit N: <stderr>", pre-exec errors map
// to script_failed with the error text, and a deadline maps to
// script_timeout. Nothing is swallowed.
func TestScriptExecEngine_ErrorMapping(t *testing.T) {
	engine := &scriptExecEngine{childEnv: func() []string { return nil }, exec: (&fakeScriptExec{
		out: nil, stderr: "boom-from-stderr", exit: 2,
		err: fmt.Errorf("execution failed (exit 2): exit status 2"),
	}).exec}
	_, werr := engine.run(context.Background(), map[string]any{"language": "python", "handler": "def handler(input):\n    return {}", "timeout_ms": 5000})
	if werr == nil || werr.Code != scriptExecErrFailed || !strings.Contains(werr.Message, "exit 2: boom-from-stderr") {
		t.Fatalf("exit>0 must map to script_failed with the stderr, got %+v", werr)
	}

	engine.exec = (&fakeScriptExec{err: fmt.Errorf("unsupported language: cobol"), exit: -1}).exec
	_, werr = engine.run(context.Background(), map[string]any{"language": "python", "handler": "x", "timeout_ms": 5000})
	if werr == nil || werr.Code != scriptExecErrFailed || !strings.Contains(werr.Message, "unsupported language") {
		t.Fatalf("pre-exec error must map to script_failed with the error detail, got %+v", werr)
	}

	hanging := &fakeScriptExec{hangCtx: true}
	engine.exec = hanging.exec
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, werr = engine.run(ctx, map[string]any{"language": "python", "handler": "x", "timeout_ms": 60000})
	if werr == nil || werr.Code != scriptExecErrTimeout {
		t.Fatalf("deadline must map to script_timeout, got %+v", werr)
	}
}

// Params validation is deterministic and self-teaching (bad_request).
func TestScriptExecEngine_BadParams(t *testing.T) {
	engine := &scriptExecEngine{childEnv: func() []string { return nil }, exec: (&fakeScriptExec{}).exec}
	for name, params := range map[string]map[string]any{
		"unsupported language": {"language": "bash", "handler": "echo hi"},
		"empty handler":        {"language": "python", "handler": ""},
		"oversize handler":     {"language": "python", "handler": strings.Repeat("x", maxScriptExecHandlerBytes+1)},
		"timeout over ceiling": {"language": "python", "handler": "x", "timeout_ms": int64((scriptExecMaxTimeout + time.Second) / time.Millisecond)},
	} {
		_, werr := engine.run(context.Background(), params)
		if werr == nil || werr.Code != scriptExecErrBadRequest {
			t.Errorf("%s must reject with bad_request, got %+v", name, werr)
		}
	}
}

// The #1455 prerequisite probe, workspace-container edition: a broken
// TMPDIR in the workspace container is the NAMED cause.
func TestScriptExecEngine_EnvCheckLoud(t *testing.T) {
	t.Setenv("TMPDIR", "/nonexistent-1878-script-exec-tmp")
	engine := newScriptExecEngine(func() []string { return nil })
	_, werr := engine.run(context.Background(), map[string]any{"language": "python", "handler": "def handler(input):\n    return {}", "timeout_ms": 5000})
	if werr == nil || werr.Code != scriptExecErrEnvUnavailable {
		t.Fatalf("broken TMPDIR must surface script_env_unavailable, got %+v", werr)
	}
	if !strings.Contains(werr.Message, "workspace container") {
		t.Fatalf("the cause must name the workspace container, got %q", werr.Message)
	}
}

// The control-socket method round trip: success result and the closed
// error enum cross the wire intact (the mixed-fleet contract — a
// supervisor without the engine answers internal, never a silent
// no-op).
func TestControlSocketScriptExec_RoundTrip(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()

	if _, werr := newControlClient(srv.addr()).ScriptExec(context.Background(), map[string]any{"language": "python", "handler": "x"}); werr == nil || werr.Code != "internal" {
		t.Fatalf("unwired engine must answer internal, got %+v", werr)
	}

	_, _, delta := scriptExecTestEnv(t)
	srv.scriptExec = newScriptExecEngine(func() []string { return productionChildEnv(delta) })
	client := newControlClient(srv.addr())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outcome, werr := client.ScriptExec(ctx, map[string]any{
		"language":   "python",
		"handler":    "def handler(input):\n    import os\n    return {\"ok\": True, \"probe\": os.environ.get(\"WT1878_DELTA_PROBE\")}",
		"input":      map[string]any{},
		"timeout_ms": 25000,
	})
	if werr != nil {
		t.Fatalf("script_exec round trip failed: %s: %s", werr.Code, werr.Message)
	}
	if !strings.Contains(outcome.Output, "delta-sourced") {
		t.Fatalf("the executed child must see the spawn delta; output: %s", outcome.Output)
	}

	_, werr = client.ScriptExec(ctx, map[string]any{"language": "bash", "handler": "echo hi"})
	if werr == nil || werr.Code != scriptExecErrBadRequest {
		t.Fatalf("closed-enum error must ride the wire, got %+v", werr)
	}
}

// Transport failure is typed: a dead supervisor surfaces the closed
// "transport" code so the mux layer can name the control socket in its
// loud script_env_unavailable.
func TestControlSocketScriptExec_TransportTyped(t *testing.T) {
	// Reserve a port and close the listener — dialing it fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	client := newControlClient(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, werr := client.ScriptExec(ctx, map[string]any{"language": "python", "handler": "x"})
	if werr == nil || werr.Code != scriptExecErrTransport {
		t.Fatalf("dead supervisor must surface transport, got %+v", werr)
	}
}
