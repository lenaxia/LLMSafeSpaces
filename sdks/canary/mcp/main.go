// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// MCP Server canary: S-MCP-TOOLS + S-MCP-AUTH-NEG + S-MCP-CRED + S-MCP-INPUT-NEG
//
// Communicates with the MCP server via stdio transport (spawning the mcp binary)
// or SSE transport (connecting to a running SSE endpoint).
//
// Run modes:
//
//	MCP_TRANSPORT=stdio  (default) — spawn ./cmd/mcp/mcp binary via exec
//	MCP_TRANSPORT=sse    — connect to MCP_SSE_URL (default http://localhost:3001)
//
// Env vars:
//
//	LLMSAFESPACES_URL          API base URL (passed to MCP server)
//	LLMSAFESPACES_API_KEY      valid API key
//	MCP_TRANSPORT             stdio | sse
//	MCP_SSE_URL               SSE server URL (sse mode only)
//	MCP_BINARY                path to mcp binary (stdio mode only)
//
// Compile:
//
//	cd sdks/canary/mcp && go build -o mcp-canary ./main.go
//
// Run:
//
//	./mcp-canary
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

// ── JSON-RPC types ─────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ── Result tracking ────────────────────────────────────────────────────────

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Result struct {
	Scenario  string  `json:"scenario"`
	SDK       string  `json:"sdk"`
	Passed    int     `json:"passed"`
	Failed    int     `json:"failed"`
	DurationS float64 `json:"duration_s"`
	Checks    []Check `json:"checks"`
}

type Runner struct {
	scenario string
	start    time.Time
	checks   []Check
	passed   int
	failed   int
}

func NewRunner(scenario string) *Runner {
	return &Runner{scenario: scenario, start: time.Now()}
}

func (r *Runner) assert(cond bool, name, detail string) {
	r.checks = append(r.checks, Check{Name: name, Passed: cond, Detail: detail})
	if cond {
		r.passed++
	} else {
		r.failed++
	}
}

func (r *Runner) ok(name string)           { r.assert(true, name, "") }
func (r *Runner) fail(name, detail string) { r.assert(false, name, detail) }

func (r *Runner) print() Result {
	res := r.result()
	fmt.Printf("=== Canary: mcp-server / %s ===\n", res.Scenario)
	for _, c := range res.Checks {
		if c.Passed {
			fmt.Printf("  PASS %s\n", c.Name)
		} else {
			fmt.Printf("  FAIL %s: %s\n", c.Name, c.Detail)
		}
	}
	fmt.Printf("--- %d passed, %d failed in %.2fs ---\n\n", res.Passed, res.Failed, res.DurationS)
	return res
}

func (r *Runner) result() Result {
	return Result{
		Scenario:  r.scenario,
		SDK:       "mcp-server",
		Passed:    r.passed,
		Failed:    r.failed,
		DurationS: time.Since(r.start).Seconds(),
		Checks:    r.checks,
	}
}

// ── MCP client (stdio) ─────────────────────────────────────────────────────

type stdioClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID int
}

func newStdioClient(binary, apiURL, apiKey string) (*stdioClient, error) {
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"LLMSAFESPACES_URL="+apiURL,
		"LLMSAFESPACES_API_KEY="+apiKey,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mcp binary: %w", err)
	}

	c := &stdioClient{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		nextID: 1,
	}

	// Initialize
	_, err = c.send("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]string{"name": "canary", "version": "1.0"},
		"capabilities":    map[string]any{},
	})
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("initialize: %w", err)
	}

	return c, nil
}

func (c *stdioClient) send(method string, params any) (json.RawMessage, error) {
	return c.sendWithTimeout(method, params, 15*time.Second)
}

func (c *stdioClient) sendWithTimeout(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++

	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')

	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !c.stdout.Scan() {
			return nil, fmt.Errorf("EOF reading response")
		}
		line := c.stdout.Text()
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
	return nil, fmt.Errorf("timeout waiting for response to %s", method)
}

func (c *stdioClient) callTool(name string, args map[string]any) (*toolResult, error) {
	result, err := c.send("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, err
	}
	var tr toolResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	return &tr, nil
}

func (c *stdioClient) callToolWithTimeout(name string, args map[string]any, timeout time.Duration) (*toolResult, error) {
	result, err := c.sendWithTimeout("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	}, timeout)
	if err != nil {
		return nil, err
	}
	var tr toolResult
	if err := json.Unmarshal(result, &tr); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	return &tr, nil
}

func (c *stdioClient) listTools() ([]map[string]any, error) {
	result, err := c.send("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return resp.Tools, nil
}

func (c *stdioClient) close() {
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
}

// ── Scenario runners ───────────────────────────────────────────────────────

func runMCPTools(ctx context.Context, r *Runner, client *stdioClient) {
	tools, err := client.listTools()
	if err != nil {
		r.fail("tools/list: no error", err.Error())
		return
	}
	r.ok("tools/list: no error")

	for _, failure := range checkToolContract(tools) {
		r.fail(failure.name, failure.detail)
	}

	// P12: non-empty description
	for _, t := range tools {
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		r.assert(desc != "", "tool-description: "+name, "empty description")
	}

	// P13: inputSchema.type=object
	for _, t := range tools {
		name, _ := t["name"].(string)
		schema, _ := t["inputSchema"].(map[string]any)
		if schema == nil {
			r.fail("tool-schema: "+name, "no inputSchema")
			continue
		}
		r.assert(schema["type"] == "object", "tool-schema-type: "+name,
			fmt.Sprintf("got %v", schema["type"]))
	}
}

func runMCPAuthNeg(ctx context.Context, r *Runner, apiURL, binary string) {
	// Create a client with an invalid key
	badClient, err := newStdioClient(binary, apiURL, "lsp_invalid_canary_key_000000000000")
	if err != nil {
		r.fail("mcp-auth-neg: client started", err.Error())
		return
	}
	defer badClient.close()

	// N1: workspace_create with bad key → isError=true
	tr, err := badClient.callTool("workspace_create", map[string]any{"runtime": "base"})
	if err != nil {
		// JSON-RPC level error is a failure
		r.fail("bad-key: workspace_create not rpc error", "got rpc error: "+err.Error())
	} else {
		r.assert(tr.IsError, "bad-key: workspace_create isError=true", "")
		text := toolResultText(tr)
		r.assert(strings.Contains(strings.ToLower(text), "401") ||
			strings.Contains(strings.ToLower(text), "unauthorized") ||
			strings.Contains(strings.ToLower(text), "auth"),
			"bad-key: error message contains auth indicator", text[:min(len(text), 100)])
	}

	// N2: credential_list with bad key
	tr2, err2 := badClient.callTool("credential_list", map[string]any{})
	if err2 != nil {
		r.fail("bad-key: credential_list not rpc error", "got rpc error: "+err2.Error())
	} else {
		r.assert(tr2.IsError, "bad-key: credential_list isError=true", "")
	}
}

func runMCPInputNeg(ctx context.Context, r *Runner, client *stdioClient) {
	testCases := []struct {
		name    string
		tool    string
		args    map[string]any
		missing string
	}{
		{"session_create-missing-ws", "session_create", map[string]any{}, "workspace_id"},
		{"session_message-missing-ws", "session_message", map[string]any{"session_id": "x", "message": "hi"}, "workspace_id"},
		{"session_message-missing-sess", "session_message", map[string]any{"workspace_id": "x", "message": "hi"}, "session_id"},
		{"session_message-empty-msg", "session_message", map[string]any{"workspace_id": "x", "session_id": "y", "message": ""}, "message"},
		{"session_history-missing-ws", "session_history", map[string]any{"session_id": "x"}, "workspace_id"},
		{"model_list-missing-ws", "model_list", map[string]any{}, "workspace_id"},
		{"model_set-missing-model", "model_set", map[string]any{"workspace_id": "x"}, "model"},
	}

	for _, tc := range testCases {
		tr, err := client.callTool(tc.tool, tc.args)
		if err != nil {
			r.fail(tc.name+": not rpc error", "got rpc error: "+err.Error())
		} else {
			r.assert(tr.IsError, tc.name+": isError=true", "")
		}
	}

	// N5: message > 1MB → isError=true
	bigMsg := strings.Repeat("x", 1024*1024+1)
	tr, err := client.callTool("session_message", map[string]any{
		"workspace_id": "x", "session_id": "y", "message": bigMsg,
	})
	if err != nil {
		r.fail("oversized-message: not rpc error", "got rpc error: "+err.Error())
	} else {
		r.assert(tr.IsError, "oversized-message: isError=true", "")
		text := toolResultText(tr)
		r.assert(strings.Contains(strings.ToLower(text), "large") ||
			strings.Contains(strings.ToLower(text), "size") ||
			strings.Contains(strings.ToLower(text), "too"),
			"oversized-message: contains size indicator", text[:min(len(text), 100)])
	}
}

func runMCPCredCRUD(ctx context.Context, r *Runner, client *stdioClient) {
	// P1: credential_create (Epic 55 shape: kind + slug, not provider)
	tr, err := client.callTool("credential_create", map[string]any{
		"kind":    "anthropic",
		"slug":    "canary-mcp-cred",
		"api_key": "sk-canary-placeholder-00000000",
		"name":    "canary-mcp-cred",
	})
	if err != nil {
		r.fail("credential_create: no rpc error", err.Error())
		return
	}
	// The canary authenticates with an API key; credential writes require
	// the per-session DEK that only an interactive login unlocks
	// (ErrDEKUnavailable, 403 "encryption key not available;
	// re-authenticate"). That is the documented auth architecture, not
	// contract drift — assert the specific shape and skip ONLY the
	// DEK-gated CRUD positives. The handler-level input-validation
	// negatives (N1–N4, below) need no DEK and always run (#905 review).
	dekGated := false
	if tr.IsError && strings.Contains(toolResultText(tr), "encryption key not available") {
		r.ok("credential_create: dek_unavailable (API-key auth — expected, CRUD skipped)")
		dekGated = true
	} else {
		r.assert(!tr.IsError, "credential_create: isError=false", toolResultText(tr))
	}
	if dekGated {
		runCredNegatives(r, client)
		return
	}

	// Extract credential ID from result
	text := toolResultText(tr)
	var credResp map[string]any
	credID := ""
	if err := json.Unmarshal([]byte(text), &credResp); err == nil {
		credID, _ = credResp["id"].(string)
	}
	r.assert(credID != "", "credential_create: id in result", text[:min(len(text), 100)])

	// P2: credential_list
	tr2, err := client.callTool("credential_list", map[string]any{})
	if err != nil {
		r.fail("credential_list: no rpc error", err.Error())
	} else {
		r.assert(!tr2.IsError, "credential_list: isError=false", toolResultText(tr2))
		listText := toolResultText(tr2)
		r.assert(credID == "" || strings.Contains(listText, credID),
			"credential_list: contains created credential", "")
	}

	// P3: credential_delete
	if credID != "" {
		tr3, err := client.callTool("credential_delete", map[string]any{"credential_id": credID})
		if err != nil {
			r.fail("credential_delete: no rpc error", err.Error())
		} else {
			r.assert(!tr3.IsError, "credential_delete: isError=false", toolResultText(tr3))
			r.assert(strings.Contains(strings.ToLower(toolResultText(tr3)), "delet"),
				"credential_delete: result contains 'deleted'", toolResultText(tr3))
		}
	}

	runCredNegatives(r, client)
}

// runCredNegatives asserts the credential tools' handler-level input
// validation — DEK-independent, so they run on EVERY auth shape
// including API-key canaries (#905 review: the DEK skip must not bypass
// them).
func runCredNegatives(r *Runner, client *stdioClient) {
	// N1: missing kind
	if trN1, _ := client.callTool("credential_create", map[string]any{"slug": "x", "api_key": "sk-x"}); trN1 != nil {
		r.assert(trN1.IsError, "cred_create-missing-kind: isError=true", "")
	}
	// N2: missing slug
	if trN1b, _ := client.callTool("credential_create", map[string]any{"kind": "anthropic", "api_key": "sk-x"}); trN1b != nil {
		r.assert(trN1b.IsError, "cred_create-missing-slug: isError=true", "")
	}
	// N3: missing api_key
	if trN2, _ := client.callTool("credential_create", map[string]any{"kind": "anthropic", "slug": "x"}); trN2 != nil {
		r.assert(trN2.IsError, "cred_create-missing-key: isError=true", "")
	}
	// N4: delete nonexistent
	if trN4, _ := client.callTool("credential_delete", map[string]any{"credential_id": "00000000-0000-0000-0000-000000000099"}); trN4 != nil {
		r.assert(trN4.IsError, "cred_delete-nonexistent: isError=true", "")
	}
}

func runDeepWorkspace(ctx context.Context, r *Runner, client *stdioClient, apiURL, apiKey string) {
	sdkClient := llm.New(apiURL, llm.WithAPIKey(apiKey), llm.WithTimeout(60*time.Second))

	tr, err := client.callTool("workspace_create", map[string]any{
		"runtime": "base",
		"name":    "canary-mcp-deep",
	})
	if err != nil {
		r.fail("ws-create: call failed", err.Error())
		return
	}
	r.assert(!tr.IsError, "ws-create: isError=false", toolResultText(tr))
	text := toolResultText(tr)
	var wsResp map[string]any
	wsID := ""
	if jsonErr := json.Unmarshal([]byte(text), &wsResp); jsonErr == nil {
		wsID, _ = wsResp["id"].(string)
	}
	r.assert(wsID != "", "ws-create: id in result", text[:min(len(text), 100)])
	if wsID == "" {
		return
	}
	defer func() { _ = sdkClient.Workspaces.Delete(context.Background(), wsID) }()

	// CANARY_NO_CONTROLLER=1: the CI environment declares it runs no
	// workspace controller. Only the controller writes .status.phase
	// (controller phase_active.go), so without one the phase is EMPTY —
	// never "Pending" — and can never become Active. Environment-provided
	// truth, no phase inference: with the flag set, the Active-gated tail
	// (activate/stop) is skipped after creation verifies. In an
	// environment WITH a controller, no flag: WaitActive must succeed
	// (#905 review — a stuck workspace there is real drift and fails).
	if os.Getenv("CANARY_NO_CONTROLLER") == "1" {
		r.ok("ws-wait-active: skipped (CANARY_NO_CONTROLLER=1 — no controller writes .status.phase in CI)")
		return
	}
	phase := canary.WaitActive(ctx, sdkClient, wsID)
	r.assert(phase == "Active", "ws-wait-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}
	r.ok("ws-wait-active")

	tr3, err3 := client.callTool("workspace_activate", map[string]any{"workspace_id": wsID})
	if err3 != nil {
		r.fail("ws-activate: call failed", err3.Error())
	} else {
		r.assert(!tr3.IsError, "ws-activate: isError=false", toolResultText(tr3))
		text3 := toolResultText(tr3)
		r.assert(strings.Contains(text3, "resumed"), "ws-activate: has resumed field", text3[:min(len(text3), 100)])
	}

	tr4, err4 := client.callTool("workspace_stop", map[string]any{"workspace_id": wsID})
	if err4 != nil {
		r.fail("ws-stop: call failed", err4.Error())
	} else {
		r.assert(!tr4.IsError, "ws-stop: isError=false", toolResultText(tr4))
		text4 := toolResultText(tr4)
		r.assert(strings.Contains(text4, wsID), "ws-stop: contains workspace ID", text4[:min(len(text4), 200)])
	}

	trN1, _ := client.callTool("workspace_create", map[string]any{})
	if trN1 != nil {
		r.assert(trN1.IsError, "ws-create-no-runtime: isError=true", "")
	}

	trN2, _ := client.callTool("workspace_activate", map[string]any{"workspace_id": "00000000-0000-0000-0000-000000009999"})
	if trN2 != nil {
		r.assert(trN2.IsError, "ws-activate-nonexistent: isError=true", "")
	}

	trN3, _ := client.callTool("workspace_stop", map[string]any{"workspace_id": "00000000-0000-0000-0000-000000009999"})
	if trN3 != nil {
		r.assert(trN3.IsError, "ws-stop-nonexistent: isError=true", "")
	}
}

func runDeepSession(ctx context.Context, r *Runner, client *stdioClient, apiURL, apiKey string) {
	if os.Getenv("LLMSAFESPACES_LLM_API_KEY") == "" {
		r.ok("session: skipped (no LLM API key)")
		return
	}

	sdkClient := llm.New(apiURL, llm.WithAPIKey(apiKey), llm.WithTimeout(60*time.Second))

	ws, err := sdkClient.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-mcp-session", Runtime: "base", StorageSize: "1Gi",
	})
	if err != nil {
		r.fail("sdk-create-ws: no error", err.Error())
		return
	}
	wsID := ws.ID
	defer func() { _ = sdkClient.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, sdkClient, wsID)
	r.assert(phase == "Active", "session-ws-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	tr2, err2 := client.callTool("session_create", map[string]any{"workspace_id": wsID})
	if err2 != nil {
		r.fail("session-create: call failed", err2.Error())
		return
	}
	r.assert(!tr2.IsError, "session-create: isError=false", toolResultText(tr2))
	text2 := toolResultText(tr2)
	var sessResp map[string]any
	sessionID := ""
	if jsonErr := json.Unmarshal([]byte(text2), &sessResp); jsonErr == nil {
		sessionID, _ = sessResp["id"].(string)
		if sessionID == "" {
			sessionID, _ = sessResp["sessionId"].(string)
		}
	}
	r.assert(sessionID != "", "session-create: id in result", text2[:min(len(text2), 100)])
	if sessionID == "" {
		return
	}

	tr3, err3 := client.callToolWithTimeout("session_message", map[string]any{
		"workspace_id": wsID,
		"session_id":   sessionID,
		"message":      "Reply with exactly: MCP-OK",
	}, 120*time.Second)
	if err3 != nil {
		r.fail("session-message: call failed", err3.Error())
	} else {
		r.assert(!tr3.IsError, "session-message: isError=false", toolResultText(tr3))
		text3 := toolResultText(tr3)
		r.assert(len(text3) > 0, "session-message: non-empty result", "")
	}

	tr4, err4 := client.callTool("session_history", map[string]any{
		"workspace_id": wsID,
		"session_id":   sessionID,
	})
	if err4 != nil {
		r.fail("session-history: call failed", err4.Error())
	} else {
		r.assert(!tr4.IsError, "session-history: isError=false", toolResultText(tr4))
		text4 := toolResultText(tr4)
		var histArr []any
		if jsonErr := json.Unmarshal([]byte(text4), &histArr); jsonErr == nil {
			r.assert(len(histArr) >= 1, "session-history: >=1 entry", fmt.Sprintf("got %d", len(histArr)))
		} else {
			r.fail("session-history: valid JSON array", jsonErr.Error())
		}
	}
}

func runDeepPromptAsync(ctx context.Context, r *Runner, client *stdioClient, apiURL, apiKey string) {
	if os.Getenv("LLMSAFESPACES_LLM_API_KEY") == "" {
		r.ok("prompt-async: skipped (no LLM API key)")
		return
	}

	sdkClient := llm.New(apiURL, llm.WithAPIKey(apiKey), llm.WithTimeout(60*time.Second))

	ws, err := sdkClient.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-mcp-prompt", Runtime: "base", StorageSize: "1Gi",
	})
	if err != nil {
		r.fail("sdk-create-ws: no error", err.Error())
		return
	}
	wsID := ws.ID
	defer func() { _ = sdkClient.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, sdkClient, wsID)
	r.assert(phase == "Active", "prompt-ws-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	sess, err := canary.EnsureSessionWithRetry(ctx, sdkClient, wsID, 5)
	if err != nil {
		r.fail("ensure-session: no error", err.Error())
		return
	}
	sessionID := sess.SessionID
	r.assert(sessionID != "", "ensure-session: sessionId", "")
	if sessionID == "" {
		return
	}

	tr2, err2 := client.callToolWithTimeout("session_message", map[string]any{
		"workspace_id": wsID,
		"session_id":   sessionID,
		"message":      "Reply with exactly: PROMPT-ASYNC-OK",
	}, 120*time.Second)
	if err2 != nil {
		r.fail("prompt-session-message: call failed", err2.Error())
	} else {
		r.assert(!tr2.IsError, "prompt-session-message: isError=false", toolResultText(tr2))
		text2 := toolResultText(tr2)
		r.assert(len(text2) > 0, "prompt-session-message: non-empty text", "")
	}

	history, histErr := sdkClient.Sessions.GetHistory(ctx, wsID, sessionID)
	if histErr != nil {
		r.fail("prompt-history: no error", histErr.Error())
	} else {
		r.assert(len(history) >= 1, "prompt-history: >=1 entry", fmt.Sprintf("got %d", len(history)))
	}
}

func runDeepModel(ctx context.Context, r *Runner, client *stdioClient, apiURL, apiKey string) {
	if os.Getenv("LLMSAFESPACES_LLM_API_KEY") == "" {
		r.ok("model: skipped (no LLM API key)")
		return
	}

	sdkClient := llm.New(apiURL, llm.WithAPIKey(apiKey), llm.WithTimeout(60*time.Second))

	ws, err := sdkClient.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-mcp-model", Runtime: "base", StorageSize: "1Gi",
	})
	if err != nil {
		r.fail("sdk-create-ws: no error", err.Error())
		return
	}
	wsID := ws.ID
	defer func() { _ = sdkClient.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, sdkClient, wsID)
	r.assert(phase == "Active", "model-ws-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	tr1, err1 := client.callTool("model_list", map[string]any{"workspace_id": wsID})
	if err1 != nil {
		r.fail("model-list: call failed", err1.Error())
	} else {
		r.assert(!tr1.IsError, "model-list: isError=false", toolResultText(tr1))
		text1 := toolResultText(tr1)
		r.assert(len(text1) > 0, "model-list: non-empty result", "")
		var modelResp map[string]any
		if jsonErr := json.Unmarshal([]byte(text1), &modelResp); jsonErr == nil {
			_, hasModels := modelResp["models"]
			r.assert(hasModels, "model-list: has models field", "")
		}
	}

	llmModel := os.Getenv("LLMSAFESPACES_LLM_MODEL")
	if llmModel == "" {
		r.ok("model-set: skipped (no LLMSAFESPACES_LLM_MODEL)")
	} else {
		tr2, err2 := client.callTool("model_set", map[string]any{
			"workspace_id": wsID,
			"model":        llmModel,
		})
		if err2 != nil {
			r.fail("model-set: call failed", err2.Error())
		} else {
			r.assert(!tr2.IsError, "model-set: isError=false", toolResultText(tr2))
			text2 := toolResultText(tr2)
			r.assert(strings.Contains(text2, llmModel), "model-set: contains model name", text2[:min(len(text2), 100)])
		}
	}

	trN1, _ := client.callTool("model_list", map[string]any{"workspace_id": "00000000-0000-0000-0000-000000009999"})
	if trN1 != nil {
		r.assert(trN1.IsError, "model-list-nonexistent: isError=true", "")
	}

	trN2, _ := client.callTool("model_set", map[string]any{
		"workspace_id": "00000000-0000-0000-0000-000000009999",
		"model":        "test-model",
	})
	if trN2 != nil {
		r.assert(trN2.IsError, "model-set-nonexistent: isError=true", "")
	}
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	apiURL := os.Getenv("LLMSAFESPACES_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8080"
	}
	apiKey := os.Getenv("LLMSAFESPACES_API_KEY")
	binary := os.Getenv("MCP_BINARY")
	if binary == "" {
		// Find the mcp binary relative to repo root
		binary = "./cmd/mcp/mcp"
	}

	// Build the binary if it doesn't exist
	if _, err := os.Stat(binary); os.IsNotExist(err) {
		fmt.Printf("Building MCP binary at %s...\n", binary)
		cmd := exec.Command("go", "build", "-o", binary, "./cmd/mcp/")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to build MCP binary: %v\n", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	totalPassed, totalFailed := 0, 0

	// ── S-MCP-TOOLS ──
	{
		r := NewRunner("mcp-tools")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runMCPTools(ctx, r, client)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── S-MCP-AUTH-NEG ──
	{
		r := NewRunner("mcp-auth-neg")
		runMCPAuthNeg(ctx, r, apiURL, binary)
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── S-MCP-CRED + S-MCP-INPUT-NEG ── (share one client)
	{
		r := NewRunner("mcp-cred-and-input-neg")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runMCPCredCRUD(ctx, r, client)
			runMCPInputNeg(ctx, r, client)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── D-MCP-WORKSPACE ──
	{
		deepCtx, deepCancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer deepCancel()
		r := NewRunner("d-mcp-workspace")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runDeepWorkspace(deepCtx, r, client, apiURL, apiKey)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── D-MCP-SESSION ──
	{
		deepCtx, deepCancel := context.WithTimeout(context.Background(), 480*time.Second)
		defer deepCancel()
		r := NewRunner("d-mcp-session")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runDeepSession(deepCtx, r, client, apiURL, apiKey)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── D-MCP-PROMPT-ASYNC ──
	{
		deepCtx, deepCancel := context.WithTimeout(context.Background(), 480*time.Second)
		defer deepCancel()
		r := NewRunner("d-mcp-prompt-async")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runDeepPromptAsync(deepCtx, r, client, apiURL, apiKey)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	// ── D-MCP-MODEL ──
	{
		deepCtx, deepCancel := context.WithTimeout(context.Background(), 300*time.Second)
		defer deepCancel()
		r := NewRunner("d-mcp-model")
		client, err := newStdioClient(binary, apiURL, apiKey)
		if err != nil {
			r.fail("start-mcp-server", err.Error())
		} else {
			runDeepModel(deepCtx, r, client, apiURL, apiKey)
			client.close()
		}
		res := r.print()
		totalPassed += res.Passed
		totalFailed += res.Failed
	}

	fmt.Printf("=== Total: %d passed, %d failed ===\n", totalPassed, totalFailed)
	if totalFailed > 0 {
		os.Exit(1)
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────

func toolResultText(tr *toolResult) string {
	if tr == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range tr.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// rawHTTPDo is used by the SSE-based MCP test if needed.
func rawHTTPDo(ctx context.Context, method, url, apiKey string, body []byte) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// contractFailure is one tools/list contract violation.
type contractFailure struct{ name, detail string }

// canaryExpectedTools is the stable NAMED subset of the MCP tool
// contract: every base tool (including run_resolve — US-65.7 collapsed
// the three session_*_reply tools into it) plus a representative slice
// of the Epic-64 workflow/trigger surface. Pinned against the real
// registry by pkg/repolint (TestCanary_MCPTools_Parity) so this list
// cannot silently drift from the server again (#880).
var canaryExpectedTools = []string{
	"workspace_create", "workspace_activate", "workspace_stop",
	"workspace_refresh_compute",
	"session_create", "session_message", "session_history",
	"run_resolve",
	"credential_create", "credential_list", "credential_delete",
	"model_list", "model_set",
	"workflow_create", "workflow_list", "workflow_run",
	"trigger_create", "trigger_list",
}

// canaryToolFloor is the minimum tool count. Equals the full current
// registry (24) so ANY net removal fails; additive changes pass without
// canary edits (the #880 fix direction: no exact-count pinning).
const canaryToolFloor = 24

// checkToolContract validates a tools/list response: the named subset is
// present and the count is at or above the floor. Extracted for unit
// tests (sdks/canary/mcp/tools_test.go).
func checkToolContract(tools []map[string]any) []contractFailure {
	var failures []contractFailure
	if len(tools) < canaryToolFloor {
		failures = append(failures, contractFailure{
			"tools: min count",
			fmt.Sprintf("expected >= %d, got %d", canaryToolFloor, len(tools)),
		})
	}
	toolMap := make(map[string]bool, len(tools))
	for _, t := range tools {
		name, _ := t["name"].(string)
		toolMap[name] = true
	}
	for _, name := range canaryExpectedTools {
		if !toolMap[name] {
			failures = append(failures, contractFailure{"tool-present: " + name, ""})
		}
	}
	return failures
}
