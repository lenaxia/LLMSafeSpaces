// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_version_visibility_test.go — RC-A (2026-10-30 incident)
// test-pins: the running agentd's build identity must be visible on
// the CRD/API status, and an agentd older than the fleet release must
// be flagged (condition + event + gauge + episode counter) — the
// silent 15-hour skew the incident produced can no longer happen
// unnoticed.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.44.7", "0.44.11", -1, true}, // the incident: 7 < 11 numerically, not lexically
		{"0.44.11", "0.44.7", 1, true},
		{"0.44.11", "0.44.11", 0, true},
		{"v0.44.9", "0.44.9", 0, true},          // v-prefix tolerated
		{"0.44.7+g6ce1859f", "0.44.7", 0, true}, // build metadata ignored
		{"0.45.0", "0.44.11", 1, true},
		{"1.0", "1.0.0", 0, true}, // missing segments pad as zero
		{"0.44.9", "0.44.10", -1, true},
		{"unknown", "0.44.11", 0, false}, // unstamped build: incomparable
		{"0.44.11", "unknown", 0, false},
		{"dev", "1.2.3", 0, false},
		{"0.44.x", "0.44.1", 0, false},
		{"", "0.44.11", 0, false},
	}
	for _, tc := range cases {
		got, ok := compareVersions(tc.a, tc.b)
		assert.Equal(t, tc.ok, ok, "comparability of %q vs %q", tc.a, tc.b)
		if tc.ok {
			assert.Equal(t, tc.want, got, "compareVersions(%q, %q)", tc.a, tc.b)
		}
	}
}

// versionTestReconciler builds a reconciler with a FakeRecorder and a
// FleetVersion, mirroring the production wiring (controller.go stamps
// pkg/version.Version; tests inject explicitly). Extra runtime objects
// (e.g. the pod) join the workspace in the fake client.
func versionTestReconciler(t *testing.T, fleetVersion string, ws *v1.Workspace, objs ...runtime.Object) (*WorkspaceReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	all := append([]runtime.Object{ws}, objs...)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(all...).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	rec := record.NewFakeRecorder(16)
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme, APIReader: fc, Recorder: rec}
	r.FleetVersion = fleetVersion
	return r, rec
}

func staleCondition(t *testing.T, ws *v1.Workspace) *v1.WorkspaceCondition {
	t.Helper()
	return findCondition(ws, v1.WorkspaceConditionAgentdVersionStale)
}

func TestMirrorAgentdVersion_MirrorsIdentityAndFlagsStale(t *testing.T) {
	ws := makeWorkspace("ws-rca-stale", "default", v1.WorkspacePhaseActive)
	r, rec := versionTestReconciler(t, "0.44.11", ws)

	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{
		Healthy: true, Version: "0.44.7", CommitSHA: "6ce1859f", BuildTime: "2026-10-29T02:08:00Z",
	})

	// Visibility: the incident's manual `curl localhost:4098/v1/healthz`
	// fields now ride the CRD status (and through it the API read).
	assert.Equal(t, "0.44.7", ws.Status.AgentdVersion)
	assert.Equal(t, "6ce1859f", ws.Status.AgentdCommit)
	assert.Equal(t, "2026-10-29T02:08:00Z", ws.Status.AgentdBuildTime)

	// Skew flag: condition True, gauge 1, event fired.
	cond := staleCondition(t, ws)
	require.NotNil(t, cond, "AgentdVersionStale condition must be set")
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, v1.ReasonAgentdVersionStale, cond.Reason)
	assert.Contains(t, cond.Message, "0.44.7")
	assert.Contains(t, cond.Message, "0.44.11")

	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-stale")),
		"skew gauge must read 1 while stale")
	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-stale")),
		"one episode tick per detected pair")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "AgentdVersionStale")
		assert.Contains(t, ev, "0.44.7")
	default:
		t.Fatal("expected an AgentdVersionStale warning event")
	}
}

func TestMirrorAgentdVersion_CurrentNoFlag(t *testing.T) {
	ws := makeWorkspace("ws-rca-current", "default", v1.WorkspacePhaseActive)
	r, rec := versionTestReconciler(t, "0.44.11", ws)

	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{
		Healthy: true, Version: "0.44.11", CommitSHA: "abc12345", BuildTime: "2026-10-30T02:23:00Z",
	})

	cond := staleCondition(t, ws)
	require.NotNil(t, cond)
	assert.Equal(t, "False", cond.Status)
	assert.Equal(t, v1.ReasonAgentdVersionCurrent, cond.Reason)
	assert.Equal(t, float64(0), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-current")))
	select {
	case ev := <-rec.Events:
		t.Fatalf("current agentd must not warn, got event: %s", ev)
	default:
	}
}

func TestMirrorAgentdVersion_AheadOfFleetIsCurrent(t *testing.T) {
	// A Renovate agentd digest bump can briefly run agentd NEWER than
	// the controller's release — ahead is not the incident's skew class.
	ws := makeWorkspace("ws-rca-ahead", "default", v1.WorkspacePhaseActive)
	r, _ := versionTestReconciler(t, "0.44.11", ws)

	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{
		Healthy: true, Version: "0.45.0", CommitSHA: "feed0001",
	})

	cond := staleCondition(t, ws)
	require.NotNil(t, cond)
	assert.Equal(t, "False", cond.Status)
	assert.Equal(t, v1.ReasonAgentdVersionCurrent, cond.Reason)
}

func TestMirrorAgentdVersion_IncomparableReportsUnknownAndLatchesGauge(t *testing.T) {
	ws := makeWorkspace("ws-rca-unknown", "default", v1.WorkspacePhaseActive)
	r, _ := versionTestReconciler(t, "0.44.11", ws)

	// Stale first, so the gauge holds a comparable verdict.
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	require.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-unknown")))

	// Then an unstamped build answers: the condition reports Unknown
	// honestly and the gauge does NOT flap green on a weird response.
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "unknown"})
	cond := staleCondition(t, ws)
	require.NotNil(t, cond)
	assert.Equal(t, "False", cond.Status)
	assert.Equal(t, v1.ReasonAgentdVersionUnknown, cond.Reason)
	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-unknown")),
		"last comparable verdict stays latched")

	// A dev controller (fleet version unknown) also disables the
	// comparison rather than guessing.
	ws2 := makeWorkspace("ws-rca-devctrl", "default", v1.WorkspacePhaseActive)
	r2, _ := versionTestReconciler(t, "unknown", ws2)
	r2.mirrorAgentdVersion(context.Background(), ws2, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	cond2 := staleCondition(t, ws2)
	require.NotNil(t, cond2)
	assert.Equal(t, v1.ReasonAgentdVersionUnknown, cond2.Reason)
}

func TestMirrorAgentdVersion_EventExactlyOncePerPair(t *testing.T) {
	ws := makeWorkspace("ws-rca-once", "default", v1.WorkspacePhaseActive)
	r, rec := versionTestReconciler(t, "0.44.11", ws)

	// Two scrapes of the same skew pair: one event, one episode tick.
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-once")))
	select {
	case <-rec.Events:
	default:
		t.Fatal("first observation must warn")
	}
	select {
	case ev := <-rec.Events:
		t.Fatalf("same (agentd, fleet) pair must not re-warn, got: %s", ev)
	default:
	}

	// A new stale pair (pod refreshed but STILL behind a newer fleet):
	// warns again — each distinct skew episode is its own signal.
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.9", CommitSHA: "9fd0e121"})
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "0.44.9")
	default:
		t.Fatal("a new skew pair must warn again")
	}
	assert.Equal(t, float64(2), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-once")))
}

// setupHealthzTest stands an httptest server answering /v1/healthz with
// the given HealthzResponse and points the reconciler's health poll at
// it (the setupHealthTest pattern, healthz-shaped).
func setupHealthzTest(t *testing.T, resp agentd.HealthzResponse) (*WorkspaceReconciler, *v1.Workspace, *record.FakeRecorder) {
	t.Helper()

	origAdminPort := agentdAdminPort
	agentdAdminPort = 0
	t.Cleanup(func() { agentdAdminPort = origAdminPort })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(resp)
		})},
	}
	server.Start()
	t.Cleanup(server.Close)

	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	agentdAdminPort, _ = strconv.Atoi(portStr)

	ws := makeWorkspace("ws-rca-e2e", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"

	r, rec := versionTestReconciler(t, "0.44.11", ws)

	origInterval := healthCheckInterval
	healthCheckInterval = 0
	t.Cleanup(func() { healthCheckInterval = origInterval })

	return r, ws, rec
}

// TestCheckAgentHealth_MirrorsAgentdVersionAndSkew pins the WIRING:
// the production liveness poll decodes healthz and routes it through
// mirrorAgentdVersion — identity on status, skew flagged.
func TestCheckAgentHealth_MirrorsAgentdVersionAndSkew(t *testing.T) {
	r, ws, rec := setupHealthzTest(t, agentd.HealthzResponse{
		Healthy: true, Version: "0.44.7", CommitSHA: "6ce1859f", BuildTime: "2026-10-29T02:08:00Z",
	})

	r.checkAgentHealth(context.Background(), ws)

	assert.Equal(t, "0.44.7", ws.Status.AgentdVersion)
	assert.Equal(t, "6ce1859f", ws.Status.AgentdCommit)
	cond := staleCondition(t, ws)
	require.NotNil(t, cond, "the health poll must evaluate agentd skew")
	assert.Equal(t, "True", cond.Status)
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "AgentdVersionStale")
	default:
		t.Fatal("the health poll must surface the skew warning event")
	}
}

// TestCheckAgentHealth_UnreachableClearsAgentdVersion pins the
// evidence rule: a dead pod's build identity is unknown, not carried
// forward from a previous pod.
func TestCheckAgentHealth_UnreachableClearsAgentdVersion(t *testing.T) {
	ws := makeWorkspace("ws-rca-dead", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.AgentdVersion = "0.44.7"
	ws.Status.AgentdCommit = "6ce1859f"
	ws.Status.AgentdBuildTime = "2026-10-29T02:08:00Z"

	origAdminPort := agentdAdminPort
	origInterval := healthCheckInterval
	agentdAdminPort = 1 // unreachable
	healthCheckInterval = 0
	t.Cleanup(func() {
		agentdAdminPort = origAdminPort
		healthCheckInterval = origInterval
	})

	r, _ := versionTestReconciler(t, "0.44.11", ws)
	r.checkAgentHealth(context.Background(), ws)

	assert.Empty(t, ws.Status.AgentdVersion)
	assert.Empty(t, ws.Status.AgentdCommit)
	assert.Empty(t, ws.Status.AgentdBuildTime)
}

// TestMirrorAgentdVersion_AncientBuildIsUnknownNotCurrent (review r1):
// a pre-2026-08-15 agentd reports OPENCODE's version in healthz
// `version` with NEITHER commit_sha NOR build_time (both landed with
// the identity work; every stamped build reports non-empty values).
// Comparing "1.18.10" against "0.44.11" used to classify the ancient
// pod Current — silently wrong for exactly the never-rolled population
// this feature targets. Now: Unknown verdict, version NOT mirrored.
func TestMirrorAgentdVersion_AncientBuildIsUnknownNotCurrent(t *testing.T) {
	ws := makeWorkspace("ws-rca-ancient", "default", v1.WorkspacePhaseActive)
	r, rec := versionTestReconciler(t, "0.44.11", ws)

	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{
		Healthy: true, Version: "1.18.10", // opencode's version, not agentd's
	})

	cond := staleCondition(t, ws)
	require.NotNil(t, cond)
	assert.Equal(t, "False", cond.Status)
	assert.Equal(t, v1.ReasonAgentdVersionUnknown, cond.Reason)
	assert.Contains(t, cond.Message, "predates build-identity reporting")
	assert.Empty(t, ws.Status.AgentdVersion,
		"opencode's version must NOT be mirrored as the agentd's")
	select {
	case ev := <-rec.Events:
		t.Fatalf("an ancient build must not be a stale warning, got: %s", ev)
	default:
	}
}

// TestMirrorAgentdVersion_EpisodeSignalsOnlyAfterPersist (review r1):
// the log line, counter tick, and event fire only AFTER the exactly-once
// annotation stamp persists — a persist failure must not re-tick the
// counter every 15s scrape against its "once per episode" contract.
// The fake client here does NOT hold the workspace, so the persist Get
// fails deterministically.
func TestMirrorAgentdVersion_EpisodeSignalsOnlyAfterPersist(t *testing.T) {
	ws := makeWorkspace("ws-rca-nostore", "default", v1.WorkspacePhaseActive)
	scheme := testScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Workspace{}).
		Build() // deliberately NO workspace object → persist Get fails
	rec := record.NewFakeRecorder(16)
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme, APIReader: fc, Recorder: rec}
	r.FleetVersion = "0.44.11"

	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})

	// The verdict surfaces immediately (condition + gauge)…
	cond := staleCondition(t, ws)
	require.NotNil(t, cond)
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-nostore")))
	// …but the episode signals do NOT fire while the stamp cannot persist.
	assert.Equal(t, float64(0), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-nostore")),
		"no episode tick until the exactly-once stamp persists")
	select {
	case ev := <-rec.Events:
		t.Fatalf("no warning event before the stamp persists, got: %s", ev)
	default:
	}
}

// TestMirrorAgentdVersion_EventExactlyOnceAcrossRestart (review r1):
// the annotation — not the in-memory fast path — is the restart-
// surviving exactly-once mechanism. A "restarted controller" is a NEW
// reconciler over a fresh object carrying the persisted annotation.
func TestMirrorAgentdVersion_EventExactlyOnceAcrossRestart(t *testing.T) {
	ws := makeWorkspace("ws-rca-restart", "default", v1.WorkspacePhaseActive)
	r1, rec1 := versionTestReconciler(t, "0.44.11", ws)
	r1.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})
	require.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-restart")))
	select {
	case <-rec1.Events:
	default:
		t.Fatal("first observation must warn")
	}

	// Persisted object (annotation survives; fake-client quirk resets
	// status on plain Update — irrelevant, the mirror re-derives it).
	var stored v1.Workspace
	require.NoError(t, r1.Get(context.Background(),
		types.NamespacedName{Name: "ws-rca-restart", Namespace: "default"}, &stored))
	require.Equal(t, "0.44.7|0.44.11",
		stored.Annotations[agentdVersionStaleReportedAnnotation],
		"the exactly-once stamp must persist on metadata")

	// Controller restart: fresh reconciler, fresh object read.
	r2, rec2 := versionTestReconciler(t, "0.44.11", &stored)
	var fresh v1.Workspace
	require.NoError(t, r2.Get(context.Background(),
		types.NamespacedName{Name: "ws-rca-restart", Namespace: "default"}, &fresh))
	r2.mirrorAgentdVersion(context.Background(), &fresh, &agentd.HealthzResponse{Version: "0.44.7", CommitSHA: "6ce1859f"})

	assert.Equal(t, float64(1), testutil.ToFloat64(
		metrics.WorkspaceAgentdVersionSkewDetectedTotal.WithLabelValues("ws-rca-restart")),
		"restart + same pair: no second episode tick")
	select {
	case ev := <-rec2.Events:
		t.Fatalf("persisted stamp must suppress the duplicate event, got: %s", ev)
	default:
	}
	// The verdict still holds after the restart.
	cond := staleCondition(t, &fresh)
	require.NotNil(t, cond)
	assert.Equal(t, "True", cond.Status)
}

// TestCheckAgentHealth_UndecodableClearsAgentdVersion pins the SECOND
// clear site (review r1: only unreachable was pinned): a response that
// decodes to garbage clears the identity fields — evidence from a pod
// whose healthz is broken must not survive as build identity.
func TestCheckAgentHealth_UndecodableClearsAgentdVersion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not-json{"))
		})},
	}
	server.Start()
	t.Cleanup(server.Close)
	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ws := makeWorkspace("ws-rca-garbage", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.AgentdVersion = "0.44.7"
	ws.Status.AgentdCommit = "6ce1859f"
	ws.Status.AgentdBuildTime = "2026-10-29T02:08:00Z"

	origAdminPort := agentdAdminPort
	origInterval := healthCheckInterval
	agentdAdminPort = port
	healthCheckInterval = 0
	t.Cleanup(func() {
		agentdAdminPort = origAdminPort
		healthCheckInterval = origInterval
	})

	r, _ := versionTestReconciler(t, "0.44.11", ws)
	r.checkAgentHealth(context.Background(), ws)

	assert.Empty(t, ws.Status.AgentdVersion)
	assert.Empty(t, ws.Status.AgentdCommit)
	assert.Empty(t, ws.Status.AgentdBuildTime)
}

// TestCheckAgentHealth_UnhealthyStillMirrorsAgentdVersion pins the
// "ANY decoded response" evidence rule (review r1: only Healthy=true
// was tested): an agentd that answers UNHEALTHY still carries its own
// build identity — the mirror and the skew evaluation must run.
func TestCheckAgentHealth_UnhealthyStillMirrorsAgentdVersion(t *testing.T) {
	r, ws, rec := setupHealthzTest(t, agentd.HealthzResponse{
		Healthy: false, Version: "0.44.7", CommitSHA: "6ce1859f", BuildTime: "2026-10-29T02:08:00Z",
	})

	r.checkAgentHealth(context.Background(), ws)

	assert.Equal(t, "0.44.7", ws.Status.AgentdVersion,
		"identity is a property of the binary that answered, not of its health")
	cond := staleCondition(t, ws)
	require.NotNil(t, cond, "skew evaluation runs on any decoded response")
	assert.Equal(t, "True", cond.Status)
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "AgentdVersionStale")
	default:
		t.Fatal("the skew warning must fire even on an unhealthy answer")
	}
}

// TestCheckAgentHealth_RestartClearsSkewGauge (review r1, robustness 1):
// the skew series dies with the pod — the health-restart path
// (threshold of unreachable failures) deletes it so a workspace deleted
// or restarted while stale cannot hold the alert open forever.
func TestCheckAgentHealth_RestartClearsSkewGauge(t *testing.T) {
	ws := makeWorkspace("ws-rca-gauge", "default", v1.WorkspacePhaseActive)
	ws.UID = "ws-rca-gauge-uid"
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = 2 // one more push to threshold
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")

	origAdminPort := agentdAdminPort
	origInterval := healthCheckInterval
	agentdAdminPort = 1 // unreachable
	healthCheckInterval = 0
	t.Cleanup(func() {
		agentdAdminPort = origAdminPort
		healthCheckInterval = origInterval
	})

	r, _ := versionTestReconciler(t, "0.44.11", ws, pod)
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues(ws.Name).Set(1)
	require.Equal(t, v1.WorkspacePhaseActive, ws.Status.Phase)

	r.checkAgentHealth(context.Background(), ws)

	require.Equal(t, v1.WorkspacePhaseCreating, ws.Status.Phase,
		"the threshold path must have restarted the pod")
	assert.False(t, metrics.WorkspaceAgentdVersionSkew.DeleteLabelValues(ws.Name),
		"the restart path must have DELETED the series (second delete finds nothing)")
}

// TestHandleTerminating_ClearsSkewGauge (review r1, robustness 1): a
// workspace DELETED while stale must not page forever — the terminating
// path deletes the gauge series with the pod.
func TestHandleTerminating_ClearsSkewGauge(t *testing.T) {
	ws := wsForTerminate("ws-rca-term")
	pwSecret := makePasswordSecret("ws-rca-term", "default")
	r := reconcilerFor(t, ws, pwSecret)
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-term").Set(1)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rca-term", "default"))
	require.NoError(t, err)

	assert.False(t, metrics.WorkspaceAgentdVersionSkew.DeleteLabelValues("ws-rca-term"),
		"termination must have DELETED the skew series — the runbook's 'gauge drops to 0' convergence has to be achievable")
}

// TestHandleSuspending_ClearsSkewGauge (review r1, robustness 1): a
// suspended workspace has no pod and rebuilds with the CURRENT digest
// at resume — its stale verdict is meaningless and the series dies.
func TestHandleSuspending_ClearsSkewGauge(t *testing.T) {
	ws := makeWorkspace("ws-rca-susp", "default", v1.WorkspacePhaseSuspending)
	pod := makeRunningPod(podName("ws-rca-susp", string(ws.UID)), "default", "10.0.0.99")
	ws.Status.PodName = pod.Name
	ws.Status.PodIP = "10.0.0.99"

	r := reconcilerFor(t, ws, pod)
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-susp").Set(1)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rca-susp", "default"))
	require.NoError(t, err)

	assert.False(t, metrics.WorkspaceAgentdVersionSkew.DeleteLabelValues("ws-rca-susp"),
		"suspension must have DELETED the skew series")
}

// TestSuspendFromPreActive_ClearsSkewGauge (review r2, the blocker):
// the SECOND suspend path — a pre-Active workspace parked Suspended
// directly — must also delete the skew series. The reachable shape: a
// workspace that was Active (gauge set by its scraped pod), recycled
// to Creating, then suspended from Creating before reaching Active.
func TestSuspendFromPreActive_ClearsSkewGauge(t *testing.T) {
	ws := makeWorkspace("ws-rca-pre", "default", v1.WorkspacePhaseCreating)
	suspendTrue := true
	ws.Spec.Suspend = &suspendTrue
	pod := makeRunningPod(podName("ws-rca-pre", string(ws.UID)), "default", "10.0.0.99")

	r := reconcilerFor(t, ws, pod)
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-pre").Set(1)

	_, err := r.Reconcile(context.Background(), reqFor("ws-rca-pre", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-rca-pre", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase,
		"sanity: the pre-Active suspend path must have run")
	assert.False(t, metrics.WorkspaceAgentdVersionSkew.DeleteLabelValues("ws-rca-pre"),
		"suspendFromPreActive must have DELETED the skew series — a workspace parked Suspended must not page on a previous pod's verdict")
}

// TestEnterRecovery_ClearsSkewGauge (review r2, the same-class funnel):
// every failure-class Active exit funnels through enterRecovery — the
// failed pod's verdict dies there, so a workspace cycling recovery
// never pages on a previous pod's stale verdict.
func TestEnterRecovery_ClearsSkewGauge(t *testing.T) {
	ws := makeWorkspace("ws-rca-recov", "default", v1.WorkspacePhaseActive)
	r, _ := versionTestReconciler(t, "0.44.11", ws)
	metrics.WorkspaceAgentdVersionSkew.WithLabelValues("ws-rca-recov").Set(1)

	_, err := r.enterRecovery(context.Background(), ws, FailureClassProcess)
	require.NoError(t, err)

	assert.Equal(t, v1.WorkspacePhaseCreating, ws.Status.Phase)
	assert.False(t, metrics.WorkspaceAgentdVersionSkew.DeleteLabelValues("ws-rca-recov"),
		"enterRecovery must have DELETED the skew series")
}
