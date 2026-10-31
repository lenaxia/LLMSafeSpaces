// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workspace_eventbus.go — the workspace event bus wiring (design
// 0072): Redis Streams transport over one stream per workspace
// (workspace:{id}:events), the producer leg riding the notifier's
// settle fold, and the consumer groups this pod runs.
//
// Topology today:
//
//	frame stream → notifier settle fold ──publish──→ workspace stream
//	                     ▲                                 │
//	                     │              ┌──────────────────┤
//	                     │              ▼                  ▼
//	                     │       notifier group        lease group
//	                     │       (session.*) —        (lease.*) — the
//	                     │       per-subscriber       NudgeSender
//	                     │       SessionSend          generalization:
//	                     │       (v1 semantics,      lease.{task}.nudged
//	                     │       bus transport)      → sentinel SessionSend
//	                     │                                  ▲
//	                     └── the API's nudge ladder publishes (S4,
//	                         design 0072 §9.3 — LANDED): nudged is the
//	                         sentinel's DELIVERY (this group sends it;
//	                         the API's v1 direct send is the fallback),
//	                         and renewed/wedge_verdict/reclaimed are
//	                         the ladder's own decisions logged here —
//	                         the observation plane's visibility).
//
// Rollout is additive and env-gated: AGENTD_EVENTBUS_ADDR unset (the
// default) keeps subscriptions v1 point-to-point verbatim — the bus
// only switches the delivery layer when a Redis is actually
// configured. The API-side consumers (audit → board_events, SSE → UI
// live updates) join later as their own groups on the same stream —
// the fan-out is the point (design 0072 §3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
	"go.uber.org/zap"
)

// eventbusConsumerName mints this process's consumer identity: the
// pod hostname (stable across in-pod restarts — same-name resume
// drains its own pending without waiting out MinIdle; a pod change
// orphans the old name's entries and the reclaim sweep picks them
// up). AGENTD_EVENTBUS_CONSUMER overrides for multi-instance pods.
func eventbusConsumerName(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("AGENTD_EVENTBUS_CONSUMER")); v != "" {
		return v
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "agentd"
	}
	return host
}

// redisOptionsFromAddr accepts a redis://[/rediss://] URL or a bare
// host:port (the same leniency the API's config applies to its Redis
// addr — one knob, two spellings).
func redisOptionsFromAddr(raw string) (*redis.Options, error) {
	if strings.Contains(raw, "://") {
		return redis.ParseURL(raw)
	}
	return &redis.Options{Addr: raw}, nil
}

// urlFreeReason renders a malformed-URL error WITHOUT the URL: a
// *url.Error's Error() embeds the raw address verbatim (parse
// "redis://u:p@host:port": …) — on the malformed branch the redacted
// addr field is pointless if the error field then leaks the password
// (review r2, Finding 2). The wrapped inner reason is logged instead;
// anything not a *url.Error degrades to a generic classification.
func urlFreeReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return "malformed redis address"
}

// redactRedisAddr strips userinfo from a raw addr before it touches
// a log line: credentialed URLs (redis://u:p@host:port/db — a
// supported spelling) must never leak their password, and the warns
// fire precisely during incidents, when logs get read. Non-URL
// spellings (host:port) pass through unchanged. The split is at the
// LAST '@' (net/url's own userinfo boundary — a password may itself
// contain '@', and the first-'@' split would leak its tail).
func redactRedisAddr(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.LastIndexByte(rest, '@'); j >= 0 {
			return raw[:i+3] + "***@" + rest[j+1:]
		}
	}
	return raw
}

// eventbusFromEnv wires the transport when AGENTD_EVENTBUS_ADDR
// configures one (a redis:// URL or host:port) and WORKSPACE_ID
// scopes the stream key. Every failure mode returns nil — nil is the
// honest "bus disabled" state and the notifier's v1 leg keeps
// wake-ups flowing (a bus that cannot be trusted is a bus the
// delivery layer must not depend on).
func eventbusFromEnv(getenv func(string) string, logger *zap.Logger) *eventbus.RedisBus {
	if logger == nil {
		logger = zap.NewNop()
	}
	raw := strings.TrimSpace(getenv("AGENTD_EVENTBUS_ADDR"))
	if raw == "" {
		return nil // not configured: subscriptions stay point-to-point
	}
	workspace := strings.TrimSpace(getenv("WORKSPACE_ID"))
	if workspace == "" {
		logger.Warn("event bus: AGENTD_EVENTBUS_ADDR set but WORKSPACE_ID empty — bus disabled (the stream key is workspace-scoped by design)")
		return nil
	}
	opts, err := redisOptionsFromAddr(raw)
	if err != nil {
		logger.Warn("event bus: malformed AGENTD_EVENTBUS_ADDR — bus disabled",
			zap.String("addr", redactRedisAddr(raw)), zap.String("reason", urlFreeReason(err)))
		return nil
	}
	// AGENTD_EVENTBUS_PASSWORD (the secretKeyRef spelling — the
	// controller injects it from the platform's credentials secret so
	// the address itself stays a plaintext host:port in the pod spec):
	// applied only when the parsed options carry NO credentials of
	// their own — a redis://user:pass@… URL keeps what it embedded
	// (the env is for the host:port form; both-set is a config smell
	// — the URL wins, deterministically, and NOW loudly: the warn is
	// the operator's signal to drop one of the two).
	if pw := strings.TrimSpace(getenv("AGENTD_EVENTBUS_PASSWORD")); pw != "" {
		if opts.Password != "" {
			logger.Warn("AGENTD_EVENTBUS_PASSWORD set alongside a credentialed AGENTD_EVENTBUS_ADDR URL — the URL's own credentials win; drop one (the env is the secretKeyRef spelling for credentialess host:port addresses)")
		} else {
			opts.Password = pw
		}
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus, err := eventbus.NewRedisBus(ctx, client, eventbus.StreamKey(workspace), 0)
	if err != nil {
		logger.Warn("event bus: redis unreachable — bus disabled, subscriptions stay point-to-point",
			zap.String("addr", opts.Addr), zap.Error(err))
		_ = client.Close()
		return nil
	}
	// S3: the publish counter seam — every Publish outcome (success,
	// validation, transport) ticks the workspace's publishes_total.
	bus.SetPublishObserver(eventbusPublishObserver(workspace))
	logger.Info("event bus: transport wired", zap.String("stream", bus.Key()), zap.String("addr", opts.Addr))
	return bus
}

// runEventbusConsumers starts the consumer groups this pod runs on
// the workspace stream (design 0072 §3): the notifier group
// (subscription deliveries — the settle events the producer leg
// published) and the lease group (nudge sentinel deliveries — the
// NudgeSender generalization). Both join on wg for clean shutdown.
func runEventbusConsumers(ctx context.Context, wg *sync.WaitGroup, bus *eventbus.RedisBus, n *sessionNotifier, password string, logger *zap.Logger) {
	if bus == nil || n == nil {
		return
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	consumer := eventbusConsumerName(os.Getenv)
	seam := func() *opencode.Client { return seamClientWithPassword(password) }
	// S3: the consume counter seam — every dispatch outcome (group,
	// outcome, redelivery) ticks the workspace's deliveries_total.
	ws := workspaceIDFromEnv()
	if ws == "" {
		ws = "unknown"
	}
	onDelivery := eventbusDeliveryObserver(ws)

	// notifier: session.{id}.{idle,error} → per-subscriber SessionSend
	// (suppression §4.4, rate bound, compose — all unchanged; only the
	// transport differs from v1). New groups start LIVE: a notifier
	// woken by retained history would re-notify settled episodes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		opts := eventbus.ConsumeOptions{
			Group:      eventbus.GroupNotifier,
			Consumer:   consumer + "-notifier",
			Topics:     []string{"session.*.*"},
			OnDelivery: onDelivery,
			OnError: func(m eventbus.Message, err error) {
				logger.Warn("event bus: subscription delivery failed — pending, will redeliver",
					zap.String("topic", m.Event.Topic), zap.Error(err))
			},
		}
		if err := bus.Consume(ctx, opts, n.ConsumeSettleEvent); err != nil {
			logger.Warn("event bus: notifier consumer exited", zap.Error(err))
		}
	}()

	// lease: lease.{task_id}.* → the nudge sentinel's SessionSend.
	wg.Add(1)
	go func() {
		defer wg.Done()
		lc := &leaseBusConsumer{seam: seam, logger: logger}
		opts := eventbus.ConsumeOptions{
			Group:      eventbus.GroupLease,
			Consumer:   consumer + "-lease",
			Topics:     []string{"lease.*.*"},
			OnDelivery: onDelivery,
			OnError: func(m eventbus.Message, err error) {
				logger.Warn("event bus: lease delivery failed — pending, will redeliver",
					zap.String("topic", m.Event.Topic), zap.Error(err))
			},
		}
		if err := bus.Consume(ctx, opts, lc.handle); err != nil {
			logger.Warn("event bus: lease consumer exited", zap.Error(err))
		}
	}()

	// S3: the health collector — lag/pending/stalled gauges and the
	// edge-triggered stall log lines (both topologies get it here,
	// alongside the groups they own).
	runEventbusHealthLoop(ctx, wg, bus, logger)
}

// ---- the lease consumer (the NudgeSender generalization) ----

// leaseNudgeSentinelFields is the agentmessage-v1 additive payload
// the nudge sentinel carries — the SAME grammar the API's nudge
// ladder composes (api/internal/app/board_liveness.go's
// leaseNudgeSentinel): unknown keys degrade to plain text in
// grammar-ignorant parsers, which is exactly the prose fallback a
// wedged worker needs. One source of truth is impossible across the
// API/agentd split (pkg/board drags pgx); the test suite pins
// byte-for-byte parity instead.
type leaseNudgeSentinelFields struct {
	Mode    string `json:"mode"`
	TaskID  string `json:"taskId"`
	NudgeID string `json:"nudgeId"`
	Expects string `json:"expects"`
}

// leaseNudgeSentinelMode is the sentinel's mode value.
const leaseNudgeSentinelMode = "lease-nudge"

// composeLeaseNudge renders the nudge message: the v1 marker line
// (the ordinal anchor board_heartbeat's response discipline keys on)
// followed by the surgical directive prose — RESPOND first, then
// continue/advance/fail, never restart (owner ruling 2026-10-28).
func composeLeaseNudge(taskShortID, nudgeID string) string {
	payload, _ := json.Marshal(leaseNudgeSentinelFields{
		Mode:    leaseNudgeSentinelMode,
		TaskID:  taskShortID,
		NudgeID: nudgeID,
		Expects: "respond|advance|fail",
	})
	var b strings.Builder
	b.WriteString("<!-- lsp:agent-message-v1 ")
	b.Write(payload)
	b.WriteString(" -->\n")
	fmt.Fprintf(&b, "BOARD LEASE NUDGE: your claim on task %s has gone quiet. "+
		"First RESPOND (call board_heartbeat once, with your claimToken) to confirm you are active — "+
		"respond before running any long command so the wedge clock clears. "+
		"Then: continue working if there is more work to do; otherwise move your task to its next stage "+
		"(board_move_stage with your claimToken), or declare it failed with a reason if you cannot complete it. "+
		"Do NOT restart — assess your current state.", taskShortID)
	return b.String()
}

// leaseBusConsumer translates lease.* topics into SessionSend
// deliveries. nudged → the sentinel nudge above (at-least-once on the
// group: a redelivered nudge is a duplicate sentinel — benign, the
// ladder's ordinal anchor is latest-only); wedge_verdict / renewed /
// reclaimed → log only (the S4 observation plane: the verdict reclaims
// the row at the engine — messaging a wedged worker is noise, the NEXT
// claimant gets the task; a renewed worker is alive and working; a
// reclaimed row's assignee is gone. The LOUD WARN lines ARE the
// product — the 2026-10-30 incident's reclaims were invisible until
// transcript forensics).
type leaseBusConsumer struct {
	seam   func() *opencode.Client
	logger *zap.Logger
}

func (c *leaseBusConsumer) handle(ctx context.Context, msg eventbus.Message) error {
	taskID, arm, ok := eventbus.ParseLease(msg.Event.Topic)
	if !ok {
		return nil // filtered upstream; ack
	}
	switch arm {
	case "nudged":
		assignee := msg.Event.Payload["assignee"]
		if assignee == "" {
			c.logger.Warn("event bus: lease nudge without an assignee — acking unread", zap.String("task", taskID))
			return nil
		}
		shortID := msg.Event.Payload["task_short_id"]
		if shortID == "" {
			shortID = taskID
		}
		// The body IS the sentinel (marker line + directive prose) —
		// the same single-string shape the API's nudger delivers; no
		// agentmessage.Compose wrapper (that grammar stamps a
		// fromSession provenance this system-originated sentinel
		// deliberately does not carry).
		body := composeLeaseNudge(shortID, msg.Event.Payload["nudge_id"])
		// Detached (WithoutCancel): the sentinel's delivery must
		// outlive the consumer round — its ack is what the pending
		// machinery redelivers, and a kill mid-POST would only
		// duplicate a benign sentinel.
		if _, err := c.seam().SessionSend(context.WithoutCancel(ctx), assignee, body, "", nil); err != nil {
			return fmt.Errorf("lease nudge POST /session/%s/message: %w", assignee, err)
		}
		c.logger.Warn("event bus: lease nudge delivered",
			zap.String("task", shortID),
			zap.String("assignee", assignee),
			zap.String("nudge_id", msg.Event.Payload["nudge_id"]))
		return nil
	case "wedge_verdict":
		c.logger.Warn("event bus: lease wedge verdict",
			zap.String("task", taskID),
			zap.String("assignee", msg.Event.Payload["assignee"]),
			zap.String("verdict", msg.Event.Payload["verdict"]),
			zap.String("evidence", msg.Event.Payload["evidence"]))
		return nil
	case "renewed":
		// The ladder answered ALIVE — which rung is the operator's
		// "why" (heartbeat_response | movement | post_nudge_reaction |
		// busy_gate).
		c.logger.Info("event bus: lease renewed",
			zap.String("task", taskID),
			zap.String("assignee", msg.Event.Payload["assignee"]),
			zap.String("source", msg.Event.Payload["source"]))
		return nil
	case "reclaimed":
		// The 2026-10-30 incident's invisible transition — now a WARN
		// on the pod the claim lived in: which claim lapsed, why, and
		// the failure count it landed on.
		c.logger.Warn("event bus: lease reclaimed",
			zap.String("task", taskID),
			zap.String("assignee", msg.Event.Payload["assignee"]),
			zap.String("reason", msg.Event.Payload["reason"]),
			zap.String("failure_count", msg.Event.Payload["failure_count"]))
		return nil
	default:
		return nil // vocabulary is a convention — unknown arms ack unread
	}
}
