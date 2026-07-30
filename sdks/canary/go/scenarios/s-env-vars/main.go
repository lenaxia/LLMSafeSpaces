// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-ENV-VARS
// Tests workspace environment variable API (not injection — that's D-ENV-INJECTION).
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
	run := canary.NewRunner("env-vars", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	runEnvVars(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("env-vars", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runEnvVars(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runEnvVars(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.Password == "" {
		run.OK("skipped (LLMSAFESPACES_PASSWORD not set — JWT login required for DEK-dependent operations)")
		return
	}
	c := cfg.JWTClient()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-envvars-test", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create-ws: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	// P1: Set env var
	err = c.Workspaces.SetEnv(ctx, wsID, map[string]string{"CANARY_VAR": "hello"})
	run.AssertNoError(err, "set-env: no error")

	// P2: Get env — contains CANARY_VAR
	env, err := c.Workspaces.GetEnv(ctx, wsID)
	if run.AssertNoError(err, "get-env: no error") {
		vars, _ := env["vars"].([]any)
		found := false
		for _, v := range vars {
			if s, ok := v.(string); ok && s == "CANARY_VAR" {
				found = true
				break
			}
		}
		run.Assert(found, "get-env: CANARY_VAR present", fmt.Sprintf("vars=%v", vars))
	}

	// P3: Upsert same var with new value
	err = c.Workspaces.SetEnv(ctx, wsID, map[string]string{"CANARY_VAR": "updated"})
	run.AssertNoError(err, "upsert-env: no error")

	// P4: Delete env var
	err = c.Workspaces.DeleteEnv(ctx, wsID, "CANARY_VAR")
	run.AssertNoError(err, "delete-env: no error")

	// P5: Absent after delete
	env2, err := c.Workspaces.GetEnv(ctx, wsID)
	if run.AssertNoError(err, "get-env-after-delete: no error") {
		vars, _ := env2["vars"].([]any)
		found := false
		for _, v := range vars {
			if s, ok := v.(string); ok && s == "CANARY_VAR" {
				found = true
				break
			}
		}
		run.Assert(!found, "get-env-after-delete: CANARY_VAR absent", "")
	}

	// N1: GET env on nonexistent workspace
	_, err = c.Workspaces.GetEnv(ctx, "00000000-0000-0000-0000-000000000000")
	run.Assert(err != nil, "get-env-nonexistent: error", canary.ErrDetail(err, "expected error"))

	// N2: PUT env with missing vars body
	status, _, _ := canary.RawDo(ctx, "PUT",
		cfg.APIURL+"/api/v1/workspaces/"+wsID+"/env",
		cfg.APIKey, []byte(`{}`))
	run.Assert(status == 400, "set-env-no-vars: 400", fmt.Sprintf("got %d", status))

	// N3: DELETE nonexistent var
	status3, _, _ := canary.RawDo(ctx, "DELETE",
		cfg.APIURL+"/api/v1/workspaces/"+wsID+"/env/NONEXISTENT_VAR_XYZ",
		cfg.APIKey, nil)
	run.Assert(status3 == 404, "delete-nonexistent-var: 404", fmt.Sprintf("got %d", status3))
}
