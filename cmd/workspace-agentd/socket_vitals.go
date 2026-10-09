// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_vitals.go — US-2 (design 0051): sidecar-mode watchdog
// corroboration over the control socket.
//
// Post-#892 the kill set is DEAD-LISTENER ONLY: HUNG requires
// tcpRefused + supervised pid alive + past boot grace. The sidecar gets
// all three without /proc: the TCP dial works over the shared netns,
// and pid/boot evidence comes from the supervisor's `status`
// (child_pid, last_restart_at — the supervisor's clock stamps child
// starts, and pod-shared clock makes the age comparison valid).
//
// CPU-delta evidence (#1632 fix #1): the supervisor's `vitals` method
// reads /proc/<child>/stat + cgroup cpu.stat in ITS pidns and reports
// the counters; this gatherer samples twice across the window and
// derives the delta, making cpuKnown=true (FLAT/STARVED distinguishable)
// in sidecar mode. An older supervisor (or a degraded socket) answers
// method_unknown/vitals_unavailable — degraded to cpuKnown=false, the
// pre-#1631 honest-UNKNOWN path. The lethal refused+alive+past-boot
// shape never needed CPU evidence, so the kill set is unchanged by
// construction.

import (
	"context"
	"errors"
	"fmt"
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
	// sampleWindow is the CPU-counter observation window (mirrors
	// procVitalsGatherer.sampleWindow; default vitalsSampleWindow, var
	// for tests).
	sampleWindow time.Duration
}

func newSocketVitalsGatherer(agentAddr string, cc *controlClient) *socketVitalsGatherer {
	return &socketVitalsGatherer{
		agentAddr:     agentAddr,
		cc:            cc,
		dialTimeout:   vitalsDialTimeout,
		statusTimeout: 2 * time.Second,
		sampleWindow:  vitalsSampleWindow,
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

	// CPU evidence (#1632 fix #1): two vitals samples, window apart.
	// Any pid disagreement (vs status, or between samples) is a restart
	// in flight — the delta would be garbage across two processes, and
	// crash recovery owns the lifecycle: pidGone routes refused shapes
	// to RESPAWN and open shapes to UNKNOWN. Cancellation means agentd
	// is shutting down: no evidence, no future in which acting on this
	// sample helps (same ruling as procVitalsGatherer).
	t1, err1 := g.vitals(ctx)
	if err1 != nil {
		v.cpuErr = "socket vitals unavailable: " + err1.Error()
		return v
	}
	if t1.ChildPID != st.ChildPID {
		v.pidGone = true
		v.cpuErr = fmt.Sprintf("agent pid changed between status and vitals (%d → %d): restart in flight", st.ChildPID, t1.ChildPID)
		return v
	}
	throttleBefore := t1.ThrottledUS

	timer := time.NewTimer(g.sampleWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		v.pidGone = true
		v.cpuErr = "context canceled during sample"
		return v
	case <-timer.C:
	}

	t2, err2 := g.vitals(ctx)
	if err2 != nil {
		v.cpuErr = "socket vitals unavailable: " + err2.Error()
		return v
	}
	if t2.ChildPID != t1.ChildPID {
		v.pidGone = true
		v.cpuErr = fmt.Sprintf("agent pid changed during sample (%d → %d): restart in flight", t1.ChildPID, t2.ChildPID)
		return v
	}

	v.cpuDeltaTicks = t2.CPUTicks - t1.CPUTicks
	v.cpuKnown = true
	v.throttleDeltaUS = t2.ThrottledUS - throttleBefore
	return v
}

func (g *socketVitalsGatherer) status(ctx context.Context) (*controlStatus, error) {
	sctx, cancel := context.WithTimeout(ctx, g.statusTimeout)
	defer cancel()
	return g.cc.Status(sctx)
}

// vitals fetches one instant sample with its own short deadline (the
// window between samples is where the time belongs, not in the fetch).
func (g *socketVitalsGatherer) vitals(ctx context.Context) (*controlVitals, error) {
	vctx, cancel := context.WithTimeout(ctx, g.statusTimeout)
	defer cancel()
	return g.cc.Vitals(vctx)
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
