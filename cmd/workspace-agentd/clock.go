// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// clock.go — the agentd clock seam (#1532's fix class).
//
// The CI-load flake family (#1532), instances 1 and 3: two real-clock
// sites — the staging sweeper's TTL ticker and the health-watchdog
// loop's poll ticker (+ the vitals boot-grace window) — raced the
// runner: tests asserted "enough ticks/sleeps happened in N real
// milliseconds", which a loaded runner routinely violates, and the
// family's accumulated wall clock blew the package's 5-minute alarm
// twice. (Instance 2 — the supervisor-helper family — waits on
// EXTERNAL subprocess events a package-var seam cannot reach; triaged
// I/O-inherent and left open on #1532.)
//
// The seam routes the two sites' time reads and tickers through these
// indirections. PRODUCTION DEFAULTS ARE THE STDLIB FUNCTIONS —
// behavior is byte-identical (pinned by TestClockSeam_ProductionDefaults).
// Tests substitute the manualClock fake (clock_test.go) and assert tick
// ORDERING and window boundaries deterministically instead of sleeping
// past them.
//
// Discipline (the setWatchdogTiming precedent): a test that swaps
// these vars restores them on cleanup and joins any goroutine reading
// them BEFORE the restore — the seam-swapping tests themselves must
// not run in parallel; a -race regression here is a test bug, not a
// seam bug.

import "time"

var (
	// agentdNow is the de-timed wall-clock read in the two #1532
	// sites (the sweeper's scrub age-out and the vitals boot-grace
	// window). Other real reads deliberately remain: the watchdog's
	// rate-limiter window (wd.maybeFire(time.Now()) — unit-tested
	// with synthetic times, not reachable by the boot-window test)
	// and LastRefreshedAt bookkeeping.
	agentdNow = time.Now
	// agentdNewTicker builds the two loops' tick sources.
	agentdNewTicker = stdNewTicker
)

// stdNewTicker is the production ticker constructor (time.NewTicker
// behind the seam's channel+stop shape).
func stdNewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
