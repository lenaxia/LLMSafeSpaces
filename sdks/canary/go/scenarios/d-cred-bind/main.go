// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: D-CRED-BIND
// Tests credential bind + reload + unbind + reload-empty (reload with no bindings → status not_modified/applied, never an error).
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
	run := canary.NewRunner("cred-bind", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	runCredBind(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("cred-bind", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	runCredBind(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runCredBind(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.JWTClient()

	// Create workspace and wait for Active
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-cred-bind", Runtime: "base", StorageSize: "1Gi",
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

	// Create credential
	credValue, _ := json.Marshal(map[string]string{
		"kind":   cfg.LLMProvider,
		"slug":   "canary-bind-llm",
		"apiKey": "sk-canary-placeholder-key",
	})
	cred, err := c.Secrets.Create(ctx, "canary-cred-to-bind", "llm-provider", string(credValue))
	if !run.AssertNoError(err, "create-cred: no error") {
		return
	}
	credID := cred.ID
	defer func() { _ = c.Secrets.Delete(context.Background(), credID) }()

	// P2: Bind credential
	err = c.Workspaces.SetBindings(ctx, wsID, []string{credID})
	run.AssertNoError(err, "bind-cred: no error")

	// P3: Get bindings
	bindings, err := c.Workspaces.GetBindings(ctx, wsID)
	if run.AssertNoError(err, "get-bindings: no error") {
		found := false
		for _, b := range bindings.Bindings {
			if b.ID == credID {
				found = true
				break
			}
		}
		run.Assert(found, "get-bindings: cred present", "")
	}

	// P4: Reload secrets → pod-reported outcome (US-70.3: status, not a count)
	result, err := c.Workspaces.ReloadSecrets(ctx, wsID)
	if run.AssertNoError(err, "reload-secrets: no error") {
		run.Assert(result.Status == "applied" || result.Status == "not_modified",
			"reload-secrets: status ∈ {applied, not_modified}",
			fmt.Sprintf("got %q", result.Status))
	}

	// P5: Status credentialState.available = true after reload
	status, err := c.Workspaces.GetStatus(ctx, wsID)
	if run.AssertNoError(err, "status-after-reload: no error") {
		run.Assert(status.CredentialState.Available, "status: credentialState.available=true", "")
	}

	// P6+P7: Unbind, then reload-empty → still a valid pod outcome (not an error)
	err = c.Workspaces.SetBindings(ctx, wsID, []string{})
	run.AssertNoError(err, "unbind: no error")

	emptyBindings, err := c.Workspaces.GetBindings(ctx, wsID)
	if run.AssertNoError(err, "get-bindings-after-unbind: no error") {
		run.Assert(len(emptyBindings.Bindings) == 0, "unbind: empty bindings",
			fmt.Sprintf("got %d", len(emptyBindings.Bindings)))
	}

	emptyReload, err := c.Workspaces.ReloadSecrets(ctx, wsID)
	if run.AssertNoError(err, "reload-after-unbind: no error (not an error)") {
		run.Assert(emptyReload.Status == "applied" || emptyReload.Status == "not_modified",
			"reload-after-unbind: status ∈ {applied, not_modified}",
			fmt.Sprintf("got %q", emptyReload.Status))
	}

	// P8: credentialState.available after clearing
	statusAfter, err := c.Workspaces.GetStatus(ctx, wsID)
	if run.AssertNoError(err, "status-after-unbind: no error") {
		// Either false or reason="NotChecked" — just confirm it's present
		run.Assert(!statusAfter.CredentialState.Available || statusAfter.CredentialState.Reason != "",
			"status-after-unbind: credentialState reflects cleared state",
			fmt.Sprintf("available=%v reason=%q", statusAfter.CredentialState.Available, statusAfter.CredentialState.Reason))
	}

	// N1: Reload on suspended workspace
	err = c.Workspaces.Suspend(ctx, wsID)
	run.AssertNoError(err, "suspend-for-reload-test: no error")
	canary.WaitPhase(ctx, c, wsID, "Suspended", 60*time.Second)

	_, err = c.Workspaces.ReloadSecrets(ctx, wsID)
	run.Assert(err != nil, "reload-suspended: error (no running pod)",
		canary.ErrDetail(err, "expected 409 or error"))
}
