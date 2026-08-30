// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd-overlay-01: image-volume delivery wiring (#863).
//
// The controller pins a digest-addressed agentd image volume into every
// workspace pod when the operator enables agentdDelivery. The invariants
// these tests lock in:
//
//   - Disabled (default): no volume, no mount, no env — identical pod to
//     the legacy baked-in-agentd mode.
//   - Enabled: image volume with digest-pinned reference + IfNotPresent;
//     readOnly mount (load-bearing: an RW mount lets the main container
//     overwrite the binary and have it executed on container restart
//     within the same pod — init containers do not re-run); env pins
//     carrying the absolute overlay binary path and per-arch sha256s.
//   - Verify failures (entrypoint exit 81/82) set a condition, emit one
//     event per episode, increment the metric once per episode, and do
//     NOT enter the crashloop recovery path (restart cannot fix a
//     digest mismatch).
//   - Config validation: all-or-nothing, hex-format hashes, digest-pinned
//     reference.

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/require"
)

const (
	testAgentdImage    = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testAgentdSHAAMD64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAgentdSHAARM64 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func reconcilerWithAgentd(t *testing.T) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerFor(t)
	r.AgentdImage = testAgentdImage
	r.AgentdBinarySHA256AMD64 = testAgentdSHAAMD64
	r.AgentdBinarySHA256ARM64 = testAgentdSHAARM64
	r.Recorder = record.NewFakeRecorder(16)
	return r
}

// --- buildPod wiring ------------------------------------------------------

func TestAgentdOverlay_Enabled_WiresImageVolume(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "agentd" {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, vol, "agentd image volume must exist when delivery is enabled")
	require.NotNil(t, vol.Image, "volume must use an Image volume source")
	require.Equal(t, testAgentdImage, vol.Image.Reference,
		"reference must be the digest-pinned image")
	require.Equal(t, corev1.PullIfNotPresent, vol.Image.PullPolicy,
		"digest-pinned immutable content: IfNotPresent, never Always")
}

func TestAgentdOverlay_MountIsReadOnly(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	found := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "agentd" {
			found = true
			require.Equal(t, "/agentd", m.MountPath)
			require.True(t, m.ReadOnly,
				"agentd mount MUST be readOnly — an RW mount lets the main container swap the binary for the next container restart (init containers do not re-run within a pod)")
		}
	}
	require.True(t, found, "workspace container must mount the agentd volume")
}

func TestAgentdOverlay_EnvPinsBinaryPathAndBothArchHashes(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	require.Equal(t, "/agentd/usr/local/bin/workspace-agentd", env["LLMSAFESPACES_AGENTD_BINARY"],
		"absolute overlay path — never PATH lookup, never relative")
	require.Equal(t, testAgentdSHAAMD64, env["LLMSAFESPACES_AGENTD_SHA256_AMD64"])
	require.Equal(t, testAgentdSHAARM64, env["LLMSAFESPACES_AGENTD_SHA256_ARM64"],
		"per-arch hashes: the manifest list carries different binaries per arch")
}

// --- verify-failure detection (handleActive) ------------------------------

// activeOverlayWorkspace returns an Active-phase workspace fixture with the
// restart generation observed (so handleActive proceeds past the
// restart-generation branch) and the PVC pre-created.
func activeOverlayWorkspace(t *testing.T, r *WorkspaceReconciler, name string) *v1.Workspace {
	t.Helper()
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseActive)
	ws.Status.PVCName = "pvc-" + name
	ws.Status.ObservedRestartGeneration = ws.Spec.RestartGeneration
	ws.Status.StartTime = &metav1.Time{}
	require.NoError(t, r.Create(context.Background(), ws))
	// Password secret must exist or handleActive recycles the pod before
	// reaching the agentd verify detection.
	pw := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: passwordSecretName(name), Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("test-token")},
	}
	require.NoError(t, r.Create(context.Background(), pw))
	return ws
}

// makeWorkspacePod builds a workspace pod owned by ws. waitingReason
// controls cs.State ("" → Running); exit/termMsg control the last
// terminated state (exit < 0 → no terminated state).
func makeWorkspacePod(ws *v1.Workspace, waitingReason string, exit int32, termMsg string) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "workspace", RestartCount: 3}
	if waitingReason != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingReason}}
	} else {
		cs.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	}
	if exit >= 0 {
		cs.LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: termMsg},
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(ws.Name, string(ws.UID)),
			Namespace: ws.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "llmsafespaces.dev/v1", Kind: "Workspace", Name: ws.Name, UID: ws.UID},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			// The overlay wiring the real pod builder adds in the same
			// branch as the volume; detection/verification are gated on
			// it (see the legacy-pod regression tests below).
			Containers: []corev1.Container{{
				Name: "workspace",
				Env:  []corev1.EnvVar{{Name: "AGENTD_IMAGE_VOLUME", Value: "1"}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

func TestAgentdVerify_Mismatch_SetsConditionEmitsEventAndMetric(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-verify")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed,
		"AgentdVerificationFailed: expected=aaaa got=cccc binary=/agentd/usr/local/bin/workspace-agentd")
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, cond, "AgentdVerified condition must be set")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonAgentdVerificationFailed), cond.Reason)
	require.Contains(t, cond.Message, "expected=aaaa")
	require.Contains(t, cond.Message, "got=cccc")

	require.Equal(t, before+1.0, testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab")),
		"verify-failure metric must increment on detection")

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		require.Contains(t, e, "AgentdVerificationFailed")
		require.Contains(t, e, "expected=aaaa")
	default:
		t.Fatal("expected a warning event on the Workspace")
	}
}

func TestAgentdVerify_OverlayMissing_UsesOverlayReason(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-overlay")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitOverlayMissing,
		"agentd-overlay: pinned binary missing at /agentd/usr/local/bin/workspace-agentd")
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("overlay_missing", "node-1", "0123456789ab"))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, cond)
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonAgentdOverlayMissing), cond.Reason)
	require.Equal(t, before+1.0, testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("overlay_missing", "node-1", "0123456789ab")))
}

func TestAgentdVerify_FailureIsIdempotentPerEpisode(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-dedup")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed, "AgentdVerificationFailed: expected=aaaa got=cccc")
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	for i := 0; i < 3; i++ {
		_, err := r.handleActive(context.Background(), ws)
		require.NoError(t, err)
	}
	after := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	require.Equal(t, before+1.0, after,
		"repeated reconciles of the same failure episode must not re-increment")
}

func TestAgentdVerify_CrashloopFromOtherCause_DoesNotSetCondition(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-other")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", 137, "some other crash")
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified),
		"non-agentd exit codes must not set the AgentdVerified condition")
}

func TestAgentdVerify_HealthyPod_SetsTrueCondition(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-ok")

	pod := makeWorkspacePod(ws, "", -1, "")
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)

	cond := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, cond, "overlay mode must surface a positive AgentdVerified condition")
	require.Equal(t, "True", cond.Status)
}

func TestAgentdVerify_RecoveryNotEnteredOnVerifyFailure(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-norec")

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed, "AgentdVerificationFailed: expected=aaaa got=cccc")
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, v1.WorkspacePhaseActive, ws.Status.Phase,
		"a digest mismatch cannot be fixed by pod restarts — must not enter recovery/crashloop machinery")
	require.Equal(t, int32(0), ws.Status.RestartCount, "no restart may be counted")
}

// TestAgentdVerify_CreatingPhase_FirstBootBadPinIsDetected is the
// regression for the review finding that detection lived only in
// handleActive: a bad pin at first boot/resume exits 81 before the
// container is ever Ready, so the workspace sits in Creating with the
// Running-not-ready fall-through requeueing every 2s forever — no
// condition, no event, no metric, no page.
func TestAgentdVerify_CreatingPhase_FirstBootBadPinIsDetected(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := makeWorkspace("ws-creating", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "pvc-ws-creating"
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed,
		"AgentdVerificationFailed: expected=aaaa got=cccc")
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	res, err := r.handleCreating(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, agentdVerifyFailureRequeue, res.RequeueAfter,
		"verify failure in Creating must use the slow requeue, not the 2s fall-through")

	cond := conditionOfType(ws, v1.WorkspaceConditionAgentdVerified)
	require.NotNil(t, cond, "condition must fire from handleCreating")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonAgentdVerificationFailed), cond.Reason)
	after := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	require.Equal(t, before+1.0, after, "metric must fire from handleCreating")
}

// --- config validation ----------------------------------------------------

func TestAgentdValidateConfig(t *testing.T) {
	cases := []struct {
		name    string
		image   string
		amd64   string
		arm64   string
		wantErr string
	}{
		{"all empty (legacy)", "", "", "", ""},
		{"fully configured", testAgentdImage, testAgentdSHAAMD64, testAgentdSHAARM64, ""},
		// Renovate-friendly: image-only is the NORMAL pin form now —
		// hashes resolve from index annotations at startup.
		{"image only (renovate form)", testAgentdImage, "", "", ""},
		{"hashes only", "", testAgentdSHAAMD64, testAgentdSHAARM64, "image"},
		{"partial hash override", testAgentdImage, testAgentdSHAAMD64, "", "BOTH"},
		{"partial hash override (mirrored)", testAgentdImage, "", testAgentdSHAARM64, "BOTH"},
		{"short hash", testAgentdImage, "abc", testAgentdSHAARM64, "64 hex"},
		{"non-hex hash", testAgentdImage, strings.Repeat("z", 64), testAgentdSHAARM64, "64 hex"},
		{"tag not digest", "ghcr.io/x/agentd:v1", testAgentdSHAAMD64, testAgentdSHAARM64, "@sha256:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentdDeliveryConfig(tc.image, tc.amd64, tc.arm64)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// --- helpers --------------------------------------------------------------

func conditionOfType(ws *v1.Workspace, ct v1.WorkspaceConditionType) *v1.WorkspaceCondition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == ct {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

// TestAgentdVerify_LegacyPodNeverVerified is the live-cluster regression
// (#863 validation finding 2): during a rollout window the controller
// flag is on but pre-existing pods still run the baked binary. A
// healthy legacy pod must NOT get AgentdVerified=True — nothing about it
// was ever verified, and the condition is security-relevant.
func TestAgentdVerify_LegacyPodNeverVerified(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-legacy-ok")
	pod := makeWorkspacePod(ws, "", -1, "")
	// Strip the overlay wiring: pre-enable pod.
	for i := range pod.Spec.Containers[0].Env {
		if pod.Spec.Containers[0].Env[i].Name == "AGENTD_IMAGE_VOLUME" {
			pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env[:i], pod.Spec.Containers[0].Env[i+1:]...)
			break
		}
	}
	require.NoError(t, r.Create(context.Background(), pod))

	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified),
		"legacy pod must not be marked verified — the entrypoint never ran the overlay check")
}

// TestAgentdVerify_LegacyPodExit81NotMisread: an unrelated process on a
// legacy pod exiting 81 must not be classified as an agentd verify
// failure (the gate keys on the pod, not the exit code).
func TestAgentdVerify_LegacyPodExit81NotMisread(t *testing.T) {
	r := reconcilerWithAgentd(t)
	ws := activeOverlayWorkspace(t, r, "ws-legacy-81")
	pod := makeWorkspacePod(ws, "CrashLoopBackOff", agentdExitVerifyFailed, "unrelated crash")
	pod.Spec.Containers[0].Env = nil
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	_, err := r.handleActive(context.Background(), ws)
	require.NoError(t, err)
	after := testutil.ToFloat64(metricsAgentdVerifyFailures.WithLabelValues("verify_failed", "node-1", "0123456789ab"))
	require.Equal(t, before, after, "legacy-pod exit-81 must not fire the verify-failure metric")
	require.Nil(t, conditionOfType(ws, v1.WorkspaceConditionAgentdVerified))
}
