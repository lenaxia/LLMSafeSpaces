// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// turn_resume.go — #1732 leg A2: the infra-loss abort-classification
// and ONE bounded auto-resume of an interrupted turn.
//
// The wire-proven asymmetry this component owns (real opencode 1.18.10
// probe, #1732; abort side from the #1722 probe):
//
//   - USER ABORT  emits session.error {name: "MessageAbortedError"}
//     BEFORE the idle burst — an EVENT;
//   - INFRA CUT   (provider stream severed mid-generation — the
//     relay-router/api-roll halt mechanism) emits NO session.error;
//     the assistant message completes with finish "unknown" and the
//     session settles idle cleanly — a FINISH.
//
// Therefore: finish "unknown" on a completed assistant turn, with no
// session.error observed for the session, is ROLL-CLASSIFIED loss —
// never a user abort. The supervisor re-prompts the session ONCE
// (bounded per-session budget + cooldown), never for user aborts, and
// never when the user has already re-engaged (a user message newer than
// the interrupted turn means the human took over).

import (
	"context"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode/wire"
)

// TurnResumeText is the single re-prompt sent on roll-classified loss.
// It lands as a user-visible message by design: the resume must be
// auditable in the transcript (the owner's amendment: infra-loss must
// never masquerade as silent user action).
const TurnResumeText = "[auto-resume] The previous turn was interrupted by a platform infrastructure event (not a user action). Please continue from where the response left off."

// Turn-resume bounds. The budget shape (per-session hourly cap + hard
// cooldown) guarantees a persistently-broken provider path can never
// hot-loop resumes: at most ResumeCooldown between attempts and at most
// ResumeHourlyCap attempts per rolling hour per session.
const (
	// ResumeCooldown is the minimum spacing between auto-resumes of one
	// session.
	ResumeCooldown = 10 * time.Minute
	// ResumeHourlyCap bounds auto-resumes per session per rolling hour.
	ResumeHourlyCap = 3
	// ErrorRecencyWindow: a session.error newer than the interrupted
	// turn's completion marks the turn user-aborted (or errored) — never
	// resumed. The window's width only needs to cover event reordering
	// on the wire (the abort burst's ~30ms, generously bounded).
	ErrorRecencyWindow = 5 * time.Minute
)

// turnResumePrompter is the action seam (OpenCodeClient satisfies it;
// tests fake it).
type turnResumePrompter interface {
	ResumeTurn(ctx context.Context, sessionID, text string) error
}

// turnResumeSupervisor classifies turn ends off the tracker's raw event
// feed and fires the bounded auto-resume.
type turnResumeSupervisor struct {
	// prompt executes the resume. Nil disables the supervisor (bare
	// tracker behavior — pre-#1732 constructions).
	prompt turnResumePrompter
	// now is the clock seam (tests).
	now func() time.Time

	mu    sync.Mutex
	state map[string]*turnResumeSession
}

type turnResumeSession struct {
	// lastErrorAt is the last session.error observed for the session.
	lastErrorAt time.Time
	// lastUserMessageAt is the last user-sent message stamp.
	lastUserMessageAt time.Time
	// lastInterruptedAt is the last finish-unknown completion seen.
	lastInterruptedAt time.Time
	// lastResumeAt stamps fired resumes; resumes[] is the rolling-hour
	// budget ledger.
	lastResumeAt time.Time
	resumes      []time.Time
}

func newTurnResumeSupervisor(prompt turnResumePrompter) *turnResumeSupervisor {
	return &turnResumeSupervisor{
		prompt: prompt,
		now:    time.Now,
		state:  map[string]*turnResumeSession{},
	}
}

// Observe classifies one raw /event payload (the tracker calls this in
// processEvent, before dialect parsing — same position as onRawEvent).
// Fires the resume SYNCHRONOUSLY when the classification lands: the
// tracker's read loop tolerates a bounded HTTP call here (the prompt is
// admit-and-schedule, milliseconds).
//
// Branch order matters: an assistant turn-end upsert ALSO matches
// ParseMessageLevelProgress's LLM-completed leg, so the turn-end parse
// must consume assistant completions first; the progress branch then
// sees user-sent messages in practice (the user-takeover clock).
func (s *turnResumeSupervisor) Observe(eventType string, props []byte) {
	if s == nil || s.prompt == nil {
		return
	}

	// The interrupted-turn finish (assistant terminal upserts only).
	if te, ok := wire.ParseAssistantTurnEnd(eventType, props); ok {
		if te.Finish != wire.TurnFinishUnknown {
			return
		}
		s.mu.Lock()
		st := s.stateFor(te.SessionID)
		st.lastInterruptedAt = s.now()
		now := s.now()
		eligible := s.eligibleLocked(st, now)
		s.mu.Unlock()
		if eligible {
			s.resume(te.SessionID)
		}
		return
	}

	// The abort marker: any session.error recency kills resume
	// eligibility for the session's interrupted turns.
	if se, ok := wire.ParseSessionError(eventType, props); ok {
		s.mu.Lock()
		st := s.stateFor(se.SessionID)
		st.lastErrorAt = s.now()
		s.mu.Unlock()
		return
	}

	// User re-engagement: a user-sent message (the #1685 progress
	// definition's user leg) newer than a pending interruption cancels
	// the resume — the human took over.
	if sid, ok := wire.ParseMessageLevelProgress(eventType, props); ok {
		s.markUserMessage(sid)
	}
}

// markUserMessage stamps the session's user-message clock.
func (s *turnResumeSupervisor) markUserMessage(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	st := s.stateFor(sessionID)
	st.lastUserMessageAt = s.now()
	s.mu.Unlock()
}

// eligibleLocked applies the full gate stack. Caller holds s.mu.
func (s *turnResumeSupervisor) eligibleLocked(st *turnResumeSession, now time.Time) bool {
	// Abort classification: a session.error within the recency window
	// means the turn's end was user-initiated (or explicitly errored) —
	// the #1732 amendment's hard rule: infra-loss must never count as
	// user abort, and user abort must never auto-resume.
	if !st.lastErrorAt.IsZero() && now.Sub(st.lastErrorAt) < ErrorRecencyWindow {
		return false
	}
	// Human takeover: a user message after the interrupted turn ended.
	if st.lastUserMessageAt.After(st.lastInterruptedAt) && !st.lastUserMessageAt.IsZero() {
		return false
	}
	// Cooldown + hourly budget.
	if !st.lastResumeAt.IsZero() && now.Sub(st.lastResumeAt) < ResumeCooldown {
		return false
	}
	kept := st.resumes[:0]
	for _, at := range st.resumes {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	st.resumes = kept
	return len(st.resumes) < ResumeHourlyCap
}

// resume fires the single bounded re-prompt. SKELETON (#1732 pins
// commit): the classification and gates are real; the action is inert
// and lands with the fix commit.
func (s *turnResumeSupervisor) resume(sessionID string) {
}

func (s *turnResumeSupervisor) stateFor(sessionID string) *turnResumeSession {
	st, ok := s.state[sessionID]
	if !ok {
		st = &turnResumeSession{}
		s.state[sessionID] = st
	}
	return st
}
