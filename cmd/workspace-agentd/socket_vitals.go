// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_vitals.go — US-2 (design 0051): sidecar-mode watchdog
// corroboration over the control socket.
//
// Post-#892 the kill set is DEAD-LISTENER ONLY: HUNG requires
// tcpRefused + supervised pid alive + past boot grace. The sidecar can
// still gather ALL three without /proc: the TCP dial works over the
// shared netns, and pid/boot evidence comes from the supervisor's
// `status` (child_pid, last_restart_at — the supervisor's clock stamps
// child starts, and pod-shared clock makes the age comparison valid).
//
// CPU-delta evidence is honestly unavailable cross-container (/proc of
// another container's processes is not readable) and maps to
// cpuKnown=false — which only degrades suppression LABELS, never the
// lethal verdict (see watchdog_vitals.go's classify precedence: with the
// port open, !cpuKnown → UNKNOWN → suppress, which is the #892 policy).

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// socketVitalsGatherer implements vitalsGatherer for sidecar mode.
type socketVitalsGatherer struct {
	agentAddr     string // opencode's port (shared netns)
	cc            *controlClient
	dialTimeout   time.Duration
	statusTimeout time.Duration
}

func newSocketVitalsGatherer(agentAddr string, cc *controlClient) *socketVitalsGatherer {
	return &socketVitalsGatherer{
		agentAddr:     agentAddr,
		cc:            cc,
		dialTimeout:   vitalsDialTimeout,
		statusTimeout: 2 * time.Second,
	}
}

func (g *socketVitalsGatherer) gather(ctx context.Context) vitalSigns {
	var v vitalSigns
	v.tcpOpen, v.tcpRefused = g.probeTCP(ctx)

	st, err := g.status(ctx)
	if err != nil || st == nil {
		// No supervisor evidence at all. pidGone MUST be true: with a
		// refused dial, classify() treats an unproven pid (pidGone=false)
		// as ALIVE and returns HUNG — killing on zero evidence, the
		// exact #892 ban. pidGone=true routes the refused shape to
		// RESPAWN (suppress): when the probe itself is degraded, the
		// watchdog has no business firing.
		v.pidGone = true
		v.cpuErr = "supervisor status unavailable"
		if err != nil {
			v.cpuErr = "supervisor status unavailable: " + err.Error()
		}
		return v
	}

	if st.ChildPID <= 0 || st.ChildState == "stopped" {
		v.pidGone = true
		v.cpuErr = "supervisor reports no live child"
		return v
	}

	// Boot grace: the supervisor stamps every child start; a young
	// child with a refused dial is the respawn's port-not-yet-bound
	// window — never the watchdog's to kill (same rule as
	// procVitalsGatherer.childBootAt).
	if !st.LastRestartAt.IsZero() && time.Since(st.LastRestartAt) < vitalsBootGraceWindow {
		v.booting = true
	}

	// Cross-container: no CPU counter access. cpuKnown stays false —
	// classify() routes open-port shapes to UNKNOWN (suppress), and the
	// lethal refused+alive+past-boot shape does not need CPU evidence.
	v.cpuErr = "cpu evidence unavailable cross-container (sidecar mode)"
	return v
}

func (g *socketVitalsGatherer) status(ctx context.Context) (*controlStatus, error) {
	sctx, cancel := context.WithTimeout(ctx, g.statusTimeout)
	defer cancel()
	return g.cc.Status(sctx)
}

func (g *socketVitalsGatherer) probeTCP(ctx context.Context) (open, refused bool) {
	dialer := &net.Dialer{Timeout: g.dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", g.agentAddr)
	if err == nil {
		_ = conn.Close()
		return true, false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false, true
	}
	return false, false
}
