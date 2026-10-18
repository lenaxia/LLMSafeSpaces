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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

// Test1732_TurnResume_PromptFailureKeepsSessionRetryable is the
// unhappy-path gate for the resume-action layer (r1 blocking finding):
// the ledger stamps ONLY on a successful prompt — a FAILED resume must
// consume no budget, no cooldown, and no metric, so a wedged harness
// stays eligible and the next interrupted turn retries. A refactor that
// stamped the ledger before the error check would silently convert a
// wedged harness into "already resumed, never retried" — the precise
// failure class #1732 exists to fix — and fail this pin.
//
// Documented bound (review r1, Robustness): failed resumes consume no
// budget and carry no backoff of their own; repeated attempts are
// bounded only by interrupted-turn cadence (each retry needs a fresh
// finish-unknown, i.e. a fresh turn that streamed and died).
func Test1732_TurnResume_PromptFailureKeepsSessionRetryable(t *testing.T) {
	// Deterministic metric label: RecordTurnResume maps the pod's
	// WORKSPACE_ID through to the counter label ("" → "unknown").
	t.Setenv("WORKSPACE_ID", "")
	resumeMetric := func() float64 {
		return testutil.ToFloat64(pkgOpsMetrics.turnResumes.WithLabelValues("unknown"))
	}

	p := &fakeResumePrompter{err: errors.New("harness wedged: connection refused")}
	s := newTurnResumeSupervisor(p)
	now := time.Now()
	s.now = func() time.Time { return now }

	metricBefore := resumeMetric()
	turnEventFeed(t, s, evUserMsg1732, evCut1732)
	require.Equal(t, 1, p.count("ses_a"), "sanity: the resume prompt was attempted")

	// (a) No ledger stamp: neither the cooldown clock nor the budget
	// ledger moved — the failed attempt consumed nothing.
	s.mu.Lock()
	st := s.stateFor("ses_a")
	ledgerEmpty := st.lastResumeAt.IsZero() && len(st.resumes) == 0
	s.mu.Unlock()
	require.True(t, ledgerEmpty, "a failed prompt must not stamp the resume ledger (no cooldown, no budget)")

	// (b) No metric increment: only FIRED resumes count.
	assert.Equal(t, metricBefore, resumeMetric(), "a failed prompt must not increment workspace_agentd_turn_resumes_total")

	// (c) Still eligible: the harness recovers, the NEXT interrupted
	// turn retries WITHOUT waiting out the 10-minute cooldown — well
	// under it.
	now = now.Add(2 * time.Second)
	p.err = nil
	turnEventFeed(t, s, evCut1732)
	assert.Equal(t, 2, p.count("ses_a"),
		"the failed resume consumed no budget — the next interrupted turn must re-fire immediately (the retryability property)")

	// The successful retry stamps the ledger AND the metric.
	assert.Greater(t, resumeMetric(), metricBefore,
		"the fired resume increments workspace_agentd_turn_resumes_total{workspace_id}")
	s.mu.Lock()
	st = s.stateFor("ses_a")
	stamped := !st.lastResumeAt.IsZero() && len(st.resumes) == 1
	s.mu.Unlock()
	assert.True(t, stamped, "the successful retry stamps the ledger exactly once")
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
