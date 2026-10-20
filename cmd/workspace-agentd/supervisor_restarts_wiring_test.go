// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_restarts_wiring_test.go — #1760: the watchdog's fire,
// unknown-episode escalation, and suppression paths RECORD into the
// agentRestartLog. Recording is observational only: the decision logic
// (latch, rate limit, suppression policy) is pinned unchanged by the
// existing watchdog tests.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatchdogFire_RecordsRestartWithHungVerdict: a corroborated
// dead-listener fire lands in the log with the full attribution — the
// incident's "restarts are invisible" gap closed at the source.
func TestWatchdogFire_RecordsRestartWithHungVerdict(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	hung := &fakeVitals{v: vitalSigns{tcpRefused: true, cpuKnown: true, cpuDeltaTicks: 50}}
	log := newAgentRestartLog()

	cache := runWatchdogLoopWithRestarts(t, srv.URL, 40*time.Millisecond, fr, nil, hung, log)

	require.Eventually(t, func() bool { return fr.callCount() >= 1 },
		15*time.Second, 20*time.Millisecond, "hung fire must fire")
	settleWatchdogToFailed(t, cache)

	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.AgentRestarts, "exactly one recorded restart (latch — re-fire moments must not double-count)")
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "health_watchdog", snap.LastAgentRestart.Reason)
	assert.Equal(t, "hung", snap.LastAgentRestart.Verdict)
	assert.NotEmpty(t, snap.LastAgentRestart.LastError, "the health-check error that drove the episode rides along")
	assert.False(t, snap.LastAgentRestart.At.IsZero())
	assert.Equal(t, 0, snap.SuppressedRestarts, "a corroborated fire is not a suppression")
}

// TestWatchdogSuppression_RecordsVerdict: the incident's shape — the
// watchdog holds its kill because the agent is starved-but-alive; the
// suppression must be COUNTED with its verdict, not just logged.
func TestWatchdogSuppression_RecordsVerdict(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	starved := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 25}}
	log := newAgentRestartLog()

	cache := runWatchdogLoopWithRestarts(t, srv.URL, 40*time.Millisecond, fr, nil, starved, log)

	settleWatchdogToFailed(t, cache)

	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Zero(t, fr.callCount(), "starved must never fire")
	assert.Zero(t, snap.AgentRestarts, "a suppressed would-fire is not a restart")
	assert.Greater(t, snap.SuppressedRestarts, 0, "every suppressed would-fire must be counted")
	assert.Equal(t, "starved", snap.LastSuppressedVerdict)
}

// TestWatchdogSuppression_FlatVerdictSurfaces: the five-value taxonomy —
// flat is a distinct verdict from starved.
func TestWatchdogSuppression_FlatVerdictSurfaces(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	flat := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 0}}
	log := newAgentRestartLog()

	cache := runWatchdogLoopWithRestarts(t, srv.URL, 40*time.Millisecond, fr, nil, flat, log)

	settleWatchdogToFailed(t, cache)
	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, "flat", snap.LastSuppressedVerdict)
	assert.Greater(t, snap.SuppressedRestarts, 0)
}

// TestWatchdogFire_WithoutCorroboration_RecordsEmptyVerdict: vitals nil
// (pre-#892 semantics, tests/partial wiring) — the fire still records,
// with an empty verdict (no evidence claimed).
func TestWatchdogFire_WithoutCorroboration_RecordsEmptyVerdict(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	log := newAgentRestartLog()

	cache := runWatchdogLoopWithRestarts(t, srv.URL, 40*time.Millisecond, fr, nil, nil, log)
	require.NotNil(t, cache)

	require.Eventually(t, func() bool { return fr.callCount() >= 1 },
		15*time.Second, 20*time.Millisecond, "uncorroborated fire must still fire (pre-#892 semantics)")
	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.AgentRestarts)
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "health_watchdog", snap.LastAgentRestart.Reason)
	assert.Empty(t, snap.LastAgentRestart.Verdict, "no corroboration → no verdict claimed")
}

// TestWatchdogUnknownEpisode_RecordsEscalation: the #1632 bounded soft
// restart records with reason health_watchdog_unknown_episode and
// verdict unknown.
func TestWatchdogUnknownEpisode_RecordsEscalation(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	setUnknownEpisodeBound(t, 500*time.Millisecond)
	srv := newHungServer(t, 200*time.Millisecond)
	fr := &fakeRestarter{}
	log := newAgentRestartLog()

	_ = runWatchdogLoopWithRestarts(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, unknownVitals(), log)

	require.Eventually(t, func() bool { return fr.callCount() >= 1 },
		15*time.Second, 50*time.Millisecond, "UNKNOWN episode must escalate")

	snap := log.snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.AgentRestarts, "the escalation is a recorded restart")
	require.NotNil(t, snap.LastAgentRestart)
	assert.Equal(t, "health_watchdog_unknown_episode", snap.LastAgentRestart.Reason)
	assert.Equal(t, "unknown", snap.LastAgentRestart.Verdict)
	assert.Greater(t, snap.SuppressedRestarts, 0, "the UNKNOWN suppressions that aged into the escalation are counted too")
	assert.Equal(t, "unknown", snap.LastSuppressedVerdict)
}

// TestWatchdogHealthy_RecordsNothing: a healthy agent leaves the log
// empty (snapshot nil — the mixed-fleet omission contract).
func TestWatchdogHealthy_RecordsNothing(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":true,"version":"v1.0"}`))
	}))
	t.Cleanup(srv.Close)
	fr := &fakeRestarter{}
	log := newAgentRestartLog()

	cache := runWatchdogLoopWithRestarts(t, srv.URL, 2*time.Second, fr, nil, nil, log)

	// Wait on the OBSERVABLE (the cache's first healthy refresh), then
	// settle past the next tick so a would-fire moment would have
	// recorded — not a tautology.
	require.Eventually(t, func() bool { return cache.Snapshot().Healthy },
		5*time.Second, 50*time.Millisecond, "loop must run its first healthy refresh")
	time.Sleep(200 * time.Millisecond)
	assert.Nil(t, log.snapshot(), "a healthy run records nothing")
	assert.Zero(t, fr.callCount())
}
