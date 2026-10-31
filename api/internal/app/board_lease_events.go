// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_lease_events.go — the API's FIRST eventbus producer (design
// 0072 S4): the nudge ladder's decisions — nudges sent, renewals (with
// their rung), wedge verdicts (with their evidence), reclaims —
// published onto the per-workspace stream (workspace:{id}:events)
// where agentd's lease consumer group already waits.
//
// The 2026-10-30 incident's finding: the ladder was WIRED but
// invisible — every decision left no event anywhere, and reconstructing
// what happened forced transcript forensics. This file closes that:
// each decision becomes a stream entry any consumer (and the S3 health
// surface: stream_entries, last_publish_timestamp, the lease group's
// lag/deliveries) can see.
//
// Posture (design 0072 §9.3, "preserve the v1 fallback"): the producer
// is OPT-IN and fail-open —
//
//   - addr unset (the default) ⇒ nil producer ⇒ nudges take the v1
//     direct send verbatim and the sink is inert: byte-identical
//     pre-S4 behavior;
//   - armed but a publish fails ⇒ the nudge falls back to the v1
//     direct send (the engine's phantom-nudge contract — never claim a
//     sentinel that never landed — keys on the return), and the
//     observability events are dropped with a warn;
//   - ARMING IS ONE POSTURE WITH THE AGENTD EDGE: the controller's
//     --workspace-eventbus-addr arms agentd's consumers; this knob
//     arms the API's publisher against the SAME Redis. Arm both or
//     neither — a published nudge on a stream whose workspace runs an
//     unarmed agentd has no consumer (the lost-wake-up shape S3's
//     gauges make visible: publishes rising, the lease group absent or
//     lagging).
//
// The renewal/wedge/reclaim sink is pure observability: a dropped
// event changes no board state (the row, the audit trail, and the
// ladder's semantics live in pkg/board; this is the projection).

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// boardLeasePublishBudget bounds one publish and one bus (re)wire:
// fail-open means a slow or dead Redis costs at most one budget before
// the breaker (below) gates the rest — a stalled sweep is never an
// acceptable observability bill.
const boardLeasePublishBudget = 2 * time.Second

// boardLeaseBreakerPause gates re-attempts after a publish failure:
// the sweep's own cadence (30s default) — one bounded failure per
// window, then fail-fast until the window turns. The nudge path's v1
// fallback answers immediately in that window (a dead bus must not add
// a 2s tax to every sentinel).
const boardLeaseBreakerPause = 30 * time.Second

// The API-side visibility (expvar on /debug/vars — the
// boardLivenessDegraded precedent; agentd's S3 prometheus surface
// observes the same stream from its own side). published counts
// XADDs that landed; failed counts every drop (unwired excluded — the
// unwired producer does not exist).
var (
	boardLeaseEventsPublished expvar.Int
	boardLeaseEventsFailed    expvar.Int
)

func init() {
	expvar.Publish("board_lease_events_published_total", &boardLeaseEventsPublished)
	expvar.Publish("board_lease_events_failed_total", &boardLeaseEventsFailed)
}

// boardLeaseEventbus publishes the ladder's events onto each target
// workspace's stream — S4's producer, riding the shared boardBusClient
// transport (S6 extracted the client/breaker/buses machinery both
// legs use; the posture and semantics are byte-identical). The nil
// pointer is the OFF posture — every method is nil-receiver-safe.
type boardLeaseEventbus struct {
	tr    *boardBusClient
	scope boardScopeResolver
}

// newBoardLeaseEventbus wires the producer. A nil return (addr empty
// or Redis unreachable at boot — logged) is the OFF posture: the v1
// direct leg verbatim, matching agentd's eventbusFromEnv contract (a
// bus that cannot be trusted at boot is a bus the ladder must not
// depend on).
func newBoardLeaseEventbus(addr, password string, scope boardScopeResolver) *boardLeaseEventbus {
	tr := newBoardBusClient(addr, password, "lease event")
	if tr == nil {
		return nil
	}
	slog.Info("board: lease event bus wired — the nudge ladder's decisions publish onto the workspace streams (design 0072 S4)")
	return &boardLeaseEventbus{tr: tr, scope: scope}
}

// publish delegates to the shared transport with the lease leg's
// counters (kept as a method: the nudger's bus-first leg and the S4
// tests key on it — including the nil-receiver OFF posture).
func (b *boardLeaseEventbus) publish(ctx context.Context, workspaceID, topic string, payload map[string]string) error {
	if b == nil {
		var tr *boardBusClient // nil-receiver-safe: reports "unwired"
		return tr.publish(ctx, workspaceID, topic, payload, &boardLeaseEventsPublished, &boardLeaseEventsFailed)
	}
	return b.tr.publish(ctx, workspaceID, topic, payload, &boardLeaseEventsPublished, &boardLeaseEventsFailed)
}

// leaseEventbusOptions mirrors agentd's redisOptionsFromAddr leniency:
// one knob, two spellings (redis:// URL or bare host:port). The URL's
// own credentials win over the password env (the secretKeyRef spelling
// is for credentialess addresses) — the same deterministic rule the
// agentd edge applies. Shared by every API-side board bus leg.
func leaseEventbusOptions(raw, password string) (*redis.Options, error) {
	var opts *redis.Options
	var err error
	if strings.Contains(raw, "://") {
		opts, err = redis.ParseURL(raw)
		if err != nil {
			// The raw address never reaches the error (a *url.Error
			// embeds it verbatim — the agentd redaction posture).
			return nil, errors.New("malformed redis address")
		}
	} else {
		opts = &redis.Options{Addr: raw}
	}
	if strings.TrimSpace(password) != "" && opts.Password == "" {
		opts.Password = password
	}
	return opts, nil
}

// LeaseEventSink projects the engine's ladder decisions (installed via
// board.WithLeaseEventSink). Pure observability: every failure drops
// the event with a warn and changes nothing.
func (b *boardLeaseEventbus) LeaseEventSink(ctx context.Context, ev board.LeaseEvent) {
	if b == nil || ev.TaskID == "" {
		return
	}
	_, workspaceID, err := b.scope(ctx, ev.TaskID)
	if err != nil {
		boardLeaseEventsFailed.Add(1)
		slog.WarnContext(ctx, "board: lease event scope unresolved — event dropped",
			"task", ev.TaskShortID, "kind", ev.Kind, "err", err.Error())
		return
	}
	var topic string
	var payload map[string]string
	switch ev.Kind {
	case board.LeaseEventRenewed:
		topic = eventbus.LeaseRenewedTopic(ev.TaskID)
		payload = eventbus.LeaseRenewPayload(ev.TaskID, ev.TaskShortID, ev.Assignee, ev.Source)
	case board.LeaseEventWedged:
		topic = eventbus.LeaseWedgeTopic(ev.TaskID)
		payload = eventbus.LeaseWedgePayload(ev.TaskID, ev.TaskShortID, ev.Assignee, ev.NudgeID, ev.Evidence)
	case board.LeaseEventReclaimed:
		topic = eventbus.LeaseReclaimedTopic(ev.TaskID)
		payload = eventbus.LeaseReclaimPayload(ev.TaskID, ev.TaskShortID, ev.Assignee, ev.Reason, ev.FailureCount)
	default:
		return // the vocabulary is a convention — unknown kinds are not published
	}
	if err := b.publish(ctx, workspaceID, topic, payload); err != nil {
		slog.WarnContext(ctx, "board: lease event publish failed — dropped (board semantics unaffected)",
			"task", ev.TaskShortID, "kind", ev.Kind, "err", err.Error())
	}
}

// wrapNudger was considered and rejected: the bus leg must sit INSIDE
// newBoardNudger (board_liveness.go), after markNudge — the sentinel's
// ordinal send-mark is recorded BEFORE any delivery path (a mark that
// precedes its sentinel can only err false-alive), and only the inner
// nudger holds the hub.
