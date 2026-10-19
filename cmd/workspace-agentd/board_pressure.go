// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_pressure.go — the #1747 round-3 resource-pressure reporter
// (r1 SHOULD, landed minimal): agentd samples its own pod's CPU /
// memory / disk state from the cgroup + filesystem primitives
// (sysmetrics.go), applies STATIC thresholds with HYSTERESIS (r1
// posture — adaptive later), and pushes the sample over the seam to
// the engine, which stops claiming into a pressured workspace and
// surfaces the informational note on reads.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// boardPressureThresholds: enter/exit pairs (hysteresis — a dimension
// pressures at the high mark and clears only below the low mark; a
// sticky signal, never a flapping one).
type boardPressureThresholds struct {
	cpuEnter, cpuExit   float64 // fractions of the cgroup CPU quota rate
	memEnter, memExit   float64 // fractions of the memory limit
	diskEnter, diskExit float64 // fractions of the filesystem capacity
}

var boardPressureDefault = boardPressureThresholds{
	cpuEnter: 0.90, cpuExit: 0.75,
	memEnter: 0.90, memExit: 0.80,
	diskEnter: 0.90, diskExit: 0.85,
}

// boardPressureState carries the hysteresis latches (process-lifetime).
type boardPressureState struct {
	cpu, mem, disk bool
}

// sampleBoardPressure computes one sample from the sysmetrics
// primitives with the latched thresholds.
func sampleBoardPressure(st *boardPressureState, th boardPressureThresholds) map[string]any {
	cpuRate, cpuOK := boardCPUUtilizationRate()
	memRate, memOK := boardMemoryUtilizationRate()
	diskRate, diskOK := boardDiskUtilizationRate()

	if cpuOK {
		st.cpu = hysteresis(st.cpu, cpuRate, th.cpuEnter, th.cpuExit)
	}
	if memOK {
		st.mem = hysteresis(st.mem, memRate, th.memEnter, th.memExit)
	}
	if diskOK {
		st.disk = hysteresis(st.disk, diskRate, th.diskEnter, th.diskExit)
	}

	sample := map[string]any{
		"at":        time.Now().UTC().Format(time.RFC3339Nano),
		"cpu":       st.cpu,
		"memory":    st.mem,
		"disk":      st.disk,
		"cpu_rate":  round3(cpuRate),
		"mem_rate":  round3(memRate),
		"disk_rate": round3(diskRate),
	}
	return sample
}

// boardCPUUtilizationRate computes the pod's CPU utilization fraction
// over the inter-sample window (cumulative cpu.stat deltas against the
// quota rate). ok=false when the cgroup or quota is unreadable.
func boardCPUUtilizationRate() (float64, bool) {
	cur := getCPUUsage()
	if cur == nil || cur.LimitMicrosPerSec <= 0 {
		return 0, false
	}
	last := boardCPULastSample
	now := time.Now()
	boardCPULastSample = &cpuWindow{usage: cur.UsageMicros, at: now}
	if last == nil || last.usage <= 0 || cur.UsageMicros < last.usage {
		return 0, false // first sample (or counter reset): no window yet
	}
	elapsed := now.Sub(last.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	rate := float64(cur.UsageMicros-last.usage) / 1e6 / elapsed // cores used
	return rate / (float64(cur.LimitMicrosPerSec) / 1e6), true
}

type cpuWindow struct {
	usage int64
	at    time.Time
}

var boardCPULastSample *cpuWindow

// hysteresis latches: pressurize at enter, clear only below exit.
func hysteresis(latched bool, v, enter, exit float64) bool {
	if latched {
		return v >= exit
	}
	return v >= enter
}

// boardMemoryUtilizationRate is the working-set fraction of the limit.
func boardMemoryUtilizationRate() (float64, bool) {
	m := getMemoryUsage()
	if m == nil || m.TotalBytes <= 0 {
		return 0, false
	}
	return float64(m.UsedBytes) / float64(m.TotalBytes), true
}

// boardDiskUtilizationRate is the workspace filesystem's used fraction.
func boardDiskUtilizationRate() (float64, bool) {
	d := getDiskUsage()
	if d == nil || d.TotalBytes <= 0 {
		return 0, false
	}
	return float64(d.UsedBytes) / float64(d.TotalBytes), true
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}

// boardPressureLoop pushes one sample per interval until the context
// ends (the rearm-loop pattern: quiet failure, next tick retries).
func boardPressureLoop(ctx context.Context, log *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	st := &boardPressureState{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pushBoardPressure(ctx, log, st)
		}
	}
}

// pushBoardPressure resolves the automation deps and posts one sample.
func pushBoardPressure(ctx context.Context, log *slog.Logger, st *boardPressureState) {
	saToken, apiURL, workspaceID, err := automationDeps()
	if err != nil {
		return // automation not configured on this pod — nothing to do
	}
	sample := sampleBoardPressure(st, boardPressureDefault)
	body, err := json.Marshal(sample)
	if err != nil {
		return
	}
	client := opencode.NewLoopbackClient(apiURL, "")
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.BoardResourcePressure(cctx, saToken, workspaceID, body); err != nil && log != nil {
		log.Debug("board: resource-pressure push failed (next tick retries)", "error", err.Error())
	}
}
