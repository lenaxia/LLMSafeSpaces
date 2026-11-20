// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_board_turnend_v2_test.go — issue #2100's transport-v2 pins for
// the pi leg (lease-evidence-core design §2.1/§2.2): under the v2 flag
// the observer drops BOTH file consults — the worker-registry scan
// (the claim-holder gate the incident proved EROFS-dead) and the
// advance-tail derivation (advanced moves store-side). Every
// harness-attested settle-fold turn-end journals with advanced=false.
//
//	P2-pi-ungated   NO registry file exists anywhere, yet the turn-end
//	                journals (pre-fix: sessionIsBoardWorkerViaFile
//	                returns false on a missing file and the emission is
//	                silently dropped — the #2100-A dead path verbatim).
//	P2-pi-gated     flag off: the registry consult stands (a missing
//	                registry suppresses — today's behavior).

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetBoardTransportV2 re-resolves the process-once transport flag
// for one test (via the env — the once's body re-derives from env, so
// the value cannot be set directly; restored after).
func resetBoardTransportV2(t *testing.T, on bool) {
	t.Helper()
	t.Setenv("LLMSAFESPACES_BOARD_PRESENCEV2", strconv.FormatBool(on))
	boardTransportV2Once = sync.Once{}
	t.Cleanup(func() { boardTransportV2Once = sync.Once{} })
}

func TestPin_P2_PiObserverJournalsWithoutRegistry(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "turnends.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_TURNENDS_PATH", journal)
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", filepath.Join(dir, "advances.jsonl"))
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", filepath.Join(dir, "workers.jsonl"))
	// NOTE: no registry file is written — the pure no-registry shape.

	resetBoardTransportV2(t, true)
	piBoardTurnEndObserver("ses_pi_worker_1", time.Now().UTC().Add(-time.Minute), time.Now().UTC())

	entries := readJournal(t, journal)
	require.Len(t, entries, 1, "the turn-end journals with NO registry entries anywhere (assignee-match is the consumer's job)")
	assert.Equal(t, "ses_pi_worker_1", entries[0].SessionID)
	assert.False(t, entries[0].Advanced, "advanced is store-derived now — the tail consult is deleted")
	assert.False(t, entries[0].TurnStartedAt == "", "the journal entry carries the window start")
}

func TestPin_P2_PiObserverGatedDropsWithoutRegistry(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "turnends.jsonl")
	t.Setenv("LLMSAFESPACES_BOARD_TURNENDS_PATH", journal)
	t.Setenv("LLMSAFESPACES_BOARD_ADVANCES_PATH", filepath.Join(dir, "advances.jsonl"))
	t.Setenv("LLMSAFESPACES_BOARD_WORKERS_PATH", filepath.Join(dir, "workers.jsonl"))

	resetBoardTransportV2(t, false)
	piBoardTurnEndObserver("ses_pi_worker_2", time.Now().UTC().Add(-time.Minute), time.Now().UTC())

	assert.Empty(t, readJournal(t, journal),
		"flag off: the registry consult stands (missing registry suppresses — today's behavior)")
	_, err := os.Stat(journal)
	require.True(t, os.IsNotExist(err), "no artifact is created for a suppressed emission")
}
