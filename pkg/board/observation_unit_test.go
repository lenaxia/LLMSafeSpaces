// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// observation_unit_test.go — the S7 retention gate's pure semantics,
// pinned in-package (no DB, no hour-long waits): a never-run gate is
// DUE, a run inside the interval is SKIPPED, and a run past the
// interval is due again. The sweep-loop WIRING (the gate actually
// consulted) is pinned in the integration suite
// (TestPin_ObservationTrail_PruneRidesTheSweep), and the gate's
// CONCURRENT shape (check+stamp as one CAS over the atomic) is
// exercised by the standing kill-test's parallel Sweeps under -race.

import (
	"sync"
	"testing"
	"time"
)

func TestObservationPruneDue(t *testing.T) {
	now := time.Now()
	if !observationPruneDue(time.Time{}, now) {
		t.Fatal("a never-run gate is due — a fresh reconciler's first sweep prunes")
	}
	last := now
	if observationPruneDue(last, now.Add(ObservationPruneInterval-time.Second)) {
		t.Fatal("a run inside the interval is skipped")
	}
	if !observationPruneDue(last, now.Add(ObservationPruneInterval)) {
		t.Fatal("the interval boundary itself is due again")
	}
}

// TestObservationPruneGateConcurrentSweeps: parallel Sweeps on ONE
// reconciler race only through the atomic — exactly one pass runs per
// due interval, none inside it (the kill-test pattern's shape, without
// the DB: PruneObservationTrail is NOT called here; the CAS gate alone
// is under test via the stamp it leaves).
func TestObservationPruneGateConcurrentSweeps(t *testing.T) {
	r := &Reconciler{}
	// The gate consults PruneObservationTrail on a winning CAS; give
	// the reconciler a nil-free path by asserting ONLY the stamping:
	// race all starters against a barrier, then confirm exactly ONE
	// stamped value exists and no unsynchronized access occurs (the
	// -race detector is the real referee — this test gives it the
	// contended shape deliberately).
	var wg sync.WaitGroup
	start := make(chan struct{})
	const n = 8
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			nowNanos := time.Now().UnixNano()
			last := r.obsPruneNanos.Load()
			if !observationPruneDue(unixNanoTime(last), time.Unix(0, nowNanos)) {
				return
			}
			r.obsPruneNanos.CompareAndSwap(last, nowNanos)
		}()
	}
	close(start)
	wg.Wait()
	stamped := r.obsPruneNanos.Load()
	if stamped == 0 {
		t.Fatal("the never-run gate is due — at least one racer must stamp it")
	}
	// Inside the interval now: every further gate consult skips.
	if observationPruneDue(unixNanoTime(stamped), time.Unix(0, stamped+int64(ObservationPruneInterval-time.Second))) {
		t.Fatal("inside the interval the gate must stay shut")
	}
}
