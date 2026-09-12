// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package faultmatrix is the epic-71 / 2b assertion harness (#1312
// change item 2): fault-leg rows driven through the REAL sessionstate
// authority with harness fakes, asserting the S-invariants via
// violation counters and the L-bounds via convergence samples. This is
// the in-memory shape the soak row reuses at N workspaces × M sessions
// × fault rate λ — the same gates (violations empty, convergence within
// bound) scale from CI rows to the soak.
//
// Test scaffolding only: production code must not import it (the
// abitest boundary discipline).
package faultmatrix

import (
	"context"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// --- the engine ---------------------------------------------------------------

// Violations counts S/L breaches; a row ends with it empty or fails.
// The invariant naming (S1-S11, L1-L9 per #1312) stays at the call site
// where the semantics live — the counter is bookkeeping, not judgment.
type Violations struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewViolations() *Violations {
	return &Violations{counts: map[string]int{}}
}

func (v *Violations) Add(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.counts[name]++
}

// Counts returns a copy (assertion surface).
func (v *Violations) Counts() map[string]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]int, len(v.counts))
	for k, n := range v.counts {
		out[k] = n
	}
	return out
}

func (v *Violations) Empty() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.counts) == 0
}

// ConvergenceLog holds per-bound samples — the in-memory ancestor of
// the soak's convergence histograms (L1-L5).
type ConvergenceLog struct {
	mu      sync.Mutex
	samples map[string][]time.Duration
}

func NewConvergenceLog() *ConvergenceLog {
	return &ConvergenceLog{samples: map[string][]time.Duration{}}
}

func (l *ConvergenceLog) Record(bound string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples[bound] = append(l.samples[bound], d)
}

// Max returns the worst sample under a bound (0 when none).
func (l *ConvergenceLog) Max(bound string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var max time.Duration
	for _, d := range l.samples[bound] {
		if d > max {
			max = d
		}
	}
	return max
}

// Within reports whether every sample under the bound fits the budget.
// An UNRECORDED bound fails closed: a row that never measured has not
// converged within anything (review r1 — no vacuous L-gate passes).
func (l *ConvergenceLog) Within(bound string, budget time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	samples := l.samples[bound]
	if len(samples) == 0 {
		return false
	}
	for _, d := range samples {
		if d > budget {
			return false
		}
	}
	return true
}

// WaitConverges polls fn until it holds or the bound breaches; the
// elapsed span is the CALLER's to record (rows decide which span a
// bound governs). Returns (elapsed, ok) — a false return is the row's
// cue to add the L-violation.
func WaitConverges(ctx context.Context, bound time.Duration, poll time.Duration, fn func() bool) (time.Duration, bool) {
	start := time.Now()
	deadline := start.Add(bound)
	for {
		if fn() {
			// A success observed past the deadline is a breach, not a
			// convergence — ok=true with elapsed>bound would make the
			// two row gates disagree (review finding).
			if time.Now().After(deadline) {
				return time.Since(start), false
			}
			return time.Since(start), true
		}
		if time.Now().After(deadline) {
			return time.Since(start), false
		}
		select {
		case <-ctx.Done():
			return time.Since(start), false
		case <-time.After(poll):
		}
	}
}

// --- the harness fakes ----------------------------------------------------------

// NoopParser satisfies the required Parser seam; rows ingest typed
// events directly (IngestForTest) — nothing to parse.
type NoopParser struct{}

func (NoopParser) Parse(raw []byte) (*abiv1.Event, bool, error) { return nil, false, nil }

// EvidenceStore is the harness-store fake: session truth (status +
// pending set — the S5 diff's "live" side) and message presence (the
// S7 evidence). One store backs the StoreReader seam AND the row's
// fault injection — the coupling IS the point (truth and evidence read
// the same transcript, as in production).
type EvidenceStore struct {
	mu           sync.Mutex
	states       map[string]sessionstate.SessionSeed
	present      map[string]map[string]bool
	pendingCalls int
	// FailPendingInputs makes the gather error (the negative control:
	// an indeterminate truth source the diff must skip, never trust).
	FailPendingInputs bool
}

func NewEvidenceStore() *EvidenceStore {
	return &EvidenceStore{
		states:  map[string]sessionstate.SessionSeed{},
		present: map[string]map[string]bool{},
	}
}

// SetState overwrites a session's truth (status + pending set) — the
// row's fault lever for silent drops and status loss.
func (s *EvidenceStore) SetState(sessionID string, status abiv1.SessionStatus, pending ...*abiv1.InputRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[sessionID] = sessionstate.SessionSeed{Status: status, PendingInputs: pending}
}

// MarkPresent records a transcript message (turn evidence).
func (s *EvidenceStore) MarkPresent(sessionID, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.present[sessionID] == nil {
		s.present[sessionID] = map[string]bool{}
	}
	s.present[sessionID][messageID] = true
}

func (s *EvidenceStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]sessionstate.SessionSeed, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out, nil
}

// PendingInputsCalls returns the count of gather invocations — leg 9's
// cheapness observable (a serve storm must cost O(TTL windows), not
// O(serves)).
func (s *EvidenceStore) PendingInputsCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingCalls
}

// TranscriptCount reports how many distinct messages the session's
// transcript holds — S2's observable (≤1 user message per entry).
func (s *EvidenceStore) TranscriptCount(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.present[sessionID])
}

// PendingInputs serves the live ask registries keyed by session — the
// lease diff's truth source (STRICT semantics: a clean read or an
// error, never a fabricated empty).
func (s *EvidenceStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingCalls++
	if s.FailPendingInputs {
		return nil, errText("evidence store: pending gather failed (negative control)")
	}
	out := make(map[string][]*abiv1.InputRequest, len(s.states))
	for sid, seed := range s.states {
		if len(seed.PendingInputs) > 0 {
			out[sid] = seed.PendingInputs
		}
	}
	return out, nil
}

func (s *EvidenceStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		out[id] = s.present[sessionID][id]
	}
	return out, nil
}

// InputPresent reports the live pending truth for the answer path.
func (s *EvidenceStore) InputPresent(sessionID, inputID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.states[sessionID].PendingInputs {
		if in.GetId() == inputID {
			return true
		}
	}
	return false
}

// PendingInputs completes the StoreReader seam (#1310 slice B: the lease
// diff's truth source). #1337 landed the harness without it, leaving
// main's vet red (*EvidenceStore missing method) — this is the obvious
// projection of the same states the other readers serve: the fake's
// truth is in-memory, so the read is always clean and error stays nil
// (the STRICT-failure contract concerns real store fetches).
func (s *EvidenceStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]*abiv1.InputRequest, len(s.states))
	for sid, seed := range s.states {
		if len(seed.PendingInputs) > 0 {
			out[sid] = seed.PendingInputs
		}
	}
	return out, nil
}

// AnswerActor implements the harness answer seam: a live ask answers
// successfully; an ask ABSENT from the store's truth 404s — the leg-2
// stale-click shape resolve-by-absence must convert (S6).
type AnswerActor struct {
	Store *EvidenceStore
}

func (a *AnswerActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	switch act := req.GetAction().(type) {
	case *abiv1.ActionRequest_AnswerQuestion:
		if !a.Store.InputPresent(sessionID, act.AnswerQuestion.GetInputId()) {
			return nil, connect.NewError(connect.CodeNotFound, errText("input not found"))
		}
		return &abiv1.ActionResult{
			Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{
				InputId: act.AnswerQuestion.GetInputId(),
			}},
		}, nil
	default:
		return nil, connect.NewError(connect.CodeUnimplemented, errText("faultmatrix actor: answers only"))
	}
}

// InstantAdmitter models the production admission contract incl. 0a's
// entry-level idempotency: the passed messageID IS the harness-store
// dedupe key (attempt-independent), and the transcript write is a KEYED
// upsert — a re-admission of the same key overwrites, never appends
// (the #1315 sixteen-copies class). Evidence keyed by it is therefore
// S2's observable.
type InstantAdmitter struct {
	Out *EvidenceStore
}

func (ad *InstantAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	if ad.Out != nil {
		ad.Out.MarkPresent(sessionID, messageID)
	}
	return messageID, nil
}

type errText string

func (e errText) Error() string { return string(e) }
