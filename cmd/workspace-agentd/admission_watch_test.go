// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// admission_watch_test.go — #2045 pins: the prompt-path admission canary.
// opencode answers plain HTTP fast (401s) while ONE session's prompt
// pipeline is dead ("No user message found in stream" wedge, Nov 13-15
// 2026) — the supervisor's TCP-dial + /global/health model cannot see
// it. These pins hold the full escalation:
//
//   - CLASSIFICATION: only await-headers timeouts (client timeout /
//     context deadline) are admission failures. Fast provider 4xx
//     refusals and slow-BODY successes never count (R5).
//   - THE CANARY: ≥3 classified failures on ONE session inside the
//     10-minute window declare a wedge suspect and request ONE restart
//     through the EXISTING session-aware decision seam (busy deferral,
//     markers, metrics, restart log — R1/R2).
//   - STORM GUARD: at most one FIRED restart per 30 minutes per
//     workspace, anchored on the last fire; suppressed requests still
//     count (R2).
//   - TURN-RESUME (R4): a repeated timeout-class resume failure within
//     the cooldown escalates WARN → ERROR with wedge_suspect=true and
//     increments the suspects counter.
//
// The e2e pins drive the REAL wiring (turn-resume supervisor →
// ResumeTurn → classification → canary → wedgeRestartFunc →
// makeSessionAwareRestartDecision) against fake-opencode harnesses in
// the managed_process_test.go / admission_incident_repro_test.go style.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// fakeOpencodeServe mounts the admission-path fixtures on an httptest
// server. Each mode models one wire shape from the incident:
//
//   - slowHeaders: the WEDGE — the POST hangs before any response
//     headers; client.Do dies awaiting headers (the classified class);
//   - slowBody: headers land fast, the body drips — a SUCCESSFUL
//     admission (Do returned; the drain never classifies);
//   - refuse401: the fast provider-refusal shape — never counts.
type fakeOpencodeServe struct {
	srv *httptest.Server
}

func newFakeOpencodeServe(t *testing.T, mode string) *fakeOpencodeServe {
	t.Helper()
	f := &fakeOpencodeServe{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/session/") || !strings.HasSuffix(r.URL.Path, "/message") {
			w.WriteHeader(http.StatusOK)
			return
		}
		switch mode {
		case "slowHeaders":
			// The wedged pipeline: never answer. The client times out
			// awaiting headers.
			time.Sleep(2 * time.Second)
			w.WriteHeader(http.StatusOK)
		case "slowBody":
			// Headers land immediately (admission SUCCEEDED); the body
			// is slow. The Flush pushes the headers out of the server's
			// bufio — without it nothing reaches the client until the
			// handler returns, which would be the slow-headers shape.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"info":{"id":"msg_1"`))
			if rc := http.NewResponseController(w); rc != nil {
				_ = rc.Flush()
			}
			time.Sleep(2 * time.Second)
			_, _ = w.Write([]byte(`}}`))
		case "refuse401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"provider auth refused"}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpencodeServe) url() string { return f.srv.URL }

// quickClient is the OpenCodeClient with a test-shrunk whole-request
// timeout (the production shape: main.go's 5s http.Client fires before
// the 30s resume ctx — both are the timeout class).
func quickClient(base string) *OpenCodeClient {
	orig := agentAddrAtomic.Load()
	agentAddrAtomic.Store(base)
	_ = orig
	return &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 150 * time.Millisecond}}
}

// freshWatch builds an isolated canary (no production singleton state).
func freshWatch() *admissionWatch {
	return newAdmissionWatch(nil)
}

// installWedgeFire wires the watch's fire through the REAL
// wedgeRestartFunc seam (marker + metric + restart log + the
// session-aware decision) with a test-shrunk poll interval.
func installWedgeFire(t *testing.T, w *admissionWatch, proc restartableProcess, tracker *sessionStatusTracker, restarts *agentRestartLog) {
	t.Helper()
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(t.TempDir(), "marker"))
	w.setFire(wedgeRestartFunc(context.Background(), nil, proc, tracker, nil, nil, restarts, 5*time.Millisecond))
}

// awaitRestart polls (bounded) for the mock proc to reach n restarts —
// the fire is async by design (send paths never block on a restart).
func awaitRestart(t *testing.T, proc *mockManagedProcess, n int, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return proc.restartCount() >= n }, 5*time.Second, 5*time.Millisecond, msg)
}

// withObservedGlobalLog swaps the package log for an observer and
// returns the recorded logs (restored on cleanup).
func withObservedGlobalLog(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	prev := log
	core, logs := observer.New(zapcore.DebugLevel)
	log = zap.New(core)
	t.Cleanup(func() { log = prev })
	return logs
}

// ---------------------------------------------------------------------------
// classification (R1/R5)
// ---------------------------------------------------------------------------

func TestClassifyAdmission_Table(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		classified bool
	}{
		{"client whole-request timeout", &url.Error{Op: "Post", URL: "http://x/s/m", Err: timeoutErr{}}, true},
		{"awaiting-headers ctx deadline", context.DeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), true},
		{"url.Error wrapping deadline", &url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}, true},
		{"canceled context (shutdown)", context.Canceled, false},
		{"url.Error wrapping canceled", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, false},
		{"connection refused (fast fail)", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("dial refused")}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAdmission("ses_a", tc.err)
			if tc.classified {
				require.Error(t, got, "must be classified as an admission timeout")
				assert.True(t, isAdmissionTimeout(got), "classified error must satisfy isAdmissionTimeout")
				var at *errAdmissionTimeout
				if errors.As(got, &at) {
					assert.Equal(t, "ses_a", at.sessionID, "classification must carry the session id")
				}
			} else {
				assert.NoError(t, got, "must NOT be classified as an admission timeout")
			}
		})
	}
}

// timeoutErr is the minimal net.Error timeout shape http.Client returns
// on a whole-request timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestResumeTurn_SlowHeadersTimeout_ClassifiedAsAdmissionTimeout(t *testing.T) {
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "slowHeaders")
	c := quickClient(f.url())

	start := time.Now()
	err := c.ResumeTurn(context.Background(), "ses_a", "probe")
	require.Error(t, err, "the hung POST must fail on the client timeout")
	assert.Less(t, time.Since(start), time.Second, "the client timeout (150ms) must fire, not the wedge's 2s answer")
	assert.True(t, isAdmissionTimeout(err), "await-headers timeout must classify as errAdmissionTimeout, got %T: %v", err, err)
}

func TestResumeTurn_SlowBody_IsSuccessfulAdmission_NeverClassified(t *testing.T) {
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "slowBody")
	c := quickClient(f.url())

	// Do returns when the headers land; the drain ignores the
	// mid-body timeout — a successful admission, never a strike.
	err := c.ResumeTurn(context.Background(), "ses_a", "probe")
	assert.NoError(t, err, "slow BODY is a successful admission (headers landed): ResumeTurn must succeed")
}

func TestResumeTurn_FastRefusal_NeverClassified(t *testing.T) {
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "refuse401")
	c := quickClient(f.url())

	err := c.ResumeTurn(context.Background(), "ses_a", "probe")
	require.Error(t, err, "a 401 must surface as an error")
	assert.False(t, isAdmissionTimeout(err), "a fast provider 4xx refusal is NOT an admission timeout (R5)")
}

// ---------------------------------------------------------------------------
// the canary (R1) — unit pins on the ring
// ---------------------------------------------------------------------------

func TestAdmissionWatch_ThreeStrikesSameSession_DeclareSuspect(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))
	failuresBefore := testutil.ToFloat64(pkgOpsMetrics.admissionFailuresCounter("unknown"))

	w := freshWatch()
	now := time.Now()
	w.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	assert.Equal(t, suspectsBefore+1, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"3 strikes in the window must declare exactly one wedge suspect")
	assert.Equal(t, failuresBefore+3, testutil.ToFloat64(pkgOpsMetrics.admissionFailuresCounter("unknown")),
		"every classified failure ticks the failures counter")
	assert.True(t, w.suspect("ses_a"), "ses_a must be suspect after 3 strikes")
	assert.False(t, w.suspect("ses_b"), "an unstruck session is never suspect")
}

func TestAdmissionWatch_SubThresholdSilence(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	w := freshWatch()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
	now = now.Add(time.Minute)
	w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
	now = now.Add(time.Minute)

	assert.Equal(t, suspectsBefore, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"2 strikes in the window must NOT declare a suspect")
	assert.False(t, w.suspect("ses_a"))
}

func TestAdmissionWatch_StrikesAgeOutOfWindow(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	w := freshWatch()
	now := time.Now()
	w.now = func() time.Time { return now }
	w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
	now = now.Add(11 * time.Minute) // strike 1 aged out (window is 10m)
	w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
	now = now.Add(time.Minute)
	w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
	now = now.Add(time.Minute)

	assert.Equal(t, suspectsBefore, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"strikes older than the 10-minute window never count toward the threshold")
	assert.False(t, w.suspect("ses_a"))
}

func TestAdmissionWatch_SessionIsolation(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	w := freshWatch()
	now := time.Now()
	w.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		w.record(&errAdmissionTimeout{sessionID: fmt.Sprintf("ses_%d", i), cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	assert.Equal(t, suspectsBefore, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"strikes spread across sessions never cross the per-session threshold")
}

func TestAdmissionWatch_UnclassifiedErrorsNeverCount(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	failuresBefore := testutil.ToFloat64(pkgOpsMetrics.admissionFailuresCounter("unknown"))

	w := freshWatch()
	for i := 0; i < 10; i++ {
		w.record(fmt.Errorf("POST /session/ses_a/message returned 401: refused"))
		w.record(context.Canceled)
		w.record(nil)
	}

	assert.Equal(t, failuresBefore, testutil.ToFloat64(pkgOpsMetrics.admissionFailuresCounter("unknown")),
		"4xx refusals, canceled contexts, and nil never tick the failures counter")
	assert.False(t, w.suspect("ses_a"))
}

func TestAdmissionWatch_SuspectDeclarationsDedupePerWindow(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	w := freshWatch()
	now := time.Now()
	w.now = func() time.Time { return now }
	// A continuous 1-minute failure stream for 25 minutes: many
	// threshold crossings, but the suspects counter ticks once per
	// session per window (windows at ~T+2, T+12, T+22 → 3 ticks).
	for i := 0; i < 25; i++ {
		w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	assert.Equal(t, suspectsBefore+3, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"the suspects counter must dedupe per session per window")
}

func TestAdmissionWatch_NilFire_ObservabilityOnly(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	w := freshWatch() // fire == nil (unwired topology)
	assert.NotPanics(t, func() {
		for i := 0; i < 5; i++ {
			w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
		}
	})
	assert.Equal(t, suspectsBefore+1, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"the suspects counter still ticks with no restart trigger wired")
}

func TestAdmissionWatch_ConcurrentRecord(t *testing.T) {
	w := freshWatch()
	var wg sync.WaitGroup
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				w.record(&errAdmissionTimeout{sessionID: sid, cause: errors.New("t")})
			}
		}(fmt.Sprintf("ses_%d", s))
	}
	wg.Wait()
	for s := 0; s < 8; s++ {
		assert.True(t, w.suspect(fmt.Sprintf("ses_%d", s)), "every session must cross the threshold under concurrency")
	}
}

// ---------------------------------------------------------------------------
// storm guard (R2)
// ---------------------------------------------------------------------------

func TestAdmissionWatch_StormGuard_FiveSuspectsAtMostOneRestartPer30Minutes(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suppressedBefore := testutil.ToFloat64(pkgOpsMetrics.restartSuppressedCounter("unknown", RestartReasonPromptPathWedge))

	proc := &mockManagedProcess{}
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), newAgentRestartLog())

	now := time.Now()
	w.now = func() time.Time { return now }

	// Five distinct suspect crossings, 12 minutes apart — inside any
	// 30-minute window there are ≥2 crossings, yet at most ONE restart
	// may fire per 30 minutes (the storm guard).
	crossing := func() {
		for i := 0; i < 3; i++ {
			w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
			now = now.Add(time.Minute)
		}
	}
	crossing() // T+0..T+2  → FIRE #1 (first)
	now = now.Add(9 * time.Minute)
	crossing() // T+14..T+16 → suppressed (14m < 30m since fire)
	now = now.Add(9 * time.Minute)
	crossing() // T+26..T+28 → suppressed (26m < 30m since fire)
	now = now.Add(9 * time.Minute)
	crossing() // T+38..T+40 → FIRE #2 (≥30m since fire #1)
	now = now.Add(9 * time.Minute)
	crossing() // T+50..T+52 → suppressed

	awaitRestart(t, proc, 2, "exactly two fires across ~50 minutes — one per 30-minute window")
	// Give any spurious third fire a beat to surface.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 2, proc.restartCount(),
		"five suspects in a row must yield at most one restart per 30 minutes")
	assert.GreaterOrEqual(t,
		testutil.ToFloat64(pkgOpsMetrics.restartSuppressedCounter("unknown", RestartReasonPromptPathWedge))-suppressedBefore,
		float64(3), "suppressed requests still count (three crossings were withheld)")
}

func TestAdmissionWatch_StormGuardAnchoredOnFire_PersistentWedgeKeepsRetrying(t *testing.T) {
	proc := &mockManagedProcess{}
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), newAgentRestartLog())

	now := time.Now()
	w.now = func() time.Time { return now }

	// Suspect crossings ~33 minutes apart: the guard anchors on the
	// last FIRE, so a persistent wedge retries at the ~30-minute
	// cadence instead of starving itself silent.
	for i := 0; i < 4; i++ {
		for j := 0; j < 3; j++ {
			w.record(&errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("t")})
			now = now.Add(time.Minute)
		}
		now = now.Add(30 * time.Minute)
	}

	awaitRestart(t, proc, 4,
		"crossings at T+0..121m fire at T+0/33/66/99 — a persistent wedge retries every ~30 minutes, never storms")
}

func TestAdmissionWatch_ContinuousFailureStream_KeepsRequestingEveryWindow(t *testing.T) {
	// The complement to the anchored-on-fire pin: a failure stream that
	// NEVER lets the ring decay (strikes every minute for two hours)
	// must still re-request every window — the request arm is
	// timestamped, not a latch — so the storm guard (fires) and the
	// re-arm (requests) jointly bound the wedge to one restart per 30
	// minutes with visible suppressions in between.
	t.Setenv("WORKSPACE_ID", "")
	suppressedBefore := testutil.ToFloat64(pkgOpsMetrics.restartSuppressedCounter("unknown", RestartReasonPromptPathWedge))

	proc := &mockManagedProcess{}
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), newAgentRestartLog())

	now := time.Now()
	w.now = func() time.Time { return now }

	for i := 1; i <= 120; i++ { // 1-min cadence, ring never decays
		w.record(&errAdmissionTimeout{sessionID: "ses_stuck", cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	awaitRestart(t, proc, 4, "fires at T+3/33/63/93 — one restart per 30 minutes under a continuous wedge")
	assert.Equal(t, suppressedBefore+8,
		testutil.ToFloat64(pkgOpsMetrics.restartSuppressedCounter("unknown", RestartReasonPromptPathWedge)),
		"the 10-minute re-arms between fires surface as suppressed restart requests (still count/alert)")
}

// ---------------------------------------------------------------------------
// born-red e2e — the REAL wiring (supervisor → ResumeTurn → canary → seam)
// ---------------------------------------------------------------------------

func TestAdmissionCanary_E2E_SlowHeaders_RestartRequestedWithSessionID(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(t.TempDir(), "marker"))
	restartsBefore := testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge))

	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "slowHeaders")
	c := quickClient(f.url())

	proc := &mockManagedProcess{}
	tracker := newSessionStatusTracker()
	restartLog := newAgentRestartLog()
	w := freshWatch()
	installWedgeFire(t, w, proc, tracker, restartLog)

	sup := newTurnResumeSupervisorWithWatch(c, w)

	// Three interrupted-turn events on ses_a (the shared fixtures'
	// session): each fires a resume → ResumeTurn POST → hung headers →
	// client timeout → classified → recorded. The third strike crosses.
	turnEventFeed(t, sup, evUserMsg1732, evCut1732, evCut1732, evCut1732)

	awaitRestart(t, proc, 1, "3 slow-header admission failures on one session must request a supervised restart")

	// The restart-log reason carries the offending session id.
	snap := restartLog.snapshot()
	require.NotNil(t, snap, "the wedge restart must land in the agentRestartLog")
	require.NotNil(t, snap.LastAgentRestart, "the wedge restart must be the last recorded restart")
	assert.Contains(t, snap.LastAgentRestart.Reason, RestartReasonPromptPathWedge,
		"the restart reason must carry the prompt_path_wedge class")
	assert.Contains(t, snap.LastAgentRestart.Reason, "ses_a",
		"the restart reason must carry the offending session id")

	// The restart-reason marker was written with the class reason.
	m, ok := readRestartReasonMarker(markerPathFromEnv(), zap.NewNop().Core())
	require.True(t, ok, "the wedge fire must write the restart-reason marker")
	assert.Equal(t, RestartReasonPromptPathWedge, m.Reason)

	assert.Equal(t, restartsBefore+1,
		testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge)),
		"workspace_restarts_total{reason=prompt_path_wedge} ticks once")
}

func TestAdmissionCanary_E2E_SlowBodyAdmissions_NeverRestart(t *testing.T) {
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "slowBody")
	c := quickClient(f.url())

	proc := &mockManagedProcess{}
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), newAgentRestartLog())
	sup := newTurnResumeSupervisorWithWatch(c, w)

	// Far more resume attempts than the threshold — a slow BODY is a
	// successful admission (ResumeTurn returns nil), so nothing ever
	// records and nothing ever restarts.
	for i := 0; i < 6; i++ {
		turnEventFeed(t, sup, evCut1732)
	}
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 0, proc.restartCount(),
		"slow-body (successful) admissions must never count toward the wedge threshold")
	assert.False(t, w.suspect("ses_a"))
}

func TestAdmissionCanary_E2E_ProviderRefusals_NeverRestart(t *testing.T) {
	orig := agentAddrAtomic.Load()
	defer agentAddrAtomic.Store(orig)
	f := newFakeOpencodeServe(t, "refuse401")
	c := quickClient(f.url())

	proc := &mockManagedProcess{}
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), newAgentRestartLog())
	sup := newTurnResumeSupervisorWithWatch(c, w)

	for i := 0; i < 6; i++ {
		turnEventFeed(t, sup, evCut1732)
	}
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, 0, proc.restartCount(),
		"fast provider 4xx refusals must never count toward the wedge threshold (R5)")
	assert.False(t, w.suspect("ses_a"))
}

// ---------------------------------------------------------------------------
// busy deferral through the EXISTING seam (R2/R5)
// ---------------------------------------------------------------------------

func TestWedgeRestartFunc_BusySessions_DeferThroughExistingSeam(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	deferralsBefore := testutil.ToFloat64(restartDeferralsFired)

	proc := &mockManagedProcess{}
	tracker := newSessionStatusTracker()
	tracker.set("ses_other", "busy") // ANOTHER session mid-turn: defer

	w := freshWatch()
	installWedgeFire(t, w, proc, tracker, newAgentRestartLog())

	now := time.Now()
	w.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		w.record(&errAdmissionTimeout{sessionID: "ses_wedged", cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	// The wedge's restart request went through the session-aware
	// decision: the busy session defers it (no immediate restart), and
	// the deferral metric — the SAME one every restart class uses —
	// ticks.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, proc.restartCount(),
		"a busy session must defer the wedge restart (existing deferral semantics)")
	assert.Greater(t, testutil.ToFloat64(restartDeferralsFired), deferralsBefore,
		"agentd_restart_deferrals_fired_total must tick through the wedge's deferral")

	// The busy session goes idle → the deferred restart applies.
	tracker.set("ses_other", "idle")
	awaitRestart(t, proc, 1, "the deferred wedge restart applies when sessions go idle")
}

// ---------------------------------------------------------------------------
// attribution: marker / metric / enum (R3)
// ---------------------------------------------------------------------------

func TestWedgeRestartFunc_Attribution_MarkerMetricRestartLog(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(t.TempDir(), "marker"))
	restartsBefore := testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge))

	proc := &mockManagedProcess{}
	restartLog := newAgentRestartLog()
	w := freshWatch()
	installWedgeFire(t, w, proc, newSessionStatusTracker(), restartLog)

	now := time.Now()
	w.now = func() time.Time { return now }
	// TWO wedged sessions, interleaved at a 1-minute cadence for 35
	// minutes. Fire #1 (ses_x's 3rd strike at T+4) names the crossing
	// session; fire #2 (~T+34, the storm guard's re-arm) names BOTH
	// current suspects — the multi-wedge attribution.
	for i := 0; i < 35; i++ {
		w.record(&errAdmissionTimeout{sessionID: "ses_x", cause: errors.New("t")})
		now = now.Add(time.Minute)
		w.record(&errAdmissionTimeout{sessionID: "ses_y", cause: errors.New("t")})
		now = now.Add(time.Minute)
	}

	awaitRestart(t, proc, 3, "the interleaved double wedge fires at ~30-minute cadence (T+4/34/64)")

	snap := restartLog.snapshot()
	require.NotNil(t, snap.LastAgentRestart)
	assert.Contains(t, snap.LastAgentRestart.Reason, "ses_x",
		"the restart-log reason carries every offending session id")
	assert.Contains(t, snap.LastAgentRestart.Reason, "ses_y",
		"the multi-wedge fire names all current suspects")

	assert.Equal(t, restartsBefore+3,
		testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge)))
}

func TestRestartReasonPromptPathWedge_EnumTriplePin(t *testing.T) {
	// (1) The control socket's CLOSED enum accepts the reason — the
	// socket rejects unknown reasons (A.4 invariant 2), so a member
	// missing here would bounce the sidecar's wedge restart.
	assert.True(t, restartReasons[RestartReasonPromptPathWedge],
		"prompt_path_wedge must be a member of the control socket's closed reason enum")

	// (2) The enum member crosses the REAL socket round trip.
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	cc := newControlClient(srv.addr())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := cc.Restart(ctx, RestartReasonPromptPathWedge, 5)
	require.NoError(t, err, "the real control socket must accept the wedge reason")
	require.NotNil(t, res)
	assert.True(t, res.Restarted, "the fake proc restarts")
	require.NotNil(t, proc.lastReason.Load(), "the supervisor-side proc must receive the reason")
	assert.Equal(t, RestartReasonPromptPathWedge, *proc.lastReason.Load(),
		"the reason must arrive verbatim — never misattributed as credential_reload")

	// (3) The metric label is the same constant.
	assert.Equal(t, "prompt_path_wedge", RestartReasonPromptPathWedge)
}

// ---------------------------------------------------------------------------
// sidecar topology: wedgeRestartProc over the control socket
// ---------------------------------------------------------------------------

func TestWedgeRestartProc_ControlSocket_CarriesWedgeReason(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(t.TempDir(), "marker"))
	restartsBefore := testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge))

	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	cc := newControlClient(srv.addr())
	restartLog := newAgentRestartLog()

	p := &wedgeRestartProc{cc: cc, restarts: restartLog}
	p.fire([]string{"ses_side"})

	require.NotNil(t, proc.lastReason.Load(), "the wedge restart must cross the control socket")
	assert.Equal(t, RestartReasonPromptPathWedge, *proc.lastReason.Load())
	assert.Equal(t, int64(1), proc.restarts.Load())

	assert.Equal(t, restartsBefore+1,
		testutil.ToFloat64(pkgOpsMetrics.restartCounter("unknown", RestartReasonPromptPathWedge)),
		"the sidecar records the outcome-truthful restart metric")

	snap := restartLog.snapshot()
	require.NotNil(t, snap.LastAgentRestart)
	assert.Contains(t, snap.LastAgentRestart.Reason, "ses_side",
		"the sidecar's restart log carries the offending session id")

	m, ok := readRestartReasonMarker(markerPathFromEnv(), zap.NewNop().Core())
	require.True(t, ok)
	assert.Equal(t, RestartReasonPromptPathWedge, m.Reason)
}

// ---------------------------------------------------------------------------
// R4: turn_resume escalation
// ---------------------------------------------------------------------------

func Test1732_TurnResume_RepeatedAdmissionTimeout_EscalatesToErrorAndSuspectCounter(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	logs := withObservedGlobalLog(t)
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	// The wedged-harness prompter: every resume POST dies on the
	// client timeout (the classified class).
	p := &fakeResumePrompter{err: &errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("hang")}}
	w := freshWatch()
	s := newTurnResumeSupervisorWithWatch(p, w)

	turnEventFeed(t, s, evUserMsg1732, evCut1732) // failure #1 → WARN
	turnEventFeed(t, s, evCut1732)                // failure #2 → ERROR + suspect

	// R4 asserted via METRICS (per the acceptance criteria): the second
	// timeout failure within the cooldown increments the suspects
	// counter.
	assert.Equal(t, suspectsBefore+1, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"the second admission-timeout resume failure must increment the wedge-suspects counter")

	// The log-field pin: the escalation is ERROR with wedge_suspect=true
	// (zap.Bool carries the value in the field's Integer slot).
	escalated := false
	for _, entry := range logs.All() {
		if entry.Level == zapcore.ErrorLevel && strings.Contains(entry.Message, "auto-resume") {
			for _, f := range entry.Context {
				if f.Key == "wedge_suspect" {
					escalated = true
					assert.NotZero(t, f.Integer, "wedge_suspect must be true on the ERROR escalation")
				}
			}
			assert.True(t, escalated, "the repeated-failure log must carry the wedge_suspect field")
		}
	}
	assert.True(t, escalated, "the repeated admission-timeout failure must log at ERROR")
}

func Test1732_TurnResume_FirstAdmissionTimeout_StaysWarnNoSuspect(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	logs := withObservedGlobalLog(t)
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	p := &fakeResumePrompter{err: &errAdmissionTimeout{sessionID: "ses_a", cause: errors.New("hang")}}
	s := newTurnResumeSupervisorWithWatch(p, freshWatch())

	turnEventFeed(t, s, evUserMsg1732, evCut1732) // the FIRST timeout failure

	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "auto-resume prompt failed") {
			assert.NotEqual(t, zapcore.ErrorLevel, entry.Level,
				"the first failure stays WARN — the escalation is for REPEATED failures")
		}
	}
	assert.Equal(t, suspectsBefore, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"the first timeout failure declares no suspect")
}

func Test1732_TurnResume_NonTimeoutFailures_NeverDeclareSuspect(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "")
	suspectsBefore := testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown"))

	// A fast 401 refusal from the harness: a failure, but never the
	// wedge class.
	p := &fakeResumePrompter{err: fmt.Errorf("POST /session/ses_a/message returned 401: refused")}
	s := newTurnResumeSupervisorWithWatch(p, freshWatch())

	for i := 0; i < 5; i++ {
		turnEventFeed(t, s, evCut1732)
	}

	assert.Equal(t, suspectsBefore, testutil.ToFloat64(pkgOpsMetrics.wedgeSuspectsCounter("unknown")),
		"non-timeout resume failures never declare a wedge suspect (R5)")
}

// ---------------------------------------------------------------------------
// wiring smoke: the singleton + install seams
// ---------------------------------------------------------------------------

func TestInstallAdmissionWatchRestart_NilProc_NoFire(t *testing.T) {
	assert.NotPanics(t, func() {
		installAdmissionWatchRestart(context.Background(), nil, nil, nil, nil, nil, nil)
	})
}

func TestPkgAdmissionWatch_RecordWithoutFire_NeverPanics(t *testing.T) {
	assert.NotPanics(t, func() {
		pkgAdmissionWatch.record(&errAdmissionTimeout{sessionID: "ses_z", cause: errors.New("t")})
	})
}
