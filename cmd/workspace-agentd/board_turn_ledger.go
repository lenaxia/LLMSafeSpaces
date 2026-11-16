// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_turn_ledger.go — #2061/#2064 story 1: the harness-agnostic
// board turn-end emitter machinery, shared by BOTH in-tree harness
// legs (the opencode transport via the sessionstate authority's probe
// seam, and the pi driver via the supervise-pi pump's settle fold).
// No opencode-only branch (audit ruling 5, the #2030 lane's owner
// ruling).
//
// Three facts, three sources, one discipline:
//
//   - CLAIM-HOLDER (spawner-minted assignees only, never user chats):
//     the boardWorkerSessions registry (board_workers.go — recorded at
//     spawn by the workflow node executor, append-only JSONL on the
//     shared /workspace volume, so BOTH processes on a pi pod read the
//     same truth).
//   - ADVANCE (the narrow set, nothing richer): record_progress |
//     stage move | verdict — the advance-class board tool calls
//     observed at agentd's own MCP dispatch choke point (callMCPTool),
//     keyed on the harness-attested lsp_injected_session ONLY. Presence
//     reads (board_read/list/health/validate) and heartbeats are
//     deliberately NOT advance (the keystone lesson); no productivity
//     detection exists beyond the set. The stamps are agentd-local
//     bookkeeping (epic LLD R3): memory for the serving process, plus
//     an append-only JSONL tail on the shared volume so the supervise-pi
//     process (a different agentd process on the pod) derives the same
//     flag at its own turn ends.
//   - EPOCH: the emitter's 1-based per-session ordinal of observed
//     turn ends (the opencode leg: in-process counter; the pi leg: the
//     turn-end journal's per-session line count — restart-stable).
//
// The emission-site rule lives at each leg's attested fold (the
// authority's applyHarnessEventLocked; the pump's agent_settled
// handler) — never in this file: the ledger only answers questions.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"go.uber.org/zap"
)

// boardAdvanceTools is the advance set — the single definition of "this
// board tool call advanced the worker's claim", pinned per source in
// board_turn_ledger_test.go. board_record_progress does not exist in
// the tool registry yet (#2044's lane owns it): it is named here so the
// day it lands it joins the set by NAME, with no derivation change.
var boardAdvanceTools = map[string]bool{
	"board_record_progress": true,
	"board_move_stage":      true,
	"board_record_verdict":  true,
}

const boardAdvancesDefaultPath = "/workspace/.opencode-board-advances"

// boardAdvancesPath resolves the advance-ledger tail's location (the
// markerPathFromEnv pattern; shared-volume default so the sidecar
// appends and the supervise-pi process reads the same file).
func boardAdvancesPath() string {
	if p := os.Getenv("LLMSAFESPACES_BOARD_ADVANCES_PATH"); p != "" {
		return p
	}
	return boardAdvancesDefaultPath
}

// boardAdvanceLedger is the per-session advance observation: the last
// wall-clock stamp per session that made an advance-class board tool
// call through THIS agentd (both harnesses share the MCP surface).
// Memory is authoritative for the serving process; the JSONL tail
// carries the fact across processes on the pod (the pi leg).
type boardAdvanceLedger struct {
	mu     sync.Mutex
	last   map[string]time.Time
	path   string
	appnd  *os.File
	fails  int64
	logger *zap.Logger
}

// newBoardAdvanceLedger opens (creating if needed) the shared tail.
// Best-effort: an unopenable file degrades to memory-only (the
// opencode leg loses nothing; the pi leg's emission for that pod
// degrades to advanced=false-unless-memory — the sweep backstop owns
// the rest, epic R5).
func newBoardAdvanceLedger(path string, logger *zap.Logger) *boardAdvanceLedger {
	if logger == nil {
		logger = zap.NewNop()
	}
	l := &boardAdvanceLedger{last: map[string]time.Time{}, path: path, logger: logger}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		l.fails++
		return l
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.fails++
		logger.Warn("board advance ledger: tail unavailable — memory-only (pi-leg derivation degrades)", zap.Error(err))
		return l
	}
	l.appnd = f
	return l
}

func (l *boardAdvanceLedger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.appnd != nil {
		_ = l.appnd.Close()
		l.appnd = nil
	}
}

// observeToolCall is the choke-point stamp: called at callMCPTool's
// head for every board tool, BEFORE dispatch (observation at arrival,
// outcome untouched — a refused call still observed: the platform's
// refusal is the outcome, the ATTEMPT is the advance signal the board
// sees on its own wire anyway). Identity is the harness-attested
// lsp_injected_session ONLY — a model-declared sessionId copy never
// stamps (the #2040 evidence-poisoning class stays closed, pinned).
func (l *boardAdvanceLedger) observeToolCall(tool string, args map[string]any) {
	if !boardAdvanceTools[tool] {
		return
	}
	injected, _ := args["lsp_injected_session"].(string)
	sid := strings.TrimSpace(injected)
	if sid == "" {
		return // unattributable (plugin absent / non-injected caller) — never a declared copy
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last[sid] = now
	if l.appnd != nil {
		b, _ := json.Marshal(map[string]any{"session_id": sid, "advanced_at": now.UTC().Format(time.RFC3339Nano)})
		if _, err := l.appnd.Write(append(b, '\n')); err != nil {
			l.fails++
			l.logger.Warn("board advance ledger: tail append failed (memory holds)", zap.Error(err))
		}
	}
}

// advancedSince answers whether an advance-class call was observed for
// the session inside the turn window that opened at turnStart.
func (l *boardAdvanceLedger) advancedSince(sessionID string, turnStart time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.last[sessionID]
	return ok && at.After(turnStart)
}

// readBoardAdvanceStamps scans the shared tail for the session's last
// advance stamp (the pi leg's cross-process consult; scan cost is one
// small-file read per observed turn end — advance calls are rare).
func readBoardAdvanceStamps(path, sessionID string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	var best time.Time
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec struct {
			SessionID  string `json:"session_id"`
			AdvancedAt string `json:"advanced_at"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.SessionID != sessionID {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, rec.AdvancedAt); err == nil && at.After(best) {
			best = at
		}
	}
	return best, !best.IsZero()
}

// sessionHoldsBoardAdvance consults both stores (memory first, then the
// shared tail) — the wiring probe and the pi pump share this one
// derivation.
func sessionHoldsBoardAdvance(l *boardAdvanceLedger, path, sessionID string, turnStart time.Time) bool {
	if l != nil && l.advancedSince(sessionID, turnStart) {
		return true
	}
	if turnStart.IsZero() {
		// No window anchor (should not happen — the fold always knows
		// busySince): fail toward "no advance" (the safe direction —
		// the board due-marks, the sweep re-checks).
		if at, ok := readBoardAdvanceStamps(path, sessionID); ok {
			_ = at
			return true
		}
		return false
	}
	at, ok := readBoardAdvanceStamps(path, sessionID)
	return ok && at.After(turnStart)
}

// boardAdvanceLedgerInst is the process-wide advance ledger (the MCP
// choke point stamps; the wiring probe and the pi leg consult). The
// shared-volume tail is best-effort from the start: a missing/unwritable
// path degrades to memory-only, loudly once.
//
// LAZY (#2064): the tail path resolves at FIRST USE, not var-init —
// the test binary's TestMain redirects
// LLMSAFESPACES_BOARD_ADVANCES_PATH before any stamp (hermetic: no
// test appends into the pod's real shared file), and production's
// first use happens long after the logger exists.
var (
	boardAdvanceLedgerOnce sync.Once
	boardAdvanceLedgerVal  *boardAdvanceLedger
)

func boardAdvanceLedgerInst() *boardAdvanceLedger {
	boardAdvanceLedgerOnce.Do(func() {
		boardAdvanceLedgerVal = newBoardAdvanceLedger(boardAdvancesPath(), log)
	})
	return boardAdvanceLedgerVal
}

// boardTurnEmitterInst is the process-wide opencode-leg probe backend
// (registry + ledger + epoch ordinals), wired into the sessionstate
// authority's Config at newStateAuthority. Same laziness: the ledger
// must exist first, under the same first-use env resolution.
var (
	boardTurnEmitterOnce sync.Once
	boardTurnEmitterVal  *boardTurnEmitter
)

func boardTurnEmitterInst() *boardTurnEmitter {
	boardTurnEmitterOnce.Do(func() {
		boardTurnEmitterVal = newBoardTurnEmitter(boardWorkerSessions, boardAdvanceLedgerInst())
	})
	return boardTurnEmitterVal
}

// boardTurnEmitter is the opencode leg's probe backend: claim-holder
// registry + advance ledger + the in-process epoch ordinal.
type boardTurnEmitter struct {
	registry *boardWorkerRegistry
	ledger   *boardAdvanceLedger
	mu       sync.Mutex
	epochs   map[string]uint64
}

func newBoardTurnEmitter(registry *boardWorkerRegistry, ledger *boardAdvanceLedger) *boardTurnEmitter {
	return &boardTurnEmitter{registry: registry, ledger: ledger, epochs: map[string]uint64{}}
}

// facts is the sessionstate Config.BoardTurnEndProbe implementation for
// the opencode leg (wired in sessionstate_wiring.go). It runs under the
// authority's lock at turn transitions: two map reads and (on emission)
// a counter — cheap by contract.
func (e *boardTurnEmitter) facts(sessionID string, turnStart time.Time) sessionstate.BoardTurnEndFacts {
	claim := e.registry != nil && e.registry.isWorker(sessionID)
	return sessionstate.BoardTurnEndFacts{
		ClaimHolder: claim,
		Advanced:    claim && sessionHoldsBoardAdvance(e.ledger, boardAdvancesPath(), sessionID, turnStart),
	}
}

// nextEpoch is the pi leg's ordinal source when it shares this emitter
// (tests); production pi uses the journal count (restart-stable).
func (e *boardTurnEmitter) nextEpoch(sessionID string) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.epochs[sessionID]++
	return e.epochs[sessionID]
}
