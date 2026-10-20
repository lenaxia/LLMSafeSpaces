// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// restart_marker_coverage_test.go — #1801: every supervisor restart
// class must write the restart-reason marker. This file pins the
// write per class and hosts the relay-injector family (the 06:19
// incident class — the one class that never wrote).
//
// Enumeration (decision sites in cmd/workspace-agentd) and the pin
// that guards each class's marker write:
//
//	A. managedProcess.supervise loop (both topologies)
//	  A.1 crash (unexpected non-OOM exit)  — TestRestartMarker_CrashRecoveryWritesMarker (below)
//	  A.2 oom (SIGKILL-classified exit)    — TestHandleOOMExit_WritesRestartReasonMarker (oom_detection_test.go)
//	  A.3 restartRequested branch — the loop never writes; each caller must:
//	    3a watchdog hung fire                — TestWatchdogFire_WritesRestartReasonMarker (below)
//	    3b watchdog unknown-episode fire     — TestWatchdog_UnknownEpisode_BoundedSoftKill (watchdog_unknown_episode_test.go)
//	    3c secrets apply, single-container   — TestRestartMarker_SecretsApplySingleContainer (below)
//	    3c' secrets apply, sidecar socket    — spawn_env_consumer_test.go reload marker pin
//	    3d  XDG agent-config watcher         — TestWatchAgentConfigForChanges_RestartsOnChange (extended, xdg_config_layer_test.go)
//	    3e  relay injector / re-arm kill     — TestRelayKillFunc_Writes* family (below) — #1801 fix
//
// Suppressed watchdog verdicts (starved/flat/respawn/unknown below the
// bound) restart nothing and correctly write nothing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markerAt decodes the full restart-reason marker at path.
func markerAt(t *testing.T, path string) restartReason {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "restart-reason marker must be readable")
	var m restartReason
	require.NoError(t, json.Unmarshal(data, &m), "marker JSON must decode: %s", string(data))
	return m
}

// -----------------------------------------------------------------------
// Class 3e — relay injector / re-arm kill (the #1801 / 06:19 class)
// -----------------------------------------------------------------------

// TestRelayKillFunc_WritesMarker_ImmediateLeg pins the #1801 fix's core:
// an idle-session relay kill writes reason "relay_injector" to the
// marker BEFORE the session-aware restart fires. Red mode: delete the
// write in relayKillFunc — the marker assertion fails (the 06:19 shape:
// restart happened, marker stale).
func TestRelayKillFunc_WritesMarker_ImmediateLeg(t *testing.T) {
	withTestLogger(t)
	markerPath := filepath.Join(t.TempDir(), "restart-reason.json")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", markerPath)
	t.Setenv("WORKSPACE_ID", "ws-1801-relay-immediate")

	proc := &mockManagedProcess{}
	tracker := newSessionStatusTracker()
	kill := relayKillFunc(context.Background(), nil, proc, tracker, nil, nil)
	kill()

	assert.Equal(t, 1, proc.restartCount(), "idle sessions — restart must fire immediately")
	m := markerAt(t, markerPath)
	assert.Equal(t, RestartReasonRelayInjector, m.Reason, "relay restart must write its class marker")
	assert.Empty(t, m.SecretNames, "relay restarts carry no secret names")
	_, err := time.Parse(time.RFC3339, m.Timestamp)
	assert.NoError(t, err, "marker timestamp must be RFC3339")
}

// TestRelayKillFunc_WritesMarker_AtDecisionTimeWhileDeferred pins the
// M3 decision-time semantics for the relay class: the marker is written
// when the restart is REQUESTED, not when the deferred restart later
// COMPLETES. With a busy session the restart is withheld, yet the
// marker must already exist — the accepted M3 trade-off documented at
// restart_reason.go (decision time is when the relay config change
// became relevant; a deferred restart that never fires leaves a
// stale-at-boot marker, mitigated by the 10-minute stale threshold).
func TestRelayKillFunc_WritesMarker_AtDecisionTimeWhileDeferred(t *testing.T) {
	withTestLogger(t)
	markerPath := filepath.Join(t.TempDir(), "restart-reason.json")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", markerPath)

	proc := &mockManagedProcess{}
	tracker := newSessionStatusTracker()
	tracker.set("ses_busy", "busy")
	// Cancelable ctx so the deferred goroutine releases the
	// deferredRestarts gauge at test end (isolation note in
	// TestRelayKillFunc_BusyTracker_DefersRestart).
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	kill := relayKillFunc(ctx, nil, proc, tracker, nil, nil)
	kill()

	assert.Equal(t, 0, proc.restartCount(), "busy session — restart must be deferred")
	m := markerAt(t, markerPath)
	assert.Equal(t, RestartReasonRelayInjector, m.Reason,
		"marker must land at DECISION time even while the restart is deferred")
}

// TestRelayKillFunc_MarkerFailureDoesNotBlockRestart mirrors the H2
// doctrine: the marker is observability, never a gate. A sabotaged
// marker path (parent is a regular file → MkdirAll fails) must not
// stop the restart, and the failure must be counted.
func TestRelayKillFunc_MarkerFailureDoesNotBlockRestart(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	blockingFile := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blockingFile, []byte("x"), 0o600))
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(blockingFile, "marker"))
	t.Setenv("WORKSPACE_ID", "ws-1801-relay-markerfail")

	before := testutil.ToFloat64(pkgOpsMetrics.markerWriteFailures.WithLabelValues("ws-1801-relay-markerfail", RestartReasonRelayInjector))

	proc := &mockManagedProcess{}
	kill := relayKillFunc(context.Background(), nil, proc, newSessionStatusTracker(), nil, nil)
	kill()

	assert.Equal(t, 1, proc.restartCount(), "restart must proceed despite marker-write failure")
	after := testutil.ToFloat64(pkgOpsMetrics.markerWriteFailures.WithLabelValues("ws-1801-relay-markerfail", RestartReasonRelayInjector))
	assert.Equal(t, before+1, after, "marker-write failure must be counted")
}

// TestRelayKillFunc_RecordsRestartMetric pins the ops-metric leg: the
// relay class appears in workspace_restarts_total{reason="relay_injector"}.
func TestRelayKillFunc_RecordsRestartMetric(t *testing.T) {
	withTestLogger(t)
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", filepath.Join(t.TempDir(), "restart-reason.json"))
	t.Setenv("WORKSPACE_ID", "ws-1801-relay-metric")

	before := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-1801-relay-metric", RestartReasonRelayInjector))

	proc := &mockManagedProcess{}
	kill := relayKillFunc(context.Background(), nil, proc, newSessionStatusTracker(), nil, nil)
	kill()

	after := testutil.ToFloat64(pkgOpsMetrics.restartsTotal.WithLabelValues("ws-1801-relay-metric", RestartReasonRelayInjector))
	assert.Equal(t, before+1, after,
		`workspace_restarts_total{reason="relay_injector"} must increment on a relay restart`)
}

// TestRestartReasonRelayInjector_Constant pins the shared string: the
// marker reason, the metric label, and the control-socket closed enum
// (control_socket.go) are one value.
func TestRestartReasonRelayInjector_Constant(t *testing.T) {
	assert.Equal(t, "relay_injector", RestartReasonRelayInjector)
	assert.True(t, restartReasons[RestartReasonRelayInjector],
		"the marker reason must be a member of the control-socket closed reason enum")
}

// -----------------------------------------------------------------------
// Class A.1 — crash recovery (managedProcess supervise loop)
// -----------------------------------------------------------------------

// TestRestartMarker_CrashRecoveryWritesMarker drives the REAL supervise
// loop with a real subprocess that exits unexpectedly (clean exit, no
// signal → exitCrash classification): the loop's crash path must write
// reason "crash" to the env-resolved marker path before backing off.
func TestRestartMarker_CrashRecoveryWritesMarker(t *testing.T) {
	withTestLogger(t)
	markerPath := filepath.Join(t.TempDir(), "restart-reason.json")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", markerPath)
	t.Setenv("WORKSPACE_ID", "ws-1801-crash")

	p := &managedProcess{}
	p.cmdFactory = func() *exec.Cmd {
		// A child that dies on its own: unexpected exit, not OOM
		// (no signal → exitCrash, not exitSigKill).
		return exec.Command("sleep", "0.05") //nolint:gosec // fixed argv test child
	}
	p.start()
	defer p.stop()

	require.Eventually(t, func() bool {
		_, ok := markerReasonAt(markerPath)
		return ok
	}, 10*time.Second, 50*time.Millisecond, "crash path must write the restart-reason marker")
	m := markerAt(t, markerPath)
	assert.Equal(t, "crash", m.Reason, "unexpected non-OOM exit must be attributed crash")
}

// -----------------------------------------------------------------------
// Class 3a — watchdog hung fire (corroborated-dead-listener path)
// -----------------------------------------------------------------------

// TestWatchdogFire_WritesRestartReasonMarker pins the FIRE-path marker
// write (healthz_cache.go), not just the writer function: a corroborated
// dead-listener hang must leave reason "health_watchdog" on disk.
func TestWatchdogFire_WritesRestartReasonMarker(t *testing.T) {
	withTestLogger(t)
	setWatchdogTiming(t, 60*time.Millisecond, 40*time.Millisecond, 3)
	markerPath := filepath.Join(t.TempDir(), "restart-reason.json")
	t.Setenv("LLMSAFESPACES_RESTART_MARKER_PATH", markerPath)

	var callCount atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true, "version": "v1.0"})
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}))
	defer mock.Close()

	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: readinessRefreshTimeout}}
	cache := newHealthzCache()
	fr := &fakeRestarter{}

	runRefreshLoop(t, mock.URL, client, cache, fr, nil)

	require.Eventually(t, func() bool {
		return fr.callCount() == 1
	}, 20*time.Second, 50*time.Millisecond, "watchdog must fire once on the hang")
	reason, ok := markerReasonAt(markerPath)
	require.True(t, ok, "watchdog fire must write the restart-reason marker")
	assert.Equal(t, RestartReasonHealthWatchdog, reason)
}

// -----------------------------------------------------------------------
// Class 3c — secrets apply, single-container leg
// -----------------------------------------------------------------------

// TestRestartMarker_SecretsApplySingleContainer pins the apply-handler
// marker write for the in-process (non-socket) topology: an env-secret
// batch restart records reason env_secrets_changed with the var name.
func TestRestartMarker_SecretsApplySingleContainer(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "secrets-env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	markerPath := filepath.Join(dir, "restart-reason.json")

	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")
	proc := &mockManagedProcess{}

	body := `[{"type":"env-secret","name":"x","metadata":{"var_name":"GH_TOKEN"},"plaintext":"v"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/apply-batch", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	applyBatchHandler(cfg, applySecretsDeps{
		OpencodePassword:        "test-pw",
		Proc:                    proc,
		Tracker:                 tracker,
		RestartReasonMarkerPath: markerPath,
	})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, 1, proc.restartCount(), "idle sessions — restart must fire")
	m := markerAt(t, markerPath)
	assert.Equal(t, "env_secrets_changed", m.Reason)
	assert.Equal(t, []string{"GH_TOKEN"}, m.SecretNames)
}
