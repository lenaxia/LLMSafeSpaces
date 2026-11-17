// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// crd_skew_test.go — the #2092 observability + UX pins at the
// controller tier: the llmsafespaces_crd_skew gauge's set/heal edges,
// the once-per-process startup capability probe, the on-skew re-probe
// cadence (resolution-on-heal without a standalone loop), and the
// UNBOUND GATE's tolerance of the degraded BindNotPersisted verdict
// (ruling 3 — the API's marker must not be rewritten away).
//
// Born-red: the parked-tolerance pin fails on pre-change main (the gate
// rewrote any non-HarnessUnbound condition back to the parked verdict,
// erasing the degraded surface on the next reconcile).

// --- the gauge ------------------------------------------------------------

func TestCRDSkewGauge_SetAndHeal(t *testing.T) {
	resetCRDSkewGauge(t)
	recordCRDSkewSet()
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField)),
		"the skew verdict sets the (crd, field) series to 1")
	recordCRDSkewHealed()
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField)),
		"heal evidence flips the series back to 0 — the alert's resolution edge")
}

func resetCRDSkewGauge(t *testing.T) {
	t.Helper()
	metrics.CRDSkew.Reset()
	t.Cleanup(func() { metrics.CRDSkew.Reset() })
}

// --- the probe body ---------------------------------------------------------

// pruningCreateInterceptor simulates the skewed cluster for the probe's
// DRY-RUN create: the create is accepted, the unknown spec.harness
// field is silently dropped from the RETURNED object (structural
// pruning), and the dry-run option is captured (nothing persists).
func pruningCreateInterceptor(pruning *bool) (interceptor.Funcs, *int) {
	dryRunCreates := 0
	return interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			for _, o := range opts {
				if _, ok := o.(interface {
					ApplyToCreate(*ctrlclient.CreateOptions)
				}); ok {
					// counted below via the merged options
				}
			}
			co := &ctrlclient.CreateOptions{}
			co.ApplyOptions(opts)
			if len(co.DryRun) > 0 {
				dryRunCreates++
			}
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}
			if ws, isWS := obj.(*v1.Workspace); isWS && *pruning {
				ws.Spec.Harness = "" // pruned from the would-be-persisted object
			}
			return nil
		},
	}, &dryRunCreates
}

func crdSkewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	return scheme
}

// The probe verdicts: PRUNED response = skewed+conclusive (the gauge
// arms from boot, before any reconcile observes a pruned write).
func TestProbeHarnessCRDDryRun_SkewedClusterIsConclusive(t *testing.T) {
	pruning := true
	intc, dryRunCreates := pruningCreateInterceptor(&pruning)
	c := fake.NewClientBuilder().WithScheme(crdSkewScheme(t)).WithInterceptorFuncs(intc).Build()

	skewed, conclusive := probeHarnessCRDDryRun(context.Background(), c, "default")
	assert.True(t, conclusive, "an admitted dry-run create is conclusive evidence")
	assert.True(t, skewed, "a pruned read-back is the skew verdict")
	assert.Equal(t, 1, *dryRunCreates, "the probe rides DryRun (nothing persists)")

	// Heal mid-flight: the operator applies the CRD — the same probe now
	// keeps the field.
	pruning = false
	skewed, conclusive = probeHarnessCRDDryRun(context.Background(), c, "default")
	assert.True(t, conclusive)
	assert.False(t, skewed, "a kept field reads healthy (heal evidence)")
}

// A refused create is INCONCLUSIVE, never a skew verdict (the #1685
// absence-is-not-evidence posture): RBAC/webhook/apiserver errors must
// not arm the gauge.
func TestProbeHarnessCRDDryRun_ErrorIsInconclusive(t *testing.T) {
	boom := interceptor.Funcs{
		Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
			return assert.AnError
		},
	}
	c := fake.NewClientBuilder().WithScheme(crdSkewScheme(t)).WithInterceptorFuncs(boom).Build()
	skewed, conclusive := probeHarnessCRDDryRun(context.Background(), c, "default")
	assert.False(t, conclusive, "an errored probe is not evidence")
	assert.False(t, skewed)
}

// --- the once-per-process startup probe --------------------------------------

func TestProbeHarnessCRDCapabilityOnce_RunsExactlyOncePerProcess(t *testing.T) {
	r, _ := newHarnessBindingReconciler(t)
	calls := 0
	restore := stubProbeHarnessCRD(func(_ context.Context, _ ctrlclient.Client, _ string) (bool, bool) {
		calls++
		return false, true
	})
	defer restore()

	r.probeHarnessCRDCapabilityOnce(context.Background(), "default")
	r.probeHarnessCRDCapabilityOnce(context.Background(), "other")
	assert.Equal(t, 1, calls, "the startup probe is once per reconciler process, not per reconcile")
}

// --- the on-skew re-probe cadence ---------------------------------------------

func TestMaybeReprobeHarnessCRD_CadenceAndHeal(t *testing.T) {
	resetCRDSkewGauge(t)
	r, _ := newHarnessBindingReconciler(t)
	ws := makeWorkspace("ws-reprobe", "default", v1.WorkspacePhaseCreating)

	calls := 0
	restore := stubProbeHarnessCRD(func(_ context.Context, _ ctrlclient.Client, _ string) (bool, bool) {
		calls++
		return false, true // conclusive healthy
	})
	defer restore()

	r.maybeReprobeHarnessCRD(context.Background(), ws)
	assert.Equal(t, 1, calls, "first call probes (no record)")
	r.maybeReprobeHarnessCRD(context.Background(), ws)
	assert.Equal(t, 1, calls, "within the window: no re-probe (piggybacked cadence, no loop)")
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField)),
		"a conclusive healthy verdict heals the gauge — resolution without a legacy workspace")

	// The window elapses (the operator has had time to apply the CRD).
	r.harnessCRDProbeMu.Lock()
	r.harnessCRDProbeAt[crdSkewProbeKey] = time.Now().Add(-2 * reprobeHarnessCRDAfterHealWindow)
	r.harnessCRDProbeMu.Unlock()
	r.maybeReprobeHarnessCRD(context.Background(), ws)
	assert.Equal(t, 2, calls, "past the window the probe re-runs")
}

func TestMaybeReprobeHarnessCRD_SkewedReaffirms(t *testing.T) {
	resetCRDSkewGauge(t)
	r, _ := newHarnessBindingReconciler(t)
	ws := makeWorkspace("ws-reprobe2", "default", v1.WorkspacePhaseCreating)

	restore := stubProbeHarnessCRD(func(_ context.Context, _ ctrlclient.Client, _ string) (bool, bool) {
		return true, true // conclusive skewed
	})
	defer restore()

	r.maybeReprobeHarnessCRD(context.Background(), ws)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.CRDSkew.WithLabelValues(crdSkewWorkspaceCRD, crdSkewHarnessField)),
		"a conclusive skewed re-probe re-affirms the series at 1")
}

// stubProbeHarnessCRD swaps the probe body var and returns a restore fn.
func stubProbeHarnessCRD(fn func(ctx context.Context, c ctrlclient.Client, namespace string) (bool, bool)) func() {
	old := probeHarnessCRDDryRun
	probeHarnessCRDDryRun = fn
	return func() { probeHarnessCRDDryRun = old }
}

// --- the unbound gate's tolerance of the degraded verdict (ruling 3) ---------

// THE parked-tolerance pin: a True/BindNotPersisted condition is PARKED
// — the gate must not rewrite it back to HarnessUnbound (the
// picker/sidebar's "blocked on cluster CRD" hint would flap on every
// reconcile), and no status write happens at all.
func TestGateUnboundCreating_BindNotPersistedIsParked(t *testing.T) {
	statusWrites := 0
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	ws := makeWorkspace("ws-degraded", "default", v1.WorkspacePhaseCreating)
	ws.Spec.Harness = "" // born-unbound (makeWorkspace's test-seam default stamps opencode)
	ws.Annotations = map[string]string{v1.AnnotationHarnessUnbound: "true"}
	ws.Status.Conditions = []v1.WorkspaceCondition{{
		Type:    v1.WorkspaceConditionHarnessBindingPending,
		Status:  "True",
		Reason:  v1.WorkspaceReasonBindNotPersisted,
		Message: "bind attempts are failing to persist (the #2092 degraded verdict)",
	}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1.Workspace{}).
		WithObjects(ws).
		WithInterceptorFuncs(interceptor.Funcs{
			// SubResource writes ride the fake's Update on the status
			// subresource — count every one.
			Update: func(ctx context.Context, cl ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
				statusWrites++
				return cl.Update(ctx, obj, opts...)
			},
		}).
		Build()
	r := &WorkspaceReconciler{Client: c, Scheme: scheme}

	handled, err := r.gateUnboundCreating(context.Background(), ws)
	require.NoError(t, err)
	assert.True(t, handled, "the degraded unbound workspace is still gated (no pod)")
	assert.Equal(t, 0, statusWrites, "a parked degraded verdict writes NOTHING — no flap, no churn")
	assert.Equal(t, v1.WorkspaceReasonBindNotPersisted, ws.Status.Conditions[0].Reason,
		"the degraded reason stands (not rewritten to HarnessUnbound)")
}

// The helper's full truth table: both steady reasons park; the bound
// flip (False) and absence do not.
func TestHarnessBindingPendingIsParked_Reasons(t *testing.T) {
	parked := func(reason string, status string) bool {
		ws := makeWorkspace("ws-r", "default", v1.WorkspacePhaseCreating)
		ws.Status.Conditions = []v1.WorkspaceCondition{{
			Type: v1.WorkspaceConditionHarnessBindingPending, Status: status, Reason: reason,
		}}
		return harnessBindingPendingIsParked(ws)
	}
	assert.True(t, parked("HarnessUnbound", "True"), "the steady unbound wait parks")
	assert.True(t, parked(v1.WorkspaceReasonBindNotPersisted, "True"), "the degraded verdict parks")
	assert.False(t, parked(v1.WorkspaceReasonBindNotPersisted, "False"), "a bound flip never parks")
	assert.False(t, parked("SomethingElse", "True"), "an unknown reason is not steady state")

	ws := makeWorkspace("ws-none", "default", v1.WorkspacePhaseCreating)
	assert.False(t, harnessBindingPendingIsParked(ws), "no condition = not parked (the gate writes one)")
}
