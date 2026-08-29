// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// opencode-overlay (design 0053 §4.2): image-volume delivery wiring for
// the opencode binary — the second platform overlay artifact.
//
// The invariants these tests lock in (mirroring agentd_overlay_test.go):
//
//   - Disabled (default): no volume, no mount, no env — inert until the
//     base is stripped (S3).
//   - Enabled: image volume with digest-pinned reference + IfNotPresent;
//     readOnly mount at /opencode on the workspace container ONLY (the
//     supervisor in the workspace container spawns opencode; neither the
//     agentd sidecar nor any init container needs it); env pins carrying
//     the absolute overlay binary path and per-arch sha256s.
//   - Verify failures (supervisor exit 83/84) set the OpencodeVerified
//     condition, emit one event per episode, increment the metric once
//     per episode, and do NOT enter the crashloop recovery path.
//   - Exit-code disambiguation on dual-overlay pods: agentd's 81/82 must
//     never set the opencode condition, and opencode's 83/84 must never
//     set the agentd condition (failure attribution must not cross).

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/require"
)

const (
	testOpencodeImage    = "ghcr.io/lenaxia/llmsafespaces/opencode@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testOpencodeSHAAMD64 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testOpencodeSHAARM64 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	opencodeMetricLabel = "fedcba987654"
)

func reconcilerWithOpencode(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerFor(t)
	r.OpencodeImage = testOpencodeImage
	r.OpencodeBinarySHA256AMD64 = testOpencodeSHAAMD64
	r.OpencodeBinarySHA256ARM64 = testOpencodeSHAARM64
	r.Recorder = record.NewFakeRecorder(16)
	return r
}

// reconcilerWithBothOverlays enables agentd + opencode delivery (and the
// agentd sidecar where asked) — the dual-overlay pod shape S1 must keep
// attribution-clean.
func reconcilerWithBothOverlays(t *testing.T, sidecar bool) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerWithAgentd(t)
	r.AgentdSidecarEnabled = sidecar
	r.OpencodeImage = testOpencodeImage
	r.OpencodeBinarySHA256AMD64 = testOpencodeSHAAMD64
	r.OpencodeBinarySHA256ARM64 = testOpencodeSHAARM64
	return r
}

// withOpencodeOverlayMarker stamps the env marker the controller sets in
// the same branch as the opencode volume — detection keys on it.
func withOpencodeOverlayMarker(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "OPENCODE_IMAGE_VOLUME", Value: "1"})
	return pod
}

// --- buildPod wiring ------------------------------------------------------

func TestOpencodeOverlay_DisabledByDefault_NoVolumeMountOrEnv(t *testing.T) {
	ws := newWorkspaceForSecurity(t)

	for name, r := range map[string]*WorkspaceReconciler{
		"nothing enabled":  reconcilerFor(t),
		"agentd only":      reconcilerWithAgentd(t),
		"agentd + sidecar": func() *WorkspaceReconciler { r := reconcilerWithAgentd(t); r.AgentdSidecarEnabled = true; return r }(),
	} {
		pod, err := r.buildPod(context.Background(), ws)
		require.NoError(t, err)

		for _, vol := range pod.Spec.Volumes {
			require.NotEqual(t, "opencode", vol.Name, "%s: opencode volume must not exist when opencodeDelivery is unset", name)
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			require.NotEqual(t, "/opencode", m.MountPath, "%s: no opencode mount when opencodeDelivery is unset", name)
		}
		for _, env := range pod.Spec.Containers[0].Env {
			require.False(t, strings.HasPrefix(env.Name, "LLMSAFESPACES_OPENCODE_"),
				"%s: no opencode env pins when opencodeDelivery is unset, got %s", name, env.Name)
			require.NotEqual(t, "OPENCODE_IMAGE_VOLUME", env.Name,
				"%s: no opencode overlay marker when opencodeDelivery is unset", name)
		}
	}
}

func TestOpencodeOverlay_Enabled_WiresImageVolume(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithOpencode(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "opencode" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "opencode image volume must exist when delivery is enabled")
	require.NotNil(t, vol.Image, "volume must use an Image volume source")
	require.Equal(t, testOpencodeImage, vol.Image.Reference,
		"reference must be the digest-pinned image")
	require.Equal(t, corev1.PullIfNotPresent, vol.Image.PullPolicy,
		"digest-pinned immutable content: IfNotPresent, never Always")
}

func TestOpencodeOverlay_MountIsReadOnly(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithOpencode(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "opencode" {
			found = true
			require.Equal(t, "/opencode", m.MountPath)
			require.True(t, m.ReadOnly,
				"opencode mount MUST be readOnly — an RW mount lets the main container swap the binary for the next container restart (init containers do not re-run within a pod)")
		}
	}
	require.True(t, found, "workspace container must mount the opencode volume")
}

// TestOpencodeOverlay_WorkspaceContainerOnly locks the mount topology:
// the supervisor in the WORKSPACE container spawns opencode, so the
// volume mounts there and ONLY there — not on the agentd sidecar, not on
// any init container.
func TestOpencodeOverlay_WorkspaceContainerOnly(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithBothOverlays(t, true)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	require.Equal(t, "workspace", pod.Spec.Containers[0].Name)
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "opencode" {
			require.Equal(t, "/opencode", m.MountPath)
			require.True(t, m.ReadOnly)
		}
	}
	for i := range pod.Spec.InitContainers {
		for _, m := range pod.Spec.InitContainers[i].VolumeMounts {
			require.NotEqual(t, "opencode", m.Name,
				"init container %s must not mount the opencode volume", pod.Spec.InitContainers[i].Name)
		}
	}
	for i := range pod.Spec.Containers {
		if i == 0 {
			continue
		}
		for _, m := range pod.Spec.Containers[i].VolumeMounts {
			require.NotEqual(t, "opencode", m.Name, "non-workspace containers must not mount the opencode volume")
		}
	}
}

func TestOpencodeOverlay_EnvPinsBinaryPathAndBothArchHashes(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithOpencode(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	require.Equal(t, "1", env["OPENCODE_IMAGE_VOLUME"],
		"overlay marker — the detection gate keys on it")
	require.Equal(t, "/opencode/usr/local/bin/opencode", env["LLMSAFESPACES_OPENCODE_BINARY"],
		"absolute overlay path — never PATH lookup, never relative")
	require.Equal(t, testOpencodeSHAAMD64, env["LLMSAFESPACES_OPENCODE_SHA256_AMD64"])
	require.Equal(t, testOpencodeSHAARM64, env["LLMSAFESPACES_OPENCODE_SHA256_ARM64"],
		"per-arch hashes: the manifest list carries different binaries per arch")
}

// --- verify-failure detection (handleActive) ------------------------------

func TestOpencodeVerify_Mismatch_SetsConditionEmitsEventAndMetric(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-verify")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed,
		"OpencodeVerificationFailed: expected=cccc got=eeee binary=/opencode/usr/local/bin/opencode")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, cond, "OpencodeVerified condition must be set")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonOpencodeVerificationFailed), cond.Reason)
	require.Contains(t, cond.Message, "expected=cccc")
	require.Contains(t, cond.Message, "got=eeee")

	require.Equal(t, before+1.0, testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel)),
		"verify-failure metric must increment on detection")

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		require.Contains(t, e, "OpencodeVerificationFailed")
		require.Contains(t, e, "expected=cccc")
	default:
		t.Fatal("expected a warning event on the Workspace")
	}
}

func TestOpencodeVerify_OverlayMissing_UsesOverlayReason(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-overlay")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitOverlayMissing,
		"opencode-overlay: pinned binary missing at /opencode/usr/local/bin/opencode")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("overlay_missing", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, cond)
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonOpencodeOverlayMissing), cond.Reason)
	require.Equal(t, before+1.0, testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("overlay_missing", "node-1", opencodeMetricLabel)))
}

func TestOpencodeVerify_FailureIsIdempotentPerEpisode(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-dedup")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed, "OpencodeVerificationFailed: expected=cccc got=eeee")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	for i := 0; i < 3; i++ {
		_, err := r.handleActive(context.Background(), ws)
		require.NoError(t, err)
	}
	after := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	require.Equal(t, before+1.0, after,
		"repeated reconciles of the same failure episode must not re-increment")
}

func TestOpencodeVerify_CrashloopFromOtherCause_DoesNotSetCondition(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-other")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", 137, "some other crash")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"non-opencode exit codes must not set the OpencodeVerified condition")
}

func TestOpencodeVerify_HealthyPod_SetsTrueCondition(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-ok")

	pod := makeWorkspacePod(ws, "", -1, "")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, cond, "overlay mode must surface a positive OpencodeVerified condition")
	require.Equal(t, "True", cond.Status)
	require.Equal(t, string(v1.ReasonOpencodeVerified), cond.Reason)
}

// TestOpencodeVerify_HealthyDualOverlayPod_SetsBothConditions: with both
// overlays enabled, a healthy pod publishes both verification conditions.
func TestOpencodeVerify_HealthyDualOverlayPod_SetsBothConditions(t *testing.T) {
	r := reconcilerWithBothOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-oc-both-ok")

	pod := makeWorkspacePod(ws, "", -1, "")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	oc := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, oc)
	require.Equal(t, "True", oc.Status)
	ag := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, ag)
	require.Equal(t, "True", ag.Status)
}

func TestOpencodeVerify_RecoveryNotEnteredOnVerifyFailure(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-norec")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed, "OpencodeVerificationFailed: expected=cccc got=eeee")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, v1.WorkspacePhaseActive, ws.Status.Phase,
		"a digest mismatch cannot be fixed by pod restarts — must not enter recovery/crashloop machinery")
	require.Equal(t, int32(0), ws.Status.RestartCount, "no restart may be counted")
}

// TestOpencodeVerify_CreatingPhase_FirstBootBadPinIsDetected mirrors the
// agentd regression: a bad pin at first boot/resume exits 83 before the
// container is ever Ready, so detection must fire from handleCreating
// too — otherwise an eternal Creating with no condition/event/metric.
func TestOpencodeVerify_CreatingPhase_FirstBootBadPinIsDetected(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := makeWorkspace("ws-oc-creating", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-ws-oc-creating"
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed,
		"OpencodeVerificationFailed: expected=cccc got=eeee")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	res, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, opencodeVerifyFailureRequeue, res.RequeueAfter,
		"verify failure in Creating must use the slow requeue, not the 2s fall-through")

	cond := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, cond, "condition must fire from handleCreating")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonOpencodeVerificationFailed), cond.Reason)
	after := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	require.Equal(t, before+1.0, after, "metric must fire from handleCreating")
}

// --- exit-code disambiguation on dual-overlay pods -------------------------

// TestOpencodeVerify_AgentdExit81_DoesNotSetOpencodeCondition: a
// dual-overlay pod whose agentd verify failed (exit 81) must attribute
// the failure to agentd ONLY — the opencode condition stays absent and
// the opencode metric stays untouched.
func TestOpencodeVerify_AgentdExit81_DoesNotSetOpencodeCondition(t *testing.T) {
	r := reconcilerWithBothOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-oc-agentd81")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed,
		"AgentdVerificationFailed: expected=aaaa got=cccc")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	ocBefore := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	ag := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, ag, "the agentd exit-81 must still be attributed to agentd")
	require.Equal(t, "False", ag.Status)
	require.Equal(t, string(v1.ReasonAgentdVerificationFailed), ag.Reason)

	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"agentd's exit 81 must never set the opencode condition — failure attribution must not cross artifacts")
	require.Equal(t, ocBefore, testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel)),
		"agentd's exit 81 must not increment the opencode metric")
}

// TestOpencodeVerify_OpencodeExit83_DoesNotSetAgentdCondition: the
// mirror — an opencode verify failure (exit 83) on a dual-overlay pod
// must not set the agentd condition or metric.
func TestOpencodeVerify_OpencodeExit83_DoesNotSetAgentdCondition(t *testing.T) {
	r := reconcilerWithBothOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-oc-exit83")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed,
		"OpencodeVerificationFailed: expected=cccc got=eeee")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	agBefore := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	oc := conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified)
	require.NotNil(t, oc, "the opencode exit-83 must be attributed to opencode")
	require.Equal(t, "False", oc.Status)
	require.Equal(t, string(v1.ReasonOpencodeVerificationFailed), oc.Reason)

	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified),
		"opencode's exit 83 must never set the agentd condition")
	require.Equal(t, agBefore, testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab")),
		"opencode's exit 83 must not increment the agentd metric")
}

// TestOpencodeVerify_AgentdOverlayMissing82_DoesNotSetOpencodeCondition:
// the overlay-missing direction of the same disambiguation.
func TestOpencodeVerify_AgentdOverlayMissing82_DoesNotSetOpencodeCondition(t *testing.T) {
	r := reconcilerWithBothOverlays(t, false)
	ws := activeOverlayWorkspace(t, r, "ws-oc-agentd82")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitOverlayMissing, "agentd-overlay missing")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	require.NotNil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified))
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"agentd's exit 82 must never set the opencode condition")
}

// --- legacy-pod gating ------------------------------------------------------

// TestOpencodeVerify_LegacyPodNeverVerified: during a rollout window the
// controller flag is on but pre-existing pods carry no opencode wiring —
// they must not be marked OpencodeVerified (the gate keys on the POD's
// env marker, not the controller flag).
func TestOpencodeVerify_LegacyPodNeverVerified(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-legacy-ok")
	pod := makeWorkspacePod(ws, "", -1, "")
	// Strip the agentd marker the fixture injects: pre-enable pod.
	pod.Spec.Containers[0].Env = nil
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"legacy pod must not be marked verified — nothing about it was ever verified")
}

// TestOpencodeVerify_LegacyPodExit83NotMisread: an unrelated process on
// a pod WITHOUT the opencode wiring exiting 83 must not be classified as
// an opencode verify failure.
func TestOpencodeVerify_LegacyPodExit83NotMisread(t *testing.T) {
	r := reconcilerWithOpencode(t)
	ws := activeOverlayWorkspace(t, r, "ws-oc-legacy-83")
	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed, "unrelated crash")
	// Keep only the agentd marker — the opencode overlay never wired this pod.
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	after := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	require.Equal(t, before, after, "legacy-pod exit-83 must not fire the verify-failure metric")
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified))
}

// TestOpencodeVerify_DeliveryDisabledOnReconciler_PodExit83Ignored is the
// rollout-DOWN leg of the gating matrix: opencodeDelivery disabled on the
// reconciler (flag off / rolled back) while the pod still carries the
// overlay wiring (OPENCODE_IMAGE_VOLUME=1) and the workspace container
// genuinely exited 83. No opencode condition, no event, no metric may
// fire — with delivery off, the controller has no OpencodeImage to
// attribute against, and an exit-83 report from a pod the controller no
// longer owns the contract for must not page anyone.
//
// This test was written FIRST and passes immediately against the current
// code — verified, not assumed: the gate at detectOpencodeVerification-
// Failure (opencode_overlay.go ~:165) is
// `!r.opencodeOverlayEnabled() || !podHasOpencodeOverlay(pod)`, i.e. it
// requires BOTH the reconciler config AND the pod marker, so the
// reconciler-disabled case returns false before any condition/event/
// metric is touched. The test exists to pin that conjunction: replacing
// the gate with a pod-marker-only check (e.g. "the pod says it has the
// overlay, trust it") makes this fail, restoring the rollout-down
// misattribution the #863 live-cluster finding class warns about.
func TestOpencodeVerify_DeliveryDisabledOnReconciler_PodExit83Ignored(t *testing.T) {
	r := reconcilerFor(t)
	r.Recorder = record.NewFakeRecorder(16)
	ws := activeOverlayWorkspace(t, r, "ws-oc-rollout-down")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", opencodeExitVerifyFailed,
		"OpencodeVerificationFailed: expected=cccc got=eeee binary=/opencode/usr/local/bin/opencode")
	require.NoError(t, r.Create(context.Background(), withOpencodeOverlayMarker(pod)))

	before := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	after := testutil.ToFloat64(metricsOpencodeVerifyFailures.WithLabelValues("verify_failed", "node-1", opencodeMetricLabel))
	require.Equal(t, before, after,
		"delivery disabled on the reconciler: exit-83 from a still-marked pod must not fire the verify-failure metric")
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionOpencodeVerified),
		"no OpencodeVerified condition when delivery is disabled on the reconciler")

	rec := r.Recorder.(*record.FakeRecorder)
	for {
		select {
		case e := <-rec.Events:
			require.NotContains(t, e, "OpencodeVerificationFailed",
				"no opencode warning event when delivery is disabled on the reconciler")
		default:
			return
		}
	}
}
