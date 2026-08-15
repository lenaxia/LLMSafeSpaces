// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-WS-CRUD
// Tests workspace create/get/list/rename/delete plus storage size validation.
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
	run := canary.NewRunner("ws-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	runWSCRUD(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("ws-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runWSCRUD(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runWSCRUD(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.Client()

	// P1: Create
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-crud-test", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create: no error") {
		return
	}
	run.Assert(ws.ID != "", "create: id non-empty", "")
	run.Assert(ws.Name == "canary-crud-test", "create: name", ws.Name)
	run.Assert(ws.Runtime == "base", "create: runtime", ws.Runtime)
	run.Assert(ws.StorageSize == "1Gi", "create: storageSize", ws.StorageSize)
	run.Assert(!ws.CreatedAt.IsZero(), "create: createdAt present", "")
	run.Assert(!ws.UpdatedAt.IsZero(), "create: updatedAt present", "")
	wsID := ws.ID

	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	// P2: Get
	got, err := c.Workspaces.Get(ctx, wsID)
	if run.AssertNoError(err, "get: no error") {
		run.Assert(got.ID == wsID, "get: id matches", got.ID)
	}

	// P3: List — present in items
	list, err := c.Workspaces.List(ctx, 50, 0)
	if run.AssertNoError(err, "list: no error") {
		found := false
		for _, item := range list.Items {
			if item.ID == wsID {
				found = true
				break
			}
		}
		run.Assert(found, "list: workspace present", "")
		// P4: pagination metadata
		run.Assert(list.Pagination != nil, "list: pagination present", "")
	}

	// P5: Pagination
	page1, err := c.Workspaces.List(ctx, 1, 0)
	if run.AssertNoError(err, "list-limit1: no error") {
		run.Assert(len(page1.Items) <= 1, "list-limit1: ≤1 item", fmt.Sprintf("got %d", len(page1.Items)))
	}

	// P6: Rename
	err = c.Workspaces.Rename(ctx, wsID, "canary-crud-renamed")
	if run.AssertNoError(err, "rename: no error") {
		renamed, _ := c.Workspaces.Get(ctx, wsID)
		if renamed != nil {
			run.Assert(renamed.Name == "canary-crud-renamed", "rename: name updated", renamed.Name)
		}
	}

	// N4: Rename with empty name — must happen BEFORE delete so wsID still exists
	status, _, _ := canary.RawDo(ctx, "PUT", cfg.APIURL+"/api/v1/workspaces/"+wsID,
		cfg.APIKey, []byte(`{}`))
	run.Assert(status == 400, "rename-empty-name: 400",
		fmt.Sprintf("got %d", status))

	// P7: Delete
	err = c.Workspaces.Delete(ctx, wsID)
	run.AssertNoError(err, "delete: no error")

	// P8: After delete — 404 (CRD is garbage-collected; API returns NotFound)
	time.Sleep(1 * time.Second)
	_, err = c.Workspaces.Get(ctx, wsID)
	run.Assert(err != nil && llm.IsNotFound(err), "after-delete: 404",
		canary.ErrDetail(err, "expected 404 after delete"))

	// N1: Get nonexistent
	_, err = c.Workspaces.Get(ctx, "00000000-0000-0000-0000-000000000000")
	run.Assert(err != nil && llm.IsNotFound(err), "get-nonexistent: 404", canary.ErrDetail(err, "expected 404"))

	// N2: Delete nonexistent
	err = c.Workspaces.Delete(ctx, "00000000-0000-0000-0000-000000000001")
	run.Assert(err != nil, "delete-nonexistent: error", canary.ErrDetail(err, "expected error"))

	// N3: Create with empty runtime — the API resolves the default-runtime
	// hierarchy (user preference → org policy → workspace.defaultImage →
	// "base" RuntimeEnvironment), so it should succeed (not fail). Verify the
	// returned workspace has a non-empty runtime (a default was applied).
	wsDefault, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{Name: "canary-default-runtime", Runtime: "", StorageSize: "1Gi"})
	if run.AssertNoError(err, "create-empty-runtime: defaults (no error)") {
		detail := ""
		if wsDefault != nil {
			detail = wsDefault.Runtime
		}
		run.Assert(wsDefault != nil && wsDefault.Runtime != "", "create-empty-runtime: runtime defaulted", detail)
		// Clean up the defaulted workspace.
		_ = c.Workspaces.Delete(ctx, wsDefault.ID)
	}

	// N5: Storage size too large
	_, err = c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-oversized", Runtime: "base", StorageSize: "9999Gi",
	})
	run.Assert(err != nil, "create-oversized-storage: error", canary.ErrDetail(err, "expected validation error"))

	// N6: Invalid storage size format
	_, err = c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-invalid-size", Runtime: "base", StorageSize: "invalid",
	})
	run.Assert(err != nil, "create-invalid-storage-format: error", canary.ErrDetail(err, "expected validation error"))
}
