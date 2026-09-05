package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
