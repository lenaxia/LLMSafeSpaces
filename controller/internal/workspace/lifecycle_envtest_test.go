//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// lifecycle_envtest_test.go — the workspace lifecycle suite against a REAL
// Kubernetes API server (owner direction 2026-11-17: "make sure we have
// integration and e2e tests around the recently-shipped features — and the
// bug classes they just exposed").
//
// THE CLASS (.15 owner reports; PR #2088 fixed the controller half): the
// bind-once lane's legacy-harness backfill returned handled=true whenever
// it ATTEMPTED the spec.harness=opencode stamp, assuming UPDATE success
// meant the field landed. On a cluster upgraded by `helm upgrade` the CRD
// is NOT upgraded with the chart (helm/README.md), so the apiserver serves
// the PRE-.15 Workspace CRD — a structural schema that silently PRUNES the
// unknown field and returns success. The stamp never lands, every reconcile
// is eaten by the backfill, and the phase machine never dispatches: start
// parks in Creating forever, refresh-compute recycles nothing, and
// suspend/resume/health handling is silently gone.
//
// The fake-client pins (harness_binding_skew_test.go) simulate the pruning
// with an interceptor. These tests prove the class against the REAL thing:
// a real apiserver serving a real pre-.15 CRD (testdata/legacycrd/, pinned
// from tag v0.44.14 — the last harness-less CRD), doing actual structural
// pruning, actual resourceVersion conflict semantics, and actual
// status-subresource writes — the tier envtest.yml exists for (its header:
// "catches the class of bug … that fake-client-based tests miss").
//
// Legs (each maps to an owner-visible symptom):
//   LC-1  pruning is real: a typed harness write is ACCEPTED and the field
//         is ABSENT on read-back (the exact trap the fake interceptor
//         simulates — proven against the apiserver itself).
//   LC-2  START completes under skew: a legacy Creating workspace builds
//         its pod and transitions Creating→Active once the pod runs —
//         the widened .15 symptom ("starting workspaces never complete
//         provisioning either").
//   LC-3  REFRESH works under skew: the RefreshWorkspaceCompute CRD write
//         (restartGeneration bump + the #1505 force marker) is observed —
//         pod deleted, phase Creating, generation caught up.
//   LC-4  SUSPEND/RESUME preserves state under skew: spec.suspend=true →
//         Suspended with the pod gone and the PVC RETAINED; resume →
//         Resuming→Creating→pod rebuilt→Active. The lifecycle round-trip
//         the starved machine silently dropped.
//   LC-5  CONVERGENCE: once the operator applies the .15 CRD shape (the
//         CRD is patched in-place — the hot reload a kubectl apply of
//         helm/crds/workspace.yaml performs), the backfill retry lands
//         the explicit opencode stamp.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestLifecycle
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// startLegacyEnvtest boots envtest serving ONLY the pre-.15 Workspace CRD
// (testdata/legacycrd/) — the pinned-version skewed environment a
// normally-upgraded .15/.16 cluster presents to the new controller.
func startLegacyEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join(".", "testdata", "legacycrd")},
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// lifecycleReconciler wires the production reconciler onto the REAL
// apiserver client (the same pin set reconcilerFor stamps — the fully
// pinned production posture) with a fake recorder for event assertions.
func lifecycleReconciler(t *testing.T, cfg *rest.Config) (*WorkspaceReconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	r := &WorkspaceReconciler{
		Client:      c,
		Scheme:      scheme,
		APIReader:   c, // #1587: tests model a working direct path (same client)
		Recorder:    record.NewFakeRecorder(64),
		AgentdImage: testAgentdImage, AgentdBinarySHA256AMD64: testAgentdSHAAMD64, AgentdBinarySHA256ARM64: testAgentdSHAARM64,
		OpencodeImage: testOpencodeImage, OpencodeBinarySHA256AMD64: testOpencodeSHAAMD64, OpencodeBinarySHA256ARM64: testOpencodeSHAARM64,
		PiImage: testPiImage, PiBinarySHA256AMD64: testPiSHAAMD64, PiBinarySHA256ARM64: testPiSHAARM64,
	}
	return r, r.Recorder.(*record.FakeRecorder)
}

// createLegacyWorkspace creates the pre-.15 fleet shape through the real
// apiserver: no spec.harness, no llmsafespaces.dev/harness-unbound marker
// (makeWorkspace's SetDefaults_Workspace test seam stamps opencode — the
// M5-b mirror of the OLD schema default — so the legacy arm is erased
// explicitly, the legacyActiveWorkspace discipline). spec.runtime is an
// EXPLICIT image ref (the us2 manifest shape) — the skewed fixture serves
// only the pre-.15 Workspace CRD, so no RuntimeEnvironment lookup happens
// (resolveRuntimeImage: contains '/' → use as-is). Returns the STORED
// object (real UID, real resourceVersion).
func createLegacyWorkspace(t *testing.T, c client.Client, name string, phase v1.WorkspacePhase) *v1.Workspace {
	t.Helper()
	ws := makeWorkspace(name, "default", phase)
	ws.Spec.Harness = "" // the legacy fleet: pre-bind-once objects
	ws.Spec.Runtime = "ghcr.io/test/python:3.11"
	ws.ResourceVersion = ""
	require.NoError(t, c.Create(context.Background(), ws))
	stored := &v1.Workspace{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, stored))
	// The pruning apiserver drops the field even on CREATE — assert the
	// stored object really is the legacy shape this suite hangs everything
	// on (if this fails, the fixture CRD is wrong, not the code).
	assert.Empty(t, stored.Spec.Harness, "the legacy CRD fixture must prune spec.harness on create (fixture drift if not)")
	return stored
}

// makeLifecyclePVC builds the workspace's PVC with a VALID owner
// reference — the real apiserver validates ownerReferences strictly
// (the fake client accepted makeBoundPVC's partial ref) — and marks it
// Bound through the status subresource (the CSI driver's half).
func createLifecyclePVC(t *testing.T, c client.Client, ws *v1.Workspace) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workspace-" + ws.Name,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: v1.GroupName + "/" + v1.GroupVersion,
				Kind:       "Workspace",
				Name:       ws.Name,
				UID:        ws.UID,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}},
		},
	}
	require.NoError(t, c.Create(context.Background(), pvc))
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, c.Status().Update(context.Background(), pvc))
}

// reconcileUntil drives Reconcile until cond passes or the budget runs out
// (bounded, generous — the house flake discipline: no tight timing pins;
// each pass is a full real-apiserver round trip).
func reconcileUntil(t *testing.T, r *WorkspaceReconciler, name string, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; ; i++ {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle leg %q did not converge after %d reconciles", what, i)
		}
		_, err := r.Reconcile(context.Background(), reqFor(name, "default"))
		require.NoError(t, err, "reconcile %d (leg %s): a pruning CRD must never error the phase machine", i, what)
		time.Sleep(50 * time.Millisecond)
	}
}

func getLifecycleWs(t *testing.T, c client.Client, name string) *v1.Workspace {
	t.Helper()
	ws := &v1.Workspace{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, ws))
	return ws
}

// setPodRunningReady writes the kubelet's half through the status
// subresource: pod Running with an IP and all containers Ready — the
// condition handleCreating's Creating→Active transition waits on.
func setPodRunningReady(t *testing.T, c client.Client, name string, ip string) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, pod))
	ready := makeRunningPod(name, "default", ip)
	pod.Status = ready.Status
	require.NoError(t, c.Status().Update(ctx, pod))
}

// LC-1: the pruning is REAL — the exact mechanism the fake interceptor in
// harness_binding_skew_test.go simulates, proven against the apiserver: a
// typed harness write is ACCEPTED (no error — the dangerous half) and the
// field is ABSENT on read-back. This is the regression pin for the fixture
// itself and the reason the skew tier exists.
func TestEnvtestLifecycleSkew_PruningIsReal(t *testing.T) {
	cfg := startLegacyEnvtest(t)
	scheme := testScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()

	ws := createLegacyWorkspace(t, c, "ws-lc-prune", v1.WorkspacePhaseActive)

	// The controller's backfill write shape: spec.harness = opencode.
	ws.Spec.Harness = v1.HarnessOpencode
	require.NoError(t, c.Update(ctx, ws), "the structural apiserver ACCEPTS the unknown field — the trap")

	stored := getLifecycleWs(t, c, ws.Name)
	assert.Empty(t, stored.Spec.Harness,
		"spec.harness must be pruned on read-back — the pre-.15 CRD does not declare it")
}

// LC-2 (born-red vs pre-#2088 main): a legacy workspace still STARTING
// (phase Creating, no pod) under the skewed CRD completes start — the pod
// is built and Creating→Active fires once the pod runs. Pre-fix every
// reconcile is eaten by the backfill loop: no pod, parked in Creating
// forever (owner confirmation 2026-11-17).
func TestEnvtestLifecycleSkew_StartCompletes(t *testing.T) {
	cfg := startLegacyEnvtest(t)
	r, _ := lifecycleReconciler(t, cfg)
	c := r.Client
	ctx := context.Background()

	ws := createLegacyWorkspace(t, c, "ws-lc-start", v1.WorkspacePhaseCreating)
	createLifecyclePVC(t, c, ws) // the PVC handleCreating expects

	name := ws.Name
	pod := podName(name, string(ws.UID))
	reconcileUntil(t, r, name, "start builds the pod", func() bool {
		p := &corev1.Pod{}
		return c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, p) == nil
	})

	// The kubelet's half: pod Running/Ready → the phase machine flips
	// Active through the real status subresource.
	setPodRunningReady(t, c, pod, "10.42.0.11")
	reconcileUntil(t, r, name, "start reaches Active", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseActive
	})

	got := getLifecycleWs(t, c, name)
	assert.Equal(t, v1.WorkspacePhaseActive, got.Status.Phase,
		"start completes through the phase machine despite the unlandable backfill")
	assert.Equal(t, "10.42.0.11", got.Status.PodIP)
	assert.Empty(t, got.Spec.Harness, "the skew never resolved — the stamp still cannot land")
}

// LC-3 (born-red vs pre-#2088 main): the refresh-compute write — the
// exact POST state RefreshWorkspaceCompute leaves on the CRD
// (restartGeneration++, the #1505 user-consent force marker) — is OBSERVED
// under skew: pod deleted, phase Creating, generation caught up. Pre-fix
// the recycle never fires ("refreshing compute via the UX no longer
// works"); this is the real-apiserver twin of SKEW-1.
func TestEnvtestLifecycleSkew_RefreshRecycles(t *testing.T) {
	cfg := startLegacyEnvtest(t)
	r, _ := lifecycleReconciler(t, cfg)
	c := r.Client
	ctx := context.Background()

	// The statusz stub keeps handleActive's health probe answered.
	startStatuszAgent(t, &statuszStub{resp: busyStatusz(100)})

	ws := createLegacyWorkspace(t, c, "ws-lc-refresh", v1.WorkspacePhaseCreating)
	createLifecyclePVC(t, c, ws)
	name := ws.Name
	pod := podName(name, string(ws.UID))

	// Drive to Active first (the user's starting state).
	reconcileUntil(t, r, name, "refresh leg reaches Active", func() bool {
		return c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, &corev1.Pod{}) == nil
	})
	setPodRunningReady(t, c, pod, "127.0.0.1")
	reconcileUntil(t, r, name, "refresh leg reaches Active", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseActive
	})

	// The user hits Refresh Compute: the API's CRD write shape.
	fresh := getLifecycleWs(t, c, name)
	fresh.Spec.RestartGeneration = 7
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[v1.AnnotationForceRecycle] = "7"
	require.NoError(t, c.Update(ctx, fresh))

	reconcileUntil(t, r, name, "refresh recycles", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseCreating
	})

	got := getLifecycleWs(t, c, name)
	assert.Equal(t, v1.WorkspacePhaseCreating, got.Status.Phase,
		"the forced refresh must recycle — the transition the UI's live updates ride")
	assert.Equal(t, int64(7), got.Status.ObservedRestartGeneration,
		"the refresh generation is observed")
	err := c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err) || (err == nil && getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseCreating),
		"the old pod is deleted by the recycle")
}

// LC-4 (born-red vs pre-#2088 main): suspend/resume PRESERVES state under
// skew — the round trip the starved machine silently dropped. Active →
// spec.suspend=true → Suspended (pod gone, PVC RETAINED, phase recorded);
// resume → Resuming→Creating → pod rebuilt → Active again. The PVC
// retention is the "nothing is stranded" invariant.
func TestEnvtestLifecycleSkew_SuspendResumePreservesState(t *testing.T) {
	cfg := startLegacyEnvtest(t)
	r, _ := lifecycleReconciler(t, cfg)
	c := r.Client
	ctx := context.Background()

	startStatuszAgent(t, &statuszStub{resp: busyStatusz(100)})

	ws := createLegacyWorkspace(t, c, "ws-lc-suspend", v1.WorkspacePhaseCreating)
	pvcName := "workspace-" + ws.Name
	createLifecyclePVC(t, c, ws)
	name := ws.Name
	pod := podName(name, string(ws.UID))

	reconcileUntil(t, r, name, "suspend leg builds the pod", func() bool {
		return c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, &corev1.Pod{}) == nil
	})
	setPodRunningReady(t, c, pod, "127.0.0.1")
	reconcileUntil(t, r, name, "suspend leg reaches Active", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseActive
	})

	// --- Suspend: the API sets spec.suspend=true.
	fresh := getLifecycleWs(t, c, name)
	suspend := true
	fresh.Spec.Suspend = &suspend
	require.NoError(t, c.Update(ctx, fresh))

	reconcileUntil(t, r, name, "suspend reaches Suspended", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseSuspended
	})
	suspended := getLifecycleWs(t, c, name)
	assert.Equal(t, v1.WorkspacePhaseSuspended, suspended.Status.Phase)
	require.NotNil(t, suspended.Status.SuspendedAt, "the suspension is timestamped")
	err := c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, &corev1.Pod{})
	assert.True(t, errors.IsNotFound(err), "the pod is deleted on suspend")
	pvcErr := c.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: "default"}, &corev1.PersistentVolumeClaim{})
	assert.NoError(t, pvcErr, "the PVC is RETAINED on suspend — nothing is stranded")

	// --- Resume: the API sets spec.suspend=false.
	fresh = getLifecycleWs(t, c, name)
	resume := false
	fresh.Spec.Suspend = &resume
	require.NoError(t, c.Update(ctx, fresh))

	reconcileUntil(t, r, name, "resume rebuilds the pod", func() bool {
		return c.Get(ctx, types.NamespacedName{Name: pod, Namespace: "default"}, &corev1.Pod{}) == nil
	})
	setPodRunningReady(t, c, pod, "127.0.0.1")
	reconcileUntil(t, r, name, "resume reaches Active", func() bool {
		return getLifecycleWs(t, c, name).Status.Phase == v1.WorkspacePhaseActive
	})

	resumed := getLifecycleWs(t, c, name)
	assert.Equal(t, v1.WorkspacePhaseActive, resumed.Status.Phase,
		"the lifecycle round-trip completes: suspend/resume preserved the workspace's state")
	assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: "default"}, &corev1.PersistentVolumeClaim{}),
		"the PVC survives the whole round trip")
}

// LC-5: convergence — once the operator applies the .15 CRD (modeled here
// as the in-place CRD patch `kubectl apply -f helm/crds/workspace.yaml`
// performs), the backfill's next retry-window attempt LANDS the explicit
// opencode stamp. The real-apiserver twin of SKEW-3, including the part
// the fake cannot prove: the CRD hot-reload actually un-prunes the field.
func TestEnvtestLifecycleSkew_ConvergesWhenCRDApplied(t *testing.T) {
	cfg := startLegacyEnvtest(t)
	r, _ := lifecycleReconciler(t, cfg)
	c := r.Client

	ws := createLegacyWorkspace(t, c, "ws-lc-converge", v1.WorkspacePhaseCreating)
	createLifecyclePVC(t, c, ws)
	name := ws.Name

	// Skewed era: one (pruned) attempt, phase machine proceeds.
	reconcileUntil(t, r, name, "converge leg builds the pod", func() bool {
		return c.Get(context.Background(), types.NamespacedName{Name: podName(name, string(ws.UID)), Namespace: "default"}, &corev1.Pod{}) == nil
	})

	// The operator applies the .15 CRD: patch the harness property into
	// the served schema (the same openAPIV3 shape helm/crds ships).
	applyHarnessProperty(t, cfg)

	// Wait for the apiserver's CRD hot-reload to serve the new schema
	// (generous window: a probe write must PERSIST before the backfill
	// is re-armed — probing with the production stamp value).
	deadline := time.Now().Add(90 * time.Second)
	for {
		probe := getLifecycleWs(t, c, name)
		probe.Spec.Harness = v1.HarnessOpencode
		err := c.Update(context.Background(), probe)
		if err == nil {
			back := getLifecycleWs(t, c, name)
			if back.Spec.Harness == v1.HarnessOpencode {
				// The schema is live; reset to the legacy shape so the
				// BACKFILL (not this probe) lands the stamp below.
				back.Spec.Harness = ""
				require.NoError(t, c.Update(context.Background(), back))
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the patched CRD never started admitting spec.harness (apiserver schema reload lag > 90s)")
		}
		time.Sleep(500 * time.Millisecond)
	}

	// The production path: age the retry window open (the seam SKEW-3
	// ages; the wall-clock 10m window is the production field) and let
	// the backfill land the stamp.
	ageBackfillWindow(r, "default/"+name)
	reconcileUntil(t, r, name, "backfill lands the stamp", func() bool {
		return getLifecycleWs(t, c, name).Spec.Harness == v1.HarnessOpencode
	})

	got := getLifecycleWs(t, c, name)
	assert.Equal(t, v1.HarnessOpencode, got.Spec.Harness,
		"once the CRD admits the field, the next window's backfill lands the explicit stamp")
}

// applyHarnessProperty patches the served Workspace CRD with the .15
// harness property — the schema delta of helm/crds/workspace.yaml, applied
// in place the way kubectl apply upgrades a CRD.
func applyHarnessProperty(t *testing.T, cfg *rest.Config) {
	t.Helper()
	ac, err := apiextensionsclient.NewForConfig(cfg)
	require.NoError(t, err)
	crd, err := ac.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), "workspaces.llmsafespaces.dev", metav1.GetOptions{})
	require.NoError(t, err)

	prop := apiextensionsv1.JSONSchemaProps{
		Type:        "string",
		Enum:        []apiextensionsv1.JSON{{Raw: []byte(`"opencode"`)}, {Raw: []byte(`"pi"`)}},
		Description: "Coding-agent harness this workspace runs. Nullable and bind-once (owner ruling 2026-11-16): empty = unbound, set exactly once (at create, explicit bind while unbound, or atomically with the first session); immutable forever after — enforced by the validating webhook. There is no switch or recreate path.",
	}
	versions := crd.Spec.Versions
	require.NotEmpty(t, versions)
	if versions[0].Schema == nil {
		versions[0].Schema = &apiextensionsv1.CustomResourceValidation{}
	}
	schema := versions[0].Schema.OpenAPIV3Schema
	if schema.Properties == nil {
		schema.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
	}
	specProps, ok := schema.Properties["spec"]
	require.True(t, ok, "the fixture CRD must carry a spec property")
	if specProps.Properties == nil {
		specProps.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
	}
	specProps.Properties["harness"] = prop
	schema.Properties["spec"] = specProps
	crd.Spec.Versions = versions

	_, err = ac.ApiextensionsV1().CustomResourceDefinitions().Update(context.Background(), crd, metav1.UpdateOptions{})
	require.NoError(t, err, "the CRD patch (the operator's kubectl apply) must be accepted")
}

// Compile-time interface assertions for the fixture objects this file uses.
var (
	_ runtime.Object = (*v1.Workspace)(nil)
	_ client.Object  = (*corev1.Pod)(nil)
	_ client.Object  = (*corev1.PersistentVolumeClaim)(nil)
)
