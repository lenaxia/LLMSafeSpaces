// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// US-44.1: Terminal SSE events for agent death.
//
// When opencode dies mid-stream, the SSE stream cuts to EOF. The proxy must
// emit a synthetic `event: error` with `type: "agent_died"` so the client can
// distinguish "agent gone" from "stream complete".
//
// Heuristic (per design, US-44.1 acceptance): if any bytes were received
// before EOF, emit the error event. False positives (graceful close after
// data) are explicitly acceptable per the design; false negatives (silent
// failure) are not.

// eofAfterDataTransport writes one SSE chunk then closes the pipe with io.EOF
// to simulate an opencode process that died after emitting at least one event.
type eofAfterDataTransport struct{}

func (t *eofAfterDataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		_, _ = pw.Write([]byte("data: {\"type\":\"session.started\"}\n\n"))
		// Simulate the opencode process dying: pipe closes with io.EOF.
		pw.Close()
	}()
	return resp, nil
}

// eofZeroBytesTransport closes the pipe immediately with io.EOF without
// writing any bytes — simulates a workspace pod that accepted the connection
// but the agent process was already gone.
type eofZeroBytesTransport struct{}

func (t *eofZeroBytesTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		// No data written — pure EOF.
		pw.Close()
	}()
	return resp, nil
}

// partialChunkThenEOFTransport writes a partial SSE chunk (no trailing
// newline) then EOF — simulates the agent dying mid-event. This is the
// canonical OOMKill signature from Incident A.
type partialChunkThenEOFTransport struct{}

func (t *partialChunkThenEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		// Partial chunk — agent died mid-event.
		_, _ = pw.Write([]byte("data: {\"type\":\"message.chunk\",\"content\":\"hel"))
		pw.Close()
	}()
	return resp, nil
}

// multiChunkThenEOFTransport writes three separate SSE events across three
// independent Write calls (each followed by a yield) before closing the
// pipe. Verifies that bytesReceived accumulates across multiple Read
// calls in doProxy's loop — a regression that resets the counter or only
// counts the final read would silently disable agent_died on multi-chunk
// streams (the common production case).
type multiChunkThenEOFTransport struct{}

func (t *multiChunkThenEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		_, _ = pw.Write([]byte("data: {\"type\":\"first\"}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"second\"}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"third\"}\n\n"))
		pw.Close()
	}()
	return resp, nil
}

// charsetSSETransport emits the SSE Content-Type with the canonical
// `; charset=utf-8` suffix that opencode and most SSE producers use.
// Verifies strings.HasPrefix detection (vs equality) so charset-decorated
// responses still trigger agent_died.
type charsetSSETransport struct{}

func (t *charsetSSETransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		Body:       pr,
		Request:    req,
	}
	go func() {
		_, _ = pw.Write([]byte("data: {\"type\":\"started\"}\n\n"))
		pw.Close()
	}()
	return resp, nil
}

// singleReadEOFTransport exercises the io.Reader contract case where a
// single Read returns n>0 AND err==io.EOF simultaneously (legal per
// io.Reader docs). Verifies the implementer's ordering — increment
// bytesReceived, then evaluate readErr — handles this case correctly.
type singleReadEOFTransport struct{}

func (t *singleReadEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := []byte("data: {\"type\":\"only-chunk\"}\n\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(singleEOFReader(body)),
		Request:    req,
	}
	return resp, nil
}

// singleEOFReader returns the full payload and io.EOF from a single Read
// call — exercises the (n>0, EOF) case in one call.
type singleEOFReader []byte

func (r singleEOFReader) Read(p []byte) (int, error) {
	n := copy(p, r)
	return n, io.EOF
}

// newTerminalEventTestEnv wires a ProxyHandler against a custom transport.
func newTerminalEventTestEnv(t *testing.T, transport http.RoundTripper) *testEnv {
	t.Helper()
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/message", handler.SendMessage)

	return &testEnv{
		handler: handler,
		k8sMock: k8sMock,
		llmMock: llmMock,
		wsMock:  wsMock,
		router:  router,
		log:     &testLogger{},
	}
}

func postMessage(t *testing.T, env *testEnv) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST",
		"/api/v1/workspaces/ws-1/sessions/s1/message",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)
	return w
}

// TestProxy_US44_1_AgentDiedAfterData_EmitsAgentDiedEvent verifies that when
// the upstream agent dies after streaming at least one byte (EOF after data),
// the proxy emits a terminal SSE error event tagged with
// `type:"agent_died"`, `reason:"unknown"`.
//
// This is the primary fix for Incident A (OOMKill) and Incident B (unsafe
// restart): without it the SSE stream closes silently and the client cannot
// tell that the agent is gone.
func TestProxy_US44_1_AgentDiedAfterData_EmitsAgentDiedEvent(t *testing.T) {
	env := newTerminalEventTestEnv(t, &eofAfterDataTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code,
		"status is committed 200 by the time the stream starts")
	assert.Contains(t, body, "event: error",
		"agent death (EOF after data) must emit terminal SSE error event")
	assert.Contains(t, body, `"type":"agent_died"`,
		"error event must be typed agent_died so clients can route it")
	assert.Contains(t, body, `"reason":"unknown"`,
		"reason must be unknown — proxy cannot distinguish OOM/crash/SIGTERM")
}

// TestProxy_US44_1_PartialChunkThenEOF_EmitsAgentDiedEvent covers the OOM
// signature from Incident A: a partial SSE chunk followed by EOF. The
// heuristic must trigger on partial-byte streams too, not just complete
// events.
func TestProxy_US44_1_PartialChunkThenEOF_EmitsAgentDiedEvent(t *testing.T) {
	env := newTerminalEventTestEnv(t, &partialChunkThenEOFTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, "event: error",
		"partial-chunk-then-EOF (canonical OOMKill signature) must emit error event")
	assert.Contains(t, body, `"type":"agent_died"`,
		"event must be tagged agent_died")
}

// TestProxy_US44_1_EOFZeroBytes_NoErrorEvent verifies the inverse: an EOF
// with zero bytes received (agent gone before any output) must NOT emit a
// spurious error event — there is no stream to terminate.
func TestProxy_US44_1_EOFZeroBytes_NoErrorEvent(t *testing.T) {
	env := newTerminalEventTestEnv(t, &eofZeroBytesTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.NotContains(t, body, "event: error",
		"EOF with zero bytes received must not emit a spurious error event")
	assert.NotContains(t, body, "agent_died",
		"EOF with zero bytes received must not reference agent_died")
}

// TestProxy_US44_1_MidStreamNonEOFError_KeepsUpstreamConnectionLost verifies
// that the pre-existing Epic 25 B2 behavior (non-EOF errors emit
// "upstream connection lost") is preserved and not retroactively re-typed
// as agent_died. Non-EOF errors are network failures (TCP RST, timeout),
// not process death.
func TestProxy_US44_1_MidStreamNonEOFError_KeepsUpstreamConnectionLost(t *testing.T) {
	env := newTerminalEventTestEnv(t, &midStreamResetTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, "event: error",
		"non-EOF mid-stream errors must still emit an SSE error event")
	assert.Contains(t, body, "upstream connection lost",
		"non-EOF errors must keep the existing 'upstream connection lost' message")
	assert.NotContains(t, body, "agent_died",
		"non-EOF errors must not be re-typed as agent_died — they are network failures, not process death")
}

// TestProxy_US44_1_ErrorEventFormat_IsValidSSE verifies the agent_died
// event is valid SSE (event line + data line with valid JSON + blank-line
// terminator). The data payload must include "type":"agent_died" and
// "reason" fields — the contract frontend parsers depend on. A "message"
// field was added for user-facing surfacing (worklog 0747) but is not
// part of the parser contract.
func TestProxy_US44_1_ErrorEventFormat_IsValidSSE(t *testing.T) {
	env := newTerminalEventTestEnv(t, &eofAfterDataTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	// Must contain the event line.
	assert.Contains(t, body, "event: error\n",
		"agent_died event must start with 'event: error'")
	// Must contain the required JSON fields.
	assert.Contains(t, body, `"type":"agent_died"`,
		"agent_died event data must include type:agent_died")
	assert.Contains(t, body, `"reason":"unknown"`,
		"agent_died event data must include reason")
	// Must be terminated by a blank line (SSE spec).
	assert.Contains(t, body, "\n\n",
		"agent_died event must be terminated by blank line")
}

// TestProxy_US44_1_AgentDiedResponseIncludesOriginalData verifies that the
// real streamed data BEFORE the EOF is still delivered to the client; the
// terminal error event is appended, it does not replace what was already
// flushed. This matters for incident forensics — the user can see what the
// agent said before dying.
func TestProxy_US44_1_AgentDiedResponseIncludesOriginalData(t *testing.T) {
	env := newTerminalEventTestEnv(t, &eofAfterDataTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, "session.started",
		"original streamed data must remain in the response body — terminal event is appended, not a replacement")
}

// TestProxy_US44_1_NonSSEJSONResponse_NoAgentDiedEvent verifies the
// scope-limiting guard: when the upstream response is NOT an SSE stream
// (e.g. a JSON list-sessions response from opencode), EOF after data must
// NOT trigger agent_died. Normal HTTP responses legitimately complete via
// EOF after data — emitting an SSE event into the JSON body would corrupt
// downstream parsers. This is the adversarial finding that refined the
// US-44.1 heuristic from "any data before EOF" to
// "any data before EOF on an SSE stream".
func TestProxy_US44_1_NonSSEJSONResponse_NoAgentDiedEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"method":"GET","path":"/session"}`))
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/workspaces/:id/sessions", handler.ListSessions)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/sessions", nil)
	router.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, `{"method":"GET","path":"/session"}`, body,
		"non-SSE JSON response must be passed through byte-for-byte without appended agent_died event")
	assert.NotContains(t, body, "agent_died",
		"non-SSE responses must not be tagged as agent_died — JSON/REST legitimately completes via EOF")
}

// TestProxy_US44_1_SSECleanClose_AcceptableFalsePositive documents the
// design-accepted false-positive: when an SSE stream is closed cleanly
// after data (e.g. opencode intentionally ending a stream), the proxy
// still emits agent_died. Per US-44.1 acceptance: "false positives
// (graceful close) are acceptable; false negatives (silent failures) are
// not." In production, opencode's SSE streams are long-lived (the broker
// stream has no clean close while the agent lives); an EOF after data on
// an SSE response IS the signature of agent death. This test pins the
// trade-off explicitly so a future change cannot silently reverse it.
func TestProxy_US44_1_SSECleanClose_AcceptableFalsePositive(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"session.done\"}\n\n"))
		// Backend closes after writing — clean EOF after data on SSE stream.
	}))
	defer backend.Close()

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset)

	crd := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	wsMock.On("Get", mock.Anything, "ws-1", metav1.GetOptions{}).Return(crd, nil)
	secret := makePasswordSecret("ws-1", "test-password")
	_, err := fakeClientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/message", handler.SendMessage)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/sessions/s1/message", strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	body := w.Body.String()
	assert.Contains(t, body, "session.done",
		"original data must be preserved — terminal event is appended, not a replacement")
	assert.Contains(t, body, `"type":"agent_died"`,
		"SSE stream closed after data — false-positive agent_died emitted per US-44.1 design trade-off")
}

// TestProxy_US44_1_MultiChunkAccumulation verifies that bytesReceived
// accumulates correctly across multiple Read calls. A regression that
// resets the counter inside the loop, or only counts the final read,
// would silently disable agent_died emission on multi-chunk streams.
func TestProxy_US44_1_MultiChunkAccumulation(t *testing.T) {
	env := newTerminalEventTestEnv(t, &multiChunkThenEOFTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, "first", "first chunk must be delivered")
	assert.Contains(t, body, "second", "second chunk must be delivered")
	assert.Contains(t, body, "third", "third chunk must be delivered")
	assert.Contains(t, body, `"type":"agent_died"`,
		"agent_died must fire after multi-chunk stream ends — bytesReceived accumulates across Read calls")
}

// TestProxy_US44_1_CharsetSuffix_MatchesSSE verifies the SSE detection
// handles the canonical charset-decorated Content-Type
// `text/event-stream; charset=utf-8` per RFC 9394. opencode and most SSE
// producers emit the charset suffix; strings.HasPrefix over `==` is the
// scoped-heuristic guard against this drift.
func TestProxy_US44_1_CharsetSuffix_MatchesSSE(t *testing.T) {
	env := newTerminalEventTestEnv(t, &charsetSSETransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, `"type":"agent_died"`,
		"SSE responses with '; charset=utf-8' suffix must still trigger agent_died — HasPrefix, not equality")
}

// TestProxy_US44_1_NPositiveAndEOFSameRead covers the io.Reader contract
// case where a single Read call returns n>0 AND err==EOF simultaneously
// (legal per io.Reader docs). The implementer's ordering — increment
// bytesReceived first, then handle readErr — must handle this case; a
// regression that flips the order would silently skip emitting agent_died.
func TestProxy_US44_1_NPositiveAndEOFSameRead(t *testing.T) {
	env := newTerminalEventTestEnv(t, &singleReadEOFTransport{})

	w := postMessage(t, env)

	body := w.Body.String()
	assert.Contains(t, body, "only-chunk",
		"the single chunk must be delivered even when Read returns (n>0, EOF) together")
	assert.Contains(t, body, `"type":"agent_died"`,
		"agent_died must fire when Read returns (n>0, EOF) in the same call — bytesReceived incremented before EOF check")
}

// TestProxy_US44_1_ErrorShapesAreDocumented pins both `event: error`
// wire shapes that the proxy emits, so any future change to either is
// caught as contract drift. This addresses the asymmetric-JSON finding
// from the skeptical validator: the asymmetry itself is intentional
// (B2 = network failure; US-44.1 = process death), but the two shapes
// must be explicitly documented and pinned.
func TestProxy_US44_1_ErrorShapesAreDocumented(t *testing.T) {
	t.Run("network_error_B2_shape", func(t *testing.T) {
		env := newTerminalEventTestEnv(t, &midStreamResetTransport{})
		w := postMessage(t, env)
		body := w.Body.String()
		// The B2 wire format must carry the "error" field. A "message"
		// field was added for user-facing surfacing (worklog 0747) —
		// the contract is that "error" is present and stable, not that
		// the JSON is a fixed shape.
		assert.Contains(t, body,
			`event: error`+"\n",
			"B2 wire format MUST start with event: error")
		assert.Contains(t, body,
			`"error":"upstream connection lost"`,
			"B2 wire format MUST carry the 'error' field — network-failure clients depend on it")
	})

	t.Run("agent_died_US44_1_shape", func(t *testing.T) {
		env := newTerminalEventTestEnv(t, &eofAfterDataTransport{})
		w := postMessage(t, env)
		body := w.Body.String()
		// The US-44.1 wire format must carry "type":"agent_died" and
		// "reason". A "message" field was added for user-facing
		// surfacing (worklog 0747) — the contract is that "type" and
		// "reason" are present and stable.
		assert.Contains(t, body,
			`event: error`+"\n",
			"US-44.1 wire format MUST start with event: error")
		assert.Contains(t, body,
			`"type":"agent_died"`,
			"US-44.1 wire format MUST carry 'type':'agent_died'")
		assert.Contains(t, body,
			`"reason":"unknown"`,
			"US-44.1 wire format MUST carry 'reason'")
	})
}
