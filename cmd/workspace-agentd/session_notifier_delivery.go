// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_delivery.go — session idle-subscriptions v1, §4.5:
// notification compose + the delivery vehicle + the per-subscriber
// storm backstop.
//
// Compose stamps the agentmessage v1 provenance sentinel in its
// additive "notify" mode (the package contract is additive-only and
// forward-compatible: an unknown mode value parses as a string, so
// pre-notify parsers degrade to plain provenance — exactly the
// degraded path every other v1 addition rides).
//
// The vehicle is the same one send_message/create_session use: a
// detached V1 POST /session/{id}/message through Client.SessionSend,
// wrapped in context.WithoutCancel with NO artificial timeout on top
// — a busy subscriber queues server-side at its turn boundary and the
// queued delivery IS the wake-up (the seam client's own transport
// bound, shared with send_message, is the only cap; a context deadline
// here would kill exactly the legitimate queue-waits this feature
// exists to serve).
//
// The rate bound is the sessionLimiter pattern (token bucket keyed per
// subscriber): 60/hour, whole-budget burst — purely a storm backstop
// (§4.5: the exclusions and cycle refusal are the primary defenses).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/session/agentmessage"
	"go.uber.org/zap"
)

// notifySentinelMode is the v1 sentinel's additive mode value for
// subscription notifications (§4.5): "notify" joins "injected" and
// "self-declared" — same trust semantics (provenance metadata, never
// authentication), a different delivery origin (the subscription
// engine, not a session's own send).
const notifySentinelMode = "notify"

// errNotifyRateLimited is the storm-backstop refusal: a typed sentinel
// so deliverNotices can distinguish "dropped by the bound" from
// "delivery failed, keep the intent" — a rate-refused wake-up is
// dropped (its intent cleared), never re-enqueued: a boot retry would
// only deliver a stale wake-up after the storm the bound just stopped.
var errNotifyRateLimited = errors.New("notification rate limit refused")

// notifyWordingIdle / notifyWordingError are §4.5's two bodies (ruling
// 1: ERROR settles notify, with its own "the work may be incomplete"
// wording — the manager must read death differently from completion).
const (
	notifyWordingIdle  = "Subscription notification: session %s is now idle — its turn is complete. No reply required unless you want to act on it."
	notifyWordingError = "Subscription notification: session %s ended its turn in error — the work may be incomplete. No reply required unless you want to act on it."
)

// composeSettleNotification renders the delivered body: the v1 sentinel
// (mode "notify", fromSession = the broadcaster's PHRASE — the
// agent-facing address, harness id only in the cold-replica window,
// §7.8) followed by the event's wording. The title rides the session
// label parenthetically when known.
func composeSettleNotification(broadcasterPhrase, title, event string) (string, error) {
	label := broadcasterPhrase
	if title != "" {
		label = fmt.Sprintf("%s (%q)", broadcasterPhrase, title)
	}
	wording := notifyWordingIdle
	if event == subscriptionEventError {
		wording = notifyWordingError
	}
	return agentmessage.Compose(
		fmt.Sprintf(wording, label),
		agentmessage.Origin{FromSession: broadcasterPhrase, Mode: notifySentinelMode},
	)
}

// ---- per-subscriber token bucket (the sessionLimiter pattern) ----

// Notify-rate defaults (§4.5: "generous default (60/h)"): burst 60 =
// the full hour's budget spendable at once (advisory wake-ups; the
// bound exists so a runaway loop fan-out is finite, not so honest
// managers are throttled), refilled at one token per minute.
const (
	notifyRatePerHour = 60
	notifyRateBurst   = notifyRatePerHour
)

// notifyRateLimiter is a per-subscriber token bucket,
// sessionstate/service.go:207-259's sessionLimiter shape: lazy bucket
// creation, refill on read, burst cap, opportunistic pruning so the
// map stays bounded by ACTIVE subscribers.
type notifyRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*notifyTokenBucket
	burst   float64
	refill  float64 // tokens per second
}

type notifyTokenBucket struct {
	tokens float64
	last   time.Time
}

// newNotifyRateLimiter builds the 60/h default bucket set.
func newNotifyRateLimiter() *notifyRateLimiter {
	return newNotifyRateLimiterAt(notifyRateBurst, notifyRatePerHour, time.Hour)
}

// newNotifyRateLimiterAt is the parameterized constructor (tests shrink
// the window; production uses the defaults above).
func newNotifyRateLimiterAt(burst, perWindow int, window time.Duration) *notifyRateLimiter {
	if burst <= 0 || perWindow <= 0 || window <= 0 {
		return newNotifyRateLimiterAt(notifyRateBurst, notifyRatePerHour, time.Hour)
	}
	return &notifyRateLimiter{
		buckets: map[string]*notifyTokenBucket{},
		burst:   float64(burst),
		refill:  float64(perWindow) / window.Seconds(),
	}
}

// allow consumes one token for the subscriber; false = refused.
func (l *notifyRateLimiter) allow(subscriber string) bool {
	if l == nil {
		return true
	}
	subscriber = strings.TrimSpace(subscriber)
	if subscriber == "" {
		return false
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[subscriber]
	if !ok {
		b = &notifyTokenBucket{tokens: l.burst, last: now}
		l.buckets[subscriber] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.refill
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	// Opportunistic pruning (sessionLimiter's bound): a map grown past
	// the ceiling sheds subscribers idle for the refill window's span.
	if len(l.buckets) > 4096 {
		idle := time.Duration(float64(time.Hour) / l.refill)
		for id, bb := range l.buckets {
			if now.Sub(bb.last) > idle {
				delete(l.buckets, id)
			}
		}
	}
	return true
}

// ---- the vehicle ----

// seamSettleDeliverer is the §4.5 delivery seam over the loopback seam
// client: rate bound → compose → detached SessionSend. The client is a
// factory (seamClientWithPassword's password is per-request in the MCP
// layer; the notifier's wiring owns its instance) so the outbox boot
// retry and the live path share one vehicle.
type seamSettleDeliverer struct {
	client  func() *opencode.Client
	limiter *notifyRateLimiter
	logger  *zap.Logger
}

// newSeamSettleDeliverer wires the vehicle; a nil limiter still bounds
// (the default 60/h) — the backstop is not optional.
func newSeamSettleDeliverer(client func() *opencode.Client, limiter *notifyRateLimiter, logger *zap.Logger) *seamSettleDeliverer {
	if limiter == nil {
		limiter = newNotifyRateLimiter()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &seamSettleDeliverer{client: client, limiter: limiter, logger: logger}
}

// DeliverSettle performs one notification delivery. The ctx passed in
// is the notifier's delivery parent; the POST itself detaches via
// context.WithoutCancel — shutdown must not cut an in-flight wake-up
// whose intent is already durable in the outbox (ruling 4: the next
// boot's retry is the recovery, so the attempt runs to its own
// conclusion). No timeout is layered on top: busy subscribers queue
// server-side at the turn boundary — that queue-wait is the feature.
func (d *seamSettleDeliverer) DeliverSettle(ctx context.Context, notice settleNotice) error {
	if d == nil || d.client == nil {
		return errors.New("session notifier: seam client unwired")
	}
	if !d.limiter.allow(notice.Subscriber) {
		return fmt.Errorf("%w: subscriber %s (60/hour default)", errNotifyRateLimited, emitID(notice.Subscriber))
	}
	body, err := composeSettleNotification(emitID(notice.Broadcaster), notice.Title, notice.Event)
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	if _, err := d.client().SessionSend(context.WithoutCancel(ctx), notice.Subscriber, body, "", nil); err != nil {
		return fmt.Errorf("POST /session/%s/message: %w", notice.Subscriber, err)
	}
	d.logger.Debug("session notifier: notification delivered",
		zap.String("broadcaster", notice.Broadcaster),
		zap.String("subscriber", notice.Subscriber),
		zap.String("event", notice.Event))
	return nil
}
