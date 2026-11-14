// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// pi-overlay (design 0053 §5.4): image-volume delivery wiring for the
// second-harness pi artifact — the third platform overlay.
//
// The invariants these tests lock in (mirroring
// opencode_overlay_test.go, extended to the TRI-overlay pod):
//
//   - Enabled: image volume with digest-pinned reference + IfNotPresent;
//     readOnly mount at /pi on the workspace container ONLY; env pins
//     carrying the absolute overlay binary path (/pi/pi — the upstream
//     tree layout) and per-arch sha256s.
//   - Verify failures (supervisor exit 85/86) set the PiVerified
//     condition, emit one event per episode, increment the metric once
//     per episode, and do NOT enter the crashloop recovery path.
//   - Exit-code disambiguation on TRI-overlay pods: agentd's 81/82 and
//     opencode's 83/84 must never set the pi condition, and pi's 85/86
//     must never set either sibling's (failure attribution must not
//     cross artifacts).

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/require"
)

const (
	testPiImage    = "ghcr.io/lenaxia/llmsafespaces/pi@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPiSHAAMD64 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	testPiSHAARM64 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	piMetricLabel = "0123456789ab"
)

func reconcilerWithPi(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerFor(t)
	r.PiImage = testPiImage
	r.PiBinarySHA256AMD64 = testPiSHAAMD64
	r.PiBinarySHA256ARM64 = testPiSHAARM64
	// S5: a pi workspace requires the sidecar topology (supervise-pi is
	// a PID-1 supervisor) — a pi-test reconciler models a pi-capable
	// controller.
	r.AgentdSidecarEnabled = true
	r.Recorder = record.NewFakeRecorder(16)
	return r
}

// piHarnessWorkspace stamps spec.harness=pi — under S5 the pi overlay
// wires ONLY for a pi-harness workspace (exactly one harness overlay per
// pod, harness_delivery.go).
func piHarnessWorkspace(t *testing.T) *v1.Workspace {
	t.Helper()
	ws := newWorkspaceForSecurity(t)
	ws.Spec.Harness = v1.HarnessPi
	return ws
}

// reconcilerWithAllOverlays enables agentd + opencode + pi delivery —
// the tri-overlay pod shape whose failure attribution must stay
// disjoint across 81/82, 83/84, and 85/86.
func reconcilerWithAllOverlays(t *testing.T, sidecar bool) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerWithBothOverlays(t, sidecar)
	r.PiImage = testPiImage
	r.PiBinarySHA256AMD64 = testPiSHAAMD64
	r.PiBinarySHA256ARM64 = testPiSHAARM64
	return r
}

// withPiOverlayMarker stamps the env marker the controller sets in the
// same branch as the pi volume — detection keys on it.
func withPiOverlayMarker(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "PI_IMAGE_VOLUME", Value: "1"})
	return pod
}

// --- buildPod wiring ------------------------------------------------------

func TestPiOverlay_Enabled_WiresImageVolume(t *testing.T) {
	ws := piHarnessWorkspace(t)
	r := reconcilerWithPi(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "pi" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "pi image volume must exist when delivery is enabled")
	require.NotNil(t, vol.Image, "volume must use an Image volume source")
	require.Equal(t, testPiImage, vol.Image.Reference,
		"reference must be the digest-pinned image")
	require.Equal(t, corev1.PullIfNotPresent, vol.Image.PullPolicy,
		"digest-pinned immutable content: IfNotPresent, never Always")
}

func TestPiOverlay_MountIsReadOnly(t *testing.T) {
	ws := piHarnessWorkspace(t)
	r := reconcilerWithPi(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "pi" {
			found = true
			require.Equal(t, "/pi", m.MountPath)
			require.True(t, m.ReadOnly,
				"pi mount MUST be readOnly — an RW mount lets the main container swap the binary for the next container restart (init containers do not re-run within a pod)")
		}
	}
	require.True(t, found, "workspace container must mount the pi volume")
}

// TestPiOverlay_WorkspaceContainerOnly locks the mount topology: the
// supervisor in the WORKSPACE container (the future harness driver)
// owns pi, so the volume mounts there and ONLY there — not on the agentd
// sidecar, not on any init container.
func TestPiOverlay_WorkspaceContainerOnly(t *testing.T) {
	ws := piHarnessWorkspace(t)
	r := reconcilerWithAllOverlays(t, true)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Equal(t, "workspace", pod.Spec.Containers[0].Name)
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "pi" {
			require.Equal(t, "/pi", m.MountPath)
			require.True(t, m.ReadOnly)
		}
	}
	for i := range pod.Spec.InitContainers {
		for _, m := range pod.Spec.InitContainers[i].VolumeMounts {
			require.NotEqual(t, "pi", m.Name,
				"init container %s must not mount the pi volume", pod.Spec.InitContainers[i].Name)
		}
	}
	for i := range pod.Spec.Containers {
		if i == 0 {
			continue
		}
		for _, m := range pod.Spec.Containers[i].VolumeMounts {
			require.NotEqual(t, "pi", m.Name, "non-workspace containers must not mount the pi volume")
		}
	}
}

// TestPiOverlay_EnvPinsBinaryPathAndBothArchHashes: the binary env is
// /pi/pi — the upstream TREE-ROOT layout (bun package.json autoload
// adjacency), not a /pi/bin/pi subpath (ruling in runtimes/pi/Dockerfile).
func TestPiOverlay_EnvPinsBinaryPathAndBothArchHashes(t *testing.T) {
	ws := piHarnessWorkspace(t)
	r := reconcilerWithPi(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	require.Equal(t, "1", env["PI_IMAGE_VOLUME"],
		"overlay marker — the detection gate keys on it")
	require.Equal(t, "/pi/pi", env["LLMSAFESPACES_PI_BINARY"],
		"absolute overlay path at the TREE ROOT — upstream layout, package.json adjacency (never relocate inside the tree)")
	require.Equal(t, testPiSHAAMD64, env["LLMSAFESPACES_PI_SHA256_AMD64"])
	require.Equal(t, testPiSHAARM64, env["LLMSAFESPACES_PI_SHA256_ARM64"],
		"per-arch hashes: the manifest list carries different binaries per arch")
}

// TestPiOverlay_PerHarnessSelection_Coexistence (S5): exactly ONE
// harness overlay wires per workspace, selected by spec.harness
// (harness_delivery.go). On one fully-pinned controller an opencode
// workspace wires agentd+opencode and never the pi volume; a pi
// workspace wires agentd+pi and never the opencode volume. The S1-era
// "all three overlays in one pod" shape died with the harness
// dimension; exit-code attribution disjointness (81/82 vs 83/84 vs
// 85/86) stays covered by the tests below.
func TestPiOverlay_PerHarnessSelection_Coexistence(t *testing.T) {
	r := reconcilerWithAllOverlays(t, true)

	overlayShape := func(t *testing.T, ws *v1.Workspace) (volNames, mounts map[string]string) {
		t.Helper()
		pod, err := r.buildPod(context.Background(), ws)
		require.NoError(t, err)
		volNames, mounts = map[string]string{}, map[string]string{}
		for _, v := range pod.Spec.Volumes {
			if v.Image != nil {
				volNames[v.Name] = v.Image.Reference
			}
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.Name == "agentd" || m.Name == "opencode" || m.Name == "pi" {
				mounts[m.Name] = m.MountPath
			}
		}
		return volNames, mounts
	}

	// Default (harness="") workspace: agentd + opencode, never pi.
	ocVols, ocMounts := overlayShape(t, newWorkspaceForSecurity(t))
	require.Equal(t, testAgentdImage, ocVols["agentd"], "agentd overlay volume")
	require.Equal(t, testOpencodeImage, ocVols["opencode"], "opencode overlay volume")
	require.NotContains(t, ocVols, "pi", "an opencode workspace must never wire the pi overlay")
	require.Equal(t, "/agentd", ocMounts["agentd"])
	require.Equal(t, "/opencode", ocMounts["opencode"])
	require.NotContains(t, ocMounts, "pi")

	// pi workspace on the SAME controller: agentd + pi, never opencode.
	piVols, piMounts := overlayShape(t, piHarnessWorkspace(t))
	require.Equal(t, testAgentdImage, piVols["agentd"], "agentd overlay volume")
	require.Equal(t, testPiImage, piVols["pi"], "pi overlay volume")
	require.NotContains(t, piVols, "opencode", "a pi workspace must never wire the opencode overlay")
	require.Equal(t, "/agentd", piMounts["agentd"])
	require.Equal(t, "/pi", piMounts["pi"])
	require.NotContains(t, piMounts, "opencode")
}

// --- verify-failure detection (handleActive) ------------------------------

func TestPiVerify_Mismatch_SetsConditionEmitsEventAndMetric(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-verify")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitVerifyFailed,
		"PiVerificationFailed: expected=eeee got=aaaa binary=/pi/pi")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionPiVerified)
	require.NotNil(t, cond, "PiVerified condition must be set")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonPiVerificationFailed), cond.Reason)
	require.Contains(t, cond.Message, "expected=eeee")
	require.Contains(t, cond.Message, "got=aaaa")

	require.Equal(t, before+1.0, testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel)),
		"verify-failure metric must increment on detection")

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		require.Contains(t, e, "PiVerificationFailed")
		require.Contains(t, e, "expected=eeee")
	default:
		t.Fatal("expected a warning event on the Workspace")
	}
}

func TestPiVerify_OverlayMissing_UsesOverlayReason(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-overlay")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitOverlayMissing,
		"PiOverlayMissing: binary=/pi/pi")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("overlay_missing", "node-1", piMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionPiVerified)
	require.NotNil(t, cond)
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonPiOverlayMissing), cond.Reason)
	require.Equal(t, before+1.0, testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("overlay_missing", "node-1", piMetricLabel)))
}

func TestPiVerify_FailureIsIdempotentPerEpisode(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-dedup")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitVerifyFailed, "PiVerificationFailed: expected=eeee got=aaaa")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	for i := 0; i < 3; i++ {
		_, err := r.handleActive(context.Background(), ws)
		require.NoError(t, err)
	}
	after := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	require.Equal(t, before+1.0, after,
		"repeated reconciles of the same failure episode must not re-increment")
}

func TestPiVerify_CrashloopFromOtherCause_DoesNotSetCondition(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-other")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", 137, "some other crash")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionPiVerified),
		"non-pi exit codes must not set the PiVerified condition")
}

func TestPiVerify_HealthyPod_SetsTrueCondition(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-ok")

	pod := makeWorkspacePod(ws, "", -1, "")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionPiVerified)
	require.NotNil(t, cond, "overlay mode must surface a positive PiVerified condition")
	require.Equal(t, "True", cond.Status)
	require.Equal(t, string(v1.ReasonPiVerified), cond.Reason)
}

func TestPiVerify_RecoveryNotEnteredOnVerifyFailure(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-norec")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitVerifyFailed, "PiVerificationFailed: expected=eeee got=aaaa")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, v1.WorkspacePhaseActive, ws.Status.Phase,
		"a digest mismatch cannot be fixed by pod restarts — must not enter recovery/crashloop machinery")
	require.Equal(t, int32(0), ws.Status.RestartCount, "no restart may be counted")
}

// TestPiVerify_CreatingPhase_FirstBootBadPinIsDetected: a bad pin at
// first boot/resume exits 85 before the container is ever Ready, so
// detection must fire from handleCreating too.
func TestPiVerify_CreatingPhase_FirstBootBadPinIsDetected(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := makeWorkspace("ws-pi-creating", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-ws-pi-creating"
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitVerifyFailed,
		"PiVerificationFailed: expected=eeee got=aaaa")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(withOpencodeOverlayMarker(pod))))

	before := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	res, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, piVerifyFailureRequeue, res.RequeueAfter,
		"verify failure in Creating must use the slow requeue, not the 2s fall-through")

	cond := conditionOfType(ws, v1.WorkspaceConditionPiVerified)
	require.NotNil(t, cond, "condition must fire from handleCreating")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonPiVerificationFailed), cond.Reason)
	after := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	require.Equal(t, before+1.0, after, "metric must fire from handleCreating")
}

// --- exit-code disambiguation on TRI-overlay pods ---------------------------

// TestPiVerify_AgentdExit81_DoesNotSetPiCondition: a tri-overlay pod
// whose agentd verify failed (exit 81) must attribute the failure to
// agentd ONLY — the pi condition stays absent and the pi metric
// untouched.
func TestPiVerify_AgentdExit81_DoesNotSetPiCondition(t *testing.T) {
	r := reconcilerWithAllOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-pi-agentd81")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed,
		"AgentdVerificationFailed: expected=aaaa got=eeee")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(withOpencodeOverlayMarker(pod))))

	piBefore := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	ag := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, ag, "the agentd exit-81 must still be attributed to agentd")
	require.Equal(t, "False", ag.Status)
	require.Equal(t, string(v1.ReasonAgentdVerificationFailed), ag.Reason)

	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionPiVerified),
		"agentd's exit 81 must never set the pi condition — failure attribution must not cross artifacts")
	require.Equal(t, piBefore, testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel)),
		"agentd's exit 81 must not increment the pi metric")
}

// TestPiVerify_OpencodeExit83_DoesNotSetPiCondition: an opencode verify
// failure (exit 83) on a tri-overlay pod must not set the pi condition.
func TestPiVerify_OpencodeExit83_DoesNotSetPiCondition(t *testing.T) {
	r := reconcilerWithAllOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-pi-exit83")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed,
		"OpencodeVerificationFailed: expected=cccc got=eeee")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(withOpencodeOverlayMarker(pod))))

	piBefore := testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	oc := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, oc, "the opencode exit-83 must be attributed to opencode")
	require.Equal(t, "False", oc.Status)

	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionPiVerified),
		"opencode's exit 83 must never set the pi condition")
	require.Equal(t, piBefore, testutil.ToFloat64(metricsPiVerifyFailures.WithLabelValues("verify_failed", "node-1", piMetricLabel)),
		"opencode's exit 83 must not increment the pi metric")
}

// TestPiVerify_PiExit85_DoesNotSetSiblingConditions: the mirror — a pi
// verify failure (exit 85) on a tri-overlay pod must not set the agentd
// or opencode conditions or metrics.
func TestPiVerify_PiExit85_DoesNotSetSiblingConditions(t *testing.T) {
	r := reconcilerWithAllOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-pi-exit85")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", piExitVerifyFailed,
		"PiVerificationFailed: expected=eeee got=aaaa")
	require.NoError(t, r.Create(context.Background(), withPiOverlayMarker(pod)))

	agBefore := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	ocBefore := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	pi := conditionOfType(ws, v1.WorkspaceConditionPiVerified)
	require.NotNil(t, pi, "the pi exit-85 must be attributed to pi")
	require.Equal(t, "False", pi.Status)
	require.Equal(t, string(v1.ReasonPiVerificationFailed), pi.Reason)

	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified),
		"pi's exit 85 must never set the agentd condition")
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"pi's exit 85 must never set the opencode condition")
	require.Equal(t, agBefore, testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab")),
		"pi's exit 85 must not increment the agentd metric")
	require.Equal(t, ocBefore, testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel)),
		"pi's exit 85 must not increment the opencode metric")
}

// --- legacy-pod gating ------------------------------------------------------

// TestPiVerify_LegacyPodNeverVerified: pods from before the pi overlay
// landed carry no PI_IMAGE_VOLUME marker — they must not be marked
// PiVerified (the gate keys on the POD's env marker, not the flag).
func TestPiVerify_LegacyPodNeverVerified(t *testing.T) {
	r := reconcilerWithPi(t)
	ws := activeOverlayWorkspace(t, r, "ws-pi-legacy-ok")
	pod := makeWorkspacePod(ws, "", -1, "")
	pod.Spec.Containers[0].Env = nil
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionPiVerified),
		"legacy pod must not be marked verified — nothing about it was ever verified")
}
