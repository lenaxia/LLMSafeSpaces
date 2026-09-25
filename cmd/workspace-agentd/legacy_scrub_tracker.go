// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// legacy_scrub_tracker.go — US-72.6: the one-shot holder for the
// legacy-key scrub's outcome. runOnce is wired as the relay monitor's
// first-Present hook (a post-flip pod); the report is static thereafter
// and surfaces on healthz (/v1/healthz — the surface the controller
// polls) for the LegacyKeysScrubbed mirror.

type legacyScrubTracker struct {
	once   sync.Once
	root   string
	mu     sync.Mutex
	report *agentd.LegacyScrubHealth
}

func newLegacyScrubTracker(root string) *legacyScrubTracker {
	return &legacyScrubTracker{root: root}
}

// runOnce performs the scrub exactly once and stores the report. A
// scrub error is RECORDED, never fatal — the migration is best-effort
// with a loud report (the controller surfaces the error string).
func (t *legacyScrubTracker) runOnce() {
	t.once.Do(func() {
		rep := &agentd.LegacyScrubHealth{RanAt: time.Now().Unix()}
		scrub, err := scrubLegacyKeys(t.root)
		rep.AuthKeysRemoved = scrub.AuthKeysRemoved
		rep.ConfigKeysRemoved = scrub.ConfigKeysRemoved
		if err != nil {
			rep.Error = err.Error()
		}
		t.mu.Lock()
		t.report = rep
		t.mu.Unlock()
	})
}

// snapshot returns the static report (nil before the scrub ran).
func (t *legacyScrubTracker) snapshot() *agentd.LegacyScrubHealth {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.report
}

// bootLegacyScrub fires the one-time scrub at boot, UNCONDITIONALLY of
// the relay monitor's Present state (run 36135708380: M2's fail-open
// fallback can deliver a raw batch on any unconverged boot, leaving the
// first-Present hook unfired exactly when the residue migration must
// run). The monitor hook remains wired as belt-and-braces; the
// tracker's sync.Once collapses whichever fires first.
func bootLegacyScrub(t *legacyScrubTracker) {
	if t == nil {
		return
	}
	go t.runOnce()
}

// supervisorLegacyScrub holds the supervise-opencode process's boot
// scrub tracker (US-72.6): the control socket's status method reads it
// (sidecar mode — the sidecar's healthz mirror polls the supervisor for
// the report because the scrub itself can only run in this uid-1000
// process, the sole one with /workspace visibility). Set once at boot;
// read-only thereafter.
var supervisorLegacyScrub atomic.Pointer[legacyScrubTracker]

// legacyScrubRootFromEnv resolves the scrub root (the PVC workspace
// mount; overridable for exec-level integration tests the same way the
// subcommand's --workspace-root is).
func legacyScrubRootFromEnv() string {
	if r := os.Getenv("LLMSAFESPACES_LEGACY_SCRUB_ROOT"); r != "" {
		return r
	}
	return "/workspace"
}
