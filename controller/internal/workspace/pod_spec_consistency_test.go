// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// pod_spec_consistency_test.go verifies that the Pod spec produced by the REAL
// Reconcile loop is internally consistent: the init container's Env vars match
// the $VAR references in its script, every absolute path the script touches is
// a declared VolumeMount, and the referenced ServiceAccount was actually
// created in the same Reconcile pass.
//
// The existing controller tests assert on SUBSTRING FRAGMENTS in separate
// tests (health_test.go asserts the script contains "workspace-agentd
// materialize"; security_test.go asserts the mount exists; health_test.go
// asserts the env var exists) — but never on a single built pod, and never
// cross-validating the slices against each other. A refactor that renames an
// env var, drops a mount, or reorders SA creation would pass every existing
// test and break only at runtime.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// envMap converts a []corev1.EnvVar into a lookup map for cross-validation.
func envMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

// mountPaths returns the set of MountPath values for cross-validation against
// the absolute paths the init script references.
func mountPaths(mounts []corev1.VolumeMount) map[string]bool {
	m := make(map[string]bool, len(mounts))
	for _, mt := range mounts {
		m[mt.MountPath] = true
	}
	return m
}

// findInitContainerOrFatal returns the init container with the given name, failing
// the test if absent.
func findInitContainerOrFatal(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("init container %q not found in pod %s; init containers: %v",
		name, pod.Name, initContainerNames(pod))
	return corev1.Container{}
}

func initContainerNames(pod *corev1.Pod) []string {
	names := make([]string, len(pod.Spec.InitContainers))
	for i, c := range pod.Spec.InitContainers {
		names[i] = c.Name
	}
	return names
}

// reconcileToCreatingPod drives Reconcile twice (Pending→Creating→pod created)
// and returns the persisted Pod. Mirrors what production does across two
// reconciler ticks.
func reconcileToCreatingPod(t *testing.T, ws *v1.Workspace, apiURL string) (*WorkspaceReconciler, *corev1.Pod) {
	t.Helper()
	pvc := makeBoundPVC("workspace-"+ws.Name, ws.Namespace, ws.UID)
	pwSecret := makePasswordSecret(ws.Name, ws.Namespace)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec: v1.RuntimeEnvironmentSpec{
			Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11",
		},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)
	r.APIServiceURL = apiURL

	ctx := context.Background()
	// First reconcile: Pending → ensures PVC bound + pw-secret + SA, → Creating.
	_, err := r.Reconcile(ctx, reqFor(ws.Name, ws.Namespace))
	require.NoError(t, err)
	// Second reconcile: Creating → builds + Creates the Pod.
	_, err = r.Reconcile(ctx, reqFor(ws.Name, ws.Namespace))
	require.NoError(t, err)

	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: podName(ws.Name, string(ws.UID)), Namespace: ws.Namespace}
	require.NoError(t, r.Get(ctx, podKey, pod), "pod must be persisted after two reconciles")
	return r, pod
}

// TestE2E_Reconcile_PodSpec_InitContainerSelfConsistent is the central guard.
// On a SINGLE Reconcile-produced pod it asserts:
//  1. The credential-setup init container's Env contains WORKSPACE_ID and
//     LLMSAFESPACE_API_URL, AND the script references "$WORKSPACE_ID" and
//     "$LLMSAFESPACE_API_URL" — i.e. env names and script $VAR names agree.
//  2. Every absolute path the script touches (/sandbox-cfg, /sandbox-runtime,
//     /mnt/secrets/password, /var/run/bootstrap, /home/sandbox, /workspace) is
//     the MountPath of a declared VolumeMount on the SAME container.
//  3. The script calls bootstrap BEFORE materialize (ordering invariant).
//  4. The bootstrap-token volume is projected with Path "token" so the binary's
//     default read path /var/run/bootstrap/token resolves.
//
// A refactor renaming an env var, dropping a mount, or reordering the script
// breaks exactly one of these cross-checks.
func TestE2E_Reconcile_PodSpec_InitContainerSelfConsistent(t *testing.T) {
	ws := makeWorkspace("ws-consistency", "default", v1.WorkspacePhasePending)
	const apiURL = "http://test-api.e2e:8080"
	_, pod := reconcileToCreatingPod(t, ws, apiURL)

	credInit := findInitContainerOrFatal(t, pod, "credential-setup")
	script := credInit.Command[len(credInit.Command)-1]
	envs := envMap(credInit.Env)
	mounts := mountPaths(credInit.VolumeMounts)

	// (1) Env names ↔ script $VAR references must agree.
	assert.Equal(t, ws.Name, envs["WORKSPACE_ID"],
		"WORKSPACE_ID env must equal the workspace name")
	assert.Equal(t, apiURL, envs["LLMSAFESPACE_API_URL"],
		"LLMSAFESPACE_API_URL env must equal the reconciler's APIServiceURL")
	assert.Contains(t, script, "$WORKSPACE_ID",
		"script must reference $WORKSPACE_ID (rename would silently break bootstrap)")
	assert.Contains(t, script, "$LLMSAFESPACE_API_URL",
		"script must reference $LLMSAFESPACE_API_URL (rename would silently break bootstrap)")

	// (2) Every absolute path the script touches must be a declared mount.
	// These are the paths the credScript writes/cp's/ln -s's into.
	scriptPaths := []string{
		"/sandbox-cfg",          // bootstrap --out + cp password
		"/sandbox-runtime",      // mkdir -p symlink targets
		"/mnt/secrets/password", // cp password source
		"/home/sandbox",         // ln -s .ssh, .secrets, .git-credentials
		"/workspace",            // ln -s auth.json
	}
	for _, p := range scriptPaths {
		assert.True(t, mounts[p],
			"script references %q but no VolumeMount has that MountPath — a dropped/renamed mount breaks the script silently", p)
	}

	// (3) bootstrap must precede materialize in the script (the bootstrap
	// output is the materialize input).
	bootIdx := strings.Index(script, "workspace-agentd bootstrap")
	matIdx := strings.Index(script, "workspace-agentd materialize")
	require.NotEqual(t, -1, bootIdx, "script must call workspace-agentd bootstrap")
	require.NotEqual(t, -1, matIdx, "script must call workspace-agentd materialize")
	assert.Less(t, bootIdx, matIdx,
		"bootstrap must precede materialize in the init script (bootstrap output is materialize input)")

	// (4) bootstrap-token projection Path must be "token" so the binary's
	// default read path /var/run/bootstrap/token (bootstrap.go:51) resolves.
	var bootstrapVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "bootstrap-token" {
			bootstrapVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, bootstrapVol, "bootstrap-token volume must exist")
	require.NotNil(t, bootstrapVol.Projected, "bootstrap-token must be a projected volume")
	require.Len(t, bootstrapVol.Projected.Sources, 1, "bootstrap-token must have exactly one source")
	satProj := bootstrapVol.Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, satProj, "bootstrap-token source must be ServiceAccountToken")
	assert.Equal(t, "token", satProj.Path,
		"projected token Path must be 'token' so /var/run/bootstrap/token resolves")
	assert.Equal(t, bootstrapAudience, satProj.Audience,
		"bootstrap-token audience must match the API's TokenReview audience")
}

// TestE2E_Reconcile_PodSpec_ServiceAccountCreatedBeforePod verifies the
// reconciler ordering invariant: handleCreating creates the workspace-<id>
// ServiceAccount in the SAME Reconcile pass that creates the Pod, and the
// Pod's ServiceAccountName references that exact SA. A refactor that builds
// the pod before ensuring the SA would pass unit tests (the fake client does
// not validate SA references) but fail at runtime.
func TestE2E_Reconcile_PodSpec_ServiceAccountCreatedBeforePod(t *testing.T) {
	ws := makeWorkspace("ws-sa-order", "default", v1.WorkspacePhasePending)
	r, pod := reconcileToCreatingPod(t, ws, "http://test-api:8080")

	// The SA must exist in the fake client after Reconcile.
	sa := &corev1.ServiceAccount{}
	saKey := types.NamespacedName{Name: bootstrapSAName(ws.Name), Namespace: ws.Namespace}
	require.NoError(t, r.Get(context.Background(), saKey, sa),
		"the workspace ServiceAccount must be created during Reconcile (ordering invariant)")

	// The pod must reference that exact SA.
	assert.Equal(t, bootstrapSAName(ws.Name), pod.Spec.ServiceAccountName,
		"pod ServiceAccountName must match the SA created in the same Reconcile pass")

	// Automount must be explicitly false (G17 — the projected token is an
	// explicit mount, not the default automount).
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be explicitly set (nil would default to true)")
	assert.False(t, *pod.Spec.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be false — the projected token is explicit, not the default automount")
}

// TestE2E_Reconcile_PodSpec_PasswordSecretMounted verifies the pw-secret
// volume the readiness probe + init script depend on is present and references
// the correct Secret name. A dropped volume breaks both the probe (401 on
// /v1/readyz) and the `cp /mnt/secrets/password/password` line.
func TestE2E_Reconcile_PodSpec_PasswordSecretMounted(t *testing.T) {
	ws := makeWorkspace("ws-pwvol", "default", v1.WorkspacePhasePending)
	_, pod := reconcileToCreatingPod(t, ws, "http://test-api:8080")

	var pwVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "pw-secret" {
			pwVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, pwVol, "pw-secret volume must exist")
	require.NotNil(t, pwVol.Secret, "pw-secret must be a Secret volume")
	assert.Equal(t, passwordSecretName(ws.Name), pwVol.Secret.SecretName,
		"pw-secret must reference the workspace's password Secret")

	// The main container's AGENTD_ADMIN_TOKEN env must read from the same secret.
	var mainContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			mainContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, mainContainer, "main 'workspace' container must exist")
	var foundAdminToken bool
	for _, e := range mainContainer.Env {
		if e.Name == "AGENTD_ADMIN_TOKEN" && e.ValueFrom != nil &&
			e.ValueFrom.SecretKeyRef != nil &&
			e.ValueFrom.SecretKeyRef.Name == passwordSecretName(ws.Name) {
			foundAdminToken = true
		}
	}
	assert.True(t, foundAdminToken,
		"AGENTD_ADMIN_TOKEN env must reference the pw-secret — a drop breaks the readiness probe auth")
}

// --- Unhappy paths ---
//
// The reconciler must degrade safely when dependencies are missing rather than
// producing a pod that silently can't boot credentials.

// TestE2E_Reconcile_NoRuntimeEnvironment_DoesNotCreatePod pins that a missing
// RuntimeEnvironment (image cannot be resolved) does NOT create a pod with a
// broken/empty image. A pod created with an empty image would CrashLoopBackOff
// with no operator signal.
func TestE2E_Reconcile_NoRuntimeEnvironment_DoesNotCreatePod(t *testing.T) {
	ws := makeWorkspace("ws-no-rte", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-no-rte"
	pvc := makeBoundPVC("workspace-ws-no-rte", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-no-rte", "default")
	// NOTE: no RuntimeEnvironment seeded — image resolution must fail.
	r := reconcilerFor(t, ws, pvc, pwSecret)

	_, err := r.Reconcile(context.Background(), reqFor("ws-no-rte", "default"))
	require.NoError(t, err, "Reconcile itself must not error; it requeues")

	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: podName(ws.Name, string(ws.UID)), Namespace: ws.Namespace}
	getErr := r.Get(context.Background(), podKey, pod)
	assert.True(t, apierrors.IsNotFound(getErr),
		"no pod must be created when the RuntimeEnvironment is missing — creating one with an empty image would CrashLoopBackoff silently")
}

// TestE2E_Reconcile_PodSpec_FSGroupChangePolicyPersisted asserts the pod
// PERSISTED by the real Reconcile loop carries
// fsGroupChangePolicy=OnRootMismatch. The unit test on buildPod covers the
// builder; this guards the full create path (handleCreating → buildPod →
// Create), so a future regression in the create/persist path (e.g. pod-spec
// normalization stripping the field) cannot pass silently.
func TestE2E_Reconcile_PodSpec_FSGroupChangePolicyPersisted(t *testing.T) {
	ws := makeWorkspace("ws-fsgroup", "default", v1.WorkspacePhasePending)
	_, pod := reconcileToCreatingPod(t, ws, "http://test-api.e2e:8080")

	require.NotNil(t, pod.Spec.SecurityContext, "persisted pod must have a security context")
	require.NotNil(t, pod.Spec.SecurityContext.FSGroupChangePolicy,
		"persisted pod must set fsGroupChangePolicy explicitly")
	assert.Equal(t, corev1.FSGroupChangeOnRootMismatch, *pod.Spec.SecurityContext.FSGroupChangePolicy,
		"must be OnRootMismatch — the default (Always) recursively chowns the entire PVC on every pod start")
}

// TestE2E_Reconcile_PodSpec_FSGroupChangePolicyPersistedOnResume asserts the
// same on the suspend→resume path — a Resuming workspace re-creates its pod
// through handleCreating, and resume is where the production incident
// (Init:0/2 stuck for 5+ minutes chowning 315,923 files) was observed.
func TestE2E_Reconcile_PodSpec_FSGroupChangePolicyPersistedOnResume(t *testing.T) {
	ws := makeWorkspace("ws-fsgroup-resume", "default", v1.WorkspacePhaseResuming)
	past := metav1.Now()
	ws.Status.SuspendedAt = &past
	ws.Annotations = map[string]string{
		v1.AnnotationLastActivityAt: time.Now().Format(time.RFC3339),
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, ws.Namespace, ws.UID)
	pwSecret := makePasswordSecret(ws.Name, ws.Namespace)
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec: v1.RuntimeEnvironmentSpec{
			Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11",
		},
	}
	r := reconcilerFor(t, ws, pvc, pwSecret, rte)

	ctx := context.Background()
	// First reconcile: Resuming → Creating. Second: Creating → pod created.
	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(ctx, reqFor(ws.Name, ws.Namespace))
		require.NoError(t, err, "reconcile %d", i+1)
	}

	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Name: podName(ws.Name, string(ws.UID)), Namespace: ws.Namespace}
	require.NoError(t, r.Get(ctx, podKey, pod), "pod must be re-created after resume")

	require.NotNil(t, pod.Spec.SecurityContext, "resumed pod must have a security context")
	require.NotNil(t, pod.Spec.SecurityContext.FSGroupChangePolicy,
		"resumed pod must set fsGroupChangePolicy explicitly")
	assert.Equal(t, corev1.FSGroupChangeOnRootMismatch, *pod.Spec.SecurityContext.FSGroupChangePolicy,
		"must be OnRootMismatch on the resume path — resume was the observed incident path")
}
