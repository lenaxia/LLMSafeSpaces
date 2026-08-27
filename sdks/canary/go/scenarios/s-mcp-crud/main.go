// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Canary scenario: S-MCP-CRUD (#1046)
// User-scope MCP server CRUD round-trip against the live API.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	ll "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("mcp-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	runMCPCRUD(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("mcp-crud", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runMCPCRUD(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runMCPCRUD(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	c := cfg.JWTClient()

	// P1: Create (http transport, no secret)
	srv, err := c.McpServers.Create(ctx, ll.CreateMcpServerRequest{
		Name: "canary-mcp-server", Transport: "http", URL: "https://mcp.example.com/mcp",
	})
	if !run.AssertNoError(err, "create-mcp: no error") {
		return
	}
	run.Assert(srv.ID != "", "create-mcp: id non-empty", "")
	run.Assert(srv.Transport == "http", "create-mcp: transport=http", srv.Transport)
	srvID := srv.ID

	defer func() { _ = c.McpServers.Delete(context.Background(), srvID) }()

	// P2: Get
	got, err := c.McpServers.Get(ctx, srvID)
	if run.AssertNoError(err, "get-mcp: no error") {
		run.Assert(got.Name == "canary-mcp-server", "get-mcp: name", got.Name)
	}

	// P3: List — present
	list, err := c.McpServers.List(ctx)
	if run.AssertNoError(err, "list-mcp: no error") {
		found := false
		for _, s := range list {
			if s.ID == srvID {
				found = true
				break
			}
		}
		run.Assert(found, "list-mcp: present", "")
	}

	// P4: Update (rename)
	updated, err := c.McpServers.Update(ctx, srvID, ll.UpdateMcpServerRequest{Name: strPtr("canary-mcp-renamed")})
	if run.AssertNoError(err, "update-mcp: no error") {
		run.Assert(updated.Name == "canary-mcp-renamed", "update-mcp: renamed", updated.Name)
	}

	// P5: Auto-apply create + list
	if err := c.McpServers.CreateAutoApply(ctx, srvID, "user", nil); run.AssertNoError(err, "auto-apply-create: no error") {
		rules, err := c.McpServers.ListAutoApply(ctx, srvID)
		if run.AssertNoError(err, "auto-apply-list: no error") {
			found := false
			for _, rule := range rules {
				if rule.TargetType == "user" {
					found = true
					break
				}
			}
			run.Assert(found, "auto-apply-list: user rule present", "")
		}
	}

	// P6: Delete + verify gone
	if err := c.McpServers.Delete(ctx, srvID); run.AssertNoError(err, "delete-mcp: no error") {
		_, err := c.McpServers.Get(ctx, srvID)
		run.Assert(err != nil, "get-after-delete: error", canary.ErrDetail(err, "expected 404"))
	}

	// N1: Get nonexistent
	_, err = c.McpServers.Get(ctx, "00000000-0000-0000-0000-000000000098")
	run.Assert(err != nil, "get-nonexistent-mcp: error", canary.ErrDetail(err, "expected error"))

	// N2: Create with missing name — API should reject
	_, err = c.McpServers.Create(ctx, ll.CreateMcpServerRequest{Transport: "http"})
	run.Assert(err != nil, "create-malformed-mcp: error", canary.ErrDetail(err, "expected 400"))
}

func strPtr(s string) *string { return &s }
