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
func TestOpencodeAdmitter_UsesSteerDelivery(t *testing.T) {
	var gotDelivery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Delivery string `json:"delivery"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotDelivery = body.Delivery
		_, _ = w.Write([]byte(`{"data":{"id":"msg_x"}}`))
	}))
	defer srv.Close()
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)

	a := opencodeAdmitter{password: "pw"}
	id, err := a.Admit(context.Background(), "ses_1", "hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != "msg_x" {
		t.Fatalf("messageID roundtrip: %q", id)
	}
	if gotDelivery != "steer" {
		t.Fatalf("delivery mode = %q, want \"steer\" (the TUI semantics; queue never drains on the pinned opencode, #755/#1288)", gotDelivery)
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
		case "/api/session/ses_1/prompt":
			order = append(order, "prompt")
			_, _ = w.Write([]byte(`{"data":{"id":"msg_x"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	agentAddrAtomic.Store(srv.URL)

	a := opencodeAdmitter{password: "pw"}
	id, err := a.Admit(context.Background(), "ses_1", "hello", "thekaocloud/glm-5.3")
	if err != nil || id != "msg_x" {
		t.Fatalf("admit: %v %q", err, id)
	}
	if len(order) != 2 || order[0] == "prompt" {
		t.Fatalf("model must be set BEFORE the prompt: %v", order)
	}
	if !strings.Contains(order[0], `"provider":"thekaocloud"`) || !strings.Contains(order[0], `"id":"glm-5.3"`) {
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
	if _, err := a.Admit(context.Background(), "ses_1", "hello", "bogus-model"); err == nil {
		t.Fatal("a rejected model must fail the admission loudly, not run the session default")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return string(b)
}
