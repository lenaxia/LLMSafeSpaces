// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

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
	run := canary.NewRunner("session-get", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	runSessionGet(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("session-get", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	runSessionGet(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSessionGet(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-session-get", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create-ws") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, c, wsID)
	run.Assert(phase == "Active", "ws-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
	if !run.AssertNoError(err, "ensure-session") {
		return
	}
	sessionID := sess.SessionID

	sessObj, err := c.Sessions.Get(ctx, wsID, sessionID)
	if run.AssertNoError(err, "p1-get-session") {
		run.Assert(sessObj.ID == sessionID, "p2-id-matches",
			fmt.Sprintf("got %q want %q", sessObj.ID, sessionID))
		run.Assert(sessObj.Title != "", "p3-has-title-field", sessObj.Title)
		run.Assert(sessObj.WorkspaceID == wsID && sessObj.Status != "", "p3b-contract-fields",
			fmt.Sprintf("workspaceId=%q status=%q", sessObj.WorkspaceID, sessObj.Status))
	}

	newTitle := "canary-renamed-title"
	err = c.Sessions.Rename(ctx, wsID, sessionID, newTitle)
	run.AssertNoError(err, "p4-rename")

	renamedObj, err := c.Sessions.Get(ctx, wsID, sessionID)
	if run.AssertNoError(err, "p4-get-after-rename") {
		run.Assert(renamedObj.Title == newTitle, "p4-title-updated",
			fmt.Sprintf("got %q want %q", renamedObj.Title, newTitle))
	}

	_, err = c.Sessions.Get(ctx, wsID, "nonexistent-id-000000000")
	run.Assert(err != nil, "n1-get-nonexistent", canary.ErrDetail(err, "expected error"))

	s, _, _ := canary.RawDo(ctx, "GET",
		fmt.Sprintf("%s/api/v1/workspaces/%s/sessions/../../etc/passwd",
			cfg.APIURL, wsID),
		cfg.APIKey, nil)
	run.Assert(s == 400 || s == 404, "n2-path-traversal",
		fmt.Sprintf("got %d", s))
}
