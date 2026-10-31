// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_worker_comms_test.go — the #1959 delivery-seam WIRING pins.
//
// The #1959 root cause: the bounce re-dispatch seam (and the halt/resume
// directive messenger) delivered via adapter.SendAsync — opencode's V2
// prompt route (POST /api/session/:sid/prompt, delivery:queue) — which is
// DEAD on the pinned opencode 1.18.x (#755, CLOSED SEV1: the v2/ route
// group was deleted in 1.18.10; every V2 admission 404s). The existing
// fake-based TestBounce1959_* pins faked the adapter, so they could not
// see the HTTP route the production adapter actually rides — every bounce
// deterministically fell back to a fresh spawn (the owner's token-drain
// concern) while the fakes stayed green.
//
// These pins drive the PRODUCTION constructors against a stub opencode
// that mirrors the 1.18.x reality: V1 POST /session/:id/message is LIVE,
// the V2 prompt route 404s. Born RED against the SendAsync wiring (the
// re-dispatch fails against the stub even though V1 answers), green only
// on the V1-carrying path.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// stubOpencode is the 1.18.x-shaped backend: V1 answers, V2 is gone.
type stubOpencode struct {
	*httptest.Server
	mu sync.Mutex

	v1Requests int           // POST /session/:id/message count
	v2Requests int           // POST /api/session/:id/prompt count (must stay 0)
	v1Texts    []string      // the text parts posted to V1, in order
	v1Status   int           // V1 response status (0 → 200; 404 = dead session)
	responded  chan struct{} // closed-per-response signal (delay-then-respond)
	once       sync.Once     // responded channel construction
	v1Delay    time.Duration // hold a V1 request before responding (the slow-turn shape)
}

func newStubOpencode(t *testing.T) *stubOpencode {
	t.Helper()
	s := &stubOpencode{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && isV1MessagePath(r.URL.Path):
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var texts []string
			for _, p := range body.Parts {
				if p.Type == "text" {
					texts = append(texts, p.Text)
				}
			}
			s.mu.Lock()
			s.v1Requests++
			s.v1Texts = append(s.v1Texts, strings.Join(texts, "\n"))
			status := s.v1Status
			delay := s.v1Delay
			s.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusOK {
				// The V1 message wire the adapter's ParseMessageWire
				// consumes (same shape as the handlers e2e stub).
				_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_v1_reply","time":{"created":1786400000000}},"parts":[{"type":"text","text":"V1 reply"}]}`))
			}
			s.signal()
		case r.Method == http.MethodPost && isV2PromptPath(r.URL.Path):
			// opencode 1.18.10: the v2/ route group is DELETED — every
			// V2 admission meets a 404 (#755).
			s.mu.Lock()
			s.v2Requests++
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			s.signal()
		default:
			w.WriteHeader(http.StatusNotFound)
			s.signal()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubOpencode) respondedSignal() <-chan struct{} {
	s.once.Do(func() { s.responded = make(chan struct{}, 64) })
	return s.responded
}

func (s *stubOpencode) signal() {
	s.respondedSignal() // ensure the channel exists
	s.responded <- struct{}{}
}

func (s *stubOpencode) counts() (v1, v2 int, texts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v1Requests, s.v2Requests, append([]string(nil), s.v1Texts...)
}

// isV1MessagePath matches POST /session/:id/message (the live route).
func isV1MessagePath(p string) bool {
	return strings.HasPrefix(p, "/session/") && strings.HasSuffix(p, "/message")
}

// isV2PromptPath matches POST /api/session/:id/prompt (the dead route).
func isV2PromptPath(p string) bool {
	return strings.HasPrefix(p, "/api/session/") && strings.HasSuffix(p, "/prompt")
}

// newBoardCommsAdapter builds the PRODUCTION opencode.Adapter pointed at
// the stub (static pod-IP/password resolvers + WithAdapterPort — the
// pkg/agent/opencode adapter_test.go pattern, mirrored here because its
// helpers are package-private).
func newBoardCommsAdapter(t *testing.T, srv *httptest.Server) *agentoc.Adapter {
	t.Helper()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.SplitN(hostPort, ":", 2)
	port, err := strconv.Atoi(parts[1])
	require.NoError(t, err, "parse stub server port from %s", srv.URL)
	pw := agentoc.PasswordResolver(func(_ context.Context, _ string) (string, error) {
		return "test-password", nil
	})
	ip := &staticBoardPodIP{ip: parts[0]}
	return agentoc.NewAdapter(pw, ip, zap.NewNop(),
		agentoc.WithAdapterHTTPClient(srv.Client()),
		agentoc.WithAdapterPort(port),
	)
}

type staticBoardPodIP struct{ ip string }

func (p *staticBoardPodIP) GetWorkspacePodIP(_ context.Context, _, _ string) (string, error) {
	return p.ip, nil
}

// newBoardTestSender wraps the production adapter in the #1959c
// resolving sender with a FAIL-LOUD phrase lookup: these route pins
// address sessions by RAW ids only, so the registry must never be
// consulted (a raw passthrough that suddenly consulted would fail the
// pin loudly instead of silently passing).
func newBoardTestSender(adapter *agentoc.Adapter) *boardResolvingSender {
	return newBoardResolvingSender(adapter, func(_ context.Context, _, phrase string) (string, error) {
		return "", fmt.Errorf("unexpected phrase-registry lookup for %q in a raw-id route pin", phrase)
	})
}

// stubScope is the injected boardScopeResolver: fixed addressing keys,
// recording the task ref it resolved.
type stubScope struct {
	mu   sync.Mutex
	refs []string
	err  error
}

func (s *stubScope) resolve(_ context.Context, taskRef string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs = append(s.refs, taskRef)
	if s.err != nil {
		return "", "", s.err
	}
	return "u-owner", "ws-target", nil
}

func (s *stubScope) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.refs...)
}

const (
	redispatchText = "BOARD BOUNCE RE-DISPATCH: your task tsk-wiring-pin failed review. Your FRESH claim token: tok-fresh"
	wireShortID    = "tsk-wiring-pin"
)

// TestBoardWorkerComms_Redispatcher_RidesV1NotTheDeadV2Queue is THE
// #1959 wiring pin: the production re-dispatch seam must deliver over
// the LIVE V1 route (POST /session/:id/message) and must never touch
// the dead V2 queue route. Born RED against the SendAsync wiring — the
// stub implements V1 and 404s V2, so the old seam errors (the
// deterministic fresh-worker fallback observed live on 0.44.2) even
// though a live V1 endpoint sits right next to it.
func TestBoardWorkerComms_Redispatcher_RidesV1NotTheDeadV2Queue(t *testing.T) {
	stub := newStubOpencode(t)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	redispatch := newBoardRedispatcher(scope.resolve, newBoardTestSender(adapter))

	err := redispatch(context.Background(), "ses_original", redispatchText, wireShortID)
	require.NoError(t, err,
		"the re-dispatch must deliver over the LIVE V1 route — an error here is the dead V2 queue (#755) firing the fresh-worker fallback")

	v1, v2, texts := stub.counts()
	assert.Equal(t, 1, v1, "V1 POST /session/:id/message must be hit exactly once")
	assert.Equal(t, 0, v2, "V2 POST /api/session/:id/prompt must NEVER be hit (dead on opencode 1.18.x, #755)")
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "BOARD BOUNCE RE-DISPATCH", "the V1 body must carry the re-dispatch turn text")
	assert.Contains(t, texts[0], "tok-fresh", "the fresh bounce claim token must ride the delivered text")
	assert.Equal(t, []string{wireShortID}, scope.recorded(), "the seam must resolve scope through the task ref")
}

// TestBoardWorkerComms_Redispatcher_DeadSession_FastFailKeepsFallback
// pins the fallback contract: an immediate V1 4xx on a dead/reaped
// session MUST surface as a synchronous seam error — pkg/board's
// fresh-worker fallback keys on it (the engine's decision tree is
// correct; only the transport was broken).
func TestBoardWorkerComms_Redispatcher_DeadSession_FastFailKeepsFallback(t *testing.T) {
	stub := newStubOpencode(t)
	stub.mu.Lock()
	stub.v1Status = http.StatusNotFound // the session is gone; V1 answers 404
	stub.mu.Unlock()
	adapter := newBoardCommsAdapter(t, stub.Server)
	redispatch := newBoardRedispatcher((&stubScope{}).resolve, newBoardTestSender(adapter))

	start := time.Now()
	err := redispatch(context.Background(), "ses_reaped", redispatchText, wireShortID)
	elapsed := time.Since(start)
	require.Error(t, err, "a dead session's immediate 4xx must return error so pkg/board spawns a fresh worker")
	assert.Less(t, elapsed, boardV1FastFail, "the fast-fail verdict must arrive within the window, not after the turn budget")
	assert.Contains(t, err.Error(), "/session/ses_reaped/message",
		"the error must name the V1 route (the liveness answer's provenance)")
}

// TestBoardWorkerComms_Redispatcher_SlowTurnIsInFlight_NotCallerBlocked
// pins the dispatch context discipline: a live session's re-dispatch
// turn routinely runs minutes — the seam must return nil (in-flight)
// once the fast-fail window expires, the caller (the reviewer's verdict
// request) must not block on Round 2, and the DETACHED delivery must
// survive the caller's ctx ending (the send_message WithoutCancel
// discipline — canceling the caller must not kill the re-dispatched
// turn mid-flight).
func TestBoardWorkerComms_Redispatcher_SlowTurnIsInFlight_NotCallerBlocked(t *testing.T) {
	restore := boardV1FastFail
	boardV1FastFail = 100 * time.Millisecond
	t.Cleanup(func() { boardV1FastFail = restore })

	stub := newStubOpencode(t)
	stub.mu.Lock()
	stub.v1Delay = 600 * time.Millisecond // the slow-turn shape: the POST pends
	stub.mu.Unlock()
	adapter := newBoardCommsAdapter(t, stub.Server)
	redispatch := newBoardRedispatcher((&stubScope{}).resolve, newBoardTestSender(adapter))

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	err := redispatch(ctx, "ses_original", redispatchText, wireShortID)
	elapsed := time.Since(start)
	require.NoError(t, err, "a window expiry is NOT a delivery failure — nil means in-flight, no fresh-worker fallback")
	assert.Less(t, elapsed, 500*time.Millisecond, "the caller must return at the fast-fail window, not wait out the turn")

	// The caller leaves; the detached V1 delivery must still complete.
	cancel()
	select {
	case <-stub.respondedSignal():
	case <-time.After(5 * time.Second):
		t.Fatal("the detached V1 delivery did not complete after the caller's ctx was canceled — the re-dispatched turn died with the request")
	}
	v1, v2, _ := stub.counts()
	assert.Equal(t, 1, v1, "the detached delivery must land the V1 POST")
	assert.Equal(t, 0, v2)
}

// TestBoardWorkerComms_Messenger_HaltAndResume_RideV1Detached pins the
// same-disease fix: the pause/resume directives rode the SAME dead V2
// route (halt was never live-verified on 1.18.x). The messenger is
// fire-and-forget — the pause request must not block on the worker's
// turn — and both directive texts must land on the LIVE V1 route.
func TestBoardWorkerComms_Messenger_HaltAndResume_RideV1Detached(t *testing.T) {
	stub := newStubOpencode(t)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	messenger := newBoardMessenger(scope.resolve, newBoardTestSender(adapter))

	start := time.Now()
	require.NoError(t, messenger(context.Background(), "ses_worker", "halt", wireShortID))
	assert.Less(t, time.Since(start), 500*time.Millisecond,
		"a directive send is fire-and-forget — the pause request must not block on the worker's turn")

	// Wait for the detached delivery, then assert route + text.
	require.Eventually(t, func() bool {
		v1, _, _ := stub.counts()
		return v1 >= 1
	}, 5*time.Second, 20*time.Millisecond, "the halt directive must be delivered over V1")

	require.NoError(t, messenger(context.Background(), "ses_worker", "resume", wireShortID))
	require.Eventually(t, func() bool {
		v1, _, _ := stub.counts()
		return v1 >= 2
	}, 5*time.Second, 20*time.Millisecond, "the resume directive must be delivered over V1")

	v1, v2, texts := stub.counts()
	assert.Equal(t, 2, v1, "both directives must land on V1 POST /session/:id/message")
	assert.Equal(t, 0, v2, "no directive may touch the dead V2 route (#755)")
	require.Len(t, texts, 2)
	assert.Contains(t, texts[0], "BOARD PAUSE DIRECTIVE", "the halt text must carry the pause contract")
	assert.Contains(t, texts[0], wireShortID)
	assert.Contains(t, texts[1], "BOARD RESUME DIRECTIVE", "the resume text must carry the resume contract")
	assert.Contains(t, texts[1], wireShortID)
}

// TestBoardWorkerComms_Messenger_Deposed_RideV1Detached pins the S5
// DEPOSED directive's transport: the same V1 detached seam as
// halt/resume (the HALT/RESUME family — the transport precedent), with
// the deposed text carrying the stop contract (the token is revoked;
// the salvage pointer lives on the row for the inheritor).
func TestBoardWorkerComms_Messenger_Deposed_RideV1Detached(t *testing.T) {
	stub := newStubOpencode(t)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{}
	messenger := newBoardMessenger(scope.resolve, newBoardTestSender(adapter))

	require.NoError(t, messenger(context.Background(), "ses_worker", "deposed", wireShortID))
	require.Eventually(t, func() bool {
		v1, _, _ := stub.counts()
		return v1 >= 1
	}, 5*time.Second, 20*time.Millisecond, "the deposed directive must be delivered over V1")

	v1, v2, texts := stub.counts()
	assert.Equal(t, 1, v1, "the deposed directive lands on V1 POST /session/:id/message")
	assert.Equal(t, 0, v2, "no directive may touch the dead V2 route (#755)")
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "BOARD DEPOSED DIRECTIVE", "the deposed text must carry the deposed contract")
	assert.Contains(t, texts[0], wireShortID)
	assert.Contains(t, texts[0], "salvage pointer", "the text names what was recorded for the inheritor")
	assert.Contains(t, texts[0], "409 claim_token", "the deposed worker is told its writes now refuse")
}

// TestBoardWorkerComms_ScopeErrorPropagatesToFallback: an unresolvable
// task scope is a synchronous seam error — pkg/board's fallback arm.
func TestBoardWorkerComms_ScopeErrorPropagatesToFallback(t *testing.T) {
	stub := newStubOpencode(t)
	adapter := newBoardCommsAdapter(t, stub.Server)
	scope := &stubScope{err: fmt.Errorf("no such task")}
	redispatch := newBoardRedispatcher(scope.resolve, newBoardTestSender(adapter))

	err := redispatch(context.Background(), "ses_original", redispatchText, wireShortID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "re-dispatch scope", "the error must be attributable to the scope leg")

	v1, v2, _ := stub.counts()
	assert.Equal(t, 0, v1, "no delivery may fire when the scope fails")
	assert.Equal(t, 0, v2)
}
