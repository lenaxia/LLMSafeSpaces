// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1757 sqlmock pins: the registration UPDATE (NULL-only guard), the
// fleet-scope phrase lookup, and the lazy backfill on GetWorkspace.

func TestEnsureWorkspacePhraseRegistered_FreshMint(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET phrase_id = $2, updated_at = NOW()`)).
		WithArgs("ws-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT phrase_id FROM workspaces WHERE id = $1`)).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"phrase_id"}).AddRow("amber-river-7"))

	got, err := s.EnsureWorkspacePhraseRegistered(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("EnsureWorkspacePhraseRegistered: %v", err)
	}
	// The read-back is authoritative: whatever the index accepted.
	if got != "amber-river-7" {
		t.Fatalf("got %q; want the surviving row's phrase", got)
	}
}

func TestEnsureWorkspacePhraseRegistered_RetriesOnFleetCollision(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Fleet collision (the global partial unique index raises 23505) →
	// fresh draw, not a counter suffix (the digit IS the disambiguator
	// slot; there is no -2 form for workspace phrases).
	for i := 0; i < 2; i++ {
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET phrase_id = $2, updated_at = NOW()`)).
			WithArgs("ws-1", sqlmock.AnyArg()).
			WillReturnError(uniqueViolationErr())
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET phrase_id = $2, updated_at = NOW()`)).
		WithArgs("ws-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT phrase_id FROM workspaces WHERE id = $1`)).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"phrase_id"}).AddRow("linen-orchard-3"))

	got, err := s.EnsureWorkspacePhraseRegistered(context.Background(), "ws-1")
	if err != nil || got != "linen-orchard-3" {
		t.Fatalf("collision retry broken: %q,%v", got, err)
	}
}

func TestFindWorkspaceIDByPhrase_FleetScopeCaseInsensitive(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Stored lowercase; the lookup normalizes the input.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM workspaces WHERE phrase_id = $1 AND deleted_at IS NULL`)).
		WithArgs("amber-river-7").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-42"))

	got, err := s.FindWorkspaceIDByPhrase(context.Background(), "Amber-River-7")
	if err != nil || got != "ws-42" {
		t.Fatalf("phrase lookup: %q,%v", got, err)
	}
}

func TestFindWorkspaceIDByName_UnambiguousOnly(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Two same-name rows ⇒ miss (""), never a guess.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM workspaces WHERE user_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`)).
		WithArgs("user-1", "deploy").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-a").AddRow("ws-b"))

	got, err := s.FindWorkspaceIDByName(context.Background(), "user-1", "deploy")
	if err != nil || got != "" {
		t.Fatalf("ambiguous name must miss: %q,%v", got, err)
	}
}

// uniqueViolationErr shapes a 23505 the way pgx surfaces it (the
// imagefactory isUniqueViolation sqlstate contract).
func uniqueViolationErr() error {
	return &testPgError{code: "23505"}
}

type testPgError struct{ code string }

func (e *testPgError) SQLState() string { return e.code }
func (e *testPgError) Error() string    { return "unique violation" }

// r1: the DB read is PURE — a NULL phrase_id surfaces as an empty
// PhraseID with ZERO writes. The lazy backfill moved to the
// user-scoped read (post-verifyOwner) precisely so a pre-authorization
// caller can never mint by probing; this pin holds that property at
// the tier that owns the SQL.
// r2 (replacing the r1 inert form): the pin must be able to FAIL. A
// REGISTERED, error-armed UPDATE expectation makes any write loud —
// consumed ⇒ ExpectationsWereMet returns nil ⇒ the fatal below fires;
// unconsumed (the pure read) leaves exactly this expectation unmet.
// The r1 form (zero write expectations) could never fail: an
// unexpected sqlmock call returns an error the r0 best-effort
// backfill swallowed, and ExpectationsWereMet cannot see extra calls.
func TestGetWorkspace_NullPhrase_PerformsNoWrite(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	rows := sqlmock.NewRows([]string{"id", "user_id", "name", "runtime", "storage_size", "image_tag", "agent_version", "created_at", "updated_at", "default_model", "agent_needs_refresh", "credentials_pending_since", "org_id", "phrase_id"}).
		AddRow("ws-legacy", "user-1", "Legacy", "python:3.11", "10Gi", "sha-abc", "1.15.12",
			time.Now(), time.Now(), "", false, nil, nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size")).
		WithArgs("ws-legacy").
		WillReturnRows(rows)
	// Armed tripwire: any UPDATE during the read consumes this,
	// silences the unmet-expectation error, and fails the assertion.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE workspaces")).
		WillReturnError(errors.New("pure read must not write"))

	ws, err := s.GetWorkspace(context.Background(), "ws-legacy")
	require.NoError(t, err)
	require.Empty(t, ws.PhraseID, "NULL phrase_id must surface as empty")

	// nil here means the tripwire was CONSUMED — a write fired.
	unmet := mock.ExpectationsWereMet()
	require.Error(t, unmet, "ExpectationsWereMet==nil ⇒ the UPDATE was consumed ⇒ GetWorkspace wrote")
	assert.Contains(t, unmet.Error(), "UPDATE workspaces",
		"the only unmet expectation must be the armed UPDATE")
}

// r2 (verdict Robustness #1): the registration UPDATE carries the
// deleted_at IS NULL guard — a soft-deleted row (0 rows affected,
// empty read-back) answers ("", nil) immediately instead of minting
// onto a dead row or burning the retry budget.
func TestEnsureWorkspacePhraseRegistered_SoftDeletedRow_NeverWrites(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET phrase_id = $2, updated_at = NOW()`)).
		WillReturnResult(sqlmock.NewResult(0, 0)) // guard: no live row matched
	// The REAL dead-row state (r3): migration 000038 leaves legacy rows
	// NULL — a plain-string scan would ERROR here before the early
	// return; the pin stubs the actual NULL the seam must survive.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT phrase_id FROM workspaces WHERE id = $1`)).
		WithArgs("ws-dead").
		WillReturnRows(sqlmock.NewRows([]string{"phrase_id"}).AddRow(nil))

	phrase, err := s.EnsureWorkspacePhraseRegistered(context.Background(), "ws-dead")
	require.NoError(t, err)
	assert.Empty(t, phrase, "a dead row has nothing live to converge onto")
	require.NoError(t, mock.ExpectationsWereMet(),
		"exactly one UPDATE + one read-back — no retry loop on a dead row")
}

// r1: a mid-stream failure on the name lookup is an ERROR on every
// cardinality path — previously rows.Err() was only checked on the
// exactly-one-row return, folding a broken stream into a clean
// "absent"/"ambiguous" miss (a confident 404 on an infra fault).
func TestFindWorkspaceIDByName_StreamErrorIsError(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM workspaces WHERE user_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`)).
		WithArgs("user-1", "my-space").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("ws-a").AddRow("ws-b").AddRow("ws-c").
			RowError(2, errors.New("connection reset mid-stream")))

	if _, err := s.FindWorkspaceIDByName(context.Background(), "user-1", "my-space"); err == nil {
		t.Fatal("a mid-stream failure must surface as an error, never a clean miss")
	}
}
