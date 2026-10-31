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
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
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
	evt.Actor, evt.Payload = translateBoardEventIDs(ctx, pool, workspaceID, evt.Actor, evt.Payload)
	h.publishBoardFanout(owner, workspaceID, evt)
}

// translateBoardEventIDs is the SSE leg's bounded consult: parse the
// actor blob and the payload's assignee keys, resolve them against the
// project's target workspace in ONE session_index consult, translate
// both. Failures degrade raw (the translators never break a read).
func translateBoardEventIDs(ctx context.Context, pool *pgxpool.Pool, workspaceID string, actor, payload []byte) ([]byte, []byte) {
	if workspaceID == "" || pool == nil {
		// No workspace scope or no pool (the bus join's tests mount the
		// broker without one): degrade raw — the translators never
		// break a read.
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
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	phrases := board.LookupSessionPhrases(tctx, pool, workspaceID, ids)
	return board.TranslateActorSessionPhrase(actor, phrases),
		board.TranslatePayloadAssignees(payload, phrases)
}

// ---- The bus join (design 0072 S6) ----
//
// When the board eventbus is armed, the SSE projection is served from
// the per-workspace stream: the app-side publisher appends one
// board.{project}.event entry per COMMITTED board_events row (plus
// task.{id}.stamped entries for landed assignee stamps), and the
// API-side SSE consumer group renders those entries into the SAME
// board.event frames through the SAME translate+fan-out path as the
// v1 leg below. Parity is byte-level: one committed row renders to
// exactly the frame boardSSEEvent produces, whichever leg carried it.
//
// The consumer is deliberately drop-tolerant: render or fan-out
// failures log and ACK (never wedge the group on an advisory
// projection — the reseed belt is the consumer's safety net, the same
// posture as the v1 leg's overflow drop). The group's value here is
// cursor durability and per-replica fan-out, not redelivery.

// boardStampAction is the SSE action for landed assignee stamps. A
// stamp writes no board_events row (it is a guarded row write, not a
// caller action), so the frame synthesizes the row: the frontend's
// ["board"] invalidation refetches and the assignee appears without
// waiting for the 30s poll.
const boardStampAction = "assignee_stamped"

// boardFanoutBroker is the broker surface the bus join needs (the
// user event broker satisfies it; tests mount it directly).
type boardFanoutBroker interface {
	PublishToUser(userID string, evt apitypes.WorkspaceSSEEvent)
	PublishToWorkspace(workspaceID string, evt apitypes.WorkspaceSSEEvent)
}

// BoardEventBusHandler renders bus entries into board.event frames
// and fans them out. Scope (owner + workspace) rides the payload —
// resolved at PUBLISH time — so delivery performs NO project lookup
// (the retired per-delivery read of the v1 leg). Exported over the
// broker interface so the join is constructible end-to-end in tests
// (app-side publisher → stream → this handler → broker).
func BoardEventBusHandler(broker boardFanoutBroker, pool *pgxpool.Pool, log warnLogger) eventbus.Handler {
	return func(ctx context.Context, msg eventbus.Message) error {
		if broker == nil {
			// Always-ACK: an advisory projection never wedges the group.
			return nil
		}
		deliverBoardBusMessage(ctx, broker, pool, log, msg)
		return nil
	}
}

// BoardEventBusHandler mounts the join on this handler's broker (nil
// when no broker is mounted — engine dark, the join never starts).
func (h *ProxyHandler) BoardEventBusHandler(pool *pgxpool.Pool, log warnLogger) eventbus.Handler {
	if h.userBroker == nil {
		return nil
	}
	return BoardEventBusHandler(h.userBroker, pool, log)
}

// deliverBoardBusMessage is one entry's render+fan-out.
func deliverBoardBusMessage(ctx context.Context, broker boardFanoutBroker, pool *pgxpool.Pool, log warnLogger, msg eventbus.Message) {
	p := msg.Event.Payload
	if projectID, ok := eventbus.ParseBoardEvent(msg.Event.Topic); ok {
		createdAt, err := time.Parse(time.RFC3339Nano, p[eventbus.FieldBoardCreatedAt])
		if err != nil {
			log.Warn("board: SSE bus join — unparseable created_at, dropping entry (reseed belt covers)",
				"projectID", projectID, "eventID", p[eventbus.FieldBoardEventID])
			return
		}
		evt := board.Event{
			ID:        p[eventbus.FieldBoardEventID],
			ProjectID: projectID,
			ItemKind:  p[eventbus.FieldBoardItemKind],
			ItemID:    p[eventbus.FieldBoardItemID],
			Action:    p[eventbus.FieldBoardAction],
			Actor:     []byte(p[eventbus.FieldBoardActor]),
			Payload:   []byte(p[eventbus.FieldBoardPayload]),
			CreatedAt: createdAt,
		}
		fanOutBoardEvent(ctx, broker, pool, log, evt, p[eventbus.FieldBoardOwner], p[eventbus.FieldBoardWorkspace])
		return
	}
	if taskID, arm, ok := eventbus.ParseTask(msg.Event.Topic); ok && arm == "stamped" {
		lag, _ := strconv.ParseFloat(p[eventbus.FieldStampLagSeconds], 64)
		inner, err := json.Marshal(map[string]any{
			"task_short_id":     p[eventbus.FieldBoardShortID],
			"assignee_session":  p[eventbus.FieldStampAssignee],
			"assignee_origin":   p[eventbus.FieldStampOrigin],
			"stamp_lag_seconds": lag,
		})
		if err != nil {
			log.Warn("board: SSE bus join — stamp payload marshal failed, dropping entry", "task", taskID)
			return
		}
		actor, _ := json.Marshal(map[string]string{
			"kind": "system", "surface": "engine", "session_id": p[eventbus.FieldStampAssignee],
		})
		evt := board.Event{
			ID:        "stamp-" + msg.ID,
			ProjectID: p["project_id"],
			ItemKind:  "task",
			ItemID:    taskID,
			Action:    boardStampAction,
			Actor:     actor,
			Payload:   inner,
			CreatedAt: msg.Event.At,
		}
		fanOutBoardEvent(ctx, broker, pool, log, evt, p[eventbus.FieldBoardOwner], p[eventbus.FieldBoardWorkspace])
		return
	}
	// Unknown shape: the vocabulary is a convention — log and move on.
	log.Warn("board: SSE bus join — unknown topic shape, dropping entry", "topic", msg.Event.Topic)
}

// fanOutBoardEvent is the shared delivery tail: the SAME bounded
// phrase translation and dual-channel fan-out the v1 leg runs at
// emission, with the scope from the payload instead of a project
// lookup.
func fanOutBoardEvent(ctx context.Context, broker boardFanoutBroker, pool *pgxpool.Pool, log warnLogger, evt board.Event, owner, workspaceID string) {
	evt.Actor, evt.Payload = translateBoardEventIDs(ctx, pool, workspaceID, evt.Actor, evt.Payload)
	if workspaceID != "" {
		broker.PublishToWorkspace(workspaceID, boardSSEEvent(evt))
	}
	if owner == "" {
		return
	}
	userCopy := boardSSEEvent(evt)
	userCopy.WorkspaceID = workspaceID
	broker.PublishToUser(owner, userCopy)
}
