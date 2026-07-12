// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// pod_builder_test.go — regression tests for workspace pod construction.
//
// Each test in this file pins one behavioral assertion about the pod spec
// produced by buildPod(). Tests are named after the worklog/epic that
// introduced the requirement they guard.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/controller/internal/freemodels"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func newWorkspaceForPodBuilder(t *testing.T) *v1.Workspace {
	t.Helper()
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-pod-builder-test",
			Namespace: "default",
		},
		Spec: v1.WorkspaceSpec{
			Runtime: "ghcr.io/lenaxia/llmsafespaces/runtimes/base:test",
		},
		Status: v1.WorkspaceStatus{
			PVCName: "pvc-pod-builder-test",
		},
	}
}

// TestPodBuilder_ContainerEnv_RequiredVars checks that the workspace container
// includes the minimum set of env vars needed for the agent to function.
func TestPodBuilder_ContainerEnv_RequiredVars(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var mainEnv map[string]string
	for _, c := range pod.Spec.Containers {
		if c.Name == "workspace" {
			mainEnv = make(map[string]string, len(c.Env))
			for _, e := range c.Env {
				mainEnv[e.Name] = e.Value
			}
			break
		}
	}
	require.NotNil(t, mainEnv, "workspace container not found in pod spec")

	assert.Equal(t, ws.Name, mainEnv["WORKSPACE_ID"])
	assert.NotEmpty(t, mainEnv["WORKSPACE_DIR"])
}

// TestPodBuilder_ContainerEnv_OpenCodeExperimentalEventSystem is the regression
// test for the context-usage "0/Unknown" bug (worklog 0263).
//
// Root cause: OPENCODE_EXPERIMENTAL_EVENT_SYSTEM was not set in the workspace pod
// env, so opencode never emitted session.next.step.ended to the /event SSE stream.
// The API proxy's persistContextFromEvent was therefore never called, leaving
// session_index.context_used NULL for every session and the Sidebar showing "0/Unknown".
//
// Fix: set OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true unconditionally in all workspace pods.
//
// Proven by live cluster experiment (worklog 0263): adding the flag to /tmp/secrets-env
// and restarting opencode caused context_used to be written to session_index within one
// second of the next LLM step completing (114422 tokens, exact match with
// input + cache.read + cache.write from the step.ended event).
func TestPodBuilder_ContainerEnv_OpenCodeExperimentalEventSystem(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var found bool
	for _, c := range pod.Spec.Containers {
		if c.Name != "workspace" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "OPENCODE_EXPERIMENTAL_EVENT_SYSTEM" {
				assert.Equal(t, "true", e.Value,
					"OPENCODE_EXPERIMENTAL_EVENT_SYSTEM must be 'true' — "+
						"without it opencode never emits step.ended and context_used is never written to DB")
				found = true
			}
		}
	}
	assert.True(t, found,
		"OPENCODE_EXPERIMENTAL_EVENT_SYSTEM must be present in the workspace container env — "+
			"it is required for the context usage bar to display real values")
}

// TestPodBuilder_ReadinessProbe_TightTiming verifies the readiness probe
// is configured for fast pod-Ready detection (cold-start optimization,
// 2026-06-23 perf audit).
//
// Pre-fix: InitialDelaySeconds=10, PeriodSeconds=15 — kubelet would wait
// 10s before probing, then poll every 15s. The agent reaches /v1/readyz=200
// at roughly T+22s after PodScheduled, so on a bad probe-phase alignment
// the pod could remain "not Ready" for an additional 5–13s after the agent
// was actually ready.
//
// Post-fix: InitialDelaySeconds=2, PeriodSeconds=2 — overall ready-detection
// budget is similar (FailureThreshold raised to 30 → 60s tolerance) but
// post-readyz-200 latency drops to a single 2s tick.
func TestPodBuilder_ReadinessProbe_TightTiming(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Len(t, pod.Spec.Containers, 1, "expected one main container")
	probe := pod.Spec.Containers[0].ReadinessProbe
	require.NotNil(t, probe, "readiness probe must be set")

	assert.Equal(t, int32(2), probe.InitialDelaySeconds,
		"InitialDelaySeconds must be 2s — kubelet should start probing quickly so "+
			"a cold-started agent transitions to Ready within one poll period")
	assert.Equal(t, int32(2), probe.PeriodSeconds,
		"PeriodSeconds must be 2s — readiness checks must align tightly to "+
			"agent /v1/readyz=200 to minimize post-ready dead time")
	assert.Equal(t, int32(2), probe.TimeoutSeconds,
		"TimeoutSeconds must be 2s — /v1/readyz is cache-backed, sub-50ms in "+
			"the steady state, so 2s is a generous failure budget")
	assert.Equal(t, int32(30), probe.FailureThreshold,
		"FailureThreshold must be 30 — preserves 60s total ready budget at 2s period")
}

// TestPodBuilder_StartupProbe_FastDetection verifies the startup probe
// allows aggressive cold-start polling without affecting steady-state
// liveness behavior (2026-06-23 perf audit).
//
// Why a separate startup probe: kubelet runs only one probe at a time
// per container — when the startup probe is set, liveness and readiness
// probes are paused until startup succeeds. This lets us probe at 1s
// intervals during boot without paying the cost on every steady-state
// liveness check.
func TestPodBuilder_StartupProbe_FastDetection(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	probe := pod.Spec.Containers[0].StartupProbe
	require.NotNil(t, probe, "startup probe must be set so the readiness probe is "+
		"unblocked the moment agentd reports ready, not on the next poll cycle")
	require.NotNil(t, probe.HTTPGet, "startup probe must use HTTP, matching readiness")
	assert.Equal(t, "/v1/readyz", probe.HTTPGet.Path,
		"startup probe path must match readiness — same gate, faster cadence")
	assert.Equal(t, int32(1), probe.PeriodSeconds,
		"PeriodSeconds=1 — probe every second during boot")
	assert.GreaterOrEqual(t, probe.FailureThreshold, int32(60),
		"FailureThreshold must be >=60 to give the relay-injector restart cycle (~30s) "+
			"plus a safety margin before the pod is killed")
}

// TestPodBuilder_Probes_AuthHeader closes the test gap noted in the
// PR #386 review (commit 35c248c8 follow-up): /v1/readyz is gated by
// requireBearerToken in cmd/workspace-agentd/server.go, so BOTH the
// readiness probe AND the startup probe must include
// `Authorization: Bearer <admin-token>` headers. A probe without the
// header would 401 on every attempt and the pod would never pass —
// a silent, hard-to-debug failure.
//
// The header value comes from the password Secret created by
// ensurePasswordSecret in handlePending (Data["password"]). The pod
// builder reads the Secret via the controller client; this test
// seeds a password Secret into the fake client so adminToken is
// non-empty and the header MUST be present.
//
// 2026-06-23 perf audit, items #3 + #4 (readiness + startup probes).
func TestPodBuilder_Probes_AuthHeader(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	pwSecret := makePasswordSecret(ws.Name, ws.Namespace)
	r := reconcilerFor(t, pwSecret)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	main := &pod.Spec.Containers[0]

	expectAuthHeader := func(t *testing.T, probe *corev1.Probe, label string) {
		t.Helper()
		require.NotNil(t, probe, "%s probe must be set", label)
		require.NotNil(t, probe.HTTPGet, "%s probe must use HTTP", label)
		var got *corev1.HTTPHeader
		for i := range probe.HTTPGet.HTTPHeaders {
			if probe.HTTPGet.HTTPHeaders[i].Name == "Authorization" {
				got = &probe.HTTPGet.HTTPHeaders[i]
				break
			}
		}
		require.NotNil(t, got,
			"%s probe MUST include an Authorization header — "+
				"/v1/readyz is gated by requireBearerToken (server.go), "+
				"a probe without the header always 401s and the pod stays NotReady",
			label)
		assert.Equal(t, "Bearer test-password", got.Value,
			"%s probe header value must be `Bearer <admin-token>` where "+
				"admin-token is read from the password Secret's `password` key",
			label)
	}

	expectAuthHeader(t, main.ReadinessProbe, "readiness")
	expectAuthHeader(t, main.StartupProbe, "startup")
}

// TestPodBuilder_Probes_NoAuthHeaderWhenSecretMissing covers the
// graceful-degradation path: if the password Secret is somehow not
// available at buildPod time (Get fails or Data["password"] is
// missing), the probes must NOT carry an empty Bearer header (which
// /v1/readyz would still reject). Instead they omit the header
// entirely — the probe will 401 and the pod stays NotReady, which
// is observable + safe (vs. a malformed header which would be
// harder to diagnose).
func TestPodBuilder_Probes_NoAuthHeaderWhenSecretMissing(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t) // no pwSecret seeded

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := &pod.Spec.Containers[0]

	for _, p := range []*corev1.Probe{main.ReadinessProbe, main.StartupProbe} {
		require.NotNil(t, p)
		require.NotNil(t, p.HTTPGet)
		assert.Empty(t, p.HTTPGet.HTTPHeaders,
			"probe must omit headers entirely when admin token is unavailable — "+
				"a `Bearer ` (empty) header would still be rejected, providing no value")
	}
}

// TestPodBuilder_LivenessProbe_StableTiming pins the liveness probe to a
// gentle steady-state cadence. Liveness-probe failures kill the pod, so
// timeouts and failure thresholds must be conservative; the startup probe
// (above) handles the boot-time tightening.
func TestPodBuilder_LivenessProbe_StableTiming(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	probe := pod.Spec.Containers[0].LivenessProbe
	require.NotNil(t, probe)
	require.NotNil(t, probe.HTTPGet)
	assert.Equal(t, "/v1/healthz", probe.HTTPGet.Path)
	// Period and threshold are deliberately gentle — liveness failures
	// kill the pod, so we want lots of slack against transient network
	// or overload conditions.
	assert.GreaterOrEqual(t, probe.PeriodSeconds, int32(10))
	assert.GreaterOrEqual(t, probe.FailureThreshold, int32(3))
}

// TestPodBuilder_TerminationGracePeriod_Tight verifies the pod's
// terminationGracePeriodSeconds is set to a tight value (2026-06-23 perf
// audit, item #5). The default kubelet value is 30s, but agentd has been
// measured to exit cleanly in under 1s on the live cluster — the headroom
// was unused.
//
// Concrete impact: on every controller-initiated pod recycle (suspend,
// restartGeneration bump, architecture drift, password-secret heal),
// this saves up to ~25s of dead time waiting for SIGKILL.
//
// Lower bound is 5s (not 1s) to leave room for opencode SIGTERM
// propagation by agentd's supervisor (managed_process.go reserves a
// 5s SIGTERM-then-SIGKILL window for the opencode child).
func TestPodBuilder_TerminationGracePeriod_Tight(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds,
		"terminationGracePeriodSeconds must be set explicitly — "+
			"the default of 30s wastes ~25s on every pod termination")
	assert.GreaterOrEqual(t, *pod.Spec.TerminationGracePeriodSeconds, int64(5),
		"must allow >=5s for agentd to SIGTERM opencode and exit cleanly")
	assert.LessOrEqual(t, *pod.Spec.TerminationGracePeriodSeconds, int64(15),
		"must be tight enough that suspend/recycle latency benefits — "+
			"agentd exits in <1s in practice, 30s default was over-provisioned")
}

// findVolume returns the named Volume from a pod spec, or nil.
func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// findInitContainer returns the named init container, or nil.
func findInitContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// findVolumeMount returns the named VolumeMount on a container, or nil.
func findVolumeMount(c *corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

// findEnv returns the named env var on a container, or nil.
func findEnv(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// reconcilerWithRelay returns a reconciler configured with a fake relay URL.
// The relay being non-empty is what triggers the Phase B free-models volume
// + env propagation. The chart's default is direct-to-Zen (empty relay URL);
// tests that exercise the relay path use this helper.
func reconcilerWithRelay(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerFor(t)
	r.InferenceRelayURL = "https://relay.test.example/"
	return r
}

// TestPodBuilder_FreeModelsVolume_AbsentWhenNoRelay verifies that when
// the controller is configured WITHOUT a relay URL, the free-models
// ConfigMap volume is NOT added to the pod spec — there's nothing for
// it to feed.
//
// 2026-06-23 cold-start optimization, item #1a (Phase B).
func TestPodBuilder_FreeModelsVolume_AbsentWhenNoRelay(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t) // no relay URL
	require.Empty(t, r.InferenceRelayURL)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	assert.Nil(t, findVolume(pod, "free-models"),
		"free-models volume must be absent when no relay URL is configured — "+
			"the ConfigMap exists but the pod has no use for it")

	credInit := findInitContainer(pod, "credential-setup")
	require.NotNil(t, credInit)
	assert.Nil(t, findVolumeMount(credInit, "free-models"),
		"credential-setup must not mount the free-models volume when relay is off")
	assert.Nil(t, findEnv(credInit, "INFERENCE_RELAY_BASEURL"),
		"INFERENCE_RELAY_BASEURL env must be absent when relay is off — "+
			"its presence is what tells materialize to attempt relay injection")
}

// TestPodBuilder_FreeModelsVolume_PresentWhenRelayConfigured verifies
// that with a relay URL, the pod spec carries:
//   - The free-models ConfigMap volume (optional: true)
//   - A volume mount on credential-setup at /mnt/freemodels (read-only)
//   - INFERENCE_RELAY_BASEURL env on the credential-setup init container
//     (so the materialize subcommand can read it before opencode boots)
//
// 2026-06-23 cold-start optimization, item #1a (Phase B).
func TestPodBuilder_FreeModelsVolume_PresentWhenRelayConfigured(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerWithRelay(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	vol := findVolume(pod, "free-models")
	require.NotNil(t, vol, "free-models volume must be present when relay is configured")
	require.NotNil(t, vol.ConfigMap, "free-models must be sourced from a ConfigMap")
	assert.Equal(t, freemodels.ConfigMapName, vol.ConfigMap.Name,
		"free-models volume must reference the cluster-wide ConfigMap published by the refresher")
	require.NotNil(t, vol.ConfigMap.Optional)
	assert.True(t, *vol.ConfigMap.Optional,
		"free-models ConfigMap mount MUST be optional — pods can boot before "+
			"the controller's first refresh completes; missing CM must not fail the pod")

	credInit := findInitContainer(pod, "credential-setup")
	require.NotNil(t, credInit, "credential-setup init container must exist")

	mount := findVolumeMount(credInit, "free-models")
	require.NotNil(t, mount, "credential-setup must mount the free-models volume")
	assert.Equal(t, "/mnt/freemodels", mount.MountPath,
		"free-models must be mounted at /mnt/freemodels — the credential-setup script "+
			"copies models.json from this path into /sandbox-cfg/free-models.json")
	assert.True(t, mount.ReadOnly, "free-models mount must be read-only")
}

// TestPodBuilder_RelayBaseURLEnv_OnInitContainer verifies that
// INFERENCE_RELAY_BASEURL is propagated to the credential-setup init
// container env (in addition to the main container, where it already
// was). Without this, agentd's materialize subcommand running INSIDE
// the init container has no way to know whether to attempt relay
// injection — and the whole point of Phase C is to do it pre-opencode.
//
// 2026-06-23 cold-start optimization, item #1a (Phase B).
func TestPodBuilder_RelayBaseURLEnv_OnInitContainer(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerWithRelay(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	credInit := findInitContainer(pod, "credential-setup")
	require.NotNil(t, credInit)

	env := findEnv(credInit, "INFERENCE_RELAY_BASEURL")
	require.NotNil(t, env,
		"INFERENCE_RELAY_BASEURL must be set on the credential-setup init container "+
			"so the materialize subcommand can pre-render the relay block")
	assert.Equal(t, "https://relay.test.example/", env.Value,
		"relay URL is the controller's InferenceRelayURL verbatim (no path-segment rewriting)")
}

// TestPodBuilder_RelayBaseURLEnv_MainContainer guards the existing
// behavior: INFERENCE_RELAY_BASEURL on the main container's env was
// already there pre-Phase-B; this test pins it so the new Phase B
// init-container env doesn't accidentally remove it.
func TestPodBuilder_RelayBaseURLEnv_MainContainer(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerWithRelay(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Len(t, pod.Spec.Containers, 1)
	main := &pod.Spec.Containers[0]
	require.Equal(t, "workspace", main.Name)

	env := findEnv(main, "INFERENCE_RELAY_BASEURL")
	require.NotNil(t, env, "main container must carry INFERENCE_RELAY_BASEURL when InferenceRelayURL is set")
	assert.Equal(t, "https://relay.test.example/", env.Value,
		"INFERENCE_RELAY_BASEURL equals InferenceRelayURL verbatim")
}

// TestPodBuilder_FreeModelsScriptCopy verifies the credential-setup
// init script copies the optional free-models file into /sandbox-cfg/
// when the file is present. The materialize subcommand expects to find
// it at /sandbox-cfg/free-models.json, sibling to other config files.
//
// The cp is guarded by `if [ -f /mnt/freemodels/models.json ]` so a
// missing CM (Optional: true) doesn't break the script — the copy
// happens IFF the file exists.
func TestPodBuilder_FreeModelsScriptCopy(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerWithRelay(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	credInit := findInitContainer(pod, "credential-setup")
	require.NotNil(t, credInit)
	require.Len(t, credInit.Command, 3)
	script := credInit.Command[2]

	assert.Contains(t, script, "if [ -f /mnt/freemodels/models.json ]",
		"copy must be conditional — missing CM (Optional: true) must not fail the script")
	assert.Contains(t, script, "cp /mnt/freemodels/models.json /sandbox-cfg/free-models.json",
		"models.json must land at /sandbox-cfg/free-models.json so materialize can find it")
}

// TestPodBuilder_InitXDGDataHome locks in the PR #401 review fix:
// the credential-setup init container must carry XDG_DATA_HOME set
// to /workspace/.local so agentd's materialize subcommand reads
// auth.json from the same path opencode reads it from in the main
// container (the symlink the init script creates that points into
// /sandbox-runtime/rt/auth.json).
//
// Without this env var, preBootAuthJSONPath in the init container
// falls back to $HOME/.local/opencode/auth.json
// (=/home/sandbox/.local/opencode/auth.json), which is NOT the same
// file opencode reads. For a resumed pod with a stale pre-US-35.7
// auth.json carrying a personal opencode key on the PVC home subpath,
// the bypass check would silently miss the key and the cold-start
// optimization would be lost (legacy in-pod injector would still
// catch it but the user loses the savings).
func TestPodBuilder_InitXDGDataHome(t *testing.T) {
	ws := newWorkspaceForPodBuilder(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	credInit := findInitContainer(pod, "credential-setup")
	require.NotNil(t, credInit)

	xdg := findEnv(credInit, "XDG_DATA_HOME")
	require.NotNil(t, xdg,
		"credential-setup init must carry XDG_DATA_HOME so preBootAuthJSONPath "+
			"resolves to the same auth.json opencode reads in the main container")
	assert.Equal(t, "/workspace/.local", xdg.Value,
		"XDG_DATA_HOME must match entrypoint-opencode.sh's value (/workspace/.local) — "+
			"any drift between the two would silently break the personal-key bypass")
}

// Silence "imported and not used" if any test above is removed.
var _ = metav1.ObjectMeta{}
