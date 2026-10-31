// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus

// health.go — the bus health surface (S3): one snapshot call answers
// "is the bus alive, and is every consumer group keeping up?" without
// the caller speaking Redis. Silence on the bus is legitimate (a quiet
// workspace publishes nothing); SILENCE PLUS A GROWING BACKLOG is the
// failure this surface exists to expose: a stalled consumer group
// (entries pile up undelivered) or a wedged one (delivered entries
// never ack, pending idle grows past every reclaim threshold).
//
// The snapshot deliberately does NOT use go-redis's structured XINFO
// helpers: v8's readXGroupInfo hard-requires the Redis 6 reply shape
// (exactly name/consumers/pending/last-delivered-id) and ERRORS on the
// entries-read/lag fields Redis 7+ — and this repo's own test Redis
// (miniredis) — return. Health parsing goes through the GENERIC Do
// reply and ignores fields it does not know, so the surface survives
// Redis/Valkey version drift by construction. Lag is likewise COMPUTED
// (entries after the group's cursor, via exclusive-range XRANGE pages)
// rather than trusted from XINFO's lag field, whose meaning differs
// across server versions and mock implementations.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
)

// Health probe bounds. Lag is counted in pages and capped: a group
// more than HealthMaxLag entries behind is unambiguously stalled, and
// the exact number past the cap is not worth the extra rounds. The
// pending probe reads only the first HealthMaxPending PEL entries —
// enough for the oldest-idle signal; a PEL that large is its own
// incident.
const (
	HealthLagPageSize = int64(500)
	HealthMaxLag      = int64(10_000)
	HealthMaxPending  = int64(128)
	healthLagMaxPages = HealthMaxLag / HealthLagPageSize
)

// GroupHealth is one consumer group's snapshot: whether it is keeping
// up (Lag), whether its members are alive (OldestPendingIdle vs the
// reclaim threshold, Consumers), and how much un-acked work it holds
// (Pending, MaxPendingAttempts).
type GroupHealth struct {
	// Name is the consumer group name.
	Name string
	// Consumers is the group's registered consumer count (Redis keeps
	// names until XGROUP DELCONSUMER — a stale name from a dead pod
	// still counts; Consumers==0 means NO consumer ever joined or all
	// were explicitly removed).
	Consumers int64
	// Pending is the group's un-acked (delivered-but-not-acked) count.
	Pending int64
	// LastDeliveredID is the group's cursor: the last entry id handed
	// to ANY of its consumers.
	LastDeliveredID string
	// Lag is the count of stream entries AFTER the group's cursor —
	// the undelivered backlog. Capped at HealthMaxLag.
	Lag int64
	// OldestPendingIdle is how long the longest-idle pending entry has
	// sat un-acked (0 when the PEL is empty, or when the probe's first
	// page holds none). A healthy consumer reclaims past MinIdle
	// (60s default); idle far past that means no consumer is
	// reclaiming — the wedged-group signal.
	OldestPendingIdle time.Duration
	// MaxPendingAttempts is the highest delivery count among the
	// probed pending entries — the poison-adjacent signal (a handler
	// failing repeatedly on the same entry).
	MaxPendingAttempts int64
}

// StreamHealth is the whole stream's snapshot: the publish side
// (Length, LastPublishAt) and every consumer group's GroupHealth.
type StreamHealth struct {
	// Length is the stream's entry count (bounded by the publisher's
	// MAXLEN; ~10k in production).
	Length int64
	// LastEntryID is the newest entry's id ("" when the stream is
	// empty).
	LastEntryID string
	// LastPublishAt is the newest entry's publish time (the entry's
	// `at` field). Zero when the stream is empty — the honest
	// "no publish age known", not "published at epoch".
	LastPublishAt time.Time
	// Groups holds one GroupHealth per consumer group on the stream.
	Groups []GroupHealth
}

// Health snapshots the stream and its consumer groups. A stream that
// does not exist yet (no publish, no group ever created — the
// dormant-bus shape) is a ZERO StreamHealth, not an error: an empty
// bus is a healthy bus until proven otherwise. Errors mean the probe
// itself failed (Redis unreachable) — the caller's stalled/stale
// gauges keep their last values and the error is the signal.
func (b *RedisBus) Health(ctx context.Context) (*StreamHealth, error) {
	if b == nil || b.client == nil {
		return nil, errors.New("eventbus: bus unwired")
	}
	h := &StreamHealth{}
	length, err := b.Len(ctx)
	if err != nil {
		return nil, fmt.Errorf("eventbus: health XLEN %s: %w", b.key, err)
	}
	h.Length = length
	if length == 0 {
		return h, nil // no stream, no groups, no last entry
	}
	if err := b.probeLastEntry(ctx, h); err != nil {
		return nil, err
	}
	groups, err := b.probeGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		gh := GroupHealth{Name: g.Name, Consumers: g.Consumers, Pending: g.Pending, LastDeliveredID: g.LastDeliveredID}
		if err := b.probeGroupDetail(ctx, &gh); err != nil {
			return nil, err
		}
		h.Groups = append(h.Groups, gh)
	}
	return h, nil
}

// probeLastEntry fills LastEntryID/LastPublishAt from the stream's
// newest entry (XREVRANGE + COUNT 1).
func (b *RedisBus) probeLastEntry(ctx context.Context, h *StreamHealth) error {
	res, err := b.client.XRevRangeN(ctx, b.key, "+", "-", 1).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil // raced a trim to empty; Length said non-zero
		}
		return fmt.Errorf("eventbus: health XREVRANGE %s: %w", b.key, err)
	}
	if len(res) == 0 {
		return nil
	}
	h.LastEntryID = res[0].ID
	msg := messageOf(res[0].ID, res[0].Values, 1)
	h.LastPublishAt = msg.Event.At
	return nil
}

// rawGroupInfo is the subset of XINFO GROUPS the health surface uses,
// parsed from the generic reply.
type rawGroupInfo struct {
	Name            string
	Consumers       int64
	Pending         int64
	LastDeliveredID string
}

// probeGroups reads XINFO GROUPS through the generic command surface
// (see the file header: the structured v8 helper is version-brittle).
func (b *RedisBus) probeGroups(ctx context.Context) ([]rawGroupInfo, error) {
	v, err := b.client.Do(ctx, "XINFO", "GROUPS", b.key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("eventbus: health XINFO GROUPS %s: %w", b.key, err)
	}
	rows, ok := v.([]interface{})
	if !ok {
		return nil, nil // unexpected reply shape: no groups reported
	}
	out := make([]rawGroupInfo, 0, len(rows))
	for _, row := range rows {
		fields, ok := row.([]interface{})
		if !ok {
			continue
		}
		kv := toStringPairs(fields)
		g := rawGroupInfo{}
		for i := 0; i+1 < len(kv); i += 2 {
			switch kv[i] {
			case "name":
				g.Name = kv[i+1]
			case "consumers":
				g.Consumers = parseCount(kv[i+1])
			case "pending":
				g.Pending = parseCount(kv[i+1])
			case "last-delivered-id":
				g.LastDeliveredID = kv[i+1]
			}
		}
		if g.Name != "" {
			out = append(out, g)
		}
	}
	return out, nil
}

// toStringPairs flattens one XINFO reply row to its string pairs.
// Integer replies (counts) arrive from Do as int64, bulk strings as
// string, nested replies (first-entry/last-entry arrays) are skipped.
func toStringPairs(fields []interface{}) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		switch v := f.(type) {
		case string:
			out = append(out, v)
		case int64:
			out = append(out, strconv.FormatInt(v, 10))
		}
	}
	return out
}

func parseCount(s string) int64 {
	var n int64
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

// probeGroupDetail fills Lag (computed: entries after the group's
// cursor) and the pending probes (oldest idle, max attempts).
func (b *RedisBus) probeGroupDetail(ctx context.Context, g *GroupHealth) error {
	lag, err := b.probeLag(ctx, g.LastDeliveredID)
	if err != nil {
		return err
	}
	g.Lag = lag
	return b.probePending(ctx, g)
}

// probeLag counts stream entries strictly after the group's cursor by
// paging exclusive-start XRANGE: "(<last-delivered>" .. "+". A cursor
// at "0-0" (a replay group that never delivered) counts the whole
// stream — every entry is undelivered for that group, which is the
// true answer. Counting ourselves (instead of trusting XINFO's lag
// field) keeps the number identical across Redis, Valkey, and
// miniredis.
func (b *RedisBus) probeLag(ctx context.Context, lastDelivered string) (int64, error) {
	if lastDelivered == "" {
		return 0, nil
	}
	start := "(" + lastDelivered
	var total int64
	for page := int64(0); page < healthLagMaxPages; page++ {
		res, err := b.client.XRangeN(ctx, b.key, start, "+", HealthLagPageSize).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return total, nil
			}
			return 0, fmt.Errorf("eventbus: health XRANGE(lag) %s: %w", b.key, err)
		}
		total += int64(len(res))
		if int64(len(res)) < HealthLagPageSize {
			return total, nil
		}
	}
	return HealthMaxLag, nil // capped: far past any healthy backlog
}

// probePending fills OldestPendingIdle and MaxPendingAttempts from the
// first HealthMaxPending pending entries. Bounded by design: the
// signal (pending idle far past reclaim) is present on the first page
// long before the PEL grows past it.
func (b *RedisBus) probePending(ctx context.Context, g *GroupHealth) error {
	pend, err := b.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: b.key,
		Group:  g.Name,
		Start:  "0-0",
		End:    "+",
		Count:  HealthMaxPending,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil // empty PEL
		}
		return fmt.Errorf("eventbus: health XPENDING %s/%s: %w", b.key, g.Name, err)
	}
	// go-redis's XPendingExt.Idle is ALREADY a time.Duration (the wire
	// milliseconds converted at the reply boundary — command.go's
	// `Idle: time.Duration(idle) * time.Millisecond`), and RetryCount
	// is Redis's 1-based delivery count (1 = first delivery, the same
	// vocabulary as DeliveryEvent.Attempts). Neither needs conversion
	// here — the review round-1 bugs were re-multiplying idle ×10⁶ and
	// double-counting attempts.
	for _, p := range pend {
		if p.Idle > g.OldestPendingIdle {
			g.OldestPendingIdle = p.Idle
		}
		if p.RetryCount > g.MaxPendingAttempts {
			g.MaxPendingAttempts = p.RetryCount
		}
	}
	return nil
}
