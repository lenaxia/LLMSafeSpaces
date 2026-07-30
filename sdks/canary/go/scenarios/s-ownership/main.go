// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-OWNERSHIP
// Tests cross-user isolation: User2 cannot access User1's resources.
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
	run := canary.NewRunner("ownership", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	runOwnership(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("ownership", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runOwnership(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runOwnership(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.APIKeyUser2 == "" {
		run.OK("ownership: skipped (LLMSAFESPACES_API_KEY_USER2 not set)")
		return
	}

	c1 := cfg.Client()
	c2 := cfg.Client2()

	// Create User1 workspace and secret
	ws1, err := c1.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-ownership-user1", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "user1-create-ws: no error") {
		return
	}
	defer func() { _ = c1.Workspaces.Delete(context.Background(), ws1.ID) }()

	secret1, err := c1.Secrets.CreateWithMetadata(ctx, "canary-ownership-s1", "env-secret", "val1", map[string]string{"var_name":"CANARY_VAR"})
	if !run.AssertNoError(err, "user1-create-secret: no error") {
		return
	}
	defer func() { _ = c1.Secrets.Delete(context.Background(), secret1.ID) }()

	// Create User2 workspace
	ws2, err := c2.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-ownership-user2", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "user2-create-ws: no error") {
		return
	}
	defer func() { _ = c2.Workspaces.Delete(context.Background(), ws2.ID) }()

	// P1: User1 can GET their workspace
	_, err = c1.Workspaces.Get(ctx, ws1.ID)
	run.AssertNoError(err, "user1-get-own-ws: no error")

	// P2: User2 can GET their workspace
	_, err = c2.Workspaces.Get(ctx, ws2.ID)
	run.AssertNoError(err, "user2-get-own-ws: no error")

	// P3: User1 list — W1 present, W2 absent
	list1, err := c1.Workspaces.List(ctx, 50, 0)
	if run.AssertNoError(err, "user1-list: no error") {
		foundW1, foundW2 := false, false
		for _, item := range list1.Items {
			if item.ID == ws1.ID {
				foundW1 = true
			}
			if item.ID == ws2.ID {
				foundW2 = true
			}
		}
		run.Assert(foundW1, "user1-list: W1 present", "")
		run.Assert(!foundW2, "user1-list: W2 absent", "")
	}

	// P4: User2 list — W2 present, W1 absent
	list2, err := c2.Workspaces.List(ctx, 50, 0)
	if run.AssertNoError(err, "user2-list: no error") {
		foundW1, foundW2 := false, false
		for _, item := range list2.Items {
			if item.ID == ws1.ID {
				foundW1 = true
			}
			if item.ID == ws2.ID {
				foundW2 = true
			}
		}
		run.Assert(!foundW1, "user2-list: W1 absent", "")
		run.Assert(foundW2, "user2-list: W2 present", "")
	}

	// N1: User2 GET User1's workspace.
	// Validated: verifyOwner returns NewForbiddenError (HTTP 403), not 404.
	// The Go SDK maps 403 → IsAuth=true. The server intentionally does NOT
	// return 404 here — it returns 403 so callers know the resource exists but
	// they are not the owner. Bindings route is the exception (returns 404).
	_, err = c2.Workspaces.Get(ctx, ws1.ID)
	run.Assert(err != nil && llm.IsAuth(err),
		"user2-get-user1-ws: 403 Forbidden",
		canary.ErrDetail(err, "expected 403 (IsAuth=true)"))

	// N2: User2 DELETE User1's workspace → error
	err = c2.Workspaces.Delete(ctx, ws1.ID)
	run.Assert(err != nil, "user2-delete-user1-ws: error",
		canary.ErrDetail(err, "expected error"))

	// N3: User2 GET status of User1's workspace → 403
	_, err = c2.Workspaces.GetStatus(ctx, ws1.ID)
	run.Assert(err != nil && llm.IsAuth(err), "user2-status-user1-ws: 403",
		canary.ErrDetail(err, "expected 403"))

	// N4: User2 GET User1's secret → error (secret service returns its own error shape)
	_, err = c2.Secrets.Get(ctx, secret1.ID)
	run.Assert(err != nil, "user2-get-user1-secret: error",
		canary.ErrDetail(err, "expected error"))

	// N5: User2 ensure session on User1's workspace → 403
	_, err = c2.Sessions.Ensure(ctx, ws1.ID)
	run.Assert(err != nil, "user2-ensure-session-user1-ws: error",
		canary.ErrDetail(err, "expected error"))

	// N6: Bindings route uses secrets service which returns 404 for cross-user access
	// (validated: handleSecretError maps ErrWorkspaceNotOwned → 404).
	status, body, _ := canary.RawDo(ctx, "GET",
		cfg.APIURL+"/api/v1/workspaces/"+ws1.ID+"/bindings",
		cfg.APIKeyUser2, nil)
	run.Assert(status == 404, "user2-bindings-user1-ws: 404 (secrets handler returns 404 for cross-user)",
		fmt.Sprintf("got %d: %s", status, truncate(body, 200)))
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Ensure json import is used (via encoding/json for truncate)
var _ = json.Marshal
