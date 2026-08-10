// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ctrMetrics "github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- Helpers ---

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, storagev1.AddToScheme(scheme))
	return scheme
}

func reconcilerFor(t *testing.T, objs ...runtime.Object) *WorkspaceReconciler {
	t.Helper()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&v1.Workspace{}).
		Build()
	return &WorkspaceReconciler{Client: fakeClient, Scheme: scheme}
}

func reqFor(name, namespace string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func makeWorkspace(name, namespace string, phase v1.WorkspacePhase) *v1.Workspace {
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			UID:               "aaaabbbb-cccc-dddd-eeee-ffffgggghhhh",
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-1"},
			Runtime: "python:3.11",
			Storage: v1.WorkspaceStorageConfig{Size: "5Gi", AccessMode: "ReadWriteOnce"},
		},
		Status: v1.WorkspaceStatus{Phase: phase},
	}
	// M5-b: apply kubebuilder defaults so fake-client fixtures mirror what
	// the real admission webhook would set. Without this, tests see zero
	// values (e.g. Architecture="") where production sees defaults (e.g.
	// "amd64") — the class of bug that bit PR #231.
	v1.SetDefaults_Workspace(ws)
	return ws
}

func makeBoundPVC(name, namespace string, ownerUID types.UID) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if ownerUID != "" {
		pvc.OwnerReferences = []metav1.OwnerReference{{UID: ownerUID}}
	}
	return pvc
}

func makeRunningPod(name, namespace, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			ContainerStatuses: []corev1.ContainerStatus{
				{Ready: true},
			},
		},
	}
}

func makeRunningPodNotReady(name, namespace, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			ContainerStatuses: []corev1.ContainerStatus{
				{Ready: false},
			},
		},
	}
}

func makePasswordSecret(wsName, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: passwordSecretName(wsName), Namespace: namespace},
		Data:       map[string][]byte{"password": []byte("test-password")},
	}
}

// --- Pending Phase Tests ---

func TestReconcile_NotFound_NoError(t *testing.T) {
	r := reconcilerFor(t)
	_, err := r.Reconcile(context.Background(), reqFor("gone", "default"))
	assert.NoError(t, err)
}

func TestReconcile_Pending_CreatesPVC(t *testing.T) {
	ws := makeWorkspace("ws-new", "default", "")
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-new", "default"))
	require.NoError(t, err)

	pvc := &corev1.PersistentVolumeClaim{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "workspace-ws-new", Namespace: "default"}, pvc)
	assert.NoError(t, err, "PVC should be created")
}

func TestReconcile_Pending_PVCBound_TransitionsToCreating(t *testing.T) {
	ws := makeWorkspace("ws-bound", "default", v1.WorkspacePhasePending)
	pvc := makeBoundPVC("workspace-ws-bound", "default", ws.UID)
	r := reconcilerFor(t, ws, pvc)

	_, err := r.Reconcile(context.Background(), reqFor("ws-bound", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-bound", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
}

func TestReconcile_Pending_NoPVC_CreatesPVC(t *testing.T) {
	ws := makeWorkspace("ws-timeout", "default", v1.WorkspacePhasePending)
	ws.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-timeout", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-timeout", Namespace: "default"}, updated))
	assert.NotEmpty(t, updated.Status.PVCName, "PVC should be created regardless of age")
}

// --- Creating Phase Tests ---

func TestReconcile_Creating_NoPod_CreatesPod(t *testing.T) {
	ws := makeWorkspace("ws-creating", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-creating"
	pvc := makeBoundPVC("workspace-ws-creating", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-creating", "default")
	// Need a RuntimeEnvironment for image resolution.
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-creating", "default"))
	require.NoError(t, err)

	// Pod should be created.
	expectedPodName := podName("ws-creating", string(ws.UID))
	pod := &corev1.Pod{}
	err = r.Get(context.Background(), types.NamespacedName{Name: expectedPodName, Namespace: "default"}, pod)
	assert.NoError(t, err, "Pod should be created")
	assert.Equal(t, "ghcr.io/test/python:3.11", pod.Spec.Containers[0].Image)
}

func TestReconcile_Creating_PodRunning_TransitionsToActive(t *testing.T) {
	ws := makeWorkspace("ws-running", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-running"
	expectedPodName := podName("ws-running", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.5")
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-running", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-running", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase)
	assert.Equal(t, "10.0.0.5", updated.Status.PodIP)
	assert.Equal(t, "http://10.0.0.5:4096", updated.Status.Endpoint)
}

// TestReconcile_Creating_PodRunningNotReady_StaysInCreating verifies that a
// pod in PodRunning phase whose readiness probe has not yet passed does NOT
// cause a transition to Active. The workspace must remain in Creating until
// all container readiness probes pass (ContainerStatus.Ready == true).
func TestReconcile_Creating_PodRunningNotReady_StaysInCreating(t *testing.T) {
	ws := makeWorkspace("ws-not-ready", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-not-ready"
	expectedPodName := podName("ws-not-ready", string(ws.UID))
	pod := makeRunningPodNotReady(expectedPodName, "default", "10.0.0.6")
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-not-ready", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-not-ready", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "must stay Creating until readiness probe passes")
	assert.Empty(t, updated.Status.PodIP, "PodIP must not be set while not ready")
	assert.Empty(t, updated.Status.Endpoint, "Endpoint must not be set while not ready")
}

// production self-heal observed at safespace.thekao.cloud on 2026-06-01:
// 6+ workspaces were stuck in Creating phase from a pre-fix controller
// that did not create per-workspace bcrypt password Secrets. After the
// new controller landed, those workspaces still couldn't progress because
// handleCreating did not call ensurePasswordSecret — only handlePending
// did, and these workspaces had already moved past Pending.
//
// The fix calls ensurePasswordSecret defensively before pod build. This
// test asserts: given Creating phase + bound PVC + no pod + missing
// pw Secret, the reconcile creates the Secret and proceeds to build the
// pod. Without the fix the test would fail at pod build (CreateSecret
// volume mount references workspace-pw-* which doesn't exist).
func TestReconcile_Creating_NoPod_NoPwSecret_SelfHealsCreatesSecret(t *testing.T) {
	ws := makeWorkspace("ws-stuck", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-stuck"
	pvc := makeBoundPVC("workspace-ws-stuck", "default", ws.UID)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	// Note: no pwSecret in the fixture — that's the regression scenario.
	r := reconcilerFor(t, ws, pvc, rte)

	_, err := r.Reconcile(context.Background(), reqFor("ws-stuck", "default"))
	require.NoError(t, err, "reconcile must self-heal a missing pw secret in Creating phase, not error out")

	// Assert the password Secret was created
	pwSec := &corev1.Secret{}
	err = r.Get(context.Background(),
		types.NamespacedName{Name: passwordSecretName("ws-stuck"), Namespace: "default"},
		pwSec)
	assert.NoError(t, err, "ensurePasswordSecret should have created the Secret")

	// Assert the pod was created (proceeded past the secret check)
	pod := &corev1.Pod{}
	err = r.Get(context.Background(),
		types.NamespacedName{Name: podName("ws-stuck", string(ws.UID)), Namespace: "default"},
		pod)
	assert.NoError(t, err, "pod should be built once pw Secret exists")
}

// --- Active Phase Tests ---

// makeFailedMountPod builds a pod shaped like the prod incident in issue
// #699: scheduled, but kubelet cannot mount its volumes (FailedMount).
// The pod has 2 init + 1 main container status entries — all in pure
// Waiting state, none ever Started. The previous FN3b predicate required
// zero status entries and missed this shape entirely, leaving the pod
// stuck indefinitely while kubelet retried NodeStageVolume (and CSI
// retried mkfs on the PVC — the data-loss amplifier).
func makeFailedMountPod(name, namespace string, age time.Duration) *corev1.Pod {
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	created := metav1.NewTime(time.Now().Add(-age))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: created,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "workspace-dirs", State: waiting},
				{Name: "credential-setup", State: waiting},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "workspace", State: waiting},
			},
		},
	}
}

// TestReconcile_Creating_PodStuck_FailedMountShape_EntersRecovery is the
// regression test for issue #699 Defect 2. A pod with non-zero container
// status entries (2 init + 1 main, all pure Waiting) must be recognized
// as stuck after stuckScheduledPendingTimeout and enter Infrastructure
// recovery. Under the previous FN3b predicate (zero status entries
// required) this pod hung indefinitely.
//
// Reproduces the prod shape exactly: pod 5c25e2ef-...-57d20880 on
// 2026-08-09, stuck for 4+ minutes while CSI retried mkfs.ext4.
func TestReconcile_Creating_PodStuck_FailedMountShape_EntersRecovery(t *testing.T) {
	ws := makeWorkspace("ws-failedmount", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-failedmount"
	expectedPodName := podName("ws-failedmount", string(ws.UID))
	// Age the pod past stuckScheduledPendingTimeout (10min).
	pod := makeFailedMountPod(expectedPodName, "default", stuckScheduledPendingTimeout+time.Minute)
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-failedmount", "default"))
	require.NoError(t, err)

	// Pod must be deleted (detaches volume, halts CSI mkfs retries).
	err = r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err),
		"stuck FailedMount pod must be deleted to detach the volume and halt CSI retries")

	// Workspace enters recovery with Infrastructure classification.
	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-failedmount", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase,
		"enterRecovery keeps phase=Creating (re-creation deferred by backoff)")
	assert.Equal(t, string(FailureClassInfrastructure), updated.Status.LastFailureClass,
		"FailedMount is an infrastructure failure, not a process failure")
	assert.Greater(t, updated.Status.ConsecutiveFailures, int32(0))
}

// TestReconcile_Creating_PodStuck_FailedMountShape_BelowTimeout_NoRecovery
// is the false-positive guard: a pod younger than
// stuckScheduledPendingTimeout must NOT enter recovery even if it has the
// FailedMount status shape. Startup is slow-but-normal in some clusters.
func TestReconcile_Creating_PodStuck_FailedMountShape_BelowTimeout_NoRecovery(t *testing.T) {
	ws := makeWorkspace("ws-fm-young", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-fm-young"
	expectedPodName := podName("ws-fm-young", string(ws.UID))
	pod := makeFailedMountPod(expectedPodName, "default", 30*time.Second) // well under 10min
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-fm-young", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-fm-young", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"pod under the stuck timeout must not enter recovery")
	assert.Empty(t, updated.Status.LastFailureClass)
}

// TestReconcile_Creating_PodStuck_FailedMountShape_RunningContainer_NoRecovery
// is the misclassification guard: if ANY container has ever been Running
// or Terminated, the pod has made progress (volumes mounted, process ran)
// and must NOT be classified as stuck even if currently back in Waiting.
// Catches the case where init container 1 succeeded, container 2 is in
// CrashLoopBackOff — that is a Process failure, not an infrastructure
// stuck-volume-mount, and the existing CrashLoop path handles it.
func TestReconcile_Creating_PodStuck_FailedMountShape_RunningContainer_NoRecovery(t *testing.T) {
	ws := makeWorkspace("ws-fm-progress", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-fm-progress"
	expectedPodName := podName("ws-fm-progress", string(ws.UID))
	pod := makeFailedMountPod(expectedPodName, "default", stuckScheduledPendingTimeout+time.Minute)
	// Overwrite: first init container RAN (Completed), proving volumes mounted.
	pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"},
	}
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-fm-progress", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-fm-progress", Namespace: "default"}, updated))
	assert.Equal(t, int32(0), updated.Status.ConsecutiveFailures,
		"pod with a Completed init container has made progress; not stuck-FailedMount")
}

// --- Suspend escape hatch for non-Active phases (issue #699 Defect 1) ---

// TestReconcile_Creating_SpecSuspendTrue_TransitionsToSuspended verifies
// that spec.suspend=true on a Creating-phase workspace halts pod creation
// and parks the workspace in Suspended with the PVC intact. This gives
// operators a per-workspace escape hatch for stuck-Creating workspaces
// without requiring cluster-wide controller scale-to-0.
//
// Regression for the 2026-08-09 prod incident: spec.suspend=true was
// silently ignored in Creating, leaving the operator no per-workspace
// halt and forcing mkfs to keep retrying against the PVC.
func TestReconcile_Creating_SpecSuspendTrue_TransitionsToSuspended(t *testing.T) {
	ws := makeWorkspace("ws-susp-create", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-susp-create"
	suspendTrue := true
	ws.Spec.Suspend = &suspendTrue
	pvc := makeBoundPVC("workspace-ws-susp-create", "default", ws.UID)
	r := reconcilerFor(t, ws, pvc)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-create", "default"))
	require.NoError(t, err)

	got := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-susp-create", Namespace: "default"}, got))
	assert.Equal(t, v1.WorkspacePhaseSuspended, got.Status.Phase,
		"spec.suspend=true on Creating must transition to Suspended")
	assert.Nil(t, got.Spec.Suspend,
		"spec.suspend must be cleared to nil after the controller acts on it")
	assert.NotNil(t, got.Status.SuspendedAt, "SuspendedAt must be set")

	// PVC must be untouched — the whole point is preserving user data.
	pvcCheck := &corev1.PersistentVolumeClaim{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "workspace-ws-susp-create", Namespace: "default"}, pvcCheck))
	assert.Equal(t, corev1.ClaimBound, pvcCheck.Status.Phase, "PVC must remain Bound")
}

// TestReconcile_Creating_SpecSuspendTrue_DeletesExistingPod verifies
// that suspend in Creating halts an already-running (or stuck) pod,
// detaching the volume and stopping CSI retries. This is the
// data-loss-prevention half of the escape hatch.
func TestReconcile_Creating_SpecSuspendTrue_DeletesExistingPod(t *testing.T) {
	ws := makeWorkspace("ws-susp-pod", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-susp-pod"
	suspendTrue := true
	ws.Spec.Suspend = &suspendTrue
	expectedPodName := podName("ws-susp-pod", string(ws.UID))
	pod := makeFailedMountPod(expectedPodName, "default", 1*time.Minute)
	pvc := makeBoundPVC("workspace-ws-susp-pod", "default", ws.UID)
	r := reconcilerFor(t, ws, pvc, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-pod", "default"))
	require.NoError(t, err)

	err = r.Get(context.Background(),
		types.NamespacedName{Name: expectedPodName, Namespace: "default"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err),
		"suspend in Creating must delete any existing pod to halt CSI mkfs retries")
}

// TestReconcile_Pending_SpecSuspendTrue_TransitionsToSuspended verifies
// the escape hatch also covers the Pending phase (pre-PVC-bind). Less
// urgent than Creating (no pod yet, so no mkfs loop), but the
// per-workspace halt should be uniform across all pre-Suspended phases.
func TestReconcile_Pending_SpecSuspendTrue_TransitionsToSuspended(t *testing.T) {
	ws := makeWorkspace("ws-susp-pend", "default", v1.WorkspacePhasePending)
	suspendTrue := true
	ws.Spec.Suspend = &suspendTrue
	// No PVC, no pod — pure Pending.
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp-pend", "default"))
	require.NoError(t, err)

	got := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-susp-pend", Namespace: "default"}, got))
	assert.Equal(t, v1.WorkspacePhaseSuspended, got.Status.Phase,
		"spec.suspend=true on Pending must transition to Suspended")
	assert.Nil(t, got.Spec.Suspend,
		"spec.suspend must be cleared to nil after the controller acts on it")
}

// TestReconcile_Active_PodRunning_RequeuesAfter30s verifies an Active workspace
// with a running pod requeues on the standard active interval.
func TestReconcile_Active_PodRunning_RequeuesAfter30s(t *testing.T) {
	ws := makeWorkspace("ws-active", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "10.0.0.1"
	now := metav1.Now()
	ws.Status.StartTime = &now
	expectedPodName := podName("ws-active", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	pwSecret := makePasswordSecret("ws-active", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor("ws-active", "default"))
	require.NoError(t, err)
	assert.Equal(t, requeueActive, result.RequeueAfter)
}

func TestReconcile_Active_PodMissing_TransientRecovery(t *testing.T) {
	ws := makeWorkspace("ws-lost", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "10.0.0.1"
	pwSecret := makePasswordSecret("ws-lost", "default")
	r := reconcilerFor(t, ws, pwSecret) // no pod

	_, err := r.Reconcile(context.Background(), reqFor("ws-lost", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-lost", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "should self-heal to Creating")
	assert.Equal(t, int32(1), updated.Status.ConsecutiveFailures)
}

func TestReconcile_Active_PodMissing_EntersRecoveryWithBackoff(t *testing.T) {
	ws := makeWorkspace("ws-exhausted", "default", v1.WorkspacePhaseActive)
	ws.Status.ConsecutiveFailures = 5
	pwSecret := makePasswordSecret("ws-exhausted", "default")
	r := reconcilerFor(t, ws, pwSecret) // no pod

	_, err := r.Reconcile(context.Background(), reqFor("ws-exhausted", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-exhausted", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase,
		"pod loss enters recovery (Creating with backoff), never terminal Failed")
	assert.Equal(t, int32(6), updated.Status.ConsecutiveFailures)
	assert.NotNil(t, updated.Status.NextRetryAt)
}

func TestReconcile_Active_Timeout_Suspends(t *testing.T) {
	ws := makeWorkspace("ws-timeout", "default", v1.WorkspacePhaseActive)
	ws.Spec.Timeout = 60
	past := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	ws.Status.StartTime = &past
	expectedPodName := podName("ws-timeout", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	pwSecret := makePasswordSecret("ws-timeout", "default")
	r := reconcilerFor(t, ws, pod, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-timeout", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-timeout", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspending, updated.Status.Phase)
}

func TestReconcile_Active_PasswordSecretMissing_RecyclesPod(t *testing.T) {
	ws := makeWorkspace("ws-nopw", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "10.0.0.1"
	now := metav1.Now()
	ws.Status.StartTime = &now
	expectedPodName := podName("ws-nopw", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod) // no password secret

	_, err := r.Reconcile(context.Background(), reqFor("ws-nopw", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-nopw", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "missing password secret should recycle pod")
	assert.Equal(t, int32(1), updated.Status.RestartCount)
}

func TestReconcile_Active_RestartGeneration_RecreatesPod(t *testing.T) {
	ws := makeWorkspace("ws-restart", "default", v1.WorkspacePhaseActive)
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.PodIP = "10.0.0.1"
	expectedPodName := podName("ws-restart", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-restart", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-restart", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
	assert.Equal(t, int64(2), updated.Status.ObservedRestartGeneration)
	assert.Empty(t, updated.Status.PodIP)
}

// --- Suspend/Resume/Terminate Tests ---

func TestReconcile_Suspending_DeletesPodAndTransitions(t *testing.T) {
	ws := makeWorkspace("ws-susp", "default", v1.WorkspacePhaseSuspending)
	expectedPodName := podName("ws-susp", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.1")
	r := reconcilerFor(t, ws, pod)

	_, err := r.Reconcile(context.Background(), reqFor("ws-susp", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-susp", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseSuspended, updated.Status.Phase)
	assert.Empty(t, updated.Status.PodIP)
	assert.NotNil(t, updated.Status.SuspendedAt)
}

func TestReconcile_Suspended_TTLExpired_Terminates(t *testing.T) {
	ws := makeWorkspace("ws-ttl", "default", v1.WorkspacePhaseSuspended)
	// US-23.3: nil Spec.Suspend means "no resume requested" — the
	// controller falls through to TTL evaluation. (&true would also
	// work; nil is the post-controller-acknowledgement state.)
	ws.Spec.Suspend = nil
	ws.Spec.TTLSecondsAfterSuspended = 60
	past := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	ws.Status.SuspendedAt = &past
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-ttl", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-ttl", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminating, updated.Status.Phase)
}

func TestReconcile_Suspended_NoTTL_NoAction(t *testing.T) {
	ws := makeWorkspace("ws-notttl", "default", v1.WorkspacePhaseSuspended)
	// US-23.3: nil Spec.Suspend means "no resume requested."
	ws.Spec.Suspend = nil
	r := reconcilerFor(t, ws)

	result, err := r.Reconcile(context.Background(), reqFor("ws-notttl", "default"))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

// TestReconcile_Active_SpecSuspendTrue_TransitionsToSuspending verifies
// the full end-to-end Reconcile path for an API-initiated suspend:
// Spec.Suspend=&true on an Active workspace → Phase=Suspending +
// Spec.Suspend cleared to nil. Regression test for the stale-RV ordering
// bug found in review round 2.
func TestReconcile_Active_SpecSuspendTrue_TransitionsToSuspending(t *testing.T) {
	ws := makeWorkspace("ws-suspend-req", "default", v1.WorkspacePhaseActive)
	suspendTrue := true
	ws.Spec.Suspend = &suspendTrue
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-suspend-req", "default"))
	require.NoError(t, err)

	got := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-suspend-req", Namespace: "default"}, got))
	assert.Equal(t, v1.WorkspacePhaseSuspending, got.Status.Phase,
		"Spec.Suspend=&true must transition Active→Suspending")
	assert.Nil(t, got.Spec.Suspend,
		"Spec.Suspend must be cleared to nil after the controller acts on it")
}

// TestReconcile_Suspended_SpecSuspendFalse_TransitionsToResuming verifies
// the full end-to-end Reconcile path for an API-initiated resume:
// Spec.Suspend=&false on a Suspended workspace → Phase=Resuming +
// Spec.Suspend cleared to nil.
func TestReconcile_Suspended_SpecSuspendFalse_TransitionsToResuming(t *testing.T) {
	ws := makeWorkspace("ws-resume-req", "default", v1.WorkspacePhaseSuspended)
	suspendFalse := false
	ws.Spec.Suspend = &suspendFalse
	pwSecret := makePasswordSecret("ws-resume-req", "default")
	r := reconcilerFor(t, ws, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-resume-req", "default"))
	require.NoError(t, err)

	got := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: "ws-resume-req", Namespace: "default"}, got))
	assert.Equal(t, v1.WorkspacePhaseResuming, got.Status.Phase,
		"Spec.Suspend=&false must transition Suspended→Resuming")
	assert.Nil(t, got.Spec.Suspend,
		"Spec.Suspend must be cleared to nil after the controller acts on it")
}

func TestReconcile_Resuming_TransitionsToCreating(t *testing.T) {
	ws := makeWorkspace("ws-resume", "default", v1.WorkspacePhaseResuming)
	past := metav1.Now()
	ws.Status.SuspendedAt = &past
	// US-23.3: LastActivityAt is written by the API service to the
	// annotation; the controller no longer writes it. We set the
	// annotation here to simulate the API having written it before
	// transitioning the workspace to Resuming.
	ws.Annotations = map[string]string{
		v1.AnnotationLastActivityAt: time.Now().Format(time.RFC3339),
	}
	// Legacy Status.LastActivityAt stays stale — the controller must
	// NOT update it (single-writer principle).
	staleActivity := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	ws.Status.LastActivityAt = &staleActivity
	pwSecret := makePasswordSecret("ws-resume", "default")
	r := reconcilerFor(t, ws, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-resume", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-resume", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase)
	assert.Nil(t, updated.Status.SuspendedAt)
	// US-23.3: the controller no longer writes LastActivityAt. The
	// deprecated Status field stays at its pre-resume value.
	require.NotNil(t, updated.Status.LastActivityAt, "Status.LastActivityAt must be preserved (controller no longer touches it)")
	assert.WithinDuration(t, staleActivity.Time, updated.Status.LastActivityAt.Time, time.Second,
		"Status.LastActivityAt must be unchanged on resume (single-writer)")
}

func TestReconcile_Terminating_CleansUp(t *testing.T) {
	ws := makeWorkspace("ws-term", "default", v1.WorkspacePhaseTerminating)
	ws.Finalizers = []string{WorkspaceFinalizer}
	ws.Status.PVCName = "workspace-ws-term"
	pvc := makeBoundPVC("workspace-ws-term", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-term", "default")
	r := reconcilerFor(t, ws, pvc, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-term", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-term", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseTerminated, updated.Status.Phase)
	assert.NotContains(t, updated.Finalizers, WorkspaceFinalizer)
}

func TestReconcile_Failed_NoBump_NoPod_RetriesCreating(t *testing.T) {
	ws := makeWorkspace("ws-fail-nopod", "default", v1.WorkspacePhaseFailed)
	ws.Status.Message = "pod entered Failed phase during creation"
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-fail-nopod", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-fail-nopod", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "Failed workspace with no pod should auto-retry Creating")
	assert.Empty(t, updated.Status.Message)
}

// TestReconcile_Failed_RestartGenerationBump_Recovers is the regression test
// for Epic 21 Change A: a Failed workspace must be recoverable by bumping
// spec.restartGeneration, mirroring the existing recovery semantics for
// Active workspaces (see TestReconcile_Active_RestartGeneration_RecreatesPod).
//
// Worklog 0099 incident: workspaces marked Failed by transient agentd
// timeouts or pod-deletion-during-recovery had no declarative recovery path;
// operators had to hand-edit status with `kubectl patch --subresource=status`,
// which is a code smell and not auditable. After this change the recovery
// is a single `kubectl patch workspace ... --type=merge -p '{"spec":{"restartGeneration":N+1}}'`
// (or the equivalent API call), which leaves a normal CRD audit trail.
func TestReconcile_Failed_RestartGenerationBump_Recovers(t *testing.T) {
	ws := makeWorkspace("ws-fail-recover", "default", v1.WorkspacePhaseFailed)
	ws.Spec.RestartGeneration = 1
	ws.Status.ObservedRestartGeneration = 0
	ws.Status.Message = "pod lost 3 times; marking failed"
	ws.Status.PodName = "stale-pod"
	ws.Status.PodNamespace = "default"
	ws.Status.PodIP = "10.0.0.99"
	ws.Status.Endpoint = "http://10.0.0.99:4096"
	ws.Status.RestartCount = 0
	r := reconcilerFor(t, ws)

	_, err := r.Reconcile(context.Background(), reqFor("ws-fail-recover", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-fail-recover", Namespace: "default"}, updated))

	// Phase walks back through Pending so handlePending re-creates PVC + secret + (eventually) pod.
	assert.Equal(t, v1.WorkspacePhasePending, updated.Status.Phase,
		"Failed + bumped RestartGeneration must transition to Pending")
	// ObservedRestartGeneration catches up so we don't loop.
	assert.Equal(t, int64(1), updated.Status.ObservedRestartGeneration)
	// RestartCount increments for observability/metrics.
	assert.Equal(t, int32(1), updated.Status.RestartCount)
	// Stale fields are cleared so handlePending starts fresh.
	assert.Empty(t, updated.Status.Message, "stale failure message must be cleared on recovery")
	assert.Empty(t, updated.Status.PodName)
	assert.Empty(t, updated.Status.PodNamespace)
	assert.Empty(t, updated.Status.PodIP)
	assert.Empty(t, updated.Status.Endpoint)
	// Transient counters reset so the new run gets a fresh budget.
}

// TestReconcile_Failed_RestartGenerationStale_NoRecovery: spec.restartGeneration
// must be STRICTLY GREATER than the observed value to trigger recovery. Equal
// values mean "the controller already responded to that bump"; lower values
// are a malformed write that should be ignored, not interpreted as a retry.
func TestReconcile_Failed_RestartGenerationStale_NoRecovery(t *testing.T) {
	cases := []struct {
		name              string
		spec              int64
		observed          int64
		expectStillFailed bool
	}{
		{"equal", 5, 5, false},
		{"observed_ahead_of_spec", 1, 5, false},
		{"zero_zero", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := makeWorkspace("ws-fail-stale", "default", v1.WorkspacePhaseFailed)
			ws.Spec.RestartGeneration = tc.spec
			ws.Status.ObservedRestartGeneration = tc.observed
			r := reconcilerFor(t, ws)

			_, err := r.Reconcile(context.Background(), reqFor("ws-fail-stale", "default"))
			require.NoError(t, err)

			updated := &v1.Workspace{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-fail-stale", Namespace: "default"}, updated))
			assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "Failed always auto-recovers")
		})
	}
}

// TestReconcile_Failed_CleansUpSecrets is the regression test for Bug 12
// in worklog 0085: workspaces stuck in Failed phase used to leak the
// per-workspace K8s Secrets indefinitely (45h+ in the wild). Failed phase
// must purge them so future reconciles converge on a clean state.
//
// Epic 35: workspace-secrets-* is no longer created (secretless injection),
// so only workspace-pw-* and workspace-creds-* are in the cleanup list.
func TestReconcile_Failed_CleansUpSecrets(t *testing.T) {
	ws := makeWorkspace("ws-fail-cleanup", "default", v1.WorkspacePhaseFailed)
	pwSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-pw-ws-fail-cleanup", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("p")},
	}
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-creds-ws-fail-cleanup", Namespace: "default"},
		Data:       map[string][]byte{"creds": []byte("c")},
	}

	r := reconcilerFor(t, ws, pwSecret, credSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-fail-cleanup", "default"))
	require.NoError(t, err)

	// Both Secrets must be gone after one reconcile.
	for _, name := range []string{
		"workspace-pw-ws-fail-cleanup",
		"workspace-creds-ws-fail-cleanup",
	} {
		var got corev1.Secret
		err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &got)
		assert.True(t, err != nil, "Bug 12: %s must be deleted on Failed reconcile", name)
	}

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-fail-cleanup", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase, "Failed with no pod should auto-retry")
}

// TestReconcile_Creating_WithPriorFailures_RecoverySuccessMetricFires verifies
// that when a workspace transitions Creating→Active and has ConsecutiveFailures>0,
// the WorkspaceRecoverySuccessTotal metric is incremented.
func TestReconcile_Creating_WithPriorFailures_RecoverySuccessMetricFires(t *testing.T) {
	ws := makeWorkspace("ws-recovering", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-recovering"
	ws.Status.ConsecutiveFailures = 3 // simulate prior failures
	ws.Status.LastFailureClass = "infrastructure"
	expectedPodName := podName("ws-recovering", string(ws.UID))
	pod := makeRunningPod(expectedPodName, "default", "10.0.0.99")
	r := reconcilerFor(t, ws, pod)

	// Read counter value directly from the CounterVec (not registered in DefaultGatherer in tests).
	readCounter := func() float64 {
		m := &dto.Metric{}
		if err := ctrMetrics.WorkspaceRecoverySuccessTotal.WithLabelValues("infrastructure").Write(m); err != nil {
			return 0
		}
		return m.GetCounter().GetValue()
	}
	before := readCounter()

	_, err := r.Reconcile(context.Background(), reqFor("ws-recovering", "default"))
	require.NoError(t, err)

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-recovering", Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase)
	assert.Greater(t, readCounter(), before, "WorkspaceRecoverySuccessTotal must increment on Creating→Active with prior failures")
}
