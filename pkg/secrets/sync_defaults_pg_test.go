// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package secrets

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bindingSources reads the workspace's binding rows as secret_id ->
// bind_source straight from the table (the provenance is the contract).
func bindingSources(t *testing.T, pool *pgxpool.Pool, workspaceID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT secret_id, bind_source FROM user_secret_bindings WHERE workspace_id = $1`, workspaceID)
	require.NoError(t, err)
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var sid, src string
		require.NoError(t, rows.Scan(&sid, &src))
		out[sid] = src
	}
	require.NoError(t, rows.Err())
	return out
}

// TestPgSyncGlobalDefaultBindings_Lifecycle pins the SQL-level contract:
// inserts missing auto rows, removes auto rows absent from the keep-set
// (flag off / secret deleted), never touches manual rows, and reports
// exactly the changed ids. Runs against a real Postgres because the
// semantics (ON CONFLICT, <> ALL, advisory lock) are the store's.
func TestPgSyncGlobalDefaultBindings_Lifecycle(t *testing.T) {
	pool := getTestPool(t)
	const userID = "user-sync-defaults"
	const wsID = "00000000-0000-0000-0001-0000000000f1"
	cleanupSyncDefaults(t, pool, userID, wsID)
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	store := NewPgSecretStore(pool)

	ctx := context.Background()
	create := func(name string, globalDefault bool) string {
		require.NoError(t, store.CreateSecret(ctx, &UserSecret{
			UserID: userID, Name: name, Type: SecretTypeEnvSecret,
			Ciphertext: []byte("ct"), KeyVersion: 1, Metadata: json.RawMessage("{}"),
			GlobalDefault: globalDefault,
		}))
		// CreateSecret lets the DB mint the uuid; resolve it by name.
		sec, err := store.GetSecretByName(ctx, userID, name)
		require.NoError(t, err)
		require.NotNil(t, sec)
		return sec.ID
	}
	secA := create("n-a", true)
	secB := create("n-b", true)
	secC := create("n-c", false)

	// A manual row for secC (explicit user claim) exists before any sync.
	require.NoError(t, store.SetBindings(ctx, wsID, []string{secC}))

	// Sync 1: secA + secB are defaults. secC's manual row must survive
	// untouched; both defaults land as global_default rows.
	added, removed, err := store.SyncGlobalDefaultBindings(ctx, wsID, []string{secA, secB})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{secA, secB}, added)
	assert.Empty(t, removed)
	assert.Equal(t, map[string]string{
		secA: BindSourceGlobalDefault,
		secB: BindSourceGlobalDefault,
		secC: BindSourceManual,
	}, bindingSources(t, pool, wsID))

	// Sync 2: idempotent — no changes reported.
	added, removed, err = store.SyncGlobalDefaultBindings(ctx, wsID, []string{secA, secB})
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.Empty(t, removed)

	// Sync 3: secB's flag flips off. Its auto row is removed; secA
	// stays; secC's manual row is still untouched.
	added, removed, err = store.SyncGlobalDefaultBindings(ctx, wsID, []string{secA})
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.ElementsMatch(t, []string{secB}, removed)
	assert.Equal(t, map[string]string{
		secA: BindSourceGlobalDefault,
		secC: BindSourceManual,
	}, bindingSources(t, pool, wsID))

	// Sync 4: empty keep-set (owner has no defaults left) removes the
	// remaining auto row but never the manual one.
	added, removed, err = store.SyncGlobalDefaultBindings(ctx, wsID, nil)
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.ElementsMatch(t, []string{secA}, removed)
	assert.Equal(t, map[string]string{secC: BindSourceManual}, bindingSources(t, pool, wsID))
}

// TestPgDeleteSecret_CascadesBindingRows pins the DB invariant the sync
// relies on: user_secret_bindings has ON DELETE CASCADE FKs to
// user_secrets, so a deleted secret can never leave a binding row
// behind — the sync's removal path only ever needs to cover flag
// flips.
func TestPgDeleteSecret_CascadesBindingRows(t *testing.T) {
	pool := getTestPool(t)
	const userID = "user-sync-delete"
	const wsID = "00000000-0000-0000-0001-0000000000f2"
	cleanupSyncDefaults(t, pool, userID, wsID)
	ensureTestUser(t, pool, userID)
	ensureTestWorkspace(t, pool, wsID, userID)
	store := NewPgSecretStore(pool)

	ctx := context.Background()
	require.NoError(t, store.CreateSecret(ctx, &UserSecret{
		UserID: userID, Name: "n-doomed", Type: SecretTypeEnvSecret,
		Ciphertext: []byte("ct"), KeyVersion: 1, Metadata: json.RawMessage("{}"),
	}))
	sec, err := store.GetSecretByName(ctx, userID, "n-doomed")
	require.NoError(t, err)
	require.NotNil(t, sec)

	require.NoError(t, store.SetBindings(ctx, wsID, []string{sec.ID}))
	require.NoError(t, store.DeleteSecret(ctx, userID, sec.ID))

	assert.Empty(t, bindingSources(t, pool, wsID))
}

func cleanupSyncDefaults(t *testing.T, pool *pgxpool.Pool, userID, wsID string) {
	t.Helper()
	cleanupSecrets(t, pool, userID)
	pool.Exec(context.Background(), "DELETE FROM workspaces WHERE id = $1", wsID)
}
