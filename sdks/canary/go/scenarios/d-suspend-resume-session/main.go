// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-SUSPEND-RESUME-SESSION
// Session history survives suspend/resume cycle.
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
	run := canary.NewRunner("suspend-resume-session", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 600*time.Second)
	defer cancel()
	runSuspendResumeSession(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("suspend-resume-session", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	runSuspendResumeSession(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSuspendResumeSession(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("suspend-resume-session: skipped (no LLM API key)")
		return
	}

	c := cfg.Client()

	// P1: Create workspace, wait Active
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-susp-res-sess", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create-ws: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, c, wsID)
	run.Assert(phase == "Active", "reach-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	// P2: Ensure session, send BEFORE message
	sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
	if !run.AssertNoError(err, "ensure-session: no error") {
		return
	}
	sessionID := sess.SessionID

	msg1, err := c.Sessions.SendMessage(ctx, wsID, sessionID, "Reply with exactly: BEFORE")
	if run.AssertNoError(err, "send-before: no error") {
		run.Assert(len(msg1.TextContent()) > 0, "send-before: non-empty", "")
	}

	// P3: History before suspend
	hist1, err := c.Sessions.GetHistory(ctx, wsID, sessionID)
	run.AssertNoError(err, "history-before-suspend: no error")
	run.Assert(len(hist1) >= 1, "history-before-suspend: ≥1 entry",
		fmt.Sprintf("got %d", len(hist1)))

	// P4: Suspend → Suspended
	err = c.Workspaces.Suspend(ctx, wsID)
	run.AssertNoError(err, "suspend: no error")
	suspPhase := canary.WaitPhase(ctx, c, wsID, "Suspended", 60*time.Second)
	run.Assert(suspPhase == "Suspended", "suspend: phase=Suspended",
		fmt.Sprintf("got %q", suspPhase))

	// P5: Activate → Active
	_, err = c.Workspaces.Activate(ctx, wsID)
	run.AssertNoError(err, "activate: no error")
	resumePhase := canary.WaitActive(ctx, c, wsID)
	run.Assert(resumePhase == "Active", "activate: phase=Active",
		fmt.Sprintf("got %q", resumePhase))

	// P6: Ensure session (with retries for agent startup)
	sessPost, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 8)
	if !run.AssertNoError(err, "ensure-session-post-resume: no error") {
		return
	}
	run.Assert(sessPost.SessionID != "", "ensure-session-post-resume: sessionId", "")

	// P7: Send AFTER message to the new session
	msg2, err := c.Sessions.SendMessage(ctx, wsID, sessPost.SessionID, "Reply with exactly: AFTER")
	if run.AssertNoError(err, "send-after: no error") {
		run.Assert(len(msg2.TextContent()) > 0, "send-after: non-empty", "")
	}

	// P8: The BEFORE message must still be retrievable on the ORIGINAL session ID.
	// This is the actual persistence test — opencode stores history in the PVC
	// (/workspace), which survives suspend/resume. If the PVC is wiped or the
	// session store is corrupt, GetHistory on sessionID will return empty or error.
	histOriginal, err := c.Sessions.GetHistory(ctx, wsID, sessionID)
	if run.AssertNoError(err, "history-original-session-after-resume: no error") {
		run.Assert(len(histOriginal) >= 1,
			"history-original-session-after-resume: BEFORE message persisted",
			fmt.Sprintf("got %d entries — history was wiped by suspend/resume", len(histOriginal)))
	}

	// Also verify the new session has its AFTER message
	hist2, err := c.Sessions.GetHistory(ctx, wsID, sessPost.SessionID)
	if run.AssertNoError(err, "history-new-session-after-resume: no error") {
		run.Assert(len(hist2) >= 1, "history-new-session-after-resume: ≥1 entry",
			fmt.Sprintf("got %d", len(hist2)))
	}
}
