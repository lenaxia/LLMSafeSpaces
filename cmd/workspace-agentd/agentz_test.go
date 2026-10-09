// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// agentz_test.go — #1632 fix #3: the agent-health-derived liveness
// endpoint and the health-cache episode clock that feeds it.
//
// The episode clock is the whole design: the 2026-10-08 incident's wedge
// CYCLED (stall ~10min → one healthy poll → stall), so a naive
// continuous-failure bound never fires, while the #892 starved-healthy
// incident failed in short bursts with genuine recovery. The clock
// resets only after healthyEpisodeResetPolls CONSECUTIVE healthy polls.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/stretchr/testify/require"
)

// fakeClockSnapshot builds a cache snapshot for handler tests.
func fakeClockSnapshot(initialized, healthy bool, episodeStart time.Time, consecHealthy int) *healthzCache {
	c := newHealthzCache()
	c.snapshot.Store(&healthzCacheSnapshot{
		Initialized:               initialized,
		Healthy:                   healthy,
		UnhealthyEpisodeStartedAt: episodeStart,
		ConsecutiveHealthy:        consecHealthy,
		LastRefreshedAt:           time.Now(),
	})
	return c
}

// TestAgentz_Uninitialized_Returns200: no evidence (boot, or the cache
// of a freshly restarted sidecar) must never fail liveness.
func TestAgentz_Uninitialized_Returns200(t *testing.T) {
	c := fakeClockSnapshot(false, false, time.Time{}, 0)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAgentz_Healthy_Returns200.
func TestAgentz_Healthy_Returns200(t *testing.T) {
	c := fakeClockSnapshot(true, true, time.Time{}, 5)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}

// TestAgentz_YoungEpisode_Returns200: an unhealthy episode younger than
// the sustain bound stays healthy-for-liveness — this is the window in
// which #892-class bursts live; they must never trip the probe.
func TestAgentz_YoungEpisode_Returns200(t *testing.T) {
	c := fakeClockSnapshot(true, false, time.Now().Add(-2*time.Minute), 0)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body agentd.AgentzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.OK)
	require.Equal(t, "unhealthy-episode", body.State)
}

// TestAgentz_SustainedEpisode_Returns503: the incident's shape — the
// episode has outlasted the sustain bound (brief recoveries did not
// reset the clock) → the probe must fail so kubelet restarts the
// workspace container.
func TestAgentz_SustainedEpisode_Returns503(t *testing.T) {
	c := fakeClockSnapshot(true, false, time.Now().Add(-11*time.Minute), 0)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body agentd.AgentzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.False(t, body.OK)
	require.Equal(t, "unhealthy-episode-sustained", body.State)
	require.GreaterOrEqual(t, body.EpisodeSeconds, 600)
	require.Equal(t, 600, body.SustainSeconds)
}

// TestAgentz_SustainedEpisode_SurvivesHealthyFlip (r1 laundering pin):
// a sustained episode PLUS a momentary answered-healthy poll
// (Healthy=true, ConsecutiveHealthy below the reset threshold) must
// STILL 503. A Healthy-gated handler returns 200 through this blip and
// resets kubelet's failure streak — the dense-recovery-window wedge
// would never be restarted. The episode clock is the only decision
// input.
func TestAgentz_SustainedEpisode_SurvivesHealthyFlip(t *testing.T) {
	c := fakeClockSnapshot(true, true, time.Now().Add(-11*time.Minute), 1)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"a single healthy blip must not launder a sustained episode")
}

// TestAgentz_SustainedBoundary_AtExactlySustain: episodeFor == sustain
// is over the line (< sustain is the 200 branch).
func TestAgentz_SustainedBoundary_AtExactlySustain(t *testing.T) {
	c := fakeClockSnapshot(true, false, time.Now().Add(-agentUnhealthyEpisodeSustain), 0)
	h := buildAgentzHandler(serverDeps{healthCache: c})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"exactly at the sustain bound the probe fails")
}

// TestAgentz_AdminServerWiring (r1): the route + bearer composition as
// production registers it — a dropped route or a missing
// requireBearerToken wrapper means 404/401 forever = a workspace
// restart loop (~11min cadence). Readyz wiring-test precedent (F1.4.2).
func TestAgentz_AdminServerWiring(t *testing.T) {
	withTestLogger(t)
	deps := serverDeps{healthCache: fakeClockSnapshot(true, true, time.Time{}, 5)}

	mux := http.NewServeMux()
	mux.Handle("/v1/agentz", requireBearerToken("tok", buildAgentzHandler(deps)))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(auth string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/agentz", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	require.Equal(t, http.StatusUnauthorized, get(""), "agentz enforces bearer auth")
	require.Equal(t, http.StatusUnauthorized, get("wrong"), "agentz rejects bad tokens")
	require.Equal(t, http.StatusOK, get("tok"), "authorized healthy request: 200")
}

// TestAgentz_GenerationReArm: noteAgentGeneration (fired at every agent
// generation boundary — supervisor restart, crash recovery, kubelet
// container restart) clears an open episode: the replacement child gets
// a fresh sustain budget and cannot be killed mid-boot inheriting the
// dead generation's clock.
func TestAgentz_GenerationReArm(t *testing.T) {
	c := fakeClockSnapshot(true, false, time.Now().Add(-11*time.Minute), 0) // sustained: would 503
	h := buildAgentzHandler(serverDeps{healthCache: c})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "pre-condition: sustained episode fails")

	c.noteAgentGeneration()

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/agentz", nil))
	require.Equal(t, http.StatusOK, rec.Code,
		"a generation boundary re-arms the episode — the replacement boots with a fresh budget")
	require.True(t, c.Snapshot().UnhealthyEpisodeStartedAt.IsZero())
}

// TestAgentz_GenerationReArm_NoOpenEpisode_Noop: idempotent when
// healthy.
func TestAgentz_GenerationReArm_NoOpenEpisode_Noop(t *testing.T) {
	c := fakeClockSnapshot(true, true, time.Time{}, 9)
	c.noteAgentGeneration()
	require.True(t, c.Snapshot().Healthy)
	require.True(t, c.Snapshot().UnhealthyEpisodeStartedAt.IsZero())
}

// TestAgentz_GenerationReArm_SurvivesInFlightRefresh (r3 interleaving
// pin — the lost-update race): refreshOnce is a read-modify-write with
// up-to-4s of I/O between the read and the Store; a generation boundary
// landing inside that window must NOT be clobbered by the refresh's
// completion (which derived its episode state from the STALE pre-poll
// snapshot). The r3 reviewer demonstrated the unfixed race reinstating
// a 12-minute episode empirically; this pin is the durable version.
// Sequence: open a 12-minute episode → start refreshOnce against a
// slow-but-healthy mock → noteAgentGeneration MID-FLIGHT → refresh
// completes healthy → the episode must stay cleared.
func TestAgentz_GenerationReArm_SurvivesInFlightRefresh(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the poll open past the generation bump
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v"})
	}))
	defer srv.Close()
	origAddr := getAgentAddr()
	setAgentAddr(srv.URL)
	t.Cleanup(func() { setAgentAddr(origAddr) })

	c := newHealthzCache()
	c.snapshot.Store(&healthzCacheSnapshot{
		Initialized: true, Healthy: false,
		UnhealthyEpisodeStartedAt: time.Now().Add(-12 * time.Minute),
		LastError:                 "stale",
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshOnce(t.Context(), &OpenCodeClient{password: "t", client: &http.Client{Timeout: 5 * time.Second}}, c, testLogger(), nil)
	}()

	// Let the poll get well and truly in flight, then fire the
	// generation boundary mid-poll.
	time.Sleep(50 * time.Millisecond)
	c.noteAgentGeneration()
	require.True(t, c.Snapshot().UnhealthyEpisodeStartedAt.IsZero(),
		"pre-condition: the generation bump cleared the episode")
	close(release)
	<-done

	s := c.Snapshot()
	require.True(t, s.UnhealthyEpisodeStartedAt.IsZero(),
		"the in-flight refresh must not reinstate the pre-generation episode (lost-update race, r3 Correctness 1)")
	require.True(t, s.Healthy, "the poll itself completed healthy")
}

// TestEpisodeClock_PanicPathStartsEpisode (r1): a panicking refresh is
// an unhealthy poll — at the threshold crossing the episode must start
// through the recovery path too, or a panicky refresher would starve
// agentz of its clock.
func TestEpisodeClock_PanicPathStartsEpisode(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated opencode panic")
	}))
	defer mock.Close()

	origAddr := getAgentAddr()
	setAgentAddr(mock.URL)
	t.Cleanup(func() { setAgentAddr(origAddr) })

	client := &OpenCodeClient{password: "t", client: &http.Client{Timeout: 2 * time.Second}}
	c := newHealthzCache()
	c.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, ConsecutiveHealthy: 5})

	for i := 0; i < readinessFailureThreshold; i++ {
		require.NotPanics(t, func() {
			refreshOnce(t.Context(), client, c, testLogger(), nil)
		})
	}
	s := c.Snapshot()
	require.False(t, s.Healthy)
	require.False(t, s.UnhealthyEpisodeStartedAt.IsZero(),
		"the panic-recovery path must run the same episode clock as the normal path")
	require.Zero(t, s.ConsecutiveHealthy)
}

// --- Episode clock (refreshOnce) ---

// refreshWith drives one refreshOnce against a controlled opencode
// answer and returns the new snapshot. The agent addr is pointed at the
// mock for the duration (refreshOnce's client resolves the live addr
// via the package-level setter — without this the poll would hit this
// pod's REAL opencode when the tests run inside a workspace pod).
func refreshWith(t *testing.T, prev *healthzCache, healthy bool, errHappens bool) healthzCacheSnapshot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case errHappens:
			time.Sleep(300 * time.Millisecond) // exceed the shrunk timeout
		case healthy:
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": false, "version": "v"})
		}
	}))
	t.Cleanup(srv.Close)
	origAddr := getAgentAddr()
	setAgentAddr(srv.URL)
	t.Cleanup(func() { setAgentAddr(origAddr) })
	origTimeout := readinessRefreshTimeout
	readinessRefreshTimeout = 50 * time.Millisecond
	t.Cleanup(func() { readinessRefreshTimeout = origTimeout })

	client := &OpenCodeClient{password: "t", client: &http.Client{Timeout: readinessRefreshTimeout}}
	refreshOnce(t.Context(), client, prev, testLogger(), nil)
	return prev.Snapshot()
}

// TestEpisodeClock_StartsAtThresholdAndSurvivesBlips: erred polls cross
// the failure threshold → episode starts; ONE healthy poll (a blip,
// below healthyEpisodeResetPolls) does NOT reset it; more failures age
// it onward.
func TestEpisodeClock_StartsAtThresholdAndSurvivesBlips(t *testing.T) {
	c := newHealthzCache()
	// Prime: initialized + healthy + episode-clear (5 consecutive healthy).
	c.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, ConsecutiveHealthy: 5})

	// Three failures → threshold (readinessFailureThreshold=3) → episode starts.
	refreshWith(t, c, false, true)
	refreshWith(t, c, false, true)
	s := refreshWith(t, c, false, true)
	require.False(t, s.Healthy)
	require.False(t, s.UnhealthyEpisodeStartedAt.IsZero(), "episode must start at the threshold crossing")
	started := s.UnhealthyEpisodeStartedAt

	// ONE healthy poll — the incident's brief recovery. Episode MUST hold.
	s = refreshWith(t, c, true, false)
	require.True(t, s.Healthy)
	require.Equal(t, started, s.UnhealthyEpisodeStartedAt,
		"a single healthy poll must NOT reset the episode clock (incident's cycling)")

	// Second and third consecutive healthy polls reach
	// healthyEpisodeResetPolls → episode ends.
	s = refreshWith(t, c, true, false)
	s = refreshWith(t, c, true, false)
	require.True(t, s.UnhealthyEpisodeStartedAt.IsZero(),
		"%d consecutive healthy polls must end the episode", healthyEpisodeResetPolls)
}

// TestEpisodeClock_TwoHealthyPollsThenFailure_KeepsOriginalStart: a
// near-recovery that collapses before reaching the reset threshold
// keeps the ORIGINAL episode start — cycling cannot launder the clock.
func TestEpisodeClock_TwoHealthyPollsThenFailure_KeepsOriginalStart(t *testing.T) {
	c := newHealthzCache()
	c.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, ConsecutiveHealthy: 5})

	refreshWith(t, c, false, true)
	refreshWith(t, c, false, true)
	s := refreshWith(t, c, false, true)
	started := s.UnhealthyEpisodeStartedAt
	require.False(t, started.IsZero())

	// 2 healthy (below reset), then failure again. Healthy needs the
	// full failure threshold to re-flip (existing semantics); the
	// EPISODE is what must survive the laundering attempt.
	s = refreshWith(t, c, true, false)
	s = refreshWith(t, c, true, false)
	s = refreshWith(t, c, false, true)
	require.Equal(t, started, s.UnhealthyEpisodeStartedAt,
		"a collapsed near-recovery must keep the original episode start")
}

// TestEpisodeClock_AnsweredUnhealthy_ImmediateStart: opencode ANSWERS
// unhealthy (no error) — Healthy flips immediately, and so must the
// episode.
func TestEpisodeClock_AnsweredUnhealthy_ImmediateStart(t *testing.T) {
	c := newHealthzCache()
	c.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true, ConsecutiveHealthy: 5})

	s := refreshWith(t, c, false, false)
	require.False(t, s.Healthy)
	require.False(t, s.UnhealthyEpisodeStartedAt.IsZero(),
		"an answered-unhealthy poll flips Healthy immediately; the episode starts with it")
}
