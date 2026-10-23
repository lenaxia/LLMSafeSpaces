// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_turn_evidence.go — the #1832 WS4 stillborn guard's production
// evidence source: the session index's message_count (agentd-maintained,
// the same counter session_metadata serves). A session id is a globally
// unique platform key, so the lookup needs no workspace scoping; a
// MISSING row is a zero (a session the index never saw produced no
// turns — the stillborn's exact shape).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// boardTurnEvidence adapts the session index to the board seam.
type boardTurnEvidence struct {
	pool *pgxpool.Pool
}

// SessionTurnEvidence reports the session's message count (0 for a
// row the index never recorded — positive-zero, the guard's trigger).
func (b *boardTurnEvidence) SessionTurnEvidence(ctx context.Context, sessionID string) (int, error) {
	if b == nil || b.pool == nil {
		return 0, fmt.Errorf("board: turn evidence: no pool")
	}
	var n int
	err := b.pool.QueryRow(ctx,
		`SELECT message_count FROM session_index WHERE session_id = $1`, sessionID).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil // never indexed = never turned
		}
		return 0, err
	}
	return n, nil
}
