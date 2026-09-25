// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// clock.go — the agentd clock seam (#1532's fix class).
//
// The CI-load flake family (#1532): three real-clock sites — the
// staging sweeper's TTL ticker, the health-watchdog loop's poll ticker
// (+ the vitals boot-grace window), and the supervisor tests' poll
// sleeps — raced the runner: tests asserted "enough ticks/sleeps
// happened in N real milliseconds", which a loaded runner routinely
// violates, and the family's accumulated wall clock blew the package's
// 5-minute alarm twice.
//
// The seam routes the three sites' time reads, tickers, and sleeps
// through these indirections. PRODUCTION DEFAULTS ARE THE STDLIB
// FUNCTIONS — behavior is byte-identical (pinned by
// TestClockSeam_ProductionDefaults). Tests substitute the manualClock
// fake (clock_test.go) and assert tick ORDERING and window boundaries
// deterministically instead of sleeping past them.
//
// Discipline (the setWatchdogTiming precedent): the vars are swapped
// per-test with restore-on-cleanup, and the owning goroutine is joined
// BEFORE restore — the tests in this package run non-parallel by
// convention; a -race regression here is a test bug, not a seam bug.

import "time"

var (
	// agentdNow is every wall-clock READ in the three #1532 sites.
	agentdNow = time.Now
	// agentdNewTicker builds the three loops' tick sources.
	agentdNewTicker = stdNewTicker
	// agentdSleep is the poll/retry sleep in the #1532 sites.
	agentdSleep = time.Sleep
)

// stdNewTicker is the production ticker constructor (time.NewTicker
// behind the seam's channel+stop shape).
func stdNewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
