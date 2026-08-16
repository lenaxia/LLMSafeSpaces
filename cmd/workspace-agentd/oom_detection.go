// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// oomKillsCounter tracks OOM kills per workspace for ops dashboards.
// Registered once at package level (Prometheus default registry rejects
// duplicates).
var oomKillsCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "workspace_oom_kills_total",
	Help: "Total number of opencode OOM kills (exit 137 / SIGKILL from OOM killer)",
}, []string{"workspace_id"})

// exitKind classifies how the opencode process exited.
type exitKind int

const (
	exitNormal exitKind = iota
	exitSigKill
	exitSigTerm
	exitCrash
)

// classifyExit determines the kind of process exit from the wait error
// and process state. Used by the supervisor to decide whether to treat
// the exit as a potential OOM kill.
func classifyExit(waitErr error) exitKind {
	if waitErr == nil {
		return exitNormal
	}
	// Check if the process was killed by a signal.
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		ps := exitErr.Sys()
		if ws, ok := ps.(syscall.WaitStatus); ok {
			if ws.Signaled() {
				switch syscall.Signal(ws.Signal()) {
				case syscall.SIGKILL:
					return exitSigKill
				case syscall.SIGTERM:
					return exitSigTerm
				}
				return exitCrash
			}
		}
	}
	return exitCrash
}

// isOOMExit returns true if the exit kind indicates a potential OOM kill.
// SIGKILL is the signal the Linux OOM killer sends; no other common
// path produces SIGKILL for a well-behaved process.
func isOOMExit(kind exitKind) bool {
	return kind == exitSigKill
}

// handleOOMExit is called from the supervisor's crash path when an OOM kill
// is detected. It writes the unified restart-reason marker (US-44.7),
// logs the restart-reason in real time, and increments the Prometheus
// counters (OOM kills + restarts).
//
// restartReasonMarkerPath is the path for the unified restart-reason marker
// consumed on next boot. It is passed in (rather than reading the package
// constant) so tests can target a tempdir.
//
// Worklog 371 H5: the separate OOM-specific marker (OOMMarkerPath +
// writeOOMMarker) was removed because it had zero read-side consumers
// across the controller, API, and frontend. The restart-reason marker
// (reason="oom") subsumes the useful information, and the OOM-specific
// exitCode/memoryLimit fields were dead data. The OOM kill is still
// recorded in workspace_oom_kills_total (below) and workspace_restarts_total
// (via pkgOpsMetrics.RecordRestart).
func handleOOMExit(workspaceID, restartReasonMarkerPath string) {
	if workspaceID == "" {
		workspaceID = "unknown"
	}
	log.Warn("opencode was killed by OOM killer (SIGKILL/exit 137)",
		zap.String("workspace_id", workspaceID))

	// US-44.7: write the generalized restart-reason marker and log it in
	// real time. This is the unified reason surface consumed by
	// logRestartReason on the next boot.
	if err := writeRestartReasonMarker(restartReasonMarkerPath, "oom", nil); err != nil {
		log.Error("failed to write restart-reason marker", zap.Error(err))
		pkgOpsMetrics.RecordMarkerWriteFailure(workspaceID, "oom")
	} else {
		logRestartReasonAtWrite("oom", nil, log.Core())
	}

	oomKillsCounter.WithLabelValues(workspaceID).Inc()
	pkgOpsMetrics.RecordRestart(workspaceID, "oom")
}

// workspaceIDFromEnv reads the WORKSPACE_ID environment variable set by
// the controller's pod builder. Returns empty string if not set (tests).
func workspaceIDFromEnv() string {
	return os.Getenv("WORKSPACE_ID")
}

// readCgroupMemoryCurrent reads the current memory usage from cgroup v2.
// Returns an error if the file is not available (non-Linux, cgroup v1 —
// see H4: cgroup v2 is a documented hard requirement; on v1 hosts the
// memory pressure monitor and the workspace_memory_bytes gauge silently
// produce nothing, surfaced by a warning log in the pressure monitor).
func readCgroupMemoryCurrent() (int64, error) {
	data, err := os.ReadFile("/sys/fs/cgroup/memory.current")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}
