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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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
// workspace's stream. One shared redis client; one RedisBus per
// workspace (the stream key is workspace-scoped by design), wired
// lazily and evicted on failure so the next event re-proves the
// transport. The nil pointer is the OFF posture — every method is
// nil-receiver-safe (the wiring hands the sink to the engine
// unconditionally; unarmed means inert, never panicked).
type boardLeaseEventbus struct {
	client *redis.Client
	scope  boardScopeResolver

	mu    sync.Mutex
	buses map[string]*eventbus.RedisBus

	// breakerNanos is the fail-fast gate (unix nanos; 0 = closed): a
	// publish failure opens it for boardLeaseBreakerPause.
	breakerNanos atomic.Int64
}

// newBoardLeaseEventbus wires the producer. A nil return (addr empty
// or Redis unreachable at boot — logged) is the OFF posture: the v1
// direct leg verbatim, matching agentd's eventbusFromEnv contract (a
// bus that cannot be trusted at boot is a bus the ladder must not
// depend on).
func newBoardLeaseEventbus(addr, password string, scope boardScopeResolver) *boardLeaseEventbus {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	opts, err := leaseEventbusOptions(addr, password)
	if err != nil {
		slog.Warn("board: lease event bus address malformed — the nudge ladder stays on the v1 direct leg", "err", err.Error())
		return nil
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), boardLeasePublishBudget)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("board: lease event bus unreachable — the nudge ladder stays on the v1 direct leg", "err", err.Error())
		_ = client.Close()
		return nil
	}
	slog.Info("board: lease event bus wired — the nudge ladder's decisions publish onto the workspace streams (design 0072 S4)")
	return &boardLeaseEventbus{client: client, scope: scope, buses: map[string]*eventbus.RedisBus{}}
}

// leaseEventbusOptions mirrors agentd's redisOptionsFromAddr leniency:
// one knob, two spellings (redis:// URL or bare host:port). The URL's
// own credentials win over the password env (the secretKeyRef spelling
// is for credentialess addresses) — the same deterministic rule the
// agentd edge applies.
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

// breakerOpen reports the fail-fast state, stamping the read (a probe
// counts as an attempt only on failure — see publish).
func (b *boardLeaseEventbus) breakerOpen() bool {
	return time.Now().UnixNano() < b.breakerNanos.Load()
}

// busFor returns the workspace's bus, wiring it on first use. The
// constructor pings, so a workspace whose bus was evicted after a
// failure re-proves the transport here — one budget, then the breaker
// owns the pause.
func (b *boardLeaseEventbus) busFor(ctx context.Context, workspaceID string) (*eventbus.RedisBus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bus, ok := b.buses[workspaceID]; ok {
		return bus, nil
	}
	wctx, cancel := context.WithTimeout(ctx, boardLeasePublishBudget)
	defer cancel()
	bus, err := eventbus.NewRedisBus(wctx, b.client, eventbus.StreamKey(workspaceID), 0)
	if err != nil {
		return nil, fmt.Errorf("board: lease event bus for workspace %s: %w", workspaceID, err)
	}
	b.buses[workspaceID] = bus
	return bus, nil
}

// evictBus drops a workspace's bus after a publish failure (the next
// event rewires; a stream that went away heals).
func (b *boardLeaseEventbus) evictBus(workspaceID string) {
	b.mu.Lock()
	delete(b.buses, workspaceID)
	b.mu.Unlock()
}

// publish appends one event to the workspace's stream: bounded,
// counted, fail-open. The breaker makes a downed Redis cost at most
// ONE budget per pause window; every later call fails fast (the nudge
// path falls straight back to v1).
func (b *boardLeaseEventbus) publish(ctx context.Context, workspaceID, topic string, payload map[string]string) error {
	if b == nil || b.client == nil {
		return errors.New("board: lease event bus unwired")
	}
	if b.breakerOpen() {
		boardLeaseEventsFailed.Add(1)
		return errors.New("board: lease event bus paused after a failed publish")
	}
	bus, err := b.busFor(ctx, workspaceID)
	if err != nil {
		b.trip()
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, boardLeasePublishBudget)
	defer cancel()
	if _, err := bus.Publish(pctx, eventbus.Event{Topic: topic, At: time.Now(), Payload: payload}); err != nil {
		b.trip()
		b.evictBus(workspaceID)
		return err
	}
	boardLeaseEventsPublished.Add(1)
	return nil
}

// trip opens the breaker for one pause window.
func (b *boardLeaseEventbus) trip() {
	boardLeaseEventsFailed.Add(1)
	b.breakerNanos.Store(time.Now().Add(boardLeaseBreakerPause).UnixNano())
	slog.Warn("board: lease event publish failed — bus paused one sweep window (v1 semantics unaffected)",
		"retry_in", boardLeaseBreakerPause.String())
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
