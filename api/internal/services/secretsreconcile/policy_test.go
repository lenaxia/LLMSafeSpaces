// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secretsreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
)

// fakePolicy records SyncGlobalDefaultBindings calls in call order so
// tests can pin sequencing against the revision seam.
type fakePolicy struct {
	calls []string // append-only event log: "sync:<ws>:<owner>" / "sync-err:<ws>"
	err   map[string]error
}

func (f *fakePolicy) SyncGlobalDefaultBindings(_ context.Context, ownerUserID, workspaceID string) ([]string, []string, error) {
	if err, ok := f.err[workspaceID]; ok {
		f.calls = append(f.calls, "sync-err:"+workspaceID)
		return nil, nil, err
	}
	f.calls = append(f.calls, "sync:"+workspaceID+":"+ownerUserID)
	return nil, nil, nil
}

// orderRecorder wraps the revision seam, appending an event whenever
// ManifestFor runs so tests can assert the policy sync happens BEFORE
// the manifest derivation in the same pass.
type orderRecorder struct {
	*fakeRevisions
	events *[]string
}

func (o *orderRecorder) ManifestFor(ctx context.Context, owner, ws string) (string, error) {
	*o.events = append(*o.events, "manifest:"+ws)
	return o.fakeRevisions.ManifestFor(ctx, owner, ws)
}

// TestRunPass_PolicySyncRunsBeforeManifest: policy convergence must
// materialize bindings BEFORE the pass derives the live manifest, so a
// flag flip that adds bindings diverges the manifest in the SAME pass
// (mint + notify follow the existing machinery).
func TestRunPass_PolicySyncRunsBeforeManifest(t *testing.T) {
	resetReconcileMetrics()
	policy := &fakePolicy{err: map[string]error{}}
	revisions := newFakeRevisions(map[string]fakeRevRow{})
	var events []string
	rec := &orderRecorder{fakeRevisions: revisions, events: &events}
	svc := New(&fakeLister{}, rec, &fakeNotifier{}, WithPolicySource(policy))

	ws := ActiveWorkspace{WorkspaceID: "ws-1", OwnerUserID: "user-1"}
	svc.reconcileWorkspace(context.Background(), ws)

	require.Len(t, events, 1, "manifest must be derived exactly once")
	assert.Equal(t, []string{"sync:ws-1:user-1", "manifest:ws-1"}, append(policy.calls, events...))
}

// changePolicy scripts a deterministic add/remove result.
type changePolicy struct{}

func (changePolicy) SyncGlobalDefaultBindings(context.Context, string, string) ([]string, []string, error) {
	return []string{"a", "b"}, []string{"c"}, nil
}

// TestRunPass_PolicyChangeCountsMetric: adds/removes from the policy
// step increment the policy-bindings counter by op.
func TestRunPass_PolicyChangeCountsMetric(t *testing.T) {
	resetReconcileMetrics()
	metrics.SecretsPolicyBindingsCounter().Reset()
	addedBefore := testutil.ToFloat64(metrics.SecretsPolicyBindingsCounter().WithLabelValues("added"))
	removedBefore := testutil.ToFloat64(metrics.SecretsPolicyBindingsCounter().WithLabelValues("removed"))

	lister := &fakeLister{list: []ActiveWorkspace{{WorkspaceID: "ws-1", OwnerUserID: "user-1"}}}
	svc := New(lister, newFakeRevisions(map[string]fakeRevRow{}), &fakeNotifier{}, WithPolicySource(changePolicy{}))
	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, float64(2), testutil.ToFloat64(metrics.SecretsPolicyBindingsCounter().WithLabelValues("added"))-addedBefore)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.SecretsPolicyBindingsCounter().WithLabelValues("removed"))-removedBefore)
}

// TestRunPass_PolicyErrorSkipsWorkspace: a failing policy sync skips
// the workspace's manifest/mint/notify steps for this pass (counted as
// policy_sync) — the next pass retries. One bad workspace must not
// blind the rest of the pass.
func TestRunPass_PolicyErrorSkipsWorkspace(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-bad", OwnerUserID: "user-1"},
		{WorkspaceID: "ws-good", OwnerUserID: "user-1"},
	}}
	revisions := newFakeRevisions(map[string]fakeRevRow{})
	policy := &fakePolicy{err: map[string]error{"ws-bad": errors.New("db outage")}}
	svc := New(lister, revisions, &fakeNotifier{}, WithPolicySource(policy))

	skipsBefore := skipCount(t, "policy_sync")
	require.NoError(t, svc.runPass(context.Background()))

	assert.Equal(t, float64(1), skipCount(t, "policy_sync")-skipsBefore)
	assert.Equal(t, []string{"sync-err:ws-bad", "sync:ws-good:user-1"}, policy.calls)
	// The good workspace still derived its manifest and minted its
	// first revision (no row existed); the bad one never reached the
	// manifest step.
	assert.Equal(t, 1, revisions.mints["ws-good"])
	assert.NotContains(t, revisions.mints, "ws-bad")
}

// TestRunPass_NilPolicySourceStillReconciles: the policy seam is
// optional (nil = disabled) so existing wiring and tests keep the
// pre-policy behavior byte-for-byte.
func TestRunPass_NilPolicySourceStillReconciles(t *testing.T) {
	resetReconcileMetrics()
	lister := &fakeLister{list: []ActiveWorkspace{{WorkspaceID: "ws-1", OwnerUserID: "user-1"}}}
	revisions := newFakeRevisions(map[string]fakeRevRow{})
	svc := New(lister, revisions, &fakeNotifier{})

	require.NoError(t, svc.runPass(context.Background()))
	assert.Equal(t, 1, revisions.mints["ws-1"])
}
