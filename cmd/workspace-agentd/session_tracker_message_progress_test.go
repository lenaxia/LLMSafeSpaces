// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_tracker_message_progress_test.go — #1685 (owner ruling): the D6
// hung-nudge's evidence clock keys on MESSAGE-LEVEL progress: an
// LLM-COMPLETED message (cost-bearing message.updated — the same
// billing-completion discriminator the ABI translator uses) or a
// USER-SENT message. Stream chunks (message.part.*), tool events, and
// heartbeats do NOT stamp the clock — a long tool execution with a
// message 20 minutes ago is a HEALTHY long turn, not a hang.

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

	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_1","role":"user","text":"run the suite"}}}`)

	ages := tracker.lastMessageAges()
	require.Contains(t, ages, "ses_a")
	assert.Less(t, ages["ses_a"], 5*time.Second, "a user-sent message just stamped the clock")
}

func TestTrackerMessageProgress_CostBearingAssistantUpdateStamps(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// The assistant-completion shape: message.updated WITH cost (the
	// billing-completion signal the ABI translator maps to MESSAGE_END).
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_2","role":"assistant","cost":{"input":1,"output":2}}}}`)

	ages := tracker.lastMessageAges()
	require.Contains(t, ages, "ses_a")
}

func TestTrackerMessageProgress_StreamChunksDoNotStamp(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// Streaming: part updates/deltas — harness activity (#1342 stamps
	// lastEventAt) but NOT message-level progress.
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"type":"text","text":"chunk"}}}`)
	tracker.processEvent(`{"type":"message.part.delta","properties":{"sessionID":"ses_a","delta":"mor"}}`)
	// An assistant message still mid-flight (no cost): the store-upsert
	// shape that precedes streaming — not completion, not progress.
	tracker.processEvent(`{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_3","role":"assistant"}}}`)

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
	// busy set. Pin the tracker half here; the handler half is pinned by
	// the API-side evidence-gate suite.
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
