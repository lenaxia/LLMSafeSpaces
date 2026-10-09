// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// watchdog_unknown_episode_test.go — #1632 fix #2: a CONTINUOUS
// UNKNOWN episode must be BOUNDED. The 2026-10-08 incident: sidecar
// topology made CPU evidence structurally unavailable, every would-fire
// landed verdictUnknown, and "killing without evidence is banned"
// (#892) degenerated into "suppress forever" — 36+ suppressions, wedge
// never self-healed. These tests pin the new episode bound: UNKNOWN
// older than watchdogUnknownEpisodeBound escalates ONCE per episode to
// a rate-limited soft restart (marker + metric), while honest evidence
// verdicts (STARVED/FLAT/RESPAWN) never escalate no matter how long
// they suppress.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// setUnknownEpisodeBound shrinks the #1632 UNKNOWN-episode bound for the
// test and restores it after (same pattern as setWatchdogTiming).
func setUnknownEpisodeBound(t *testing.T, d time.Duration) {
	t.Helper()
	orig := watchdogUnknownEpisodeBound
	watchdogUnknownEpisodeBound = d
	t.Cleanup(func() { watchdogUnknownEpisodeBound = orig })
}

// unknownVitals is a gatherer whose evidence is structurally missing —
// the incident's exact shape (sidecar, no /proc, status fine).
func unknownVitals() *fakeVitals {
	return &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: false, cpuErr: "cpu evidence unavailable cross-container (sidecar mode)"}}
}

// markerReasonAt reads and decodes the restart-reason marker, if present.
func markerReasonAt(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var m struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(data, &m) != nil {
		return "", false
	}
	return m.Reason, true
}

// TestWatchdog_UnknownEpisode_BoundedSoftKill: an UNKNOWN episode past
// the bound fires the restarter exactly once (episode latch), writes
// the distinct marker reason, and keeps suppressing afterwards.
func TestWatchdog_UnknownEpisode_BoundedSoftKill(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	setUnknownEpisodeBound(t, 500*time.Millisecond)
	markerPath := filepath.Join(t.TempDir(), "restart-reason.json")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", markerPath)

	srv := newHungServer(t, 200*time.Millisecond)
	fr := &fakeRestarter{}
	cache := runWatchdogLoop(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, unknownVitals())

	// Boot arms the watchdog (first healthy), then every poll fails and
	// every would-fire is UNKNOWN → suppressions accumulate until the
	// episode bound (500ms) escalates.
	require.Eventually(t, func() bool { return fr.callCount() >= 1 },
		15*time.Second, 50*time.Millisecond,
		"UNKNOWN episode older than the bound must escalate to a soft restart")

	// Settle several ticks: the episode latch must hold at exactly one.
	time.Sleep(400 * time.Millisecond)
	require.Equal(t, 1, fr.callCount(),
		"escalation must fire once per episode (latch), not per poll")
	require.False(t, cache.Snapshot().Healthy)

	reason, ok := markerReasonAt(markerPath)
	require.True(t, ok, "escalation must write the restart-reason marker")
	require.Equal(t, RestartReasonHealthWatchdogUnknownEpisode, reason)
}

// TestWatchdog_UnknownEpisode_BoundNotYetReached_Suppresses: below the
// bound, UNKNOWN suppresses with NO restart — the #892 policy holds for
// transient probe degradation.
func TestWatchdog_UnknownEpisode_BoundNotYetReached_Suppresses(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	// Bound far beyond the observation window.
	setUnknownEpisodeBound(t, 30*time.Second)

	srv := newHungServer(t, 200*time.Millisecond)
	fr := &fakeRestarter{}
	runWatchdogLoop(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, unknownVitals())

	time.Sleep(1 * time.Second)
	require.Zero(t, fr.callCount(),
		"UNKNOWN younger than the bound must suppress (no evidence → no kill)")
}

// TestWatchdog_UnknownEpisode_NonUnknownVerdictsNeverEscalate: the
// bound is keyed to UNKNOWN runs specifically. STARVED (and FLAT,
// RESPAWN) are honest evidence of a live/owned process — suppressing
// forever remains the correct #892 policy for them.
func TestWatchdog_UnknownEpisode_NonUnknownVerdictsNeverEscalate(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	setUnknownEpisodeBound(t, 300*time.Millisecond)

	for name, vit := range map[string]*fakeVitals{
		"starved": {v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 25}},
		"flat":    {v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 0}},
		"respawn": {v: vitalSigns{tcpRefused: true, pidGone: true}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newHungServer(t, 200*time.Millisecond)
			fr := &fakeRestarter{}
			runWatchdogLoop(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, vit)
			time.Sleep(800 * time.Millisecond)
			require.Zero(t, fr.callCount(),
				"%s suppression must never escalate — the process is alive/owned", name)
		})
	}
}

// TestWatchdog_UnknownEpisode_ClockReZeroesOnEvidence: a non-UNKNOWN
// verdict in the middle of an UNKNOWN run re-zeroes the episode clock —
// the bound ages CONSECUTIVE UNKNOWN, not cumulative UNKNOWN.
func TestWatchdog_UnknownEpisode_ClockReZeroesOnEvidence(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	setUnknownEpisodeBound(t, 600*time.Millisecond)

	srv := newHungServer(t, 200*time.Millisecond)
	fr := &fakeRestarter{}

	// Alternating gatherer: UNKNOWN, UNKNOWN, STARVED, repeat. The
	// UNKNOWN runs stay 2 polls (~80ms) — far under the 600ms bound;
	// every STARVED re-zeroes the clock. No escalation may fire.
	alternating := &alternatingVitals{
		steps: []vitalSigns{
			{tcpOpen: true, cpuKnown: false, cpuErr: "unavailable"},
			{tcpOpen: true, cpuKnown: false, cpuErr: "unavailable"},
			{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 30},
		},
	}
	runWatchdogLoop(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, alternating)

	time.Sleep(1500 * time.Millisecond)
	require.Zero(t, fr.callCount(),
		"alternating UNKNOWN/starved must never age the UNKNOWN clock past the bound")
}

// alternatingVitals returns a different canned sample per gather call,
// cycling.
type alternatingVitals struct {
	n     int
	steps []vitalSigns
}

func (a *alternatingVitals) gather(context.Context) vitalSigns {
	a.n++
	return a.steps[(a.n-1)%len(a.steps)]
}

// TestWatchdog_UnknownEpisode_ResetClearsClock: after recovery the
// episode state resets — a SECOND wedge episode escalates again (once
// per episode), proving the latch is per-episode, not per-process.
func TestWatchdog_UnknownEpisode_ResetClearsClock(t *testing.T) {
	setWatchdogTiming(t, 40*time.Millisecond, 25*time.Millisecond, 3)
	setUnknownEpisodeBound(t, 400*time.Millisecond)

	var hang atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hang.Load() {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "vtest"})
			return
		}
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	fr := &fakeRestarter{}
	cache := runWatchdogLoop(t, srv.URL, 25*time.Millisecond, fr, idleSessions{}, unknownVitals())

	// Arm: first healthy poll completes boot.
	require.Eventually(t, func() bool { return cache.Snapshot().Initialized },
		5*time.Second, 20*time.Millisecond)

	// Episode 1: hang → UNKNOWN suppressions age to the bound → fire 1.
	hang.Store(true)
	require.Eventually(t, func() bool { return fr.callCount() == 1 },
		15*time.Second, 50*time.Millisecond, "first UNKNOWN episode must escalate")

	// Recover: latch + episode clock reset.
	hang.Store(false)
	require.Eventually(t, func() bool { return cache.Snapshot().Healthy },
		5*time.Second, 50*time.Millisecond)

	// Episode 2: a fresh UNKNOWN episode must age and fire again.
	hang.Store(true)
	require.Eventually(t, func() bool { return fr.callCount() == 2 },
		20*time.Second, 50*time.Millisecond,
		"second UNKNOWN episode after recovery must escalate again (per-episode latch)")

	time.Sleep(300 * time.Millisecond)
	require.Equal(t, 2, fr.callCount(), "each episode fires exactly once")
}
