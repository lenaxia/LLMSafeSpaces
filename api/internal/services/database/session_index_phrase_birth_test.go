// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// #1850 persistence-by-construction: every row-creating session_index
// upsert must birth the row WITH its phrase — short_id rides the INSERT
// leg (single statement = the phrase is minted in the same transaction
// as the row), and the ON CONFLICT DO UPDATE leg never touches
// short_id (the existing phrase always wins; a re-birth after row
// death mints fresh, it never silently settles phrase-less). Before
// #1850 these six upserts inserted WITHOUT short_id: any of them
// landing while the row was absent (reaper, workspace reset, a lost
// create-time registration race) created a phrase-less row whose
// phrase was then "settled lazily" by a later backfill mint — a
// DIFFERENT phrase than the one create_session promised the spawner.
// These pins fail red against that shape.

// phraseBirthPins enumerates the six row-creating upserts: the SQL
// prefix each must carry (short_id in the INSERT column list) and the
// closure that exercises the call. matchers supplies the WithArgs
// tail after (workspace, session, phrase-recorder) — methods differ
// in what else they bind.
type phraseBirthPin struct {
	name    string
	exec    func(s *Service) error
	matcher func(rec recordingArg) []driver.Value
}

func phraseBirthPins(ws, ses string) []phraseBirthPin {
	at := time.Unix(1770000000, 0).UTC()
	any := sqlmock.AnyArg()
	return []phraseBirthPin{
		{
			name: "UpsertSessionMessage",
			exec: func(s *Service) error {
				return s.UpsertSessionMessage(context.Background(), ws, ses, at)
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec, any} },
		},
		{
			name: "UpsertSessionTitle",
			exec: func(s *Service) error {
				return s.UpsertSessionTitle(context.Background(), ws, ses, "title")
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec, any} },
		},
		{
			name: "UpsertSessionContextUsed",
			exec: func(s *Service) error {
				return s.UpsertSessionContextUsed(context.Background(), ws, ses, 1234)
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec, any} },
		},
		{
			name: "UpsertSessionMessageCount",
			exec: func(s *Service) error {
				return s.UpsertSessionMessageCount(context.Background(), ws, ses, 7)
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec, any} },
		},
		{
			name: "UpsertSessionParent",
			exec: func(s *Service) error {
				return s.UpsertSessionParent(context.Background(), ws, ses, "ses_parent")
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec, any} },
		},
		{
			// last-seen binds ONLY (ws, ses, phrase) — its timestamps
			// are statement-local NOW().
			name: "UpdateSessionLastSeen",
			exec: func(s *Service) error {
				return s.UpdateSessionLastSeen(context.Background(), ws, ses)
			},
			matcher: func(rec recordingArg) []driver.Value { return []driver.Value{ws, ses, rec} },
		},
	}
}

// TestPhraseBirthUpsertsCarryShortID pins the INSERT shape: the column
// list carries short_id and the third bound argument is a canonical
// phrase (the mint). Red against the pre-#1850 phrase-less inserts.
func TestPhraseBirthUpsertsCarryShortID(t *testing.T) {
	for _, pin := range phraseBirthPins(regWS, regLiveID) {
		t.Run(pin.name, func(t *testing.T) {
			s, mock, done := setupMockDB(t)
			defer done()

			var binds []string
			mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id,`)).
				WithArgs(pin.matcher(recordingArg{seen: &binds})...).
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := pin.exec(s); err != nil {
				t.Fatalf("%s: %v", pin.name, err)
			}
			if len(binds) != 1 {
				t.Fatalf("expected 1 bound short_id, saw %d — the INSERT leg must carry the mint", len(binds))
			}
			if !shortid.IsShortIDForm(binds[0]) {
				t.Errorf("bound short_id %q is not a canonical #1756 phrase", binds[0])
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestPhraseBirthUpsert_DOUPDATENeverTouchesShortID pins the conflict
// leg: the ON CONFLICT DO UPDATE clause must not appear to rebind
// short_id — anchored on the message upsert (the spawn-path writer).
// The anchored SQL must contain the DO UPDATE SET list verbatim; a
// revert that adds short_id to the SET list changes the clause and
// fails the anchor.
func TestPhraseBirthUpsert_DOUPDATENeverTouchesShortID(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	at := time.Unix(1770000000, 0).UTC()
	mock.ExpectExec(regexp.QuoteMeta(`ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		   last_message_at = EXCLUDED.last_message_at,
		   message_count = session_index.message_count + 1,
		   updated_at = NOW()`)).
		WithArgs(regWS, regLiveID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.UpsertSessionMessage(context.Background(), regWS, regLiveID, at); err != nil {
		t.Fatalf("UpsertSessionMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestPhraseBirthUpsert_CollisionRetriesWithSuffix: a phrase collision
// (the (workspace_id, short_id) partial unique index raises 23505 on
// the INSERT leg) re-mints with the deterministic counter suffix —
// ruling 3's discipline, now on every birth path, not just the
// registration seam.
func TestPhraseBirthUpsert_CollisionRetriesWithSuffix(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	var binds []string
	uniq := &pgconn.PgError{Code: "23505", ConstraintName: "idx_session_index_short"}
	at := time.Unix(1770000000, 0).UTC()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id,`)).
		WithArgs(regWS, regLiveID, recordingArg{seen: &binds}, sqlmock.AnyArg()).
		WillReturnError(uniq)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id,`)).
		WithArgs(regWS, regLiveID, recordingArg{seen: &binds}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.UpsertSessionMessage(context.Background(), regWS, regLiveID, at); err != nil {
		t.Fatalf("UpsertSessionMessage after collision retry: %v", err)
	}
	if len(binds) != 2 {
		t.Fatalf("expected 2 bound inserts, saw %d", len(binds))
	}
	if binds[1] != binds[0]+"-2" {
		t.Errorf("retry bound %q; want base-2 (%q-2) — the base must not re-roll", binds[1], binds[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestPhraseBirthUpsert_NonUniqueErrorPropagates: non-collision
// errors surface, never retry-loop.
func TestPhraseBirthUpsert_NonUniqueErrorPropagates(t *testing.T) {
	s, mock, done := setupMockDB(t)
	defer done()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO session_index (workspace_id, session_id, short_id,`)).
		WillReturnError(errors.New("connection refused"))

	if err := s.UpsertSessionTitle(context.Background(), regWS, regLiveID, "t"); err == nil {
		t.Fatal("non-23505 errors must propagate, not retry-loop")
	}
}
