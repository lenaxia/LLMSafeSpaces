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
func TestGetWorkspace_NullPhrase_IsPureRead(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	rows := sqlmock.NewRows([]string{"id", "user_id", "name", "runtime", "storage_size", "image_tag", "agent_version", "created_at", "updated_at", "default_model", "agent_needs_refresh", "credentials_pending_since", "org_id", "phrase_id"}).
		AddRow("ws-legacy", "user-1", "Legacy", "python:3.11", "10Gi", "sha-abc", "1.15.12",
			time.Now(), time.Now(), "", false, nil, nil, nil)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT w.id, w.user_id, w.name, w.runtime, w.storage_size")).
		WithArgs("ws-legacy").
		WillReturnRows(rows)

	ws, err := s.GetWorkspace(context.Background(), "ws-legacy")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if ws.PhraseID != "" {
		t.Fatalf("NULL phrase_id must surface as empty, got %q", ws.PhraseID)
	}
	// No UPDATE, no convergence SELECT — ExpectationsWereMet with zero
	// write expectations IS the pin.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL fired: %v", err)
	}
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
