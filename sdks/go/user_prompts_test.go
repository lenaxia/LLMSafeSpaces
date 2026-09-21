// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

// #1499: the user-prompts service's wire-level tests — paths, methods,
// body shapes, and the named envelope against a mock API (the
// mcp_servers_test.go convention).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestUserPrompts_CRUDWire(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newMcpTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case gotPath == "/api/v1/me/prompts" && gotMethod == "POST":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"prompt":{"id":"p1","name":"Weekly summary","content":"c","createdAt":"t1","updatedAt":"t2"}}`)
		case gotPath == "/api/v1/me/prompts" && gotMethod == "GET":
			fmt.Fprint(w, `{"prompts":[{"id":"p1","name":"Weekly summary","content":"c"}]}`)
		case gotPath == "/api/v1/me/prompts/p1" && gotMethod == "PUT":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			fmt.Fprint(w, `{"prompt":{"id":"p1","name":"renamed","content":"new"}}`)
		case gotPath == "/api/v1/me/prompts/p1" && gotMethod == "DELETE":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", gotMethod, gotPath)
			w.WriteHeader(404)
		}
	})

	created, err := c.UserPrompts.Create(context.Background(), CreateUserPromptRequest{Name: "Weekly summary", Content: "c"})
	if err != nil || created.ID != "p1" || created.Name != "Weekly summary" {
		t.Fatalf("create: %v %+v", err, created)
	}
	if gotBody["name"] != "Weekly summary" || gotBody["content"] != "c" {
		t.Fatalf("create body shape: %+v", gotBody)
	}

	prompts, err := c.UserPrompts.List(context.Background())
	if err != nil || len(prompts) != 1 || prompts[0].ID != "p1" {
		t.Fatalf("list: %v %+v", err, prompts)
	}

	updated, err := c.UserPrompts.Update(context.Background(), "p1", UpdateUserPromptRequest{Name: strPtr("renamed")})
	if err != nil || updated.Name != "renamed" {
		t.Fatalf("update: %v %+v", err, updated)
	}

	if err := c.UserPrompts.Delete(context.Background(), "p1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/me/prompts/p1") {
		t.Fatalf("last call path: %s", gotPath)
	}
}
