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
// not self-certify its own work through ANY met-flag-writing manager
// shape: the setCriteriaMet check-off, a patchFile criteria replace,
// and pre-met criteria on create/import; #1946's grant stays
// reviewer-persona-only).
//
// Storage (#2100-A remediation, lease-evidence-core design §1.2): an
// append-only JSON-lines file under /exchange — the ONE dual-RW
// subtree (0070 §2.4a; the pre-fix default was /workspace, READ-ONLY
// to exactly the sidecar process that writes, so every append
// EROFS-failed SILENTLY and the emitting half hydrated a frozen
// artifact — the #2100 dead path). FLAT name (the 0070
// no-nested-exchange-paths rule).
//
// Threat model, stated honestly (/exchange is agent-writable by
// design): these files are agent-readable AND agent-writable; they
// are not 0070 payload files and never ride the payload read
// discipline. Hydration tolerates poisoned/corrupt lines per-line
// (below); the registry's remaining authority is the SELF-CERT
// refusal, where a fabricated line could only WIDEN the refusal
// (refuse a non-worker's self-cert — fail-closed, single-tenant pod,
// the owner's forgery ruling). Under transport v2 the registry gates
// NOTHING cross-process (board_transport_v2.go); the file is belt for
// THIS half's restarts.
//
// Every write failure is LOUD (#2100's defect class was the SILENT
// EROFS): record() WARNs and increments
// board_workers_registry_write_failures — a frozen channel reads on
// /debug/vars and in the log stream, never invisible again.

import (
	"bufio"
	"encoding/json"
	"expvar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// boardWorkersDefaultPath is the remediated default: /exchange, the
// dual-RW subtree (#2100-A). FLAT name (0070).
const boardWorkersDefaultPath = "/exchange/.opencode-board-workers"

// boardWorkersLegacyDefaultPath is the pre-#2100 location — read ONLY
// when the remediated path is absent (one release of in-place-upgrade
// grace so pre-existing entries carry over), never written again.
const boardWorkersLegacyDefaultPath = "/workspace/.opencode-board-workers"

// boardWorkersWriteFailures counts registry append failures (the loud
// gauge; the silent-EROFS defect class dies here).
var boardWorkersWriteFailures expvar.Int

func init() { expvar.Publish("board_workers_registry_write_failures", &boardWorkersWriteFailures) }

// boardWorkersPath resolves the registry file's location (the
// markerPathFromEnv pattern).
func boardWorkersPath() string {
	if p := os.Getenv("LLMSAFESPACES_BOARD_WORKERS_PATH"); p != "" {
		return p
	}
	return boardWorkersDefaultPath
}

// boardWorkersLegacyPath resolves the legacy (pre-#2100) location —
// env-overridable for the compat pins, defaulting at the frozen
// /workspace artifact.
func boardWorkersLegacyPath() string {
	if p := os.Getenv("LLMSAFESPACES_BOARD_WORKERS_LEGACY_PATH"); p != "" {
		return p
	}
	return boardWorkersLegacyDefaultPath
}

// boardWorkerRegistry is the in-memory set of board-worker session ids,
// hydrated from the append-only file and extended on every spawn.
// Hydration is ALSO on-demand: isWorker on an unknown id stats the
// file and re-reads it when the mtime moved (#2100: the emitting half
// sees sidecar appends without restarts — the cross-process bridge the
// incident froze).
type boardWorkerRegistry struct {
	mu      sync.RWMutex
	ids     map[string]bool
	path    string
	lastMod time.Time // the hydrate-on-demand mtime gate
	known   bool      // the primary path has been seen (absent ⇒ legacy grace)
}

// hydrateFileLocked reads one append-only JSONL file into the set,
// skipping corrupt lines per-line (the scanner posture: poison never
// errors the hydrate). Returns whether the file EXISTS (the
// legacy-compat signal).
func (r *boardWorkerRegistry) hydrateFileLocked(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false // absent/unreadable: not hydrated from this path
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
	if st, err := f.Stat(); err == nil {
		r.lastMod = st.ModTime()
	}
	return true
}

// newBoardWorkerRegistry hydrates from disk: the remediated path
// first; ONLY when that file is absent does the legacy /workspace
// artifact carry pre-existing entries over (the frozen file never
// wins over a live one). Absent everywhere = empty.
func newBoardWorkerRegistry(path string) *boardWorkerRegistry {
	r := &boardWorkerRegistry{ids: map[string]bool{}, path: path}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known = r.hydrateFileLocked(path)
	if !r.known {
		if legacy := boardWorkersLegacyPath(); legacy != "" && legacy != path {
			_ = r.hydrateFileLocked(legacy) // grace: pre-existing entries survive the upgrade
		}
	}
	return r
}

// record appends one spawned worker session (spawn-time call site: the
// board-worker node execution). The in-memory set is authoritative for
// this process; the file carries restarts. LOUD on failure (#2100's
// defect class): a failed append WARNs and increments the failure
// expvar — never the silent EROFS again.
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
	fail := func(what string, err error) {
		boardWorkersWriteFailures.Add(1)
		if log != nil {
			log.Warn("board worker registry: append failed — in-memory holds, restarts lose this id (#2100 loud-degradation)",
				zap.String("what", what), zap.String("path", r.path), zap.Error(err))
		}
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		fail("mkdir", err)
		return
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fail("open", err)
		return
	}
	defer func() { _ = f.Close() }()
	b, _ := json.Marshal(map[string]string{"session_id": sessionID})
	if _, err := f.Write(append(b, '\n')); err != nil {
		fail("write", err)
		return
	}
	if st, err := f.Stat(); err == nil {
		r.lastMod = st.ModTime()
	}
}

// isWorker reports whether the session was spawned as a board worker.
// Unknown ids consult the FILE on mtime movement (hydrate-on-demand —
// #2100: the sidecar's appends become visible to a half that started
// before them; the consult stays a bounded small read of a
// bounded-by-construction file).
func (r *boardWorkerRegistry) isWorker(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.RLock()
	known := r.ids[sessionID]
	path := r.path
	r.mu.RUnlock()
	if known {
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		return false // no file / unreadable: nothing to re-hydrate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids[sessionID] {
		return true // raced a concurrent hydrate
	}
	if !st.ModTime().After(r.lastMod) {
		return false // quiet file: the id genuinely is not there
	}
	r.hydrateFileLocked(path) // mtime moved: merge the appends in
	return r.ids[sessionID]
}

// boardWorkerSessions is the process-wide registry (the spawn path and
// the MCP dispatcher share it).
var boardWorkerSessions = newBoardWorkerRegistry(boardWorkersPath())
