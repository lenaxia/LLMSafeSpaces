// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_workers_2100_test.go — issue #2100-A's born-red pins (the
// lease-evidence-core design's Phase 1.2): the registry/tail artifact
// remediation. The defect class, verified live: boardWorkerSessions'
// record() ran sidecar-side where /workspace is READ-ONLY, the append
// EROFS-failed SILENTLY, and the emitting half hydrated a frozen
// artifact (11 test ids, mtime Nov 16 04:59, zero prod workers) — the
// platform could not see that a prod worker went quiet, so no nudge
// ever fired.
//
// The remediation, pinned here:
//
//	paths    the three board file artifacts default under /exchange —
//	         the ONE dual-RW subtree (0070 §2.4a) — with FLAT names
//	         (the no-nested-exchange-paths rule). Born-red on main:
//	         the defaults point at read-only /workspace.
//	lands    a sidecar write at the remediated path lands and a FRESH
//	         registry (the emitting half's shape) hydrates it — no
//	         restart, no luck.
//	loud     an unwritable registry path increments
//	         board_workers_registry_write_failures and WARNs — the
//	         silent-EROFS defect class is dead (pre-fix: record()
//	         returns on file error with no signal whatsoever, which is
//	         exactly how the freeze stayed invisible for two days).
//	tail     the advance tail's open failure increments
//	         board_advance_tail_write_failures (the memory-only
//	         degrade is loud, pre-fix: fails++ exists but no expvar).
//	hydr     hydrate-on-demand: isWorker on an unknown id re-reads the
//	         file when its mtime moved (the emitting half sees sidecar
//	         appends without restarts — pre-fix: hydrate is once, at
//	         process start, forever).
//	frozen   compat + primacy: the legacy /workspace path is read ONLY
//	         when the new path is absent (one release of upgrade
//	         grace), a poisoned/corrupt file never errors the hydrate
//	         (per-line skip stands), and a fabricated line never
//	         fabricates an advance the memory ledger contradicts.

import (
	"encoding/json"
	"expvar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// pin2100Env redirects the artifact envs at a fresh temp dir and
// returns it (the TestMain redirect pattern, scoped per-test).
func pin2100Env(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", filepath.Join(d, "workers.jsonl"))
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", filepath.Join(d, "advances.jsonl"))
	t.Setenv("LLMSAFESPACES_BOARD_TURNENDS_PATH", filepath.Join(d, "turnends.jsonl"))
	return d
}

// TestPin_2100A_DefaultPathsUnderExchange — the remediation's anchor:
// the three board artifacts default under the ONE dual-RW subtree, so
// the sidecar's appends can land at all. Born-red on main (the
// defaults point at /workspace — read-only to exactly the writer that
// matters).
func TestPin_2100A_DefaultPathsUnderExchange(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  func() string
	}{
		{"workers registry", func() string { return boardWorkersDefaultPath }},
		{"advances tail", func() string { return boardAdvancesDefaultPath }},
		{"turnends journal", func() string { return boardTurnEndsDefaultPath }},
	} {
		assert.Equal(t, map[string]string{
			"workers registry": "/exchange/.opencode-board-workers",
			"advances tail":    "/exchange/.opencode-board-advances",
			"turnends journal": "/exchange/.opencode-board-turnends",
		}[tt.name], tt.got(), tt.name+" default must live under /exchange (the dual-RW subtree)")
	}
	// The env overrides still win (the test/pin discipline and the
	// in-pod escape hatch survive the move).
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", "/tmp/x-workers.jsonl")
	assert.Equal(t, "/tmp/x-workers.jsonl", boardWorkersPath())
}

// TestPin_2100A_SidecarRegistryWriteLands — the cross-process bridge
// the incident killed: the sidecar records a spawn; a FRESH registry
// (the emitting half's constructor) hydrates the same file and answers
// isWorker=true. With the remediated default the write can actually
// land (on main this passes only when the env points somewhere
// writable — the default pointed at read-only /workspace, which is the
// born-red leg pinned above).
func TestPin_2100A_SidecarRegistryWriteLands(t *testing.T) {
	pin2100Env(t)
	w := newBoardWorkerRegistry(boardWorkersPath())
	w.record("ses_sidecar_spawn_1")
	w.record("ses_sidecar_spawn_1") // idempotent append-only

	fresh := newBoardWorkerRegistry(boardWorkersPath()) // the OTHER half
	assert.True(t, fresh.isWorker("ses_sidecar_spawn_1"), "the emitting half hydrates the sidecar's append")
	assert.False(t, fresh.isWorker("ses_never_spawned"))
}

// TestPin_2100A_LoudOnRegistryWriteFailure — the silent-EROFS class
// dies: an unwritable path increments the failure expvar and WARNs.
// Pre-fix: record() returns on file error with no signal at all.
func TestPin_2100A_LoudOnRegistryWriteFailure(t *testing.T) {
	d := t.TempDir()
	// A read-only DIRECTORY (not a missing one): MkdirAll succeeds
	// (the dir exists), the append open fails EACCES/EROFS — the
	// sidecar's exact /workspace shape.
	ro := filepath.Join(d, "ro")
	require.NoError(t, os.MkdirAll(ro, 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(ro, "sub"), 0o500))
	target := filepath.Join(ro, "sub", "workers.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", target)

	obs, logs := observer.New(zap.WarnLevel)
	prevLog := log
	log = zap.New(obs)
	t.Cleanup(func() { log = prevLog })
	r := newBoardWorkerRegistry(target)
	before := expvar.Get("board_workers_registry_write_failures").(*expvar.Int).Value()
	r.record("ses_cannot_land")
	after := expvar.Get("board_workers_registry_write_failures").(*expvar.Int).Value()

	assert.Equal(t, before+1, after, "the failed append is COUNTED (loud, not silent)")
	warned := false
	for _, l := range logs.All() {
		if l.Level == zap.WarnLevel && strings.Contains(l.Message, "board worker registry") {
			warned = true
		}
	}
	assert.True(t, warned, "the failed append WARNs (the freeze that stayed invisible for two days)")
	// The in-memory set still answers (best-effort posture unchanged).
	assert.True(t, r.isWorker("ses_cannot_land"))
}

// TestPin_2100A_TailOpenFailureLoud — the advance tail's memory-only
// degrade is counted in expvar (the design's second loud gauge).
func TestPin_2100A_TailOpenFailureLoud(t *testing.T) {
	d := t.TempDir()
	ro := filepath.Join(d, "ro2", "sub")
	require.NoError(t, os.MkdirAll(filepath.Dir(ro), 0o750))
	require.NoError(t, os.MkdirAll(ro, 0o500))
	target := filepath.Join(ro, "advances.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", target)

	before := expvar.Get("board_advance_tail_write_failures").(*expvar.Int).Value()
	l := newBoardAdvanceLedger(target, zap.NewNop())
	after := expvar.Get("board_advance_tail_write_failures").(*expvar.Int).Value()
	assert.Equal(t, before+1, after, "the unopenable tail is COUNTED")
	assert.Nil(t, l.appnd, "memory-only degrade")
	// The stamp still lands in memory (the opencode leg loses nothing).
	l.observeToolCall("board_move_stage", map[string]any{"lsp_injected_session": "ses_x"})
	assert.True(t, l.advancedSince("ses_x", time.Now().Add(-time.Minute)))
}

// TestPin_2100A_HydrateOnDemandSeesSidecarAppends — the emitting half
// converges without a restart: isWorker on an unknown id re-reads the
// file when its mtime moved. Pre-fix: hydrate happens once at
// construction and the half stays frozen forever (the live defect).
func TestPin_2100A_HydrateOnDemandSeesSidecarAppends(t *testing.T) {
	pin2100Env(t)
	r := newBoardWorkerRegistry(boardWorkersPath())
	assert.False(t, r.isWorker("ses_late_append"))

	// Another PROCESS appends (the sidecar's shape: same path, same
	// append-only file, later mtime).
	b, err := json.Marshal(map[string]string{"session_id": "ses_late_append"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(boardWorkersPath(), append(b, '\n'), 0o600))

	assert.True(t, r.isWorker("ses_late_append"),
		"an mtime-moved file re-hydrates on consult — the half sees the sidecar's append")
}

// TestPin_2100A_FrozenOldPathNeverWins — compat with primacy: the
// legacy /workspace path is consulted ONLY when the new path is
// absent; when both exist, the remediated path is the truth (the
// frozen artifact never wins over the live one), and a poisoned file
// (garbage JSON, wrong-shape records, fabricated ids) hydrates
// per-line-skip without error.
func TestPin_2100A_FrozenOldPathNeverWins(t *testing.T) {
	d := t.TempDir()
	newPath := filepath.Join(d, "workers.jsonl")
	legacy := filepath.Join(d, "legacy-workers.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", newPath)
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_LEGACY_PATH", legacy)

	// Both files exist: the frozen legacy artifact claims DIFFERENT
	// ids than the live one. The live one wins; the legacy ids are
	// invisible.
	legacyLines := "garbage not json\n" + `{"session_id":"ses_frozen_legacy"}` + "\n"
	require.NoError(t, os.WriteFile(legacy, []byte(legacyLines), 0o600))
	liveLines := `{"session_id":"ses_live_new"}` + "\n{\n" // trailing partial line: skipped per-line
	require.NoError(t, os.WriteFile(newPath, []byte(liveLines), 0o600))

	r := newBoardWorkerRegistry(newPath)
	assert.True(t, r.isWorker("ses_live_new"), "the live (remediated) path's ids hydrate")
	assert.False(t, r.isWorker("ses_frozen_legacy"), "a present new path means the legacy artifact is NOT consulted")

	// Only the legacy file exists (in-place upgrade): its valid ids
	// carry over (one release of grace); its poison never errors.
	require.NoError(t, os.Remove(newPath))
	r2 := newBoardWorkerRegistry(newPath)
	assert.True(t, r2.isWorker("ses_frozen_legacy"), "legacy-only: pre-existing entries carry over across the upgrade")
	assert.False(t, r2.isWorker("ses_live_new"))
}
