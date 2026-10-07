// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
)

func TestSetSessionArchivedStatus_ArchivesRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE session_index SET archived`)).
		WithArgs("ws-1", "ses-1", true).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.SetSessionArchivedStatus(context.Background(), "ws-1", "ses-1", true)
	require.NoError(t, err)
}

func TestSetSessionArchivedStatus_UnarchivesRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE session_index SET archived`)).
		WithArgs("ws-1", "ses-1", false).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.SetSessionArchivedStatus(context.Background(), "ws-1", "ses-1", false)
	require.NoError(t, err)
}

func TestSetSessionArchivedStatus_RowAbsent_ReturnsNotFound(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE session_index SET archived`)).
		WithArgs("ws-1", "ses-ghost", true).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := svc.SetSessionArchivedStatus(context.Background(), "ws-1", "ses-ghost", true)
	require.Error(t, err)
	var apiErr *apierrors.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 404, apiErr.StatusCode(), "unindexed session must surface as 404, not a silent no-op")
}

func TestSetSessionArchivedStatus_ExecError_Propagates(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE session_index SET archived`)).
		WithArgs("ws-1", "ses-1", true).
		WillReturnError(errors.New("db down"))

	err := svc.SetSessionArchivedStatus(context.Background(), "ws-1", "ses-1", true)
	require.Error(t, err)
}

func TestIsSessionArchived_TrueForArchivedRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"archived"}).AddRow(true)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT archived FROM session_index`)).
		WithArgs("ws-1", "ses-1").
		WillReturnRows(rows)

	got, err := svc.IsSessionArchived(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.True(t, got)
}

func TestIsSessionArchived_FalseForUnarchivedRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{"archived"}).AddRow(false)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT archived FROM session_index`)).
		WithArgs("ws-1", "ses-1").
		WillReturnRows(rows)

	got, err := svc.IsSessionArchived(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestIsSessionArchived_AbsentRowIsNotArchived(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	// The session index is event-fed and rows can lag or be absent
	// (#1452 list-visibility gap): an absent row MUST read as
	// not-archived — archiving can never lock an unknown session.
	rows := sqlmock.NewRows([]string{"archived"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT archived FROM session_index`)).
		WithArgs("ws-1", "ses-unindexed").
		WillReturnRows(rows)

	got, err := svc.IsSessionArchived(context.Background(), "ws-1", "ses-unindexed")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestIsSessionArchived_QueryError_Propagates(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT archived FROM session_index`)).
		WithArgs("ws-1", "ses-1").
		WillReturnError(errors.New("db down"))

	_, err := svc.IsSessionArchived(context.Background(), "ws-1", "ses-1")
	require.Error(t, err)
}

func TestListSessionIndex_IncludesArchivedFlag(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at",
		"message_count", "last_seen_at", "has_unread", "context_used", "archived",
	}).AddRow("ses-1", "Old chat", nil, nil, 7, nil, false, nil, true).
		AddRow("ses-2", "Live chat", nil, nil, 3, nil, false, nil, false)

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT session_id, title, parent_session_id, last_message_at, message_count, last_seen_at`,
	)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.True(t, items[0].Archived, "archived row surfaces the flag for the sidebar's Archived group")
	assert.False(t, items[1].Archived)
}
