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
// the context). See worklog NNNN_2026-09-26.

import (
	"sync"
)

// generationSignal detects generation boundaries from the supervisor's
// child PID: the first observation (boot) and every change (respawn).
type generationSignal struct {
	mu   sync.Mutex
	last int
}

func newGenerationSignal() *generationSignal { return &generationSignal{} }

// observe records a ChildPID observation; true when it opens a new
// generation (including the boot observation).
func (g *generationSignal) observe(pid int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == pid {
		return false
	}
	g.last = pid
	return true
}

// lastKnown returns the last observed generation PID (0 = none yet).
func (g *generationSignal) lastKnown() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}
