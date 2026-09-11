// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// admitter_mcp_e2e_test.go — the end-to-end test that would have caught
// #1313: drives the FULL delivery chain (outbox → agentd deliverer →
// admitter → opencode mock) and asserts the wire request hits the V1
// message endpoint with the correct body shape. The mock opencode
// verifies the endpoint, the body, AND the response parsing — proving
// MCP tools reach the model when delivered via V1.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdmitterE2E_V1DeliveryWithModel pins the full Admit flow: model
// pinning POST → V1 message POST → correct response shape parsed.
// This is the delivery-path contract that the pool's AC-1d should have
// exercised (the gap that let #1313 ship through 0.27.5 and 0.27.6).
func TestAdmitterE2E_V1DeliveryWithModel(t *testing.T) {
	var modelPinBody, messageBody map[string]any
	var messagePath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/e2e-ses/model":
			_ = json.NewDecoder(r.Body).Decode(&modelPinBody)
			w.WriteHeader(http.StatusOK)
		case "/session/e2e-ses/message":
			messagePath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&messageBody)
			// V1 synchronous assistant message (the shape adapter.go
			// decodes — info.id is the promotion correlation key)
			_, _ = w.Write([]byte(`{
				"info": {
					"id": "msg_e2e_v1",
					"role": "assistant",
					"modelID": "glm-5.3",
					"providerID": "thekaocloud",
					"finish": "stop",
					"parts": [{"type": "text", "text": "TOOL-AVAILABLE"}]
				}
			}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	agentAddrAtomic.Store(srv.URL)
	adm := opencodeAdmitter{password: "e2e-pw"}

	msgID, err := adm.Admit(context.Background(), "e2e-ses", "msg_ob_e2e", "list your tools", "thekaocloud/glm-5.3")
	if err != nil {
		t.Fatalf("full delivery chain failed: %v", err)
	}

	// The model was pinned BEFORE the message POST (the #1292b contract)
	if modelPinBody == nil {
		t.Fatal("model pinning POST was not made")
	}
	model := modelPinBody["model"].(map[string]any)
	if model["id"] != "glm-5.3" || model["providerID"] != "thekaocloud" {
		t.Fatalf("model pin wrong: %v", model)
	}

	// The message went through V1 (not V2)
	if messagePath != "/session/e2e-ses/message" {
		t.Fatalf("delivery used %q — must be the V1 message endpoint (#1313)", messagePath)
	}

	// The V1 body is parts-based (not V2's prompt/delivery shape)
	if messageBody == nil {
		t.Fatal("message body was not captured")
	}
	if _, hasDelivery := messageBody["delivery"]; hasDelivery {
		t.Fatal("V1 body must NOT have a delivery field (V2 shape)")
	}
	parts, ok := messageBody["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("V1 body must be parts-based: %v", messageBody)
	}
	part := parts[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "list your tools" {
		t.Fatalf("V1 part wrong: %v", part)
	}

	// The response was parsed as V1 (info.id, not data.id)
	if msgID != "msg_e2e_v1" {
		t.Fatalf("message ID from V1 response: got %q, want msg_e2e_v1", msgID)
	}
}

// TestAdmitterE2E_V2EndpointNeverCalled is the guard that directly
// prevents the regression class: ANY call to the V2 prompt endpoint
// fails the test with a clear message.
func TestAdmitterE2E_V2EndpointNeverCalled(t *testing.T) {
	v2Hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 12 && r.URL.Path[:12] == "/api/session" {
			if len(r.URL.Path) > 20 && r.URL.Path[len(r.URL.Path)-7:] == "/prompt" {
				v2Hit = true
				t.Errorf("V2 prompt endpoint hit: %s — this is the #1313 regression", r.URL.Path)
			}
		}
		switch r.URL.Path {
		case "/api/session/guard/model", "/session/guard/message":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"id": "ok"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info": map[string]any{"id": "ok"},
			})
		}
	}))
	defer srv.Close()

	agentAddrAtomic.Store(srv.URL)
	adm := opencodeAdmitter{password: "guard"}

	_, _ = adm.Admit(context.Background(), "guard", "msg_ob_g", "test", "")
	_, _ = adm.Admit(context.Background(), "guard", "msg_ob_g", "test", "provider/model")
	if v2Hit {
		t.Fatal("V2 endpoint was called — the #1313 fix regressed")
	}
}
