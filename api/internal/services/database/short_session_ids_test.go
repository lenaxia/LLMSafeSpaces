// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// #1663 regression ids.
const (
	regLiveID  = "ses_ed9183a46ffeRiNw2PddpL5dAA"
	regShortID = "oc-ab12cd34"
	regWS      = "ws-1691"
)

func TestEnsureSessionRegistered_FreshMint(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WithArgs(regWS, regLiveID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode")
	if err != nil {
		t.Fatalf("EnsureSessionRegistered: %v", err)
	}
	// #1756: the mint is a 3-word phrase — never a prefixed hash.
	if !shortid.IsShortIDForm(got) || shortid.IsLegacyShortIDForm(got) {
		t.Errorf("minted id %q: not a canonical #1756 phrase", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEnsureSessionRegistered_IdempotentNoRemint(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Row exists with a short id: INSERT no-ops, read-back returns it.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`)).
		WithArgs(regWS, regLiveID).
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow(regShortID))

	got, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode")
	if err != nil {
		t.Fatalf("EnsureSessionRegistered: %v", err)
	}
	if got != regShortID {
		t.Errorf("got %q; want existing %q (re-register must never re-mint)", got, regShortID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEnsureSessionRegistered_BackfillsNullShortID(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Pre-rollout row (short_id NULL): read-back sees NULL, the
	// conditional UPDATE fills it.
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`)).
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow(nil))
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE session_index SET short_id = $3, updated_at = NOW()`)).
		WillReturnRows(sqlmock.NewRows([]string{"short_id"}).AddRow(regShortID))

	got, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode")
	if err != nil {
		t.Fatalf("EnsureSessionRegistered: %v", err)
	}
	if got != regShortID {
		t.Errorf("got %q; want backfilled %q", got, regShortID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEnsureSessionRegistered_RetriesOnShortIDCollision(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Ruling 3's check-and-retry: the (workspace_id, short_id) partial
	// unique index arbitrates a mint collision with 23505 — the loser
	// re-mints and re-inserts. Two collisions, then success.
	uniq := &pgconn.PgError{Code: "23505", ConstraintName: "idx_session_index_short"}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnError(uniq)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnError(uniq)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode")
	if err != nil {
		t.Fatalf("EnsureSessionRegistered after retries: %v", err)
	}
	if !shortid.IsShortIDForm(got) {
		t.Errorf("got %q; want a minted short id", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEnsureSessionRegistered_NonUniqueErrorPropagates(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WillReturnError(errors.New("connection refused"))

	if _, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode"); err == nil {
		t.Fatal("non-23505 errors must propagate, not retry-loop")
	}
}

func TestFindSessionByShortID_CaseInsensitive(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id FROM session_index WHERE workspace_id = $1 AND lower(short_id) = $2`)).
		WithArgs(regWS, "oc-ab12cd34").
		WillReturnRows(sqlmock.NewRows([]string{"session_id"}).AddRow(regLiveID))

	got, err := s.FindSessionByShortID(context.Background(), regWS, "OC-AB12CD34")
	if err != nil {
		t.Fatalf("FindSessionByShortID: %v", err)
	}
	if got != regLiveID {
		t.Errorf("got %q; want %q (case-insensitive resolution)", got, regLiveID)
	}
}

func TestFindSessionByShortID_Absent(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id FROM session_index WHERE workspace_id = $1 AND lower(short_id) = $2`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id"}))

	got, err := s.FindSessionByShortID(context.Background(), regWS, "oc-00000000")
	if err != nil {
		t.Fatalf("absent short id must not error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q; want \"\"", got)
	}
}

func TestFindSessionsByAlias_AmbiguityVisible(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// Two sessions share the title (the fleet's live alias-collision
	// case): BOTH rows must come back — the resolution layer refuses,
	// the suggestion layer disambiguates.
	// ANCHORED (review r2 finding C): sqlmock matches by substring, so
	// the expectation must carry the ORDER BY explicitly — deleting it
	// from the query must FAIL this pin (the deterministic newest-first
	// truncation contract).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT session_id, title, short_id, last_message_at FROM session_index WHERE workspace_id = $1 AND lower(btrim(title)) = $2 ORDER BY last_message_at DESC NULLS LAST, session_id ASC`)).
		WithArgs(regWS, "worker: #1691").
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "title", "short_id", "last_message_at"}).
			AddRow("ses_a", "Worker: #1691", "oc-11111111", nil).
			AddRow("ses_b", "worker: #1691 ", "oc-22222222", nil))

	rows, err := s.FindSessionsByAlias(context.Background(), regWS, "Worker: #1691")
	if err != nil {
		t.Fatalf("FindSessionsByAlias: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows; want 2 (ambiguity must be visible, never collapsed)", len(rows))
	}
	if rows[0].ShortID != "oc-11111111" || rows[1].ShortID != "oc-22222222" {
		t.Errorf("short ids not carried: %+v", rows)
	}
}

func TestListWorkspaceIDsByOwner(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id FROM workspaces WHERE user_id = $1 AND deleted_at IS NULL`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-a").AddRow("ws-b"))

	ids, err := s.ListWorkspaceIDsByOwner(context.Background(), "user-1")
	if err != nil || len(ids) != 2 || ids[0] != "ws-a" || ids[1] != "ws-b" {
		t.Fatalf("ListWorkspaceIDsByOwner = %v,%v", ids, err)
	}
}

func TestFindRefCandidatesAcrossWorkspaces(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	// TWO ref bindings (review r1 finding 2): the normalized form for
	// the short-id/alias matches AND the raw form for the legacy id's
	// byte-equal match.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT workspace_id, session_id, title, short_id, last_message_at FROM session_index`)).
		WithArgs("ws-a", "ws-b", "oc-ab12cd34", "oc-ab12cd34").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "session_id", "title", "short_id", "last_message_at"}).
			AddRow("ws-b", regLiveID, "Worker: #1648", regShortID, nil))

	rows, err := s.FindRefCandidatesAcrossWorkspaces(context.Background(), []string{"ws-a", "ws-b"}, regShortID)
	if err != nil {
		t.Fatalf("FindRefCandidatesAcrossWorkspaces: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != regLiveID {
		t.Fatalf("got %+v; want the ws-b row", rows)
	}
	// Workspace must ride on the row for the cross-workspace payload.
	if rows[0].Workspace != "ws-b" {
		t.Errorf("workspace not carried on the candidate row")
	}
}

// TestFindRefCandidatesAcrossWorkspaces_MixedCaseLegacyID pins
// finding 2's fix at the SQL-shape tier: a MIXED-CASE legacy id binds
// the byte-equal comparison RAW (second ref arg), not lowercased —
// a normalized-only binding would make every mixed-case ses_… row
// unmatchable across workspaces.
func TestFindRefCandidatesAcrossWorkspaces_MixedCaseLegacyID(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mixed := "ses_eD9183a46FfeRiNw2PddpL5dAA"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT workspace_id, session_id, title, short_id, last_message_at FROM session_index`)).
		WithArgs("ws-b", strings.ToLower(mixed), mixed).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "session_id", "title", "short_id", "last_message_at"}).
			AddRow("ws-b", mixed, "Worker: #1648", "", nil))

	rows, err := s.FindRefCandidatesAcrossWorkspaces(context.Background(), []string{"ws-b"}, mixed)
	if err != nil {
		t.Fatalf("FindRefCandidatesAcrossWorkspaces: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != mixed {
		t.Fatalf("mixed-case legacy id must byte-equal match: %+v", rows)
	}
}

// recordingArg captures the bound short_id on every attempt and matches
// anything — the determinism pin below constrains the SEQUENCE.
type recordingArg struct{ seen *[]string }

func (r recordingArg) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		*r.seen = append(*r.seen, s)
	}
	return true
}

// #1756 finding 7 (post-union review hard gate): the mint base is
// drawn ONCE and retries suffix the SAME base — attempt 2 must bind
// base+"-2", attempt 3 base+"-3" (two racers on one base converge on
// the identical sequence). A revert to a fresh mint per attempt binds
// three unrelated phrases and fails.
func TestEnsureSessionRegistered_RetriesSuffixTheSameBase(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	var binds []string
	uniq := &pgconn.PgError{Code: "23505", ConstraintName: "idx_session_index_short"}
	for i := 0; i < 3; i++ {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
			WithArgs(regWS, regLiveID, recordingArg{seen: &binds}).
			WillReturnError(uniq)
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)`)).
		WithArgs(regWS, regLiveID, recordingArg{seen: &binds}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := s.EnsureSessionRegistered(context.Background(), regWS, regLiveID, "opencode")
	if err != nil {
		t.Fatalf("EnsureSessionRegistered after retries: %v", err)
	}
	if len(binds) != 4 {
		t.Fatalf("expected 4 bound inserts, saw %d (%v)", len(binds), binds)
	}
	for i, b := range binds {
		if !shortid.IsShortIDForm(b) {
			t.Fatalf("bind %d %q is not a phrase form", i, b)
		}
	}
	if binds[1] != binds[0]+"-2" {
		t.Errorf("attempt 2 bound %q; want base-2 (%q-2) — the base must not re-roll", binds[1], binds[0])
	}
	if binds[2] != binds[0]+"-3" {
		t.Errorf("attempt 3 bound %q; want base-3 (%q-3)", binds[2], binds[0])
	}
	if binds[3] != binds[0]+"-4" {
		t.Errorf("attempt 4 bound %q; want base-4 (%q-4)", binds[3], binds[0])
	}
	if got != binds[3] {
		t.Errorf("returned %q; want the last bound %q", got, binds[3])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
