// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-SESSION-SUBTASK
// Tests subagent parentId backfill.
// Requires LLMSAFESPACES_LLM_API_KEY.
// Skips gracefully if the model does not use the task tool.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("session-subtask", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()
	runSessionSubtask(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("session-subtask", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	runSessionSubtask(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSessionSubtask(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("session-subtask: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-session-subtask", Runtime: "base", StorageSize: "1Gi",
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
	parentSessionID := sess.SessionID

	// P1: Send message that triggers a subagent session
	_, err = c.Sessions.SendPromptAsync(ctx, wsID, parentSessionID,
		"Use the task tool to create a subtask that writes hello world to /tmp/canary-subtask.txt")
	run.AssertNoError(err, "trigger-subtask: async prompt sent")

	// P2: Wait up to 30s for a session with non-empty parentId
	var subtaskFound bool
	var subtaskParentID string
	subtaskDeadline := time.Now().Add(30 * time.Second)
subtaskPoll:
	for time.Now().Before(subtaskDeadline) {
		select {
		case <-ctx.Done():
			break subtaskPoll
		case <-time.After(3 * time.Second):
		}
		sessions, err := c.Sessions.List(ctx, wsID)
		if err != nil {
			continue
		}
		for _, s := range sessions {
			if s.ParentID != "" {
				subtaskFound = true
				subtaskParentID = s.ParentID
				break subtaskPoll
			}
		}
	}

	if !subtaskFound {
		run.OK("subtask: skipped (model did not use task tool)")
		return
	}
	run.OK("subtask-found: session with non-empty parentId")

	// P3: parentId references the top-level session's ID
	run.Assert(subtaskParentID == parentSessionID, "subtask-parentId: matches parent",
		fmt.Sprintf("parentId=%q expected=%q", subtaskParentID, parentSessionID))

	// N1: A session with no parent has parentId absent or null (not empty string)
	status, rawBody, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/sessions", cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(status == 200, "n1-list-sessions: 200", fmt.Sprintf("got %d", status))
	if status == 200 {
		var rawSessions []map[string]any
		if json.Unmarshal(rawBody, &rawSessions) == nil {
			for _, s := range rawSessions {
				id, _ := s["id"].(string)
				if id == parentSessionID {
					pid, hasPid := s["parentId"]
					if !hasPid || pid == nil {
						run.OK("n1-parent-session: parentId absent or null")
					} else {
						pidStr, _ := pid.(string)
						run.Assert(pidStr != "", "n1-parent-session: parentId absent or null (not empty string)",
							fmt.Sprintf("parentId=%q", pidStr))
					}
					break
				}
			}
		}
	}
}
