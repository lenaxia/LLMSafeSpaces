// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// resync_flight_test.go — #1884's acceptance pins: the pod→api
// usage-stream leg is the ONE silent-loss leg with no user-stream
// reconciliation. The browser leg heals on reconnect (wipe #1646) and
// broker drop (resync #1687); the consumer's #1666 reconcile heals the
// per-session busy DIFFS it can compute — but sessions absent from the
// fold, and browser latches whose loss the gate's bridged baseline
// cannot see, are only healed by an AUTHORITY flight (the busy-snapshot
// handshake #1848 serves on subscribe and on the lease recheck). These
// rows pin the missing trigger: every NON-INITIAL snapshot seed must
// notify the bridge so the handler layer can run the per-workspace
// flight. The gate's INITIAL connect fires nothing: nothing was lost
// before it, and the connect-path flight already covers the
// subscribers.
//
// Design 0069 amended WHEN a seed exists: a reconnect the replay ring
// can serve RESUMES (no snapshot, no notify — nothing was lost; the
// resumed rows live in billing_replay_test.go). A seed now lands only
// on the honest FALLBACK (cursor below the ring's low-water mark, or
// client-ahead of its current seq) or the in-stream reseed redial —
// all genuine possible-loss windows, so the notify still fires on
// every seed that remains. The fallback rows below drive a tiny-ring
// authority so the dead window's filler events evict the cursor past
// low-water deterministically.

// resyncBridge extends syncBridge with the #1884 optional seam: it
// records StreamResynced calls under the same mutex discipline (the
// consumer goroutine calls back while the test polls).
type resyncBridge struct {
	syncBridge
	mu       sync.Mutex
	resynced []string
}

// StreamResynced is the #1884 seam: fired when a snapshot seed lands
// that is NOT the gate's first — frames may have been lost since the
// previous one, and the surfaces downstream of the bridge (the user
// stream's browser latches) reconcile from an authority flight.
func (b *resyncBridge) StreamResynced(workspaceID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resynced = append(b.resynced, workspaceID)
}

func (b *resyncBridge) resyncCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.resynced)
}

func (b *resyncBridge) resyncSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.resynced...)
}

// TestResyncFlightE2E_InitialConnectFiresNothing: the gate's first
// connection is not a reconnect — no frames were lost before it, and
// the user stream's connect-path flight already seeds subscribers. A
// notify here would spray redundant flights on every gate arm.
func TestResyncFlightE2E_InitialConnectFiresNothing(t *testing.T) {
	auth, ts, _ := newKillingAuthority(t)
	br := &resyncBridge{}
	c := newReconcileConsumer(t, ts.URL, br)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	time.Sleep(300 * time.Millisecond)
	require.Zero(t, br.resyncCount(),
		"the gate's initial connect must NOT fire the resync notify — only a reconnect/reseed seed may")
}

// TestResyncFlightE2E_SilentDeathRedialFiresResync: THE #1884 row, on
// the design-0069 FALLBACK leg (the only silent-loss shape left): the
// stream dies silently mid-flight AND the loss exceeds what the replay
// ring can serve (here: a 2-frame ring, the busy frame plus fillers
// evicted past the client's cursor), so the redial must re-snapshot.
// That snapshot seed must notify the bridge (the handler layer's cue to
// run the authority flight that reconciles every subscriber's latches).
func TestResyncFlightE2E_SilentDeathRedialFiresResync(t *testing.T) {
	auth, ts, kill := newKillingAuthorityRing(t, 2)
	br := &resyncBridge{}
	c := newReconcileConsumerRetry(t, ts.URL, br, 400*time.Millisecond)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// The busy flip's frame dies on the wire; the fold goes busy. The
	// filler events (no bridge semantics) evict the 2-frame ring past
	// the client's cursor=0 inside the dead window — the redial CANNOT
	// resume and must take the snapshot fallback.
	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitSubscribers(t, auth, 0, "the killed stream never dropped its subscription")
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "filler-a"}})
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "filler-b"}})
	waitSubscribers(t, auth, 1, "the consumer never redialed after the silent death")

	// The redial's snapshot seed (which also carries the #1666 busy
	// correction) must fire the resync notify exactly once.
	require.Eventually(t, func() bool { return br.resyncCount() >= 1 },
		5*time.Second, 10*time.Millisecond,
		"the fallback redial's snapshot seed must fire the resync notify — the lost-frame heal's cue")
	require.Equal(t, []string{"ws1"}, br.resyncSnapshot(),
		"exactly one notify per reconnect seed (not a spray)")

	// Quiet afterwards: an idle live stream publishes no further seeds.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 1, br.resyncCount(), "no further notifies without another snapshot seed")
}

// TestResyncFlightE2E_AgreeingFoldStillFires: the notify is NOT a diff.
// The #1666 corrections fire only on bridged/fold disagreement; the
// resync notify must fire on EVERY fallback seed — a fold that agrees
// with the bridge baseline can still disagree with every BROWSER's
// latch (the loss the gate cannot compute), which only the authority
// flight heals.
func TestResyncFlightE2E_AgreeingFoldStillFires(t *testing.T) {
	auth, ts, kill := newKillingAuthorityRing(t, 2)
	br := &resyncBridge{}
	c := newReconcileConsumerRetry(t, ts.URL, br, 400*time.Millisecond)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// Deliver busy cleanly (no kill): the bridge baseline and the fold agree.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitForStatuses(t, &br.syncBridge, []string{"ws1:s1:busy"})
	require.Zero(t, br.resyncCount(), "precondition: a live delivery is not a seed")

	// Silent death at a semantics-free frame, then fillers that evict
	// the 2-frame ring past the client's cursor=1 — the redial falls
	// back to a snapshot. The redial fold still says busy — the #1666
	// diff stays quiet, but the notify must fire.
	kill.arm()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "still busy"}})
	waitSubscribers(t, auth, 0, "the killed stream never dropped its subscription")
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "filler-a"}})
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_UPDATED, SessionId: "s1",
		Session: &abiv1.Session{Id: "s1", Title: "filler-b"}})
	waitSubscribers(t, auth, 1, "the consumer never redialed after the silent death")

	require.Eventually(t, func() bool { return br.resyncCount() == 1 },
		5*time.Second, 10*time.Millisecond,
		"an agreeing fold must still fire the resync notify — the browser-side loss the diff cannot see")

	// And the diff stayed quiet: no extra session.status emissions
	// beyond the live busy delivery.
	require.Equal(t, []string{"ws1:s1:busy"}, br.snapshot())
}

// TestResyncFlightE2E_InStreamReseedFiresResync: the in-stream
// projection.reseeded redial happens inside abiclient.Stream (no
// return, no run-loop reconnect) — WithResynced re-arms the seed
// branch, so the reseed's fresh snapshot must fire the notify too (the
// reseed rebuilt the fold from store truth; everything since the last
// seed is unasserted).
func TestResyncFlightE2E_InStreamReseedFiresResync(t *testing.T) {
	store := &e2eStore{}
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       store,
		Passwords:   []string{"pw"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	_, h := auth.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	br := &resyncBridge{}
	c := newReconcileConsumer(t, ts.URL, br)
	c.Open("ws1")
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers >= 1 },
		30*time.Second, 10*time.Millisecond, "consumer never subscribed")
	require.Zero(t, br.resyncCount(), "precondition: the initial connect fires nothing")

	store.set(map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	require.NoError(t, auth.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	require.Eventually(t, func() bool { return br.resyncCount() == 1 },
		5*time.Second, 10*time.Millisecond,
		"the in-stream reseed's snapshot seed must fire the resync notify")
}
