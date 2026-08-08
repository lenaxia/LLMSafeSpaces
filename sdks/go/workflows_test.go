// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSDK_ListWorkflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/workflows" {
			t.Errorf("unexpected path: %s (expected /api/v1/me/workflows)", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"workflows": []map[string]any{
				{"id": "wf-1", "name": "test", "status": "active"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	result, err := c.Workflows.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].ID != "wf-1" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestSDK_CreateWorkflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/workflows" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "test-wf" {
			t.Errorf("unexpected name: %v", body["name"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "wf-new", "name": "test-wf", "status": "draft",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	result, err := c.Workflows.Create(context.Background(), CreateWorkflowReq{
		Name: "test-wf", SpecYAML: "{}", Status: "draft",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID != "wf-new" {
		t.Errorf("unexpected id: %s", result.ID)
	}
}

func TestSDK_RunWorkflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/workflows/wf-1/runs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "run-1", "workflowId": "wf-1", "status": "queued",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	result, err := c.Workflows.Run(context.Background(), "wf-1", nil, "ws-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID != "run-1" || result.Status != "queued" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestSDK_CancelRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/runs/run-1/cancel" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Workflows.CancelRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSDK_ListTriggers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/triggers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"triggers": []map[string]any{
				{"id": "trig-1", "name": "cron-test", "sourceType": "cron", "enabled": true},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	result, err := c.Triggers.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].ID != "trig-1" {
		t.Errorf("unexpected result: %+v", result)
	}
}

func TestSDK_DeleteTrigger(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/triggers/trig-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != "DELETE" {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("lsp_test"))
	err := c.Triggers.Delete(context.Background(), "trig-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
