// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// events.go — idempotency + audit (design §4.2): every mutating action
// accepts an optional caller-supplied requestId; board_events.id is that
// key, written in the SAME transaction as the effect (marker-after-persist
// ordering the outbox established — here effect and marker are one commit).
// A replayed requestId hits the (project_id, id) uniqueness violation and
// the action returns the original outcome ("200-with-original, never an
// error"). System (reconciler) writes carry deterministic ids so
// convergence never depends on caller discipline.

// eventWriter runs fn inside a transaction that also writes the audit
// event. On a duplicate requestId it returns (*DuplicateError, nil) after
// the transaction aborts — callers render the original outcome.
type eventWriter struct {
	svc       *Service
	projectID string
	requestID string
	action    string
	itemKind  string
	itemID    string
	actor     Actor
}

// run executes: BEGIN → fast-path duplicate check → fn(effect) → failpoint
// → INSERT event → COMMIT. fn receives the tx and returns its outcome
// payload (serialized into the event row so a replay can return the
// original).
func (w *eventWriter) run(ctx context.Context, fn func(tx pgx.Tx) (map[string]any, error)) (map[string]any, *DuplicateError, error) {
	if w.requestID == "" {
		w.requestID = uuid.NewString()
	}
	pool := w.svc.store.pool
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Fast-path duplicate check (the authoritative gate is the INSERT).
	var existing []byte
	err = tx.QueryRow(ctx,
		`SELECT payload FROM board_events WHERE project_id=$1 AND id=$2`,
		w.projectID, w.requestID).Scan(&existing)
	if err == nil {
		return decodeEventPayload(existing), &DuplicateError{EventID: w.requestID, Action: w.action, Result: decodeEventPayload(existing)}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}

	outcome, err := fn(tx)
	if err != nil {
		return nil, nil, err
	}

	// The crash-between-action-and-effect fault seam (§6 invariant 4): a
	// fault here aborts effect AND marker together — nothing lands.
	if w.svc.failpoint != nil {
		if ferr := w.svc.failpoint(w.action + ".effect"); ferr != nil {
			return nil, nil, ferr
		}
	}

	payload, err := json.Marshal(outcome)
	if err != nil {
		return nil, nil, fmt.Errorf("board: event payload marshal: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO board_events (id, project_id, item_kind, item_id, action, actor, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.requestID, w.projectID, w.itemKind, w.itemID, w.action, actorViewOf(w.actor), payload)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// A concurrent replay raced us to the marker: abort ours,
			// return theirs (the outbox's race-loser discipline).
			_ = tx.Rollback(ctx)
			var orig []byte
			if qErr := pool.QueryRow(ctx,
				`SELECT payload FROM board_events WHERE project_id=$1 AND id=$2`,
				w.projectID, w.requestID).Scan(&orig); qErr == nil {
				return decodeEventPayload(orig), &DuplicateError{EventID: w.requestID, Action: w.action, Result: decodeEventPayload(orig)}, nil
			}
			return nil, &DuplicateError{EventID: w.requestID, Action: w.action}, nil
		}
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return outcome, nil, nil
}

func decodeEventPayload(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// EventPublisher receives committed board events (design §4.6: SSE is a
// read projection of board_events — persist first, publish after the
// commit). Installed at wiring time (app.go → the user event broker);
// nil means no projection (engine dark / broker absent — writes succeed
// unaffected). Publishers must not block: the SSE reseed belt (#1646/
// #1687) is the consumer's safety net, not publisher retry logic.
type EventPublisher func(Event)

// insertEventTx writes an event row inside an open transaction with a
// caller-chosen deterministic id (engine/reconciler writes). ON CONFLICT
// DO NOTHING: a replayed deterministic id is inert convergence.
func insertEventTx(ctx context.Context, tx pgx.Tx, projectID, id, action, itemKind, itemID string, actor Actor, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO board_events (id, project_id, item_kind, item_id, action, actor, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project_id, id) DO NOTHING`,
		id, projectID, itemKind, itemID, action, actorViewOf(actor), b)
	return err
}
