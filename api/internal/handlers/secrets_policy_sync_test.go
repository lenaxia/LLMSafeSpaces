// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTestSecretStoreSyncGlobalDefaultBindings_ManualShadowAndFlagFlip
// pins the PgSecretStore contract on the handlers fixture: a manual row
// shadows policy (never auto-marked, never retracted on flag flip), and
// a manual replace-set clears auto tracking for the reconciler to
// re-assert.
func TestTestSecretStoreSyncGlobalDefaultBindings_ManualShadowAndFlagFlip(t *testing.T) {
	store := newTestSecretStore()
	ctx := context.Background()
	const ws = "ws-1"

	require.NoError(t, store.SetBindings(ctx, ws, []string{"sec-a"}))

	added, removed, err := store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-a", "sec-b"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-b"}, added, "sec-a is shadowed by its manual row")
	assert.Empty(t, removed)
	assert.NotContains(t, store.autoBound[ws], "sec-a")

	added, removed, err = store.SyncGlobalDefaultBindings(ctx, ws, nil)
	require.NoError(t, err)
	assert.Empty(t, added)
	assert.ElementsMatch(t, []string{"sec-b"}, removed)
	assert.Equal(t, []string{"sec-a"}, store.bindings[ws])

	_, _, err = store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-c"})
	require.NoError(t, err)
	require.NoError(t, store.SetBindings(ctx, ws, []string{"sec-a"}))
	added, _, err = store.SyncGlobalDefaultBindings(ctx, ws, []string{"sec-c"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sec-c"}, added,
		"policy re-asserts after a manual replace cleared the auto rows")
}
