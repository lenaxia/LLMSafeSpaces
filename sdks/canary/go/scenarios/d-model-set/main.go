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
	run := canary.NewRunner("model-set", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	runModelSet(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("model-set", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	runModelSet(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runModelSet(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("skipped: no LLM key")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-model-set", Runtime: "base", StorageSize: "1Gi",
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

	models, err := c.Workspaces.GetModels(ctx, wsID)
	if !run.AssertNoError(err, "get-models") {
		return
	}

	var targetModel string
	if cfg.LLMModel != "" {
		targetModel = cfg.LLMModel
	} else if len(models.Models) > 0 {
		targetModel = models.Models[0].ID
	}
	run.Assert(targetModel != "", "have-target-model", "")

	if targetModel != "" {
		err = c.Workspaces.SetModel(ctx, wsID, targetModel)
		run.AssertNoError(err, "p1-set-model")

		updated, err := c.Workspaces.GetModels(ctx, wsID)
		if run.AssertNoError(err, "p2-get-models") {
			run.Assert(updated.CurrentModel == targetModel, "p2-current-matches",
				fmt.Sprintf("got %q want %q", updated.CurrentModel, targetModel))
			found := false
			for _, m := range updated.Models {
				if m.ID == targetModel && m.Selected {
					found = true
				}
			}
			run.Assert(found, "p2-target-selected", targetModel)
		}

		sess, err := canary.EnsureSessionWithRetry(ctx, c, wsID, 5)
		if run.AssertNoError(err, "ensure-session") {
			msg, err := c.Sessions.SendMessage(ctx, wsID, sess.SessionID, "Reply with exactly: MODEL-SET-OK")
			if run.AssertNoError(err, "p3-send-message") {
				run.Assert(len(msg.TextContent()) > 0, "p3-non-empty-response", "")
			}
		}
	}

	err = c.Workspaces.SetModel(ctx, wsID, "")
	run.Assert(err != nil, "n1-empty-model", canary.ErrDetail(err, "expected error"))

	err = c.Workspaces.SetModel(ctx, "00000000-0000-0000-0000-000000000000", "some-model")
	run.Assert(err != nil, "n2-nonexistent-ws", canary.ErrDetail(err, "expected error"))

	err = c.Workspaces.SetModel(ctx, wsID, cfg.BadModel)
	_ = err
	time.Sleep(3 * time.Second)
	wsCheck, checkErr := c.Workspaces.Get(ctx, wsID)
	if checkErr == nil {
		run.Assert(wsCheck.Phase == "Active" || wsCheck.Phase == "Suspending" || wsCheck.Phase == "Suspended",
			"n3-still-active-after-bad-model",
			fmt.Sprintf("got %q", wsCheck.Phase))
	} else {
		run.OK("n3-ws-get-failed-after-bad-model")
	}
}
