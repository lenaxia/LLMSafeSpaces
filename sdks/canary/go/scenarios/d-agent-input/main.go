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

	// P1: GET /question → 200, array — the CONTRACT InputRequest shape
	// (4a-2: kind-discriminated, camelCase; ZERO legacy envelope
	// fields — the issue's kind e2e happy leg).
	qStatus, qBody, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/question", cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(qStatus == 200, "get-question: 200", fmt.Sprintf("got %d", qStatus))
	if qStatus == 200 {
		var questions []map[string]any
		if json.Unmarshal(qBody, &questions) != nil {
			run.Assert(false, "get-question: array response (hard)", string(qBody))
			return
		}
		{
			for _, q := range questions {
				run.Assert(q["kind"] == "question", "get-question: contract kind field", fmt.Sprintf("%v", q["kind"]))
				run.Assert(q["sessionId"] != nil || q["id"] != nil, "get-question: camelCase tags", "")
				run.Assert(q["questions"] == nil, "get-question: NO legacy envelope", "questions[] present")
				run.Assert(q["session_id"] == nil, "get-question: NO snake_case", "session_id present")
			}
		}
	}

	// P2: GET /permission → 200, array — contract shape.
	pStatus, pBody, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission", cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(pStatus == 200, "get-permission: 200", fmt.Sprintf("got %d", pStatus))
	if pStatus == 200 {
		var permissions []map[string]any
		if json.Unmarshal(pBody, &permissions) != nil {
			run.Assert(false, "get-permission: array response (hard)", string(pBody))
			return
		}
		{
			for _, p := range permissions {
				run.Assert(p["kind"] == "permission", "get-permission: contract kind field", fmt.Sprintf("%v", p["kind"]))
				run.Assert(p["session_id"] == nil, "get-permission: NO snake_case", "session_id present")
			}
		}
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
		var perms []map[string]any
		status, body, _ := canary.RawDo(ctx, "GET",
			fmt.Sprintf("%s/api/v1/workspaces/%s/permission", cfg.APIURL, wsID),
			cfg.APIKey, nil)
		if status == 200 && json.Unmarshal(body, &perms) == nil {
			if len(perms) >= 1 {
				// The LIVE pending entry carries the contract shape
				// (the issue's kind e2e happy leg, executed).
				run.Assert(perms[0]["kind"] == "permission", "live-permission: contract kind", fmt.Sprintf("%v", perms[0]["kind"]))
				run.Assert(perms[0]["session_id"] == nil, "live-permission: NO snake_case", "session_id present")
				permID, _ = perms[0]["id"].(string)
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
	// [a-zA-Z0-9._-]. A REAL charset violation ('%') with a VALID body
	// 400s at the ID check (a conforming id with this body would pass
	// validation and fail downstream instead).
	n1Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/question/bad%%24id/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"answers":[["Go"]]}`))
	run.Assert(n1Status == 400, "n1-charset-violation: 400 (the generic contract; valid body)", fmt.Sprintf("got %d", n1Status))

	// N2: a CONFORMING but dead id takes the resolved paths (404
	// terminus / 202 late-answer / 502 flag-off) — never the prefix
	// 400 the retired contract produced. Assert NOT a 400-class
	// validation failure.
	n2Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission/some-id/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"reply":"maybe"}`))
	run.Assert(n2Status != 400, "n2-valid-generic-id: not a validation 400 (the prefix contract is retired)", fmt.Sprintf("got %d", n2Status))

	// N3 (r2): a single-segment traversal id — multi-segment paths
	// 404 at the router before validation (Go's client does not clean
	// dot segments); a..b exercises validRequestID's traversal check.
	n3Status, _, _ := canary.RawDo(ctx, "POST",
		fmt.Sprintf("%s/api/v1/workspaces/%s/permission/a..b/reply", cfg.APIURL, wsID),
		cfg.APIKey, []byte(`{"reply":"once"}`))
	run.Assert(n3Status == 400, "n3-traversal-id: 400 (validRequestID's '..' check; valid body)", fmt.Sprintf("got %d", n3Status))
}
