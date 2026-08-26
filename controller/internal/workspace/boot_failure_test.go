// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// boot_failure_test.go — design 0051 sidecar migration, step 1:
// platform-boot failure visibility (TDD: authored before the
// implementation).
//
// Post-migration, boot-phase failures (init-fs operational failure,
// materialize exit 2/3 in the sidecar's boot phase or the legacy
// platform-materialize init) present as CrashLoopBackOff on PLATFORM
// containers during the Creating phase. Pre-migration, the incident
// class (2026-08-25) surfaced as an eternal Creating workspace with no
// reason on the Workspace object.
//
// Invariants locked here (mirroring the #863 verify-failure pattern):
//
//   - A terminated-with-error state on a platform container (platform-
//     init, platform-bootstrap, platform-materialize, or the agentd
//     sidecar) sets BootReady=False with the container's message, emits
//     ONE warning event per episode, increments the metric, and returns
//     true so the caller skips crashloop recovery (a boot bug cannot be
//     fixed by deleting the pod).
//   - Non-platform containers (workspace main) never trip it.
//   - A healthy pod marks BootReady=True once.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// makeSidecarModePod builds a pod shaped like sidecar-mode buildPod
// output: platform-init init container + agentd native sidecar, with the
// given container's status terminated-with-error.
func makeSidecarModePod(ws *v1.Workspace, failedContainer string, exit int32, msg string) *corev1.Pod {
	term := &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg}
	var initStatuses, mainStatuses []corev1.ContainerStatus
	if failedContainer == "platform-init" {
		initStatuses = append(initStatuses, corev1.ContainerStatus{
			Name: "platform-init",
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			},
			LastTerminationState: corev1.ContainerState{Terminated: term},
			RestartCount:         3,
		})
	} else {
		initStatuses = append(initStatuses, corev1.ContainerStatus{
			Name:  "platform-init",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		})
	}
	if failedContainer == "agentd" {
		initStatuses = append(initStatuses, corev1.ContainerStatus{
			Name:                 "agentd",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: term},
			RestartCount:         2,
		})
	}
	mainStatuses = append(mainStatuses, corev1.ContainerStatus{
		Name:  "workspace",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
	})
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
			InitContainers: []corev1.Container{
				{Name: "platform-init", Image: testAgentdImage},
				{Name: "agentd", Image: testAgentdImage},
			},
			Containers: []corev1.Container{{Name: "workspace"}},
		},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodPending,
			InitContainerStatuses: initStatuses,
			ContainerStatuses:     mainStatuses,
		},
	}
}

// TestPlatformBootFailure_SetsConditionEmitsEventAndMetric: the sidecar
// crash-looping on a boot-phase failure surfaces as BootReady=False on
// the Workspace (not an eternal, reason-less Creating).
func TestPlatformBootFailure_SetsConditionEmitsEventAndMetric(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	ws := newWorkspaceForSecurity(t)
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeSidecarModePod(ws, "agentd", 2,
		"materialize: parsing /sandbox-runtime/rt/secrets.json: json: cannot unmarshal array")
	require.NoError(t, r.Create(context.Background(), pod))

	before := testutil.ToFloat64(metrics.WorkspacePlatformBootFailuresTotal.WithLabelValues("agentd", "node-1"))
	detected := r.detectPlatformBootFailure(context.Background(), ws, pod)
	require.True(t, detected, "sidecar boot failure must be detected")

	cond := conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady)
	require.NotNil(t, cond, "BootReady condition must be set")
	require.Equal(t, "False", cond.Status)
	require.Equal(t, string(v1.ReasonPlatformBootFailed), cond.Reason)
	require.Contains(t, cond.Message, "cannot unmarshal")

	require.Equal(t, before+1.0, testutil.ToFloat64(metrics.WorkspacePlatformBootFailuresTotal.WithLabelValues("agentd", "node-1")),
		"boot-failure metric increments once per episode")

	rec := r.Recorder.(*record.FakeRecorder)
	select {
	case e := <-rec.Events:
		require.Contains(t, e, "PlatformBootFailed")
	default:
		t.Fatal("expected a warning event on the Workspace")
	}
}

// TestPlatformBootFailure_InitContainerToo: platform-init failing
// (missing password source, unwritable fs) is the same visibility class.
func TestPlatformBootFailure_InitContainerToo(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	ws := newWorkspaceForSecurity(t)
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeSidecarModePod(ws, "platform-init", 1, "init-fs: password source: read /mnt/secrets/password/password: no such file")
	require.NoError(t, r.Create(context.Background(), pod))

	require.True(t, r.detectPlatformBootFailure(context.Background(), ws, pod))
	cond := conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady)
	require.NotNil(t, cond)
	require.Equal(t, "False", cond.Status)
	require.Contains(t, cond.Message, "password")
}

// TestPlatformBootFailure_HealthyPod_MarksTrue: a pod with all platform
// containers clean marks BootReady=True once, idempotently.
func TestPlatformBootFailure_HealthyPod_MarksTrue(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	ws := newWorkspaceForSecurity(t)
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeSidecarModePod(ws, "", 0, "")
	require.NoError(t, r.Create(context.Background(), pod))

	require.False(t, r.detectPlatformBootFailure(context.Background(), ws, pod))
	r.markBootReady(pod, ws)
	cond := conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady)
	require.NotNil(t, cond)
	require.Equal(t, "True", cond.Status)

	// Idempotent: second call does not flap the condition.
	r.markBootReady(pod, ws)
	cond = conditionOfTypeLocal(ws, v1.WorkspaceConditionBootReady)
	require.Equal(t, "True", cond.Status)
}

// TestPlatformBootFailure_MainContainerIgnored: a workspace-container
// crash is NOT a boot failure (it has its own health/recovery paths).
func TestPlatformBootFailure_MainContainerIgnored(t *testing.T) {
	r := reconcilerWithAgentdSidecar(t)
	ws := newWorkspaceForSecurity(t)
	require.NoError(t, r.Create(context.Background(), ws))

	pod := makeSidecarModePod(ws, "", 0, "")
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting:    &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		Terminated: nil,
	}
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137},
	}

	require.False(t, r.detectPlatformBootFailure(context.Background(), ws, pod),
		"main-container crashes must not trip BootReady")
}
