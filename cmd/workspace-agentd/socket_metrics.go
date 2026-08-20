// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_metrics.go — US-2 (design 0051 A.2 `metrics`): the supervisor
// reads the WORKSPACE container's own cgroup (its cgroup IS the workspace
// container's — 0050 finding: a sidecar reading its own cgroup gets the
// wrong container) and serves the numbers over the control socket. The
// sidecar consumes them for statusz, the pressure monitor, and the ops
// metrics ticker.

import (
	"os"
	"strconv"
	"strings"
)

// cgroupMetrics is the v1 metrics field set (A.2 — frozen envelope;
// changing a field is a protocol change under A.5).
type cgroupMetrics struct {
	MemoryCurrentBytes int64 `json:"memory_current_bytes"`
	MemoryMaxBytes     int64 `json:"memory_max_bytes"` // 0 = unlimited
	CPUUsageUsec       int64 `json:"cpu_usage_usec"`
	CPUThrottledUsec   int64 `json:"cpu_throttled_usec"`
}

// cgroupV2Reader reads the four v1 fields from an injectable cgroupfs
// layout. Production points at /sys/fs/cgroup; tests at a fixture dir.
type cgroupV2Reader struct {
	memoryCurrentPath string
	memoryMaxPath     string
	cpuStatPath       string
}

// newWorkspaceCgroupReader is the production reader: the supervisor's own
// container's cgroup v2 files.
func newWorkspaceCgroupReader() *cgroupV2Reader {
	return &cgroupV2Reader{
		memoryCurrentPath: "/sys/fs/cgroup/memory.current",
		memoryMaxPath:     "/sys/fs/cgroup/memory.max",
		cpuStatPath:       "/sys/fs/cgroup/cpu.stat",
	}
}

// read returns the current values. Absent files and unparsable content
// degrade to zero fields — metrics are diagnostics; a half-populated
// envelope is more useful than an error the caller cannot act on.
func (r *cgroupV2Reader) read() *cgroupMetrics {
	m := &cgroupMetrics{}
	if data, err := os.ReadFile(r.memoryCurrentPath); err == nil {
		m.MemoryCurrentBytes = parseFirstInt(data)
	}
	if data, err := os.ReadFile(r.memoryMaxPath); err == nil {
		if trimmed := strings.TrimSpace(string(data)); trimmed != "max" {
			m.MemoryMaxBytes = parseFirstInt(data)
		}
	}
	if data, err := os.ReadFile(r.cpuStatPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			switch fields[0] {
			case "usage_usec":
				m.CPUUsageUsec = parseFirstInt([]byte(fields[1]))
			case "throttled_usec":
				m.CPUThrottledUsec = parseFirstInt([]byte(fields[1]))
			}
		}
	}
	return m
}

func parseFirstInt(data []byte) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
