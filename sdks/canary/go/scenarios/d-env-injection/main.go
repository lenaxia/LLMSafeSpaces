// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-ENV-INJECTION
// Tests env var reaches agent and clears on unbind.
// Requires LLMSAFESPACES_LLM_API_KEY.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("env-injection", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 480*time.Second)
	defer cancel()
	runEnvInjection(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("env-injection", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 480*time.Second)
	defer cancel()
	runEnvInjection(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runEnvInjection(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("env-injection: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	// P1: Create workspace, wait Active
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-env-inject", Runtime: "base", StorageSize: "1Gi",
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

	// P2: SetEnv CANARY_INJECT=canary-xyz
	err = c.Workspaces.SetEnv(ctx, wsID, map[string]string{"CANARY_INJECT": "canary-xyz"})
	run.AssertNoError(err, "set-env: no error")

	// P3: Ensure session, send message that reads env var
	sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
	if !run.AssertNoError(err, "ensure-session: no error") {
		return
	}
	sessionID := sess.SessionID

	msg, err := c.Sessions.SendMessage(ctx, wsID, sessionID,
		`Run: python3 -c 'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))'`)
	if run.AssertNoError(err, "send-message: no error") {
		// P4: Agent response contains canary-xyz
		run.Assert(strings.Contains(msg.TextContent(), "canary-xyz"),
			"response-contains-value: canary-xyz in output",
			fmt.Sprintf("content=%.200s", msg.TextContent()))
	}

	// P5: DeleteEnv, then ReloadSecrets
	err = c.Workspaces.DeleteEnv(ctx, wsID, "CANARY_INJECT")
	run.AssertNoError(err, "delete-env: no error")

	_, err = c.Workspaces.ReloadSecrets(ctx, wsID)
	run.AssertNoError(err, "reload-secrets: no error")

	// P6: Send same command again → response contains NOTFOUND
	msg2, err := c.Sessions.SendMessage(ctx, wsID, sessionID,
		`Run: python3 -c 'import os; print(os.environ.get("CANARY_INJECT", "NOTFOUND"))'`)
	if run.AssertNoError(err, "send-message-2: no error") {
		run.Assert(strings.Contains(msg2.TextContent(), "NOTFOUND"),
			"response-contains-notfound: NOTFOUND in output (var cleared)",
			fmt.Sprintf("content=%.200s", msg2.TextContent()))
	}
}
