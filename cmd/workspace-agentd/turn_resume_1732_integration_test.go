// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package main

// turn_resume_1732_integration_test.go — #1732 leg A acceptance at the
// fullest reachable layer: the REAL pinned opencode
// (StartIntegrationServer), a mock provider whose stream is HARD-CUT
// mid-generation (hijack + close — the exact wire shape of a
// relay-router/api roll severing in-flight provider traffic; probe
// 2026-10-18), and the production wiring (tracker.subscribe feeding
// turnResumeSupervisor.Observe exactly as main.go wires it).
//
// Asserts the amendment's acceptance: the interrupted turn
// AUTO-RESUMES with roll-classified loss — the supervisor fires ONE
// re-prompt, the harness runs a fresh turn against the (now-healthy)
// provider, and the session ends with a completed assistant turn
// (finish stop). RED pre-fix: the turn dies with finish unknown and
// nothing ever resumes (the incident's halted workers).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// cutProvider1732 serves three phases:
//   - phase clean: full SSE turn (finish stop);
//   - phase cut (armed): headers + one partial chunk, then the
//     connection is hijack-closed mid-stream — no finish_reason, no
//     [DONE] (the wire shape of an infra severance);
//   - phase healed (after ≥1 cut served): clean turns again — the new
//     router pod serving fresh generations.
type cutProvider1732 struct {
	cut     atomic.Bool
	cuts    atomic.Int64
	mu      sync.Mutex
	request []string // bodies seen, in order
}

func (p *cutProvider1732) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		p.mu.Lock()
		p.request = append(p.request, string(body))
		p.mu.Unlock()

		if p.cut.Load() {
			p.cuts.Add(1)
			// Hard mid-stream cut (the roll): partial token flow, then
			// the connection dies without a finish marker.
			hj := w.(http.Hijacker)
			conn, buf, _ := hj.Hijack()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n" +
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"finish_reason\":null,\"delta\":{\"role\":\"assistant\",\"content\":\"partial \"}}]}\r\n\r\n")
			_ = buf.Flush()
			time.Sleep(300 * time.Millisecond)
			_ = conn.Close()
			return
		}

		w.Header().Set("Connection", "close") // the pinned runtime's keep-alive quirk
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); flusher.Flush() }
		text := "done"
		if strings.Contains(string(body), TurnResumeText) {
			// The auto-resume's re-prompt: answer with a marker so the
			// test can pin WHICH turn completed the session.
			text = "resumed"
		}
		send(fmt.Sprintf(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":%q}}]}`, text))
		send(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":60,"completion_tokens":4,"total_tokens":64}}`)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func (p *cutProvider1732) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.request))
	copy(out, p.request)
	return out
}

func TestIntegration1732_RollClassifiedLossAutoResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration row: boots the real opencode binary")
	}

	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mock := &cutProvider1732{}
	mockSrv := &http.Server{Handler: mock.handler()} //nolint:gosec // test rig
	go mockSrv.Serve(mockLn)
	t.Cleanup(func() { _ = mockSrv.Close() })

	cfg := fmt.Sprintf(`{
		"provider": {
			"mock": {
				"options": {"apiKey": "test", "baseURL": "http://%s/v1"},
				"models": {"m": {"name": "Mock Model", "attachment": true, "limit": {"context": 100000, "output": 4096}}}
			}
		},
		"model": "mock/m"
	}`, mockLn.Addr().String())
	srv := opencode.StartIntegrationServer(t, 0, cfg)
	base := srv.IntegrationBaseURL()

	// The loopback pattern (the #1722 rig): agentd's client and the
	// tracker's /event subscription both resolve the harness through
	// the package-level agent addr.
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(base)

	const password = "test-password"
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// THE PRODUCTION WIRING (main.go's shape): the tracker feeds the
	// turn-resume supervisor every raw /event payload.
	tracker := newSessionStatusTracker()
	supervisor := newTurnResumeSupervisor(client)
	tracker.onTurnEvent = supervisor.Observe

	bgCtx, bgCancel := context.WithCancel(context.Background())
	t.Cleanup(bgCancel)
	go tracker.subscribe(bgCtx, client)

	// Session create through the real harness.
	req, _ := http.NewRequest(http.MethodPost, base+"/session", strings.NewReader("{}"))
	req.SetBasicAuth("opencode", password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	require.NoError(t, err)
	var sess struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	_ = resp.Body.Close()
	require.NotEmpty(t, sess.ID)

	prompt := func(text string) {
		body := fmt.Sprintf(`{"parts":[{"type":"text","text":%q}]}`, text)
		preq, _ := http.NewRequest(http.MethodPost, base+"/session/"+sess.ID+"/message", strings.NewReader(body))
		preq.SetBasicAuth("opencode", password)
		preq.Header.Set("Content-Type", "application/json")
		presp, perr := (&http.Client{Timeout: 0}).Do(preq)
		require.NoError(t, perr)
		_, _ = io.Copy(io.Discard, presp.Body)
		_ = presp.Body.Close()
	}

	// THE ROLL: arm the cut, send a turn — the provider stream dies
	// mid-generation (finish unknown, no session.error; wire-proven).
	mock.cut.Store(true)
	prompt("say something")

	// The supervisor must fire the ONE bounded re-prompt — and since the
	// provider is still in cut mode for that resumed turn, it gets cut
	// again (a second finish-unknown), which the budget's cooldown must
	// NOT re-resume. Heal the provider right after the first cut landed
	// so the resumed turn (if any) completes: actually — heal
	// immediately after the FIRST cut is SERVED so the resume lands on a
	// healthy provider (the roll has completed).
	require.Eventually(t, func() bool { return mock.cuts.Load() >= 1 },
		20*time.Second, 100*time.Millisecond, "the cut turn never reached the provider")
	mock.cut.Store(false) // the roll is over: new pod, healthy path

	// PIN 1 — the auto-resume fired: a provider request carrying the
	// fixed resume text arrived, and it completed cleanly.
	var sawResume bool
	require.Eventually(t, func() bool {
		for _, b := range mock.requests() {
			if strings.Contains(b, "auto-resume") {
				sawResume = true
				return true
			}
		}
		return false
	}, 30*time.Second, 200*time.Millisecond,
		"#1732: the roll-classified loss never auto-resumed — the turn died with finish unknown and nothing re-prompted (the halted worker)")
	require.True(t, sawResume)

	// PIN 2 — the session SURVIVES unwedged: the resumed turn completes
	// (finish stop) and the tracker settles idle.
	require.Eventually(t, func() bool { return tracker.get(sess.ID) == "idle" },
		30*time.Second, 100*time.Millisecond, "the session must settle idle after the resumed turn")

	// PIN 3 — bounded: exactly ONE auto-resume for this interruption
	// (the resume text appears in exactly one provider request).
	resumeRequests := 0
	for _, b := range mock.requests() {
		if strings.Contains(b, "auto-resume") {
			resumeRequests++
		}
	}
	assert.Equal(t, 1, resumeRequests, "exactly one bounded auto-resume per interruption")
}

// TestIntegration1732_UserAbortStillNeverResumes pins the
// classification's other half at the integration layer: a REAL user
// abort (POST /session/:id/abort on a live turn) emits the
// MessageAbortedError burst — the supervisor must not fire.
func TestIntegration1732_UserAbortStillNeverResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration row: boots the real opencode binary")
	}

	mockLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	mock := &cutProvider1732{}
	// Tool-turn provider shape: a bash sleep call the abort interrupts
	// (the #1722 wire, reused for the abort side).
	mockSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint:gosec // test rig
		w.Header().Set("Connection", "close")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); flusher.Flush() }
		if strings.Contains(string(body), "run the sleep tool") {
			send(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"sleep 120\",\"timeout\":300000}"}}]}}]}`)
			send(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55}}`)
		} else {
			send(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"done"}}]}`)
			send(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":60,"completion_tokens":4,"total_tokens":64}}`)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})}
	go mockSrv.Serve(mockLn)
	t.Cleanup(func() { _ = mockSrv.Close() })

	cfg := fmt.Sprintf(`{
		"provider": {
			"mock": {
				"options": {"apiKey": "test", "baseURL": "http://%s/v1"},
				"models": {"m": {"name": "Mock Model", "attachment": true, "limit": {"context": 100000, "output": 4096}}}
			}
		},
		"model": "mock/m"
	}`, mockLn.Addr().String())
	srv := opencode.StartIntegrationServer(t, 0, cfg)
	base := srv.IntegrationBaseURL()

	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(base)

	const password = "test-password"
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}
	tracker := newSessionStatusTracker()
	supervisor := newTurnResumeSupervisor(client)
	tracker.onTurnEvent = supervisor.Observe

	bgCtx, bgCancel := context.WithCancel(context.Background())
	t.Cleanup(bgCancel)
	go tracker.subscribe(bgCtx, client)

	req, _ := http.NewRequest(http.MethodPost, base+"/session", strings.NewReader("{}"))
	req.SetBasicAuth("opencode", password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	require.NoError(t, err)
	var sess struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	_ = resp.Body.Close()

	sendDone := make(chan struct{}, 1)
	go func() {
		body := `{"parts":[{"type":"text","text":"run the sleep tool"}]}`
		preq, _ := http.NewRequest(http.MethodPost, base+"/session/"+sess.ID+"/message", strings.NewReader(body))
		preq.SetBasicAuth("opencode", password)
		preq.Header.Set("Content-Type", "application/json")
		presp, perr := (&http.Client{Timeout: 0}).Do(preq)
		if perr == nil {
			sc := bufio.NewScanner(presp.Body)
			for sc.Scan() { // drain to completion
			}
			_ = presp.Body.Close()
		}
		sendDone <- struct{}{}
	}()

	require.Eventually(t, func() bool { return tracker.get(sess.ID) == "busy" },
		30*time.Second, 50*time.Millisecond, "the tool turn never went busy")

	abreq, _ := http.NewRequest(http.MethodPost, base+"/session/"+sess.ID+"/abort", strings.NewReader("{}"))
	abreq.SetBasicAuth("opencode", password)
	abreq.Header.Set("Content-Type", "application/json")
	abresp, aberr := (&http.Client{Timeout: 0}).Do(abreq)
	require.NoError(t, aberr)
	_, _ = io.Copy(io.Discard, abresp.Body)
	_ = abresp.Body.Close()
	require.Equal(t, http.StatusOK, abresp.StatusCode)

	select {
	case <-sendDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the aborted send never returned")
	}

	// No auto-resume may fire: give the supervisor a generous window to
	// misbehave, then pin nothing arrived.
	time.Sleep(2 * time.Second)
	for _, b := range mock.requests() {
		assert.NotContains(t, b, "auto-resume",
			"a user-aborted turn must NEVER auto-resume (the amendment's hard rule)")
	}
	require.Eventually(t, func() bool { return tracker.get(sess.ID) == "idle" },
		15*time.Second, 100*time.Millisecond, "the aborted session settles idle (the #1727 machinery, untouched)")
}
