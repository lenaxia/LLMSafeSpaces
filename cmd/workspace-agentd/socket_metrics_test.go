// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_metrics_test.go — US-2 (design 0051): the `metrics` control-socket
// method returns the WORKSPACE container's cgroup numbers, read by the
// supervisor (whose own cgroup IS the workspace container's — 0050 finding:
// a sidecar reading its own cgroup gets the wrong container). US-1 shipped
// the reserved envelope; these tests pin the real field set.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeCgroupFixture lays out a minimal cgroup v2 tree and returns a reader
// pointed at it.
func writeCgroupFixture(t *testing.T, memoryCurrent, memoryMax, cpuStat string) *cgroupV2Reader {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"memory.current": memoryCurrent,
		"memory.max":     memoryMax,
		"cpu.stat":       cpuStat,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	return &cgroupV2Reader{
		memoryCurrentPath: filepath.Join(dir, "memory.current"),
		memoryMaxPath:     filepath.Join(dir, "memory.max"),
		cpuStatPath:       filepath.Join(dir, "cpu.stat"),
	}
}

// TestCgroupV2Reader_FieldsPinned pins the v1 metrics field set (A.2):
// memory current/max, cpu usage_usec and throttled_usec. This is the
// frozen envelope the sidecar consumes; changing it is a protocol change.
func TestCgroupV2Reader_FieldsPinned(t *testing.T) {
	r := writeCgroupFixture(t, "536870912\n", "2147483648\n",
		"usage_usec 4000000\nnr_periods 100\nnr_throttled 10\nthrottled_usec 250000\n")
	m := r.read()
	require.Equal(t, int64(536870912), m.MemoryCurrentBytes)
	require.Equal(t, int64(2147483648), m.MemoryMaxBytes)
	require.Equal(t, int64(4000000), m.CPUUsageUsec)
	require.Equal(t, int64(250000), m.CPUThrottledUsec)
}

// TestCgroupV2Reader_UnlimitedAndMissing covers the degenerate shapes a
// real cgroupfs produces: memory.max "max" (unlimited → 0), absent files,
// and a cpu.stat without throttled lines (v1-ish leftovers must not panic).
func TestCgroupV2Reader_UnlimitedAndMissing(t *testing.T) {
	r := writeCgroupFixture(t, "1024\n", "max\n", "usage_usec 7\n")
	m := r.read()
	require.Equal(t, int64(1024), m.MemoryCurrentBytes)
	require.Equal(t, int64(0), m.MemoryMaxBytes, "memory.max \"max\" means unlimited — reported as 0")
	require.Equal(t, int64(7), m.CPUUsageUsec)
	require.Equal(t, int64(0), m.CPUThrottledUsec)

	// Entirely absent files → zero-valued metrics, no error path.
	r2 := &cgroupV2Reader{
		memoryCurrentPath: t.TempDir() + "/nonexistent",
		memoryMaxPath:     t.TempDir() + "/nonexistent",
		cpuStatPath:       t.TempDir() + "/nonexistent",
	}
	m2 := r2.read()
	require.Equal(t, int64(0), m2.MemoryCurrentBytes)
	require.Equal(t, int64(0), m2.CPUUsageUsec)
}

// TestControlSocket_MetricsReservedEnvelopeWithoutSource pins the US-1
// behavior for a server wired without a metrics source: the reserved
// envelope ({"cgroup":{}}) — the shape A.6 golden tests froze.
func TestControlSocket_MetricsReservedEnvelopeWithoutSource(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	resp := mustDial(t, srv.addr(), `{"v":1,"id":7,"method":"metrics","params":{}}`)
	require.Equal(t, float64(7), resp["id"])
	res := resp["result"].(map[string]any)
	require.Equal(t, map[string]any{}, res["cgroup"])
}

// TestControlSocket_MetricsCarriesCgroupValues is the US-2 wire test: the
// supervisor-side reader's values appear verbatim under result.cgroup.
func TestControlSocket_MetricsCarriesCgroupValues(t *testing.T) {
	r := writeCgroupFixture(t, "111\n", "222\n", "usage_usec 333\nthrottled_usec 444\n")
	srv := newControlSocketServerWithProcAndMetrics(t, "127.0.0.1:0", &fakeRestartProc{}, r.read)
	go srv.serve()
	resp := mustDial(t, srv.addr(), `{"v":1,"id":9,"method":"metrics","params":{}}`)
	res := resp["result"].(map[string]any)
	cg := res["cgroup"].(map[string]any)
	require.Equal(t, float64(111), cg["memory_current_bytes"])
	require.Equal(t, float64(222), cg["memory_max_bytes"])
	require.Equal(t, float64(333), cg["cpu_usage_usec"])
	require.Equal(t, float64(444), cg["cpu_throttled_usec"])
}
