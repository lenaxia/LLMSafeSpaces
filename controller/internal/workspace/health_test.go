// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestSetCondition_AppendsNew(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-cond", "default", v1.WorkspacePhaseActive)
	r.setCondition(ws, v1.WorkspaceConditionCredentialsAvailable, "True", v1.ReasonCredentialsValid, "")
	require.Len(t, ws.Status.Conditions, 1)
	assert.Equal(t, v1.WorkspaceConditionCredentialsAvailable, ws.Status.Conditions[0].Type)
	assert.Equal(t, "True", ws.Status.Conditions[0].Status)
	assert.Equal(t, v1.ReasonCredentialsValid, ws.Status.Conditions[0].Reason)
	assert.False(t, ws.Status.Conditions[0].LastTransitionTime.IsZero())
}

func TestSetCondition_UpdatesExisting(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-cond", "default", v1.WorkspacePhaseActive)
	r.setCondition(ws, v1.WorkspaceConditionCredentialsAvailable, "True", v1.ReasonCredentialsValid, "")
	firstTransition := ws.Status.Conditions[0].LastTransitionTime

	time.Sleep(10 * time.Millisecond)
	r.setCondition(ws, v1.WorkspaceConditionCredentialsAvailable, "False", v1.ReasonCredentialEmpty, "empty")
	require.Len(t, ws.Status.Conditions, 1)
	assert.Equal(t, "False", ws.Status.Conditions[0].Status)
	assert.Equal(t, v1.ReasonCredentialEmpty, ws.Status.Conditions[0].Reason)
	assert.True(t, ws.Status.Conditions[0].LastTransitionTime.After(firstTransition.Time))
}

func TestSetCondition_SameStatusAndReason_NoTransitionTimeUpdate(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-cond", "default", v1.WorkspacePhaseActive)
	r.setCondition(ws, v1.WorkspaceConditionCredentialsAvailable, "True", v1.ReasonCredentialsValid, "msg1")
	firstTransition := ws.Status.Conditions[0].LastTransitionTime

	time.Sleep(10 * time.Millisecond)
	r.setCondition(ws, v1.WorkspaceConditionCredentialsAvailable, "True", v1.ReasonCredentialsValid, "msg2")
	require.Len(t, ws.Status.Conditions, 1)
	assert.Equal(t, firstTransition, ws.Status.Conditions[0].LastTransitionTime)
	assert.Equal(t, "msg2", ws.Status.Conditions[0].Message)
}

func TestShouldRunHealthCheck_NilLastCheck(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-hc", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	assert.True(t, r.shouldRunHealthCheck(ws))
}

func TestShouldRunHealthCheck_WithinGracePeriod(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-hc", "default", v1.WorkspacePhaseActive)
	now := metav1.Now()
	ws.Status.StartTime = &now
	assert.False(t, r.shouldRunHealthCheck(ws))
}

func TestShouldRunHealthCheck_WithinInterval(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-hc", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	ws.Status.StartTime = &past
	recent := metav1.NewTime(time.Now().Add(-5 * time.Second))
	ws.Status.LastHealthCheckAt = &recent
	assert.False(t, r.shouldRunHealthCheck(ws))
}

func TestShouldRunHealthCheck_BackoffAfterFailures(t *testing.T) {
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-hc", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.ConsecutiveHealthFailures = 3

	withinBackoff := metav1.NewTime(time.Now().Add(-30 * time.Second))
	ws.Status.LastHealthCheckAt = &withinBackoff
	assert.False(t, r.shouldRunHealthCheck(ws), "should not run within backoff interval")

	afterBackoff := metav1.NewTime(time.Now().Add(-90 * time.Second))
	ws.Status.LastHealthCheckAt = &afterBackoff
	assert.True(t, r.shouldRunHealthCheck(ws), "should run after backoff interval")
}

func setupHealthTest(t *testing.T, statusResp agentd.StatuszResponse) (*WorkspaceReconciler, *v1.Workspace, *httptest.Server) {
	t.Helper()
	opencode.Register()

	origPort := agentdPort
	origAdminPort := agentdAdminPort
	agentdAdminPort = 0
	agentdPort = 0
	agentdAdminPort = 0
	t.Cleanup(func() {
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(statusResp)
		})},
	}
	server.Start()
	t.Cleanup(server.Close)

	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	agentdPort, _ = strconv.Atoi(portStr)
	agentdAdminPort, _ = strconv.Atoi(portStr)

	scheme := testScheme(t)
	ws := makeWorkspace("ws-health", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws).
		WithStatusSubresource(&v1.Workspace{}).
		Build()

	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	origInterval := healthCheckInterval
	healthCheckInterval = 0
	t.Cleanup(func() { healthCheckInterval = origInterval })

	return r, ws, server
}

func TestCheckAgentHealth_Healthy(t *testing.T) {
	r, ws, server := setupHealthTest(t, agentd.StatuszResponse{
		Healthy: true, Ready: true, Connected: []string{"opencode"},
		ProvidersConfigured: 1, SessionsActive: 2, AgentVersion: "1.2.27", UptimeSeconds: 3600,
	})
	defer server.Close()

	r.checkAgentHealth(context.Background(), ws)
	// Deep-status enrichment is what writes the healthy
	// "connected=... sessions=... version=..." message. Without this
	// call the AgentHealthy condition only reflects liveness
	// ("agentd alive, uptime=Ns").
	r.enrichAgentStatus(context.Background(), ws, 60*time.Second)

	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentHealthy {
			found = true
			assert.Equal(t, "True", c.Status)
			assert.Equal(t, v1.ReasonAgentHealthy, c.Reason)
			// Regression for issue #593: the healthy condition message
			// must include configured=N so the API's regex parser
			// (configuredRe in workspace_service.go) can surface the
			// real provider count to clients. Without this, every
			// healthy workspace reports providersConfigured=0.
			assert.Contains(t, c.Message, "configured=1",
				"healthy AgentHealthy message must include the configured provider count")
			assert.Contains(t, c.Message, "connected=[opencode]")
		}
	}
	assert.True(t, found)
	assert.Equal(t, int32(0), ws.Status.ConsecutiveHealthFailures)
	assert.NotNil(t, ws.Status.LastHealthCheckAt)
}

func TestCheckAgentHealth_Degraded(t *testing.T) {
	r, ws, server := setupHealthTest(t, agentd.StatuszResponse{
		Healthy: true, Ready: false, Connected: []string{},
		ProvidersConfigured: 1, AgentVersion: "1.2.27",
	})
	defer server.Close()

	// US-22.5/22.6: Liveness check passes (agentd alive), then deep-status
	// detects the degraded state (no providers connected).
	r.checkAgentHealth(context.Background(), ws)
	r.enrichAgentStatus(context.Background(), ws, 60*time.Second)

	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentHealthy {
			found = true
			assert.Equal(t, "False", c.Status)
			assert.Equal(t, v1.ReasonAgentDegraded, c.Reason)
		}
	}
	assert.True(t, found)
}

func TestCheckAgentHealth_Unhealthy(t *testing.T) {
	r, ws, server := setupHealthTest(t, agentd.StatuszResponse{
		Healthy: false, Ready: false, Connected: nil,
	})
	defer server.Close()

	r.checkAgentHealth(context.Background(), ws)

	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentHealthy {
			found = true
			assert.Equal(t, "False", c.Status)
			assert.Equal(t, v1.ReasonAgentUnhealthy, c.Reason)
		}
	}
	assert.True(t, found)
	assert.Equal(t, int32(1), ws.Status.ConsecutiveHealthFailures)
}

func TestCheckAgentHealth_ConnectionRefused(t *testing.T) {
	opencode.Register()
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-connref", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdAdminPort = 1
	agentdPort = 1
	agentdAdminPort = 1
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	r.checkAgentHealth(context.Background(), ws)

	found := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentHealthy {
			found = true
			assert.Equal(t, "Unknown", c.Status)
			assert.Equal(t, v1.ReasonHealthCheckFailed, c.Reason)
		}
	}
	assert.True(t, found)
	assert.Equal(t, int32(1), ws.Status.ConsecutiveHealthFailures)
}

// TestCheckAgentHealth_ConnectionRefused_RestartsAfterThreshold is the
// regression test for Bug 12 in worklog 0085: a stale pod IP (e.g. pod
// deleted out from under us) used to drive ConsecutiveHealthFailures to
// 36+ without ever triggering a recreate. After threshold the controller
// must transition the workspace back to Creating so the pod is rebuilt.
func TestCheckAgentHealth_ConnectionRefused_RestartsAfterThreshold(t *testing.T) {
	opencode.Register()
	scheme := testScheme(t)
	ws := makeWorkspace("ws-stuck", "default", v1.WorkspacePhaseActive)
	ws.UID = "ws-stuck-uid"
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = 2 // one more push to threshold

	pod := makeRunningPod(podName("ws-stuck", string(ws.UID)), "default", "127.0.0.1")

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = 1 // unreachable port
	agentdAdminPort = 1
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	r.checkAgentHealth(context.Background(), ws)

	// After threshold-trip the counter is reset to 0 so the
	// freshly-spawned pod is not immediately re-restarted on its
	// first connection-refused failure.
	assert.Equal(t, int32(0), ws.Status.ConsecutiveHealthFailures,
		"counter reset to 0 after restart so new pod starts clean")
	assert.Equal(t, v1.WorkspacePhaseCreating, ws.Status.Phase,
		"Bug 12: must transition to Creating once threshold reached, not loop forever")
	assert.Empty(t, ws.Status.PodIP)
	assert.Equal(t, int32(1), ws.Status.RestartCount)
}

func TestCheckAgentHealth_SuccessResetsFailures(t *testing.T) {
	r, ws, server := setupHealthTest(t, agentd.StatuszResponse{
		Healthy: true, Ready: true, Connected: []string{"opencode"},
		ProvidersConfigured: 1, AgentVersion: "1.2.27",
	})
	defer server.Close()
	ws.Status.ConsecutiveHealthFailures = 5

	r.checkAgentHealth(context.Background(), ws)
	assert.Equal(t, int32(0), ws.Status.ConsecutiveHealthFailures, "success should reset failure count")
}

func TestCheckAgentHealth_UnhealthyRepairsPodAfterThreshold(t *testing.T) {
	opencode.Register()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agentd.StatuszResponse{Healthy: false})
	}))
	defer server.Close()

	scheme := testScheme(t)
	ws := makeWorkspace("ws-repair", "default", v1.WorkspacePhaseActive)
	ws.UID = "ws-repair-uid"
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ws.Status.PodIP = "127.0.0.1"

	pod := makeRunningPod(podName("ws-repair", string(ws.UID)), "default", "127.0.0.1")

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = port
	agentdAdminPort = port
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws, pod).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	ws.Status.ConsecutiveHealthFailures = 2
	r.checkAgentHealth(context.Background(), ws)

	// Counter is reset to 0 after the restart trigger so the
	// freshly-spawned pod starts with a clean slate; without the reset
	// the first connection-refused on the new pod would re-trip the
	// threshold immediately.
	assert.Equal(t, int32(0), ws.Status.ConsecutiveHealthFailures,
		"counter must be reset to 0 after restart trigger; otherwise the new pod loops")
	assert.Equal(t, v1.WorkspacePhaseCreating, ws.Status.Phase, "should transition to Creating to restart pod")
	assert.Empty(t, ws.Status.PodIP, "PodIP should be cleared")
	assert.Equal(t, int32(1), ws.Status.RestartCount, "RestartCount should increment")

	var podCheck corev1.Pod
	getErr := fc.Get(context.Background(), types.NamespacedName{Name: podName("ws-repair", string(ws.UID)), Namespace: "default"}, &podCheck)
	assert.True(t, getErr != nil, "pod should be deleted after health failure threshold")
}

func TestCheckAgentHealth_UnhealthyBelowThreshold_NoRepair(t *testing.T) {
	opencode.Register()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agentd.StatuszResponse{Healthy: false})
	}))
	defer server.Close()

	scheme := testScheme(t)
	ws := makeWorkspace("ws-below", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ws.Status.PodIP = "127.0.0.1"

	origInterval := healthCheckInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	agentdPort = port
	agentdAdminPort = port
	defer func() {
		healthCheckInterval = origInterval
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	ws.Status.ConsecutiveHealthFailures = 1
	r.checkAgentHealth(context.Background(), ws)

	assert.Equal(t, int32(2), ws.Status.ConsecutiveHealthFailures)
	assert.Equal(t, v1.WorkspacePhaseActive, ws.Status.Phase, "should stay Active below threshold")
	assert.Equal(t, int32(0), ws.Status.RestartCount)
}

func TestCheckAgentHealth_StaleFailuresResetOnNewPod(t *testing.T) {
	opencode.Register()
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-stale", "default", v1.WorkspacePhaseActive)
	startTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	ws.Status.StartTime = &startTime
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.ConsecutiveHealthFailures = 99
	ws.Status.LastHealthCheckAt = &metav1.Time{Time: startTime.Add(-10 * time.Minute)}

	origInterval := healthCheckInterval
	origBackoff := healthCheckBackoffInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	healthCheckBackoffInterval = 0
	agentdAdminPort = 1
	agentdPort = 1
	defer func() {
		healthCheckInterval = origInterval
		healthCheckBackoffInterval = origBackoff
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	r.checkAgentHealth(context.Background(), ws)
	assert.Equal(t, int32(1), ws.Status.ConsecutiveHealthFailures,
		"stale failures from before pod start should reset, then count new failure")
}

func TestCheckAgentHealth_NoResetWhenHealthCheckAfterStart(t *testing.T) {
	opencode.Register()
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-fresh", "default", v1.WorkspacePhaseActive)
	startTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &startTime
	ws.Status.PodIP = "127.0.0.1"
	// Start below threshold so this test exercises the "preserve and
	// increment" path without crossing into the threshold-restart
	// branch (which legitimately resets the counter to 0 — covered by
	// TestCheckAgentHealth_ConnectionRefused_RestartsAfterThreshold).
	ws.Status.ConsecutiveHealthFailures = 1
	ws.Status.LastHealthCheckAt = &metav1.Time{Time: startTime.Add(5 * time.Minute)}

	origInterval := healthCheckInterval
	origBackoff := healthCheckBackoffInterval
	origPort := agentdPort
	origAdminPort := agentdAdminPort
	healthCheckInterval = 0
	healthCheckBackoffInterval = 0
	agentdAdminPort = 1
	agentdPort = 1
	defer func() {
		healthCheckInterval = origInterval
		healthCheckBackoffInterval = origBackoff
		agentdPort = origPort
		agentdAdminPort = origAdminPort
	}()

	r.checkAgentHealth(context.Background(), ws)
	assert.Equal(t, int32(2), ws.Status.ConsecutiveHealthFailures,
		"failures from current pod should be preserved and incremented (below threshold)")
}

func TestCheckAgentHealth_EmptyPodIP(t *testing.T) {
	opencode.Register()
	r := reconcilerFor(t)
	ws := makeWorkspace("ws-noip", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = ""

	r.checkAgentHealth(context.Background(), ws)
	assert.Empty(t, ws.Status.Conditions, "no health check should run without PodIP")
}

func TestBuildPod_HTTPProbes(t *testing.T) {
	opencode.Register()
	ws := makeWorkspace("ws-probes", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-probes"
	pvc := makeBoundPVC("workspace-ws-probes", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-probes", "default")
	rte := makeRuntimeEnv("python-3.11")
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.NotNil(t, pod.Spec.Containers[0].ReadinessProbe)
	assert.NotNil(t, pod.Spec.Containers[0].ReadinessProbe.HTTPGet)
	assert.Equal(t, "/v1/readyz", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(4098), pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal)
	assert.Nil(t, pod.Spec.Containers[0].ReadinessProbe.TCPSocket)

	require.NotNil(t, pod.Spec.Containers[0].LivenessProbe)
	assert.NotNil(t, pod.Spec.Containers[0].LivenessProbe.HTTPGet)
	assert.Equal(t, "/v1/healthz", pod.Spec.Containers[0].LivenessProbe.HTTPGet.Path)
	assert.Equal(t, int32(4098), pod.Spec.Containers[0].LivenessProbe.HTTPGet.Port.IntVal)
	assert.Nil(t, pod.Spec.Containers[0].LivenessProbe.TCPSocket)

	portNames := make(map[string]bool)
	for _, p := range pod.Spec.Containers[0].Ports {
		portNames[p.Name] = true
	}
	assert.True(t, portNames["opencode"], "opencode port should be declared")
	assert.True(t, portNames["agentd"], "agentd port should be declared")
}

func TestInitContainerScript_NoElseBranch(t *testing.T) {
	opencode.Register()
	ws := makeWorkspace("ws-init", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-init"
	pvc := makeBoundPVC("workspace-ws-init", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-init", "default")
	rte := makeRuntimeEnv("python-3.11")
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var credInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "credential-setup" {
			credInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, credInit, "credential-setup init container should exist")
	require.Len(t, credInit.Command, 3)
	assert.Equal(t, "/bin/sh", credInit.Command[0])
	assert.Equal(t, "-c", credInit.Command[1])
	script := credInit.Command[2]

	// Epic 35: the init script calls bootstrap (fetches secrets + workspace-config
	// from the API) then materialize (applies them). Password is still copied
	// from the K8s Secret.
	assert.Contains(t, script, "workspace-agentd bootstrap",
		"init script must call workspace-agentd bootstrap (Epic 35 secretless injection)")
	assert.Contains(t, script, "workspace-agentd materialize",
		"init script must call workspace-agentd materialize")
	// G21: install -m 0600 (not cp) so the password file is mode 0600
	// regardless of the source Secret's defaultMode.
	assert.Contains(t, script, "install -m 0600 /mnt/secrets/password/password /sandbox-cfg/password",
		"password should be installed with mode 0600 (G21: cp preserved source mode 0644)")

	// Verify the credential-setup init container mounts bootstrap-token.
	var bootstrapMount *corev1.VolumeMount
	for i := range credInit.VolumeMounts {
		if credInit.VolumeMounts[i].Name == "bootstrap-token" {
			bootstrapMount = &credInit.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, bootstrapMount, "credential-setup init container must mount bootstrap-token")
	assert.Equal(t, "/var/run/bootstrap", bootstrapMount.MountPath)
	assert.True(t, bootstrapMount.ReadOnly)
}

// TestInitContainerScript_BootstrapEnvVars verifies the init container
// carries WORKSPACE_ID and LLMSAFESPACE_API_URL env vars needed by the
// bootstrap subcommand.
func TestInitContainerScript_BootstrapEnvVars(t *testing.T) {
	opencode.Register()
	ws := makeWorkspace("ws-env", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-env"
	pvc := makeBoundPVC("workspace-ws-env", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-env", "default")
	rte := makeRuntimeEnv("python-3.11")
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)
	r.APIServiceURL = "http://test-api:8080"

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var credInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "credential-setup" {
			credInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, credInit)

	envVars := make(map[string]string)
	for _, e := range credInit.Env {
		envVars[e.Name] = e.Value
	}
	assert.Equal(t, "ws-env", envVars["WORKSPACE_ID"],
		"WORKSPACE_ID env var must be set on init container")
	assert.Equal(t, "http://test-api:8080", envVars["LLMSAFESPACE_API_URL"],
		"LLMSAFESPACE_API_URL env var must be set on init container")
}

func makeRuntimeEnv(name string) *v1.RuntimeEnvironment {
	return &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/" + name, Language: "python", Version: "3.11"},
	}
}

func TestRemoveCondition(t *testing.T) {
	ws := &v1.Workspace{
		Status: v1.WorkspaceStatus{
			Conditions: []v1.WorkspaceCondition{
				{Type: v1.WorkspaceConditionReady, Status: "True"},
				{Type: v1.WorkspaceConditionPodRunning, Status: "True"},
				{Type: v1.WorkspaceConditionAgentHealthy, Status: "False"},
			},
		},
	}
	r := reconcilerFor(t)

	r.removeCondition(ws, v1.WorkspaceConditionPodRunning)

	assert.Len(t, ws.Status.Conditions, 2)
	for _, c := range ws.Status.Conditions {
		assert.NotEqual(t, v1.WorkspaceConditionPodRunning, c.Type)
	}
}

func TestRemoveCondition_NotPresent(t *testing.T) {
	ws := &v1.Workspace{
		Status: v1.WorkspaceStatus{
			Conditions: []v1.WorkspaceCondition{
				{Type: v1.WorkspaceConditionReady, Status: "True"},
			},
		},
	}
	r := reconcilerFor(t)

	r.removeCondition(ws, v1.WorkspaceConditionPodRunning)

	assert.Len(t, ws.Status.Conditions, 1)
}
