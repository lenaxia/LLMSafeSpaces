// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ensureRevisionMaxAttempts bounds the converge loop. Each attempt is
// one conditional UPDATE (plus, on the first-ever path, one INSERT);
// three attempts covers a two-writer race with margin. Exhaustion means
// sustained contention — surface ErrRevisionConvergeFailed rather than
// spin.
const ensureRevisionMaxAttempts = 3

// CurrentRevision returns the stored revision row for a workspace, or
// ok=false when no revision exists yet (first build). workspaceID is the
// delivery-path workspace identifier (the CR name).
func (s *PgSecretStore) CurrentRevision(ctx context.Context, workspaceID string) (int64, string, bool, error) {
	var seq int64
	var manifestHash string
	err := s.pool.QueryRow(ctx, `
		SELECT seq, manifest_hash FROM workspace_secret_revisions
		WHERE workspace_id = $1
	`, workspaceID).Scan(&seq, &manifestHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("query workspace revision: %w", err)
	}
	return seq, manifestHash, true, nil
}

// EnsureRevision converges the workspace's revision to manifestHash.
// The DB row is the single writer of the seq: every mutation of the
// stored hash happens in the same conditional UPDATE that increments
// seq, so concurrently minted seqs are distinct and monotonic by
// construction.
//
// Loop (bounded by ensureRevisionMaxAttempts):
//
//  1. UPDATE ... SET seq = seq + 1, manifest_hash = $new WHERE
//     workspace_id = $ws AND manifest_hash <> $new RETURNING seq — a row
//     means this caller won the right to publish manifestHash at that
//     seq. No row means either first-ever (INSERT ... ON CONFLICT DO
//     NOTHING mints seq 1) or someone else already published a hash —
//     fall through to the SELECT arm.
//  2. SELECT seq, manifest_hash — hash == ours means identical content
//     is already current (idempotent rebuild): return that seq. Any
//     other hash means a concurrent mutation won the race: retry from 1.
//  3. Exhausted → ErrRevisionConvergeFailed (wrapped, never a fabricated
//     seq).
func (s *PgSecretStore) EnsureRevision(ctx context.Context, workspaceID, manifestHash string) (int64, error) {
	for attempt := 0; attempt < ensureRevisionMaxAttempts; attempt++ {
		var seq int64
		err := s.pool.QueryRow(ctx, `
			UPDATE workspace_secret_revisions
			SET seq = seq + 1, manifest_hash = $2, updated_at = now()
			WHERE workspace_id = $1 AND manifest_hash <> $2
			RETURNING seq
		`, workspaceID, manifestHash).Scan(&seq)
		if err == nil {
			return seq, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("mint workspace revision: %w", err)
		}

		tag, ierr := s.pool.Exec(ctx, `
			INSERT INTO workspace_secret_revisions (workspace_id, seq, manifest_hash)
			VALUES ($1, 1, $2)
			ON CONFLICT (workspace_id) DO NOTHING
		`, workspaceID, manifestHash)
		if ierr != nil {
			return 0, fmt.Errorf("seed workspace revision: %w", ierr)
		}
		if tag.RowsAffected() == 1 {
			return 1, nil
		}

		var curHash string
		err = s.pool.QueryRow(ctx, `
			SELECT seq, manifest_hash FROM workspace_secret_revisions
			WHERE workspace_id = $1
		`, workspaceID).Scan(&seq, &curHash)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read workspace revision: %w", err)
		}
		if curHash == manifestHash {
			return seq, nil
		}
	}
	return 0, fmt.Errorf("%w: workspace %s (manifest %s)", ErrRevisionConvergeFailed, workspaceID, manifestHash)
}
