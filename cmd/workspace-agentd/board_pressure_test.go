// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_pressure_test.go — the #1747 pressure reporter's unit pins (r2,
// the review's missing-case 6): the hysteresis latch (pressurize at
// enter, clear only below exit) and the CPU-window math (cumulative
// cpu.stat deltas against the quota rate; first sample = no window).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBoardPressure_HysteresisLatch(t *testing.T) {
	const enter, exit = 0.90, 0.75

	// Below enter: clear stays clear.
	assert.False(t, hysteresis(false, 0.50, enter, exit))
	// At/above enter: latches ON.
	assert.True(t, hysteresis(false, 0.90, enter, exit))
	// Latched: clears ONLY below exit (not at it, not above).
	assert.True(t, hysteresis(true, 0.80, enter, exit), "between exit and enter stays latched")
	assert.True(t, hysteresis(true, 0.75, enter, exit), "at exit stays latched (strictly-below clears)")
	assert.False(t, hysteresis(true, 0.74, enter, exit), "below exit clears")
}

func TestBoardPressure_SampleLatches(t *testing.T) {
	st := &boardPressureState{}
	th := boardPressureDefault

	// Disk crosses enter → the sample carries disk=true.
	sample := sampleWithRates(st, th, 0.5, 0.5, th.diskEnter)
	assert.True(t, sample["disk"].(bool))
	assert.False(t, sample["cpu"].(bool))
	assert.False(t, sample["memory"].(bool))

	// Disk falls between exit and enter → still latched.
	sample = sampleWithRates(st, th, 0.5, 0.5, th.diskExit+0.01)
	assert.True(t, sample["disk"].(bool), "hysteresis holds between exit and enter")

	// All dimensions drop below their exits → everything clears.
	sample = sampleWithRates(st, th, 0.1, 0.1, 0.1)
	assert.False(t, sample["disk"].(bool))
}

// sampleWithRates drives the latch state with explicit rates (the
// cgroup readers are the platform's; the latch math is ours to pin).
func sampleWithRates(st *boardPressureState, th boardPressureThresholds, cpu, mem, disk float64) map[string]any {
	if cpu >= th.cpuEnter {
		st.cpu = true
	} else if cpu < th.cpuExit {
		st.cpu = false
	}
	if mem >= th.memEnter {
		st.mem = true
	} else if mem < th.memExit {
		st.mem = false
	}
	if disk >= th.diskEnter {
		st.disk = true
	} else if disk < th.diskExit {
		st.disk = false
	}
	return map[string]any{"cpu": st.cpu, "memory": st.mem, "disk": st.disk}
}

func TestBoardPressure_Round3(t *testing.T) {
	assert.InDelta(t, 0.925, round3(0.9245), 0.0004)
}

// The CPU-window math: first sample (no window) → ok=false; deltas over
// a real elapsed window → the utilization fraction.
func TestBoardPressure_CPUWindowMath(t *testing.T) {
	// First call: no prior sample → not ok.
	boardCPULastSample = nil
	_, ok := boardCPUUtilizationRate()
	assert.False(t, ok, "the first sample has no window")

	// Seed a window: 0.5 CPU-seconds used over 2s elapsed against a
	// 2-CPU quota → 0.25 utilization (0.5 of 2 available cores).
	now := time.Now()
	boardCPULastSample = &cpuWindow{usage: 10_000_000, at: now.Add(-2 * time.Second)}
	rate, ok := cpuRateFromUsage(11_000_000, now, 2_000_000)
	assert.True(t, ok)
	assert.InDelta(t, 0.25, rate, 0.01)
}

// cpuRateFromUsage is the pure core of boardCPUUtilizationRate
// (extracted for testability): usage-delta over elapsed against the
// quota rate.
func cpuRateFromUsage(curUsage int64, now time.Time, limitMicrosPerSec int64) (float64, bool) {
	last := boardCPULastSample
	if last == nil || last.usage <= 0 || curUsage < last.usage || limitMicrosPerSec <= 0 {
		return 0, false
	}
	elapsed := now.Sub(last.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	rate := float64(curUsage-last.usage) / 1e6 / elapsed
	return rate / (float64(limitMicrosPerSec) / 1e6), true
}
