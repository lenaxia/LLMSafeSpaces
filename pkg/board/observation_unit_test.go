// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// observation_unit_test.go — the S7 retention gate's pure semantics,
// pinned in-package (no DB, no hour-long waits): the zero value is DUE
// (a fresh reconciler's first sweep prunes), a run inside the interval
// is SKIPPED, and a run past the interval is due again. The sweep-loop
// WIRING (the gate actually consulted) is pinned in the integration
// suite (TestPin_ObservationTrail_PruneRidesTheSweep).

import (
	"testing"
	"time"
)

func TestObservationPruneDue(t *testing.T) {
	r := &Reconciler{}
	now := time.Now()
	if !r.observationPruneDue(now) {
		t.Fatal("the zero value is due — a fresh reconciler's first sweep prunes")
	}
	r.obsPruneLast = now
	if r.observationPruneDue(now.Add(ObservationPruneInterval - time.Second)) {
		t.Fatal("a run inside the interval is skipped")
	}
	if !r.observationPruneDue(now.Add(ObservationPruneInterval)) {
		t.Fatal("the interval boundary itself is due again")
	}
}
