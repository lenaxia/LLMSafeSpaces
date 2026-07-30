// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-SECRET-BINDINGS
// Tests workspace secret bindings with idempotency checks.
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
	run := canary.NewRunner("secret-bindings", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	runSecretBindings(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("secret-bindings", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runSecretBindings(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSecretBindings(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	// Setup: workspace and secret
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-bindings-test", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create-ws: no error") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	secret, err := c.Secrets.CreateWithMetadata(ctx, "canary-binding-secret", "env-secret", "bindval", map[string]string{"var_name": "CANARY_VAR"})
	if !run.AssertNoError(err, "create-secret: no error") {
		return
	}
	secretID := secret.ID
	defer func() { _ = c.Secrets.Delete(context.Background(), secretID) }()

	// P1: Bind
	err = c.Workspaces.SetBindings(ctx, wsID, []string{secretID})
	run.AssertNoError(err, "set-bindings: no error")

	// P2: Get bindings — contains secret
	bindings, err := c.Workspaces.GetBindings(ctx, wsID)
	if run.AssertNoError(err, "get-bindings: no error") {
		found := false
		for _, b := range bindings.Bindings {
			if b.ID == secretID {
				found = true
				break
			}
		}
		run.Assert(found, "get-bindings: secret present", "")
	}

	// P3: Bind same secret again — idempotent (no error, no duplicate)
	err = c.Workspaces.SetBindings(ctx, wsID, []string{secretID})
	run.AssertNoError(err, "rebind-same: idempotent no error")

	bindings2, err := c.Workspaces.GetBindings(ctx, wsID)
	if run.AssertNoError(err, "get-bindings-after-rebind: no error") {
		count := 0
		for _, b := range bindings2.Bindings {
			if b.ID == secretID {
				count++
			}
		}
		run.Assert(count == 1, "rebind-same: exactly 1 entry", fmt.Sprintf("got %d", count))
	}

	// P4+P5: Clear bindings
	err = c.Workspaces.SetBindings(ctx, wsID, []string{})
	run.AssertNoError(err, "clear-bindings: no error")

	empty, err := c.Workspaces.GetBindings(ctx, wsID)
	if run.AssertNoError(err, "get-empty-bindings: no error") {
		run.Assert(len(empty.Bindings) == 0, "clear-bindings: empty", fmt.Sprintf("got %d", len(empty.Bindings)))
	}

	// P6: GET /secrets/:id/bindings
	wsIDs, err := c.Secrets.GetBindingsForSecret(ctx, secretID)
	run.AssertNoError(err, "get-secret-bindings: no error")
	_ = wsIDs

	// N1: Bind to nonexistent workspace
	err = c.Workspaces.SetBindings(ctx, "00000000-0000-0000-0000-000000000000", []string{secretID})
	run.Assert(err != nil, "bind-nonexistent-ws: error", canary.ErrDetail(err, "expected error"))

	// N2: Get bindings for nonexistent workspace
	_, err = c.Workspaces.GetBindings(ctx, "00000000-0000-0000-0000-000000000001")
	run.Assert(err != nil, "get-bindings-nonexistent-ws: error", canary.ErrDetail(err, "expected error"))
}
