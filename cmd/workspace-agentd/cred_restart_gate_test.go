// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// fakeCredentialPuller pins the rev arithmetic of the credential_reload
// gate without a live mux (the credentialPuller seam).
type fakeCredentialPuller struct {
	res spawnEnvResponse
	err error
}

func (f *fakeCredentialPuller) pullBounded(_ context.Context) (spawnEnvResponse, string, error) {
	if f.err != nil {
		return spawnEnvResponse{}, "pull_failed", f.err
	}
	return f.res, "", nil
}

// newGateTestProcess builds a managedProcess whose child terminates
// promptly on SIGTERM (fast restarts) using the standard test helper.
func newGateTestProcess(t *testing.T) *managedProcess {
	t.Helper()
	port := freeTCPPort(t)
	p := &managedProcess{}
	p.cmdFactory = func() *exec.Cmd {
		//nolint:gosec // os.Args[0] is the trusted test binary path
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		cmd.Env = []string{
			"GO_TEST_FAKE_OPENCODE=1",
			"FAKE_PORT=" + strconv.Itoa(port),
		}
		return cmd
	}
	p.healthCheckURL = ""
	p.start()
	t.Cleanup(p.stop)
	requireFakeReachable(t, port, 2*time.Second)
	return p
}

// TestCredentialReloadGate_SuppressesSameRev pins the storm-kill case:
// a credential_reload request arriving when the mux is serving exactly
// the revision the current child spawned with is answered WITHOUT
// touching the child. Regression for the 2026-10-01 incident (6
// same-rev pushes → 6 restarts in 11s → in-flight turn killed →
// opencode wedged aborting every turn until a manual pod bounce).
func TestCredentialReloadGate_SuppressesSameRev(t *testing.T) {
	withTestLogger(t)
	p := newGateTestProcess(t)
	puller := &fakeCredentialPuller{res: spawnEnvResponse{
		Env: map[string]string{"K": "V"},
		Rev: "12:abc:deadbeef",
	}}
	a := &managedProcAdapter{p: p, puller: puller, pullCtx: context.Background()}
	a.pullMu.Lock()
	a.servedEnvRevAnchor = "12:abc" // child spawned with seq 12, hash abc
	a.pullMu.Unlock()

	_, _, before, _ := a.State()
	suppressedBefore := testutil.ToFloat64(restartsSuppressedFor(t, "credential_reload"))

	restarted, inProgress := a.Restart("credential_reload", 1)

	require.False(t, restarted, "same-rev credential_reload must be suppressed")
	require.False(t, inProgress)
	_, _, after, _ := a.State()
	require.Equal(t, before, after, "the child must not have been restarted")
	require.Greater(t, testutil.ToFloat64(restartsSuppressedFor(t, "credential_reload")), suppressedBefore,
		"the suppression must be counted — it is the observable half of the gate")
}

// restartsSuppressedFor fetches the supervisor-side suppressed counter
// with the test's workspace-id label (RecordRestartSuppressed falls back
// to "unknown" when the env is unset, matching RecordRestart).
func restartsSuppressedFor(t *testing.T, reason string) prometheus.Counter {
	t.Helper()
	return pkgOpsMetrics.restartSuppressedCounter(metricWorkspaceID(), reason)
}

// TestSocketRestart_SuppressionRoundTrip drives the storm scenario
// end-to-end at the SOCKET seam: a real controlSocketServer over a real
// adapter + child, whose puller serves the spawned rev — the socket's
// restart response must report {restarted:false, in_progress:false} and
// the child must survive. This is the contract socketReloadProc's
// outcome-truthful recording (spawn_env_consumer.go) keys off.
func TestSocketRestart_SuppressionRoundTrip(t *testing.T) {
	withTestLogger(t)
	p := newGateTestProcess(t)
	puller := &fakeCredentialPuller{res: spawnEnvResponse{Rev: "12:abc:deadbeef"}}
	a := &managedProcAdapter{p: p, puller: puller, pullCtx: context.Background()}
	a.pullMu.Lock()
	a.servedEnvRevAnchor = "12:abc"
	a.pullMu.Unlock()

	addr := "127.0.0.1:" + strconv.Itoa(freeTCPPort(t))
	srv, err := newSupervisorControlServer(addr, a, nil)
	require.NoError(t, err)
	go srv.serve()
	defer srv.close()

	cc := newControlClient(addr)
	_, _, pidBefore, _ := a.State()

	res, err := cc.Restart(context.Background(), "credential_reload", 1)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Restarted, "the gate must answer through the socket")
	require.False(t, res.InProgress)

	_, _, pidAfter, _ := a.State()
	require.Equal(t, pidBefore, pidAfter, "the child pid must be untouched")
}

// TestRecordSocketReloadOutcome pins the sidecar-side outcome recording
// — the registry the PodMonitor actually scrapes (the supervisor serves
// no HTTP; its own counters are unreachable).
func TestRecordSocketReloadOutcome(t *testing.T) {
	t.Run("restarted counts a restart", func(t *testing.T) {
		before := testutil.ToFloat64(restartsTotalFor(t, "credential_reload"))
		recordSocketReloadOutcome(&controlRestartResult{Restarted: true}, nil)
		require.Greater(t, testutil.ToFloat64(restartsTotalFor(t, "credential_reload")), before)
	})
	t.Run("suppressed counts a suppression", func(t *testing.T) {
		before := testutil.ToFloat64(restartsSuppressedFor(t, "credential_reload"))
		recordSocketReloadOutcome(&controlRestartResult{}, nil)
		require.Greater(t, testutil.ToFloat64(restartsSuppressedFor(t, "credential_reload")), before)
	})
	t.Run("in-progress counts nothing", func(t *testing.T) {
		r := restartsTotalFor(t, "credential_reload")
		s := restartsSuppressedFor(t, "credential_reload")
		beforeR, beforeS := testutil.ToFloat64(r), testutil.ToFloat64(s)
		recordSocketReloadOutcome(&controlRestartResult{InProgress: true}, nil)
		require.Equal(t, beforeR, testutil.ToFloat64(r))
		require.Equal(t, beforeS, testutil.ToFloat64(s))
	})
	t.Run("transport error counts nothing", func(t *testing.T) {
		r := restartsTotalFor(t, "credential_reload")
		before := testutil.ToFloat64(r)
		recordSocketReloadOutcome(nil, errors.New("socket down"))
		require.Equal(t, before, testutil.ToFloat64(r))
	})
}

// restartsTotalFor fetches the restart counter with the test's label.
func restartsTotalFor(t *testing.T, reason string) prometheus.Counter {
	t.Helper()
	return pkgOpsMetrics.restartCounter(metricWorkspaceID(), reason)
}

// metricWorkspaceID mirrors the Record* methods' empty→unknown
// normalization so test reads and production writes share a label.
func metricWorkspaceID() string {
	if id := workspaceIDFromEnv(); id != "" {
		return id
	}
	return "unknown"
}

// TestCredentialReloadGate_FailsOpen pins every doubtful case: a NEW
// served rev, a pull error, a degrade latch, and an unanchorable rev
// all proceed to a real restart — the gate only ever suppresses the
// provably-redundant case.
func TestCredentialReloadGate_FailsOpen(t *testing.T) {
	withTestLogger(t)

	cases := []struct {
		name     string
		puller   *fakeCredentialPuller
		anchor   string
		degraded string
	}{
		{"new served rev restarts", &fakeCredentialPuller{res: spawnEnvResponse{Rev: "13:abc:ffff"}}, "12:abc", ""},
		{"pull error restarts", &fakeCredentialPuller{err: errors.New("mux down")}, "12:abc", ""},
		{"degraded spawn restarts", &fakeCredentialPuller{res: spawnEnvResponse{Rev: "12:abc:deadbeef"}}, "12:abc", "spawn_pull_timeout"},
		{"unanchorable rev restarts", &fakeCredentialPuller{res: spawnEnvResponse{Rev: "bare-content-hash"}}, "", ""},
		{"no anchor yet restarts", &fakeCredentialPuller{res: spawnEnvResponse{Rev: "12:abc:deadbeef"}}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newGateTestProcess(t)
			a := &managedProcAdapter{p: p, puller: tc.puller, pullCtx: context.Background()}
			a.pullMu.Lock()
			a.servedEnvRevAnchor = tc.anchor
			a.degradedReason = tc.degraded
			a.pullMu.Unlock()

			restarted, _ := a.Restart("credential_reload", 1)
			require.True(t, restarted, "doubtful cases must fail open and restart")
		})
	}
}

// TestCredentialReloadGate_OtherReasonsUngated pins that only
// credential_reload is gated: manual and health_watchdog restarts fire
// unconditionally even when the rev matches.
func TestCredentialReloadGate_OtherReasonsUngated(t *testing.T) {
	withTestLogger(t)
	p := newGateTestProcess(t)
	puller := &fakeCredentialPuller{res: spawnEnvResponse{Rev: "12:abc:deadbeef"}}
	a := &managedProcAdapter{p: p, puller: puller, pullCtx: context.Background()}
	a.pullMu.Lock()
	a.servedEnvRevAnchor = "12:abc"
	a.pullMu.Unlock()

	restarted, _ := a.Restart("manual", 1)
	require.True(t, restarted, "non-credential reasons must never be gated")
}

// TestCredentialReloadGate_NilPullerRestarts pins the nil-seam case
// (adapters constructed without a puller, e.g. rev_anchor tests): the
// gate is inert and the restart proceeds — today's behavior.
func TestCredentialReloadGate_NilPullerRestarts(t *testing.T) {
	withTestLogger(t)
	p := newGateTestProcess(t)
	a := &managedProcAdapter{p: p}

	restarted, _ := a.Restart("credential_reload", 1)
	require.True(t, restarted)
}

// compile-time seam assertion: the production puller satisfies the gate.
var _ credentialPuller = (*spawnEnvPuller)(nil)
