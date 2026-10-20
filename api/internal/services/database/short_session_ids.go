// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// registerRetries bounds the mint's check-and-retry loop (ruling 3).
// Expected lifetime collisions ≈ 0.002% in a 10k-session workspace;
// 5 attempts make a lost race astronomically unlikely to surface.
const registerRetries = 5

// EnsureSessionRegistered is design 0066's single registration seam
// (§4.2): the one DB write path that can create a short id. Mint in Go
// (crypto/rand over the 36-symbol alphabet — never SQL random, never a
// deterministic derivation), INSERT … ON CONFLICT (workspace_id,
// session_id) DO NOTHING, and on the unique-violation path re-mint and
// retry (ruling 3's check-and-retry). A pre-existing row without a
// short id (rollout backfill) is filled atomically by UPDATE … WHERE
// short_id IS NULL; a raced fill converges on re-read. Idempotent:
// re-registering returns the surviving short id, never re-mints.
func (s *Service) EnsureSessionRegistered(ctx context.Context, workspaceID, sessionID, harnessType string) (string, error) {
	// #1756: the mint is a 3-word phrase; a collision (the partial
	// unique index raises 23505) re-mints with the DETERMINISTIC
	// counter suffix — base-2, then base-3, … — never a re-roll.
	for attempt := 0; attempt < registerRetries; attempt++ {
		minted, err := shortid.MintPhrase()
		if err != nil {
			return "", err
		}
		if attempt > 0 {
			minted = shortid.WithCounterSuffix(minted, attempt+1)
		}
		tag, err := s.DB.ExecContext(ctx,
			`INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at)
			 VALUES ($1, $2, $3, 0, NOW())
			 ON CONFLICT (workspace_id, session_id) DO NOTHING`,
			workspaceID, sessionID, minted)
		if err != nil {
			if isUniqueViolation(err) {
				// The (workspace_id, short_id) partial unique index
				// arbitrated a mint collision — re-mint.
				continue
			}
			return "", err
		}
		n, err := tag.RowsAffected()
		if err != nil {
			return "", err
		}
		if n == 1 {
			return minted, nil
		}
		// Row already exists: read back the surviving short id.
		var existing sql.NullString
		err = s.DB.QueryRowContext(ctx,
			`SELECT short_id FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
			workspaceID, sessionID).Scan(&existing)
		if err != nil {
			return "", err
		}
		if existing.Valid && existing.String != "" {
			return existing.String, nil
		}
		// Pre-rollout row (short_id NULL): backfill atomically. The
		// WHERE short_id IS NULL guard makes two racing replicas
		// converge — exactly one UPDATE lands, the loser re-reads.
		var filled sql.NullString
		err = s.DB.QueryRowContext(ctx,
			`UPDATE session_index SET short_id = $3, updated_at = NOW()
			 WHERE workspace_id = $1 AND session_id = $2 AND short_id IS NULL
			 RETURNING short_id`,
			workspaceID, sessionID, minted).Scan(&filled)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue // raced: re-read via the loop
			}
			if isUniqueViolation(err) {
				continue // mint collision on backfill — re-mint
			}
			return "", err
		}
		if filled.Valid && filled.String != "" {
			return filled.String, nil
		}
	}
	return "", fmt.Errorf("session registration did not converge (workspace %s, session %s) after %d attempts", workspaceID, sessionID, registerRetries)
}

// FindSessionByShortID resolves a case-insensitive short id (ruling 2:
// OC-AB12CD34 resolves) to the canonical harness session id within the
// workspace. Per-workspace uniqueness (ruling 3) means at most one row.
// Returns "" when no session holds the short id.
func (s *Service) FindSessionByShortID(ctx context.Context, workspaceID, shortID string) (string, error) {
	var sessionID string
	err := s.DB.QueryRowContext(ctx,
		`SELECT session_id FROM session_index WHERE workspace_id = $1 AND lower(short_id) = $2`,
		workspaceID, shortid.Normalize(shortID)).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

// FindSessionsByAlias returns the workspace's sessions whose current
// title equals the alias under casefold+trim (design 0066 §4.4
// precedence 3). More than one row is the ambiguity the caller must
// refuse — NEVER auto-resolve (ruling 8). Aliases are computed, never
// stored: a rename changes future resolution immediately.
func (s *Service) FindSessionsByAlias(ctx context.Context, workspaceID, alias string) ([]types.SessionListItem, error) {
	// ORDER BY last_message_at DESC: the ambiguity payload's
	// "newest first" contract (review r1: the truncation to the top-3
	// must be deterministic, never DB-order-arbitrary).
	rows, err := s.DB.QueryContext(ctx,
		`SELECT session_id, title, short_id, last_message_at
		 FROM session_index WHERE workspace_id = $1 AND lower(btrim(title)) = $2
		 ORDER BY last_message_at DESC NULLS LAST, session_id ASC`,
		workspaceID, shortid.Normalize(alias))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]types.SessionListItem, 0)
	for rows.Next() {
		var item types.SessionListItem
		var title sql.NullString
		var shortID sql.NullString
		var lastMsg sql.NullTime
		if err := rows.Scan(&item.ID, &title, &shortID, &lastMsg); err != nil {
			return nil, err
		}
		if title.Valid {
			item.Title = title.String
		}
		if shortID.Valid {
			item.ShortID = shortID.String
		}
		if lastMsg.Valid {
			t := lastMsg.Time
			item.LastMessageAt = &t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListWorkspaceIDsByOwner feeds the cross-workspace suggestion scope
// (§4.6): suggestions may only enumerate rows in workspaces the
// requester can already list.
func (s *Service) ListWorkspaceIDsByOwner(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE user_id = $1 AND deleted_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FindRefCandidatesAcrossWorkspaces exact-matches a ref across the
// given workspaces — short id case-insensitive, harness id
// byte-equal, alias casefolded. The wrong-workspace did-you-mean only:
// edit-distance scoring stays inside the caller's own workspace
// (bounded inventory, the #1667 covenant).
func (s *Service) FindRefCandidatesAcrossWorkspaces(ctx context.Context, workspaceIDs []string, ref string) ([]types.SessionListItem, error) {
	if len(workspaceIDs) == 0 {
		return nil, nil
	}
	// IN-list expansion (not = ANY($1)): stays driver-portable and
	// mock-friendly; the set is the owner's bounded workspace list.
	args := make([]any, 0, len(workspaceIDs)+2)
	placeholders := make([]string, 0, len(workspaceIDs))
	for i, id := range workspaceIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, id)
	}
	// TWO ref bindings (review r1 finding 2): the normalized (lower-
	// cased) form for the case-insensitive short-id and alias matches,
	// and the RAW form for the legacy harness id's BYTE-equal match —
	// harness ids are case-sensitive; lowercasing the needle would
	// make every mixed-case ses_… row unmatchable.
	args = append(args, shortid.Normalize(ref), ref)
	normIdx, rawIdx := len(args)-1, len(args)
	rows, err := s.DB.QueryContext(ctx,
		fmt.Sprintf(`SELECT workspace_id, session_id, title, short_id, last_message_at
		 FROM session_index
		 WHERE workspace_id IN (%s)
		   AND (lower(short_id) = $%d OR session_id = $%d OR lower(btrim(title)) = $%d)`,
			strings.Join(placeholders, ", "), normIdx, rawIdx, normIdx),
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]types.SessionListItem, 0)
	for rows.Next() {
		var item types.SessionListItem
		var title sql.NullString
		var shortID sql.NullString
		var lastMsg sql.NullTime
		if err := rows.Scan(&item.Workspace, &item.ID, &title, &shortID, &lastMsg); err != nil {
			return nil, err
		}
		if title.Valid {
			item.Title = title.String
		}
		if shortID.Valid {
			item.ShortID = shortID.String
		}
		if lastMsg.Valid {
			t := lastMsg.Time
			item.LastMessageAt = &t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// --- #1757: workspace phrase IDs -------------------------------------------

// workspacePhraseRetries bounds the fleet-unique check-and-retry.
// Workspaces are fleet-scarce; collisions are correspondingly rare.
const workspacePhraseRetries = 5

// EnsureWorkspacePhraseRegistered mints and persists a workspace's
// phrase id (idempotent — re-registering returns the surviving
// phrase). The global partial unique index (phrase_id WHERE NOT NULL
// AND deleted_at IS NULL) arbitrates races across replicas.
func (s *Service) EnsureWorkspacePhraseRegistered(ctx context.Context, workspaceID string) (string, error) {
	for attempt := 0; attempt < workspacePhraseRetries; attempt++ {
		minted := shortid.MintWorkspacePhrase()
		tag, err := s.DB.ExecContext(ctx,
			`UPDATE workspaces SET phrase_id = $2, updated_at = NOW()
			 WHERE id = $1 AND (phrase_id IS NULL OR phrase_id = '')`,
			workspaceID, minted)
		if err != nil {
			if isUniqueViolation(err) {
				continue // fleet collision — fresh draw
			}
			return "", err
		}
		n, err := tag.RowsAffected()
		if err != nil {
			return "", err
		}
		var phrase string
		readErr := s.DB.QueryRowContext(ctx,
			`SELECT phrase_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&phrase)
		if readErr != nil {
			return "", readErr
		}
		if n == 1 || phrase != "" {
			return phrase, nil
		}
	}
	return "", fmt.Errorf("workspace phrase registration did not converge (%s) after %d attempts", workspaceID, workspacePhraseRetries)
}

// FindWorkspaceIDByPhrase resolves a case-insensitive workspace
// phrase to the workspace UUID — FLEET scope (unlike session phrases,
// workspace phrases are globally unique). "" when no live workspace
// holds the phrase.
func (s *Service) FindWorkspaceIDByPhrase(ctx context.Context, phrase string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE phrase_id = $1 AND deleted_at IS NULL`,
		shortid.Normalize(phrase)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// FindWorkspaceIDByName resolves a workspace NAME to the UUID — the
// unambiguous-only input form of the #1757 deprecation window (names
// are caller-owned and may collide: 0 or >1 rows is a miss).
func (s *Service) FindWorkspaceIDByName(ctx context.Context, userID, name string) (string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE user_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`,
		userID, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	// r1: a mid-iteration stream failure must surface as an error on
	// EVERY cardinality path — previously only the exactly-one-row
	// return checked rows.Err(), folding a broken stream into a clean
	// "absent"/"ambiguous" miss (a confident 404 on infra fault).
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", nil // ambiguous or absent — never guess
	}
	return ids[0], nil
}
