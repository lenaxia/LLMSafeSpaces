// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package eventbus — the workspace event bus (design 0072): one Redis
// Stream per workspace, many independent consumer groups.
//
// The bus is a FORMALIZATION of the frame stream, not a replacement:
// the sessionstate authority remains the event source; the producer
// maps its derived events onto a topic vocabulary and appends them to
// the workspace's stream; every consumer (notifier, lease nudge, and
// later audit / SSE) subscribes with its OWN consumer group and reads
// independently — Redis Streams groups give ack/pending/redelivery
// (at-least-once) and last-delivered resume (replay) per group, which
// is exactly the outbox machinery the point-to-point delivery layer
// hand-rolled in subscriptions v1 (design 0071 §4.5), now free.
//
// Topic vocabulary — a CONVENTION, not an enum (design 0072 §2): the
// five named shapes below are the launch set; new topics join by
// agreement between one producer and its consumers, never by editing
// this package's switch statements. Topics are dot-separated segments:
//
//	session.{id}.idle            a session's turn settled idle
//	session.{id}.error           a session's turn ended in error
//	task.{id}.stage_changed      a board task changed stage
//	lease.{task_id}.nudged       the nudge ladder sent its sentinel
//	lease.{task_id}.wedge_verdict the ladder ruled a claim wedged
//	lease.{task_id}.renewed      the ladder renewed a lease (design
//	                             0072 S4: the observation plane's
//	                             renewal decisions, source-tagged)
//	lease.{task_id}.reclaimed    the ladder reclaimed a lapsed claim
//	                             (design 0072 S4: the lapse verdicts
//	                             and reclaims the 2026-10-30 incident
//	                             had to reconstruct by forensics)
//
// Subscriptions are topic PATTERNS with `*` (any ONE segment) and a
// trailing `**` (one or more remaining segments): "session.*.idle"
// matches every session's idle; "*" alone matches nothing (use an
// empty pattern list — the ConsumeOptions default — to take all
// topics).
//
// The delivery edge stays SessionSend (design 0072 §1): the bus routes
// to CONSUMERS, and a consumer that must wake a session translates
// into the session-message machinery itself — the harness-specific
// edge the bus deliberately does not abstract.
package eventbus

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// StreamKey is the per-workspace stream: one key, one totally ordered
// log, every consumer group reading its own cursor off it. Scoped by
// workspace so a pod (or the API) can only ever append to and read
// its own workspace's events.
func StreamKey(workspaceID string) string {
	return "workspace:" + workspaceID + ":events"
}

// Event field names. The topic and publish time ride every entry; a
// topic's consumers own the rest of the schema (the convention above).
const (
	FieldTopic = "topic"
	FieldAt    = "at"
)

// Settle kinds — the suffix of the session settle topics.
const (
	SettleIdle  = "idle"
	SettleError = "error"
)

// Event is one bus message: a topic, a publish time, and a flat
// string payload (Redis stream entries are field/value strings;
// structured values serialize as JSON by convention).
type Event struct {
	Topic   string
	At      time.Time
	Payload map[string]string
}

// Validate refuses an unpublishable event at the seam instead of
// round-tripping Redis for the error: empty topic, or a payload
// colliding with the reserved field names.
func (e Event) Validate() error {
	if strings.TrimSpace(e.Topic) == "" {
		return fmt.Errorf("eventbus: event has no topic")
	}
	if strings.ContainsAny(e.Topic, " \t\n\r") {
		return fmt.Errorf("eventbus: topic %q contains whitespace", e.Topic)
	}
	for k := range e.Payload {
		if k == FieldTopic || k == FieldAt {
			return fmt.Errorf("eventbus: payload field %q is reserved", k)
		}
	}
	return nil
}

// ---- Topic builders (the launch vocabulary) ----

// SessionSettleTopic is session.{id}.idle | session.{id}.error — one
// per derived busy→settle edge (the #1574 derived-busy definition the
// notifier folds; kind is SettleIdle or SettleError).
func SessionSettleTopic(sessionID, kind string) string {
	return "session." + sessionID + "." + kind
}

// TaskStageTopic is task.{id}.stage_changed.
func TaskStageTopic(taskID string) string {
	return "task." + taskID + ".stage_changed"
}

// LeaseNudgeTopic is lease.{task_id}.nudged — payload: task (the
// board task's raw id), task_short_id, nudge_id, assignee (the
// session the sentinel addresses).
func LeaseNudgeTopic(taskID string) string {
	return "lease." + taskID + ".nudged"
}

// LeaseWedgeTopic is lease.{task_id}.wedge_verdict.
func LeaseWedgeTopic(taskID string) string {
	return "lease." + taskID + ".wedge_verdict"
}

// LeaseRenewedTopic is lease.{task_id}.renewed — payload: task (the
// board task's raw id), task_short_id, assignee, source
// (heartbeat_response | movement | post_nudge_reaction | busy_gate —
// WHICH rung of the ladder answered alive; design 0072 S4).
func LeaseRenewedTopic(taskID string) string {
	return "lease." + taskID + ".renewed"
}

// LeaseReclaimedTopic is lease.{task_id}.reclaimed — payload: task,
// task_short_id, assignee ("" when none was recorded), reason
// (lease_lapse | dead_evidence | nudge_delivery_failed |
// lapse_no_assignee | persona_lapse | persona_config_cleared),
// failure_count. Design 0072 S4.
func LeaseReclaimedTopic(taskID string) string {
	return "lease." + taskID + ".reclaimed"
}

// ---- Lease payload builders (design 0072 S4) ----
//
// The payload contract for the lease family, as constructors — the
// API's publisher and agentd's consumer/tests share them, so the
// producer↔consumer agreement is code, not copy-paste (the sentinel
// BODY cannot cross the api/agentd split — pkg/board drags pgx — but
// the payload maps can; this is the byte-parity test's backstop).

// LeaseNudgePayload builds the nudged event's payload.
func LeaseNudgePayload(task, taskShortID, nudgeID, assignee string) map[string]string {
	return map[string]string{
		"task": task, "task_short_id": taskShortID, "nudge_id": nudgeID, "assignee": assignee,
	}
}

// LeaseRenewPayload builds the renewed event's payload (source: which
// ladder rung answered alive — heartbeat_response | movement |
// post_nudge_reaction | busy_gate).
func LeaseRenewPayload(task, taskShortID, assignee, source string) map[string]string {
	return map[string]string{
		"task": task, "task_short_id": taskShortID, "assignee": assignee, "source": source,
	}
}

// LeaseWedgePayload builds the wedge_verdict event's payload
// (evidence: the machine verdict's summary — why this is a verdict,
// not a guess).
func LeaseWedgePayload(task, taskShortID, assignee, nudgeID, evidence string) map[string]string {
	return map[string]string{
		"task": task, "task_short_id": taskShortID, "assignee": assignee,
		"nudge_id": nudgeID, "verdict": "wedged", "evidence": evidence,
	}
}

// LeaseReclaimPayload builds the reclaimed event's payload.
func LeaseReclaimPayload(task, taskShortID, assignee, reason string, failureCount int32) map[string]string {
	return map[string]string{
		"task": task, "task_short_id": taskShortID, "assignee": assignee,
		"reason": reason, "failure_count": strconv.FormatInt(int64(failureCount), 10),
	}
}

// ParseSessionSettle extracts (session id, settle kind) from a
// session.{id}.{kind} topic. ok is false for any other shape.
func ParseSessionSettle(topic string) (sessionID, kind string, ok bool) {
	parts := strings.Split(topic, ".")
	if len(parts) != 3 || parts[0] != "session" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ParseLease extracts (task id, arm) from a lease.{task_id}.{arm}
// topic. ok is false for any other shape.
func ParseLease(topic string) (taskID, arm string, ok bool) {
	parts := strings.Split(topic, ".")
	if len(parts) != 3 || parts[0] != "lease" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Match answers whether a topic satisfies a pattern. Segments are
// dot-separated; "*" matches exactly one segment; "**" as the FINAL
// segment matches one or more remaining segments. A pattern must
// address whole segments ("session.a*" is not a prefix glob).
func Match(pattern, topic string) bool {
	if pattern == "" {
		return false
	}
	p := strings.Split(pattern, ".")
	t := strings.Split(topic, ".")
	for i, ps := range p {
		if ps == "**" {
			// Only meaningful as the last segment; here it means the
			// topic has at least one segment left (i < len(t)).
			return i < len(t)
		}
		if i >= len(t) {
			return false
		}
		if ps != "*" && ps != t[i] {
			return false
		}
	}
	return len(p) == len(t)
}

// MatchAny answers whether any pattern matches (an empty list matches
// EVERYTHING — the ConsumeOptions default subscribe-all).
func MatchAny(patterns []string, topic string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if Match(p, topic) {
			return true
		}
	}
	return false
}

// Message is one delivered event plus its delivery identity: the
// stream entry id (the durability cursor consumers ack) and the
// delivery attempt (1 = first delivery; redeliveries count up — the
// poison-cap input).
type Message struct {
	ID       string
	Event    Event
	Attempts int64
}

// Handler processes one message. Returning nil ACKS the entry (it is
// never redelivered to this group). Returning an error LEAVES it
// pending: after MinIdle it is reclaimed and redelivered —
// at-least-once, duplicates tolerated by contract. Handlers run on
// the consumer loop's goroutine: process synchronously or own the
// fan-out; returning before the work is durable forfeits the retry.
type Handler func(ctx context.Context, msg Message) error

// Consumer-group identities in production today (design 0072 §3);
// the set is open — a new consumer joins by picking an unused group
// name and reading.
const (
	GroupNotifier = "notifier"
	GroupLease    = "lease"
)

// Bus is the transport seam: publish one event, consume a group.
// Implementations must be safe for concurrent use.
type Bus interface {
	// Publish appends one event to the workspace stream. The returned
	// id is the stream entry id (the ack cursor's unit).
	Publish(ctx context.Context, ev Event) (string, error)
	// Consume runs one consumer of a group until ctx ends (or the
	// transport fails unrecoverably — the returned error says which).
	// The group is created if absent, at StartAt's position.
	Consume(ctx context.Context, opts ConsumeOptions, h Handler) error
}

// Start positions for a NEW consumer group (existing groups resume at
// their own last-delivered id — Redis keeps it; these only matter at
// creation).
const (
	// StartLive positions a new group at the stream's end: only
	// events published AFTER the group exists are delivered. The
	// default — a notifier woken by history would re-notify.
	StartLive = "$"
	// StartReplay positions a new group at the stream's start: the
	// retained window (bounded by MAXLEN) is delivered first, then
	// live — the retained-message equivalent: a late subscriber sees
	// what it missed.
	StartReplay = "0-0"
)
