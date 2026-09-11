// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// admitter_v1_mcp_test.go — pins #1313: the admitter MUST use the V1
// message path (POST /session/:sid/message), not the V2 prompt endpoint
// (POST /api/session/:sid/prompt with delivery:"steer"). The V2 session
// runner on opencode 1.18.15 does NOT include MCP tools in the model's
// function definitions — every platform-delivered message through V2
// silently lost workspace tools (session_list, dev_preview_url, etc.).
// This test asserts the WIRE SHAPE: the admitter's POST must hit the V1
// endpoint with the parts-based body, never the V2 prompt endpoint.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdmitterUsesV1MessagePath pins the endpoint and body shape.
func TestAdmitterUsesV1MessagePath(t *testing.T) {
	var gotPath, gotDelivery, gotBodyType string
	var gotParts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session/test-ses/message":
			gotPath = r.URL.Path
			gotDelivery = "" // V1 has no delivery field
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if parts, ok := body["parts"].([]any); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]any); ok {
						gotParts = append(gotParts, pm)
					}
				}
			}
			gotBodyType = "parts"
			// V1 returns the assistant message synchronously
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"id": "msg_v1_123"},
			})
		case "/api/session/test-ses/prompt":
			t.Error("V2 prompt endpoint must NEVER be called by the admitter (#1313)")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"id": "msg_v2_WRONG"},
			})
		default:
			// Model pinning or other endpoints — pass through
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	adm := opencodeAdmitter{password: "test"}
	agentAddrAtomic.Store(srv.URL)

	msgID, err := adm.Admit(context.Background(), "test-ses", "msg_ob_t", "hello", "")
	if err != nil {
		t.Fatalf("Admit failed: %v", err)
	}

	if gotPath != "/session/test-ses/message" {
		t.Fatalf("admitter POSTed to %q — must use the V1 message path (#1313)", gotPath)
	}
	if gotDelivery != "" {
		t.Fatalf("V1 message has no delivery field — got %q", gotDelivery)
	}
	if gotBodyType != "parts" || len(gotParts) == 0 {
		t.Fatalf("V1 body must be parts-based — got type=%q parts=%v", gotBodyType, gotParts)
	}
	if gotParts[0]["type"] != "text" || gotParts[0]["text"] != "hello" {
		t.Fatalf("V1 body part must be {type:text, text:hello} — got %v", gotParts[0])
	}
	if msgID != "msg_v1_123" {
		t.Fatalf("message ID must come from info.id (V1 shape) — got %q", msgID)
	}
}

// TestAdmitterV2EndpointNotUsed asserts the V2 endpoint is never touched
// even when the model is set (the full Admit flow with model pinning).
func TestAdmitterV2EndpointNotUsed(t *testing.T) {
	v2Called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/ses/prompt" {
			v2Called = true
			t.Error("V2 prompt endpoint called — the admitter must use V1 (#1313)")
		}
		switch r.URL.Path {
		case "/api/session/ses/model":
			w.WriteHeader(http.StatusOK)
		case "/session/ses/message":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"id": "msg_ok"},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	adm := opencodeAdmitter{password: "test"}
	agentAddrAtomic.Store(srv.URL)

	_, err := adm.Admit(context.Background(), "ses", "msg_ob_t", "test", "thekaocloud/glm-5.3")
	if err != nil {
		t.Fatalf("Admit with model failed: %v", err)
	}
	if v2Called {
		t.Fatal("V2 endpoint was called despite the #1313 fix")
	}
}
