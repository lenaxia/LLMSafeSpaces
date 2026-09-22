// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
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
