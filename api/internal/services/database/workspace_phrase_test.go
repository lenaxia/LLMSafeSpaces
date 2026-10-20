// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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
