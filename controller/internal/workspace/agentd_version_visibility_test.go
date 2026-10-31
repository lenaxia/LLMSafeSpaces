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
// pkg/version.Version; tests inject explicitly).
func versionTestReconciler(t *testing.T, fleetVersion string, ws *v1.Workspace) (*WorkspaceReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws).
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
		Healthy: true, Version: "0.45.0",
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
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7"})
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
	r2.mirrorAgentdVersion(context.Background(), ws2, &agentd.HealthzResponse{Version: "0.44.7"})
	cond2 := staleCondition(t, ws2)
	require.NotNil(t, cond2)
	assert.Equal(t, v1.ReasonAgentdVersionUnknown, cond2.Reason)
}

func TestMirrorAgentdVersion_EventExactlyOncePerPair(t *testing.T) {
	ws := makeWorkspace("ws-rca-once", "default", v1.WorkspacePhaseActive)
	r, rec := versionTestReconciler(t, "0.44.11", ws)

	// Two scrapes of the same skew pair: one event, one episode tick.
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7"})
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.7"})
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
	r.mirrorAgentdVersion(context.Background(), ws, &agentd.HealthzResponse{Version: "0.44.9"})
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
