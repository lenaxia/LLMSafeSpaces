package database

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSessionIndex_IncludesLastSeenAtAndHasUnread(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used", "archived",
	}).AddRow("ses-1", "Chat", nil, now, 5, now.Add(-time.Hour), true, nil, false).
		AddRow("ses-2", "Caught up", nil, now, 3, now, false, int64(5000), false).
		AddRow("ses-3", "Never visited", nil, now, 1, nil, false, nil, false)

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT session_id, title, parent_session_id, last_message_at, message_count, last_seen_at`,
	)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 3)

	assert.True(t, items[0].HasUnread, "last_message_at > last_seen_at → has_unread=true")
	assert.NotNil(t, items[0].LastSeenAt)
	assert.Nil(t, items[0].ContextUsed, "nil context_used in DB → nil pointer")

	assert.False(t, items[1].HasUnread, "last_seen_at >= last_message_at → has_unread=false")
	assert.NotNil(t, items[1].ContextUsed)
	assert.Equal(t, int64(5000), *items[1].ContextUsed)

	assert.False(t, items[2].HasUnread, "last_seen_at IS NULL → has_unread=false")
	assert.Nil(t, items[2].LastSeenAt)
}

func TestListSessionIndex_HasUnreadTrueWhenNewerMessage(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	lastMsg := time.Now()
	lastSeen := lastMsg.Add(-2 * time.Hour)
	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used", "archived",
	}).AddRow("ses-1", "Unread", nil, lastMsg, 10, lastSeen, true, nil, false)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id, title, parent_session_id`)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, items[0].HasUnread)
}

func TestListSessionIndex_HasUnreadFalseWhenCaughtUp(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	ts := time.Now()
	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used", "archived",
	}).AddRow("ses-1", "Caught up", nil, ts, 5, ts, false, nil, false)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id, title, parent_session_id`)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].HasUnread)
}

func TestListSessionIndex_HasUnreadFalseWhenNullSeenAt(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	rows := sqlmock.NewRows([]string{
		"session_id", "title", "parent_session_id", "last_message_at", "message_count", "last_seen_at", "has_unread", "context_used", "archived",
	}).AddRow("ses-1", "New session", nil, time.Now(), 1, nil, false, nil, false)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id, title, parent_session_id`)).WithArgs("ws-1").WillReturnRows(rows)

	items, err := svc.ListSessionIndex(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].HasUnread)
}

func TestUpdateSessionLastSeen_ExistingRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`ON CONFLICT (workspace_id, session_id) DO UPDATE SET`,
	)).WithArgs("ws-1", "ses-1").WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpdateSessionLastSeen(context.Background(), "ws-1", "ses-1")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpdateSessionLastSeen_NewRow(t *testing.T) {
	svc, mock, cleanup := setupMockDB(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO session_index (workspace_id, session_id, last_seen_at`,
	)).WithArgs("ws-1", "ses-new").WillReturnResult(sqlmock.NewResult(0, 1))

	err := svc.UpdateSessionLastSeen(context.Background(), "ws-1", "ses-new")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
