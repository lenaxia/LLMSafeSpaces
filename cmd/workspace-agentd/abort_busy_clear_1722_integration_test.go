// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package main

// abort_busy_clear_1722_integration_test.go — THE #1722 deterministic
// repro at the fullest reachable layer: the REAL pinned opencode
// (StartIntegrationServer), a REAL long-running tool turn (mock
// provider → bash sleep), a REAL user abort mid-tool — with the abort's
// ~30ms clear burst (session.error + session.status idle + session.idle
// + the tool's terminal update; wire-proven against both 1.18.10 and
// 1.18.15, see the worklog) DROPPED at exactly the boundary where
// production loses it: the agentd /event subscription's replay-less
// recycle gap. Assert the busy-truth machinery converges anyway:
//
//   - the authority PUBLISHES the evidence settle (the API bridge's
//     input — without it the user stream never carries idle and the
//     badge sticks busy, the reported bug);
//   - the tracker settles (restart gating, the busy gauge, the #1685
//     evidence clock stop carrying the fiction);
//   - the derived busy is false (the projection-side heal).
//
// RED pre-fix: the publication never happens (the evidence busy-clear
// mutates silently) and the tracker stays busy forever.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// --- the mock provider: first user turn calls bash sleep, later calls
// answer with text (title generation and post-tool steps). Keyed on the
// request body (the counter approach loses to opencode's own
// title-generation call).

type abort1722Mock struct {
	mu   sync.Mutex
	reqs int
}

func (m *abort1722Mock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close") // the pinned runtime's keep-alive quirk
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		m.mu.Lock()
		m.reqs++
		m.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); flusher.Flush() }
		if strings.Contains(string(body), "run the sleep tool") {
			send(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"mockmodel","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"sleep 120\",\"timeout\":300000}"}}]}}]}`)
			send(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"mockmodel","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`)
		} else {
			send(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"mockmodel","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"done"}}]}`)
			send(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"mockmodel","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":60,"completion_tokens":4,"total_tokens":64}}`)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// dropClearBurstProxy forwards everything to the real opencode but, once
// armed, drops the abort clear-burst frames on /event — the exact class
// of frames a recycle-gap reconnect loses in production.
type dropClearBurstProxy struct {
	target  string
	mu      sync.Mutex
	armed   bool
	dropped int
}

func (p *dropClearBurstProxy) arm() {
	p.mu.Lock()
	p.armed = true
	p.mu.Unlock()
}

func (p *dropClearBurstProxy) dropCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

func (p *dropClearBurstProxy) isClearBurstFrame(payload string) bool {
	if !strings.Contains(payload, `"session"`) && !strings.Contains(payload, `"sessionID"`) {
		return false
	}
	switch {
	case strings.Contains(payload, `"type": "session.error"`),
		strings.Contains(payload, `"type":"session.error"`):
		return true
	case strings.Contains(payload, `"type": "session.idle"`),
		strings.Contains(payload, `"type":"session.idle"`):
		return true
	case strings.Contains(payload, `"session.status"`):
		return strings.Contains(payload, `"idle"`)
	}
	return false
}

func (p *dropClearBurstProxy) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			p.serveEvent(w, r)
			return
		}
		p.forward(w, r)
	}
}

// forward is the plain reverse pass-through for every non-/event
// route: same method, headers, body, status. (httputil.ReverseProxy
// intermittently delivered empty bodies against the pinned runtime —
// the store-reader's strict decode surfaced it as EOF.)
func (p *dropClearBurstProxy) forward(w http.ResponseWriter, r *http.Request) {
	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	upstream, err := http.NewRequest(r.Method, "http://"+p.target+r.URL.RequestURI(), body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	upstream.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(upstream)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *dropClearBurstProxy) serveEvent(w http.ResponseWriter, r *http.Request) {
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+p.target+"/event", nil)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if u, pw, ok := r.BasicAuth(); ok {
		upstream.SetBasicAuth(u, pw)
	}
	upstream.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(upstream)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var frame strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if frame.Len() > 0 {
				payload := frame.String()
				p.mu.Lock()
				armed := p.armed
				p.mu.Unlock()
				if armed && p.isClearBurstFrame(payload) {
					p.mu.Lock()
					p.dropped++
					p.mu.Unlock()
				} else {
					fmt.Fprintf(w, "data: %s\n\n", payload)
					if flusher != nil {
						flusher.Flush()
					}
				}
				frame.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			frame.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

// claimPort walks forward from want until a port binds (the loopback
// lesson: the pinned runtime's outbound fetch fails against ephemeral
// ports — the mock and proxy must sit on claimed fixed ports).
func claimPort1722(t *testing.T, want int) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", want+i))
		if err != nil {
			continue
		}
		_ = l.Close()
		return want + i
	}
	t.Fatalf("no free port near %d", want)
	return 0
}

// TestIntegration1722_AbortedLongToolSettlesBusy is the issue's
// acceptance repro: abort a long-running tool call mid-turn; the clear
// burst is lost (the recycle gap, modeled deterministically by the
// proxy); assert busy=false after the turn ends — at the tracker, the
// projection, AND the publication seam the bridge consumes.
func TestIntegration1722_AbortedLongToolSettlesBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("integration row: boots the real opencode binary")
	}

	mockPort := claimPort1722(t, 14210)
	proxyPort := claimPort1722(t, 14230)

	mockLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mockPort))
	require.NoError(t, err)
	mock := &abort1722Mock{}
	mockSrv := &http.Server{Handler: mock.handler()}
	go mockSrv.Serve(mockLn)
	t.Cleanup(func() { _ = mockSrv.Close() })

	cfg := fmt.Sprintf(`{
		"provider": {
			"mock": {
				"options": {"apiKey": "test", "baseURL": "http://127.0.0.1:%d/v1"},
				"models": {"mockmodel": {"name": "Mock Model", "attachment": true, "limit": {"context": 100000, "output": 4096}}}
			}
		},
		"model": "mock/mockmodel"
	}`, mockPort)
	srv := opencode.StartIntegrationServer(t, 0, cfg)
	targetHost := strings.TrimPrefix(srv.IntegrationBaseURL(), "http://")

	proxyLn, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	require.NoError(t, err)
	proxy := &dropClearBurstProxy{target: targetHost}
	proxySrv := &http.Server{Handler: proxy.handler()}
	go proxySrv.Serve(proxyLn)
	t.Cleanup(func() { _ = proxySrv.Close() })

	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))

	const password = "test-password"
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// The production wiring: the tracker feeds the authority every raw
	// /event payload; the store reader + busy truth come from the same
	// client (through the proxy — the store/registry endpoints pass
	// through untouched; only the clear burst drops).
	tracker := newSessionStatusTracker()
	// The authority wired exactly like production (integrationAuthority's
	// helper hardcodes a fake-harness password; the real binary booted
	// with test-password).
	t.Setenv("LLMSAFESPACES_PLATFORM_DIR", t.TempDir())
	authority := newStateAuthority(client, password, "")
	require.NotNil(t, authority)
	t.Cleanup(func() { _ = authority.Close() })
	tracker.onRawEvent = authority.Ingest
	busyTruth := busyTruthFrom(authority)

	bgCtx, bgCancel := context.WithCancel(context.Background())
	t.Cleanup(bgCancel)
	go tracker.subscribe(bgCtx, client)

	// Create a session through the proxy.
	resp, err := postRaw1722(password, fmt.Sprintf("http://127.0.0.1:%d/session", proxyPort), map[string]any{})
	require.NoError(t, err)
	var sess struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	_ = resp.Body.Close()
	require.NotEmpty(t, sess.ID, "session create failed")
	t.Logf("session %s", sess.ID)

	// Subscribe to the authority's stream (the bridge's transport) BEFORE
	// the turn, and count settle publications.
	frames, stop, err := authority.Stream(bgCtx)
	require.NoError(t, err)
	defer stop()
	settled := make(chan *abiv1.Event, 4)
	go func() {
		for f := range frames {
			if seq := f.GetEvent(); seq != nil {
				if evt := seq.GetEvent(); evt.GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS && evt.GetSessionId() == sess.ID {
					settled <- evt
				}
			}
		}
	}()

	// The turn: a ~120s blocking tool, aborted ~2s in. The send blocks
	// until the turn ends.
	sendDone := make(chan error, 1)
	go func() {
		r, perr := postRaw1722(password, fmt.Sprintf("http://127.0.0.1:%d/session/%s/message", proxyPort, sess.ID),
			map[string]any{"parts": []map[string]any{{"type": "text", "text": "run the sleep tool"}}})
		if perr == nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		sendDone <- perr
	}()

	// Both layers observe the live turn.
	require.Eventually(t, func() bool { return tracker.get(sess.ID) == "busy" },
		30*time.Second, 50*time.Millisecond, "tracker never marked the turn busy")
	require.Eventually(t, func() bool {
		st := authority.State().Sessions[sess.ID]
		return st != nil && st.Busy
	}, 30*time.Second, 50*time.Millisecond, "authority never derived busy")

	// Drain the turn's own publications (session.status busy markers)
	// so the settle channel carries ONLY post-reconcile frames.
	for {
		select {
		case <-settled:
			continue
		default:
		}
		break
	}

	// Arm the burst drop, then abort — the clear events leave opencode
	// but never reach agentd.
	proxy.arm()
	ab, aerr := postRaw1722(password, fmt.Sprintf("http://127.0.0.1:%d/session/%s/abort", proxyPort, sess.ID), map[string]any{})
	require.NoError(t, aerr)
	_, _ = io.Copy(io.Discard, ab.Body)
	_ = ab.Body.Close()
	require.Equal(t, http.StatusOK, ab.StatusCode, "abort must land")

	select {
	case err := <-sendDone:
		require.NoError(t, err, "the aborted send must return")
	case <-time.After(30 * time.Second):
		t.Fatal("send never returned after abort")
	}
	require.Eventually(t, func() bool { return proxy.dropCount() >= 2 },
		10*time.Second, 100*time.Millisecond, "the proxy must have dropped the clear burst (got %d)", proxy.dropCount())

	// THE SETTLE SIGNAL: store evidence (session list idle-biased +
	// live registry clear — both proven post-abort on the wire) says the
	// turn ended. Run the production watchdog's pass.
	stats := authority.Reconcile(context.Background())
	assert.Equal(t, sessionstate.ReconcileStats{BusyCleared: 1}, stats, "the projection-side heal")

	// Control (passes pre-fix): the derived busy converged.
	require.Eventually(t, func() bool {
		st := authority.State().Sessions[sess.ID]
		return st == nil || !st.Busy
	}, 5*time.Second, 50*time.Millisecond, "derived busy must converge after the evidence settle")

	// PIN 1 — the publication: the bridge's input frame arrived.
	select {
	case evt := <-settled:
		assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, evt.GetStatus(),
			"the published settle must carry the evidence status")
	case <-time.After(5 * time.Second):
		t.Fatal("#1722: the evidence settle was never PUBLISHED — the user stream cannot carry idle (stuck-busy badge)")
	}

	// PIN 2 — the tracker settle: the composition (the statusz/sessions
	// render) converges the raw mark too.
	cache := &providerCache{}
	_, _, _ = cachedState(context.Background(), client, cache, tracker, busyTruth)
	assert.Eventually(t, func() bool { return tracker.get(sess.ID) == "idle" },
		5*time.Second, 50*time.Millisecond,
		"#1722: the tracker mark must settle — else restart gating and the busy gauge carry the fiction forever")
}

func postRaw1722(password, url string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("opencode", password)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 0}
	return client.Do(req)
}
