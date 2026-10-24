// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

// session_phrase_ledger.go — #1889 F1: the durable phrase binding. The
// ledger is one row per (workspace_id, session_id) holding the phrase
// the session's index row was born under, keyed INDEPENDENTLY of the
// session_index row so it SURVIVES row death (migration 000047 — see
// its header for the uniqueness/reaping/retention design decisions).
// The mint seams consult it before minting: a re-born row converges on
// its original phrase instead of a fresh mint, killing the residual
// re-phrase class (an agent holding the old phrase watches it stop
// resolving while the session lives on under a new identity).
//
// Durability posture: the ledger is best-effort for CONVERGENCE, never
// for correctness — every failure path degrades to today's fresh-mint
// semantics and the caller's write proceeds untouched.

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// consultPhraseLedger returns the durable phrase bound to
// (workspaceID, sessionID), "" when none is bound. Any error degrades
// to "" (the caller mints fresh — convergence is best-effort).
func (s *Service) consultPhraseLedger(ctx context.Context, workspaceID, sessionID string) string {
	var phrase string
	err := s.DB.QueryRowContext(ctx,
		`SELECT short_id FROM session_phrase_ledger WHERE workspace_id = $1 AND session_id = $2`,
		workspaceID, sessionID).Scan(&phrase)
	if err != nil {
		return "" // absent, or the ledger is unavailable — mint fresh
	}
	return phrase
}

// claimPhraseLedger binds phrase to (workspaceID, sessionID) FIRST-
// WRITER-WINS and returns the WINNING phrase (the caller must birth
// the row under the winner, so racing claimants converge on one
// phrase — the registration seam's check-and-retry discipline one
// level down). The no-op DO UPDATE keeps RETURNING populated on the
// conflict path: the existing binding wins and is echoed back.
func (s *Service) claimPhraseLedger(ctx context.Context, workspaceID, sessionID, phrase string) (string, error) {
	var winner string
	err := s.DB.QueryRowContext(ctx,
		`INSERT INTO session_phrase_ledger (workspace_id, session_id, short_id, minted_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (workspace_id, session_id) DO UPDATE SET short_id = session_phrase_ledger.short_id
		 RETURNING short_id`,
		workspaceID, sessionID, phrase).Scan(&winner)
	if err != nil {
		return "", err
	}
	return winner, nil
}

// recordPhraseLedger corrects the binding to the phrase a row was
// ACTUALLY born under (the deterministic-suffix collision path: the
// ledger phrase was re-minted by another live row, so the resurrection
// settled on the suffixed form — the ledger must track reality). Last-
// writer-wins; best-effort.
func (s *Service) recordPhraseLedger(ctx context.Context, workspaceID, sessionID, phrase string) {
	_, _ = s.DB.ExecContext(ctx,
		`UPDATE session_phrase_ledger SET short_id = $3, minted_at = NOW()
		 WHERE workspace_id = $1 AND session_id = $2`,
		workspaceID, sessionID, phrase)
}

// phraseBaseFor returns the mint base for (workspaceID, sessionID):
// the ledger phrase when one is durably bound (the #1889 convergence),
// else a fresh mint claimed into the ledger (so the binding exists
// before the row is born — a crash between claim and birth still
// converges later). A ledger failure degrades to an unclaimed fresh
// mint (today's semantics).
func (s *Service) phraseBaseFor(ctx context.Context, workspaceID, sessionID string) string {
	if bound := s.consultPhraseLedger(ctx, workspaceID, sessionID); bound != "" {
		return bound
	}
	fresh, err := shortid.MintPhrase()
	if err != nil {
		return ""
	}
	winner, err := s.claimPhraseLedger(ctx, workspaceID, sessionID, fresh)
	if err != nil {
		return fresh // ledger unavailable — birth under the fresh mint
	}
	return winner
}
