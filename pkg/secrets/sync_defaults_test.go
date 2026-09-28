// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSecretRow inserts a bare secret row for sync tests. The sync path
// is manifest-tier (no decrypts), so no key material is needed.
func seedSecretRow(t *testing.T, store *mockSecretStore, id, userID string, globalDefault bool) {
	t.Helper()
	require.NoError(t, store.CreateSecret(context.Background(), &UserSecret{
		UserID:        userID,
		Name:          id,
		Type:          SecretTypeEnvSecret,
		Ciphertext:    []byte("ct"),
		KeyVersion:    1,
		GlobalDefault: globalDefault,
	}))
	// CreateSecret mints a deterministic id ("sec-"+name) only when ID
	// is empty; these tests want stable ids, so rewrite the key.
	raw, ok := store.secrets["sec-"+id]
	require.True(t, ok)
	raw.ID = id
	delete(store.secrets, "sec-"+id)
	store.secrets[id] = raw
}

// auditActions is defined in builder_test.go (auditActions(store)) —
// these tests use that existing helper directly.

// TestSyncGlobalDefaultBindings_AddsMissingDefaults: the owner has two
// global-default secrets; the workspace has no bindings. One sync adds
// both as global_default-source rows and audits a "bind" per secret.
func TestSyncGlobalDefaultBindings_AddsMissingDefaults(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	seedSecretRow(t, store, "sec-b", "user-1", true)
	svc := NewSecretService(nil, store)

	added, removed, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-a", "sec-b"}, added)
	assert.Empty(t, removed)

	bound := store.bindings["ws-1"]
	assert.Equal(t, map[string]string{"sec-a": BindSourceGlobalDefault, "sec-b": BindSourceGlobalDefault}, bound)
	assert.ElementsMatch(t, []string{"bind", "bind"}, auditActions(store))
}

// TestSyncGlobalDefaultBindings_Idempotent: a second sync over an
// already-converged workspace changes nothing and audits nothing.
func TestSyncGlobalDefaultBindings_Idempotent(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	svc := NewSecretService(nil, store)

	_, _, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	store.audit = nil

	added, removed, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Empty(t, removed)
	assert.Empty(t, store.audit)
}

// TestSyncGlobalDefaultBindings_RemovesStaleAutoRows: a secret that
// lost its global_default flag has its auto row removed (audited);
// a still-flagged secret stays bound. (Deleted secrets never leave
// stale rows: the bindings table FKs cascade on secret delete —
// pinned by TestPgDeleteSecret_CascadesBindingRows.)
func TestSyncGlobalDefaultBindings_RemovesStaleAutoRows(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	seedSecretRow(t, store, "sec-b", "user-1", false) // flag flipped off
	seedSecretRow(t, store, "sec-c", "user-1", true)
	svc := NewSecretService(nil, store)

	// Converge once with sec-b flagged on so an auto row exists.
	_, _, err := store.SyncGlobalDefaultBindings(context.Background(), "ws-1", []string{"sec-a", "sec-b", "sec-c"})
	require.NoError(t, err)
	store.audit = nil

	added, removed, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, added)
	// sec-b lost its flag — its auto row goes; sec-a/sec-c stay.
	assert.ElementsMatch(t, []string{"sec-b"}, removed)
	assert.Equal(t, map[string]string{"sec-a": BindSourceGlobalDefault, "sec-c": BindSourceGlobalDefault}, store.bindings["ws-1"])
	assert.ElementsMatch(t, []string{"unbind"}, auditActions(store))
}

// TestSyncGlobalDefaultBindings_ManualRowSurvivesFlagOff: a manual
// binding is the user's explicit claim — policy convergence must never
// remove it, even when the secret's global_default flag is off.
func TestSyncGlobalDefaultBindings_ManualRowSurvivesFlagOff(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", false)
	require.NoError(t, store.SetBindings(context.Background(), "ws-1", []string{"sec-a"}))
	svc := NewSecretService(nil, store)

	added, removed, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Empty(t, removed)
	assert.Equal(t, map[string]string{"sec-a": BindSourceManual}, store.bindings["ws-1"])
}

// TestSyncGlobalDefaultBindings_ManualRowShadows: when a manual row
// already binds a global-default secret, the sync must not report it as
// added nor rewrite its provenance (manual is the stronger claim).
func TestSyncGlobalDefaultBindings_ManualRowShadows(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	require.NoError(t, store.SetBindings(context.Background(), "ws-1", []string{"sec-a"}))
	svc := NewSecretService(nil, store)

	added, removed, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Empty(t, removed)
	assert.Equal(t, map[string]string{"sec-a": BindSourceManual}, store.bindings["ws-1"])
}

// TestSyncGlobalDefaultBindings_OtherOwnerSecretsIgnored: only the
// workspace OWNER's global defaults apply (cross-tenant isolation,
// matching the builder's owner filter in loadWorkspaceRows).
func TestSyncGlobalDefaultBindings_OtherOwnerSecretsIgnored(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	seedSecretRow(t, store, "sec-other", "user-2", true)
	svc := NewSecretService(nil, store)

	added, _, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-a"}, added)
	assert.NotContains(t, store.bindings["ws-1"], "sec-other")
}

// TestSyncGlobalDefaultBindings_StoreErrorPropagates: a failing sync
// surfaces the error so the reconcile loop can skip-and-retry.
func TestSyncGlobalDefaultBindings_StoreErrorPropagates(t *testing.T) {
	store := newMockSecretStore()
	store.syncBindingsErr = errors.New("db outage")
	svc := NewSecretService(nil, store)

	_, _, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, store.syncBindingsErr)
}

// TestSyncGlobalDefaultBindings_ListErrorPropagates: a failing
// ListGlobalDefaultSecrets aborts before any write.
func TestSyncGlobalDefaultBindings_ListErrorPropagates(t *testing.T) {
	store := newMockSecretStore()
	store.listGlobalDefaultErr = errors.New("list outage")
	svc := NewSecretService(nil, store)

	_, _, err := svc.SyncGlobalDefaultBindings(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, store.listGlobalDefaultErr)
}

// TestSeedGlobalDefaultSecrets_TagsSource: the workspace-create seeding
// path goes through the same provenance-tagged sync, so rows it creates
// carry bind_source=global_default (removable when the flag flips off).
func TestSeedGlobalDefaultSecrets_TagsSource(t *testing.T) {
	store := newMockSecretStore()
	seedSecretRow(t, store, "sec-a", "user-1", true)
	svc := NewSecretService(nil, store)

	require.NoError(t, svc.SeedGlobalDefaultSecrets(context.Background(), "ws-1", "user-1"))
	assert.Equal(t, map[string]string{"sec-a": BindSourceGlobalDefault}, store.bindings["ws-1"])
}
