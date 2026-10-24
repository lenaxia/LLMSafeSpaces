// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

// #1889 F1 — durable phrase binding surviving session_index row death.
// The mint seams (EnsureSessionRegistered and the execWithMintedPhrase
// upsert legs) must CONSULT the phrase ledger before minting: a session
// whose row died (legitimate ghost reap post-grace, hard delete,
// workspace accident) and is later re-born must CONVERGE ON ITS
// ORIGINAL phrase instead of receiving a fresh mint — the re-phrase
// defect class (an agent holding the old phrase watches it 500/degrade
// while the session lives on under a new identity).

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ledgerConsult1889 = `SELECT short_id FROM session_phrase_ledger WHERE workspace_id = $1 AND session_id = $2`

const ledgerClaim1889 = `INSERT INTO session_phrase_ledger (workspace_id, session_id, short_id, minted_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET short_id = session_phrase_ledger.short_id
		 RETURNING short_id`

// TestEnsureSessionRegistered_RowDeathRebirthConvergesOnLedgerPhrase is
// the F1 core pin: the registration seam consults the ledger and births
// the resurrected row under its ORIGINAL phrase.
func TestEnsureSessionRegistered_RowDeathRebirthConvergesOnLedgerPhrase(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// The ledger row survived the session_index row's death.
	mock.ExpectQuery(regexp.QuoteMeta(ledgerConsult1889)).
		WithArgs("ws-1", "ses_dead").
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow("amber-falcon-glides"))

	// The row is born WITH the ledger phrase — never a fresh mint.
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`,
	)).WithArgs("ws-1", "ses_dead", "amber-falcon-glides").
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := svc.EnsureSessionRegistered(context.Background(), "ws-1", "ses_dead", "opencode")
	require.NoError(t, err)
	assert.Equal(t, "amber-falcon-glides", got,
		"a resurrected session must CONVERGE on its ledger phrase, not a fresh mint (#1889 F1)")
}

// TestUpsertSessionTitle_RebirthCarriesLedgerPhrase pins the SAME
// convergence on the execWithMintedPhrase path: a row-creating upsert
// (the resurrection vector for late title/message/context events) births
// the row under the ledger phrase.
func TestUpsertSessionTitle_RebirthCarriesLedgerPhrase(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(ledgerConsult1889)).
		WithArgs("ws-1", "ses_dead").
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow("amber-falcon-glides"))

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, short_id, title, updated_at)`,
	)).WithArgs("ws-1", "ses_dead", "amber-falcon-glides", "Resurrected").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpsertSessionTitle(context.Background(), "ws-1", "ses_dead", "Resurrected")
	require.NoError(t, err, "the rebirth must carry the ledger phrase in its INSERT leg")
}

// TestEnsureSessionRegistered_FreshBirthClaimsLedger pins the durable
// write: a FIRST-EVER birth (no ledger row) mints fresh and CLAIMS the
// ledger, and the row is born under the CLAIM-WINNING phrase (a racing
// claimant that lost the INSERT converges on the winner instead of its
// own mint — the check-and-retry discipline one level down).
func TestEnsureSessionRegistered_FreshBirthClaimsLedger(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// No ledger row yet: the consult misses.
	mock.ExpectQuery(regexp.QuoteMeta(ledgerConsult1889)).
		WithArgs("ws-1", "ses_new").
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}))

	// The claim INSERT..RETURNING answers the WINNER — a racing twin's
	// claim may have landed first; the loser must use the winner, not
	// its own mint (sqlmock answers a fixed phrase for the winner).
	mock.ExpectQuery(regexp.QuoteMeta(ledgerClaim1889)).
		WithArgs("ws-1", "ses_new", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow("linen-orchard-chirps"))

	// The row birth carries the CLAIM-WINNING phrase.
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`,
	)).WithArgs("ws-1", "ses_new", "linen-orchard-chirps").
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := svc.EnsureSessionRegistered(context.Background(), "ws-1", "ses_new", "opencode")
	require.NoError(t, err)
	assert.Equal(t, "linen-orchard-chirps", got,
		"the row birth must carry the phrase the ledger claim returned (racing claimants converge)")
}

// TestEnsureSessionRegistered_LedgerUnavailableStillRegisters pins the
// degrade posture: a ledger consult failure (transient DB error, missing
// table on a not-yet-migrated replica) must never fail the registration
// — convergence is best-effort, addressing is not.
func TestEnsureSessionRegistered_LedgerUnavailableStillRegisters(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(ledgerConsult1889)).
		WithArgs("ws-1", "ses_x").
		WillReturnError(assert.AnError)

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`,
	)).WithArgs("ws-1", "ses_x", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := svc.EnsureSessionRegistered(context.Background(), "ws-1", "ses_x", "opencode")
	require.NoError(t, err, "a ledger consult failure must degrade to a fresh mint, never fail the seam")
	require.NotEmpty(t, got)
}

// ledgerCorrection1889 is the r2 robustness finding 2 pin-shape: the
// ledger UPDATE recordPhraseLedger runs.
const ledgerCorrection1889 = `UPDATE session_phrase_ledger SET short_id = $3, minted_at = NOW()
		 WHERE workspace_id = $1 AND session_id = $2`

// TestEnsureSessionRegistered_ReadBackLegCorrectsLedger (r2
// robustness finding 2): a row born while the ledger was unavailable
// (ledger empty, real phrase P1 on the row) re-registered at attempt 0
// — the claim binds P2, the INSERT conflicts, the read-back returns
// P1: the ledger MUST be corrected to P1 or a later death+resurrection
// converges on the never-exposed P2 (the re-phrase class).
func TestEnsureSessionRegistered_ReadBackLegCorrectsLedger(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// Ledger consult misses (the degraded-birth window).
	mock.ExpectQuery(regexp.QuoteMeta(ledgerConsult1889)).
		WithArgs("ws-1", "ses_pre").
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}))
	// Claim binds the fresh mint.
	mock.ExpectQuery(regexp.QuoteMeta(ledgerClaim1889)).
		WithArgs("ws-1", "ses_pre", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow("linen-orchard-chirps"))
	// The INSERT conflicts: the row already exists.
	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`,
	)).WillReturnResult(sqlmock.NewResult(0, 0))
	// Read-back returns the row's REAL phrase.
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
	)).WithArgs("ws-1", "ses_pre").
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow("amber-falcon-glides"))
	// The ledger is CORRECTED to the row's real phrase.
	mock.ExpectExec(regexp.QuoteMeta(ledgerCorrection1889)).
		WithArgs("ws-1", "ses_pre", "amber-falcon-glides").
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := svc.EnsureSessionRegistered(context.Background(), "ws-1", "ses_pre", "opencode")
	require.NoError(t, err)
	assert.Equal(t, "amber-falcon-glides", got, "the read-back phrase wins")
}

// TestDeleteSessionIndex_ClearsLedger pins the retention posture the
// migration header records (r2 correctness finding 1): the
// workspace-reset path clears the ledger FIRST — ledger rows live as
// long as the workspace, not forever.
func TestDeleteSessionIndex_ClearsLedger(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM session_phrase_ledger WHERE workspace_id = $1`,
	)).WithArgs("ws-1").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(regexp.QuoteMeta(
		`DELETE FROM session_index WHERE workspace_id = $1`,
	)).WithArgs("ws-1").WillReturnResult(sqlmock.NewResult(0, 3))

	require.NoError(t, svc.DeleteSessionIndex(context.Background(), "ws-1"),
		"the reset path must clear BOTH the rows and the ledger")
	require.NoError(t, mock.ExpectationsWereMet())
}
