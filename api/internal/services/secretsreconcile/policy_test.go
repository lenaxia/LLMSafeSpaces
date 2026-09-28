// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secretsreconcile

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
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

// TestRunPass_EmptyOwnerSkipsPolicyStep: an unparseable CRD owner
// (empty spec.owner.userID) must not strip the workspace's auto rows —
// an empty keep-set reads as "owner has no defaults". The manifest
// tier was already owner-keyed (pre-existing), so the workspace is
// skipped by the manifest step regardless; the guard keeps step 0
// from being the first destructive reader.
func TestRunPass_EmptyOwnerSkipsPolicyStep(t *testing.T) {
	resetReconcileMetrics()
	store := newCompositionStore()
	store.rows["ws-1"] = compositionRevRow{seq: 1, hash: "h0"}
	svc := secrets.NewSecretService(nil, store)

	policy := &recordingPolicy{}
	lister := &fakeLister{list: []ActiveWorkspace{{WorkspaceID: "ws-1"}}}
	loop := New(lister, svc, &fakeNotifier{}, WithPolicySource(policy))

	require.NoError(t, loop.runPass(context.Background()))
	assert.Empty(t, policy.calls, "empty owner must skip the policy step entirely")
}

// recordingPolicy is a PolicySource that only records calls.
type recordingPolicy struct {
	calls []string
}

func (p *recordingPolicy) SyncGlobalDefaultBindings(_ context.Context, owner, ws string) ([]string, []string, error) {
	p.calls = append(p.calls, owner+"/"+ws)
	return nil, nil, nil
}

// --- composition fixture: a real SecretService over an in-memory
// --- SecretStore+CredentialStore+RevisionStore, exercised by the real
// --- loop — the regression shape of the production bug.

type compositionRevRow struct {
	seq  int64
	hash string
}

type compositionStore struct {
	mu       sync.Mutex
	secrets  map[string]*secrets.UserSecret
	bindings map[string]map[string]string // ws -> secretID -> source
	rows     map[string]compositionRevRow
	mints    map[string]int
}

func newCompositionStore() *compositionStore {
	return &compositionStore{
		secrets:  make(map[string]*secrets.UserSecret),
		bindings: make(map[string]map[string]string),
		rows:     make(map[string]compositionRevRow),
		mints:    make(map[string]int),
	}
}

func (s *compositionStore) seedGlobalDefault(id, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[id] = &secrets.UserSecret{ID: id, UserID: userID, Name: id, GlobalDefault: true}
}

func (s *compositionStore) boundSources(ws string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.bindings[ws]))
	for k, v := range s.bindings[ws] {
		out[k] = v
	}
	return out
}

// SecretStore

func (s *compositionStore) CreateSecret(_ context.Context, sec *secrets.UserSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets[sec.ID] = sec
	return nil
}
func (s *compositionStore) GetSecret(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	return nil, nil
}
func (s *compositionStore) GetSecretByName(_ context.Context, _, _ string) (*secrets.UserSecret, error) {
	return nil, nil
}
func (s *compositionStore) ListSecrets(context.Context, string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *compositionStore) ListGlobalDefaultSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*secrets.UserSecret
	for _, sec := range s.secrets {
		if sec.UserID == userID && sec.GlobalDefault {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *compositionStore) UpdateSecret(context.Context, *secrets.UserSecret) error { return nil }
func (s *compositionStore) DeleteSecret(context.Context, string, string) error      { return nil }
func (s *compositionStore) ReEncryptUserSecrets(context.Context, string, int, func([]byte) ([]byte, error), func(context.Context) error) error {
	return nil
}
func (s *compositionStore) SetBindings(_ context.Context, ws string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(ids))
	for _, id := range ids {
		next[id] = secrets.BindSourceManual
	}
	s.bindings[ws] = next
	return nil
}
func (s *compositionStore) AddBindings(context.Context, string, []string) error { return nil }
func (s *compositionStore) SyncGlobalDefaultBindings(_ context.Context, ws string, ids []string) ([]string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	rows := s.bindings[ws]
	if rows == nil {
		rows = make(map[string]string)
	}
	var added, removed []string
	for id := range want {
		if _, ok := rows[id]; !ok {
			rows[id] = secrets.BindSourceGlobalDefault
			added = append(added, id)
		}
	}
	for id, src := range rows {
		if src != secrets.BindSourceGlobalDefault {
			continue
		}
		if _, ok := want[id]; !ok {
			delete(rows, id)
			removed = append(removed, id)
		}
	}
	if len(rows) > 0 {
		s.bindings[ws] = rows
	} else {
		delete(s.bindings, ws)
	}
	return added, removed, nil
}
func (s *compositionStore) GetBindings(_ context.Context, ws string) ([]*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*secrets.UserSecret
	for id := range s.bindings[ws] {
		if sec, ok := s.secrets[id]; ok {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *compositionStore) GetBindingsForSecret(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s *compositionStore) LogAudit(context.Context, *secrets.AuditEntry) error { return nil }
func (s *compositionStore) QueryAudit(context.Context, string, secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	return nil, nil
}

// CredentialStore

func (s *compositionStore) GetWorkspaceCredentials(context.Context, string) ([]secrets.CredentialBinding, error) {
	return nil, nil
}
func (s *compositionStore) UpsertFreeTierCredential(context.Context, []byte) error { return nil }
func (s *compositionStore) SeedWorkspaceCredentials(context.Context, string, string, *string) error {
	return nil
}
func (s *compositionStore) BindCredentialToAllUserWorkspaces(context.Context, string, string) error {
	return nil
}
func (s *compositionStore) HasUserProviderCredential(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *compositionStore) GetWorkspaceMCPServers(context.Context, string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}

// RevisionStore

func (s *compositionStore) CurrentRevision(_ context.Context, ws string) (int64, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[ws]
	return row.seq, row.hash, ok, nil
}
func (s *compositionStore) EnsureRevision(_ context.Context, ws, hash string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[ws]; ok && row.hash == hash {
		return row.seq, nil
	}
	s.mints[ws]++
	next := int64(1)
	if row, ok := s.rows[ws]; ok {
		next = row.seq + 1
	}
	s.rows[ws] = compositionRevRow{seq: next, hash: hash}
	return next, nil
}

// TestRunPass_Composition_PolicyMaterializesForExistingWorkspace is the
// regression test for the production bug, through the REAL composition
// the loop uses in production: a real SecretService (as both
// RevisionSource and PolicySource) over a store, one pass over an
// Active workspace that predates the secret. Without step 0 the flag
// flip is invisible: no binding row, no manifest drift, no mint, no
// notify — exactly the shipped bug. With it: the row materializes, the
// live manifest diverges, a new seq mints, and the pod is notified.
func TestRunPass_Composition_PolicyMaterializesForExistingWorkspace(t *testing.T) {
	resetReconcileMetrics()
	store := newCompositionStore()
	const wsID, ownerID = "ws-1", "user-1"

	// The workspace is converged on the EMPTY manifest: no bindings,
	// stored row seq=1 over the owner's empty-set hash, pod applied 1.
	emptyHash := secrets.ManifestHash(ownerID, nil)
	store.rows[wsID] = compositionRevRow{seq: 1, hash: emptyHash}

	// The global-default secret is created AFTER the workspace exists.
	store.seedGlobalDefault("sec-gd", ownerID)

	svc := secrets.NewSecretService(nil, store)
	lister := &fakeLister{list: []ActiveWorkspace{{WorkspaceID: wsID, OwnerUserID: ownerID, SpawnedRev: "1:" + emptyHash + ":content"}}}
	notifier := &fakeNotifier{}
	loop := New(lister, svc, notifier, WithPolicySource(svc))

	require.NoError(t, loop.runPass(context.Background()))

	// The policy materialized a provenance-tagged binding row.
	assert.Equal(t, map[string]string{"sec-gd": secrets.BindSourceGlobalDefault}, store.boundSources(wsID))

	// The live manifest diverged and a new seq minted.
	hash, err := svc.ManifestFor(context.Background(), ownerID, wsID)
	require.NoError(t, err)
	assert.NotEqual(t, emptyHash, hash)
	seq, _, ok, err := svc.CurrentRevision(context.Background(), wsID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(2), seq, "materialized policy must mint the next seq")

	// The pod's applied seq now lags and was notified.
	assert.Len(t, notifier.dispatched(), 1)
}

// TestRunPass_Composition_FlagFlipRetractsAutoRowButNotManual: through
// the real composition, turning the flag OFF retracts the auto row and
// re-converges delivery — while a manual row for the same secret is
// never touched (the provenance contract).
func TestRunPass_Composition_FlagFlipRetractsAutoRowButNotManual(t *testing.T) {
	resetReconcileMetrics()
	store := newCompositionStore()
	const wsID, ownerID, secID = "ws-1", "user-1", "sec-gd"

	store.seedGlobalDefault(secID, ownerID)
	svc := secrets.NewSecretService(nil, store)
	lister := &fakeLister{list: []ActiveWorkspace{{WorkspaceID: wsID, OwnerUserID: ownerID}}}
	notifier := &fakeNotifier{}
	loop := New(lister, svc, notifier, WithPolicySource(svc))

	// Pass 1: materializes the auto row.
	require.NoError(t, loop.runPass(context.Background()))
	assert.Equal(t, map[string]string{secID: secrets.BindSourceGlobalDefault}, store.boundSources(wsID))

	// A sibling workspace binds the same secret MANUALLY; the flag then
	// flips off for the owner.
	require.NoError(t, store.SetBindings(context.Background(), "ws-manual", []string{secID}))
	svc2 := secrets.NewSecretService(nil, store)
	lister2 := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: wsID, OwnerUserID: ownerID},
		{WorkspaceID: "ws-manual", OwnerUserID: ownerID},
	}}
	loop2 := New(lister2, svc2, &fakeNotifier{}, WithPolicySource(svc2))
	store.mu.Lock()
	store.secrets[secID].GlobalDefault = false
	store.mu.Unlock()

	require.NoError(t, loop2.runPass(context.Background()))

	// Auto row retracted; manual row untouched.
	assert.Empty(t, store.boundSources(wsID), "flag-off must retract the auto row")
	assert.Equal(t, map[string]string{secID: secrets.BindSourceManual}, store.boundSources("ws-manual"))
}
