// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secretsreconcile

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// The fixtures below rebuild the ManifestFor zero-decrypt pin at the
// reconcile boundary: a REAL *secrets.SecretService with every decrypt
// dependency wired to fail the test on touch. The live-manifest loop
// recomputes each workspace's manifest from ROWS (zero decrypts), reads
// the stored revision row, and mints drift — none of which may reach a
// ciphertext, the DEK key store, or the admin/org root providers.

type revRow struct {
	seq  int64
	hash string
}

type panickingRootKeyProvider struct{ t *testing.T }

func (p *panickingRootKeyProvider) Encrypt(context.Context, []byte) ([]byte, error) {
	p.t.Fatalf("reconcile pass must never encrypt; got an Encrypt call")
	return nil, nil
}

func (p *panickingRootKeyProvider) Decrypt(context.Context, []byte) ([]byte, error) {
	p.t.Fatalf("reconcile pass must never decrypt; got a Decrypt call")
	return nil, nil
}

type panickingKeyStore struct{ t *testing.T }

func (p *panickingKeyStore) GetUserKey(context.Context, string) (*secrets.UserKeyRecord, error) {
	p.t.Fatalf("reconcile pass must never touch user_keys")
	return nil, nil
}
func (p *panickingKeyStore) CreateUserKey(context.Context, *secrets.UserKeyRecord) error {
	p.t.Fatalf("reconcile pass must never touch user_keys")
	return nil
}
func (p *panickingKeyStore) UpdateWrappedDEK(context.Context, string, []byte, []byte, int) error {
	p.t.Fatalf("reconcile pass must never touch user_keys")
	return nil
}

type panickingDEKCache struct{ t *testing.T }

func (p *panickingDEKCache) CacheDEK(context.Context, string, []byte, time.Duration) error {
	p.t.Fatalf("reconcile pass must never touch the DEK cache")
	return nil
}
func (p *panickingDEKCache) GetDEK(context.Context, string) ([]byte, error) {
	p.t.Fatalf("reconcile pass must never touch the DEK cache")
	return nil, nil
}
func (p *panickingDEKCache) EvictDEK(context.Context, string) error {
	p.t.Fatalf("reconcile pass must never touch the DEK cache")
	return nil
}

// fleetManifestFixture describes the row set every workspace in the
// pin shares: one owner-bound env secret, no credentials, no MCP
// servers. The live manifest is therefore identical across the fleet —
// what the pin varies is the stored row (stale) so the mint path runs.
type fleetManifestFixture struct {
	owner    string
	boundOne *secrets.UserSecret
}

func newFleetManifestFixture(owner string) fleetManifestFixture {
	return fleetManifestFixture{
		owner: owner,
		boundOne: &secrets.UserSecret{
			ID:         "sec-fleet",
			UserID:     owner,
			Name:       "fleet_var",
			Type:       secrets.SecretTypeEnvSecret,
			Version:    1,
			KeyVersion: 1,
			Metadata:   json.RawMessage(`{"var_name":"FLEET_VAR"}`),
		},
	}
}

// reconcileStore satisfies secrets.SecretStore + CredentialStore +
// RevisionStore for the pin: the manifest-tier reads (workspace
// credentials, bindings, MCP servers) and the revision tier
// (CurrentRevision / EnsureRevision) carry fixture state; EVERY other
// method is fatal-on-touch because the reconcile loop must never call
// it.
type reconcileStore struct {
	t       *testing.T
	fixture fleetManifestFixture
	rows    map[string]revRow
	mu      sync.Mutex
	minted  int
	mintFn  func(workspaceID, manifestHash string) error
	credErr map[string]error
	bindErr map[string]error
	mcpsErr map[string]error
	rowErr  map[string]error
}

func (s *reconcileStore) unexpected(method string) {
	s.t.Fatalf("reconcile loop reached SecretStore.%s — the pass must use the manifest-tier reads + the revision tier only", method)
}

// --- manifest-tier reads (live rows, decrypt-free) ---

func (s *reconcileStore) GetWorkspaceCredentials(_ context.Context, workspaceID string) ([]secrets.CredentialBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.credErr[workspaceID]; err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *reconcileStore) GetBindings(_ context.Context, workspaceID string) ([]*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.bindErr[workspaceID]; err != nil {
		return nil, err
	}
	cp := *s.fixture.boundOne
	return []*secrets.UserSecret{&cp}, nil
}

func (s *reconcileStore) GetWorkspaceMCPServers(_ context.Context, workspaceID string) ([]secrets.MCPServerBindingRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mcpsErr[workspaceID]; err != nil {
		return nil, err
	}
	return nil, nil
}

// --- revision tier ---

func (s *reconcileStore) CurrentRevision(_ context.Context, workspaceID string) (int64, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rowErr[workspaceID]; err != nil {
		return 0, "", false, err
	}
	row, ok := s.rows[workspaceID]
	if !ok {
		return 0, "", false, nil
	}
	return row.seq, row.hash, true, nil
}

func (s *reconcileStore) EnsureRevision(_ context.Context, workspaceID, manifestHash string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mintFn != nil {
		if err := s.mintFn(workspaceID, manifestHash); err != nil {
			return 0, err
		}
	}
	if row, ok := s.rows[workspaceID]; ok && row.hash == manifestHash {
		return row.seq, nil
	}
	next := s.rows[workspaceID].seq + 1
	s.rows[workspaceID] = revRow{seq: next, hash: manifestHash}
	s.minted++
	return next, nil
}

func (s *reconcileStore) mintedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minted
}

// row snapshots the stored row WITHOUT the fault injection (the error
// maps stay armed for the pass; assertions must still read the state).
func (s *reconcileStore) row(ws string) (revRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[ws]
	return row, ok
}

// --- everything else: fatal-on-touch ---

func (s *reconcileStore) CreateSecret(context.Context, *secrets.UserSecret) error {
	s.unexpected("CreateSecret")
	return nil
}
func (s *reconcileStore) GetSecret(context.Context, string, string) (*secrets.UserSecret, error) {
	s.unexpected("GetSecret")
	return nil, nil
}
func (s *reconcileStore) GetSecretByName(context.Context, string, string) (*secrets.UserSecret, error) {
	s.unexpected("GetSecretByName")
	return nil, nil
}
func (s *reconcileStore) ListSecrets(context.Context, string) ([]*secrets.UserSecret, error) {
	s.unexpected("ListSecrets")
	return nil, nil
}
func (s *reconcileStore) ListGlobalDefaultSecrets(context.Context, string) ([]*secrets.UserSecret, error) {
	s.unexpected("ListGlobalDefaultSecrets")
	return nil, nil
}
func (s *reconcileStore) UpdateSecret(context.Context, *secrets.UserSecret) error {
	s.unexpected("UpdateSecret")
	return nil
}
func (s *reconcileStore) DeleteSecret(context.Context, string, string) error {
	s.unexpected("DeleteSecret")
	return nil
}
func (s *reconcileStore) ReEncryptUserSecrets(context.Context, string, int, func([]byte) ([]byte, error), func(context.Context) error) error {
	s.unexpected("ReEncryptUserSecrets")
	return nil
}
func (s *reconcileStore) SetBindings(context.Context, string, []string) error {
	s.unexpected("SetBindings")
	return nil
}
func (s *reconcileStore) AddBindings(context.Context, string, []string) error {
	s.unexpected("AddBindings")
	return nil
}
func (s *reconcileStore) GetBindingsForSecret(context.Context, string) ([]string, error) {
	s.unexpected("GetBindingsForSecret")
	return nil, nil
}
func (s *reconcileStore) LogAudit(context.Context, *secrets.AuditEntry) error {
	s.unexpected("LogAudit")
	return nil
}
func (s *reconcileStore) QueryAudit(context.Context, string, secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	s.unexpected("QueryAudit")
	return nil, nil
}
func (s *reconcileStore) UpsertFreeTierCredential(context.Context, []byte) error {
	s.unexpected("UpsertFreeTierCredential")
	return nil
}
func (s *reconcileStore) SeedWorkspaceCredentials(context.Context, string, string, *string) error {
	s.unexpected("SeedWorkspaceCredentials")
	return nil
}
func (s *reconcileStore) BindCredentialToAllUserWorkspaces(context.Context, string, string) error {
	s.unexpected("BindCredentialToAllUserWorkspaces")
	return nil
}
func (s *reconcileStore) HasUserProviderCredential(context.Context, string, string) (bool, error) {
	s.unexpected("HasUserProviderCredential")
	return false, nil
}

// panickingRevisionSource bundles the real SecretService (every decrypt
// dependency fatal-on-touch) with its fixture store so tests can count
// mints and script per-workspace read failures.
type panickingRevisionSource struct {
	*secrets.SecretService
	store *reconcileStore
}

func (p panickingRevisionSource) mintedCount() int { return p.store.mintedCount() }

// newPanickingRevisionSource builds the real SecretService over the
// fixture store: the manifest tier reads rows, the revision tier
// reads/mints rows, and every decrypt dependency is fatal-on-touch.
func newPanickingRevisionSource(t *testing.T, rows map[string]revRow, fixture fleetManifestFixture) panickingRevisionSource {
	t.Helper()
	store := &reconcileStore{
		t:       t,
		fixture: fixture,
		rows:    rows,
	}
	svc := secrets.NewSecretService(
		secrets.NewKeyService(&panickingKeyStore{t: t}, &panickingDEKCache{t: t}),
		store,
	)
	svc.SetAdminProvider(&panickingRootKeyProvider{t: t})
	svc.SetOrgProvider(&panickingRootKeyProvider{t: t})
	return panickingRevisionSource{SecretService: svc, store: store}
}

// TestRunPassZeroDecrypts_ManifestTierReadErrorsAreIsolated drives the
// REAL service with per-workspace manifest-read failures injected at
// the store seam: each failing workspace is skipped (never fatal) while
// the fleet pass still succeeds — over the real ManifestFor code path,
// with decrypt deps fatal-on-touch.
func TestRunPassZeroDecrypts_ManifestTierReadErrorsAreIsolated(t *testing.T) {
	resetReconcileMetrics()
	fixture := newFleetManifestFixture("user-1")
	src := newPanickingRevisionSource(t, map[string]revRow{
		"ws-ok":     {seq: 1, hash: "stale"},
		"ws-cred":   {seq: 1, hash: "stale"},
		"ws-bind":   {seq: 1, hash: "stale"},
		"ws-mcp":    {seq: 1, hash: "stale"},
		"ws-row":    {seq: 1, hash: "stale"},
		"ws-mint":   {seq: 1, hash: "stale"},
		"ws-manual": {seq: 1, hash: "stale"},
	}, fixture)
	src.store.credErr = map[string]error{"ws-cred": ctxErr("credential rows unreadable")}
	src.store.bindErr = map[string]error{"ws-bind": ctxErr("binding rows unreadable")}
	src.store.mcpsErr = map[string]error{"ws-mcp": ctxErr("mcp rows unreadable")}
	src.store.rowErr = map[string]error{"ws-row": ctxErr("revision row unreadable")}
	src.store.mintFn = func(ws, _ string) error {
		if ws == "ws-mint" {
			return ctxErr("mint failed")
		}
		return nil
	}

	lister := &fakeLister{list: []ActiveWorkspace{
		{WorkspaceID: "ws-ok", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
		{WorkspaceID: "ws-cred", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
		{WorkspaceID: "ws-bind", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
		{WorkspaceID: "ws-mcp", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
		{WorkspaceID: "ws-row", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
		{WorkspaceID: "ws-mint", OwnerUserID: "user-1", SpawnedRev: "1:x:y"},
	}}
	svc := newTestService(lister, src, &fakeNotifier{})

	require.NoError(t, svc.runPass(context.Background()))

	// ws-ok was NOT skipped: its row was converged to the live manifest
	// (minted past the pod's seq — divergent, but evaluated).
	live, err := src.ManifestFor(context.Background(), "user-1", "ws-ok")
	require.NoError(t, err)
	row, ok := src.store.row("ws-ok")
	require.True(t, ok)
	assert.Equal(t, live, row.hash, "the healthy workspace's row was minted to the live manifest")

	// Every failed workspace was skipped BEFORE its row was touched.
	for _, ws := range []string{"ws-cred", "ws-bind", "ws-mcp", "ws-row", "ws-mint"} {
		row, ok := src.store.row(ws)
		require.True(t, ok)
		assert.Equal(t, "stale", row.hash, "%s was skipped — its stored row is untouched", ws)
		assert.Equal(t, 0.0, convergedGauge(t, ws), "%s gets no convergence gauge", ws)
	}
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.SecretsReconcilePassesCounter().WithLabelValues("success")))
}

type errString string

func (e errString) Error() string { return string(e) }

func ctxErr(msg string) error { return errString(msg) }
