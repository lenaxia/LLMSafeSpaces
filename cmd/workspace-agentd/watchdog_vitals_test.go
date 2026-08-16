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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// --- vitalSigns.classify ---

func TestVitalSigns_Classify(t *testing.T) {
	cases := []struct {
		name string
		v    vitalSigns
		want verdict
	}{
		{"tcp refused with live past-boot pid is the only hang", vitalSigns{tcpRefused: true, cpuKnown: true, cpuDeltaTicks: 50}, verdictHung},
		{"tcp refused with live past-boot pid (no cpu evidence) is hang", vitalSigns{tcpRefused: true}, verdictHung},
		{"tcp refused with pid gone is respawn (crash recovery owns it)", vitalSigns{tcpRefused: true, pidGone: true, cpuKnown: true, cpuDeltaTicks: 50}, verdictRespawn},
		{"tcp refused on a booting child is respawn (boot grace window)", vitalSigns{tcpRefused: true, booting: true, cpuKnown: true, cpuDeltaTicks: 50}, verdictRespawn},
		{"booting flag without refused dial is irrelevant to lethality", vitalSigns{tcpOpen: true, booting: true, cpuKnown: true, cpuDeltaTicks: 0}, verdictFlat},
		{"zero value is unknown", vitalSigns{}, verdictUnknown},
		{"cpu unknown is unknown", vitalSigns{tcpOpen: true, cpuErr: "no agent pid available"}, verdictUnknown},
		{"pid gone without refused dial is unknown", vitalSigns{tcpOpen: true, pidGone: true, cpuErr: "read /proc/1/stat: no such file"}, verdictUnknown},
		{"flat cpu over window is FLAT — blocked-IO is alive, not killable (#892)", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 1}, verdictFlat},
		{"flat cpu below epsilon is FLAT", vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: cpuFlatTicks - 0.5}, verdictFlat},
		{"flat cpu with dial timeout (backlog full) is FLAT", vitalSigns{cpuKnown: true, cpuDeltaTicks: 0}, verdictFlat},
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

func TestVitalSigns_SuppressionReason(t *testing.T) {
	cases := []struct {
		v    vitalSigns
		want string
	}{
		{vitalSigns{cpuKnown: true, cpuDeltaTicks: 40}, "starved"},
		{vitalSigns{tcpOpen: true, cpuKnown: true}, "flat"},
		{vitalSigns{tcpRefused: true, pidGone: true}, "respawn"},
		{vitalSigns{}, "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.v.suppressionReason())
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
	g := newProcVitalsGatherer("127.0.0.1:1", func() int { return 0 }, nil)
	v := g.gather(context.Background())
	assert.False(t, v.cpuKnown, "pid 0 must yield unknown cpu evidence")
	assert.Contains(t, v.cpuErr, "no agent pid available")
	assert.True(t, v.pidGone, "pid 0 must mark pidGone")
	if v.tcpRefused {
		// 127.0.0.1:1 is typically refused; refused + pidGone = respawn
		// (crash recovery owns it), never a kill.
		got, _ := v.classify()
		assert.Equal(t, verdictRespawn, got)
	} else {
		got, _ := v.classify()
		assert.Equal(t, verdictUnknown, got)
	}
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

	// No childBootAt (nil): a refused dial on a live PAST-BOOT pid is the
	// one lethal verdict.
	g := &procVitalsGatherer{addr: addr, pidFn: selfPID, dialTimeout: 500 * time.Millisecond, sampleWindow: 10 * time.Millisecond}
	v := g.gather(context.Background())
	assert.True(t, v.tcpRefused, "closed port must be reported refused")
	assert.False(t, v.tcpOpen)
	got, _ := v.classify()
	assert.Equal(t, verdictHung, got)
}

func TestProcVitalsGatherer_RefusedDuringBootWindowIsRespawn(t *testing.T) {
	// Review round 1 on #898, Finding 1: a freshly spawned child has a
	// live pid but no bound port yet — refused dial + young pid must be
	// RESPAWN (boot), never HUNG. This is the exact shape that produced
	// the incident's kill-churn.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	g := &procVitalsGatherer{
		addr:         addr,
		pidFn:        selfPID,
		childBootAt:  func() time.Time { return time.Now().Add(-5 * time.Second) },
		dialTimeout:  500 * time.Millisecond,
		sampleWindow: 10 * time.Millisecond,
	}
	v := g.gather(context.Background())
	assert.True(t, v.tcpRefused)
	assert.True(t, v.booting, "young child + refused dial must set booting")
	got, why := v.classify()
	assert.Equal(t, verdictRespawn, got, "refused dial inside the boot grace window must never be lethal: %s", why)
	assert.Equal(t, "respawn", v.suppressionReason())
}

func TestProcVitalsGatherer_RefusedAfterBootGraceIsHung(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	// Past the grace window: a live pid with nothing listening is a real
	// dead-listener hang again.
	g := &procVitalsGatherer{
		addr:         addr,
		pidFn:        selfPID,
		childBootAt:  func() time.Time { return time.Now().Add(-vitalsBootGraceWindow - time.Second) },
		dialTimeout:  500 * time.Millisecond,
		sampleWindow: 10 * time.Millisecond,
	}
	v := g.gather(context.Background())
	assert.True(t, v.tcpRefused)
	assert.False(t, v.booting, "past the grace window the boot exemption must lapse")
	got, _ := v.classify()
	assert.Equal(t, verdictHung, got)
}

func TestProcVitalsGatherer_CancelDuringSampleNeverKills(t *testing.T) {
	// Review round 1 minor: a refused dial collected just before context
	// cancel must not classify HUNG and fire a restart during shutdown.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead: gather must take the cancel path
	g := &procVitalsGatherer{
		addr:         addr,
		pidFn:        selfPID,
		childBootAt:  nil,
		dialTimeout:  500 * time.Millisecond,
		sampleWindow: 10 * time.Millisecond,
	}
	v := g.gather(ctx)
	assert.True(t, v.pidGone, "cancellation must force the no-evidence suppressing path")
	got, _ := v.classify()
	assert.NotEqual(t, verdictHung, got, "shutdown must never fire the kill path")
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
		// different pid, which must invalidate the delta AND mark the
		// generation change (pidGone).
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
	assert.True(t, v.pidGone, "a pid change mid-sample must mark pidGone")
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

// idleSessions reports no busy sessions, so the watchdog reaches its
// kill decision directly — required for regression tests asserting on
// fire-vs-suppress (fakeBusy would defer instead, masking the verdict).
type idleSessions struct{}

func (idleSessions) anyBusy() bool { return false }

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

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: timeout}}
	cache := newHealthzCache()
	ctx, cancel := context.WithCancel(context.Background())
	// Join the loop goroutine on cleanup BEFORE earlier-registered
	// cleanups (setWatchdogTiming) restore the timing vars the loop
	// reads — otherwise the restore write races the loop's next tick
	// (caught by -race in the suppression tests).
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshIsHealthyLoop(ctx, client, cache, testLogger(), nil, restarter, busy, vit)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		setAgentAddr(orig)
	})
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

func TestRefreshIsHealthyLoop_WatchdogFiresOnCorroboratedDeadListener(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	// The only lethal verdict: dial refused while the supervised pid is
	// alive (#892 / design 0050 D1).
	hung := &fakeVitals{v: vitalSigns{tcpRefused: true, cpuKnown: true, cpuDeltaTicks: 50}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, hung)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Equal(t, 1, fr.callCount(),
		"corroborated dead-listener hang must fire exactly once (latch)")
}

func TestRefreshIsHealthyLoop_WatchdogSuppressesWhenFlat(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	// Listener accepts, CPU flat: a turn blocked on upstream I/O is ALIVE.
	// #892 ruling: never kill on flat CPU.
	flat := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 0}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, flat)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Zero(t, fr.callCount(),
		"flat CPU (blocked-IO turn) must never be killed — recovery is honest state + informed Stop")
}

func TestRefreshIsHealthyLoop_VitalsUnknownSuppresses(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	unknown := &fakeVitals{v: vitalSigns{}} // no evidence at all

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, unknown)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Zero(t, fr.callCount(),
		"killing without evidence is banned (#892); probe degradation must surface via metric/log, not a restart")
}

func TestRefreshIsHealthyLoop_VitalsRespawnSuppresses(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	// Dial refused AND pid gone: crash recovery is mid-restart. A kill
	// here races the respawn (6-restarts-in-11-minutes incident shape).
	respawn := &fakeVitals{v: vitalSigns{tcpRefused: true, pidGone: true}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, nil, respawn)

	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Zero(t, fr.callCount(),
		"refused dial during respawn window must not race crash recovery's restart")
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

func TestRefreshIsHealthyLoop_MaxDeferForceSuppressedWhenFlat(t *testing.T) {
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	origMax := watchdogMaxDeferrals
	watchdogMaxDeferrals = 2
	t.Cleanup(func() { watchdogMaxDeferrals = origMax })

	srv := newHungServer(t, 500*time.Millisecond)
	fr := &fakeRestarter{}
	flat := &fakeVitals{v: vitalSigns{tcpOpen: true, cpuKnown: true, cpuDeltaTicks: 0}}

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, fakeBusy{}, flat)

	// With maxDefers=2 and 60ms polls, the force path is reached ~4 times
	// in 1.2s. Old behavior: restart fires at the first force. New: the
	// flat verdict re-arms the deferral window every time — a busy
	// blocked-IO turn must survive the force path.
	time.Sleep(1200 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy)
	assert.Zero(t, fr.callCount(),
		"max-defer force must not kill busy sessions when the listener accepts — blocked-IO turns are alive (#892)")
}

func TestHealthWatchdog_ResetClearsSuppressionState(t *testing.T) {
	wd := &healthWatchdog{}
	wd.suppressedCount = 42
	wd.suppressLogged = true
	wd.reset()
	assert.Zero(t, wd.suppressedCount)
	assert.False(t, wd.suppressLogged)
}

// TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess is the review
// round 1 regression on #898 Finding 1. A REAL subprocess (the fake
// opencode harness, pre-bind delay) presents exactly the misclassified
// state: pid alive, port not yet bound, health failures at threshold.
// The REAL gatherer and REAL watchdog loop must suppress (RESPAWN —
// booting), never kill. Pre-fix, refused+live-pid classified HUNG and
// fired. Supervisor lifecycle timing is deliberately out of scope here
// (covered by the D2 generation tests and the wiring smoke below) so
// the regression is deterministic.
func TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess(t *testing.T) {
	withTestLogger(t)
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 2)

	// Health endpoint that hangs past the probe timeout: failures
	// accumulate exactly like a starved/crashed pod.
	srv := newHungServer(t, 500*time.Millisecond)

	// A real, alive child whose port will not bind during the test:
	// the fake opencode with a 60s pre-bind delay.
	port := freeTCPPort(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess") //nolint:gosec // trusted test binary
	cmd.Env = []string{
		"GO_TEST_FAKE_OPENCODE=1",
		"FAKE_PORT=" + strconv.Itoa(port),
		"FAKE_BIND_DELAY_MS=60000",
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	childPID := cmd.Process.Pid
	spawnedAt := time.Now()

	fr := &fakeRestarter{}
	vit := newProcVitalsGatherer(
		fmt.Sprintf("127.0.0.1:%d", port),
		func() int { return childPID },
		func() time.Time { return spawnedAt },
	)

	cache := runWatchdogLoop(t, srv.URL, 40*time.Millisecond, fr, idleSessions{}, vit)

	// Several would-fire moments: threshold 2, poll 60ms — 700ms spans
	// ~10 fire decisions against the booting child.
	time.Sleep(700 * time.Millisecond)
	assert.False(t, cache.Snapshot().Healthy, "sanity: health genuinely failing")
	assert.Zero(t, fr.callCount(),
		"the watchdog must not kill a booting child — refused dial inside the boot grace is RESPAWN, not HUNG")

	// Direct evidence the real sample carries the booting state (not
	// merely an accidental suppress via another verdict).
	v := vit.gather(context.Background())
	assert.True(t, v.tcpRefused)
	assert.False(t, v.pidGone)
	assert.True(t, v.booting, "real gatherer must mark the young live pid as booting")
	got, _ := v.classify()
	assert.Equal(t, verdictRespawn, got)
}

// TestBuildVitalsGatherer_WiringSmoke verifies the production wiring
// shape: the gatherer targets the agent port form and uses the
// supervisor's pid + childStartedAt accessors (review round 1: the
// server.go wiring was untested).
func TestBuildVitalsGatherer_WiringSmoke(t *testing.T) {
	withTestLogger(t)
	port := freeTCPPort(t)
	p := newTestManagedProcess(t, port, 0)
	p.start()
	defer p.stop()
	requireFakeReachable(t, port, 2*time.Second)

	g := buildVitalsGatherer(p)
	require.NotNil(t, g)
	// The production addr is the fixed agent port, not the test fake's;
	// assert the shape rather than the value.
	assert.Equal(t, fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort), g.addr)
	assert.NotNil(t, g.childBootAt, "production wiring must pass childStartedAt so the boot grace is armed")
	assert.NotZero(t, g.childBootAt(), "a started supervisor must report its child's boot time")
	assert.True(t, p.pid() > 0)
}

// TestRecordWatchdogSuppression_Metric and TestRecordMarkerWriteFailure_Metric:
// emission tests for the two counters this PR adds (review round 1:
// "currently only exercised incidentally through package-level metrics").
func TestRecordWatchdogSuppression_Metric(t *testing.T) {
	reg := prometheus.NewRegistry()
	vec := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_watchdog_suppressions_total",
	}, []string{"workspace_id", "reason"})
	m := &opsMetrics{watchdogSuppressions: vec}

	m.RecordWatchdogSuppression("ws-a", "starved")
	m.RecordWatchdogSuppression("ws-a", "flat")
	m.RecordWatchdogSuppression("ws-a", "starved")
	m.RecordWatchdogSuppression("ws-a", "")
	m.RecordWatchdogSuppression("", "respawn")

	assert.Equal(t, 2.0, testutil.ToFloat64(vec.WithLabelValues("ws-a", "starved")))
	assert.Equal(t, 1.0, testutil.ToFloat64(vec.WithLabelValues("ws-a", "flat")))
	assert.Equal(t, 1.0, testutil.ToFloat64(vec.WithLabelValues("ws-a", "unknown")), "empty reason normalizes to unknown")
	assert.Equal(t, 1.0, testutil.ToFloat64(vec.WithLabelValues("unknown", "respawn")), "empty workspace normalizes to unknown")
}

func TestRecordMarkerWriteFailure_Metric(t *testing.T) {
	reg := prometheus.NewRegistry()
	vec := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "workspace_restart_marker_write_failures_total",
	}, []string{"workspace_id", "reason"})
	m := &opsMetrics{markerWriteFailures: vec}

	m.RecordMarkerWriteFailure("ws-b", "health_watchdog")
	m.RecordMarkerWriteFailure("ws-b", "health_watchdog")
	m.RecordMarkerWriteFailure("", "")

	assert.Equal(t, 2.0, testutil.ToFloat64(vec.WithLabelValues("ws-b", "health_watchdog")))
	assert.Equal(t, 1.0, testutil.ToFloat64(vec.WithLabelValues("unknown", "unknown")))
}
