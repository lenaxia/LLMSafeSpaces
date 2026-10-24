// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_sse.go — the §4.6 board.event publisher wiring (design 0067,
// #1699 gap 4): committed board events fan out on the EXISTING user +
// workspace SSE channels as flat "board.event" payloads carrying the
// board_events row. Persist-then-publish is the engine's contract
// (pkg/board publishes strictly post-commit); this file renders the row
// and fans it out. Fail-open: no broker or a failed project lookup logs
// and drops the projection — the write already committed, and the UI's
// reseed belt (#1646/#1687) is the consumer's safety net.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// warnLogger is the minimal surface BoardEventPublisher needs
// (satisfied by *logger.Logger — app wiring — and any test double).
type warnLogger interface {
	Warn(msg string, keysAndValues ...interface{})
}

// boardSSEEventType is the user-stream type the merged frontend pins
// (useUserEventStream's board branch; BoardPage.sse tests).
const boardSSEEventType = "board.event"

// boardSSEEvent renders the committed board_events row as the flat SSE
// envelope the frontend contract defines (project_id, id, item_kind,
// item_id, action, actor, payload, created_at — siblings of `type`, not
// nested under `data`).
func boardSSEEvent(evt board.Event) apitypes.WorkspaceSSEEvent {
	at := evt.CreatedAt
	return apitypes.WorkspaceSSEEvent{
		Type:         boardSSEEventType,
		ProjectID:    evt.ProjectID,
		BoardEventID: evt.ID,
		ItemKind:     evt.ItemKind,
		ItemID:       evt.ItemID,
		Action:       evt.Action,
		BoardActor:   evt.Actor,
		BoardPayload: evt.Payload,
		CreatedAt:    &at,
	}
}

// publishBoardFanout delivers one rendered event to BOTH channels: the
// project's target workspace stream and the project owner's user stream
// (the publishWorkspaceAndUserEvent shape, board-scoped — the owner is
// the project's user_id, ruling R7, not the workspace owner).
func (h *ProxyHandler) publishBoardFanout(owner, workspaceID string, evt board.Event) {
	if h.userBroker == nil {
		return
	}
	if workspaceID != "" {
		h.userBroker.PublishToWorkspace(workspaceID, boardSSEEvent(evt))
	}
	if owner == "" {
		return
	}
	userCopy := boardSSEEvent(evt)
	userCopy.WorkspaceID = workspaceID
	h.userBroker.PublishToUser(owner, userCopy)
}

// BoardEventPublisher returns the §4.6 EventPublisher wired to the user
// event broker. The projection runs OFF the write path: publishCommitted
// (the writer's goroutine, post-commit) hands the event to a bounded
// channel drained by one goroutine — the project lookup (indexed PK
// read) and the broker fan-out never block the committing transaction's
// caller, and per-project ordering is preserved (single drainer). A
// full channel drops the event (log-only — the reseed belt covers).
// nil when no broker is mounted (engine dark — writes unaffected).
func (h *ProxyHandler) BoardEventPublisher(pool *pgxpool.Pool, log warnLogger) board.EventPublisher {
	if h.userBroker == nil {
		return nil
	}
	events := make(chan board.Event, boardPublishQueue)
	go func() {
		for evt := range events {
			h.boardEventDeliver(pool, log, evt)
		}
	}()
	return h.boardEventEnqueuer(events, log)
}

// boardEventEnqueuer is the write-path half: a non-blocking hand-off
// that drops (log-only) when the drainer is behind — the committing
// caller never waits on the projection.
func (h *ProxyHandler) boardEventEnqueuer(events chan<- board.Event, log warnLogger) board.EventPublisher {
	return func(evt board.Event) {
		select {
		case events <- evt:
		default:
			log.Warn("board: SSE projection queue full — event dropped (reseed belt covers)",
				"projectID", evt.ProjectID, "eventID", evt.ID)
		}
	}
}

// boardPublishQueue bounds the projection's hand-off channel (the write
// path never blocks on the projection; overflow drops with a log).
const boardPublishQueue = 256

func (h *ProxyHandler) boardEventDeliver(pool *pgxpool.Pool, log warnLogger, evt board.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var owner, workspaceID string
	err := pool.QueryRow(ctx,
		`SELECT user_id, target_workspace_id::text FROM projects WHERE id=$1`,
		evt.ProjectID).Scan(&owner, &workspaceID)
	cancel()
	if err != nil {
		log.Warn("board: SSE projection skipped — project lookup failed (reseed belt covers)",
			"projectID", evt.ProjectID, "error", err.Error())
		return
	}
	// #1922: the actor's session id AND the payload's embedded task
	// snapshots (delete/cascade snapshotOf rows carry assignee_session)
	// are phrase-translated at EMISSION (the #1817/#1821 SSE-precedent
	// edge class — the frame is user/agent-readable even though the
	// frontend renders neither leg today). The stored board_events row
	// is never rewritten; the degraded shapes are markDeprecatedSession-
	// ID's (raw + session_id_deprecated) and its task-field twin
	// (raw + assignee_session_deprecated).
	evt.Actor, evt.Payload = h.translateBoardEventIDs(pool, workspaceID, evt.Actor, evt.Payload)
	h.publishBoardFanout(owner, workspaceID, evt)
}

// translateBoardEventIDs is the SSE leg's bounded consult: parse the
// actor blob and the payload's assignee keys, resolve them against the
// project's target workspace in ONE session_index consult, translate
// both. Failures degrade raw (the translators never break a read).
func (h *ProxyHandler) translateBoardEventIDs(pool *pgxpool.Pool, workspaceID string, actor, payload []byte) ([]byte, []byte) {
	if workspaceID == "" {
		return actor, payload
	}
	ids := make([]string, 0, 2)
	if len(actor) > 0 {
		var m map[string]any
		if err := json.Unmarshal(actor, &m); err == nil && m != nil {
			if v, _ := m["session_id"].(string); v != "" {
				ids = append(ids, v)
			}
		}
	}
	ids = append(ids, board.PayloadAssigneeIDs(payload)...)
	if len(ids) == 0 {
		return actor, payload
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	phrases := board.LookupSessionPhrases(ctx, pool, workspaceID, ids)
	return board.TranslateActorSessionPhrase(actor, phrases),
		board.TranslatePayloadAssignees(payload, phrases)
}
