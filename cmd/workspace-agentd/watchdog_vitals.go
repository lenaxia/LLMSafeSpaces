// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// watchdog_vitals.go — corroborating evidence for the health-watchdog.
//
// Incident 2026-08-15: the watchdog killed a HEALTHY opencode six times in
// one evening. Root cause chain: the workspace ran heavy builds under a
// 2-CPU cgroup quota (~11% of periods throttled, 1300s cumulative throttle
// time), opencode's single JS event loop got starved, /global/health probes
// exceeded the 4s timeout three times in a row, and the watchdog concluded
// "hung" and SIGTERMed a process that was making progress. Each kill aborted
// in-flight LLM turns mid-stream. The busy-session deferral could not save
// the last turn: after watchdogMaxDeferrals polls the restart is FORCED
// despite busy sessions, on timeout evidence alone.
//
// This file provides the missing discriminator between the two states that
// probe timeouts cannot distinguish:
//
//	HUNG     — event loop deadlocked; it will never answer again. Restart
//	           is the ONLY remedy. This is what issue #807 exists for.
//	STARVED  — event loop alive and making progress, but CPU-starved by
//           cgroup quota contention (builds, tests, parallel turns).
//           Restart is strictly harmful: it kills live sessions AND adds
//           restart load to an already saturated CPU budget.
//
// Evidence gathered (one ~3s sample):
//
//  1. TCP dial to the agent port. The kernel completes handshakes for a
//     listening socket regardless of whether the application ever accepts,
//     so "dial refused" is decisive (nothing is listening — server crashed
//     or process gone) but "dial succeeded" is NOT proof of liveness.
//  2. CPU ticks (utime+stime from /proc/<pid>/stat) across the sample
//     window. A loop that is running — even at 1% of its fair share —
//     accumulates scheduler ticks. A deadlocked loop accrues ~zero. This
//     is the primary discriminator.
//  3. cgroup cpu.stat throttled_usec delta — informational corroboration
//     for logs ("the box was throttling while this happened").
//
// Known limitation (accepted, documented): a hot infinite JS loop and a
// genuinely busy loop are indistinguishable from inside the pod — both
// advance CPU. We side with NOT killing: a false negative (hot-loop hang
// persists until an operator intervenes) is recoverable; a false positive
// (healthy busy process killed) is the incident this file prevents. The
// watchdog logs a stand-down warning after sustained suppression so the
// condition is visible in logs and metrics.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// vitalsSampleWindow is how long the gatherer watches the agent process's
// CPU counter. Long enough for a starved-but-scheduled loop to accumulate
// several ticks; short enough not to stall the 5s watchdog poll loop
// meaningfully (refreshOnce already blocks up to readinessRefreshTimeout).
var vitalsSampleWindow = 3 * time.Second

// vitalsDialTimeout bounds the TCP corroboration dial. The kernel answers
// a localhost SYN in microseconds even under heavy CPU throttling, so a
// generous timeout avoids classifying a live listener as unreachable.
var vitalsDialTimeout = 2 * time.Second

// cpuFlatTicks is the CPU-tick delta below which the event loop counts as
// idle over vitalsSampleWindow. Linux user_hz is virtually always 100, so
// one tick is 10ms of CPU. A deadlocked loop accrues 0–1 ticks; anything
// scheduled at all accrues several. Ticks are compared directly (never
// converted to seconds) so the decision is independent of the actual
// user_hz — the epsilon just needs to scale with the window.
const cpuFlatTicks = 2.0

// verdict is the outcome of classifying a vitalSigns sample.
type verdict int

const (
	// verdictHung: evidence says opencode will not recover on its own —
	// restart. Callers should fire the watchdog.
	verdictHung verdict = iota
	// verdictStarved: evidence says opencode is alive and making progress
	// under contention. Callers must NOT restart; restart is the harm the
	// corroboration exists to prevent.
	verdictStarved
	// verdictUnknown: evidence could not be gathered (no pid, /proc read
	// failure, pid churn mid-sample). Callers should proceed with the
	// PRE-corroboration behavior (fire) so a bug in this file can never
	// make the watchdog toothless.
	verdictUnknown
)

// vitalSigns is one corroborating sample taken when the watchdog is about
// to fire. Zero value = no evidence; classify maps it to verdictUnknown.
type vitalSigns struct {
	// tcpOpen: a TCP handshake to the agent port completed. NOT proof the
	// application is serving — the kernel completes handshakes for
	// listening sockets from the accept backlog.
	tcpOpen bool
	// tcpRefused: the dial got ECONNREFUSED — nothing is listening.
	// Decisive: the server is gone even if the process lingers.
	tcpRefused bool
	// cpuKnown: cpuDeltaTicks is meaningful. False when the agent pid was
	// unavailable, /proc was unreadable, or the pid changed mid-sample
	// (restart in flight makes the delta meaningless).
	cpuKnown bool
	// cpuDeltaTicks: utime+stime ticks accrued by the agent process during
	// the sample window. Valid iff cpuKnown.
	cpuDeltaTicks float64
	// cpuErr describes why CPU evidence is missing, when it is.
	cpuErr string
	// throttleDeltaUS: cgroup cpu.stat throttled_usec delta across the
	// sample window. Informational only (logs/metrics), never a gate —
	// throttle presence does not distinguish hung from starved.
	throttleDeltaUS float64
}

// classify turns a vitalSigns sample into a watchdog decision.
//
// Precedence (first match wins):
//
//  1. tcpRefused           → HUNG   (nothing listening; server crashed or
//     process gone — restart is the remedy)
//  2. !cpuKnown            → UNKNOWN (fall back to pre-corroboration
//     behavior: fire)
//  3. cpuDelta < flat eps  → HUNG   (loop idle while the kernel queues
//     connections — deadlock)
//  4. otherwise            → STARVED (loop advancing: making progress or
//     hot-looping; restart is harm, see
//     file-doc "known limitation")
func (v vitalSigns) classify() (verdict, string) {
	switch {
	case v.tcpRefused:
		return verdictHung, "tcp dial refused — nothing listening on the agent port"
	case !v.cpuKnown:
		reason := "cpu evidence unavailable"
		if v.cpuErr != "" {
			reason += " (" + v.cpuErr + ")"
		}
		return verdictUnknown, reason
	case v.cpuDeltaTicks < cpuFlatTicks:
		return verdictHung, fmt.Sprintf("event loop idle (+%.0f CPU ticks over the sample) while connections go unanswered — deadlock suspected", v.cpuDeltaTicks)
	default:
		return verdictStarved, fmt.Sprintf("event loop advancing (+%.0f CPU ticks over the sample) — starved or busy, not hung", v.cpuDeltaTicks)
	}
}

// vitalsGatherer produces one vitalSigns sample. refreshIsHealthyLoop calls
// it only at would-fire moments (never on the happy path), so the ~3s
// sample cost is paid at most once per unhealthy episode poll. nil means
// corroboration is unavailable (tests, partially-wired deps) and the
// watchdog keeps its pre-corroboration semantics.
type vitalsGatherer interface {
	gather(ctx context.Context) vitalSigns
}

// procVitalsGatherer is the production gatherer: TCP-dials the agent port
// and samples the supervised process's CPU counter from /proc.
type procVitalsGatherer struct {
	addr         string
	pidFn        func() int
	dialTimeout  time.Duration
	sampleWindow time.Duration
}

// newProcVitalsGatherer builds the production gatherer. pidFn returns the
// current agent pid (or 0 when no child is supervised); it is re-queried
// after the sample window so a restart mid-sample invalidates the delta.
func newProcVitalsGatherer(addr string, pidFn func() int) *procVitalsGatherer {
	return &procVitalsGatherer{
		addr:         addr,
		pidFn:        pidFn,
		dialTimeout:  vitalsDialTimeout,
		sampleWindow: vitalsSampleWindow,
	}
}

// gather implements vitalsGatherer. Errors never propagate — a vitalSigns
// with cpuKnown=false is the error channel, and classify routes it to
// verdictUnknown so the watchdog degrades to its old behavior.
func (g *procVitalsGatherer) gather(ctx context.Context) vitalSigns {
	var v vitalSigns
	v.tcpOpen, v.tcpRefused = g.probeTCP()

	pid := g.pidFn()
	if pid <= 0 {
		v.cpuErr = "no agent pid available"
		return v
	}
	t1, err := readProcCPUTicks(pid)
	if err != nil {
		v.cpuErr = err.Error()
		return v
	}
	throttleBefore := readCgroupThrottledUS()

	timer := time.NewTimer(g.sampleWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		v.cpuErr = "context canceled during sample"
		return v
	case <-timer.C:
	}

	// The supervisor may have replaced the child while we sampled. A delta
	// across two different processes is garbage — invalidate instead.
	if pidNow := g.pidFn(); pidNow != pid {
		v.cpuErr = fmt.Sprintf("agent pid changed during sample (%d → %d): restart in flight", pid, pidNow)
		return v
	}
	t2, err := readProcCPUTicks(pid)
	if err != nil {
		v.cpuErr = err.Error()
		return v
	}

	v.cpuDeltaTicks = t2 - t1
	v.cpuKnown = true
	v.throttleDeltaUS = readCgroupThrottledUS() - throttleBefore
	return v
}

// probeTCP dials the agent address once. Returns (open, refused):
//
//	open=true   — handshake completed (kernel-level listener alive)
//	refused=true — ECONNREFUSED: nothing listening, decisive
//	both false  — other failure (typically timeout with a full accept
//	              backlog, i.e. overload — falls through to CPU evidence)
func (g *procVitalsGatherer) probeTCP() (open, refused bool) {
	conn, err := net.DialTimeout("tcp", g.addr, g.dialTimeout)
	if err == nil {
		_ = conn.Close()
		return true, false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return false, true
	}
	return false, false
}

// readProcCPUTicks returns utime+stime (in clock ticks) for pid from
// /proc/<pid>/stat. The comm field can contain spaces and parens, so
// parsing starts after the LAST ')'.
func readProcCPUTicks(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 > len(s) {
		return 0, fmt.Errorf("parse /proc/%d/stat: no comm terminator", pid)
	}
	// Fields after "(comm)": rest[0]=state(3), rest[11]=utime(14),
	// rest[12]=stime(15) — 1-indexed proc(5) field numbers.
	rest := strings.Fields(s[idx+2:])
	if len(rest) < 13 {
		return 0, fmt.Errorf("parse /proc/%d/stat: short field list (%d)", pid, len(rest))
	}
	utime, err1 := strconv.ParseFloat(rest[11], 64)
	stime, err2 := strconv.ParseFloat(rest[12], 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("parse /proc/%d/stat cpu fields: utime=%v stime=%v", pid, err1, err2)
	}
	return utime + stime, nil
}

// readCgroupThrottledUS reads throttled_usec from the cgroup v2 cpu.stat.
// Returns 0 on any failure — the value is informational only.
func readCgroupThrottledUS() float64 {
	data, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "throttled_usec ") {
			us, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "throttled_usec ")), 64)
			if err != nil {
				return 0
			}
			return us
		}
	}
	return 0
}
