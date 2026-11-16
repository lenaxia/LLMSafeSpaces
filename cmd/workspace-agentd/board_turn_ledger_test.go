// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_turn_ledger_test.go — #2061/#2064 story 1: the harness-agnostic
// emitter machinery's unit pins. The advance set is FROZEN here (the
// narrow ruling: record_progress | stage move | verdict — presence
// reads and heartbeats never advance), the stamp keys on the
// harness-attested lsp_injected_session ONLY (a model-declared
// sessionId copy never stamps — the #2040 evidence-poisoning guard),
// and the cross-process tail derivation (the pi leg's consult) honors
// the turn window.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBoardAdvanceTools_SetPin: the advance set is exactly the ruling's
// three names — no presence read, no heartbeat, no validate.
func TestBoardAdvanceTools_SetPin(t *testing.T) {
	for _, name := range []string{"board_record_progress", "board_move_stage", "board_record_verdict"} {
		assert.True(t, boardAdvanceTools[name], "%s must be in the advance set", name)
	}
	// The deliberately-excluded class, pinned by name: presence reads,
	// heartbeats, and the advisory validator never advance a claim.
	for _, name := range []string{
		"board_read", "board_list", "board_health", "board_validate_file",
		"board_heartbeat", "board_report_throttle", "board_depose_story",
	} {
		assert.False(t, boardAdvanceTools[name], "%s must NEVER be in the advance set (the keystone lesson)", name)
	}
}

// TestBoardAdvanceLedger_StampKeying: the stamp's identity discipline —
// lsp_injected_session only; a model-declared sessionId copy never
// stamps (the #2040 guard); non-advance tools never stamp.
func TestBoardAdvanceLedger_StampKeying(t *testing.T) {
	l := newBoardAdvanceLedger(filepath.Join(t.TempDir(), "adv.jsonl"), nil)

	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_attested"})
	assert.True(t, l.advancedSince("ses_attested", time.Time{}), "an attested advance call must stamp")

	// The #2040 guard: sessionId without the attested key is inert.
	l.observeToolCall("board_move_stage", map[string]any{"sessionId": "ses_declared"})
	assert.False(t, l.advancedSince("ses_declared", time.Time{}),
		"a model-declared sessionId copy must NEVER stamp the ledger")

	// Empty/whitespace attested identity is unattributable — inert.
	l.observeToolCall("board_record_verdict", map[string]any{"lsp_injected_session": "   "})
	l.observeToolCall("board_record_verdict", map[string]any{})

	// Non-advance board tools never stamp.
	l.observeToolCall("board_read", map[string]any{"lsp_injected_session": "ses_attested"})
	l.observeToolCall("board_health", map[string]any{"lsp_injected_session": "ses_attested"})
	assert.False(t, l.advancedSince("ses_other", time.Time{}))
}

// TestBoardAdvanceLedger_Window: the stamp honors the turn window — a
// stamp BEFORE the turn's busySince is a prior turn's advance, not this
// one's.
func TestBoardAdvanceLedger_Window(t *testing.T) {
	l := newBoardAdvanceLedger(filepath.Join(t.TempDir(), "adv.jsonl"), nil)
	before := time.Now()
	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_w"})
	turnStart := time.Now()

	assert.False(t, l.advancedSince("ses_w", turnStart),
		"a stamp before the window opened is a prior turn's advance")
	assert.True(t, l.advancedSince("ses_w", before),
		"the same stamp IS inside a window that opened earlier")
}

// TestBoardAdvanceLedger_TailRoundtrip: the shared-volume tail carries
// the stamp across processes — the pi leg consults it with a NIL memory
// ledger (a different agentd process on the pod). The tail's location
// resolves env-side (t.Setenv redirects), matching the same-scope
// posture of the reader.
func TestBoardAdvanceLedger_TailRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adv.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", path)
	l := newBoardAdvanceLedger(path, nil)
	turnStart := time.Now().Add(-time.Second)
	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_pi"})

	at, ok := readBoardAdvanceStamps("ses_pi")
	require.True(t, ok, "the tail must carry the stamp for the cross-process reader")
	assert.True(t, at.After(turnStart), "the tailed stamp must sit inside the window")

	// The pi shape: nil memory ledger, derivation from the tail alone.
	assert.True(t, sessionHoldsBoardAdvance(nil, "ses_pi", turnStart))
	assert.False(t, sessionHoldsBoardAdvance(nil, "ses_pi", time.Now().Add(time.Second)),
		"a window opening after the last stamp sees no advance")
	assert.False(t, sessionHoldsBoardAdvance(nil, "ses_nobody", turnStart))
}

// TestBoardAdvanceLedger_ZeroWindowSafeDirection: a zero turnStart (no
// window anchor) fails toward "advance seen" — the safe direction (the
// board due-marks; the sweep re-checks), pinned.
func TestBoardAdvanceLedger_ZeroWindowSafeDirection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adv.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", path)
	newBoardAdvanceLedger(path, nil).observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_z"})
	assert.True(t, sessionHoldsBoardAdvance(nil, "ses_z", time.Time{}))
	assert.False(t, sessionHoldsBoardAdvance(nil, "ses_absent", time.Time{}))
}

// TestBoardAdvanceLedger_UnwritableTailMemoryOnly: a tail that cannot
// be opened degrades to memory-only — the stamp still lands, loudly.
func TestBoardAdvanceLedger_UnwritableTailMemoryOnly(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("a file, not a dir"), 0o600))
	l := newBoardAdvanceLedger(filepath.Join(blocker, "adv.jsonl"), nil)
	assert.NotNil(t, l, "construction never fails — memory-only degrade")
	assert.Nil(t, l.appnd, "no tail handle in the degraded shape")

	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_deg"})
	assert.True(t, l.advancedSince("ses_deg", time.Time{}), "memory holds the stamp without the tail")
}

// TestBoardTurnEmitter_Facts: the probe backend composes the registry
// and the ledger — claim-holder first (a user chat answers false/false
// without a ledger consult), the advance flag strictly inside the
// claim-holder arm.
func TestBoardTurnEmitter_Facts(t *testing.T) {
	dir := t.TempDir()
	reg := newBoardWorkerRegistry(filepath.Join(dir, "workers.jsonl"))
	reg.record("ses_worker")
	em := newBoardTurnEmitter(reg, newBoardAdvanceLedger(filepath.Join(dir, "adv.jsonl"), nil))

	turnStart := time.Now().Add(time.Millisecond)
	time.Sleep(2 * time.Millisecond)
	em.ledger.observeToolCall("board_record_verdict", map[string]any{"lsp_injected_session": "ses_worker"})

	f := em.facts("ses_user", turnStart)
	assert.False(t, f.ClaimHolder)
	assert.False(t, f.Advanced)

	f = em.facts("ses_worker", turnStart)
	assert.True(t, f.ClaimHolder)
	assert.True(t, f.Advanced, "the stamp sits inside the window")

	f = em.facts("ses_worker", time.Now().Add(time.Second))
	assert.True(t, f.ClaimHolder)
	assert.False(t, f.Advanced, "a window opening after the stamp sees no advance")
}

// TestBoardTurnEmitter_NilRegistryDegraded: a nil registry (seam
// absent) answers claim-holder=false — nothing emits, sweep semantics.
func TestBoardTurnEmitter_NilRegistryDegraded(t *testing.T) {
	em := newBoardTurnEmitter(nil, newBoardAdvanceLedger(filepath.Join(t.TempDir(), "adv.jsonl"), nil))
	f := em.facts("ses_anyone", time.Time{})
	assert.False(t, f.ClaimHolder)
	assert.False(t, f.Advanced)
}

// TestBoardTurnEmitter_EpochPerSession: the ordinal is per-session —
// two workers count independently from 1.
func TestBoardTurnEmitter_EpochPerSession(t *testing.T) {
	em := newBoardTurnEmitter(nil, nil)
	assert.EqualValues(t, 1, em.nextEpoch("ses_a"))
	assert.EqualValues(t, 2, em.nextEpoch("ses_a"))
	assert.EqualValues(t, 1, em.nextEpoch("ses_b"))
}

// TestBoardAdvanceLedger_TailEntryShape: the tail line is the exact
// JSONL contract the pi reader parses (session_id + advanced_at,
// RFC3339Nano) — the cross-process shape, frozen.
func TestBoardAdvanceLedger_TailEntryShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adv.jsonl")
	newBoardAdvanceLedger(path, nil).observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_shape"})
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var rec struct {
		SessionID  string `json:"session_id"`
		AdvancedAt string `json:"advanced_at"`
	}
	require.NoError(t, json.Unmarshal(b[:len(b)-1], &rec)) // drop '\n'
	assert.Equal(t, "ses_shape", rec.SessionID)
	_, err = time.Parse(time.RFC3339Nano, rec.AdvancedAt)
	assert.NoError(t, err, "advanced_at must be RFC3339Nano")
}

// tailBoundedBase is the seed stamps' epoch (disjoint from wall-clock
// time so assertions parse exactly what the test wrote).
var tailBoundedBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedAdvanceTail writes n stamp lines for session with strictly
// increasing timestamps base+1ms .. base+nms (line i carries marker i).
func seedAdvanceTail(t *testing.T, path, session string, n int) {
	t.Helper()
	var buf []byte
	for i := 1; i <= n; i++ {
		b, err := json.Marshal(map[string]any{
			"session_id":  session,
			"advanced_at": tailBoundedBase.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
		})
		require.NoError(t, err)
		buf = append(buf, append(b, '\n')...)
	}
	require.NoError(t, os.WriteFile(path, buf, 0o600))
}

// tailStampAt reads the idx-th tail line's advanced_at (raw, for the
// kept-window assertion).
func tailStampAt(t *testing.T, path string, idx int) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	require.Greater(t, len(lines), idx)
	var rec struct {
		AdvancedAt string `json:"advanced_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(lines[idx]), &rec))
	return rec.AdvancedAt
}

// TestBoardAdvanceLedger_TailBounded: past the keep bound the tail
// rewrites keeping the newest boardAdvancesKeepLines (temp + rename),
// the append handle survives the swap (re-open after rename — the next
// stamp lands on the compacted file), and the cross-process reader
// still derives from the kept tail (mirrors the pi journal's
// TestPiBoardTurnEnd_CompactionKeepsTail).
func TestBoardAdvanceLedger_TailBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adv.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", path)
	seedAdvanceTail(t, path, "ses_seed", boardAdvancesKeepLines+5)

	l := newBoardAdvanceLedger(path, nil)
	require.NotNil(t, l.appnd, "the seeded tail must open")
	l.mu.Lock()
	l.compactTailLocked()
	l.mu.Unlock()

	require.Equal(t, boardAdvancesKeepLines, countFileLines(path), "past the bound the tail rewrites keeping the newest 1000")
	assert.Equal(t, tailBoundedBase.Add(6*time.Millisecond).Format(time.RFC3339Nano), tailStampAt(t, path, 0),
		"the newest 1000 (stamps 6..1005) survive")
	assert.Equal(t, tailBoundedBase.Add(time.Duration(boardAdvancesKeepLines+5)*time.Millisecond).Format(time.RFC3339Nano),
		tailStampAt(t, path, boardAdvancesKeepLines-1))

	// The append handle survived the rename swap: the next stamp lands
	// on the compacted file (the re-open pin — an orphaned handle would
	// append into the renamed-away inode and the reader would miss it).
	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_after"})
	require.Equal(t, boardAdvancesKeepLines+1, countFileLines(path))
	_, ok := readBoardAdvanceStamps("ses_after")
	assert.True(t, ok, "a stamp appended after compaction is readable by the cross-process reader")
}

// TestBoardAdvanceLedger_TailAutoCompacts: the bound fires from the
// append path itself — one stamp past boardAdvancesCompactLines
// compacts (pre-existing lines count: the basis is seeded at
// construction), the fresh stamp survives inside the kept window, and
// both derivations (memory, tail) still answer.
func TestBoardAdvanceLedger_TailAutoCompacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adv.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", path)
	seedAdvanceTail(t, path, "ses_seed", boardAdvancesCompactLines+5)

	l := newBoardAdvanceLedger(path, nil) // counts the seeded basis
	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_live"})

	require.Equal(t, boardAdvancesKeepLines, countFileLines(path),
		"the append past the bound compacts the tail back to the keep window")
	assert.True(t, l.advancedSince("ses_live", time.Time{}), "memory holds the fresh stamp regardless")

	at, ok := readBoardAdvanceStamps("ses_live")
	require.True(t, ok, "the fresh stamp sits inside the kept window — the cross-process reader finds it")
	assert.True(t, at.After(tailBoundedBase))

	// The newest pre-existing stamp survived the rewrite too.
	seedAt, ok := readBoardAdvanceStamps("ses_seed")
	require.True(t, ok)
	assert.Equal(t, tailBoundedBase.Add(time.Duration(boardAdvancesCompactLines+5)*time.Millisecond).Format(time.RFC3339Nano),
		seedAt.UTC().Format(time.RFC3339Nano))
}
