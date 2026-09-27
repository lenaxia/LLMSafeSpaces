// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// generation_signal.go — #1576 layer 1's missing half: the generation
// SIGNAL. The projection's orphan fold (Reseed(GenerationChange) → the
// S12 abort) existed but never fired in split mode: the sidecar never
// learned the supervisor respawned opencode (#1573's live finding —
// the D2 reset fired on NEITHER boot-restore NOR respawn). The signal
// rides observable truth that already flows: the sidecar's status
// poller fetches the supervisor's ChildPID — a PID change IS a
// generation boundary, and the FIRST observation is the boot boundary
// (tracker flags held at boot are unverified SSE cache, orphaned by
// definition). The D2 reset itself (resetBusyFlags — clears, logs, and
// counts into workspace_tracker_busy_resets_total, the orphaned-flag
// datum for the owner's deferral decision) lives on the tracker; the
// authority's generation reseed composes at the wiring sites (they own
// the context). See worklog NNNN_2026-09-26 (sentinel; bot assigns at merge).

import (
	"sync"
)

// generationSignal detects generation boundaries from the supervisor's
// child PID + restart epoch: the first observation (boot) and every
// change of EITHER component. The composite key covers SAME-SUPERVISOR
// boundaries: PID reuse within one supervisor (the epoch is monotone
// in-process) and respawns that bump the epoch. KNOWN MISS (stated,
// not claimed away): Restarts resets to 0 when the workspace CONTAINER
// restarts (the supervisor dies with it) — if the new supervisor's
// deterministic early spawn reproduces the pre-restart PID AND the
// pre-restart epoch was 0 (the common healthy-workspace case), the
// observation pair is identical and the boundary is silently missed.
// Closing that class needs a cross-container epoch marker the new
// supervisor inherits (persisted state) — tracked with the layer-3/PR4
// liveness work, where a dead-harness signal catches it independently.
type generationSignal struct {
	mu      sync.Mutex
	lastPID int
	lastGen int
}

func newGenerationSignal() *generationSignal { return &generationSignal{} }

// observe records a (ChildPID, Restarts) observation; true when it
// opens a new generation (including the boot observation).
func (g *generationSignal) observe(pid, restarts int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lastPID == pid && g.lastGen == restarts {
		return false
	}
	g.lastPID = pid
	g.lastGen = restarts
	return true
}

// lastKnown returns the last observed generation PID (0 = none yet).
func (g *generationSignal) lastKnown() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastPID
}
