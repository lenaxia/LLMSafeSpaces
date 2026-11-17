// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// harness_binding_skew_test.go — the .15 refresh-compute starvation pin
// (owner report 2026-11-16: "refreshing compute via the UX no longer
// works — workspaces are not refreshing").
//
// THE SHAPE: helm never upgrades crds/ on `helm upgrade` (helm/README.md
// "CRD upgrades"), so a normally-upgraded .15 cluster runs the new API +
// new controller against the PRE-.15 Workspace CRD — which has no
// spec.harness field. A structural apiserver silently PRUNES unknown
// fields on write: the controller's legacy backfill
// (reconcileHarnessBinding → backfillLegacyHarness stamps
// spec.harness=opencode) "succeeds" (200 OK) but the field never
// persists. The next reconcile reads harness=="" again, backfills
// again, and — because the legacy arm returns handled=true — the phase
// machine NEVER runs again for any legacy workspace:
//
//   - handleActive never observes the refresh-compute restartGeneration
//     bump → no pod recycle → "refreshing compute does nothing";
//   - no phase transitions → the API watcher's onPhaseChange never
//     fires → no workspace.phase SSE events → the UI's live updates and
//     the mutation's own invalidation refetch the SAME stale phase
//     forever → "workspaces are not refreshing".
//
// The fixture simulates the pruning apiserver with a fake-client
// interceptor that zeroes Spec.Harness on every workspace Update — the
// byte-exact semantics of structural pruning (write accepted, field
// dropped, NO error).
//
// Pins (born-red: the behavioral arms fail on unfixed main with "phase
// stayed Active / pod still exists"):
//   - SKEW-1 the repro: a user-forced refresh (generation bump + the
//     #1505 marker RefreshWorkspaceCompute stamps) on a legacy Active
//     workspace MUST still recycle — phase Creating, pod deleted,
//     generation observed — despite the unlandable backfill.
//   - SKEW-2 bounded write cadence: the backfill re-attempts at most
//     once per retry window (no hot write loop against a pruning
//     apiserver).
//   - SKEW-3 convergence: once the operator applies the .15 CRD (the
//     interceptor stops pruning), the next window's attempt lands the
//     explicit opencode stamp — the bind-once invariant is eventually
//     true, and the stamp NEVER feeds back into the phase machine.
//   - SKEW-4 healthy cluster unchanged: with a landing write the
//     existing behavior holds — one backfill pass, requeue, then the
//     bound object proceeds (the pre-.15-transition contract).
//   - SKEW-5 the widened symptom (owner confirmation 2026-11-17): a
//     STARTING legacy workspace (provisioning, phase Creating) under
//     the same skew completes start — the pod is built and the
//     Creating→Active transition fires. reconcileHarnessBinding runs
//     before EVERY phase's dispatch, so the starved machine gated
//     start/provisioning exactly as it gated refresh.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// pruningCRDInterceptor builds interceptor.Funcs that simulate a
// pre-.15 (harness-less) Workspace CRD served by a structural
// apiserver: every workspace Update is ACCEPTED and the unknown
// spec.harness field silently DROPPED before the write lands. The
// returned counter tallies how many writes actually carried a
// harness value (the observable half of the pruning — the write the
// controller attempted).
func pruningCRDInterceptor() (interceptor.Funcs, *int, *bool) {
	harnessWrites := 0
	pruning := true // the operator applies the .15 CRD mid-test (SKEW-3)
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if ws, isWS := obj.(*v1.Workspace); isWS {
				if ws.Spec.Harness != "" {
					harnessWrites++
				}
				// The pruning apiserver: accept, drop the unknown field.
				// Status subresource writes never carry spec changes, so
				// zeroing unconditionally mirrors the schema's view.
				if pruning {
					ws.Spec.Harness = ""
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	}, &harnessWrites, &pruning
}

// legacyActiveWorkspace builds a pre-.15 workspace: spec.harness == "",
// no AnnotationHarnessUnbound marker (makeWorkspace's test-seam
// defaulting stamps opencode — the production CRD has no default, and
// the legacy fleet is the counter-example every existing fixture
// erases).
func legacyActiveWorkspace(name string) *v1.Workspace {
	ws := makeWorkspace(name, "default", v1.WorkspacePhaseActive)
	ws.Spec.Harness = ""
	return ws
}

// SKEW-1 (the repro, born-red): the user hits Refresh Compute on a
// legacy Active workspace against a pruning (pre-.15 CRD) apiserver.
// The refresh's CRD write (restartGeneration++ + the #1505 user-consent
// marker) is ACCEPTED — only the harness field is pruned — so the phase
// machine MUST observe it: pod deleted, phase Creating. Pre-fix the
// legacy-backfill arm eats every reconcile pass (write "succeeds",
// field never lands, handled=true forever) and this pin fails with the
// owner's exact symptom: phase stays Active, pod survives, no phase
// transition for the SSE watchers to feed on.
func TestHarnessBindingSkew_UserRefreshStillRecycles(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)

	ws := legacyActiveWorkspace("ws-skew-refresh")
	// The exact post-POST state RefreshWorkspaceCompute writes:
	// generation bumped, user-consent force marker stamped.
	ws.Spec.RestartGeneration = 7
	ws.Status.ObservedRestartGeneration = 6
	ws.Status.PodIP = "127.0.0.1"
	ws.Annotations = map[string]string{v1.AnnotationForceRecycle: "7"}
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	f, _, _ := pruningCRDInterceptor()
	r, _ := reconcilerWithInterceptor(t, f, ws, pod, pwSecret)

	// The watch re-delivers on every requeue — a bounded loop stands in
	// for the (infinite, pre-fix) backfill requeue cycle.
	for i := 0; i < 5; i++ {
		_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		require.NoError(t, err, "reconcile %d: a pruning CRD must never error the phase machine", i)
	}

	assert.False(t, podExists(t, r, pod.Name),
		"the user-forced refresh must recycle the pod despite the unlandable harness backfill")
	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseCreating, updated.Status.Phase,
		"the phase transition must fire — it is the event source the UI's live updates ride")
	assert.Equal(t, int64(7), updated.Status.ObservedRestartGeneration,
		"the refresh generation is observed")
}

// SKEW-1b: the same skew on a plain (no user consent) generation bump
// with BUSY sessions still takes the polite #761 drain — the skew fix
// must not turn every recycle into a forced one. The drain's deferral
// proves the phase machine is dispatching at all (pre-fix it never
// reaches handleActive).
func TestHarnessBindingSkew_PlainBumpStillDrains(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)

	ws := legacyActiveWorkspace("ws-skew-plain")
	ws.Spec.RestartGeneration = 2
	ws.Status.ObservedRestartGeneration = 1
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	f, _, _ := pruningCRDInterceptor()
	r, _ := reconcilerWithInterceptor(t, f, ws, pod, pwSecret)

	result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.Equal(t, drainPollInterval, result.RequeueAfter,
		"the plain bump defers behind busy sessions — the phase machine dispatched (pre-fix it never did)")
	assert.True(t, podExists(t, r, pod.Name), "the drain defers the deletion")
}

// SKEW-2 (born-red on the write-storm half): the backfill re-attempts
// at most ONCE per retry window against the pruning apiserver — the
// pre-fix shape writes on EVERY reconcile (a hot update loop bumping
// resourceVersion and racing every API-tier CRD write, refresh-compute
// included).
func TestHarnessBindingSkew_BackfillWriteCadenceIsBounded(t *testing.T) {
	ws := legacyActiveWorkspace("ws-skew-cadence")
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	f, harnessWrites, _ := pruningCRDInterceptor()
	r, _ := reconcilerWithInterceptor(t, f, ws, pod, pwSecret)

	for i := 0; i < 10; i++ {
		_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		require.NoError(t, err)
	}

	assert.LessOrEqual(t, *harnessWrites, 1,
		"one backfill write per retry window — a pruning apiserver must not be hammered (got %d writes in 10 reconciles)", *harnessWrites)
}

// SKEW-3: convergence — once the operator applies the .15 CRD (the
// interceptor stops pruning), the next window's backfill attempt lands
// the explicit opencode stamp. The invariant ("every workspace is
// explicitly bound") is eventually true without any operator action
// beyond the CRD apply the README already mandates.
func TestHarnessBindingSkew_BackfillConvergesOnceCRDLands(t *testing.T) {
	ws := legacyActiveWorkspace("ws-skew-converge")
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	pruning, _, pruningOn := pruningCRDInterceptor()
	r, _ := reconcilerWithInterceptor(t, pruning, ws, pod, pwSecret)

	// Skewed era: one pruned attempt, phase machine proceeds.
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	// The operator applies the CRD: pruning stops. Force the retry
	// window open (the production seam is wall-clock; the test ages the
	// attempt directly) and reconcile again.
	*pruningOn = false
	ageBackfillWindow(r, ws.Namespace+"/"+ws.Name)

	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.HarnessOpencode, updated.Spec.Harness,
		"once the CRD accepts the field, the next window's backfill lands the explicit stamp")
}

// SKEW-4: the healthy cluster is unchanged — the write lands, the
// reconcile stops for the stamp pass (re-read the bound object), and
// the SECOND pass proceeds bound. The pre-existing contract, pinned so
// the skew fix cannot regress the normal .15 upgrade path.
func TestHarnessBindingSkew_HealthyCRDKeepsStampThenProceed(t *testing.T) {
	ws := legacyActiveWorkspace("ws-skew-healthy")
	ws.Status.PodIP = "127.0.0.1"
	pod := makeRunningPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSecret := makePasswordSecret(ws.Name, "default")

	r, _ := reconcilerForDrain(t, ws, pod, pwSecret)

	// Pass 1: the stamp lands; the arm holds the reconcile (handled).
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	stamped := getWs(t, r, ws.Name)
	assert.Equal(t, v1.HarnessOpencode, stamped.Spec.Harness,
		"on a healthy CRD the backfill stamp persists on the first pass")

	// Pass 2: bound — proceeds into the phase machine (Active stays
	// healthy; nothing recycles).
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase)
	assert.True(t, podExists(t, r, pod.Name), "a bound Active workspace with no generation bump keeps its pod")
}

// SKEW-5 (owner confirmation 2026-11-17 — the widened symptom): a
// legacy workspace still STARTING (provisioning, phase Creating, no pod)
// under the pruning apiserver must complete start: handleCreating builds
// the pod (leg 1) and the Creating→Active transition fires once the pod
// runs (leg 2). Pre-fix every reconcile is eaten by the backfill loop —
// no pod is ever built and the workspace parks in Creating forever:
// "start/provisioning transitions stall the same way."
func TestHarnessBindingSkew_StartingWorkspaceCompletesProvisioning(t *testing.T) {
	ws := legacyActiveWorkspace("ws-skew-start")
	ws.Status.Phase = v1.WorkspacePhaseCreating
	ws.Status.PVCName = "workspace-ws-skew-start"
	pvc := makeBoundPVC("workspace-ws-skew-start", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-skew-start", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}

	f, _, _ := pruningCRDInterceptor()
	r, _ := reconcilerWithInterceptor(t, f, ws, pvc, pwSecret, rte)

	// Leg 1 — provisioning: the pod is built despite the unlandable
	// backfill (the phase machine dispatched).
	expectedPod := podName(ws.Name, string(ws.UID))
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		require.NoError(t, err, "reconcile %d", i)
	}
	pod := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: expectedPod, Namespace: "default"}, pod),
		"handleCreating must build the pod — start does not stall on the skew")
	assert.Equal(t, "ghcr.io/test/python:3.11", pod.Spec.Containers[0].Image)

	// Leg 2 — the pod goes Running/Ready (the kubelet's half); the
	// recovered phase machine flips the workspace Active. Status rides
	// the status subresource (the fake client drops status on plain
	// Update — the same discipline as the workspace writes).
	pod.Status = makeRunningPod(expectedPod, "default", "10.0.0.7").Status
	require.NoError(t, r.Status().Update(context.Background(), pod))
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	updated := getWs(t, r, ws.Name)
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase,
		"the start completes: Creating→Active transitions through the recovered phase machine")
	assert.Equal(t, "10.0.0.7", updated.Status.PodIP)
}

// podExists / getWs ride the shared drain fixtures (session_drain_test.go
// / agentd_update_policy_test.go). Compile-time interface assertions for
// the fixture objects this file reuses.
var (
	_ *corev1.Pod          = (*corev1.Pod)(nil)
	_ runtime.Object       = (*v1.Workspace)(nil)
	_ client.Object        = (*v1.Workspace)(nil)
	_ types.NamespacedName = types.NamespacedName{}
)

// ageBackfillWindow expires one workspace's backfill retry window (the
// test seam for SKEW-3's post-CRD-apply reconciliation).
func ageBackfillWindow(r *WorkspaceReconciler, key string) {
	r.harnessBackfillMu.Lock()
	defer r.harnessBackfillMu.Unlock()
	if r.harnessBackfillAttemptAt != nil {
		r.harnessBackfillAttemptAt[key] = time.Now().Add(-harnessBackfillRetryInterval - time.Minute)
	}
}

// SKEW-6 (review r1, robustness): the persistence verify rides the
// informer-cache client; a cache not yet caught up with the write's
// watch event serves the PRE-write object. On a HEALTHY cluster that
// shape must never read as a skew verdict — no false operator ERROR,
// no 10m window suppression. The stale-read gate (read-back RV must
// match the write's RV) treats it as inconclusive: landed semantics,
// the pass holds for the re-read (phase machine withheld exactly one
// pass), and the attempt record is CLEARED. The next reconcile reads
// the caught-up cache and proceeds bound.
func TestBackfillVerifySurvivesCacheLag(t *testing.T) {
	ws := legacyActiveWorkspace("ws-skew-cachelag")
	ws.Status.Phase = v1.WorkspacePhaseCreating
	ws.Status.PVCName = "workspace-ws-skew-cachelag"
	pvc := makeBoundPVC("workspace-ws-skew-cachelag", "default", ws.UID)
	pwSecret := makePasswordSecret("ws-skew-cachelag", "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	expectedPod := podName(ws.Name, string(ws.UID))

	// The lagging-cache fixture: Get#1 (reconcile fetch) and Get#2 (the
	// backfill's fresh read) serve the tracker; Get#3 (the verify read)
	// serves the PRE-write snapshot — exactly one update behind.
	gets := 0
	var snapshot *v1.Workspace
	lag := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			gets++
			if gets == 2 {
				snapshot = obj.(*v1.Workspace).DeepCopy()
			}
			if gets == 3 && snapshot != nil {
				snapshot.DeepCopyInto(obj.(*v1.Workspace))
			}
			return nil
		},
	}
	r, _ := reconcilerWithInterceptor(t, lag, ws, pvc, pwSecret, rte)

	// Pass 1 (the lag window): inconclusive verdict — landed semantics.
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, expectedPod),
		"the inconclusive pass holds the phase machine for the re-read (landed semantics) — NOT the skew verdict's fall-through")
	r.harnessBackfillMu.Lock()
	_, suppressed := r.harnessBackfillAttemptAt[ws.Namespace+"/"+ws.Name]
	r.harnessBackfillMu.Unlock()
	assert.False(t, suppressed,
		"no window suppression on a healthy landing write — the attempt record is cleared with the landed verdict")

	// Pass 2 (cache caught up): bound short-circuit, phase machine
	// proceeds, start continues.
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.True(t, podExists(t, r, expectedPod),
		"once the cache catches up, the bound object proceeds — start completes, no skew verdict was ever logged")
}
