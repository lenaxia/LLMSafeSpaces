// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// turn_resume_1732_test.go — #1732 leg A2 unit pins: the
// abort-classification state machine. The wire contract underneath is
// pinned in pkg/agent/opencode/wire/turn_end_1732_test.go (probe-captured
// fixtures); these pins hold the DECISION layer:
//
//   - roll-classified loss (finish unknown, no session.error) → ONE
//     bounded re-prompt fires;
//   - user abort (MessageAbortedError anywhere near the turn) → NEVER
//     resumes — the amendment's hard rule;
//   - cooldown + hourly budget bound a persistently-broken provider
//     path (no hot loop);
//   - a user message newer than the interruption (human takeover, via
//     event reordering on a recycled SSE connection) cancels;
//   - clean turns (finish stop) never resume.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeResumePrompter struct {
	mu     sync.Mutex
	promts []string // session IDs prompted, in order
	err    error
}

func (f *fakeResumePrompter) ResumeTurn(_ context.Context, sessionID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promts = append(f.promts, sessionID)
	return f.err
}

func (f *fakeResumePrompter) count(sessionID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.promts {
		if s == sessionID {
			n++
		}
	}
	return n
}

// turnEventFeed drives the supervisor with probe-shaped wire events.
func turnEventFeed(t *testing.T, s *turnResumeSupervisor, events ...string) {
	t.Helper()
	for _, raw := range events {
		var env struct {
			Type       string          `json:"type"`
			Properties json.RawMessage `json:"properties"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &env))
		s.Observe(env.Type, env.Properties)
	}
}

const (
	evUserMsg1732 = `{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_u","role":"user","sessionID":"ses_a","time":{"created":100}}}}`
	evCut1732     = `{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_x","role":"assistant","time":{"created":200,"completed":300},"finish":"unknown"}}}`
	evStop1732    = `{"type":"message.updated","properties":{"sessionID":"ses_a","info":{"id":"msg_o","role":"assistant","time":{"created":200,"completed":300},"finish":"stop"}}}`
	evAbort1732   = `{"type":"session.error","properties":{"sessionID":"ses_a","name":"MessageAbortedError","data":{"message":"Aborted"}}}`
)

func Test1732_TurnResume_RollClassifiedLossFiresOneResume(t *testing.T) {
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	turnEventFeed(t, s, evUserMsg1732, evCut1732)
	assert.Equal(t, 1, p.count("ses_a"), "finish-unknown with no session.error must fire exactly one resume")
	// The same interrupted turn's re-delivered upsert (SSE recycle
	// replay) must not fire again.
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, 1, p.count("ses_a"), "the resume must not double-fire on replayed upserts (cooldown)")
}

func Test1732_TurnResume_UserAbortNeverResumes(t *testing.T) {
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	// The abort marker BEFORE the finish (the #1722-proven order — the
	// error lands first, then the terminal upsert).
	turnEventFeed(t, s, evUserMsg1732, evAbort1732, evCut1732)
	assert.Equal(t, 0, p.count("ses_a"), "a user-aborted turn must NEVER auto-resume")
}

func Test1732_TurnResume_AbortRecencyBlocksLaterInterruptedTurns(t *testing.T) {
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	turnEventFeed(t, s, evAbort1732)
	// Within the recency window: even a LATER finish-unknown stays
	// blocked (conservative — the session just errored explicitly).
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, 0, p.count("ses_a"))
}

func Test1732_TurnResume_CleanTurnNeverResumes(t *testing.T) {
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	turnEventFeed(t, s, evUserMsg1732, evStop1732)
	assert.Equal(t, 0, p.count("ses_a"), "finish stop is a clean end — nothing to resume")
}

func Test1732_TurnResume_HourlyBudgetBoundsHotLoop(t *testing.T) {
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	// A persistently-broken provider: every resumed turn is cut again.
	// The cooldown is 10m; drive the clock manually.
	now := time.Now()
	s.now = func() time.Time { return now }
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, 1, p.count("ses_a"))

	for i := 2; i <= ResumeHourlyCap; i++ {
		now = now.Add(ResumeCooldown + time.Second)
		turnEventFeed(t, s, evCut1732)
	}
	assert.Equal(t, ResumeHourlyCap, p.count("ses_a"), "budget admits exactly ResumeHourlyCap resumes")

	now = now.Add(ResumeCooldown + time.Second)
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, ResumeHourlyCap, p.count("ses_a"), "the hourly cap holds — no hot loop against a dead provider")

	// The budget re-arms as the ledger ages out.
	now = now.Add(time.Hour + time.Minute)
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, ResumeHourlyCap+1, p.count("ses_a"), "the rolling-hour ledger ages out")
}

func Test1732_TurnResume_NilPrompterDisabled(t *testing.T) {
	s := newTurnResumeSupervisor(nil)
	require.NotPanics(t, func() { turnEventFeed(t, s, evCut1732) })
}

func Test1732_TurnResume_HumanTakeoverCancelsPendingEligibility(t *testing.T) {
	// Defense-in-depth gate (event reordering on a recycled SSE
	// connection can deliver a user message with a stale clock): a
	// user-message stamp NEWER than the interrupted turn's completion
	// means the human took over — the session must not be eligible.
	p := &fakeResumePrompter{}
	s := newTurnResumeSupervisor(p)
	now := time.Now()
	s.now = func() time.Time { return now }
	turnEventFeed(t, s, evCut1732)
	require.Equal(t, 1, p.count("ses_a"), "sanity: the interrupted turn resumed")

	now = now.Add(ResumeCooldown + time.Minute)
	s.mu.Lock()
	st := s.stateFor("ses_a")
	st.lastInterruptedAt = now.Add(-time.Second)      // interrupted at T
	st.lastUserMessageAt = now.Add(-time.Millisecond) // user re-engaged at T+ε
	eligible := s.eligibleLocked(st, now.Add(time.Second))
	s.mu.Unlock()
	assert.False(t, eligible, "a user message newer than the interruption cancels eligibility")
}
