// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// registry_floor_test.go — #1672 (TDD: authored before the wiring).
//
// The initial-load busy composition was wrong because statusz's busy
// content derives ENTIRELY from SSE-event-fed sources (the tracker's
// session.status marks + the #1574 ABI overlay), while the platform's
// own session-status consumers (session_metadata, the drain gate, the
// orphaned-tool repair) read opencode's LIVE /session/status registry.
// A turn that starts inside agentd's SSE recycle/backoff gap is never
// marked busy by either event-fed leg — for the remainder of the turn —
// and the #1574 overlay then actively un-busies quiet mid-turn sessions
// the tracker did mark. The v0.36.0 incident signature: three sessions
// mid-turn, statusz (and therefore the sessions listing the frontend
// seeds busy from) reported 1-of-3 busy; the live registry reported 3.
//
// The fix shape: overlay the harness live registry into cachedState's
// rendered statuses as a BUSY FLOOR — registry-busy renders busy unless
// the #1574 permission-wait carve-out applies (authority confirms a
// pending user ask with zero active parts and zero queue depth). The
// floor is add-only: a registry "idle" never un-busies (the idle leg
// belongs to the tracker/authority), and a registry fetch failure
// leaves the composition exactly as it is today.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// registryFloorFake is a fake opencode serving the session list and a
// MUTABLE live /session/status registry (tests flip it between renders
// to model the harness's own view changing under statusz).
type registryFloorFake struct {
	registry     atomic.Value // map[string]string
	failRegistry atomic.Bool
}

func (f *registryFloorFake) setRegistry(m map[string]string) {
	f.registry.Store(m)
}

func registryFakeHandler(t *testing.T, f *registryFloorFake, ids ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			_ = json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			_ = json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			resp := make([]struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}, 0, len(ids))
			for _, id := range ids {
				resp = append(resp, struct {
					ID    string `json:"id"`
					Title string `json:"title"`
				}{ID: id, Title: id})
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/session/status":
			if f.failRegistry.Load() {
				http.Error(w, "registry unavailable", http.StatusInternalServerError)
				return
			}
			reg, _ := f.registry.Load().(map[string]string)
			if reg == nil {
				reg = map[string]string{}
			}
			out := make(map[string]struct {
				Type string `json:"type"`
			}, len(reg))
			for id, typ := range reg {
				out[id] = struct {
					Type string `json:"type"`
				}{Type: typ}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}
}

// newRegistryFloorTestClient serves the given sessions plus a mutable
// /session/status registry seeded from the given map.
func newRegistryFloorTestClient(t *testing.T, registry map[string]string, ids ...string) (*OpenCodeClient, *registryFloorFake) {
	t.Helper()
	fake := &registryFloorFake{}
	fake.setRegistry(registry)
	server := httptest.NewServer(registryFakeHandler(t, fake, ids...))
	t.Cleanup(server.Close)
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(server.URL)
	return &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}, fake
}

// statusByIDs folds a cachedState result into ID → status.
func statusByIDs(sessions []agentd.SessionInfo) map[string]string {
	out := make(map[string]string, len(sessions))
	for _, s := range sessions {
		out[s.ID] = s.Status
	}
	return out
}

// TestCachedState_RegistryFloorRecoversMissedBusy is the #1672 incident
// repro at the layer that produced it: three sessions mid-turn in ONE
// workspace, the tracker EMPTY (both busy transitions started inside the
// SSE recycle/backoff gap and were never seen), the authority busy on
// exactly the one constantly-streaming session (orchestrator) and
// not-busy/blind on the two quiet mid-turn workers — while the harness
// live registry correctly reports all three busy. The rendered statuses
// must show 3-of-3 busy, not 1-of-3.
func TestCachedState_RegistryFloorRecoversMissedBusy(t *testing.T) {
	client, _ := newRegistryFloorTestClient(t,
		map[string]string{"orch": "busy", "w23": "busy", "w24": "busy"},
		"orch", "w23", "w24")

	// The missed-transition state: the SSE tracker never saw ANY
	// session.status event for these sessions (get() defaults idle).
	tracker := newSessionStatusTracker()

	// The #1574 authority: busy only on the streaming orchestrator;
	// w23 known-and-not-busy with NO pending ask (the overlay that
	// actively un-busies quiet mid-turn workers), w24 never seen by the
	// projection.
	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{"orch": {Busy: true}, "w23": {Busy: false}}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	got := statusByIDs(sessions)
	assert.Equal(t, "busy", got["orch"], "streaming orchestrator stays busy (authority leg)")
	assert.Equal(t, "busy", got["w23"],
		"#1672: a registry-busy session the authority derives not-busy (quiet mid-turn worker) must render busy — the harness floor")
	assert.Equal(t, "busy", got["w24"],
		"#1672: a registry-busy session the projection is blind to must render busy — the harness floor")

	// The floor must hold on the TTL cache-HIT path too (the registry
	// is cached with the provider snapshot; the composition re-runs per
	// render with fresh tracker/authority input).
	freshTracker := newSessionStatusTracker()
	_, _, hit := cachedState(context.Background(), client, cache, freshTracker, busyTruth)
	gotHit := statusByIDs(hit)
	assert.Equal(t, "busy", gotHit["w23"], "registry floor holds on the cache-hit render path")
	assert.Equal(t, "busy", gotHit["w24"], "registry floor holds on the cache-hit render path")
}

// TestStatuszHandler_RegistryFloorThroughTheRealHandler: the same repro
// through the production /v1/statusz wiring — the exact surface the API
// listing merge (GetAuthoritativeActiveSessions) reads and the frontend
// initial busy seed ultimately derives from.
func TestStatuszHandler_RegistryFloorThroughTheRealHandler(t *testing.T) {
	client, _ := newRegistryFloorTestClient(t,
		map[string]string{"orch": "busy", "w23": "busy", "w24": "busy"},
		"orch", "w23", "w24")
	tracker := newSessionStatusTracker()
	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{"orch": {Busy: true}, "w23": {Busy: false}}
	}

	handler := buildStatuszHandler(client, &providerCache{}, tracker, busyTruth, newMemoryPressureMonitor(),
		time.Now(), "", defaultSysMetrics(), nil, nil, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/statusz", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Sessions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"sessions"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	got := make(map[string]string, len(body.Sessions))
	for _, s := range body.Sessions {
		got[s.ID] = s.Status
	}
	assert.Equal(t, "busy", got["orch"])
	assert.Equal(t, "busy", got["w23"], "#1672: registry-busy must reach the statusz surface the listing reads")
	assert.Equal(t, "busy", got["w24"], "#1672: registry-busy must reach the statusz surface the listing reads")
}

// TestCachedState_RegistryFloorNeverUnbusies: the floor is ADD-ONLY. A
// registry "idle" must not clear a tracker busy mark — the idle leg
// belongs to the tracker/authority (#1646-adjacent stale-busy classes
// are corrected by events and the authority, never by this overlay).
func TestCachedState_RegistryFloorNeverUnbusies(t *testing.T) {
	client, fake := newRegistryFloorTestClient(t,
		map[string]string{"s1": "idle"},
		"s1")
	fake.setRegistry(map[string]string{"s1": "idle"})
	tracker := newSessionStatusTracker()
	tracker.set("s1", "busy") // a live SSE busy mark

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, nil)
	assert.Equal(t, "busy", sessions[0].Status,
		"a registry idle must never un-busy a tracker mark (add-only floor)")
}

// TestCachedState_RegistryUnavailableKeepsComposition: when the live
// registry cannot be read (route absent / errored / old harness), the
// rendered composition is EXACTLY today's tracker ∥ authority answer —
// the floor degrades inert, never wrong.
func TestCachedState_RegistryUnavailableKeepsComposition(t *testing.T) {
	// A fake with NO /session/status route (all unknown paths return
	// an empty JSON object, which decodes as an empty registry).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			_ = json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			_ = json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			_ = json.NewEncoder(w).Encode([]struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}{{ID: "s1", Title: "s1"}, {ID: "s2", Title: "s2"}})
		default:
			// /session/status lands here: an empty object — an EMPTY
			// registry, the honest "registry knows nothing" answer.
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(server.Close)
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(server.URL)
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}

	tracker := newSessionStatusTracker()
	tracker.set("s1", "busy") // tracker busy stands
	tracker.set("s2", "idle") // authority busy stands
	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{"s2": {Busy: true}}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	got := statusByIDs(sessions)
	assert.Equal(t, "busy", got["s1"], "tracker busy must survive an empty/unreadable registry")
	assert.Equal(t, "busy", got["s2"], "authority busy must survive an empty/unreadable registry")
}

// TestCachedState_RegistryFloorRespectsPermissionCarveOut: the #1574
// ruling survives the harness floor. A session blocked on a user
// ask reports BUSY in /session/status (the turn is live, waiting on
// the user) — but #1574 ruled user-blocked ≠ busy. Where the authority
// confirms a pending ask with zero streaming/parts/queue, the registry
// floor must NOT flip the session busy.
func TestCachedState_RegistryFloorRespectsPermissionCarveOut(t *testing.T) {
	client, _ := newRegistryFloorTestClient(t,
		map[string]string{"s1": "busy", "s2": "busy"},
		"s1", "s2")
	tracker := newSessionStatusTracker()

	// s1: not-busy with NO pending ask (a quiet mid-turn worker — the
	// #1672 shape, floors to busy). s2: not-busy WITH a confirmed
	// pending user ask (the #1574 carve-out, stays idle).
	busyTruth := func() map[string]busyVerdict {
		return map[string]busyVerdict{
			"s1": {Busy: false},
			"s2": {Busy: false, PendingUserAsk: true},
		}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	got := statusByIDs(sessions)
	assert.Equal(t, "busy", got["s1"], "registry-busy with no pending ask floors to busy (#1672)")
	assert.Equal(t, "idle", got["s2"],
		"#1574 carve-out: a confirmed pending user ask (zero streaming/parts/queue) stays idle even when the harness registry says busy")
}

// TestCachedState_RegistryFloorStalenessGraceBounded: when registry
// refreshes start failing, a previously-fetched registry keeps flooring
// renders only inside the staleness grace — beyond it the floor goes
// inert so a session that finished its turn cannot stay phantom-busy
// off unrefreshably-stale data.
func TestCachedState_RegistryFloorStalenessGraceBounded(t *testing.T) {
	client, fake := newRegistryFloorTestClient(t,
		map[string]string{"s1": "busy"},
		"s1")
	tracker := newSessionStatusTracker() // tracker idle — busy comes only from the floor

	// Prime the cache with a successful fetch.
	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, nil)
	require.Equal(t, "busy", statusByIDs(sessions)["s1"], "primed: registry floors to busy")

	// Break the registry and force the miss path (TTL expired).
	fake.failRegistry.Store(true)
	cache.lastFetchedAt = time.Time{}

	// Within grace: the stale registry still floors.
	cache.registryFetchedAt = time.Now().Add(-10 * time.Second)
	_, _, sessions = cachedState(context.Background(), client, cache, tracker, nil)
	assert.Equal(t, "busy", statusByIDs(sessions)["s1"],
		"a recently-fetched registry survives a failed refresh (transient failure)")

	// Beyond grace: the floor goes inert. (lastFetchedAt reset again —
	// only the miss path re-runs the grace check; a hit-path registry
	// was grace-checked at its last miss, at most one TTL ago.)
	cache.lastFetchedAt = time.Time{}
	cache.registryFetchedAt = time.Now().Add(-registryStalenessGrace - time.Second)
	_, _, sessions = cachedState(context.Background(), client, cache, tracker, nil)
	assert.Equal(t, "idle", statusByIDs(sessions)["s1"],
		"an unrefreshably-stale registry must stop flooring (no phantom busy)")
}

// TestOpenCodeClient_GetSessionStatuses pins the client seam itself:
// map decode of the harness registry shape, and a transport-honest
// error on non-200 (never an empty-but-successful registry).
func TestOpenCodeClient_GetSessionStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/status" {
			_ = json.NewEncoder(w).Encode(map[string]string{})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]struct {
			Type string `json:"type"`
		}{"s1": {Type: "busy"}, "s2": {Type: "idle"}})
	}))
	t.Cleanup(server.Close)
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(server.URL)
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}

	got, err := client.GetSessionStatuses(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"s1": "busy", "s2": "idle"}, got)

	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failServer.Close)
	setAgentAddr(failServer.URL)
	_, err = client.GetSessionStatuses(context.Background())
	require.Error(t, err, "a non-200 registry read must fail loudly, never decode as empty")
}
