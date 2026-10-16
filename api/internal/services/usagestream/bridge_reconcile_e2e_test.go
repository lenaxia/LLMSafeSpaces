// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// bridge_reconcile_e2e_test.go — #1666's acceptance pins, deterministic
// by construction: the harness kills the in-process SSE stream at a
// NAMED frame (a server-side write abort — no SIGSTOP, no timing), so
// the busy frame dies on the wire with no close frame and no replay.
// The reconnect snapshot fold rebuilds the consumer's derived baseline
// in silence; these rows pin that the BRIDGE converges onto the fold's
// busy truth across the death — the backend twin of #1646's reconnect
// reseed (same disease, other side of the pipe).

// killSwitch aborts the server's Events response at a chosen frame:
// once armed, the NEXT write on an Events connection panics with
// http.ErrAbortHandler — net/http drops the connection silently (the
// production shape of a silent stream death: no close frame, no
// replay) while the server stays up for the consumer's redial.
type killSwitch struct {
	mu    sync.Mutex
	armed bool
}

func (k *killSwitch) arm() {
	k.mu.Lock()
	k.armed = true
	k.mu.Unlock()
}

// take consumes the arm one-shot: true exactly once, at the chosen
// frame — the abort is deterministic, never a race with the reader.
func (k *killSwitch) take() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.armed {
		k.armed = false
		return true
	}
	return false
}

// killWriter intercepts one Events response's writes.
type killWriter struct {
	http.ResponseWriter
	kill *killSwitch
}

func (w *killWriter) Write(p []byte) (int, error) {
	if w.kill.take() {
		// The chosen frame dies BEFORE any byte reaches the wire — the
		// client cannot have applied it; the truth lives on in the
		// authority's fold alone.
		panic(http.ErrAbortHandler)
	}
	return w.ResponseWriter.Write(p)
}

func (w *killWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// killingHandler wraps the authority's surface so only the Events
// procedure is killable (GetSnapshot consults ride the same endpoint
// and must stay healthy).
func killingHandler(next http.Handler, k *killSwitch) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Events") {
			w = &killWriter{ResponseWriter: w, kill: k}
		}
		next.ServeHTTP(w, r)
	})
}

// newKillingAuthority stands up the REAL sessionstate authority behind
// a kill-switch-wrapped HTTP surface.
func newKillingAuthority(t *testing.T) (*sessionstate.Authority, *httptest.Server, *killSwitch) {
	t.Helper()
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       &e2eStore{},
		Passwords:   []string{"pw"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	_, h := auth.Handler()
	kill := &killSwitch{}
	ts := httptest.NewServer(killingHandler(h, kill))
	t.Cleanup(ts.Close)
	return auth, ts, kill
}

// newReconcileConsumer wires the REAL consumer with a fast redial
// (Retry) so the reconnect lands in milliseconds and a parked settle
// window (IdleDrop) so nothing but the test drives the gate down.
func newReconcileConsumer(t *testing.T, baseURL string, br Bridge) *Consumer {
	t.Helper()
	c := New(Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			return baseURL, "pw", nil
		},
		NewClient: func(baseURL, password string) Client {
			return abiclient.New(&http.Client{Transport: basicTransport{password: password}}, baseURL)
		},
		Bridge:   br,
		IdleDrop: time.Hour,
		Retry:    50 * time.Millisecond,
	})
	t.Cleanup(c.CloseAll)
	return c
}

// waitSubscribers polls the authority's subscriber count (#1532
// discipline: the wait is outcome-deterministic, the budget only bites
// on real breakage).
func waitSubscribers(t *testing.T, auth *sessionstate.Authority, want int, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers == want },
		30*time.Second, 10*time.Millisecond, msg)
}

// TestBusyTruthE2E_StreamDeathLostBusyFrameReconcilesBridge: THE #1666
// row. The busy event's frame is the named kill point — the authority's
// fold goes busy, the frame dies on the wire (no close frame), and the
// stream has no replay: the consumer's event legs never run for it.
// Pre-fix, the reconnect snapshot rebuilds the consumer's derived
// baseline in silence and the bridge NEVER learns the session went
// busy — the harness-evidenced semantic loss (no budget fixes it: the
// frame is lost, not late). The reconnect reconcile must converge the
// bridge onto the fold's busy truth.
func TestBusyTruthE2E_StreamDeathLostBusyFrameReconcilesBridge(t *testing.T) {
	auth, ts, kill := newKillingAuthority(t)
	br := &syncBridge{}
	c := newReconcileConsumer(t, ts.URL, br)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// Arm the kill AT the busy frame, then ingest it: the fold's truth
	// is busy, the delivery dies mid-flight.
	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitSubscribers(t, auth, 0, "the killed stream never dropped its subscription")
	waitSubscribers(t, auth, 1, "the consumer never redialed after the silent death")

	// The reconnect's snapshot fold says s1 busy; the bridge (which
	// never heard the lost frame) must be converged onto that truth.
	waitForStatuses(t, br, []string{"ws1:s1:busy"})
}

// TestBusyTruthE2E_StreamDeathLostIdleFrameReconcilesBridge: the
// inverse disagreement — the bridge heard busy, then the stream dies AT
// the turn's final idle frame. The fold goes idle; without the
// reconcile the bridge's busy sticks forever (a quiet post-reconnect
// stream fires no flip candidates to consult through).
func TestBusyTruthE2E_StreamDeathLostIdleFrameReconcilesBridge(t *testing.T) {
	auth, ts, kill := newKillingAuthority(t)
	br := &syncBridge{}
	c := newReconcileConsumer(t, ts.URL, br)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitForStatuses(t, br, []string{"ws1:s1:busy"})

	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	waitSubscribers(t, auth, 0, "the killed stream never dropped its subscription")
	waitSubscribers(t, auth, 1, "the consumer never redialed after the silent death")

	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle"})
}

// TestBusyTruthE2E_StreamDeathAgreeingFoldStaysQuiet: the reconcile is
// a diff, not a spray — a second silent death whose reconnect fold
// AGREES with the last bridged emission adds nothing to the user
// stream (the gate's deliberate quiet periods stay quiet; emissions
// fire only on DISAGREEMENT).
func TestBusyTruthE2E_StreamDeathAgreeingFoldStaysQuiet(t *testing.T) {
	auth, ts, kill := newKillingAuthority(t)
	br := &syncBridge{}
	c := newReconcileConsumer(t, ts.URL, br)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// Phase 1: the lost-busy-frame row converges the bridge.
	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitSubscribers(t, auth, 0, "the killed stream never dropped its subscription")
	waitSubscribers(t, auth, 1, "the consumer never redialed after the silent death")
	waitForStatuses(t, br, []string{"ws1:s1:busy"})

	// Phase 2: a second silent death, killed at a title-only frame (no
	// busy semantics move) — the reconnect fold still says busy, which
	// the bridge already holds. No re-emission.
	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "still busy"}})
	waitSubscribers(t, auth, 0, "the second killed stream never dropped its subscription")
	waitSubscribers(t, auth, 1, "the consumer never redialed after the second silent death")
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, []string{"ws1:s1:busy"}, br.snapshot(),
		"an agreeing fold must not re-emit — the reconcile fires only on disagreement")
}
