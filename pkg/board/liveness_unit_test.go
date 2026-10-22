// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// liveness_unit_test.go — the #1847 liveness classification pins
// (unit: the rule is pure; the RC1 flow pins live in
// rc1_liveness_1847_test.go). These encode the issue's acceptance bar
// verbatim.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	livGrace  = 30 * time.Minute // ~2x the default 15m lease window
	livStale  = 5 * time.Minute
	livNow    = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	livPushed = livNow.Add(-30 * time.Second)
)

// ACCEPTANCE: "A worker inside a single 20-min silent bash call holds
// its lease (open turn)." Busy (open turn) with fresh pushes is alive
// no matter how stale the message activity is — the 20-min tool call
// emits no messages.
func TestClassifyLiveness_OpenTurnHolds(t *testing.T) {
	v := ClassifyLiveness(livNow, &LivenessSample{
		Busy: true, Started: true,
		LastActivity: livNow.Add(-25 * time.Minute), // silent the whole bash call
		PushedAt:     livPushed,
	}, livGrace, livStale)
	require.Equal(t, LivenessAlive, v)
}

// ACCEPTANCE: "A stillborn spawn is never renewed." No row — the
// session never pushed — and a row that exists but never started are
// both the none class: no renewal, no appeal.
func TestClassifyLiveness_StillbornNeverRenewed(t *testing.T) {
	require.Equal(t, LivenessNone, ClassifyLiveness(livNow, nil, livGrace, livStale),
		"no liveness row (spawn never created the session)")
	require.Equal(t, LivenessNone, ClassifyLiveness(livNow, &LivenessSample{
		Busy: false, Started: false, // created, never began a turn
		LastActivity: livNow.Add(-time.Hour),
		PushedAt:     livPushed,
	}, livGrace, livStale), "never-started session")
}

// Started + idle + activity within grace → alive (recent activity AND
// started, spec item 3); activity beyond grace → dead (the appeal
// class: the wedged turn).
func TestClassifyLiveness_ActivityArm(t *testing.T) {
	within := ClassifyLiveness(livNow, &LivenessSample{
		Busy: false, Started: true,
		LastActivity: livNow.Add(-livGrace + time.Minute),
		PushedAt:     livPushed,
	}, livGrace, livStale)
	require.Equal(t, LivenessAlive, within)

	beyond := ClassifyLiveness(livNow, &LivenessSample{
		Busy: false, Started: true,
		LastActivity: livNow.Add(-livGrace - time.Minute),
		PushedAt:     livPushed,
	}, livGrace, livStale)
	require.Equal(t, LivenessDead, beyond, "wedge: started, idle, silent past grace")
}

// ACCEPTANCE: "A wedged turn (tool timeout) releases within grace + one
// appeal ping." A busy latch whose pushes went stale (pod wedged/OOM)
// is void — a stale observation is not an open turn — but the ACTIVITY
// arm still applies (past evidence stays true): the release bound is
// grace past the last push, then the appeal window. Never infinite.
func TestClassifyLiveness_StaleBusyLatchVoids(t *testing.T) {
	// Stale latch, activity still within grace: alive via the activity
	// arm (the pod's last evidence was recent — it ages out at grace).
	v := ClassifyLiveness(livNow, &LivenessSample{
		Busy: true, Started: true,
		LastActivity: livNow.Add(-10 * time.Minute),
		PushedAt:     livNow.Add(-livStale - time.Minute), // pushes stopped
	}, livGrace, livStale)
	require.Equal(t, LivenessAlive, v, "latch void, activity arm holds")

	// Stale latch AND activity beyond grace: dead — the appeal class.
	v = ClassifyLiveness(livNow, &LivenessSample{
		Busy: true, Started: true,
		LastActivity: livNow.Add(-livGrace - time.Minute),
		PushedAt:     livNow.Add(-livStale - time.Minute),
	}, livGrace, livStale)
	require.Equal(t, LivenessDead, v, "latch void, activity aged out: release is bounded")
}

// ACCEPTANCE: "A garbage/future timestamp cannot affect a lease (no
// such input path)." A future-stamped activity clamps to its PUSH
// anchor — a sample's information cannot be newer than when it was
// pushed — so a garbage stamp is exactly as fresh as the row's honest
// push time, never more: it cannot hold a lease past what the real
// evidence would.
func TestClassifyLiveness_FutureStampClampsToPushAnchor(t *testing.T) {
	// Fresh push + future activity: clamped to ~now → alive, exactly
	// like an honest activity-now row.
	v := ClassifyLiveness(livNow, &LivenessSample{
		Busy: false, Started: true,
		LastActivity: livNow.Add(8 * time.Hour), // hallucinated future
		PushedAt:     livPushed,
	}, livGrace, livStale)
	require.Equal(t, LivenessAlive, v, "clamped to the fresh push anchor")

	// Stale push (45m ago) + future activity: clamped to the push
	// anchor → beyond grace → DEAD — byte-identical verdict to the
	// honest 45m-old-activity row. The stamp changes nothing.
	future := &LivenessSample{
		Busy: false, Started: true,
		LastActivity: livNow.Add(8 * time.Hour),
		PushedAt:     livNow.Add(-45 * time.Minute),
	}
	honest := &LivenessSample{
		Busy: false, Started: true,
		LastActivity: livNow.Add(-45 * time.Minute),
		PushedAt:     future.PushedAt,
	}
	require.Equal(t, ClassifyLiveness(livNow, honest, livGrace, livStale),
		ClassifyLiveness(livNow, future, livGrace, livStale),
		"a future stamp is exactly as fresh as its push anchor, never more")
	require.Equal(t, LivenessDead, ClassifyLiveness(livNow, future, livGrace, livStale),
		"and that anchor is beyond grace here: dead")
}
