// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// quiet_test.go — the #2047 quiet/busy-seconds surface pins: the WIRE
// SHAPE contract (camelCase assigneeQuietSeconds/assigneeBusySeconds —
// owner decision 2026-11-15, the heartbeat-response precedent, NOT the
// row-shape snake_case) and the decorate pass's eligibility + TTL
// semantics.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQuietSource scripts the quiet seam: per-ref answers plus the
// asked-ref tape.
type fakeQuietSource struct {
	answers map[string]AssigneeQuietAnswer
	asked   []string
}

func (f *fakeQuietSource) AssigneeQuiet(_ context.Context, refs []string) map[string]AssigneeQuietAnswer {
	f.asked = append(f.asked, refs...)
	out := make(map[string]AssigneeQuietAnswer, len(refs))
	for _, r := range refs {
		if a, ok := f.answers[r]; ok {
			out[r] = a
		}
	}
	return out
}

func intp(v int) *int { return &v }

// TestQuiet_WireShapeContract pins the exact wire keys: camelCase per
// the owner decision, present when set, ABSENT when unknown (nil) —
// the omitempty contract consumers build against.
func TestQuiet_WireShapeContract(t *testing.T) {
	quiet := 1875
	busy := 12
	on := Task{
		ID: "t1", Stage: StageInProgress, AssigneeSession: "ses-a",
		AssigneeQuietSeconds: &quiet, AssigneeBusySeconds: &busy,
	}
	b, err := json.Marshal(on)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(b, &wire))

	assert.Contains(t, wire, "assigneeQuietSeconds", "the quiet field rides the wire in camelCase")
	assert.Contains(t, wire, "assigneeBusySeconds", "the busy field rides the wire in camelCase")
	assert.Equal(t, float64(1875), wire["assigneeQuietSeconds"])
	assert.Equal(t, float64(12), wire["assigneeBusySeconds"])
	for _, banned := range []string{"assignee_quiet_seconds", "assignee_busy_seconds"} {
		assert.NotContains(t, wire, banned, "the net-new fields are camelCase, never the row-shape snake_case")
	}

	// Unknown: nil fields are ABSENT from the wire (omitempty) — the
	// consumer's unknown is a missing key, never a 0.
	off, err := json.Marshal(Task{ID: "t2"})
	require.NoError(t, err)
	assert.NotContains(t, string(off), "assigneeQuietSeconds")
	assert.NotContains(t, string(off), "assigneeBusySeconds")
}

// TestQuiet_DecorateEligibility: only claimed in_progress rows with an
// assignee are asked; everything else renders unknown and is never
// even queried (the asked-ref tape).
func TestQuiet_DecorateEligibility(t *testing.T) {
	src := &fakeQuietSource{answers: map[string]AssigneeQuietAnswer{
		"ses-live": {QuietSeconds: intp(42)},
	}}
	s := &Service{quietSrc: src}

	live := time.Now().Add(30 * time.Minute)
	claimed := &Task{Stage: StageInProgress, AssigneeSession: "ses-live", LeaseExpiresAt: &live}
	backlog := &Task{Stage: StageBacklog, AssigneeSession: "ses-unclaimed"}
	unassigned := &Task{Stage: StageInProgress}
	review := &Task{Stage: StageInReview, AssigneeSession: "ses-reviewer"}

	s.decorateAssigneeQuiet(context.Background(), claimed, backlog, unassigned, review)

	require.NotNil(t, claimed.AssigneeQuietSeconds)
	assert.Equal(t, 42, *claimed.AssigneeQuietSeconds)
	assert.Nil(t, backlog.AssigneeQuietSeconds)
	assert.Nil(t, unassigned.AssigneeQuietSeconds)
	assert.Nil(t, review.AssigneeQuietSeconds)
	assert.Equal(t, []string{"ses-live"}, src.asked,
		"only the claimed in_progress assignee is asked — ineligible rows are never queried")
}

// TestQuiet_BusySecondsTTL: the busy seconds ride the busy gate's
// lease TTL — a lapsed claim keeps its QUIET figure (the incident's
// exact signal) but never reports busy seconds (a lapsed claim never
// spins), mirroring busyReportable.
func TestQuiet_BusySecondsTTL(t *testing.T) {
	src := &fakeQuietSource{answers: map[string]AssigneeQuietAnswer{
		"ses-held": {QuietSeconds: intp(9000), BusySeconds: intp(9000)},
	}}
	s := &Service{quietSrc: src}

	future := time.Now().Add(30 * time.Minute)
	past := time.Now().Add(-5 * time.Minute)
	liveLease := &Task{Stage: StageInProgress, AssigneeSession: "ses-held", LeaseExpiresAt: &future}
	lapsedLease := &Task{Stage: StageInProgress, AssigneeSession: "ses-held", LeaseExpiresAt: &past}
	noLease := &Task{Stage: StageInProgress, AssigneeSession: "ses-held"}

	s.decorateAssigneeQuiet(context.Background(), liveLease, lapsedLease, noLease)

	require.NotNil(t, liveLease.AssigneeBusySeconds, "live lease + busy truth reports busy seconds")
	require.NotNil(t, liveLease.AssigneeQuietSeconds)
	assert.Nil(t, lapsedLease.AssigneeBusySeconds, "lapsed claim: busy seconds quiet under the TTL")
	require.NotNil(t, lapsedLease.AssigneeQuietSeconds, "…but the quiet figure stays visible (the incident's signal)")
	assert.Equal(t, 9000, *lapsedLease.AssigneeQuietSeconds)
	assert.Nil(t, noLease.AssigneeBusySeconds, "no lease at all: same TTL stance")
	require.NotNil(t, noLease.AssigneeQuietSeconds)
}

// TestQuiet_NilSourceInert: no seam wired (board disabled, pre-wiring
// boot) — every row renders unknown, never a fabricated figure.
func TestQuiet_NilSourceInert(t *testing.T) {
	s := &Service{}
	task := &Task{Stage: StageInProgress, AssigneeSession: "ses-x"}
	assert.NotPanics(t, func() { s.decorateAssigneeQuiet(context.Background(), task) })
	assert.Nil(t, task.AssigneeQuietSeconds)
	assert.Nil(t, task.AssigneeBusySeconds)
}
