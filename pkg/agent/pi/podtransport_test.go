// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// podtransport_test.go — the S4→S5 transport contract over the
// harness-port bridge (the review finding: zero tests referenced
// PodTransport). Pins: the resolver error surfaces (including the
// canonical no-running-pod verdict), basic-auth on every bridge call,
// the JSON-RPC error surface (404 → session-gone), SSE events with the
// injected sessionId, and the stream/RPC client timeout split (an SSE
// subscription must NEVER inherit the 120s RPC client timeout — that
// killed every stream mid-turn at exactly 120s).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// bridgeServer builds the transport against the given handler with the
// pod IP pointing at the handler's listener.

type staticPodResolver struct {
	ip  string
	err error
}

func (r staticPodResolver) GetWorkspacePodIP(context.Context, string, string) (string, error) {
	return r.ip, r.err
}

// bridgeServer builds the transport against the given handler with the
// pod IP pointing at the handler's listener.
func bridgeServer(t *testing.T, h http.Handler) (*PodTransport, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tr := NewPodTransport(
		func(context.Context, string) (string, error) { return "test-bridge-password", nil },
		staticPodResolver{ip: "127.0.0.1"},
		nil,
	)
	var port int
	_, err := fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port)
	require.NoError(t, err)
	tr.port = port
	return tr, srv
}

// --- resolver error surfaces ---------------------------------------------------

func TestPodTransport_ResolveErrors(t *testing.T) {
	// Empty pod IP is the canonical no-running-pod verdict — the
	// opencode adapter's contract, mirrored.
	tr, _ := bridgeServer(t, http.NotFoundHandler())
	tr.ip = staticPodResolver{ip: ""}
	_, err := tr.Session(context.Background(), "u", "ws", "s1")
	require.ErrorIs(t, err, agent.ErrNoRunningPod)

	// Resolver errors surface with context, never swallowed.
	tr.ip = staticPodResolver{err: errors.New("apiserver down")}
	_, err = tr.Session(context.Background(), "u", "ws", "s1")
	require.ErrorContains(t, err, "apiserver down")

	tr.pw = func(context.Context, string) (string, error) {
		return "", errors.New("secret gone")
	}
	_, err = tr.Session(context.Background(), "u", "ws", "s1")
	require.ErrorContains(t, err, "secret gone")
}

// --- RPC calls: auth + error surface --------------------------------------------

func TestPodTransport_SessionCall_AuthAndRoundTrip(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]any
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}`))
	})
	tr, _ := bridgeServer(t, h)

	sc, err := tr.Session(context.Background(), "u", "ws-x", "sess-1")
	require.NoError(t, err)
	out, err := sc.Call(context.Background(), map[string]any{"type": "get_state"})
	require.NoError(t, err)
	require.NotNil(t, out)

	// Basic auth rides the workspace password (agentd.AuthUsername).
	require.True(t, strings.HasPrefix(gotAuth, "Basic "),
		"every bridge call must carry basic auth")
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/v1/pi/sessions/sess-1/rpc", gotPath)
	require.Equal(t, "get_state", gotBody["type"])
}

func TestPodTransport_SessionCall_ErrorSurfaces(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"session is busy"}`))
	})
	tr, _ := bridgeServer(t, h)
	sc, err := tr.Session(context.Background(), "u", "ws-x", "sess-1")
	require.NoError(t, err)
	_, err = sc.Call(context.Background(), map[string]any{"type": "prompt"})
	require.ErrorContains(t, err, "session is busy")

	// 404 maps to the session-gone sentinel (agent.ErrSessionNotFound).
	h404 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such session"}`))
	})
	tr2, _ := bridgeServer(t, h404)
	sc2, err := tr2.Session(context.Background(), "u", "ws-x", "sess-1")
	require.NoError(t, err)
	_, err = sc2.Call(context.Background(), map[string]any{"type": "prompt"})
	require.ErrorIs(t, err, errPiSessionGone)
}

// --- events: SSE + sessionID injection -------------------------------------------

func TestPodTransport_Events_SSEInjectsSessionID(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, ": keepalive\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"message_update\",\"text\":\"hi\"}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"agent_settled\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // stream until the subscriber leaves
	})
	tr, _ := bridgeServer(t, h)
	sc, err := tr.Session(context.Background(), "u", "ws-x", "sess-9")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // idempotent with the in-loop cancel; vet-safe on every path
	ch, err := sc.Events(ctx)
	require.NoError(t, err)

	var raws []json.RawMessage
	for rec := range ch {
		require.NoError(t, rec.Err)
		raws = append(raws, rec.Raw)
		if len(raws) == 2 {
			cancel()
		}
	}
	require.Len(t, raws, 2, "both data records deliver; the keepalive comment is skipped")

	// The injected sessionId rides every record (the S4 transport
	// contract — ContextUsageFromEvent's non-empty-sessionID guarantee).
	for _, raw := range raws {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		assert.Equal(t, "sess-9", m["sessionId"])
	}
}

func TestPodTransport_ControlClientHasNoEventStream(t *testing.T) {
	tr, _ := bridgeServer(t, http.NotFoundHandler())
	sc, err := tr.Control(context.Background(), "u", "ws-x")
	require.NoError(t, err)
	_, err = sc.Events(context.Background())
	require.ErrorContains(t, err, "no event stream")
}

// --- the timeout split (review finding #1 pin) ------------------------------------

func TestPodTransport_ClientTimeoutSplit(t *testing.T) {
	// The bridgeServer transport above had its port seam pointed at
	// the test listener; assert the split on a FRESH production
	// construction.
	prod := NewPodTransport(nil, nil, nil)
	require.Equal(t, 120*time.Second, prod.client.Timeout,
		"RPC calls keep the bounded client timeout")
	require.Zero(t, prod.streamClient.Timeout,
		"the SSE client MUST carry no client-wide Timeout — a live event stream is unbounded; the 120s client killed every stream mid-turn")
	require.Equal(t, agentd.AgentPort, prod.port,
		"production construction targets the harness port")
}
