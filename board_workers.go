// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_workers.go — the board worker registry. The #1747 §5
// manager-grant gate it used to enforce is REPEALED (ruling
// 2026-10-27 / tsk-copper-refectory-generates: a workspace pod is
// single-tenant, every session shares one credential — identity
// gating over that is theater, and a task impersonating another
// session is the user's prompting issue). Spawned workers now hold
// the FULL manager set like every other in-workspace session, with
// ONE surviving refusal this registry owns: the reviewer-grant
// widening (boardWorkerSelfCertificationPin — a spawned worker may
// not check off its own work's criteria through the token-less
// manager shape; #1946's grant stays reviewer-persona-only).
//
// Storage: an append-only JSON-lines file beside the pod's other
// agentd state (the restart-marker pattern; env-overridable path).
// Append-only means "ever was a worker" — the strict posture the
// self-certification pin wants: a session that ever held a charter
// never gains the self-certification shape, even after its task
// moved on.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const boardWorkersDefaultPath = "/workspace/.opencode-board-workers"

// boardWorkersPath resolves the registry file's location (the
// markerPathFromEnv pattern).
func boardWorkersPath() string {
	if p := os.Getenv("LLMSAFESPACES_BOARD_WORKERS_PATH"); p != "" {
		return p
	}
	return boardWorkersDefaultPath
}

// boardWorkerRegistry is the in-memory set of board-worker session ids,
// hydrated once from the append-only file and extended on every spawn.
type boardWorkerRegistry struct {
	mu   sync.RWMutex
	ids  map[string]bool
	path string
}

// newBoardWorkerRegistry hydrates from disk (absent file = empty; a
// corrupt line is skipped — the registry fails open per-line, closed
// by the seam belt).
func newBoardWorkerRegistry(path string) *boardWorkerRegistry {
	r := &boardWorkerRegistry{ids: map[string]bool{}, path: path}
	f, err := os.Open(path)
	if err != nil {
		return r
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil || rec.SessionID == "" {
			continue
		}
		r.ids[rec.SessionID] = true
	}
	return r
}

// record appends one spawned worker session (spawn-time call site: the
// board-worker node execution). Best-effort persistence: the in-memory
// set is authoritative for this process; the file carries restarts.
func (r *boardWorkerRegistry) record(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids[sessionID] {
		return
	}
	r.ids[sessionID] = true
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	b, _ := json.Marshal(map[string]string{"session_id": sessionID})
	_, _ = f.Write(append(b, '\n'))
}

// isWorker reports whether the session was spawned as a board worker.
func (r *boardWorkerRegistry) isWorker(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ids[sessionID]
}

// boardWorkerSessions is the process-wide registry (the spawn path and
// the MCP dispatcher share it).
var boardWorkerSessions = newBoardWorkerRegistry(boardWorkersPath())
