// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// upgrade_rollout_kill_test.go — #1724 invariant pins for platform-upgrade
// rollouts, extending the #761 drain suite (session_drain_test.go fixtures)
// with the #1719 red-first kill-test pattern.
//
// Incident (2026-10-17, v0.38.0 rollout): every in-flight session died
// simultaneously during the GitOps deploy (git ref + 4 semver image tags +
// both overlay digest re-pins). The suspected mechanism — "digest bump →
// spec-hash drift → controller replaces every workspace pod" — is refuted
// by code-path analysis (see the #1724 analysis comment): the overlay
// digest enters the pod spec ONLY at build time (wireAgentdOverlay /
// wireOpencodeOverlay from buildPod) and NO reconcile path compares a
// running pod's overlay volume or env pins against the configured
// delivery images. The roll is LAZY by design: existing pods keep the
// digest they were built with until they restart for an unrelated reason
// (agentd_overlay.go header: "any pod (re)creation ... pulls the current
// pinned digest").
//
// These tests pin that contract so it cannot regress silently:
//
//   - An overlay digest change (the exact controller-state delta of a
//     platform upgrade) must NEVER delete or mutate a running pod —
//     busy sessions get zero forced kills (there is not even a deferral:
//     the drain machinery must stay untouched), and idle pods are not
//     churned either.
//   - A pod recreated for an unrelated reason AFTER the digest change
//     picks up the NEW digest (the lazy roll actually rolls).
//   - Every deletePodByName call site in the package stays within the
//     classified deletion-path inventory (#761 drain matrix) — a NEW
//     deletion path cannot appear without being consciously classified
//     here (the opencode_overlay_contract_test.go source-guard pattern).
//
// Red-first verification (worklog): each pin was validated against a
// deliberately broken mutation — an eager drift-recycle in handleActive
// (compare the running pod's agentd volume reference against r.AgentdImage,
// delete on mismatch) — and the busy pin FAILS against it (the pod dies,
// the forced metric moves). The mutation was then discarded; the pins ship
// green against the compliant main.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// --- fixtures -------------------------------------------------------------

// digestA/digestB model one release bump of BOTH overlay delivery pins
// (the controller-restart delta of a platform upgrade: new image refs and
// the startup-resolved per-arch binary sha256s that ride them).
var (
	upgradeDigestA = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	upgradeDigestB = "ghcr.io/lenaxia/llmsafespaces/agentd@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	upgradePinAmd64A = "1111111111111111111111111111111111111111111111111111111111111111"
	upgradePinAmd64B = "2222222222222222222222222222222222222222222222222222222222222222"

	upgradeOpencodeB = "ghcr.io/lenaxia/llmsafespaces/opencode@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// makeUpgradedOverlayPod builds a READY running pod whose spec carries the
// OLD (digest-A) overlay wiring — the shape every healthy workspace pod has
// the moment a platform upgrade restarts the controller with digest B.
func makeUpgradedOverlayPod(name, namespace, ip string) *corev1.Pod {
	pod := makeRunningPod(name, namespace, ip)
	pod.Spec.Containers = []corev1.Container{{Name: "workspace"}}
	main := &pod.Spec.Containers[0]
	main.VolumeMounts = []corev1.VolumeMount{
		{Name: "agentd", MountPath: "/agentd", ReadOnly: true},
	}
	main.Env = []corev1.EnvVar{
		{Name: agentdOverlayEnvKey, Value: "1"},
		{Name: "LLMSAFESPACES_AGENTD_SHA256_AMD64", Value: upgradePinAmd64A},
	}
	pod.Spec.Volumes = []corev1.Volume{
		{
			Name: "agentd",
			VolumeSource: corev1.VolumeSource{
				Image: &corev1.ImageVolumeSource{Reference: upgradeDigestA, PullPolicy: corev1.PullIfNotPresent},
			},
		},
	}
	return pod
}

// applyOverlayB reconfigures the reconciler with the POST-upgrade delivery
// pins — the exact in-memory delta of the controller pod rolling onto the
// new helm values (startup pin resolution included).
func applyOverlayB(r *WorkspaceReconciler) {
	r.AgentdImage = upgradeDigestB
	r.AgentdBinarySHA256AMD64 = upgradePinAmd64B
	r.AgentdBinarySHA256ARM64 = upgradePinAmd64B
	r.OpencodeImage = upgradeOpencodeB
}

// agentdVolumeRef extracts the pod's agentd overlay image-volume reference
// ("" when absent).
func agentdVolumeRef(pod *corev1.Pod) string {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "agentd" && pod.Spec.Volumes[i].Image != nil {
			return pod.Spec.Volumes[i].Image.Reference
		}
	}
	return ""
}

// --- kill-tests: digest change never touches a running pod -----------------

// TestUpgrade_OverlayDigestChange_BusySessionsZeroKills (#1724 kill-test):
// the exact upgrade delta (both overlay digests + resolved pins change out
// from under the controller) with BUSY in-flight sessions must produce ZERO
// pod deletions — not even a drain deferral, because a digest change is not
// a deletion trigger at all. Repeated reconciles (a poll loop's worth) must
// leave the pod and its spec untouched. An eager "spec-hash drift" recycle
// fails this test by killing the busy pod.
func TestUpgrade_OverlayDigestChange_BusySessionsZeroKills(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)

	ws := makeWorkspace("ws-upgrade-busy", "default", v1.WorkspacePhaseActive)
	start := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &start
	ws.Status.PodIP = "127.0.0.1"
	pod := makeUpgradedOverlayPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSec := makePasswordSecret(ws.Name, "default")
	r, rec := reconcilerForDrain(t, ws, pod, pwSec)
	applyOverlayB(r)

	deferredBefore := counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonRestartGeneration)
	forcedBefore := counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonRestartGeneration)
	failedOpenBefore := counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonRestartGeneration)

	// A poll loop's worth of post-upgrade reconciles.
	for i := 0; i < 3; i++ {
		result, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		require.NoError(t, err)
		assert.True(t, podExists(t, r, pod.Name),
			"reconcile %d: an overlay digest change must never delete a running pod (busy sessions in flight)", i)
		updated := &v1.Workspace{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
		assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase,
			"reconcile %d: an overlay digest change must not move the workspace out of Active", i)
		assert.NotEqual(t, drainPollInterval, result.RequeueAfter,
			"reconcile %d: the upgrade must not park the workspace in a drain deferral — no deletion was requested", i)
	}

	// The running pod's spec still carries digest A: nothing mutated it
	// in place (image volumes and env pins are immutable post-create).
	survivor := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, survivor))
	assert.Equal(t, upgradeDigestA, agentdVolumeRef(survivor),
		"the running pod keeps the digest it was built with — the roll is lazy, not in-place")

	// The drain machinery must be completely untouched by the upgrade.
	assert.Equal(t, deferredBefore, counterValue(t, metrics.WorkspaceDrainDeferredTotal, drainReasonRestartGeneration),
		"digest change must not emit drain-deferred")
	assert.Equal(t, forcedBefore, counterValue(t, metrics.WorkspaceDrainForcedTotal, drainReasonRestartGeneration),
		"digest change must not emit drain-forced")
	assert.Equal(t, failedOpenBefore, counterValue(t, metrics.WorkspaceDrainFailedOpenTotal, drainReasonRestartGeneration),
		"digest change must not emit drain-failed-open")
	for _, e := range eventsFrom(rec) {
		assert.NotContains(t, e, "SessionDrain",
			"digest change must not fire any drain event, got: %s", e)
	}

	r.drainStatesMu.Lock()
	assert.Empty(t, r.drainStates, "no drain window may open for a digest change")
	r.drainStatesMu.Unlock()
}

// TestUpgrade_OverlayDigestChange_IdlePodAlsoUntouched: laziness is the
// contract even with NO sessions in flight — an upgrade must not churn
// idle running pods either (scheduling churn, PVC reattachment, session
// cold-starts are all user-visible without a kill).
func TestUpgrade_OverlayDigestChange_IdlePodAlsoUntouched(t *testing.T) {
	stub := &statuszStub{resp: idleStatusz()}
	startStatuszAgent(t, stub)

	ws := makeWorkspace("ws-upgrade-idle", "default", v1.WorkspacePhaseActive)
	start := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &start
	ws.Status.PodIP = "127.0.0.1"
	pod := makeUpgradedOverlayPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSec := makePasswordSecret(ws.Name, "default")
	r, _ := reconcilerForDrain(t, ws, pod, pwSec)
	applyOverlayB(r)

	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)

	assert.True(t, podExists(t, r, pod.Name), "an idle running pod must also survive an overlay digest change")
	survivor := &corev1.Pod{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, survivor))
	assert.Equal(t, upgradeDigestA, agentdVolumeRef(survivor))

	updated := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: ws.Name, Namespace: "default"}, updated))
	assert.Equal(t, v1.WorkspacePhaseActive, updated.Status.Phase)
}

// TestUpgrade_RecreatedPodPicksUpNewDigest: the lazy roll actually rolls —
// once a pod is recreated for an UNRELATED reason (here: a user-consented
// restart-generation bump after the turn went idle, driven end to end
// through handleActive → delete → handleCreating → buildPod), the new pod
// carries the NEW overlay digest and pins.
func TestUpgrade_RecreatedPodPicksUpNewDigest(t *testing.T) {
	stub := &statuszStub{resp: busyStatusz(100)}
	startStatuszAgent(t, stub)

	ws := makeWorkspace("ws-upgrade-lazyroll", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = "127.0.0.1"
	pod := makeUpgradedOverlayPod(podName(ws.Name, string(ws.UID)), "default", "127.0.0.1")
	pwSec := makePasswordSecret(ws.Name, "default")
	rte := &v1.RuntimeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "python-3.11"},
		Spec:       v1.RuntimeEnvironmentSpec{Image: "ghcr.io/test/python:3.11", Language: "python", Version: "3.11"},
	}
	pvc := makeBoundPVC("workspace-"+ws.Name, "default", ws.UID)
	r, _ := reconcilerForDrain(t, ws, pod, pwSec, rte, pvc)
	applyOverlayB(r)

	// Post-upgrade reconcile with a busy turn: untouched (pinned above);
	// this also proves the busy pod survives the pending generation bump.
	ws.Spec.RestartGeneration = 2
	require.NoError(t, r.Update(context.Background(), ws))
	_, err := r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	require.True(t, podExists(t, r, pod.Name), "busy turn + digest change + pending generation: pod still untouched")

	// Turn ends (idle) → the drain releases the restart-gen recycle → the
	// pod is deleted and handleCreating rebuilds it with digest B.
	stub.set(idleStatusz())
	_, err = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
	require.NoError(t, err)
	assert.False(t, podExists(t, r, pod.Name), "idle + generation bump: the unrelated recycle proceeds")

	// Drive Creating reconciles until buildPod lands the rebuilt pod.
	rebuilt := &corev1.Pod{}
	require.Eventually(t, func() bool {
		_, _ = r.Reconcile(context.Background(), reqFor(ws.Name, "default"))
		return r.Get(context.Background(), types.NamespacedName{Name: pod.Name, Namespace: "default"}, rebuilt) == nil
	}, 5*time.Second, 100*time.Millisecond, "handleCreating must rebuild the pod")
	assert.Equal(t, upgradeDigestB, agentdVolumeRef(rebuilt),
		"the recreated pod must carry the NEW overlay digest — the lazy roll rolls")
	envMapRebuilt := envMap(envOf(rebuilt, "workspace"))
	assert.Equal(t, upgradePinAmd64B, envMapRebuilt["LLMSAFESPACES_AGENTD_SHA256_AMD64"],
		"the recreated pod must carry the NEW resolved binary pin")
}

// envOf returns the env vars of the named container ("" = first container).
func envOf(pod *corev1.Pod, container string) []corev1.EnvVar {
	for _, c := range pod.Spec.Containers {
		if container == "" || c.Name == container {
			return c.Env
		}
	}
	return nil
}

// --- drift guard: the deletion-path inventory -------------------------------

// classifiedDeletionPaths is the #761 drain matrix as a source inventory:
// every (file, enclosing function) that calls deletePodByName, with the
// ruling that makes its drain classification deliberate. A NEW call site
// fails TestUpgrade_DeletionPathInventory until it is classified HERE —
// the point is that adding a controller-initiated pod deletion is a
// design decision (which drain class? which event/metric?), never a
// drive-by.
var classifiedDeletionPaths = map[string]map[string]string{
	"phase_active.go": {
		"handleActive": "#761 drain: restart-gen (user-consent bypass via #1505), password-secret heal (fails open), arch drift",
	},
	"phase_creating.go": {
		"handleCreating": "non-serving pod only: gen-bump rebuild, Failed/Pending-stuck/init-crashloop recovery (#935)",
	},
	"phase_suspend.go": {
		"suspendFromPreActive": "pre-Active: no user turns possible",
		"handleSuspending":     "#1507 owner ruling: grace-only (40s), never waits — Spec.Suspend, org D20, idle, timeout",
	},
	"health.go": {
		"restartAgentPod": "dead agentd cannot be drained (#761 by-design exemption)",
	},
	"phase_terminating.go": {
		"handleTerminating": "explicit user destroy — intent trumps in-flight turns",
	},
	"recovery.go": {
		"handleFailed": "legacy Failed self-heal: pod not healthy",
	},
}

// TestUpgrade_DeletionPathInventory (the opencode_overlay_contract_test.go
// source-guard pattern): parses every non-test source file in this package
// and asserts each deletePodByName call site is present in
// classifiedDeletionPaths. Fails with a classification prompt on any new
// or moved site.
func TestUpgrade_DeletionPathInventory(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, entries, "no package sources found — wrong working directory?")

	seen := map[string]bool{}
	for _, path := range entries {
		if len(path) >= 8 && path[len(path)-8:] == "_test.go" {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, perr, "parsing %s", path)
		base := filepath.Base(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "deletePodByName" {
					return true
				}
				key := base + ":" + fn.Name.Name
				seen[key] = true
				classification, known := classifiedDeletionPaths[base][fn.Name.Name]
				require.True(t, known,
					"%s calls deletePodByName but is NOT in the classified deletion-path inventory (upgrade_rollout_kill_test.go).\n"+
						"Adding a controller-initiated pod deletion is a drain-design decision (#761/#1507/#1724): classify it in classifiedDeletionPaths with its ruling, or route it through the drain.", key)
				require.NotEmpty(t, classification)
				return true
			})
		}
	}

	// Every inventory entry must still exist on disk — stale entries are
	// deleted coverage debt, not documentation.
	for base, fns := range classifiedDeletionPaths {
		for fnName := range fns {
			require.True(t, seen[base+":"+fnName],
				"classified deletion path %s:%s no longer exists — remove or update its inventory entry", base, fnName)
		}
	}
}
