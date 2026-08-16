// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// watchdog_vitals_test.go — tests for starvation corroboration.
//
// The refreshIsHealthyLoop integration tests here shrink the loop timing
// vars (readinessRefreshInterval etc.) instead of sleeping for the
// production 5s/4s cadence — the same var-injection pattern the suite
// already uses for getAgentAddr/setAgentAddr.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- vitalSigns.classify ---

func TestVitalSigns_Classify(t *testing.T) {
	cases := []struct {
		name string
		v    vitalSigns
		want verdict
	}{
		{"tcp refused is decisive hang", vitalSigns{tcpRefused: true, cpuKnown: true, cpuDeltaTicks: 50}, verdictHung},
		{"zero value is unknown", vitalSigns{}, verdictUnknown},
		{"cpu unknown is unknown", vitalSigns{tcpOpen: true, cpuErr: "no agent pid available"}, verdictUnknown},
		{"flat cpu over window is hang", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 1}, verdictHung},
		{"flat cpu below epsilon is hang", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: cpuFlatTicks - 0.5}, verdictHung},
		{"cpu at or above epsilon is starved (boundary inclusive)", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: cpuFlatTicks}, verdictStarved},
		{"advancing cpu is starved", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: cpuFlatTicks + 1}, verdictStarved},
		{"advancing cpu without tcp open is starved (dial timeout → backlog full)", vitalSigns{cpuKnown: true, cpuDeltaTicks: 40}, verdictStarved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := tc.v.classify()
			assert.Equal(t, tc.want, got)
			assert.NotEmpty(t, why, "every verdict must carry evidence for logs")
		})
	}
}

// --- proc readers ---

func TestReadProcCPUTicks_ErrorsOnBadPid(t *testing.T) {
	_, err := readProcCPUTicks(-1)
	require.Error(t, err, "negative pid cannot resolve")
	_, err = readProcCPUTicks(0)
	require.Error(t, err, "pid 0 does not exist in /proc")
}

func TestProcVitalsGatherer_NoPID(t *testing.T) {
	g := newProcVitalsGatherer("127.0.0.1:1", func() int { return 0 })
	v := g.gather(context.Background())
	assert.False(t, v.cpuKnown, "pid 0 must yield unknown cpu evidence")
	assert.Contains(t, v.cpuErr, "no agent pid available")
}

func TestProcVitalsGatherer_SelfAdvancing(t *testing.T) {
	// A CPU-burning goroutine in THIS test process plus a self-pid
	// gatherer must classify as starved (loop advancing). Exercises the
	// real /proc reader and the real sample window end-to-end.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = time.Now().UnixNano() % 13
			}
		}
	}()
	defer close(stop)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	g := &procVitalsGatherer{
		addr:         ln.Addr().String(),
		pidFn:        func() int { return os.Getpid() },
		dialTimeout:  500 * time.Millisecond,
		sampleWindow: 500 * time.Millisecond,
	}
	v := g.gather(context.Background())
	assert.True(t, v.tcpOpen, "kernel must complete the handshake for a listening socket")
	assert.True(t, v.cpuKnown, "cpu evidence must be available for a live pid")
	assert.GreaterOrEqual(t, v.cpuDeltaTicks, cpuFlatTicks, "a burning loop must advance its CPU counter")
	got, why := v.classify()
	assert.Equal(t, verdictStarved, got, why)
}

func TestProcVitalsGatherer_TCPRefused(t *testing.T) {
	// Grab a port then close it so nothing is listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	g := &procVitalsGatherer{addr: addr, pidFn: selfPID, dialTimeout: 500 * time.Millisecond, sampleWindow: 10 * time.Millisecond}
	v := g.gather(context.Background())
	assert.True(t, v.tcpRefused, "closed port must be reported refused")
	assert.False(t, v.tcpOpen)
	got, _ := v.classify()
	assert.Equal(t, verdictHung, got)
}

func TestProcVitalsGatherer_PIDChangeInvalidatesCPU(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	var calls atomic.Int32
	g := &procVitalsGatherer{
		addr: ln.Addr().String(),
		// First pidFn call must return a REAL pid (this process) so the
		// first /proc read succeeds; the post-window call returns a
		// different pid, which must invalidate the delta.
		pidFn: func() int {
			if calls.Add(1) == 1 {
				return os.Getpid()
			}
			return os.Getpid() + 1
		},
		dialTimeout:  500 * time.Millisecond,
		sampleWindow: 10 * time.Millisecond,
	}
	v := g.gather(context.Background())
	assert.False(t, v.cpuKnown, "a pid change mid-sample must invalidate the delta")
	assert.Contains(t, v.cpuErr, "changed during sample")
	got, _ := v.classify()
	assert.Equal(t, verdictUnknown, got, "invalidated evidence must be unknown, never guessed")
}

func selfPID() int { return os.Getpid() }

// --- refreshIsHealthyLoop integration (fast timing) ---

// fakeVitals returns a canned sample.
type fakeVitals struct{ v vitalSigns }

func (f *fakeVitals) gather(context.Context) vitalSigns { return f.v }

// fakeBusy always reports sessions busy.
type fakeBusy struct{}

func (fakeBusy) anyBusy() bool { return true }

// setWatchdogTiming shrinks the loop timing vars for the duration of the
// test and restores them after.
func setWatchdogTiming(t *testing.T, interval, timeout time.Duration, threshold int) {
	t.Helper()
	oi, ot, oth := readinessRefreshInterval, readinessRefreshTimeout, readinessFailureThreshold
	readinessRefreshInterval, readinessRefreshTimeout, readinessFailureThreshold = interval, timeout, threshold
	t.Cleanup(func() {
		readinessRefreshInterval, readinessRefreshTimeout, readinessFailureThreshold = oi, ot, oth
	})
}

// newHungServer returns a mock opencode that answers healthy once (arming
// the watchdog via bootCompleted) and then hangs past the probe timeout.
func newHungServer(t *testing.T, hang time.Duration) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "vtest"})
			return
		}
		time.Sleep(hang)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runWatchdogLoop(t *testing.T, srvURL string, timeout time.Duration, restarter healthWatchdogRestarter, busy sessionBusyChecker, vit vitalsGatherer) *healthzCache {
	t.Helper()
	orig := getAgentAddr()
	setAgentAddr(srvURL)
	t.Cleanup(func() { setAgentAddr(orig) })

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: timeout}}
	cache := newHealthzCache()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, restarter, busy, vit)
	return cache
}

func TestRefreshIsHealthyLoop_WatchdogSuppressesWhenStarved(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	starved := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 25}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, starved)

	time.Sleep(1200 * time.Millisecond) // ~20 polls: threshold crossed many times over
	assert.False(t, cache.Snapshot().Healthy, "probe failures must still mark the cache unhealthy")
	assert.Zero(t, fr.callCount(),
		"watchdog must NOT restart a process whose event loop is advancing (starved, not hung) — incident 2026-08-15")
}

func TestRefreshIsHealthyLoop_WatchdogFiresOnCorroboratedHang(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	hung := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 0}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, hung)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Equal(t, 1, fr.callCount(),
		"corroborated hang (loop idle while connections queue) must fire exactly once (latch)")
}

func TestRefreshIsHealthyLoop_VitalsUnknownFires(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	unknown := &fakeVitals{v: vitalSigns{}} // no evidence at all

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, unknown)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Equal(t, 1, fr.callCount(),
		"inconclusive vitals must preserve pre-corroboration behavior: fire. A bug in the probe must never make the watchdog toothless")
}

func TestRefreshIsHealthyLoop_MaxDeferForceSuppressedWhenStarved(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	origMax := watchdogMaxDeferrals
	watchdogMaxDeferrals = 2
	t.Cleanup(func() { watchdogMaxDeferrals = origMax })

	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	starved := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 25}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, fakeBusy{}, starved)

	// With maxDefers=2 and 60ms polls, the force path is reached ~4 times
	// in 1.2s. Old behavior: restart fires at the first force. New: the
	// starved verdict re-arms the deferral window every time.
	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Zero(t, fr.callCount(),
		"max-defer force must not kill busy sessions when vitals prove opencode is progressing — the force exists for stale busy state on a HUNG process")
}

func TestHealthWatchdog_ResetClearsStarvationState(t *testing.T) {
	wd := &healthWatchdog{}
	wd.starvedCount = 42
	wd.starvedLogged = true
	wd.standDownLogged = true
	wd.reset()
	assert.Zero(t, wd.starvedCount)
	assert.False(t, wd.starvedLogged)
	assert.False(t, wd.standDownLogged)
}
