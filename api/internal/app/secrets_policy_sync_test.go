// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDbAdapterSyncGlobalDefaultBindings_ManualShadowAndFlagFlip pins
// the PgSecretStore contract on the in-memory dev-mode adapter:
//   - a manual row for a still-flagged secret is NOT reported as added
//     and keeps manual provenance;
//   - a flag flip then retracts only auto rows — the manual row stays;
//   - a manual replace-set clears auto tracking (the reconciler
//     re-asserts current policy on its next pass).
func TestDbAdapterSyncGlobalDefaultBindings_ManualShadowAndFlagFlip(t *testing.T) {
	store := &dbSecretStoreAdapter{}
	ctx := context.Background()
	const ws = "ws-1"

	// Manual claim on sec-a, created BEFORE any policy ran.
	require.NoError(t, store.SetBindings(ctx, ws, []string{"sec-a"}))

	// Policy: sec-a and sec-b are global defaults.
	added, removed, err := store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-a", "sec-b"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-b"}, added, "sec-a is shadowed by its manual row")
	assert.Empty(t, removed)
	assert.Equal(t, []string{"sec-a", "sec-b"}, store.bindings[ws])
	assert.NotContains(t, store.autoBound[ws], "sec-a", "a manual row never becomes auto")

	// Flag flips off for both. Only sec-b's auto row goes; sec-a's
	// manual row is the user's explicit claim and must survive.
	added, removed, err = store.SyncGlobalDefaultBindings(ctx, ws, nil)
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.ElementsMatch(t, []string{"sec-b"}, removed)
	assert.Equal(t, []string{"sec-a"}, store.bindings[ws])

	// Re-assert policy, then a manual replace-set: auto rows clear with
	// the replace (manual wins over the whole set), and the next sync
	// re-materializes them.
	_, _, err = store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-c"})
	require.NoError(t, err)
	require.NoError(t, store.SetBindings(ctx, ws, []string{"sec-a"}))
	added, _, err = store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-c"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-c"}, added,
		"policy re-asserts after a manual replace cleared the auto rows")
}
