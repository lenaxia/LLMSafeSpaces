//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// The tier-ruling live legs: the permission-tier floor rendered into a REAL
// pinned-opencode boot, denied through the REAL matcher in both
// semantics the ruling mandates — the READ tool (canonical/resolved
// symlink paths) and BASH (typed paths). The mock provider emits one
// tool call per turn; the tool result's error text proves the denial
// fired (or fails the test proving a tier leaks).
//
// Run: OPENCODE_BINARY=/opencode/usr/local/bin/opencode \
//      go test -tags=integration -run TestPermissionTier \
//      ./pkg/agent/opencode/ -timeout 600s

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierProbeProvider emits a sequence of tool calls — one per request —
// and records nothing else. Each turn's tool result comes back in the
// session transcript where the test asserts the denial.
type tierProbeProvider struct {
	mu    sync.Mutex
	calls []string // JSON argument blobs queued by the test, one per turn
}

// handler emits one queued tool call per turn (tool name + arguments
// from the queue entries; see emitCall). The bash leg and any future
// leg reuse it — there is exactly one SSE shape.
func (m *tierProbeProvider) handler(t *testing.T) http.HandlerFunc {
	return m.handlerTool(t, "read", "call_tier_1")
}

func (m *tierProbeProvider) handlerTool(t *testing.T, tool, callID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.mu.Lock()
		var call *string
		if len(m.calls) > 0 {
			c := m.calls[0]
			m.calls = m.calls[1:]
			call = &c
		}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		emit := func(payload string) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if call == nil {
			// Post-tool round: a plain-text finish so the turn completes.
			text, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "done"},
				}},
			})
			emit(string(text))
			done, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "delta": map[string]any{}}},
				"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			emit(string(done))
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": nil,
				"delta": map[string]any{"role": "assistant", "tool_calls": []map[string]any{{
					"index": 0, "id": callID, "type": "function",
					"function": map[string]any{"name": tool, "arguments": *call},
				}}},
			}},
		})
		emit(string(chunk))
		done, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-tier", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "delta": map[string]any{}}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
		emit(string(done))
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// tierBoot writes a config whose TOP-LEVEL permission.external_directory
// is EXACTLY the platform floor (as the ConfigWriter renders it since the
// tier-ruling wire finding) and boots the pinned binary via the shared harness.
// The historical mode.permissions shape is deliberately NOT written: it
// is INERT on the pinned 1.18.15 — these legs exist to prove the floor
// DENIES through the live key, so a leg passing here is proof the
// top-level shape is the one the harness reads.
func tierBoot(t *testing.T, prov *httptest.Server) *opencodeServer {
	t.Helper()
	extDir := make(map[string]string, len(platformPermissionTiers))
	for k, v := range platformPermissionTiers {
		extDir[k] = v
	}
	return tierBootExtDir(t, prov, extDir)
}

// tierBootExtDir is tierBoot with an explicit external_directory map —
// for legs that probe the ENGINE's ordering semantics on a synthetic
// rule set rather than the rendered floor.
func tierBootExtDir(t *testing.T, prov *httptest.Server, extDir map[string]string) *opencodeServer {
	t.Helper()
	perms := map[string]any{"external_directory": extDir}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"provider": map[string]any{
			"mock": map[string]any{
				"npm":     "@ai-sdk/openai-compatible",
				"options": map[string]any{"apiKey": "test", "baseURL": prov.URL + "/v1"},
				"models": map[string]any{"mockmodel": map[string]any{
					"name": "Mock Model", "attachment": true,
					"limit": map[string]int{"context": 100000, "output": 4096},
				}},
			},
		},
		"model":      "mock/mockmodel",
		"permission": perms,
	}
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	// Port 0: the harness claims a free port (r5 ownership guard) — a
	// fixed port can hand a stale server from an earlier leg the
	// live-proof role.
	return StartIntegrationServer(t, 0, string(cfgJSON))
}

// lastToolResult scans the newest session message for a tool part whose
// callID matches the emitted call and returns its error/output text.
func lastToolResult(t *testing.T, c *Client, sessionID string) string {
	t.Helper()
	body, _, err := c.SessionMessagesRaw(context.Background(), sessionID, 10, "")
	require.NoError(t, err)
	var msgs []struct {
		Parts []struct {
			Type  string `json:"type"`
			Tool  string `json:"tool"`
			State struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, p := range msgs[i].Parts {
			if p.Type == "tool" && p.Tool == "read" {
				if p.State.Error != "" {
					return p.State.Error
				}
				return p.State.Output
			}
		}
	}
	return ""
}

// TestPermissionTier_AllowPathPermitsRead — THE CORPSE-#5 REGRESSION
// LEG: the read of an ALLOWED path (/tmp — the tier pre-allow) must
// SUCCEED through the real binary. This is the leg that would have
// caught the months-long inertness (both deny legs pass under an
// over-broad deny render; only an allow leg proves allows apply) and
// the leg that guards the top-level render against a future regression
// to the inert mode.permissions shape.
func TestPermissionTier_AllowPathPermitsRead(t *testing.T) {
	probeFile := "/tmp/tier-allow-probe.txt"
	require.NoError(t, os.WriteFile(probeFile, []byte("TIER-ALLOW-PROBE-CONTENT\n"), 0o644))
	t.Cleanup(func() { _ = os.Remove(probeFile) })

	prov := tierProbeProvider{}
	prov.calls = []string{`{"filePath":"` + probeFile + `"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14500), Config: &http.Server{Handler: prov.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-allow")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "read the file", "", nil)
	require.NoError(t, err, "the turn must complete")

	res := lastToolResult(t, c, id)
	t.Logf("read tool result: %s", res)
	assert.NotContains(t, res, "prevents you from using this specific tool call",
		"an ALLOWED path must NOT be denied — an allow-tier failure here is corpse #5 reanimated (the render or the key is wrong)")
	assert.Contains(t, res, "TIER-ALLOW-PROBE-CONTENT",
		"the allowed read must return the file's content — the allow tier PERMITS through the real matcher")
}

// TestPermissionTier_ReadDeniesCanonicalTargets: the read tool resolves
// symlinks to canonical paths before the permission gate — the
// resolved-target denies (/sandbox-runtime/rt/secrets/*) must fire.
func TestPermissionTier_ReadDeniesCanonicalTargets(t *testing.T) {
	prov := tierProbeProvider{}
	prov.calls = []string{`{"filePath":"/sandbox-runtime/rt/secrets/leak.txt"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14490), Config: &http.Server{Handler: prov.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-read")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "read the file", "", nil)
	require.NoError(t, err, "the turn (denied tool + finish) must complete")

	res := lastToolResult(t, c, id)
	t.Logf("read tool result: %s", res)
	// The live 1.18.15 denial message is "The user has specified a rule
	// which prevents you from using this specific tool call" followed by
	// the matcher's own rule dump — assert the message AND that OUR deny
	// pattern is quoted in the dump (proves the tier rule fired, not
	// some other gate). The word "denied" appears nowhere in the real
	// message — an earlier draft asserted it and failed against a
	// demonstrably-firing deny (the unsatisfiable-assertion class; caught
	// by running the leg live, which is why these legs exist).
	assert.Contains(t, res, "prevents you from using this specific tool call",
		"the read of a resolved secret target must be denied by the tier floor through the real matcher")
	assert.Contains(t, res, `"/sandbox-runtime/rt/secrets/*"`,
		"the matcher's rule dump must quote the resolved-target deny — OUR rule fired")
}

// TestPermissionTier_BashDeniesTypedEtcPath: bash command parsing feeds
// TYPED paths to the external_directory gate — the /etc deny must fire
// for a typed absolute path.
func TestPermissionTier_BashDeniesTypedEtcPath(t *testing.T) {
	// Reuse the provider shape with a bash call.
	prov := tierProbeProvider{}
	prov.calls = []string{`{"command":"cat /etc/passwd"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14495), Config: &http.Server{Handler: prov.handlerTool(t, "bash", "call_tier_b")}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-bash")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "run the command", "", nil)
	require.NoError(t, err)

	res := lastBashResult(t, c, id)
	t.Logf("bash tool result: %s", res)
	// Same shape as the read leg: the live message + our quoted pattern.
	assert.Contains(t, res, "prevents you from using this specific tool call",
		"bash with a typed /etc path must be denied by the tier floor")
	assert.Contains(t, res, `"/etc/*"`,
		"the matcher's rule dump must quote the /etc deny — OUR rule fired")
}

func lastBashResult(t *testing.T, c *Client, sessionID string) string {
	t.Helper()
	body, _, err := c.SessionMessagesRaw(context.Background(), sessionID, 10, "")
	require.NoError(t, err)
	var msgs []struct {
		Parts []struct {
			Type  string `json:"type"`
			Tool  string `json:"tool"`
			State struct {
				Status string `json:"status"`
				Error  string `json:"error"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, p := range msgs[i].Parts {
			if p.Type == "tool" && p.Tool == "bash" {
				if p.State.Error != "" {
					return p.State.Error
				}
				return p.State.Output
			}
		}
	}
	return ""
}

func mustListener(t *testing.T, port int) net.Listener {
	t.Helper()
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+i))
		if err == nil {
			return l
		}
	}
	t.Fatalf("no free port near %d", port)
	return nil
}

// tierBashLeg boots the floor, runs ONE queued bash command, returns the
// bash tool result — the shared shape of the #1759 deny-preservation legs.
// The result is NEVER logged raw: these legs target credential-shaped
// paths, and a regression that lets the command execute would otherwise
// pour credential material into the test log (#1759 finding). Callers
// log verdicts, not output.
func tierBashLeg(t *testing.T, port int, command string) string {
	t.Helper()
	prov := tierProbeProvider{}
	prov.calls = []string{`{"command":"` + command + `"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, port), Config: &http.Server{Handler: prov.handlerTool(t, "bash", "call_tier_1759")}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-1759")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "run the command", "", nil)
	require.NoError(t, err)

	return lastBashResult(t, c, id)
}

// TestPermissionTier_LiveDenySpecificBeatsAllowGlob — the #1759
// precedence-ordering validation on the REAL engine. The issue's claim
// ("deny-specific over allow-glob precedence is already the engine's
// observed rule ordering") is load-bearing: the entire deny-preservation
// story rests on the LAST matching rule winning in rendered-key order.
// Boot with a broad allow sorted BEFORE a specific deny and prove the
// deny fires; boot the allow alone as the control and prove it does not.
func TestPermissionTier_LiveDenySpecificBeatsAllowGlob(t *testing.T) {
	// Deny leg: "/*" (0x2A) sorts before "/etc/*" — under findLast the
	// specific deny is the last match for /etc/passwd and must win.
	res := func() string {
		prov := tierProbeProvider{}
		prov.calls = []string{`{"command":"cat /etc/passwd"}`}
		provSrv := &httptest.Server{Listener: mustListener(t, 14520), Config: &http.Server{Handler: prov.handlerTool(t, "bash", "call_tier_ord")}}
		provSrv.Start()
		t.Cleanup(provSrv.Close)
		srv := tierBootExtDir(t, provSrv, map[string]string{"/*": "allow", "/etc/*": "deny"})
		c := NewLoopbackClient(srv.baseURL, "test-password")
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		id, err := c.SessionCreate(ctx, "tier-ord")
		require.NoError(t, err)
		_, err = c.SessionSend(ctx, id, "run the command", "", nil)
		require.NoError(t, err)
		out := lastBashResult(t, c, id)
		t.Logf("ordering deny leg result: %s", out)
		return out
	}()
	assert.Contains(t, res, "prevents you from using this specific tool call",
		"a specific deny sorted after a broad allow must STILL deny — findLast observed on the live engine")
	assert.Contains(t, res, `"/etc/*"`,
		"the matcher's rule dump must quote the specific deny — OUR rule out-ranked the allow")

	// Control leg: the allow alone — same boot, same command — permits.
	prov := tierProbeProvider{}
	prov.calls = []string{`{"command":"cat /etc/passwd"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14525), Config: &http.Server{Handler: prov.handlerTool(t, "bash", "call_tier_ctl")}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)
	srv := tierBootExtDir(t, provSrv, map[string]string{"/*": "allow"})
	c := NewLoopbackClient(srv.baseURL, "test-password")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-ctl")
	require.NoError(t, err)
	_, err = c.SessionSend(ctx, id, "run the command", "", nil)
	require.NoError(t, err)
	out := lastBashResult(t, c, id)
	t.Logf("ordering control leg result: %s", out)
	assert.NotContains(t, out, "prevents you from using this specific tool call",
		"the control: with the deny absent the broad allow permits the same command")
	assert.Contains(t, out, "root:x:",
		"the control leg's command output proves the read actually ran")
}

// TestPermissionTier_AllowPathPermitsRead_HomeSandbox — the #1759 red
// leg for the home PVC root: the read of an allowed /home/sandbox path
// must SUCCEED through the real binary (pre-fix, the root sits on ask
// and this leg fails — the permission-wall the issue documents).
func TestPermissionTier_AllowPathPermitsRead_HomeSandbox(t *testing.T) {
	probeFile := "/home/sandbox/tier-1759-allow-probe.txt"
	require.NoError(t, os.WriteFile(probeFile, []byte("TIER-1759-HOME-ALLOW-PROBE\n"), 0o644))
	t.Cleanup(func() { _ = os.Remove(probeFile) })

	prov := tierProbeProvider{}
	prov.calls = []string{`{"filePath":"` + probeFile + `"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14505), Config: &http.Server{Handler: prov.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-1759-home")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "read the file", "", nil)
	require.NoError(t, err, "the turn must complete")

	res := lastToolResult(t, c, id)
	t.Logf("read tool result: %s", res)
	assert.NotContains(t, res, "prevents you from using this specific tool call",
		"the home PVC root is ALLOWED (#1759) — an allow-tier failure here is the permission wall the issue filed")
	assert.Contains(t, res, "TIER-1759-HOME-ALLOW-PROBE",
		"the allowed home read must return the file's content through the real matcher")
}

// TestPermissionTier_AllowPathPermitsRead_Workspace — the #1759 leg for
// the workspace PVC root. The harness boots opencode with its project
// root in a temp dir, so /workspace is genuinely EXTERNAL here and the
// allow is exercised live (in the real pod /workspace is the project
// root and the rule is belt-and-braces — see permissiontiers.go).
func TestPermissionTier_AllowPathPermitsRead_Workspace(t *testing.T) {
	probeFile := "/workspace/tier-1759-allow-probe.txt"
	require.NoError(t, os.WriteFile(probeFile, []byte("TIER-1759-WORKSPACE-ALLOW-PROBE\n"), 0o644))
	t.Cleanup(func() { _ = os.Remove(probeFile) })

	prov := tierProbeProvider{}
	prov.calls = []string{`{"filePath":"` + probeFile + `"}`}
	provSrv := &httptest.Server{Listener: mustListener(t, 14510), Config: &http.Server{Handler: prov.handler(t)}}
	provSrv.Start()
	t.Cleanup(provSrv.Close)

	srv := tierBoot(t, provSrv)
	c := NewLoopbackClient(srv.baseURL, "test-password")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	id, err := c.SessionCreate(ctx, "tier-1759-ws")
	require.NoError(t, err)

	_, err = c.SessionSend(ctx, id, "read the file", "", nil)
	require.NoError(t, err, "the turn must complete")

	res := lastToolResult(t, c, id)
	t.Logf("read tool result: %s", res)
	assert.NotContains(t, res, "prevents you from using this specific tool call",
		"the workspace PVC root is ALLOWED (#1759)")
	assert.Contains(t, res, "TIER-1759-WORKSPACE-ALLOW-PROBE",
		"the allowed workspace read must return the file's content through the real matcher")
}

// TestPermissionTier_BashDeniesHomeCredentialWithRootAllow — the #1759
// deny-preservation leg: WITH the broad /home/sandbox/* allow rendered,
// a bash-typed credential path must STILL deny — deny-specific beats
// allow-glob on the live engine, exactly as the issue asserts.
// Verdicts only are logged; the probe path is credential-shaped.
func TestPermissionTier_BashDeniesHomeCredentialWithRootAllow(t *testing.T) {
	res := tierBashLeg(t, 14515, "cat /home/sandbox/.ssh/id_ed25519")
	denied := strings.Contains(res, "prevents you from using this specific tool call")
	quoted := strings.Contains(res, `"/home/sandbox/.ssh/*"`)
	t.Logf("denied=%v ruleQuoted=%v", denied, quoted)
	assert.True(t, denied,
		"a typed credential path must stay denied WITH the #1759 root allow present")
	assert.True(t, quoted,
		"the matcher's rule dump must quote the .ssh deny — the specific deny out-ranked the root allow")
}
