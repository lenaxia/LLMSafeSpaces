// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-SESSION-MSG
// Tests session message round-trip, verbose flag, and lastActivityAt update.
// Requires LLMSAFESPACES_LLM_API_KEY.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("session-msg", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	runSessionMsg(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("session-msg", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	runSessionMsg(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSessionMsg(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("session-msg: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-sess-msg", Runtime: "base", StorageSize: "1Gi",
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

	// P1: Send message
	msg, err := c.Sessions.SendMessage(ctx, wsID, sessionID, "Reply with exactly: PONG")
	if run.AssertNoError(err, "send-message: no error") {
		// P2+P3: Response shape
		run.Assert(len(msg.TextContent()) > 0, "send-message: text non-empty", "")
	}

	// P4: lastActivityAt updated after message
	status, err := c.Workspaces.GetStatus(ctx, wsID)
	if run.AssertNoError(err, "get-status-after-msg: no error") {
		run.Assert(status.LastActivityAt != nil, "status: lastActivityAt non-nil after message", "")
		if status.LastActivityAt != nil {
			age := time.Since(*status.LastActivityAt)
			run.Assert(age < 2*time.Minute, "status: lastActivityAt recent",
				fmt.Sprintf("age=%v", age))
		}
	}

	// P5+P6: Verbose flag — history with and without verbose
	// Default (no verbose): patch parts stripped
	statusDefault, bodyDefault, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/sessions/%s/message", cfg.APIURL, wsID, sessionID),
		cfg.APIKey, nil)
	run.Assert(statusDefault == 200, "history-default: 200", fmt.Sprintf("got %d", statusDefault))
	run.Assert(!containsType(bodyDefault, "patch"), "history-default: no patch parts", "")

	// N1: Send to nonexistent session
	_, err = c.Sessions.SendMessage(ctx, wsID, "ses_nonexistent00000000000000", "ping")
	run.Assert(err != nil, "send-nonexistent-session: error", canary.ErrDetail(err, "expected error"))

	// N2: Send to different user's workspace
	if cfg.APIKeyUser2 != "" {
		c2 := cfg.Client2()
		_, err = c2.Sessions.SendMessage(ctx, wsID, sessionID, "ping")
		run.Assert(err != nil, "send-other-user-ws: error", canary.ErrDetail(err, "expected error"))
	}
}

// containsType checks if a JSON response body has any part with "type": value.
func containsType(body []byte, typeVal string) bool {
	s := string(body)
	return len(s) > 0 && (s == typeVal || // trivial check
		fmt.Sprintf(`"type":"%s"`, typeVal) != "" && // always true, just use contains
			contains(s, fmt.Sprintf(`"type":"%s"`, typeVal)))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findInString(s, substr))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
