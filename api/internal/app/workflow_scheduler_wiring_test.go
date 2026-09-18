// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

// Workflow-scheduler session-index wiring pin (#1452). The fix's only
// production site is ONE line in app.go's New(): wiring sessionIndexSvc
// into the apiwf.Scheduler literal. Every engine-level test passes with
// that line deleted (the scheduler nil-skips indexing), so the wiring
// itself needs its own pin — same philosophy as the handlers package's
// no_session_derivation_test.go source scan: constructible App boot
// needs a live cluster, but the presence of the wiring is exactly what
// past regressions would silently drop.

import (
	"os"
	"strings"
	"testing"
)

func TestWorkflowScheduler_SessionIndexWired(t *testing.T) {
	raw, err := os.ReadFile("app.go")
	if err != nil {
		t.Skipf("app.go not readable from test cwd: %v", err)
	}
	src := string(raw)

	const wiring = "SessionIndex: sessionIndexSvc"
	if !strings.Contains(src, wiring) {
		t.Fatalf("app.go lost the scheduler session-index wiring (%q) — preserved routine sessions "+
			"would silently vanish from GET /workspaces/:id/sessions again (#1452): every engine test "+
			"stays green with a nil SessionIndex, only this pin and the e2e row catch it", wiring)
	}

	schedulerAt := strings.Index(src, "wfScheduler = &apiwf.Scheduler{")
	wiringAt := strings.Index(src, wiring)
	if schedulerAt == -1 {
		t.Fatal("app.go: scheduler construction literal not found — has New() been restructured?")
	}
	if wiringAt < schedulerAt || wiringAt > schedulerAt+800 {
		t.Fatalf("app.go: %q must sit inside the apiwf.Scheduler literal (found at offset %d, literal at %d)",
			wiring, wiringAt, schedulerAt)
	}
}
