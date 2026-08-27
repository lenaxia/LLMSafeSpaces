// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #1046: MCP-server CRUD across all three Epic 53 scopes. Wire-level
// tests — paths, methods, and body shapes against a mock API.

func newMcpTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, WithAPIKey("k"))
}

func TestMcpServers_UserScopeCRUD(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := newMcpTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		switch {
		case gotPath == "/api/v1/me/mcp-servers" && gotMethod == "POST":
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"srv-1","name":"n","transport":"http","hasSecret":true,"enabled":true}`)
		case gotPath == "/api/v1/me/mcp-servers" && gotMethod == "GET":
			fmt.Fprint(w, `{"servers":[{"id":"srv-1","name":"n","transport":"stdio"}]}`)
		case gotPath == "/api/v1/me/mcp-servers/srv-1" && gotMethod == "GET":
			fmt.Fprint(w, `{"id":"srv-1","name":"n","transport":"http"}`)
		case gotPath == "/api/v1/me/mcp-servers/srv-1" && gotMethod == "PUT":
			json.NewDecoder(r.Body).Decode(&gotBody)
			fmt.Fprint(w, `{"id":"srv-1","name":"renamed","transport":"http"}`)
		case gotPath == "/api/v1/me/mcp-servers/srv-1" && gotMethod == "DELETE":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", gotMethod, gotPath)
			w.WriteHeader(404)
		}
	})

	srv, err := c.McpServers.Create(context.Background(), CreateMcpServerRequest{
		Name: "n", Transport: "http", URL: "https://m.example",
	})
	if err != nil || srv.ID != "srv-1" {
		t.Fatalf("create: %v %+v", err, srv)
	}

	list, err := c.McpServers.List(context.Background())
	if err != nil || len(list) != 1 || list[0].Transport != "stdio" {
		t.Fatalf("list: %v %+v", err, list)
	}

	got, err := c.McpServers.Get(context.Background(), "srv-1")
	if err != nil || got.ID != "srv-1" {
		t.Fatalf("get: %v %+v", err, got)
	}

	_, err = c.McpServers.Update(context.Background(), "srv-1", UpdateMcpServerRequest{Name: strPtr("renamed")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if gotBody["name"] != "renamed" {
		t.Fatalf("update body: %+v", gotBody)
	}

	if err := c.McpServers.Delete(context.Background(), "srv-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestMcpServers_AdminAndOrgScopes(t *testing.T) {
	var gotPath string
	c := newMcpTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch gotPath {
		case "/api/v1/admin/mcp-servers",
			"/api/v1/orgs/org-9/mcp-servers",
			"/api/v1/admin/mcp-servers/srv-1/auto-apply",
			"/api/v1/orgs/org-9/mcp-servers/srv-1/bindings":
			fmt.Fprint(w, `{"servers":[]}`)
		default:
			t.Errorf("unexpected path %s", gotPath)
			w.WriteHeader(404)
		}
	})

	if _, err := c.AdminMcpServers.List(context.Background()); err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if _, err := c.AdminMcpServers.ListAutoApply(context.Background(), "srv-1"); err != nil {
		t.Fatalf("admin auto-apply list: %v", err)
	}
	if _, err := c.OrgMcpServers.List(context.Background(), "org-9"); err != nil {
		t.Fatalf("org list: %v", err)
	}
}

func TestMcpServers_BindAndAutoApply(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	c := newMcpTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		switch {
		case gotPath == "/api/v1/me/mcp-servers/srv-1/bindings" && gotMethod == "POST":
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(201)
		case gotPath == "/api/v1/me/mcp-servers/srv-1/bindings/ws-1" && gotMethod == "DELETE":
			w.WriteHeader(204)
		case gotPath == "/api/v1/me/mcp-servers/srv-1/auto-apply" && gotMethod == "POST":
			json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"created":true}`)
		case gotPath == "/api/v1/me/mcp-servers/srv-1/auto-apply" && gotMethod == "GET":
			fmt.Fprint(w, `{"rules":[{"targetType":"all"}]}`)
		default:
			t.Errorf("unexpected %s %s", gotMethod, gotPath)
			w.WriteHeader(404)
		}
	})

	if err := c.McpServers.Bind(context.Background(), "srv-1", "ws-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if gotBody["workspaceId"] != "ws-1" {
		t.Fatalf("bind body: %+v", gotBody)
	}
	if err := c.McpServers.Unbind(context.Background(), "srv-1", "ws-1"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if err := c.McpServers.CreateAutoApply(context.Background(), "srv-1", "user", strPtr("u-1")); err != nil {
		t.Fatalf("create auto-apply: %v", err)
	}
	if gotBody["targetType"] != "user" || gotBody["targetId"] != "u-1" {
		t.Fatalf("auto-apply body: %+v", gotBody)
	}
	rules, err := c.McpServers.ListAutoApply(context.Background(), "srv-1")
	if err != nil || len(rules) != 1 || rules[0].TargetType != "all" {
		t.Fatalf("list auto-apply: %v %+v", err, rules)
	}
}

func strPtr(s string) *string { return &s }
