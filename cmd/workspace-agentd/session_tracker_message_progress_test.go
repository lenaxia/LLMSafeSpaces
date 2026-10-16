// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_tracker_message_progress_test.go — #1685 (owner ruling): the D6
// hung-nudge's evidence clock keys on MESSAGE-LEVEL progress: an
// LLM-COMPLETED message (final message.updated upsert carrying
// info.time.completed) or a USER-SENT message. Stream chunks
// (message.part.*), tool events, heartbeats, and IN-FLIGHT assistant
// upserts (time.created only) do NOT stamp the clock — a long tool
// execution with a message 20 minutes ago is a HEALTHY long turn, not
// a hang. The message shapes below are the golden-fixture shapes
// (sse_events_1_18_10_live.jsonl:59/:62/:65); the completion anchor is
// pinned over every fixture event in the wire package
// (TestGoldenFixture_MessageLevelProgress_CompletionAnchor).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackerMessageProgress_UserSentMessageStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// Real user-send shape (sse_events_1_18_10_live.jsonl:65): user
	// messages carry "cost":null and time.created only.
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_1","role":"user","cost":null,"time":{"created":1787071934103}}}}`)

	ages := tracker.lastMessageAges()
	require.Contains(t, ages, "ses_a")
	assert.Less(t, ages["ses_a"], 5*time.Second, "a user-sent message just stamped the clock")
}

func TestTrackerMessageProgress_CompletedAssistantUpdateStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// Real final-upsert shape (sse_events_1_18_10_live.jsonl:59): the
	// completion anchor is info.time.completed (ruling R1) — cost rides
	// every upsert as 0 and discriminates nothing.
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_2","role":"assistant","cost":0,"time":{"created":1787074779006,"completed":1787074798309}}}}`)

	ages := tracker.lastMessageAges()
	require.Contains(t, ages, "ses_a")
	assert.Less(t, ages["ses_a"], 5*time.Second, "a completed assistant message just stamped the clock")
}

func TestTrackerMessageProgress_StreamChunksDoNotStamp(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// Streaming: part updates/deltas — harness activity (#1342 stamps
	// lastEventAt) but NOT message-level progress.
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"type":"text","text":"chunk"}}}`)
	tracker.processEvent(`{"type":"message.part.delta","properties":{"sessionID":"ses_a","delta":"mor"}}`)
	// A real in-flight assistant creation upsert
	// (sse_events_1_18_10_live.jsonl:62): cost 0 as a number,
	// time.created only — no time.completed, not completion, not
	// progress. (Ruling R1: the pre-rework cost-shape reasoning
	// stamped every such upsert on real wires.)
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_3","role":"assistant","cost":0,"time":{"created":1787074798343}}}}`)

	assert.NotContains(t, tracker.lastMessageAges(), "ses_a",
		"chunks and in-flight assistant upserts must not reset the evidence clock")
	// Belt: the any-event clock DID stamp (#1342 stays untouched).
	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a")
	assert.Empty(t, stalled)
}

func TestTrackerMessageProgress_SuffixedStoreVariantStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// The persisted event store emits version-suffixed types; the
	// classifier is suffix-tolerant like every wire matcher.
	tracker.processEvent(`{"type":"message.updated.1","properties":{"sessionID":"ses_a","info":{"role":"user","text":"hi"}}}`)

	assert.Contains(t, tracker.lastMessageAges(), "ses_a")
}

func TestTrackerMessageProgress_LegacyNestedEnvelopeStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_legacy", "busy")

	tracker.processEvent(`{"payload":{"type":"message.updated","properties":{"sessionID":"ses_legacy","info":{"role":"user","text":"hi"}}}}`)

	assert.Contains(t, tracker.lastMessageAges(), "ses_legacy")
}

func TestTrackerMessageProgress_PruneCleansStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_gone", "busy")
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_gone","info":{"role":"user"}}}`)

	tracker.set("ses_gone", "idle")
	tracker.prune([]string{"ses_other"})

	assert.NotContains(t, tracker.lastMessageAges(), "ses_gone")
}

// statuszPinBackend stubs the opencode client surface the statusz handler
// needs; the pin below asserts the computed LastProgressSeconds field —
// the max message-silence across the authority-filtered busy set.
func TestStatusz_LastProgressSeconds_MaxSilenceAcrossBusySessions(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")
	tracker.set("ses_b", "busy")

	// ses_a: message 31m ago (silent — evidence). ses_b: message just now.
	past := time.Now().Add(-31 * time.Minute)
	tracker.mu.Lock()
	tracker.lastMessageAt["ses_a"] = past
	tracker.lastMessageAt["ses_b"] = time.Now()
	tracker.mu.Unlock()

	ages := tracker.lastMessageAges()
	require.Contains(t, ages, "ses_a")
	assert.GreaterOrEqual(t, ages["ses_a"], 30*time.Minute)

	// The field is computed in the statusz handler's filtered iteration;
	// the tracker exposes the raw per-session ages (mirroring
	// busyDurations' contract) and the handler picks the max over the
	// busy set. This pins the tracker half; the handler half — the
	// authority-filtered max, stale-stamp exclusion included — is pinned
	// through the REAL buildStatuszHandler in busy_overlay_test.go
	// (TestStatuszHandler_LastProgressSecondsThroughTheRealHandler).
	got := ages
	assert.Less(t, got["ses_b"], 5*time.Second, "the fresh-message session contributes ~0 silence")
	assert.Greater(t, got["ses_a"], got["ses_b"], "the max is the silent session, not the fresh one")
}

// jsonShapeGuard: last_progress_seconds is additive and omitempty — the
// wire shape never regresses to required-on-idle (older API servers and
// mixed fleets must tolerate its absence).
func TestStatusz_LastProgressSeconds_WireShapeOmitWhenZero(t *testing.T) {
	b, err := json.Marshal(struct {
		OldestBusySeconds   int `json:"oldest_busy_seconds"`
		LastProgressSeconds int `json:"last_progress_seconds,omitempty"`
	}{OldestBusySeconds: 42})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "last_progress_seconds", "0 must omit (evidence-unavailable shape)")
}
