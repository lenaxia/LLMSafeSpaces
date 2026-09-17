//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Envtest integration for the #760 recovery-exhaustion escalation:
//
//  1. The derived RecoveryExhausted condition must round-trip through a
//     real Kubernetes API server — a status-subresource write against
//     the shipped CRD schema (helm/crds/workspace.yaml), proving the
//     schema admits the new condition and the controller write path
//     works against real admission, not just the fake client.
//  2. The retired status.safeMode field must be pruned by the API
//     server on write — the #760 storage note: objects written by older
//     controllers may still carry the field in etcd; the next status
//     write (any writer) drops it because the CRD no longer declares it.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestRecoveryExhausted
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

var workspaceGVK = schema.GroupVersionKind{Group: v1.GroupName, Version: v1.GroupVersion, Kind: "Workspace"}

func unstructuredWorkspace(t *testing.T, dyn client.Client, key types.NamespacedName) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(workspaceGVK)
	require.NoError(t, dyn.Get(context.Background(), key, u))
	return u
}

func TestEnvtestRecoveryExhausted_ConditionRoundTrip(t *testing.T) {
	cfg := startEnvtest(t)
	scheme := testScheme(t)
	dyn, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()

	ws := makeWorkspace("ws-envtest-exhaust", "default", v1.WorkspacePhaseCreating)
	require.NoError(t, dyn.Create(ctx, ws))

	// Bring the persisted object one failure away from Infrastructure
	// exhaustion, mirroring N prior enterRecovery episodes.
	created := &v1.Workspace{}
	require.NoError(t, dyn.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, created))
	created.Status.ConsecutiveFailures = recoveryPolicies[FailureClassInfrastructure].ExhaustionAfter - 1
	require.NoError(t, dyn.Status().Update(ctx, created))

	// Drive the real crossing through the production write path against
	// the real API server.
	r := &WorkspaceReconciler{Client: dyn, Scheme: scheme, Recorder: record.NewFakeRecorder(8)}
	latest := &v1.Workspace{}
	require.NoError(t, dyn.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, latest))
	_, err = r.enterRecovery(ctx, latest, FailureClassInfrastructure)
	require.NoError(t, err)

	got := &v1.Workspace{}
	require.NoError(t, dyn.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, got))
	cond := recoveryExhaustedCondition(got)
	require.NotNil(t, cond, "the RecoveryExhausted condition must survive the real API server round-trip")
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, v1.ReasonRecoveryExhausted, cond.Reason)
	assert.Equal(t, recoveryPolicies[FailureClassInfrastructure].ExhaustionAfter, got.Status.ConsecutiveFailures)
	assert.True(t, hasEvent(eventsFrom(r.Recorder.(*record.FakeRecorder)), v1.ReasonRecoveryExhausted),
		"the crossing must emit the warning event alongside the persisted condition")
}

// TestEnvtestRecoveryExhausted_SafeModePrunedOnWrite pins the storage
// note for the retired status.safeMode field: the shipped CRD no longer
// declares it, so the API server prunes it from any status write —
// stale values on old objects vanish on their next write.
func TestEnvtestRecoveryExhausted_SafeModePrunedOnWrite(t *testing.T) {
	cfg := startEnvtest(t)
	scheme := testScheme(t)
	dyn, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()

	ws := makeWorkspace("ws-envtest-prune", "default", v1.WorkspacePhaseCreating)
	require.NoError(t, dyn.Create(ctx, ws))

	// Simulate an old-controller status write that still carries the
	// retired field (unstructured, since the typed status no longer has it).
	key := types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}
	u := unstructuredWorkspace(t, dyn, key)
	require.NoError(t, unstructured.SetNestedField(u.Object, true, "status", "safeMode"))
	require.NoError(t, dyn.Status().Update(ctx, u))

	// Read the stored object back raw: the field must be gone.
	stored := unstructuredWorkspace(t, dyn, key)
	_, found, err := unstructured.NestedFieldNoCopy(stored.Object, "status", "safeMode")
	require.NoError(t, err)
	assert.False(t, found, "status.safeMode must be pruned by the API server on write (field removed from the CRD)")
}
