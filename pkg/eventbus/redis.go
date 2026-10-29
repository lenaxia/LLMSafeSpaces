// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus

// redis.go — the Redis Streams transport: XADD (MAXLEN-bounded) on
// the publish side; consumer groups (XREADGROUP / XACK / XPENDING /
// XCLAIM) on the consume side. The client is go-redis v8 — the same
// dependency pkg/secrets and the API's Redis services already ride;
// miniredis (also already in go.mod) drives the test suite.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

// DefaultMaxLen is the stream-length cap: XADD's approximate MAXLEN —
// the retained-replay window AND the memory bound. ~10k entries of
// flat-string payloads is single-digit megabytes; older entries fall
// off the front and a replaying group simply starts later (a group
// whose last-delivered id was trimmed resumes at the stream's head —
// Redis semantics; the at-least-once contract is unaffected).
const DefaultMaxLen = 10_000

// Consume defaults (design 0072 §3): a 5s blocking read keeps the
// loop ctx-responsive without a busy poll; orphans are reclaimed
// after 60s idle (a crashed/evicted consumer's pending becomes this
// consumer's redelivery — the at-least-once engine); a message that
// fails 5 deliveries is acked-and-dropped with a loud log (poison
// protection: at-least-once must not mean at-least-once forever); a
// transient transport error backpressures one round at 500ms (the
// notifyResubscribeBackoff posture — the consumer outlives a Redis
// restart/failover instead of dying and losing wake-ups).
const (
	DefaultBlock       = 5 * time.Second
	DefaultMinIdle     = 60 * time.Second
	DefaultMaxAttempts = 5
	DefaultReclaimTick = 15 * time.Second
	DefaultBatch       = 16
	DefaultBackoff     = 500 * time.Millisecond
)

// ConsumeOptions shapes one Consume run.
type ConsumeOptions struct {
	// Group + Consumer name the Redis consumer group and this
	// instance's membership in it (stable across restarts: a consumer
	// resuming under its own name first drains its own pending).
	Group    string
	Consumer string
	// Topics are subscription patterns (MatchAny); empty = all.
	Topics []string
	// StartAt positions a NEW group: StartLive ($) or StartReplay
	// ("0"). Existing groups resume at their own cursor. Default
	// StartLive.
	StartAt string
	// MaxLen is unused here (the publisher owns the cap) — kept off
	// the options; see DefaultMaxLen.

	// Tunables (tests shrink them; production takes the defaults):
	Block       time.Duration // XREADGROUP block per round (0 ⇒ default)
	MinIdle     time.Duration // reclaim threshold (0 ⇒ default)
	ReclaimTick time.Duration // reclaim sweep cadence (0 ⇒ default)
	Batch       int64         // max messages per read (0 ⇒ default)
	// Backoff is the transient-transport-error backpressure (0 ⇒
	// default) — one wait per failed round, then the round retries.
	Backoff time.Duration
	// MaxAttempts caps redeliveries before the loud poison drop
	// (0 ⇒ default; negative disables the cap — never in production).
	MaxAttempts int64
	// OnPoison observes poison drops (tests assert; production logs).
	OnPoison func(msg Message)
	// OnError observes handler failures (the message stays pending —
	// the reclaim is the retry; this hook is the visibility, not the
	// mechanism: a silently redelivering consumer is undebuggable).
	OnError func(msg Message, err error)
}

func (o *ConsumeOptions) normalize() error {
	if strings.TrimSpace(o.Group) == "" {
		return errors.New("eventbus: consume requires a group name")
	}
	if strings.TrimSpace(o.Consumer) == "" {
		return errors.New("eventbus: consume requires a consumer name")
	}
	if o.StartAt == "" {
		o.StartAt = StartLive
	}
	if o.StartAt != StartLive && o.StartAt != StartReplay {
		return fmt.Errorf("eventbus: StartAt must be %q or %q, got %q", StartLive, StartReplay, o.StartAt)
	}
	if o.Block <= 0 {
		o.Block = DefaultBlock
	}
	if o.MinIdle <= 0 {
		o.MinIdle = DefaultMinIdle
	}
	if o.ReclaimTick <= 0 {
		o.ReclaimTick = DefaultReclaimTick
	}
	if o.Batch <= 0 {
		o.Batch = DefaultBatch
	}
	if o.Backoff <= 0 {
		o.Backoff = DefaultBackoff
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	return nil
}

// RedisBus is the transport over one workspace stream. Construct with
// NewRedisBus. The zero value is not usable.
type RedisBus struct {
	client *redis.Client
	key    string
	maxLen int64
}

// NewRedisBus wires the transport: client is a LIVE client (the
// constructor pings — a bus over a dead Redis must fail construction,
// never silently drop publishes); key is the workspace's stream key
// (StreamKey); maxLen <= 0 ⇒ DefaultMaxLen.
func NewRedisBus(ctx context.Context, client *redis.Client, key string, maxLen int64) (*RedisBus, error) {
	if client == nil {
		return nil, errors.New("eventbus: nil redis client")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("eventbus: empty stream key")
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("eventbus: redis ping: %w", err)
	}
	if maxLen <= 0 {
		maxLen = DefaultMaxLen
	}
	return &RedisBus{client: client, key: key, maxLen: maxLen}, nil
}

// Key exposes the stream key (diagnostics).
func (b *RedisBus) Key() string { return b.key }

// Publish appends one event: XADD key MAXLEN ~ cap * topic at
// field value... — the cap rides every append, so trimming needs no
// side loop and the retained window is bounded by construction.
func (b *RedisBus) Publish(ctx context.Context, ev Event) (string, error) {
	if b == nil || b.client == nil {
		return "", errors.New("eventbus: bus unwired")
	}
	if err := ev.Validate(); err != nil {
		return "", err
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	fields := make(map[string]interface{}, len(ev.Payload)+2)
	for k, v := range ev.Payload {
		fields[k] = v
	}
	fields[FieldTopic] = ev.Topic
	fields[FieldAt] = at.UTC().Format(time.RFC3339Nano)
	id, err := b.client.XAdd(ctx, &redis.XAddArgs{
		Stream: b.key,
		MaxLen: b.maxLen,
		Approx: true,
		Values: fields,
	}).Result()
	if err != nil {
		return "", fmt.Errorf("eventbus: XADD %s: %w", b.key, err)
	}
	return id, nil
}

// Consume runs one consumer until ctx ends. Loop shape:
//
//  1. ensure the group exists (XGROUP CREATE ... MKSTREAM at StartAt;
//     BUSYGROUP = someone got there first — fine);
//  2. drain OWN pending (XREADGROUP id 0): a restart under the same
//     consumer name resumes its un-acked work immediately, no
//     MinIdle wait;
//  3. the steady loop — each round: one reclaim sweep (XPENDING
//     idle-list → XCLAIM → deliver with the durable delivery count),
//     then one blocking XREADGROUP > (new entries only).
//
// Transport resilience: a TRANSIENT Redis error (restart, failover,
// network blip) backpressures one round and retries — the consumer
// outlives the outage (the sessionNotifier.Run resubscribe posture:
// entries published while the consumer waits are delivered when the
// transport returns; a dead consumer group member is the
// lost-wake-up failure the design names as the one that matters).
// Only precondition failures (unwired bus, bad options, nil handler)
// return without retry. Consume therefore returns nil for every
// ctx-ended path.
//
// Topic filtering happens BEFORE the handler and INDEPENDENT of
// acking: a filtered-out message is acked unread (its group cursor
// must still advance) — patterns subscribe, they do not leave gaps.
func (b *RedisBus) Consume(ctx context.Context, opts ConsumeOptions, h Handler) error {
	if b == nil || b.client == nil {
		return errors.New("eventbus: bus unwired")
	}
	if err := opts.normalize(); err != nil {
		return err
	}
	if h == nil {
		return errors.New("eventbus: consume requires a handler")
	}
	// Group creation retries too — a first-boot race with Redis
	// availability must not kill the consumer before it starts.
	for {
		err := b.client.XGroupCreateMkStream(ctx, b.key, opts.Group, opts.StartAt).Err()
		if err == nil || strings.Contains(err.Error(), "BUSYGROUP") {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		if !b.sleepBackoff(ctx, &opts) {
			return nil
		}
	}
	// Own pending first: resume-under-own-name gets its un-acked
	// entries without waiting out MinIdle. Delivery counts come from
	// the PEL (XPendingExt per consumer) — XREADGROUP alone does not
	// carry them.
	for {
		err := b.drainOwnPending(ctx, &opts, h)
		if err == nil || ctx.Err() != nil {
			break
		}
		if !b.sleepBackoff(ctx, &opts) {
			return nil
		}
	}
	reclaim := time.NewTicker(opts.ReclaimTick)
	defer reclaim.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := b.claimIdle(ctx, &opts, h); err != nil {
			if !b.sleepBackoff(ctx, &opts) {
				return nil
			}
			continue
		}
		if err := b.readNew(ctx, &opts, h); err != nil {
			if !b.sleepBackoff(ctx, &opts) {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-reclaim.C:
		}
	}
}

// sleepBackoff waits one backoff tick (the transient-error
// backpressure); false reports ctx ended. The round that failed is
// retried by the caller — every X* round is idempotent (reading and
// claiming are read/claim semantics; acks ride delivery success).
func (b *RedisBus) sleepBackoff(ctx context.Context, opts *ConsumeOptions) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(opts.Backoff):
		return true
	}
}

// readNew is one blocking XREADGROUP ">" round.
func (b *RedisBus) readNew(ctx context.Context, opts *ConsumeOptions, h Handler) error {
	res, err := b.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    opts.Group,
		Consumer: opts.Consumer,
		Streams:  []string{b.key, ">"},
		Count:    opts.Batch,
		Block:    opts.Block,
	}).Result()
	if err != nil {
		// redis.Nil is the BLOCK timeout — a clean round with nothing
		// new, not an error.
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("eventbus: XREADGROUP %s/%s: %w", b.key, opts.Group, err)
	}
	for _, xs := range res {
		for _, m := range xs.Messages {
			b.dispatch(ctx, opts, h, messageOf(m.ID, m.Values, 1))
		}
	}
	return nil
}

// drainOwnPending delivers this consumer's pre-existing pending
// entries (the restart-resume leg). Entries with no recoverable
// payload (trimmed from the stream while pending — Redis keeps the
// PEL row but XREADGROUP returns values-less shells for them) are
// acked: undeliverable is not retryable.
func (b *RedisBus) drainOwnPending(ctx context.Context, opts *ConsumeOptions, h Handler) error {
	counts, err := b.pendingCounts(ctx, opts, opts.Consumer)
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		res, err := b.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    opts.Group,
			Consumer: opts.Consumer,
			Streams:  []string{b.key, "0"},
			Count:    opts.Batch,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return nil
			}
			return fmt.Errorf("eventbus: XREADGROUP(0) %s/%s: %w", b.key, opts.Group, err)
		}
		n := 0
		for _, xs := range res {
			for _, m := range xs.Messages {
				n++
				if len(m.Values) == 0 {
					// A trimmed shell: the entry the PEL row points at
					// no longer exists. Ack and move on.
					_ = b.client.XAck(ctx, b.key, opts.Group, m.ID).Err()
					continue
				}
				b.dispatch(ctx, opts, h, messageOf(m.ID, m.Values, counts[m.ID]))
			}
		}
		if n == 0 {
			return nil
		}
	}
}

// claimIdle is one reclaim sweep: the group's WHOLE pending list
// (any consumer) idle past MinIdle, XCLAIMed to this consumer, and
// delivered with the durable delivery count. This is the
// crashed-consumer leg of at-least-once: a pending entry whose
// consumer died (or is wedged) becomes this consumer's redelivery.
func (b *RedisBus) claimIdle(ctx context.Context, opts *ConsumeOptions, h Handler) error {
	pend, err := b.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: b.key,
		Group:  opts.Group,
		Idle:   opts.MinIdle,
		Start:  "0-0",
		End:    "+",
		Count:  opts.Batch,
	}).Result()
	if err != nil {
		// An empty PEL answers redis.Nil — nothing idle, not a fault.
		if errors.Is(err, redis.Nil) {
			return nil
		}
		return fmt.Errorf("eventbus: XPENDING %s/%s: %w", b.key, opts.Group, err)
	}
	if len(pend) == 0 {
		return nil
	}
	counts := make(map[string]int64, len(pend))
	ids := make([]string, 0, len(pend))
	for _, p := range pend {
		counts[p.ID] = p.RetryCount
		ids = append(ids, p.ID)
	}
	claimed, err := b.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   b.key,
		Group:    opts.Group,
		Consumer: opts.Consumer,
		MinIdle:  opts.MinIdle,
		Messages: ids,
	}).Result()
	if err != nil {
		return fmt.Errorf("eventbus: XCLAIM %s/%s: %w", b.key, opts.Group, err)
	}
	for _, m := range claimed {
		if len(m.Values) == 0 {
			_ = b.client.XAck(ctx, b.key, opts.Group, m.ID).Err()
			continue
		}
		b.dispatch(ctx, opts, h, messageOf(m.ID, m.Values, counts[m.ID]+1))
	}
	return nil
}

// pendingCounts reads the delivery count per pending id for one
// consumer (the PEL's RetryCount — the durable attempts ledger X*
// reads never return inline). PAGED from the last id forward so a
// restart with more own-pending than one page still sees true counts
// for every entry (a clamp-to-1 reset would forgive poison).
func (b *RedisBus) pendingCounts(ctx context.Context, opts *ConsumeOptions, consumer string) (map[string]int64, error) {
	out := map[string]int64{}
	start := "0-0"
	for {
		pend, err := b.client.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream:   b.key,
			Group:    opts.Group,
			Idle:     0,
			Start:    start,
			End:      "+",
			Count:    opts.Batch * 16,
			Consumer: consumer,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return out, nil // no pending for this consumer
			}
			return nil, fmt.Errorf("eventbus: XPENDING(consumer) %s/%s: %w", b.key, opts.Group, err)
		}
		if len(pend) == 0 {
			return out, nil
		}
		for _, p := range pend {
			out[p.ID] = p.RetryCount
		}
		if int64(len(pend)) < opts.Batch*16 {
			return out, nil
		}
		next, err := nextStreamID(pend[len(pend)-1].ID)
		if err != nil {
			return out, nil // unparseable tail id: the page seen is what we have
		}
		start = next
	}
}

// nextStreamID returns id's successor (ms-seq → ms-seq+1) — XPENDING
// ranges are inclusive, so paging strictly forward needs +1 on the
// seq.
func nextStreamID(id string) (string, error) {
	i := strings.IndexByte(id, '-')
	if i < 0 {
		return "", fmt.Errorf("eventbus: malformed stream id %q", id)
	}
	seq, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		return "", fmt.Errorf("eventbus: malformed stream id %q: %w", id, err)
	}
	return fmt.Sprintf("%s-%d", id[:i], seq+1), nil
}

// dispatch runs one message through pattern filter → poison cap →
// handler → ack. Filtered-out entries ack unread (the cursor must
// advance past them). A handler error leaves the entry pending — the
// reclaim sweep is the retry. An ACK failure is surfaced through
// OnError (an unacked delivered entry redelivers — a duplicate, but
// an invisible one is undebuggable).
func (b *RedisBus) dispatch(ctx context.Context, opts *ConsumeOptions, h Handler, msg Message) {
	if !MatchAny(opts.Topics, msg.Event.Topic) {
		if err := b.client.XAck(ctx, b.key, opts.Group, msg.ID).Err(); err != nil && opts.OnError != nil {
			opts.OnError(msg, fmt.Errorf("ack(filtered): %w", err))
		}
		return
	}
	if opts.MaxAttempts > 0 && msg.Attempts > opts.MaxAttempts {
		if opts.OnPoison != nil {
			opts.OnPoison(msg)
		}
		if err := b.client.XAck(ctx, b.key, opts.Group, msg.ID).Err(); err != nil && opts.OnError != nil {
			opts.OnError(msg, fmt.Errorf("ack(poison): %w", err))
		}
		return
	}
	if err := h(ctx, msg); err != nil {
		if opts.OnError != nil {
			opts.OnError(msg, err)
		}
		return // pending: at-least-once redelivery after MinIdle
	}
	if err := b.client.XAck(ctx, b.key, opts.Group, msg.ID).Err(); err != nil && opts.OnError != nil {
		opts.OnError(msg, fmt.Errorf("ack: %w", err))
	}
}

// messageOf rebuilds an Event from a raw stream entry.
func messageOf(id string, values map[string]interface{}, attempts int64) Message {
	ev := Event{Payload: map[string]string{}}
	for k, v := range values {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprintf("%v", v)
		}
		switch k {
		case FieldTopic:
			ev.Topic = s
		case FieldAt:
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				ev.At = t
			}
		default:
			ev.Payload[k] = s
		}
	}
	if attempts < 1 {
		attempts = 1
	}
	return Message{ID: id, Event: ev, Attempts: attempts}
}

// Len reports the stream length (diagnostics + tests).
func (b *RedisBus) Len(ctx context.Context) (int64, error) {
	n, err := b.client.XLen(ctx, b.key).Result()
	if err != nil && errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}
