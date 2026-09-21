// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

// #1499: the user_prompts store rows. SQL-shape pins (sqlmock) + the
// semantics that matter (unique-violation → typed name-taken; missing
// row → typed not-found) — real-Postgres rows live in the integration
// suite.

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// TestListUserPrompts_WritesExpectedSQL pins the query shape INCLUDING
// the ORDER BY updated_at DESC clause — the contract element the
// consuming lane (#1496) depends on (handler passes the store's order
// through untouched, so this IS the order pin).
func TestListUserPrompts_WritesExpectedSQL(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"id", "name", "content", "created_at", "updated_at"}).
		AddRow("p1", "Weekly summary", "Summarize…", time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC))
	// Anchored FULL-query pin: column shape + WHERE + ORDER BY in one
	// expectation (r2: the clause-only pin let a column rename through).
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, name, content, created_at, updated_at FROM user_prompts WHERE user_id = $1 ORDER BY updated_at DESC LIMIT 500`,
	)).
		WithArgs("u1").
		WillReturnRows(rows)

	out, err := svc.ListUserPrompts(context.Background(), "u1")
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "Weekly summary", out[0].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateUserPrompt_UniqueViolationIsNameTaken(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(
		`INSERT INTO user_prompts (user_id, name, content)`,
	)).
		WithArgs("u1", "name", "content").
		WillReturnError(&fakeUniqueViolation{})

	_, err := svc.CreateUserPrompt(context.Background(), "u1", "name", "content")
	require.ErrorIs(t, err, ErrPromptNameTaken)
}

func TestUpdateUserPrompt_MissingRowIsNotFound(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(
		`UPDATE user_prompts SET`,
	)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "content", "created_at", "updated_at"}))

	name := "n2"
	content := "c2"
	_, err := svc.UpdateUserPrompt(context.Background(), "u1", "p1", &name, &content)
	require.ErrorIs(t, err, ErrPromptNotFound)
}

func TestDeleteUserPrompt_MissingRowIsNotFound(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM user_prompts WHERE`)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := svc.DeleteUserPrompt(context.Background(), "u1", "p1")
	require.ErrorIs(t, err, ErrPromptNotFound)
}

// fakeUniqueViolation satisfies the SQLState() shape isUniqueViolation
// inspects (both drivers' violation forms expose it).
type fakeUniqueViolation struct{}

func (*fakeUniqueViolation) Error() string    { return "unique violation" }
func (*fakeUniqueViolation) SQLState() string { return "23505" }
