// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-SESSION-ENSURE
// Tests session ensure with auto-resume from Suspended, list, rename,
// abort, and individual session GET.
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
	run := canary.NewRunner("session-ensure", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	runSessionEnsure(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("session-ensure", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	runSessionEnsure(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSessionEnsure(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-sess-ensure", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	// P1: Wait for Active
	phase := canary.WaitActive(ctx, c, wsID)
	run.Assert(phase == "Active", "reach-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	// P2+P3: Ensure on Active workspace → resumed=false
	sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
	if !run.AssertNoError(err, "ensure-active: no error") {
		return
	}
	run.Assert(sess.SessionID != "", "ensure-active: sessionId present", "")
	run.Assert(sess.WorkspaceID == wsID, "ensure-active: workspaceId matches", "")
	run.Assert(!sess.Resumed, "ensure-active: resumed=false (was already Active)", "")
	sessionID := sess.SessionID

	// P4: Suspend workspace
	err = c.Workspaces.Suspend(ctx, wsID)
	run.AssertNoError(err, "suspend: no error")
	canary.WaitPhase(ctx, c, wsID, "Suspended", 60*time.Second)

	// P5: Ensure on Suspended → auto-resumes → resumed=true
	sessResumed, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 10)
	if run.AssertNoError(err, "ensure-suspended: no error (auto-resume)") {
		run.Assert(sessResumed.Resumed, "ensure-suspended: resumed=true", "")
		run.Assert(sessResumed.WorkspacePhase == "Active", "ensure-suspended: workspacePhase=Active",
			sessResumed.WorkspacePhase)
	}

	// P6: List sessions
	sessions, err := c.Sessions.List(ctx, wsID)
	if run.AssertNoError(err, "list-sessions: no error") {
		run.Assert(sessions != nil, "list-sessions: array", "")
	}

	// P7: Active sessions
	active, err := c.Sessions.GetActive(ctx, wsID)
	if run.AssertNoError(err, "active-sessions: no error") {
		run.Assert(active.MaxActive > 0, "active-sessions: maxActive > 0",
			fmt.Sprintf("got %d", active.MaxActive))
	}

	// P8: Rename session
	err = c.Sessions.Rename(ctx, wsID, sessionID, "canary-session-title")
	run.AssertNoError(err, "rename-session: no error")

	// P9: GET individual session
	sessionObj, err := c.Sessions.Get(ctx, wsID, sessionID)
	if run.AssertNoError(err, "get-session: no error") {
		run.Assert(sessionObj.ID == sessionID, "get-session: id matches", sessionObj.ID)
	}

	// P10: Abort (idle session — should not error)
	err = c.Sessions.Abort(ctx, wsID, sessionID)
	run.AssertNoError(err, "abort-session: no error")

	// P11: Second ensure (idempotent)
	sess2, err := c.Sessions.Ensure(ctx, wsID)
	if run.AssertNoError(err, "ensure-2nd: no error") {
		run.Assert(sess2.SessionID != "", "ensure-2nd: sessionId present", "")
	}

	// N1: Ensure on nonexistent workspace
	_, err = c.Sessions.Ensure(ctx, "00000000-0000-0000-0000-000000000000")
	run.Assert(err != nil, "ensure-nonexistent-ws: error", canary.ErrDetail(err, "expected error"))

	// N2: Rename with empty title
	status, _, _ := canary.RawDo(ctx, "PUT",
		cfg.APIURL+"/api/v1/workspaces/"+wsID+"/sessions/"+sessionID+"/title",
		cfg.APIKey, []byte(`{}`))
	run.Assert(status == 400, "rename-empty-title: 400", fmt.Sprintf("got %d", status))

	// D-SESSION-GET N2: path traversal
	s8, _, _ := canary.RawDo(ctx, "GET",
		cfg.APIURL+"/api/v1/workspaces/"+wsID+"/sessions/..%2F..%2Fetc/message",
		cfg.APIKey, nil)
	run.Assert(s8 == 400 || s8 == 404, "path-traversal-session: 400 or 404",
		fmt.Sprintf("got %d", s8))
}
