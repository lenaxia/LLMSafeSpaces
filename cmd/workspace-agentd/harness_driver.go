// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// harness_driver.go — the internal harness seam behind statusz
// busy/progress derivation (pi-scoping report §6 item 3, S5).
//
// Pre-S5 every harness-derived status fact was opencode-shaped by
// construction: the HTTP+SSE machinery (OpenCodeClient + the SSE
// tracker + the provider cache + refreshIsHealthyLoop) IS the opencode
// driver, wired inline. The seam keeps that machinery as the opencode
// implementation — unchanged, byte-identical output, the golden
// pod-spec diff's runtime twin — and adds the pi implementation over
// the supervise-pi state aggregator (S2's JSONL-RPC event pump, folded
// into the SAME parity shape).
//
// Two consumption topologies, one interface:
//
//   - IN-PROCESS (supervise-pi): the supervisor's own driver —
//     piSupervisor.HarnessStatus(), the aggregator snapshot directly.
//   - SIDECAR: the sidecar's poller — the snapshot crosses the
//     design-0051 control socket (method `harness_status`, the A.4
//     capability-typed posture) and lands in a cached store that
//     statusz/readyz/healthz read.
//
// opencode pods never construct a driver: AGENTD_HARNESS is absent
// (the controller wires it only for non-default harnesses — the
// mixed-fleet-compatible encoding), and absent = the existing inline
// opencode path, untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// envAgentdHarness carries the workspace's spec.harness into the
// agentd processes that need to branch on it (the sidecar's statusz
// derivation and boot config writer). Wired by the controller ONLY for
// non-default harnesses: absent = opencode, so a default pod's env —
// and every behavior keyed on it — is byte-identical pre/post-S5.
const envAgentdHarness = "AGENTD_HARNESS"

// harnessFromEnv resolves the harness selector, defaulting to opencode.
func harnessFromEnv(getenv func(string) string) string {
	h := getenv(envAgentdHarness)
	if h == "" {
		return "opencode"
	}
	return h
}

// harnessIsPi reports whether THIS process serves a pi workspace.
func harnessIsPi(getenv func(string) string) bool {
	return harnessFromEnv(getenv) == "pi"
}

// harnessStatusSource renders the statusz parity surface for a
// non-opencode harness (the S2 shape: sessions, busy ages, last
// progress, aggregate context — already agentd-typed). The opencode
// implementation is the existing inline statusz path; this interface
// exists for the drivers whose harness has no HTTP+SSE surface.
type harnessStatusSource interface {
	HarnessStatus() harnessStatusSnapshot
}

// harnessStatusStore is a cached, goroutine-safe snapshot holder: the
// sidecar's poller writes, statusz/readyz read. Zero value is an empty
// (never-fetched) store; readers treat empty as evidence-unavailable.
type harnessStatusStore struct {
	mu        sync.RWMutex
	snap      harnessStatusSnapshot
	fetchedAt time.Time
	ok        bool
}

func (s *harnessStatusStore) store(snap harnessStatusSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
	s.fetchedAt = time.Now()
	s.ok = true
}

// snapshot returns the last poll's result and whether one ever
// succeeded (ok=false ⇒ treat every field as unknown, never as zero).
func (s *harnessStatusStore) snapshot() (harnessStatusSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap, s.ok
}

// harnessStatusPoller is the sidecar's pi driver: it polls the
// supervisor's control socket (`harness_status`) on a fixed cadence
// into a harnessStatusStore. Implements the background-loop shape the
// opencode path fills with refreshIsHealthyLoop/SSE-subscribe.
type harnessStatusPoller struct {
	addr  string
	store *harnessStatusStore
	every time.Duration
}

func newHarnessStatusPoller(addr string, every time.Duration) *harnessStatusPoller {
	if every <= 0 {
		every = 5 * time.Second
	}
	return &harnessStatusPoller{addr: addr, store: &harnessStatusStore{}, every: every}
}

// run polls until ctx ends. A failed poll NEVER clears the last good
// snapshot (a single control-socket hiccup must not blank statusz) —
// the fetchedAt staleness is observable via StatuszResponse.Uptime
// drift and the supervisor-status poller's own degrade surface.
func (p *harnessStatusPoller) run(ctx context.Context) {
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		p.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *harnessStatusPoller) pollOnce(ctx context.Context) {
	snap, err := fetchHarnessStatus(ctx, p.addr)
	if err != nil {
		return
	}
	p.store.store(snap)
}

// fetchHarnessStatus performs one control-socket `harness_status`
// round trip. One JSON request per TCP connection (the v1 socket
// contract); the response's result field carries the snapshot.
func fetchHarnessStatus(ctx context.Context, addr string) (harnessStatusSnapshot, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return harnessStatusSnapshot{}, fmt.Errorf("harness_status dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	req := controlRequest{V: intPtr(controlProtocolVersion), Method: "harness_status"}
	payload, err := json.Marshal(req)
	if err != nil {
		return harnessStatusSnapshot{}, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return harnessStatusSnapshot{}, fmt.Errorf("harness_status write: %w", err)
	}
	var resp controlResponse
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return harnessStatusSnapshot{}, fmt.Errorf("harness_status decode: %w", err)
	}
	if resp.Error != nil {
		return harnessStatusSnapshot{}, fmt.Errorf("harness_status: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return harnessStatusSnapshot{}, err
	}
	var snap harnessStatusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return harnessStatusSnapshot{}, fmt.Errorf("harness_status result decode: %w", err)
	}
	return snap, nil
}

func intPtr(i int) *int { return &i }

// piStatuszResponse renders the pi-harness statusz body — the same
// StatuszResponse shape the opencode path emits, sourced from the
// snapshot. Honest values only: pi has no provider-connectivity HTTP
// surface (models.json IS the config), so Connected stays empty and
// Configured carries the snapshot's count; readiness is the poll
// itself succeeding (a reachable supervisor with persisted sessions is
// the pi pod's serving state — busy children are healthy, not
// unready).
func piStatuszResponse(snap harnessStatusSnapshot, ok bool, startedAt time.Time, mem func() *agentd.MemoryUsage, cpu func() *agentd.CPUUsage, disk func() *agentd.DiskUsage) *agentd.StatuszResponse {
	resp := &agentd.StatuszResponse{
		Healthy:   ok,
		Ready:     ok,
		AgentType: "pi",
		// AgentVersion rides the snapshot when the supervisor knows it
		// (pi --version surfaced at boot); empty = unreported.
		AgentVersion:        snap.AgentVersion,
		Sessions:            snap.Sessions,
		SessionsActive:      snap.SessionsActive,
		SessionsOK:          &ok,
		OldestBusySeconds:   snap.OldestBusySeconds,
		BusyAges:            snap.BusyAges,
		LastProgressSeconds: snap.LastProgressSeconds,
		Context:             snap.Context,
		UptimeSeconds:       int(time.Since(startedAt).Seconds()),
	}
	if mem != nil {
		resp.Memory = mem()
	}
	if cpu != nil {
		resp.CPU = cpu()
	}
	if disk != nil {
		resp.Disk = disk()
	}
	if resp.BusyAges == nil {
		resp.BusyAges = map[string]int{}
	}
	return resp
}
