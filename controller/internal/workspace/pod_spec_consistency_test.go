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

// TestE2E_Reconcile_PodSpec_InitContainerSelfConsistent is the central
// guard. On a SINGLE Reconcile-produced pod it asserts the platform
// init chain (design 0053 S3 — the bash heredoc is deleted):
//  1. Every platform init container runs the pinned overlay binary
//     (Command references the agentd image volume), never a PATH lookup.
//  2. platform-bootstrap receives --workspace-id == the workspace name
//     and LLMSAFESPACES_API_URL == the reconciler's APIServiceURL (the
//     binary contract: --workspace-id required, --api-url env fallback).
//  3. The bootstrap-token volume is projected with Path "token" so the
//     binary's default read path /var/run/bootstrap/token resolves, and
//     the bootstrap init mounts it read-only.
//  4. Init ordering: platform-init (subPath roots) precedes
//     platform-bootstrap, which precedes platform-materialize.
//
// A refactor renaming an arg, dropping a mount, or reordering the chain
// breaks exactly one of these cross-checks.
func TestE2E_Reconcile_PodSpec_InitContainerSelfConsistent(t *testing.T) {
	ws := makeWorkspace("ws-consistency", "default", v1.WorkspacePhasePending)
	const apiURL = "http://test-api.e2e:8080"
	_, pod := reconcileToCreatingPod(t, ws, apiURL)

	var initOrder []string
	for _, c := range pod.Spec.InitContainers {
		initOrder = append(initOrder, c.Name)
	}
	require.GreaterOrEqual(t, indexOf(initOrder, "platform-init"), 0, "platform-init must exist")
	require.Less(t, indexOf(initOrder, "platform-init"), indexOf(initOrder, "platform-bootstrap"),
		"platform-init (subPath roots) must run before bootstrap")
	require.Less(t, indexOf(initOrder, "platform-bootstrap"), indexOf(initOrder, "platform-materialize"),
		"bootstrap must run before materialize (secrets land before they are applied)")

	for _, name := range []string{"platform-init", "platform-bootstrap", "platform-materialize"} {
		c := findInitContainerOrFatal(t, pod, name)
		assert.Equal(t, agentdMountPath+agentdBinaryRelPath, c.Command[0],
			"%s must exec the pinned overlay binary", name)
	}

	bootstrap := findInitContainerOrFatal(t, pod, "platform-bootstrap")
	assert.Contains(t, bootstrap.Args, "bootstrap")
	assert.Contains(t, bootstrap.Args, "--workspace-id")
	assert.Equal(t, ws.Name, bootstrap.Args[indexOf(bootstrap.Args, "--workspace-id")+1],
		"--workspace-id must equal the workspace name")
	assert.Equal(t, apiURL, envMap(bootstrap.Env)["LLMSAFESPACE_API_URL"],
		"LLMSAFESPACES_API_URL must equal the reconciler's APIServiceURL")
	for _, e := range bootstrap.Env {
		assert.NotEqual(t, "WORKSPACE_ID", e.Name,
			"the platform subcommands take --workspace-id, not an env var")
	}

	var tokenVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "bootstrap-token" {
			tokenVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, tokenVol, "bootstrap-token volume must exist")
	require.NotNil(t, tokenVol.Projected)
	require.Len(t, tokenVol.Projected.Sources, 1)
	assert.Equal(t, "token", tokenVol.Projected.Sources[0].ServiceAccountToken.Path,
		"projected token Path must be 'token' (binary default /var/run/bootstrap/token)")
	var tokenMount *corev1.VolumeMount
	for i := range bootstrap.VolumeMounts {
		if bootstrap.VolumeMounts[i].Name == "bootstrap-token" {
			tokenMount = &bootstrap.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, tokenMount)
	assert.Equal(t, "/var/run/bootstrap", tokenMount.MountPath)
	assert.True(t, tokenMount.ReadOnly)

	// US-70.3: single-container agentd (the main container's --supervise
	// process) serves /v1/resync-secrets and must be able to read a FRESH
	// projected token for the pod's lifetime — the same volume mounts there,
	// read-only.
	require.NotEmpty(t, pod.Spec.Containers)
	var mainTokenMount *corev1.VolumeMount
	for i := range pod.Spec.Containers[0].VolumeMounts {
		if pod.Spec.Containers[0].VolumeMounts[i].Name == "bootstrap-token" {
			mainTokenMount = &pod.Spec.Containers[0].VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, mainTokenMount,
		"the main container must mount bootstrap-token (agentd resync re-pulls, US-70.3)")
	assert.Equal(t, "/var/run/bootstrap", mainTokenMount.MountPath)
	assert.True(t, mainTokenMount.ReadOnly,
		"the token mount on the main container is read-only")
}

func indexOf(items []string, want string) int {
	for i, v := range items {
		if v == want {
			return i
		}
	}
	return -1
}

// TestE2E_Reconcile_PodSpec_SingleContainerResyncBatchPathRW pins the
// US-70.3 validation B1 fix: in chart-DEFAULT single-container mode the
// MAIN container's agentd serves /v1/resync-secrets, and its batch-file
// default (/sandbox-cfg/secrets.json) lands on the ReadOnly sandbox-cfg
// mount — every changed-manifest resync 500s at the batch write. The
// controller must set LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT on the main
// container to the pod-scoped RW tmpfs coordinate (the same one the
// sidecar mode uses). Cross-validated on ONE reconcile-produced pod:
// the env is present with the exact value, and the value's parent
// directory is a declared RW VolumeMount on that same container.
//
// (The sandbox-runtime volume's tmpfs/RW properties are separately
// pinned in security_test.go; the mount-level RW check here keeps this
// pin self-contained.) A cluster-level resync e2e in single-container
// mode is a known gap — the nightly e2e runs sidecar=true; the
// attachments single-container workflow can carry it later.
func TestE2E_Reconcile_PodSpec_SingleContainerResyncBatchPathRW(t *testing.T) {
	ws := makeWorkspace("ws-resync-rw", "default", v1.WorkspacePhasePending)
	_, pod := reconcileToCreatingPod(t, ws, "http://test-api:8080")

	require.False(t, podHasAgentdSidecar(pod), "test premise: chart-default reconcile is single-container mode")

	var mainContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			mainContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, mainContainer, "main 'workspace' container must exist")

	const wantOut = "/sandbox-runtime/rt/secrets.json"
	gotOut, ok := envMap(mainContainer.Env)["LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT"]
	require.True(t, ok,
		"single-container main container must carry LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT — without it the resync batch write targets the ReadOnly /sandbox-cfg mount and 500s")
	assert.Equal(t, wantOut, gotOut, "the resync batch path must be the exact pod-scoped tmpfs coordinate")

	// The batch path's parent must be a declared, RW mount on the SAME
	// container: /sandbox-runtime (the rt/ subdir itself is created by
	// the init-fs platform subcommand in both modes).
	var runtimeMount *corev1.VolumeMount
	for i := range mainContainer.VolumeMounts {
		if mainContainer.VolumeMounts[i].MountPath == "/sandbox-runtime" {
			runtimeMount = &mainContainer.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, runtimeMount, "/sandbox-runtime must be mounted on the main container")
	assert.False(t, runtimeMount.ReadOnly,
		"/sandbox-runtime must be RW on the main container — the resync batch write lands under it")

	// And for contrast: the old default's parent is the RO mount the
	// env var exists to escape.
	var cfgMount *corev1.VolumeMount
	for i := range mainContainer.VolumeMounts {
		if mainContainer.VolumeMounts[i].MountPath == "/sandbox-cfg" {
			cfgMount = &mainContainer.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, cfgMount, "/sandbox-cfg must be mounted on the main container")
	assert.True(t, cfgMount.ReadOnly,
		"/sandbox-cfg stays ReadOnly — which is exactly why the resync batch path must not default into it")
}

// podHasAgentdSidecar reports whether the built pod carries the agentd
// native sidecar (design 0051 US-2 mode flag).
func podHasAgentdSidecar(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == "agentd" {
			return true
		}
	}
	return false
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

	// The main container's admin-token delivery must reference the same
	// secret in ONE of the two modes (#887 D5.1):
	//   file mode  (Secret carries the distinct admin-token key — the
	//               mode this reconcile produced, since ensurePasswordSecret
	//               upserts the key): AGENTD_ADMIN_TOKEN_FILE only, and the
	//               init script installs /sandbox-cfg/admin-token.
	//   legacy env mode: AGENTD_ADMIN_TOKEN ← SecretKeyRef(password).
	// A drop of BOTH breaks the readiness probe auth (401 on /v1/readyz).
	var mainContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "workspace" {
			mainContainer = &pod.Spec.Containers[i]
			break
		}
	}
	require.NotNil(t, mainContainer, "main 'workspace' container must exist")
	var fileMode, legacyEnvMode bool
	for _, e := range mainContainer.Env {
		if e.Name == "AGENTD_ADMIN_TOKEN_FILE" && e.Value == "/sandbox-cfg/admin-token" {
			fileMode = true
		}
		if e.Name == "AGENTD_ADMIN_TOKEN" && e.ValueFrom != nil &&
			e.ValueFrom.SecretKeyRef != nil &&
			e.ValueFrom.SecretKeyRef.Name == passwordSecretName(ws.Name) {
			legacyEnvMode = true
		}
	}
	assert.True(t, fileMode || legacyEnvMode,
		"admin-token delivery must be present (AGENTD_ADMIN_TOKEN_FILE file mode or AGENTD_ADMIN_TOKEN env mode)")
	if fileMode {
		assert.False(t, legacyEnvMode,
			"file mode must not ALSO carry the env var — the token would ride opencode's env into every tool process")
		// The 0400 install itself is agentd init-fs territory (design
		// 0053 S3); the controller-side precondition is that the
		// Secret reaches the init that performs it.
		platformInit := findInitContainerOrFatal(t, pod, "platform-init")
		var pwMount *corev1.VolumeMount
		for i := range platformInit.VolumeMounts {
			if platformInit.VolumeMounts[i].Name == "pw-secret" {
				pwMount = &platformInit.VolumeMounts[i]
				break
			}
		}
		require.NotNil(t, pwMount, "file mode requires pw-secret on platform-init (init-fs installs the token)")
	}
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
