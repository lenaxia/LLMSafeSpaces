// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-PROMPT-ASYNC
// Tests POST /sessions/:id/prompt (async) + SSE session.idle event.
// Critical: this is the code path the MCP server uses internally.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("prompt-async", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	runPromptAsync(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("prompt-async", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	runPromptAsync(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runPromptAsync(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("prompt-async: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-prompt-async", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, c, wsID)
	run.Assert(phase == "Active", "reach-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
	if !run.AssertNoError(err, "ensure-session: no error") {
		return
	}
	sessionID := sess.SessionID

	// P1: prompt_async returns 202 immediately
	err = c.Sessions.SendPromptAsync(ctx, wsID, sessionID, "Reply with the word: ASYNC-OK")
	run.AssertNoError(err, "prompt-async: 202 immediate response")

	// P2+P3: Subscribe to SSE and wait for session.idle
	idleReceived := waitForSessionIdle(ctx, cfg, wsID, sessionID, 90*time.Second)
	run.Assert(idleReceived, "sse: received session.idle for our session",
		"session did not become idle within timeout")

	// P4: History contains agent's response
	history, err := c.Sessions.GetHistory(ctx, wsID, sessionID)
	if run.AssertNoError(err, "history-after-async: no error") {
		run.Assert(len(history) >= 1, "history-after-async: ≥1 entry",
			fmt.Sprintf("got %d entries", len(history)))
	}

	// P5: Abort during in-flight prompt
	err = c.Sessions.SendPromptAsync(ctx, wsID, sessionID, "Count slowly from 1 to 1000")
	if run.AssertNoError(err, "second-prompt-async: 202") {
		time.Sleep(500 * time.Millisecond) // let it start
		err = c.Sessions.Abort(ctx, wsID, sessionID)
		run.AssertNoError(err, "abort-inflight: no error")
	}

	// N1: prompt_async with malformed session ID
	status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/sessions/../../etc/prompt", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"parts":[{"type":"text","text":"ping"}]}`))
	run.Assert(status == 400, "malformed-session-id: 400", fmt.Sprintf("got %d", status))

	// N2: prompt_async on non-Active workspace — verify 503 shape
	// Create a workspace but don't wait for Active; immediately send a prompt.
	// Should return 503 with {"error":"...","phase":"...","retryAfter":N} shape.
	pendingWS, err2 := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-pa-pending", Runtime: "base", StorageSize: "1Gi",
	})
	if err2 == nil {
		defer func() { _ = c.Workspaces.Delete(context.Background(), pendingWS.ID) }()
		// Workspace is Pending/Creating — prompt should fail with 503
		time.Sleep(200 * time.Millisecond)
		pendingStatus, pendingBody, _ := canary.RawDo(ctx, "POST",
			fmt.Sprintf("%s/api/v1/workspaces/%s/sessions/test-session-id/prompt",
				cfg.APIURL, pendingWS.ID),
			cfg.APIKey, []byte(`{"parts":[{"type":"text","text":"ping"}]}`))
		// Either 503 (workspace not ready) or 400 (invalid session ID format)
		run.Assert(pendingStatus == 503 || pendingStatus == 400,
			"prompt-async-not-ready: 503 or 400", fmt.Sprintf("got %d", pendingStatus))
		if pendingStatus == 503 {
			run.Assert(canary.HasField(pendingBody, "phase"),
				"503-not-ready: phase field present", "")
			run.Assert(canary.HasField(pendingBody, "retryAfter"),
				"503-not-ready: retryAfter field present", "")
		}
	}
}

// waitForSessionIdle subscribes to the workspace SSE stream and waits for
// a session.status{status:idle} event matching the given sessionID.
func waitForSessionIdle(ctx context.Context, cfg canary.Config, wsID, sessionID string, timeout time.Duration) bool {
	sseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sseCtx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/events", cfg.APIURL, wsID), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Status    string `json:"status"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if event.Type == "session.status" && event.Status == "idle" &&
			(event.SessionID == sessionID || event.SessionID == "") {
			return true
		}
	}
	return false
}
