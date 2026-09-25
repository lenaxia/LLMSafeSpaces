// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// clock_test.go — the manualClock fake (#1532, instances 1+3) + the
// production defaults pin. The per-site deterministic tests live
// beside their sites (upload_staging_test.go, watchdog_vitals_test.go).

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// manualClock is the deterministic fake for the two #1532 sites: a
// steppable now and tickers the TEST fires in order (world-advance is
// the test's choice).
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*manualTicker
}

type manualTicker struct {
	ch chan time.Time
}

func newManualClock(t *testing.T) *manualClock {
	t.Helper()
	return &manualClock{now: time.Now()}
}

// install swaps the seam vars and restores them on cleanup. The
// caller owns join-before-restore discipline for any goroutine
// reading the seam (the runWatchdogLoop precedent).
func (m *manualClock) install(t *testing.T) {
	t.Helper()
	on, ot := agentdNow, agentdNewTicker
	agentdNow = m.nowFn
	agentdNewTicker = m.newTickerFn
	t.Cleanup(func() {
		agentdNow, agentdNewTicker = on, ot
	})
}

func (m *manualClock) nowFn() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *manualClock) newTickerFn(time.Duration) (<-chan time.Time, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mt := &manualTicker{ch: make(chan time.Time, 64)}
	m.tickers = append(m.tickers, mt)
	return mt.ch, func() {}
}

// tick delivers one tick at the current fake now to every live ticker
// (buffered — the loop consumes at its pace; the test synchronizes on
// OBSERVABLE OUTCOMES, not channel drains).
func (m *manualClock) tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mt := range m.tickers {
		select {
		case mt.ch <- m.now:
		default: // a stalled consumer is a bug the outcome assert catches
		}
	}
}

// tickerCount is the locked count of live fakes (tests synchronize on
// the CONSUMER having built its ticker before flooding ticks — a tick
// delivered before the ticker exists is silently lost).
func (m *manualClock) tickerCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tickers)
}

// step moves the fake now WITHOUT delivering a tick — for asserting
// window arithmetic through direct calls (e.g. a vitals gather) while
// the loop under test must not receive another fire moment.
func (m *manualClock) step(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

// advance steps the fake now and delivers a tick (the modeled
// world-advance: a poll interval passing).
func (m *manualClock) advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
	m.tick()
}

// TestClockSeam_ProductionDefaults pins the wiring: the seam vars are
// the stdlib behaviors — a production swap to a silent fake (the
// #1532 hazard inverted: a clock that never ticks) fails here.
func TestClockSeam_ProductionDefaults(t *testing.T) {
	// now: within 5s of the real clock (same source).
	delta := time.Since(agentdNow())
	assert.LessOrEqual(t, delta, 5*time.Second, "agentdNow must read the real wall clock in production")

	// ticker: a real ticking channel (delivers within 1s at 10ms).
	ch, stop := agentdNewTicker(10 * time.Millisecond)
	defer stop()
	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatal("agentdNewTicker must be a real ticker in production")
	}

}

// TestClockSeam_ManualClockFake proves the fake itself: ticks fire in
// order and advance moves now — the deterministic substrate the site
// tests rely on.
func TestClockSeam_ManualClockFake(t *testing.T) {
	mc := newManualClock(t)
	mc.install(t)

	base := mc.nowFn()
	ch, stop := agentdNewTicker(time.Hour) // interval is IRRELEVANT under the fake
	defer stop()

	mc.advance(10 * time.Second)
	select {
	case got := <-ch:
		assert.Equal(t, base.Add(10*time.Second), got, "the tick carries the advanced fake now, in order")
	default:
		t.Fatal("advance must deliver a tick")
	}

	assert.Equal(t, base.Add(10*time.Second), mc.nowFn(), "world-advance is explicit — only advance() moves the fake")
}
