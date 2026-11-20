// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// crd_skew.go — the CRD-skew observability arm of issue #2092: the
// recurring skew verdict (previously a 10-minute-cadence ERROR line in
// controller logs) promoted to the llmsafespaces_crd_skew gauge, plus
// the STARTUP capability probe that makes the verdict available before
// any reconcile observes a pruned write.
//
// The gauge is sticky per (crd, field) series: skewed=1 on a pruned
// verdict, healed=0 the moment a verify lands. The alert
// (LLMSafeSpacesCRDSkew, helm/templates/prometheus-rules.yaml) rides
// that stickiness — one page for a fleet-wide wedge, auto-resolution on
// the CRD apply, no per-workspace storm (#2047 fatigue discipline).

// The one (crd, field) pair this controller currently verdicts on. The
// gauge is shaped for more (any future pruned-field class joins the
// same series family), but there is exactly one today.
const (
	crdSkewWorkspaceCRD = "workspaces.llmsafespaces.dev"
	crdSkewHarnessField = "spec.harness"
)

// recordCRDSkewSet marks the (crd, field) series skewed (1).
func recordCRDSkewSet() {
	metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField).Set(1)
}

// recordCRDSkewHealed flips the (crd, field) series back to 0 — the
// resolution edge. Idempotent (Set), so calling it on every landed
// verify is free once the series reads 0.
func recordCRDSkewHealed() {
	metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField).Set(0)
}

// ProbeHarnessCRDCapability — the startup + on-demand capability probe
// (issue #2092 ruling 4's controller-side twin; the API tier carries
// its own cached probe for create-time refusal). It settles the gauge
// honestly WITHOUT waiting for a pruned write to happen: a server-side
// DRY-RUN create of a minimal Workspace carrying spec.harness runs the
// full apiserver pipeline (admission, structural pruning) and returns
// the object as it would be persisted — nothing is stored. If the
// returned object still carries spec.harness the cluster CRD has the
// field (healed); if it comes back pruned the cluster CRD predates the
// bind-once schema (skewed).
//
// Any probe error is INCONCLUSIVE, not evidence (the #1685
// absence-is-not-evidence posture): the gauge is left untouched and the
// runtime detectors (backfill read-back, bind verify) remain the
// authority. The probe needs no RBAC beyond the workspace create verb
// the controller already holds (DryRun rides the same verb) and no
// CRD-read grant.
//
// Var-indirection for tests.
var probeHarnessCRDDryRun = func(ctx context.Context, c client.Client, namespace string) (skewed bool, conclusive bool) {
	probe := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "crd-skew-probe-",
			Namespace:    namespace,
		},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "crd-skew-probe"},
			Runtime: "python-3.11", // no '/': a RuntimeEnvironment NAME, no registry allow-list leg
			Storage: v1.WorkspaceStorageConfig{Size: "1Gi"},
			Harness: v1.HarnessOpencode,
		},
	}
	opts := &client.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if err := c.Create(ctx, probe, opts); err != nil {
		return false, false
	}
	return probe.Spec.Harness == "", true
}

// probeHarnessCRDCapabilityOnce — the startup capability probe, run on
// the FIRST workspace reconcile of the process (issue #2092 ruling 4's
// controller-side twin; the API tier carries its own cached probe for
// create-time refusal). It settles the gauge honestly WITHOUT waiting
// for a pruned write to happen: a server-side DRY-RUN create of a
// minimal Workspace carrying spec.harness runs the full apiserver
// pipeline (admission, structural pruning) and returns the object as it
// would be persisted — nothing is stored. If the returned object still
// carries spec.harness the cluster CRD has the field (healed); if it
// comes back pruned the cluster CRD predates the bind-once schema
// (skewed).
//
// Riding the first reconcile (not a boot hook) is deliberate: the
// reconciled workspace's namespace is by construction one the
// controller holds create RBAC for, so the dry-run cannot be refused
// for namespace reasons on namespaced-RBAC deployments — and a cluster
// with zero workspaces has no skew surface to observe anyway.
//
// Any probe error is INCONCLUSIVE, not evidence (the #1685
// absence-is-not-evidence posture): the gauge is left untouched and the
// runtime detectors (backfill read-back, bind verify) remain the
// authority. Never fatal, exactly once per process.
func (r *WorkspaceReconciler) probeHarnessCRDCapabilityOnce(ctx context.Context, namespace string) {
	r.harnessCRDProbeMu.Lock()
	if r.harnessCRDStartupProbed {
		r.harnessCRDProbeMu.Unlock()
		return
	}
	r.harnessCRDStartupProbed = true
	r.harnessCRDProbeMu.Unlock()

	logger := log.FromContext(ctx)
	skewed, conclusive := probeHarnessCRDDryRun(ctx, r.Client, namespace)
	switch {
	case !conclusive:
		logger.Info("Workspace CRD capability probe inconclusive (dry-run create refused); leaving the skew gauge to the runtime detectors",
			"crd", crdSkewWorkspaceCRD, "field", crdSkewHarnessField)
	case skewed:
		recordCRDSkewSet()
		logger.Error(nil, "cluster Workspace CRD predates 0.44.15 (spec.harness): dry-run probe came back pruned — helm does not upgrade crds/; apply the CRD (`kubectl apply -f helm/crds/workspace.yaml` or bump the Flux crds path; see docs/operator/upgrading.md, CRD skew recovery)",
			"crd", crdSkewWorkspaceCRD, "field", crdSkewHarnessField)
	default:
		recordCRDSkewHealed()
		logger.Info("Workspace CRD carries spec.harness (capability probe conclusive: supported)",
			"crd", crdSkewWorkspaceCRD, "field", crdSkewHarnessField)
	}
}

// reprobeHarnessCRDAfterHealWindow is the on-skew re-probe cadence: once
// the gauge is skewed, the probe re-runs at this interval (piggybacked
// on reconciles, never a new loop — see harnessBindingRetryProbe below)
// so a CRD applied mid-flight flips the gauge to healed within minutes
// even if no legacy workspace happens to backfill over it. Var for tests.
var reprobeHarnessCRDAfterHealWindow = 10 * time.Minute

// crdSkewProbeKey — the re-probe rides the ONE series this file owns.
const crdSkewProbeKey = crdSkewWorkspaceCRD + "/" + crdSkewHarnessField

// maybeReprobeHarnessCRD re-runs the capability probe at the heal-window
// cadence while the skew verdict is live. Called from the reconciler's
// skew verdict sites (never a standalone loop): the 10m controller
// cadence that already exists IS the scheduler. A conclusive HEALTHY
// verdict heals the gauge (resolution on heal, without waiting for a
// legacy workspace to exist); a conclusive skewed verdict re-affirms 1;
// inconclusive changes nothing.
func (r *WorkspaceReconciler) maybeReprobeHarnessCRD(ctx context.Context, ws *v1.Workspace) {
	r.harnessCRDProbeMu.Lock()
	if r.harnessCRDProbeAt == nil {
		r.harnessCRDProbeAt = make(map[string]time.Time)
	}
	last, seen := r.harnessCRDProbeAt[crdSkewProbeKey]
	due := !seen || time.Since(last) >= reprobeHarnessCRDAfterHealWindow
	if due {
		r.harnessCRDProbeAt[crdSkewProbeKey] = time.Now()
	}
	r.harnessCRDProbeMu.Unlock()
	if !due {
		return
	}
	skewed, conclusive := probeHarnessCRDDryRun(ctx, r.Client, ws.Namespace)
	if !conclusive {
		return
	}
	if skewed {
		recordCRDSkewSet()
		return
	}
	recordCRDSkewHealed()
}
