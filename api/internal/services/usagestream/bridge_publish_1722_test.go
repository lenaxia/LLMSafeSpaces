// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// bridge_publish_1722_test.go — #1722 at the bridge layer: the owner
// repro is an aborted long tool call whose ~30ms clear burst
// (session.error + session.status idle + session.idle) is lost to the
// agentd /event subscription's replay-less recycle gap. The authority's
// reconcile pass heals the projection from store evidence — but (pre
// fix) SILENTLY, and after the lost burst there are NO further events
// to trigger a consult, so this consumer never flips and the user
// stream's session.status badge stays busy forever. THE pin: the
// reconcile-driven settle must reach Bridge.SessionStatus on its own.
//
// Shape note: the wedge is a lone PART_START (running tool) — it marks
// the projection busy AND bridges busy through the flip consult. Once
// that bridge emission is observed, the consumer has fully processed
// the turn's LAST delivered event; with the clear burst lost, nothing
// else ever arrives, so any post-reconcile idle emission can only come
// from the PUBLISHED settle (the pin).

// nopAdmitter arms the ledger (the evidence busy-clear rides the ledger
// sweep in the authority; a ledger-less authority has no sweep).
type nopAdmitter struct{}

func (nopAdmitter) Admit(_ context.Context, _, _, _, _ string) (string, error) {
	return "", nil
}

// new1722BridgeAuthority mirrors newBusyTruthE2E with the ledger armed
// (production wires the admitter) and a scriptable evidence store.
func new1722BridgeAuthority(t *testing.T) (*sessionstate.Authority, *e2eStore, *syncBridge) {
	t.Helper()
	store := &e2eStore{}
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		Admitter:    nopAdmitter{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })

	_, h := auth.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	br := &syncBridge{}
	c := newBusyTruthConsumer(t, ts.URL, br, time.Hour)
	c.Open("ws1")
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers == 1 },
		30*time.Second, 10*time.Millisecond, "consumer never subscribed to the authority stream")
	return auth, store, br
}

// wedge1722 opens the lost-burst turn: one PART_START carrying a
// RUNNING tool (the blocking poll). The projection marks busy; the
// consumer bridges busy via the flip consult — waitForStatuses([busy])
// IS the drain that proves the consumer processed the turn's last
// delivered event.
func wedge1722(t *testing.T, auth *sessionstate.Authority, br *syncBridge) {
	t.Helper()
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1", Part: runningToolPart("p1")})
	waitForStatuses(t, br, []string{"ws1:s1:busy"})
	require.True(t, auth.State().Sessions["s1"].Busy, "precondition: wedged busy with a running tool")
}

// TestBridgeE2E_LostIdleBurstReconcilesThroughBridge: THE #1722 pin —
// the clear burst is NEVER ingested (the recycle gap); the store now
// says the turn ended; the authority's reconcile runs. The settle must
// reach the bridge. RED pre-fix: the projection heals silently and —
// with no events left to consult — the bridge never hears.
func TestBridgeE2E_LostIdleBurstReconcilesThroughBridge(t *testing.T) {
	auth, store, br := new1722BridgeAuthority(t)
	wedge1722(t, auth, br)

	// The user aborts; the clear burst lands in the subscription's
	// recycle gap — never ingested. Store truth says the turn is over
	// (session list idle, live registry clear).
	store.set(map[string]sessionstate.SessionSeed{"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}})

	stats := auth.Reconcile(context.Background())
	require.Equal(t, sessionstate.ReconcileStats{BusyCleared: 1}, stats,
		"the projection-side heal (existing behavior)")

	// THE PIN: the settle crossed the bridge.
	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle"})
}

// TestBridgeE2E_ErroredEvidenceSettleClearsBridge: the ERROR-evidence
// settle (the harness reports the session errored) must also clear the
// bridged busy — an errored turn is over, not busy.
func TestBridgeE2E_ErroredEvidenceSettleClearsBridge(t *testing.T) {
	auth, store, br := new1722BridgeAuthority(t)
	wedge1722(t, auth, br)

	store.set(map[string]sessionstate.SessionSeed{"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_ERROR}})
	require.Equal(t, sessionstate.ReconcileStats{BusyCleared: 1}, auth.Reconcile(context.Background()))

	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle"})
}

// TestBridgeE2E_LiveRegistryHoldsSettleSilent: while the harness's
// LIVE registry still reports a turn running (the seed's LiveBusy leg),
// the reconcile neither clears nor publishes — a silent long tool run
// must not flip the badge mid-turn (the #1672 floor's evidence analog
// at the reconcile layer).
func TestBridgeE2E_LiveRegistryHoldsSettleSilent(t *testing.T) {
	auth, store, br := new1722BridgeAuthority(t)
	wedge1722(t, auth, br)

	// Mid-turn: the LIST is idle-biased but the LIVE registry says the
	// turn runs.
	store.set(map[string]sessionstate.SessionSeed{"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE, LiveBusy: true}})
	stats := auth.Reconcile(context.Background())
	require.Equal(t, sessionstate.ReconcileStats{}, stats, "live turn: nothing settles")

	st := auth.State()
	require.True(t, st.Sessions["s1"].Busy, "the live-registry gate holds the busy view")
	require.Never(t, func() bool {
		ss := br.snapshot()
		return len(ss) > 1 && ss[len(ss)-1] == "ws1:s1:idle"
	}, 500*time.Millisecond, 25*time.Millisecond, "no settle emission while the live turn runs")
}
