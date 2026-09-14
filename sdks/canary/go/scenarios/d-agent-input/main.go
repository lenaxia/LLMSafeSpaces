// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-AGENT-INPUT
// Tests question and permission input flows via raw HTTP.
// Requires LLMSAFESPACES_LLM_API_KEY.
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
	run := canary.NewRunner("agent-input", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 480*time.Second)
	defer cancel()
	runAgentInput(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("agent-input", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 480*time.Second)
	defer cancel()
	runAgentInput(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runAgentInput(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("agent-input: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-agent-input", Runtime: "base", StorageSize: "1Gi",
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

	// P1: GET /question → 200, array (may be empty)
	qStatus, qBody, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/question", cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(qStatus == 200, "get-question: 200", fmt.Sprintf("got %d", qStatus))
	if qStatus == 200 {
		var questions []any
		run.Assert(json.Unmarshal(qBody, &questions) == nil, "get-question: array response", "")
	}

	// P2: GET /permission → 200, array
	pStatus, pBody, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission", cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(pStatus == 200, "get-permission: 200", fmt.Sprintf("got %d", pStatus))
	if pStatus == 200 {
		var permissions []any
		run.Assert(json.Unmarshal(pBody, &permissions) == nil, "get-permission: array response", "")
	}

	// P3: Send message that triggers tool-use permission
	err = c.Sessions.SendPromptAsync(ctx, wsID, sessionID,
		"Create a file called /tmp/canary-test.txt with the content: hello world")
	run.AssertNoError(err, "trigger-permission: async prompt sent")

	// P4: Poll GET /permission for ≥1 pending permission with id
	var permID string
	pollDeadline := time.Now().Add(60 * time.Second)
pollLoop:
	for time.Now().Before(pollDeadline) {
		select {
		case <-ctx.Done():
			break pollLoop
		case <-time.After(3 * time.Second):
		}
		var perms []struct {
			ID string `json:"id"`
		}
		status, body, _ := canary.RawDo(ctx, "GET",
			fmt.Sprintf("%s/api/v1/workspaces/%s/permission", cfg.APIURL, wsID),
			cfg.APIKey, nil)
		if status == 200 {
			_ = json.Unmarshal(body, &perms)
			if len(perms) >= 1 {
				permID = perms[0].ID
				break pollLoop
			}
		}
	}
	run.Assert(permID != "", "poll-permission: ≥1 pending with id",
		fmt.Sprintf("permID=%q", permID))

	// P5: POST /permission/{id}/reply with {"reply":"once"}
	if permID != "" {
		replyStatus, _, _ := canary.RawDo(ctx, "POST",
			fmt.Sprintf("%s/api/v1/workspaces/%s/permission/%s/reply", cfg.APIURL, wsID, permID),
			cfg.APIKey, []byte(`{"reply":"once"}`))
		// 2xx: 202 = the #1313 late-answer accept (4a-2).
		run.Assert(replyStatus >= 200 && replyStatus < 300, "approve-permission: success",
			fmt.Sprintf("got %d", replyStatus))
	}

	// P6: After approval, session returns to idle
	if permID != "" {
		idle := false
		idleDeadline := time.Now().Add(60 * time.Second)
	idleLoop:
		for time.Now().Before(idleDeadline) {
			select {
			case <-ctx.Done():
				break idleLoop
			case <-time.After(3 * time.Second):
			}
			detail, err := c.Sessions.Get(ctx, wsID, sessionID)
			if err == nil {
				if s, _ := detail["status"].(string); s == "idle" {
					idle = true
					break idleLoop
				}
			}
		}
		run.Assert(idle, "session-idle-after-approve: session idle", "")
	}

	// N1 (4a-2, #1302): the GENERIC request-ID contract — charset
	// [a-zA-Z0-9._-], no "..". A conforming id passes validation (its
	// downstream outcome is the harness's contract, not the handler's);
	// a charset violation 400s.
	n1Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/question/invalid-id-format/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"reply":"answer"}`))
	run.Assert(n1Status == 400, "n1-charset-violation: 400 (the generic contract)", fmt.Sprintf("got %d", n1Status))

	// N2: a CONFORMING but dead id takes the resolved paths (404
	// terminus / 202 late-answer / 502 flag-off) — never the prefix
	// 400 the retired contract produced. Assert NOT a 400-class
	// validation failure.
	n2Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission/some-id/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"reply":"maybe"}`))
	run.Assert(n2Status != 400, "n2-valid-generic-id: not a validation 400 (the prefix contract is retired)", fmt.Sprintf("got %d", n2Status))

	// N3: POST /permission/{id}/reply with invalid ID format → 400
	n3Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission/../../etc/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"reply":"once"}`))
	run.Assert(n3Status == 400, "n3-invalid-permission-id: 400", fmt.Sprintf("got %d", n3Status))
}
