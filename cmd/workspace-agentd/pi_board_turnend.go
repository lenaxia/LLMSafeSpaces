// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_board_turnend.go — #2061/#2064 story 1, the pi leg: the board
// turn-end emission at the pump's harness-attested settle fold
// (pi_state.go's ApplyEvent → agent_settled — transport truth from the
// pi child; the same emission-site rule as the opencode leg's
// applyHarnessEventLocked, and the same payload the authority
// publishes there). No opencode-only branch (audit ruling 5).
//
// Topology note (why this leg differs mechanically): a pi pod runs TWO
// agentd processes — the sidecar (serves MCP, stamps the advance
// ledger, spawns workers) and supervise-pi (this process, folds the
// child records). The shared /workspace volume carries the three facts
// across the process boundary as append-only JSONL (the
// board_workers.go pattern): the worker registry (sidecar appends at
// spawn), the advance tail (sidecar appends at the MCP choke point),
// and the turn-end journal (THIS process appends at the attested
// settle). The journal IS the emission artifact: the serialized event,
// claim-holder-filtered, ready for the board consumer (story #2065
// wires the consumer; until then the artifact journals and the sweep
// backstop owns behavior verbatim — epic R5, the accelerator-never-
// dependency posture).
//
// Degradation: any unreadable/unwritable file skips the emission
// (counted, loud-once) — never disturbs the pump's fold (R4).

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const boardTurnEndsDefaultPath = "/workspace/.opencode-board-turnends"

// boardTurnEndsPath resolves the turn-end journal's location (the
// markerPathFromEnv pattern).
func boardTurnEndsPath() string {
	if p := os.Getenv("LLMSAFESPACES_BOARD_TURNENDS_PATH"); p != "" {
		return p
	}
	return boardTurnEndsDefaultPath
}

// boardTurnEndJournalCompactLines bounds the journal before a
// compaction rewrite (keeping the tail). Compaction resets the epoch
// basis for old sessions — acceptable: the epoch is an emitter-side
// ordinal (per-session consecutive-no-advance / dedupe key), and the
// opencode leg's process-lifetime counter resets on restart by the
// same token (the consumer dedupes on (session, settled_at), never on
// epoch alone).
const boardTurnEndJournalCompactLines = 10000

// piBoardTurnEndObserver is the aggregator hook: one harness-attested
// busy→settled transition. Best-effort, recover-walled (a fault here
// never touches the pump), claim-holder-filtered, journaled.
func piBoardTurnEndObserver(sessionID string, busySince, settledAt time.Time) {
	defer func() {
		if r := recover(); r != nil {
			boardTurnEndPiFails.Add(1)
			log.Error("pi board turn-end: emission contained after panic", zap.Any("panic", r))
		}
	}()
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	if !sessionIsBoardWorkerViaFile(boardWorkersPath(), sid) {
		return // claim-holders only — user chats never emit (R1)
	}
	advanced := sessionHoldsBoardAdvance(nil, boardAdvancesPath(), sid, busySince)
	epoch := countBoardTurnEnds(boardTurnEndsPath(), sid) + 1
	duration := int64(0) //nolint:gosec // G115: turn durations are pod-lifetime bounded
	if !busySince.IsZero() && settledAt.After(busySince) {
		duration = settledAt.Sub(busySince).Milliseconds()
	}
	if err := appendBoardTurnEnd(boardTurnEndsPath(), boardTurnEndEntry{
		SessionID:      sid,
		SettledAt:      settledAt.UTC().Format(time.RFC3339Nano),
		TurnStartedAt:  busySince.UTC().Format(time.RFC3339Nano),
		Advanced:       advanced,
		Epoch:          epoch,
		TurnDurationMs: duration,
	}); err != nil {
		boardTurnEndPiFails.Add(1)
		log.Error("pi board turn-end: journal append failed — emission lost (sweep backstop owns the row)",
			zap.String("session", sid), zap.Error(err))
	}
}

// boardTurnEndPiFails counts contained pi-leg emission failures (the
// health signal for this leg; the ops bridge reads it at teardown —
// kept as an atomic here to avoid new plumbing for a counter).
var boardTurnEndPiFails atomic.Int64

// boardTurnEndEntry is the journal record: the EVENT_TYPE_BOARD_TURN_END
// payload (advanced/epoch/turn_duration_ms) plus the identity and
// timing anchors the serialized ABI event carries — one JSON line.
type boardTurnEndEntry struct {
	SessionID      string `json:"session_id"`
	SettledAt      string `json:"settled_at"`
	TurnStartedAt  string `json:"turn_started_at"`
	Advanced       bool   `json:"advanced"`
	Epoch          uint64 `json:"epoch"`
	TurnDurationMs int64  `json:"turn_duration_ms"`
}

// sessionIsBoardWorkerViaFile scans the append-only worker registry for
// the session id (fresh-by-construction: the sidecar appends at spawn,
// which always precedes the first turn end; the file is capped by the
// worker retention). One small read per observed turn end.
func sessionIsBoardWorkerViaFile(path, sessionID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.SessionID == sessionID {
			return true
		}
	}
	return false
}

// countBoardTurnEnds counts the session's prior journal entries (the
// epoch basis — restart-stable, journal-lifetime).
func countBoardTurnEnds(path, sessionID string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var n uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.SessionID == sessionID {
			n++
		}
	}
	return n
}

// appendBoardTurnEnd appends one journal line (creating the file),
// compacting first when the journal outgrows the bound.
func appendBoardTurnEnd(path string, e boardTurnEndEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if n := journalLineCount(path); n > boardTurnEndJournalCompactLines {
		if err := compactJournalTail(path); err != nil {
			// Compaction failure must not lose the emission: append
			// anyway (the file grows past the bound; next append
			// retries the compaction).
			log.Warn("pi board turn-end: journal compaction failed — appending past the bound", zap.Error(err))
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// journalLineCount is the compaction guard's read.
func journalLineCount(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
	}
	return n
}

// compactJournalTail rewrites the journal keeping the newest 1000 lines
// (temp file + rename — atomic on POSIX; a crash mid-compaction leaves
// the old journal intact).
func compactJournalTail(path string) error {
	const keep = 1000
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	_ = f.Close() // after the scan — a closed file reads as empty and compaction never fires (pinned by test)
	if len(lines) <= keep {
		return nil
	}
	lines = lines[len(lines)-keep:]
	tmp := path + ".compact"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := tf.WriteString(l + "\n"); err != nil {
			_ = tf.Close()
			return err
		}
	}
	if err := tf.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
