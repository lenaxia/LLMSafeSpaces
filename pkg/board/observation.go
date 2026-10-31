// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// observation.go — the observation plane's DURABLE audit trail (S7,
// the 2026-10-30 incident remediation: audit visibility).
//
// The incident's finding: every observation-plane action left no
// trail. Reconstructing what the platform DID — were nudges sent? did
// lapse verdicts fire? who reclaimed? — required transcript forensics
// across five sessions, and the orchestrator (no fault of its own)
// asserted a FALSE "heartbeat-extended" message because the truth was
// unobservable. S4 (lease_events.go) put the ladder's decisions on the
// workspace event bus — ephemeral, consumer-scoped, gone with the
// stream. This file is the durable answer: each observation-plane
// action lands ONE row in task_observation_events (migration 000055),
// and the recent window is ONE READ away for every orchestrator
// (Health.ObservationEvents — board_health on every surface).
//
// The six kinds (the charter's vocabulary, one per action):
//
//	stamped        — the assignee stamp landed (detail: origin spawn|
//	                 backfill, lag_seconds — S1's stamp_lag, now a
//	                 per-task fact instead of only a histogram bucket)
//	nudged         — a sentinel was DELIVERED and recorded (detail:
//	                 nudge_id, rung first_sentinel|re_anchor, session)
//	renewed        — the ladder answered alive (detail: source — the
//	                 rung: heartbeat_response|movement|
//	                 post_nudge_reaction|busy_gate)
//	wedge_verdict  — the machine lapse verdict (detail: evidence,
//	                 nudge_id, failure_count)
//	reclaimed      — the claim lapsed and the row re-queued (detail:
//	                 reason, failure_count)
//	deposed        — S5's manager deposition (detail: the salvage
//	                 pointer, from_stage, requeued, deposed session)
//
// Relation to the two existing planes (deliberate, not duplication):
//
//   - board_events is the IDEMPOTENCY LEDGER (requestId-keyed,
//     replay-semantic, SSE-projected, effectively unprunable). Its
//     reconciler rows (nudge/wedge/reclaim/depose actions) stay — the
//     permanent summary. Renewals were deliberately kept OUT of it
//     ("renewals never flood the trail" — they would flood SSE too).
//   - the event bus (S4/S6) is the EPHEMERAL plane: live consumers
//     (agentd's lease group, the SSE join) see decisions as they
//     happen; nothing is retained.
//   - THIS table is the RETAINED working window: recent rows online
//     (the health read), older truth summarized by the board_events
//     ledger that already carries the stage transitions.
//
// RETENTION BOUNDARY (documented here, enforced by pruneObservationTrail,
// surfaced on every health read as observation_retention):
//
//   - per task: the most recent ObservationKeepPerTask rows stay;
//     older rows are pruned (a chatty claim cannot grow the trail
//     unbounded — renewals land every sweep while a claim is active).
//   - global TTL: rows older than ObservationTTL are pruned regardless
//     of count (quiet tasks' trails age out; the permanent record is
//     board_events).
//   - cadence: the reconciler's sweep runs the prune at most once per
//     ObservationPruneInterval (wall-clock gate — the prune is bounded
//     and idempotent; a missed pass converges on the next).
//
// Every write is POST-COMMIT and FAIL-OPEN (the S4 sink contract): the
// trail is observability — a write failure drops ONE row, logs a warn,
// counts a metric, and changes no board state. The engine's decisions,
// the ladder's semantics, and the callers' outcomes are untouched.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Observation kinds — one per observation-plane action. A convention,
// not an enum (the bus vocabulary's rule): the launch set is pinned by
// the integration tests and consumed by the health read.
const (
	// ObservationStamped: the assignee stamp landed (engine spawn
	// post-write or S1's claim-evidence back-fill).
	ObservationStamped = "stamped"
	// ObservationNudged: a lease-nudge sentinel was delivered and
	// recorded on the row (the app nudger's success, engine-known).
	ObservationNudged = "nudged"
	// ObservationRenewed: the ladder answered ALIVE at one of its
	// rungs and extended the lease.
	ObservationRenewed = "renewed"
	// ObservationLapseVerdict: the machine wedge verdict — the sentinel
	// was positioned in the session's event order and nothing followed
	// it (the same decision S4's bus calls lease wedge_verdict).
	ObservationLapseVerdict = "lapse_verdict"
	// ObservationReclaimed: the claim lapsed and the row was re-queued
	// (or escalated) by the sweep or the persona pass.
	ObservationReclaimed = "reclaimed"
	// ObservationDeposed: S5's manager deposition — the salvage pointer
	// recorded, the claim re-queued or the pointer taken alone.
	ObservationDeposed = "deposed"
)

// Nudge rungs (ObservationNudged's detail.rung): which sentinel the
// ladder was on when it sent. A re-anchor is NOT a failure — the
// ordinal question was unanswerable by THIS source (a replica that
// never saw the mark), and the fresh sentinel re-anchors it
// (latest-only makes the duplicate benign).
const (
	NudgeRungFirst    = "first_sentinel" // the claim's first sentinel this episode
	NudgeRungReAnchor = "re_anchor"      // a fresh sentinel to re-anchor an unmeasured ordinal question
)

// The retention boundary (see the file comment). Fixed constants, not
// knobs: the boundary is a platform convention (the health read
// self-documents it), and the permanent record lives in board_events.
const (
	// ObservationKeepPerTask caps the per-task online window.
	ObservationKeepPerTask = 50
	// ObservationTTL bounds every row's age.
	ObservationTTL = 14 * 24 * time.Hour
	// ObservationPruneInterval gates the reconciler's prune cadence.
	ObservationPruneInterval = time.Hour
	// ObservationWindow is the recent-event window the health read
	// exposes (newest first).
	ObservationWindow = 30
)

// Observation metric names (the llmsafespaces_ family, stamp.go's
// precedent: production code writes, tests read).
const (
	ObservationEventsMetricName   = "llmsafespaces_board_observation_events_total"
	ObservationEventsHelp         = "Observation-plane actions landed in the per-task audit trail, by kind (S7: stamped/nudged/renewed/lapse_verdict/reclaimed/deposed — the durable record behind the board_health recent window)."
	ObservationFailuresMetricName = "llmsafespaces_board_observation_write_failures_total"
	ObservationFailuresHelp       = "Failed observation-audit writes (S7: every failure drops exactly one trail row with a warn — fail-open, board state is never affected)."
	observationPrunedMetricName   = "llmsafespaces_board_observation_pruned_total"
	observationPrunedHelp         = "Rows pruned from the observation audit trail by the retention boundary (per-task cap + TTL; the permanent record stays in board_events)."
)

var (
	observationEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: ObservationEventsMetricName,
		Help: ObservationEventsHelp,
	}, []string{"kind"})

	observationFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: ObservationFailuresMetricName,
		Help: ObservationFailuresHelp,
	}, []string{"kind"})

	observationPruned = promauto.NewCounter(prometheus.CounterOpts{
		Name: observationPrunedMetricName,
		Help: observationPrunedHelp,
	})
)

// ObservationEventsMetric exposes the landed counter for tests and
// admin surfaces (the StampLag precedent).
func ObservationEventsMetric() *prometheus.CounterVec { return observationEvents }

// ObservationFailuresMetric exposes the failure counter for tests.
func ObservationFailuresMetric() *prometheus.CounterVec { return observationFailures }

// ObservationEvent is one observation-plane action, recorded post-
// commit. Detail is FLAT (strings/numbers — the bus vocabulary's
// rule); the read renders it verbatim, Go never queries it by key.
type ObservationEvent struct {
	Kind    string // the Observation* kinds above
	TaskID  string
	Session string // the action's subject session ("" when none)
	Detail  map[string]any
}

// observationFromLease maps one ladder decision (the S4 projection's
// shape) onto the trail's row — the two planes carry the SAME
// decisions, so the mapper is the single translation point.
func observationFromLease(ev LeaseEvent) ObservationEvent {
	detail := map[string]any{}
	switch ev.Kind {
	case LeaseEventRenewed:
		detail["source"] = ev.Source
	case LeaseEventWedged:
		detail["evidence"] = ev.Evidence
		detail["nudge_id"] = ev.NudgeID
		detail["failure_count"] = ev.FailureCount
	case LeaseEventReclaimed:
		detail["reason"] = ev.Reason
		detail["failure_count"] = ev.FailureCount
	}
	return ObservationEvent{Kind: leaseKindToObservation(ev.Kind), TaskID: ev.TaskID, Session: ev.Assignee, Detail: detail}
}

// leaseKindToObservation names the ladder's decision in the trail's
// vocabulary (wedge_verdict — the bus kind — is the charter's "lapse
// verdict"; one vocabulary per plane, this is the map).
func leaseKindToObservation(kind string) string {
	switch kind {
	case LeaseEventRenewed:
		return ObservationRenewed
	case LeaseEventWedged:
		return ObservationLapseVerdict
	case LeaseEventReclaimed:
		return ObservationReclaimed
	}
	return ""
}

// recordObservation lands one trail row. FAIL-OPEN by contract: a
// failure drops the row, logs a warn, counts the metric, and returns —
// never an error to the caller (the trail is observability; the action
// that caused the row already committed).
func (s *Service) recordObservation(ctx context.Context, ev ObservationEvent) {
	if ev.Kind == "" || ev.TaskID == "" {
		return
	}
	detail, err := json.Marshal(ev.Detail)
	if err != nil {
		observationFailures.WithLabelValues(ev.Kind).Inc()
		slog.WarnContext(ctx, "board: observation audit detail marshal failed — row dropped",
			"task_id", ev.TaskID, "kind", ev.Kind)
		return
	}
	if _, err := s.q().Exec(ctx, `
		INSERT INTO task_observation_events (task_id, kind, session, detail)
		VALUES ($1::uuid, $2, $3, $4)`,
		ev.TaskID, ev.Kind, ev.Session, detail); err != nil {
		observationFailures.WithLabelValues(ev.Kind).Inc()
		slog.WarnContext(ctx, "board: observation audit write failed — row dropped (board state unaffected)",
			"task_id", ev.TaskID, "kind", ev.Kind, "err", err.Error())
		return
	}
	observationEvents.WithLabelValues(ev.Kind).Inc()
}

// TaskObservationTrail reads one task's retained window, newest first
// (the per-task drill-down behind the health surface's workspace
// window; bounded by the retention boundary, never a full history —
// the permanent record is board_events).
func (s *Service) TaskObservationTrail(ctx context.Context, taskID string, limit int) ([]ObservationRow, error) {
	if limit <= 0 || limit > ObservationKeepPerTask {
		limit = ObservationKeepPerTask
	}
	rows, err := s.q().Query(ctx, `
		SELECT kind, coalesce(session, ''), detail, created_at
		FROM task_observation_events
		WHERE task_id = $1::uuid
		ORDER BY id DESC LIMIT `+strconv.Itoa(limit), taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ObservationRow{}
	for rows.Next() {
		var r ObservationRow
		var detail []byte
		if err := rows.Scan(&r.Kind, &r.Session, &detail, &r.At); err != nil {
			return nil, err
		}
		r.Detail = decodeObservationDetail(detail)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ObservationRow is one retained trail row on a read.
type ObservationRow struct {
	Kind    string         `json:"kind"`
	Session string         `json:"session,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
	At      time.Time      `json:"at"`
}

// decodeObservationDetail renders the JSONB verbatim; an unreadable
// blob degrades to nil (a trail row's detail is a convenience, never a
// promise — the kind and the timestamp are the facts).
func decodeObservationDetail(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// PruneObservationTrail enforces the retention boundary: the per-task
// cap and the global TTL. Bounded and idempotent (a concurrent pass
// converges to the same retained set); the reconciler's sweep calls it
// at most once per ObservationPruneInterval, and operators may call it
// directly after an incident burst. The per-task cap runs only over
// tasks with more rows than the boundary — the NOT IN arm keeps the
// newest ObservationKeepPerTask per task.
func (s *Service) PruneObservationTrail(ctx context.Context) {
	tag, err := s.q().Exec(ctx, `
		DELETE FROM task_observation_events
		WHERE created_at < now() - make_interval(secs=>$1::float8)
		   OR id IN (SELECT id FROM (
				SELECT id, row_number() OVER (PARTITION BY task_id ORDER BY id DESC) AS rn
				FROM task_observation_events
			) ranked WHERE ranked.rn > $2)`,
		ObservationTTL.Seconds(), ObservationKeepPerTask)
	if err != nil {
		slog.WarnContext(ctx, "board: observation trail prune failed (converges on the next pass)",
			"err", err.Error())
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		observationPruned.Add(float64(n))
		slog.InfoContext(ctx, "board: observation trail pruned past the retention boundary",
			"pruned", n, "keep_per_task", ObservationKeepPerTask, "ttl", ObservationTTL.String())
	}
}
