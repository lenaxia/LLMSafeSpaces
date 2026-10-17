// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// busy_settle_1722_test.go — #1722 (session shows busy after turn end;
// repro: long tool call aborted mid-turn). The abort's clear burst
// (session.error + session.status idle + session.idle, ~30ms) rides
// agentd's replay-less /event subscription; lost to a recycle gap it
// leaves the sessionStatusTracker busy FOREVER — no evidence settle
// exists on that path (only a generation change or the next turn's
// events re-settle it). The composition (cachedState) already RENDERS
// the truth (the authority overlay un-busies the row — #1574), but the
// tracker's own mark sticks, keeping the pod phantom-busy for
// restart gating (trackerHasBusyOrUnknown defers credential applies
// and suspends), the ops gauge, and the #1685 evidence clock. These
// pins make the render-time settle explicit: when the authority's
// single busy definition proves a session not-busy (no pending user
// ask, no registry floor), the tracker mark converges too.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSettleTestClient serves the session list plus a scriptable
// /session/status live-turn registry (the #1672 busy floor input).
func newSettleTestClient(t *testing.T, registry map[string]string, ids ...string) *OpenCodeClient {
	t.Helper()
	if registry == nil {
		registry = map[string]string{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			_ = json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{}}})
		case "/session/status":
			out := map[string]struct {
				Type string `json:"type"`
			}{}
			for id, v := range registry {
				out[id] = struct {
					Type string `json:"type"`
				}{Type: v}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/session":
			resp := make([]struct {
				ID string `json:"id"`
			}, 0, len(ids))
			for _, id := range ids {
				resp = append(resp, struct {
					ID string `json:"id"`
				}{ID: id})
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(server.Close)
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(server.URL)
	return &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
}

// TestCachedState_SettlesTrackerFromAuthorityIdleTruth: THE #1722
// tracker pin — the composition that already renders idle must also
// SETTLE the tracker's raw mark, so restart gating, the busy gauge,
// and the #1685 evidence clock stop carrying the fiction. RED on the
// pre-fix tree: the render flips, the tracker stays busy.
func TestCachedState_SettlesTrackerFromAuthorityIdleTruth(t *testing.T) {
	client := newSettleTestClient(t, nil, "s1", "s2")
	tracker := newSessionStatusTracker()
	tracker.set("s1", "busy") // the wedge: clear burst lost to an SSE gap
	tracker.set("s2", "busy") // a permission wait — the carve-out

	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{
			"s1": {Busy: false, PendingUserAsk: false}, // turn observably finished
			"s2": {Busy: false, PendingUserAsk: true},  // user-blocked: NOT settled
		}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	byID := map[string]string{}
	for _, s := range sessions {
		byID[s.ID] = s.Status
	}
	assert.Equal(t, "idle", byID["s1"], "the render already converges (the #1574 overlay)")
	assert.Equal(t, "idle", byID["s2"], "the carve-out renders idle too (pending ask is its own signal)")

	// THE PIN: the tracker itself converges.
	assert.Equal(t, "idle", tracker.get("s1"),
		"#1722: the authority's idle truth must settle the tracker mark — else restart gating and the busy gauge carry the fiction forever")
	assert.Equal(t, "busy", tracker.get("s2"),
		"the permission-wait carve-out must NOT settle the tracker (user-blocked is not turn-end)")

	// The settle stamps observed-idle (#1672 r2): a stale registry
	// cannot re-busy the session on later renders.
	assert.True(t, tracker.observedIdleAfter("s1", time.Now().Add(-time.Minute)),
		"the settle is honest idle evidence — it must outrank a stale registry floor")
}

// TestCachedState_NoSettleUnderRegistryFloor: the floor's whole job is
// catching turns the event paths never saw (#1672) — while the registry
// floors a session busy, the tracker mark is NOT settled even if the
// authority currently derives not-busy (conservative: the floor is live
// truth, the projection may be behind).
func TestCachedState_NoSettleUnderRegistryFloor(t *testing.T) {
	client := newSettleTestClient(t, map[string]string{"s1": "busy"}, "s1") // served as {"type":"busy"} below
	tracker := newSessionStatusTracker()
	tracker.set("s1", "busy")

	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{"s1": {Busy: false, PendingUserAsk: false}}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	require.Equal(t, "busy", sessions[0].Status, "the floor re-busies the render (the #1672 contract)")
	assert.Equal(t, "busy", tracker.get("s1"),
		"a registry-floored session must not have its tracker mark settled")
}

// TestCachedState_SettleRequiresAuthorityKnowledge: sessions the
// projection does not know keep the tracker's answer verbatim — the
// settle is an overlay convergence, never a blind wipe.
func TestCachedState_SettleRequiresAuthorityKnowledge(t *testing.T) {
	client := newSettleTestClient(t, nil, "s_unknown")
	tracker := newSessionStatusTracker()
	tracker.set("s_unknown", "busy")

	cache := &providerCache{}
	_, _, _ = cachedState(context.Background(), client, cache, tracker, func() map[string]busyVerdict {
		return map[string]busyVerdict{} // projection knows nothing
	})
	assert.Equal(t, "busy", tracker.get("s_unknown"),
		"projection-unknown sessions keep the tracker's answer (the fallback contract)")
}
