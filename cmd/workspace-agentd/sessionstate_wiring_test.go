package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #1288: the admission seam uses the TUI's delivery semantics — "steer".
// "queue" on the pinned opencode 1.18.10 never drains (#755: messages
// vanished; the API's adapter path abandoned it for the same reason) and
// the #1288 incident ran on queue-mode admission. A regression to "queue"
// must fail here first.
func TestOpencodeAdmitter_UsesV1MessagePath(t *testing.T) {
	// #1313: V1 message path, not V2 steer. The V2 session runner on
	// opencode 1.18.15 does NOT include MCP tools in the model's function
	// definitions — every platform-delivered message through V2 silently
	// lost workspace tools. V1 is the HTTP surface that matches the TUI's
	// behavior (the TUI calls the session runner directly).
	var gotPath string
	var gotBodyType string
	var gotMessageID any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["parts"]; ok {
			gotBodyType = "parts"
		}
		if _, ok := body["delivery"]; ok {
			gotBodyType = "V2_SHAPE"
		}
		gotMessageID = body["messageID"]
		_, _ = w.Write([]byte(`{"info":{"id":"msg_x"}}`))
	}))
	defer srv.Close()
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)

	a := opencodeAdmitter{password: "pw"}
	id, err := a.Admit(context.Background(), "ses_1", "msg_ob_w1", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "msg_x" {
		t.Fatalf("messageID roundtrip: %q", id)
	}
	// S2 (#1315): the entry-derived dedupe key MUST ride the POST body —
	// the pinned harness uses it verbatim as the user message's store ID
	// and upserts on collision, so dropping the field reopens the 16-copy
	// class (every re-POST an unconditional transcript append).
	if gotMessageID != "msg_ob_w1" {
		t.Fatalf("body messageID = %v — the entry dedupe key must ride the V1 POST body (S2)", gotMessageID)
	}
	if gotPath != "/session/ses_1/message" {
		t.Fatalf("path = %q — must be the V1 message endpoint, not V2 prompt (#1313)", gotPath)
	}
	if gotBodyType != "parts" {
		t.Fatalf("body shape = %q — must be parts-based V1, not V2 prompt/delivery (#1313)", gotBodyType)
	}
}

// #1292b: a model ref makes Admit POST the session-model FIRST (the V2
// prompt endpoint strips per-prompt overrides — verified live), with the
// object wire form; provider/id refs split; a rejected model fails the
// admission closed.
func TestOpencodeAdmitter_SetsSessionModelBeforeSteer(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/session/ses_1/model":
			order = append(order, "model:"+mustJSON(t, body))
			w.WriteHeader(200)
		case "/session/ses_1/message":
			order = append(order, "prompt")
			_, _ = w.Write([]byte(`{"info":{"id":"msg_x"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)

	a := opencodeAdmitter{password: "pw"}
	id, err := a.Admit(context.Background(), "ses_1", "msg_ob_w1", "hello", "thekaocloud/glm-5.3")
	if err != nil || id != "msg_x" {
		t.Fatalf("admit: %v %q", err, id)
	}
	if len(order) != 2 || order[0] == "prompt" {
		t.Fatalf("model must be set BEFORE the prompt: %v", order)
	}
	if !strings.Contains(order[0], `"providerID":"thekaocloud"`) || !strings.Contains(order[0], `"id":"glm-5.3"`) {
		t.Fatalf("model wire form wrong: %s", order[0])
	}
}

func TestOpencodeAdmitter_ModelSetFailureFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/session/ses_1/model" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"unknown model"}`))
			return
		}
		t.Errorf("the prompt must never fire when the model set fails")
	}))
	defer srv.Close()
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)

	a := opencodeAdmitter{password: "pw"}
	if _, err := a.Admit(context.Background(), "ses_1", "msg_ob_w1", "hello", "bogus-model"); err == nil {
		t.Fatal("a rejected model must fail the admission loudly, not run the session default")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return string(b)
}
