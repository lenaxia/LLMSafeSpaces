// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_board_turnend_test.go — #2061/#2064 story 1, the pi leg's pins.
// The emission site is the pump's HARNESS-ATTESTED fold only: the
// aggregator's ApplyEvent busy→settled transition (agent_settled — the
// one idle transition per the settle contract). The corroboration poll
// (applyStatePoll) reconciles the cell but NEVER emits — the same
// anti-#2040 rule the opencode leg pins on its synthetic settle paths.
// The journal (env-redirected per test) is the emission artifact:
// claim-holder-filtered, epoch-ordinaled, window-correct.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// piTurnEndEnv redirects the three shared-volume paths to a temp root
// and seeds the worker registry with one claim-holder, returning the
// journal + advances paths.
func piTurnEndEnv(t *testing.T) (journal, advances, workers string) {
	t.Helper()
	dir := t.TempDir()
	journal = filepath.Join(dir, "turnends.jsonl")
	advances = filepath.Join(dir, "advances.jsonl")
	workers = filepath.Join(dir, "workers.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_TURNENDS_PATH", journal)
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", advances)
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", workers)
	require.NoError(t, os.WriteFile(workers, []byte("{\"session_id\":\"ses_pw\"}\n"), 0o600))
	return journal, advances, workers
}

// readJournal decodes the journal lines.
func readJournal(t *testing.T, path string) []boardTurnEndEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []boardTurnEndEntry
	for _, line := range splitLines(b) {
		var e boardTurnEndEntry
		require.NoError(t, json.Unmarshal(line, &e))
		out = append(out, e)
	}
	return out
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// writeAdvanceStamp APPENDS one advance-tail line for the session at
// the given instant (the tail is append-only; a truncating write would
// erase the prior stamps the window test composes).
func writeAdvanceStamp(t *testing.T, path, sid string, at time.Time) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"session_id": sid, "advanced_at": at.UTC().Format(time.RFC3339Nano)})
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	_, err = f.Write(append(b, '\n'))
	require.NoError(t, err)
}

// TestPiBoardTurnEnd_UserSessionNeverJournals: R1 — a session absent
// from the worker registry (a user chat) never journals.
func TestPiBoardTurnEnd_UserSessionNeverJournals(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)

	piBoardTurnEndObserver("ses_user", time.Now().Add(-time.Second), time.Now())

	assert.Empty(t, readJournal(t, journal), "a user session must never journal a turn end")
}

// TestPiBoardTurnEnd_WorkerNoAdvance: the no-advance arm —
// advanced=false, epoch=1, positive duration, raw session id.
func TestPiBoardTurnEnd_WorkerNoAdvance(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)
	busySince := time.Now().Add(-50 * time.Millisecond)

	piBoardTurnEndObserver("ses_pw", busySince, time.Now())

	entries := readJournal(t, journal)
	require.Len(t, entries, 1)
	e := entries[0]
	assert.Equal(t, "ses_pw", e.SessionID)
	assert.False(t, e.Advanced)
	assert.EqualValues(t, 1, e.Epoch)
	assert.Greater(t, e.TurnDurationMs, int64(0))
	assert.NotEmpty(t, e.SettledAt)
	assert.NotEmpty(t, e.TurnStartedAt)
}

// TestPiBoardTurnEnd_AdvanceWindow: the advanced flag honors the turn
// window — a stamp inside the window advances; one before it does not.
func TestPiBoardTurnEnd_AdvanceWindow(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, advances, _ := piTurnEndEnv(t)
	busySince := time.Now().Add(-2 * time.Second)

	// Before the window: a prior turn's advance.
	writeAdvanceStamp(t, advances, "ses_pw", busySince.Add(-time.Second))
	// Inside the window.
	writeAdvanceStamp(t, advances, "ses_pw", busySince.Add(time.Second))
	// A different session's stamp — inert.
	writeAdvanceStamp(t, advances, "ses_other", busySince.Add(2*time.Second))

	piBoardTurnEndObserver("ses_pw", busySince, time.Now())

	entries := readJournal(t, journal)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].Advanced, "the in-window stamp must advance the turn")
}

// TestPiBoardTurnEnd_AdvanceBeforeWindowOnly: only a stale stamp
// exists — advanced=false (window discipline, tail side).
func TestPiBoardTurnEnd_AdvanceBeforeWindowOnly(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, advances, _ := piTurnEndEnv(t)
	busySince := time.Now()
	writeAdvanceStamp(t, advances, "ses_pw", busySince.Add(-time.Minute))

	piBoardTurnEndObserver("ses_pw", busySince, time.Now().Add(time.Second))

	entries := readJournal(t, journal)
	require.Len(t, entries, 1)
	assert.False(t, entries[0].Advanced)
}

// TestPiBoardTurnEnd_EpochFromJournalCount: the epoch is the journal's
// per-session line count + 1 — restart-stable, monotone.
func TestPiBoardTurnEnd_EpochFromJournalCount(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)

	now := time.Now()
	piBoardTurnEndObserver("ses_pw", now.Add(-time.Second), now)
	piBoardTurnEndObserver("ses_pw", now.Add(-2*time.Second), now.Add(time.Second))

	entries := readJournal(t, journal)
	require.Len(t, entries, 2)
	assert.EqualValues(t, 1, entries[0].Epoch)
	assert.EqualValues(t, 2, entries[1].Epoch)
}

// TestPiBoardTurnEnd_ZeroBusySince: a settle without a busy anchor (a
// restart gap) journals duration=0 — never negative, never invented.
func TestPiBoardTurnEnd_ZeroBusySince(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)

	piBoardTurnEndObserver("ses_pw", time.Time{}, time.Now())

	entries := readJournal(t, journal)
	require.Len(t, entries, 1)
	assert.Zero(t, entries[0].TurnDurationMs)
}

// TestPiBoardTurnEnd_AppendFailureCounted: an unwritable journal (a
// directory in the way) is contained — no panic, the failure counted.
func TestPiBoardTurnEnd_AppendFailureCounted(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)
	require.NoError(t, os.Mkdir(journal, 0o750)) // the journal path is a directory
	before := boardTurnEndPiFails.Load()

	require.NotPanics(t, func() {
		piBoardTurnEndObserver("ses_pw", time.Now().Add(-time.Second), time.Now())
	})
	assert.Equal(t, before+1, boardTurnEndPiFails.Load(), "the lost emission must be counted")
}

// TestPiBoardTurnEnd_CompactionKeepsTail: past the bound, the journal
// rewrites keeping the newest 1000 lines (temp + rename).
func TestPiBoardTurnEnd_CompactionKeepsTail(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	journal, _, _ := piTurnEndEnv(t)
	var lines []byte
	for i := 0; i < 1005; i++ {
		b, _ := json.Marshal(boardTurnEndEntry{SessionID: "ses_pw", Epoch: uint64(i + 1)})
		lines = append(lines, append(b, '\n')...)
	}
	require.NoError(t, os.WriteFile(journal, lines, 0o600))

	require.NoError(t, compactJournalTail()) // the journal path resolves env-side (piTurnEndEnv)

	kept := readJournal(t, journal)
	require.Len(t, kept, 1000)
	assert.EqualValues(t, 6, kept[0].Epoch, "the newest 1000 (epochs 6..1005) survive")
	assert.EqualValues(t, 1005, kept[len(kept)-1].Epoch)
}

// TestPiBoardTurnEnd_EmissionSiteIsTheAttestedFold: THE pi-leg
// emission-site pin. The aggregator's ApplyEvent busy→settled fires the
// observer exactly once per transition; the corroboration poll settles
// the cell identically and NEVER fires it.
func TestPiBoardTurnEnd_EmissionSiteIsTheAttestedFold(t *testing.T) {
	resetBoardTransportV2(t, false) // the v1 transport's world (the flag selects; these pins own the off-half)
	agg := newPiStateAggregator()
	var fired []string
	var busySinceSeen []time.Time
	agg.setTurnEndObserver(func(sessionID string, busySince, settledAt time.Time) {
		fired = append(fired, sessionID)
		busySinceSeen = append(busySinceSeen, busySince)
	})

	t0 := time.Now()
	agg.ApplyEvent("ses_pi", &piRecord{Type: "agent_start"}, nil, t0)
	require.Empty(t, fired, "a turn START is not a turn end")

	t1 := t0.Add(3 * time.Second)
	agg.ApplyEvent("ses_pi", &piRecord{Type: "agent_settled"}, nil, t1)
	require.Len(t, fired, 1, "the attested settle must fire the observer exactly once")
	assert.Equal(t, "ses_pi", fired[0])
	assert.Equal(t, t0, busySinceSeen[0], "the observer receives the busy anchor")

	// A duplicate settle (already idle) is not a transition.
	agg.ApplyEvent("ses_pi", &piRecord{Type: "agent_settled"}, nil, t1.Add(time.Second))
	assert.Len(t, fired, 1)

	// The anti-#2040 arm: busy again, then settled by the POLL —
	// the cell folds to idle, the observer stays silent.
	t2 := t1.Add(10 * time.Second)
	agg.ApplyEvent("ses_pi", &piRecord{Type: "agent_start"}, nil, t2)
	poll, err := json.Marshal(map[string]any{"sessionId": "ses_pi", "isStreaming": false})
	require.NoError(t, err)
	agg.ApplyStatePoll("ses_pi", poll, t2.Add(5*time.Second))
	assert.Len(t, fired, 1,
		"a poll-reconciled settle is NOT a harness-attested turn end — it must never emit")
}
