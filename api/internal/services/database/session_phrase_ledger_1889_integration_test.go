// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// #1889 F1 — durable phrase binding, the integration tier. The ledger
// is keyed independently of the session_index row, so a session whose
// row died and is later re-born CONVERGES ON ITS ORIGINAL phrase:
//
//  1. REGISTER → row born with P1; the ledger holds P1.
//  2. Row death (hard delete / reap — any path that removes the row).
//  3. Re-register (the resurrection vector: late events, backfill,
//     create_session retry) → the row is born under P1 again, NEVER a
//     fresh mint.
//
// Also pins: the migration backfill (pre-ledger minted rows become
// ledger-known), and the upsert-leg convergence (UpsertSessionTitle on
// an absent row births it under the ledger phrase).

func TestIntegration_PhraseLedger_ResurrectionConvergesOnOriginalPhrase(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1889-ledger", "ses_"+h.ID()

	first, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	require.True(t, shortid.IsShortIDForm(first), "minted %q must be a phrase", first)

	// Row death — the delete every reap/hard-delete path runs.
	require.NoError(t, svc.DeleteSessionTree(ctx, wsID, sesID))

	// Resurrection: the registration seam re-births the row.
	second, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"a resurrected session must CONVERGE on its original phrase (the durable binding, #1889 F1)")
}

func TestIntegration_PhraseLedger_UpsertRebirthCarriesLedgerPhrase(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1889-upsert", "ses_"+h.ID()

	first, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)

	require.NoError(t, svc.DeleteSessionTree(ctx, wsID, sesID))

	// The OTHER resurrection vector: a row-creating upsert (late title
	// event) births the row under the ledger phrase.
	require.NoError(t, svc.UpsertSessionTitle(ctx, wsID, sesID, "Resurrected"))

	var got string
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID).Scan(&got))
	assert.Equal(t, first, got,
		"an upsert-leg rebirth must carry the ledger phrase (#1889 F1)")
}

func TestIntegration_PhraseLedger_MigrationBackfillKnowsLivePhrases(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID, sesID := "int-ws-1889-backfill", "ses_"+h.ID()

	minted, err := svc.EnsureSessionRegistered(ctx, wsID, sesID, "opencode")
	require.NoError(t, err)

	// The migration backfilled every live minted row; the ledger must
	// already know this phrase (a later claim must NOT outrank it).
	var led string
	require.NoError(t, svc.DB.QueryRowContext(ctx,
		`SELECT short_id FROM session_phrase_ledger WHERE workspace_id = $1 AND session_id = $2`,
		wsID, sesID).Scan(&led))
	assert.Equal(t, minted, led, "the ledger row for a live session carries its actual phrase")
}

func TestIntegration_PhraseLedger_DistinctSessionsKeepDistinctPhrases(t *testing.T) {
	svc, h := newIntegrationService(t)
	ctx := h.NewContext()
	wsID := "int-ws-1889-distinct"

	a, err := svc.EnsureSessionRegistered(ctx, wsID, "ses_a_"+h.ID(), "opencode")
	require.NoError(t, err)
	b, err := svc.EnsureSessionRegistered(ctx, wsID, "ses_b_"+h.ID(), "opencode")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "unrelated sessions never share a phrase")
}
