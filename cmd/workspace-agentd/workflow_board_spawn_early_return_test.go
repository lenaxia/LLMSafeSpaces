// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workflow_board_spawn_early_return_test.go — the 2026-10-29 live-fire
// regression pins: a board-worker spawn must return the session
// IDENTITY immediately, with the charter message delivery detached —
// never hostage to the worker's first-turn completion.
//
// The incident (v0.44.8, two workers): the agent-node handler blocked
// on opencode's message endpoint, which answers at TURN COMPLETION;
// first turns longer than the API executor's 15-minute client timeout
// made the client error out, spawnSync skip BOTH identity stamps
// (assignee_session + the session_origins worker mark), and the
// fully-working worker ran on as an anonymous orphan — no card link,
// no busy indicator, no Workers-folder classification, the lease
// ladder blind to its row.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpencodeHarness is an opencode stand-in whose message leg PARKS
// until released — the "first turn still running" shape.
type fakeOpencodeHarness struct {
	srv *httptest.Server

	mu             sync.Mutex
	messageCalls   int
	messageStarted chan struct{} // signaled (once) when the message POST arrives
	releaseOnce    sync.Once
	release        chan struct{} // closed (idempotently) to let the message leg finish
	messageDone    chan struct{} // closed when the message handler returns
	status         int           // the message leg's response status (200 or 500)
}

func newFakeOpencodeHarness(t *testing.T) *fakeOpencodeHarness {
	t.Helper()
	h := &fakeOpencodeHarness{
		messageStarted: make(chan struct{}),
		release:        make(chan struct{}),
		messageDone:    make(chan struct{}),
		status:         http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent", func(w http.ResponseWriter, _ *http.Request) {
		// A8 pre-validation source: the parked fake serves opencode's
		// registry (build — the specs these tests dispatch name it).
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"build"}]`)
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"ses_worker_1"}`)
	})
	mux.HandleFunc("POST /session/ses_worker_1/message", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.messageCalls++
		first := h.messageCalls == 1
		status := h.status
		h.mu.Unlock()
		if first {
			close(h.messageStarted)
		}
		defer close(h.messageDone)
		<-h.release // the first turn: parked until the test says so
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		fmt.Fprint(w, `{"info":{"id":"msg_1","sessionID":"ses_worker_1"},"parts":[{"type":"text","text":"ok"}]}`)
	})
	h.srv = httptest.NewServer(mux)
	// Deadlock guard (review round 1; registration order fixed round 2):
	// ANY failure path that Fatal()s before the test drains the leg
	// (including the regression tripwire itself) must not leave the
	// parked handler holding the connection into srv.Close. t.Cleanup is
	// LIFO — registering releaseLeg AFTER srv.Close makes it RUN BEFORE
	// srv.Close, so the leg unparks first and the server closes cleanly;
	// a future regression presents as a crisp assertion failure, not a
	// CI hang.
	t.Cleanup(h.srv.Close)
	t.Cleanup(h.releaseLeg)

	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	// A8 cache hygiene: cold registry per harness (same discipline as
	// newDedupeHarness — no cross-test stub leakage).
	t.Cleanup(func() {
		agentRegistryCache.Lock()
		agentRegistryCache.password = ""
		agentRegistryCache.names = nil
		agentRegistryCache.Unlock()
	})
	agentAddrAtomic.Store(h.srv.URL)
	return h
}

// releaseLeg unblocks the parked message leg exactly once — safe from
// the harness cleanup AND any test's explicit drain.
func (h *fakeOpencodeHarness) releaseLeg() {
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *fakeOpencodeHarness) drainMessageLeg(t *testing.T) {
	t.Helper()
	h.releaseLeg()
	select {
	case <-h.messageDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached message leg never drained after release")
	}
}

// TestBoardWorker_SpawnReturnsIdentityBeforeTurnCompletes — THE pin
// (the live-fire's exact shape): a board-worker dispatch whose message
// leg is still parked must answer 200 + sessionId + dispatched WITHIN
// SECONDS, while the first turn is still in flight.
func TestBoardWorker_SpawnReturnsIdentityBeforeTurnCompletes(t *testing.T) {
	h := newFakeOpencodeHarness(t)

	type result struct {
		w  *httptest.ResponseRecorder
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		body := `{"nodeId":"board-worker","nodeType":"agent","spec":{"agent":"build","prompt":"work the charter"},"input":{},"workflowId":"wf-1","runId":"run-1"}`
		req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
		w := httptest.NewRecorder()
		workflowExecuteHandler(testAuthPassword)(w, req)
		done <- result{w: w, ok: true}
	}()

	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("board-worker spawn did NOT return while the first turn was still running — identity is hostage to turn completion (the live-fire regression)")
	}
	if res.w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", res.w.Code, res.w.Body.String())
	}
	var resp struct {
		SessionID string          `json:"sessionId"`
		Output    json.RawMessage `json:"output"`
		ErrorCode string          `json:"errorCode"`
	}
	if err := json.Unmarshal(res.w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("expected success envelope, got errorCode=%q detail=%s", resp.ErrorCode, resp.Output)
	}
	if resp.SessionID != "ses_worker_1" {
		t.Fatalf("expected sessionId ses_worker_1 (the identity the spawner stamps), got %q", resp.SessionID)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Output, &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if out["dispatched"] != true || out["session_id"] != "ses_worker_1" {
		t.Fatalf("expected dispatched=true + session_id in the output, got %v", out)
	}

	// The charter dispatch FIRED (parked in the first turn)…
	select {
	case <-h.messageStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the detached charter dispatch never fired")
	}
	h.drainMessageLeg(t)
}

// TestAgentNode_NonBoardWorkerStillBlocks — the counter-pin: a REGULAR
// agent node (its result feeds the DAG graph) keeps the blocking
// contract: no response until the turn completes.
func TestAgentNode_NonBoardWorkerStillBlocks(t *testing.T) {
	h := newFakeOpencodeHarness(t)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body := `{"nodeId":"graph-node","nodeType":"agent","spec":{"agent":"build","prompt":"summarize"},"input":{},"workflowId":"wf-1","runId":"run-1"}`
		req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
		w := httptest.NewRecorder()
		workflowExecuteHandler(testAuthPassword)(w, req)
		done <- w
	}()

	select {
	case <-done:
		t.Fatal("a non-board agent node answered BEFORE the turn completed — the blocking contract for DAG agent nodes regressed")
	case <-time.After(300 * time.Millisecond):
		// still parked: correct.
	}
	h.drainMessageLeg(t)
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 after the turn completed, got %d: %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the non-board agent node never answered after the turn completed")
	}
}

// TestBoardWorker_DeliveryFailureKeepsIdentity — a failing charter
// delivery (the worker never starts) never rewrites the spawn's
// outcome: the identity envelope already answered, the failure logs in
// the detached leg, and the lease-lapse contract owns the heal.
func TestBoardWorker_DeliveryFailureKeepsIdentity(t *testing.T) {
	h := newFakeOpencodeHarness(t)
	h.mu.Lock()
	h.status = http.StatusInternalServerError
	h.mu.Unlock()

	body := `{"nodeId":"board-worker","nodeType":"agent","spec":{"agent":"build","prompt":"work the charter"},"input":{},"workflowId":"wf-1","runId":"run-1"}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("the spawn envelope must succeed on identity alone, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string          `json:"sessionId"`
		Output    json.RawMessage `json:"output"`
		ErrorCode string          `json:"errorCode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	// The discriminating assertions (review round 1): the pre-fix
	// blocking path ALSO answers 200 + sessionId on this failure shape
	// (the #1470 error envelope) — dispatched + empty errorCode are
	// what pin the EARLY arm specifically.
	if resp.SessionID != "ses_worker_1" {
		t.Fatalf("expected sessionId ses_worker_1, got %q", resp.SessionID)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("expected empty errorCode (identity precedes delivery outcome by design), got %q", resp.ErrorCode)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Output, &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if out["dispatched"] != true {
		t.Fatalf("expected dispatched=true, got %v", out)
	}
	// Drain the failing leg — no panic, no hang.
	h.drainMessageLeg(t)
}
