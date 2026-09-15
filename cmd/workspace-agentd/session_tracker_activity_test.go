// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_tracker_activity_test.go — #1342 item 3: restart-worthiness
// keys on PROGRESS. The tracker records per-session last-activity from
// the SSE stream (the same stream that feeds busy/idle), and partitions
// busy sessions into progressing vs stalled for the deferred-restart
// force path.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackerActivity_PartUpdateEventNotesActivity(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// A message.part.updated carrying no usage — the plain streaming
	// shape the 40-min build emits continuously.
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"prt_1","type":"text","text":"chunk"}}}`)

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a")
	assert.Empty(t, stalled, "a session with fresh part activity is progressing, never stalled")
}

func TestTrackerActivity_StepFinishUsageNotesActivity(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"type":"step-finish","tokens":{"input":100,"cache":{"read":50,"write":25}}}}}`)

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a")
	assert.Empty(t, stalled)
}

func TestTrackerActivity_SessionStatusNotesActivity(t *testing.T) {
	tracker := newSessionStatusTracker()

	tracker.processEvent(`{"type":"session.status","properties":{"sessionID":"ses_a","status":{"type":"busy"}}}`)

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a", "the busy-mark event itself is activity evidence")
	assert.Empty(t, stalled)
}

func TestTrackerActivity_LegacyNestedEventNotesActivity(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_legacy", "busy")

	// Legacy global-SSE nested envelope (no top-level type).
	tracker.processEvent(`{"payload":{"type":"message.part.updated","properties":{"sessionID":"ses_legacy","part":{"id":"p","type":"text","text":"x"}}}}`)

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_legacy")
	assert.Empty(t, stalled)
}

func TestTrackerActivity_QuietBusySessionFallsStalled(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// No events since the busy-mark. Age the busy-mark past the bound.
	tracker.mu.Lock()
	tracker.busySince["ses_a"] = time.Now().Add(-time.Minute)
	tracker.mu.Unlock()

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, stalled, "ses_a", "a busy session silent past the bound is stalled (force-eligible)")
	assert.Empty(t, prog)
}

func TestTrackerActivity_RecentActivityBeatsOldBusyMark(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")

	// Busy for an hour, but the part streamed 2s ago — progressing.
	tracker.mu.Lock()
	tracker.busySince["ses_a"] = time.Now().Add(-time.Hour)
	tracker.mu.Unlock()
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"p","type":"text","text":"still streaming"}}}`)

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a", "a 40-min build streaming output is progressing regardless of busy age")
	assert.Empty(t, stalled)
}

func TestTrackerActivity_MixedPartitions(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_prog", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_prog","part":{"id":"p1","type":"text","text":"x"}}}`)
	tracker.set("ses_stall", "busy")
	tracker.mu.Lock()
	tracker.busySince["ses_stall"] = time.Now().Add(-2 * time.Minute)
	tracker.mu.Unlock()
	tracker.set("ses_idle", "idle")

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.ElementsMatch(t, []string{"ses_prog"}, prog)
	assert.ElementsMatch(t, []string{"ses_stall"}, stalled)
}

func TestTrackerActivity_IdleSessionsExcluded(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_idle", "idle")

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Empty(t, prog)
	assert.Empty(t, stalled)
}

func TestTrackerActivity_PruneDropsActivity(t *testing.T) {
	tracker := newSessionStatusTracker()
	tracker.set("ses_a", "busy")
	tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_a","part":{"id":"p","type":"text","text":"x"}}}`)

	tracker.prune([]string{"ses_other"})

	tracker.mu.RLock()
	_, hasActivity := tracker.lastEventAt["ses_a"]
	tracker.mu.RUnlock()
	assert.False(t, hasActivity, "prune must drop the activity entry with the status entry")
}

func TestTrackerActivity_NoActivityEntryFallsBackToBusySince(t *testing.T) {
	tracker := newSessionStatusTracker()
	// busy with a recent busy-mark but no event ever observed (set()
	// directly — e.g. tracker seeded by tests): the busy-mark is the
	// activity floor.
	tracker.set("ses_a", "busy")

	prog, stalled := tracker.busyPartitions(30 * time.Second)
	assert.Contains(t, prog, "ses_a")
	assert.Empty(t, stalled)
}

// TestTrackerActivity_UnrelatedEventsDoNotPanicOrMark: events without a
// session-scoped properties payload (usage summaries, connection
// events) neither panic nor invent activity.
func TestTrackerActivity_UnrelatedEventsDoNotPanicOrMark(t *testing.T) {
	tracker := newSessionStatusTracker()
	assert.NotPanics(t, func() {
		tracker.processEvent(`{"type":"usage","properties":{"tokens":{"input":1}}}`)
		tracker.processEvent(`{"type":"connection.identify","properties":{}}`)
		tracker.processEvent(`not json at all`)
	})

	tracker.mu.RLock()
	n := len(tracker.lastEventAt)
	tracker.mu.RUnlock()
	assert.Equal(t, 0, n, "no session activity may be recorded from sessionless events")
}

// TestTrackerActivity_PartUpdateJSONDecodes validates the raw payload
// shape assumption (Rule 7): opencode part-update events carry
// properties.sessionID — pinned against the wire fixture family.
func TestTrackerActivity_PartUpdateJSONDecodes(t *testing.T) {
	raw := `{"type":"message.part.updated.1","properties":{"sessionID":"ses_a","part":{"id":"prt1","type":"step-finish"}}}`
	var evt struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &evt))
	var props struct {
		SessionID string `json:"sessionID"`
	}
	require.NoError(t, json.Unmarshal(evt.Properties, &props))
	assert.Equal(t, "ses_a", props.SessionID)
}
