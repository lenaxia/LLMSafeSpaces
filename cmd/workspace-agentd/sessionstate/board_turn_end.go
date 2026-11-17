// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"go.uber.org/zap"
)

// board_turn_end.go — #2061/#2064 story 1: the board turn-end
// publication. ONE emission site, structurally enforced: the
// harness-attested event application path (applyHarnessEventLocked —
// reached only from Ingest and its test shim IngestForTest, where
// provenance is structural: the payload arrived from the harness
// transport, through the injected parser, on BOTH in-tree harnesses —
// the opencode SSE translator and the pi driver feed this same
// boundary). The reconcile/settle/reseed paths (clearBusyFromEvidence,
// rederiveStatusLocked, the reseed flush, the declared-timeout fold)
// apply events through plain applyLocked and can therefore NEVER
// publish a due-mark-eligible event — the anti-#2040 emission-site
// rule (audit ruling 2026-11-15), implemented as a call-site rule, not
// a provenance framework.
//
// Zero liveness authority (#2042): the event is a NEW type
// (EVENT_TYPE_BOARD_TURN_END, never a SESSION_STATUS overload) and
// lands the opaque evidence class API-side (workerActivityStampsEvidence
// does not name it — pinned in api/internal/app) — it renews nothing,
// stamps no worker index; its ONLY consumer signal is the board's
// due-marking (story #2065).

// applyHarnessEventLocked applies one event at the HARNESS boundary —
// the sole emission-eligible application path. It is the recover wall's
// downstream twin of Ingest/IngestForTest: same lock hold (a.mu), same
// fold, plus the turn-transition observation that may publish the
// board turn-end event AFTER the harness event itself is fully applied
// and fanned out (the publication is additive; a probe failure or a
// cursor-persist drop never disturbs the harness event).
//
// The transition observed is rec.busy true→false on the event's
// session: the harness's own settle (SESSION_STATUS IDLE/ERROR, or an
// ERROR event clearing the turn). A mid-turn duplicate settle (idle
// while already idle) is not a transition and emits nothing (epic R4's
// mid-turn guard); the delivery FSM's own completion observation is
// unaffected — this publication is additive telemetry, never a
// lifecycle input.
func (a *Authority) applyHarnessEventLocked(evt *abiv1.Event) {
	sid := evt.GetSessionId()
	if sid == "" && evt.GetSession() != nil {
		sid = evt.GetSession().GetId()
	}
	wasBusy := false
	turnStart := time.Time{}
	if sid != "" {
		if rec := a.sessions[sid]; rec != nil {
			wasBusy = rec.busy
			turnStart = rec.busySince
		}
	}
	a.liftFoldDeadLocked(evt)
	a.applyLocked(evt)
	if sid == "" || !wasBusy {
		return
	}
	if rec := a.sessions[sid]; rec == nil || rec.busy {
		return // no busy→idle transition on this event
	}
	a.emitBoardTurnEndLocked(sid, turnStart)
}

// emitBoardTurnEndLocked consults the board seam and, for a
// claim-holder, publishes EVENT_TYPE_BOARD_TURN_END with the seam's
// advance verdict plus the authority's turn facts. Best-effort by
// contract (R4): the probe consult runs behind a recover wall — a
// panicking seam is contained and counted, the harness fold above is
// already complete; a cursor-persist failure inside applyLocked drops
// the publication (counted as such — the metric reads published turns,
// not attempted ones). a.mu must be held.
func (a *Authority) emitBoardTurnEndLocked(sid string, turnStart time.Time) {
	probe := a.cfg.BoardTurnEndProbe
	if probe == nil {
		return // feature unwired / degraded pod — sweep semantics (epic R5)
	}
	var facts BoardTurnEndFacts
	func() {
		defer func() {
			if r := recover(); r != nil {
				a.boardTurnEndProbeFailures++
				a.logger.Error("sessionstate: board turn-end probe panic contained — no publication",
					zap.String("session", sid), zap.Any("panic", r))
			}
		}()
		facts = probe(sid, turnStart)
	}()
	if !facts.ClaimHolder {
		return // claim-holder sessions only — user chats never emit (R1)
	}
	if a.boardTurnEpochs == nil {
		a.boardTurnEpochs = map[string]uint64{}
	}
	a.boardTurnEpochs[sid]++
	duration := int64(0) //nolint:gosec // G115: turn durations are pod-lifetime bounded
	if !turnStart.IsZero() {
		duration = time.Since(turnStart).Milliseconds()
	}
	seqBefore := a.seq
	a.applyLocked(&abiv1.Event{
		SessionId: sid,
		Type:      abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		BoardTurnEnd: &abiv1.BoardTurnEnd{
			Advanced:       facts.Advanced,
			Epoch:          a.boardTurnEpochs[sid],
			TurnDurationMs: duration,
		},
	})
	if a.seq > seqBefore {
		a.boardTurnEndsPublished++
	}
}
