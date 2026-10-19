// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// newIntegrationService wires the real database.Service over the
// harness's *sql.DB (migrations applied by the harness).
func newIntegrationService(t *testing.T) (*Service, *testharness.Harness) {
	t.Helper()
	h := testharness.New(t)
	svc := &Service{DB: h.SQLDB()}
	return svc, h
}

// TestIntegration_EnsureSessionRegistered_ConcurrentSameSession is
// design 0066 §6's race pin: many concurrent registrations of the SAME
// (workspace, session) pair converge on ONE short id — exactly one
// mint lands, every caller reads back the surviving row's id.
func TestIntegration_EnsureSessionRegistered_ConcurrentSameSession(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1691-race", "ses_"+h.ID()

	const racers = 8
	var wg sync.WaitGroup
	results := make([]string, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
		}(i)
	}
	wg.Wait()

	for i := range errs {
		require.NoError(t, errs[i], "racer %d", i)
	}
	for i := 1; i < racers; i++ {
		assert.Equal(t, results[0], results[i], "every racer must converge on the same short id")
	}
	require.True(t, shortid.IsShortIDForm(results[0]), "converged id %q must be a short id", results[0])

	// And the row persists exactly that id; re-registration is stable.
	again, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	assert.Equal(t, results[0], again, "re-registration must never re-mint")
}

// TestIntegration_EnsureSessionRegistered_UniqueIndexArbitrates is
// design 0066 §6's collision pin: two DIFFERENT sessions registered
// concurrently in one workspace both succeed with DISTINCT short ids
// — the partial unique index (workspace_id, short_id) arbitrates any
// mint collision by raising 23505, and the check-and-retry loop
// re-mints (the ruling-3 arithmetic says collisions are rare; this
// pin proves the machinery is correct when one happens anyway).
func TestIntegration_EnsureSessionRegistered_UniqueIndexArbitrates(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1691-collide"
	sesA, sesB := "ses_"+h.ID()+"a", "ses_"+h.ID()+"b"

	var wg sync.WaitGroup
	var a, b string
	var aErr, bErr error
	wg.Add(2)
	go func() { defer wg.Done(); a, aErr = svc.EnsureSessionRegistered(ctx, wsID, sesA, "opencode") }()
	go func() { defer wg.Done(); b, bErr = svc.EnsureSessionRegistered(ctx, wsID, sesB, "opencode") }()
	wg.Wait()

	require.NoError(t, aErr)
	require.NoError(t, bErr)
	assert.NotEqual(t, a, b, "distinct sessions must never share a short id within a workspace")
	assert.True(t, shortid.IsShortIDForm(a))
	assert.True(t, shortid.IsShortIDForm(b))

	// The partial unique index itself is the arbiter of record.
	var count int
	err := h.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM session_index WHERE workspace_id = $1 AND short_id IN ($2, $3)`,
		wsID, a, b).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

// TestIntegration_EnsureSessionRegistered_BackfillConverges is design
// 0066 §6's backfill pin: a pre-rollout row (short_id NULL) is minted
// EXACTLY ONCE — concurrent backfills and repeats all converge on the
// single surviving id, never re-minting.
func TestIntegration_EnsureSessionRegistered_BackfillConverges(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1691-backfill", "ses_"+h.ID()

	// Seed a pre-rollout row with short_id NULL (title upsert shape,
	// pre-migration write).
	_, err := h.Pool().Exec(ctx,
		`INSERT INTO session_index (workspace_id, session_id, title) VALUES ($1, $2, 'Pre-rollout')`,
		wsID, sesID)
	require.NoError(t, err)

	const racers = 6
	var wg sync.WaitGroup
	results := make([]string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
			require.NoError(t, err)
			results[i] = id
		}(i)
	}
	wg.Wait()

	for i := 1; i < racers; i++ {
		assert.Equal(t, results[0], results[i], "concurrent backfills must converge on one mint")
	}
	assert.True(t, shortid.IsShortIDForm(results[0]))

	// No row in the workspace is left NULL, and the id is stable.
	now, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	assert.Equal(t, results[0], now)
	var nullCount int
	require.NoError(t, h.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM session_index WHERE workspace_id = $1 AND short_id IS NULL`,
		wsID).Scan(&nullCount))
	assert.Zero(t, nullCount)
}

// TestIntegration_FindSessionByShortID_CaseInsensitive verifies the
// resolver against the real lower() comparison.
func TestIntegration_FindSessionByShortID_CaseInsensitive(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1691-find", "ses_"+h.ID()

	shortID, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)

	upper := shortID[:3] + upperString(shortID[3:])
	got, err := svc.FindSessionByShortID(ctx, wsID, upper)
	require.NoError(t, err)
	assert.Equal(t, sesID, got, "OC-XXXX form must resolve to the canonical id")
}

// TestIntegration_FindRefCandidatesAcrossWorkspaces_MixedCaseLegacyID
// pins review r1 finding 2's fix: a MIXED-CASE legacy id must
// byte-equal-match across workspaces (the raw ref, not the normalized
// one, binds the session_id comparison).
func TestIntegration_FindRefCandidatesAcrossWorkspaces_MixedCaseLegacyID(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1691-cross", "ses_eD9183a46FfeRiNw2PddpL5dAA" // mixed case by construction

	_, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)

	rows, err := svc.FindRefCandidatesAcrossWorkspaces(ctx, []string{wsID}, sesID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "mixed-case legacy id must byte-equal match")
	assert.Equal(t, sesID, rows[0].ID)
	assert.Equal(t, wsID, rows[0].Workspace)
}

func upperString(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 'a' - 'A'
		}
	}
	return string(out)
}

// TestIntegration_FindSessionsByAlias_NewestFirst pins the alias
// ORDER BY against real Postgres (review r2 finding C): the ambiguity
// payload's newest-first truncation contract is DB-enforced, not
// accidental return order.
func TestIntegration_FindSessionsByAlias_NewestFirst(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1691-alias"
	title := "Dup Title " + h.ID()

	old := time.Now().Add(-48 * time.Hour).UTC()
	mid := time.Now().Add(-2 * time.Hour).UTC()
	newT := time.Now().Add(-5 * time.Minute).UTC()
	for _, seed := range []struct {
		ses string
		at  time.Time
	}{{"ses_old", old}, {"ses_mid", mid}, {"ses_new", newT}} {
		_, err := h.Pool().Exec(ctx,
			`INSERT INTO session_index (workspace_id, session_id, title, last_message_at) VALUES ($1, $2, $3, $4)`,
			wsID, seed.ses, title, seed.at)
		require.NoError(t, err)
	}

	rows, err := svc.FindSessionsByAlias(ctx, wsID, "  "+title+"  ")
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, "ses_new", rows[0].ID, "newest first is the contract")
	assert.Equal(t, "ses_mid", rows[1].ID)
	assert.Equal(t, "ses_old", rows[2].ID)
}
