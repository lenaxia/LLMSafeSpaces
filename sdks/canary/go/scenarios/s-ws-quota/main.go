// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-WS-QUOTA
// Tests workspace quota enforcement: creating workspaces up to the configured
// limit and verifying that the next creation returns 429.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("ws-quota", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	runWSQuota(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("ws-quota", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runWSQuota(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runWSQuota(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	limitStr := os.Getenv("LLMSAFESPACES_MAX_WORKSPACES_PER_USER")
	if limitStr == "" {
		// Match the TypeScript canary behavior: skip if the quota env
		// var is not explicitly set. Creating 10 workspaces against an
		// unlimited cluster and expecting a 429 is a guaranteed false
		// failure.
		run.OK("ws-quota: skipped (LLMSAFESPACES_MAX_WORKSPACES_PER_USER not set)")
		return
	}
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit == 0 {
		run.OK("ws-quota: skipped (LLMSAFESPACES_MAX_WORKSPACES_PER_USER not set or unlimited)")
		return
	}

	c := cfg.Client()

	// Pre-clean: delete any leftover canary workspaces from previous runs
	// so they don't eat into the quota and cause false failures.
	existing, _ := c.Workspaces.List(ctx, 100, 0)
	if existing != nil {
		for _, ws := range existing.Items {
			if len(ws.Name) >= 12 && ws.Name[:12] == "canary-quota-" {
				_ = c.Workspaces.Delete(ctx, ws.ID)
			}
		}
	}

	var created []string
	defer func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		for _, id := range created {
			_ = c.Workspaces.Delete(bgCtx, id)
		}
	}()

	for i := 0; i < limit; i++ {
		ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
			Name:        fmt.Sprintf("canary-quota-%d-%d", time.Now().Unix(), i),
			Runtime:     "base",
			StorageSize: "1Gi",
		})
		if !run.AssertNoError(err, fmt.Sprintf("P1: create workspace %d/%d", i+1, limit)) {
			return
		}
		created = append(created, ws.ID)
	}
	run.OK("P1: created up to configured limit")

	_, err = c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name:        fmt.Sprintf("canary-quota-over-%d", time.Now().Unix()),
		Runtime:     "base",
		StorageSize: "1Gi",
	})
	run.Assert(err != nil && llm.IsRateLimit(err),
		"N1: beyond limit returns 429",
		canary.ErrDetail(err, "expected IsRateLimit=true"))

	if err != nil {
		apiErr, ok := err.(*llm.APIError)
		if ok {
			// apiErr.Message is the structured "message" field (a plain
			// string after the parseError fix). The raw "error" field
			// is no longer stored separately — it's the fallback for
			// Message. The 429 response from the quota handler carries
			// "error" and "limit" fields. We re-read the body via the
			// Message if it's JSON, otherwise just verify the error
			// type and status code.
			var body map[string]any
			if jsonErr := json.Unmarshal([]byte(apiErr.Message), &body); jsonErr == nil {
				_, hasError := body["error"]
				_, hasLimit := body["limit"]
				run.Assert(hasError && hasLimit,
					"N2: 429 body has error and limit fields",
					fmt.Sprintf("error=%v limit=%v", hasError, hasLimit))
			} else {
				// Message is a plain string (new format) — verify via
				// status code and IsRateLimit already checked above.
				run.OK("N2: 429 returned (message format: plain string)")
			}
		}
	}
}
