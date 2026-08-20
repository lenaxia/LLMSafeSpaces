// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// socket_vitals_test.go — US-2 (design 0051): sidecar-mode watchdog
// corroboration via the control socket.
//
// Post-#892 the kill set is DEAD-LISTENER ONLY: verdict HUNG requires
// tcpRefused + supervised pid alive + past boot grace. In sidecar mode the
// pid/boot evidence comes from the supervisor's `status` (child_pid,
// last_restart_at) instead of /proc — the socket gatherer must preserve
// the EXACT verdict matrix, or the sidecar either loses the only lethal
// verdict (hangs unrecoverable) or gains unjustified ones (the 2026-08-15
// kill-churn incident class). CPU-delta evidence is honestly unavailable
// cross-container and maps to cpuKnown=false.

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// vitalsFixture wires a gatherer against a real socket server (fake proc
// state) plus a real TCP target for the agent-port dial.
type vitalsFixture struct {
	g    *socketVitalsGatherer
	proc *fakeRestartProc
	srv  *controlSocketServer
	ln   net.Listener // agent-port stand-in; close to simulate refused
}

func newVitalsFixture(t *testing.T, override *procStateOverride) *vitalsFixture {
	t.Helper()
	proc := &fakeRestartProc{}
	if override != nil {
		proc.overrideState.Store(override)
	}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	g := newSocketVitalsGatherer(ln.Addr().String(), newControlClient(srv.addr()))
	return &vitalsFixture{g: g, proc: proc, srv: srv, ln: ln}
}

// TestSocketVitals_RefusedAlivePastBoot_HUNG: the ONLY lethal shape —
// listener refused, status reports a live pid whose last start is older
// than the boot grace window.
func TestSocketVitals_RefusedAlivePastBoot_HUNG(t *testing.T) {
	f := newVitalsFixture(t, &procStateOverride{
		pid: 123, state: "running",
		lastRestartAt: time.Now().Add(-10 * time.Minute),
	})
	require.NoError(t, f.ln.Close()) // refuse the agent port

	v := f.g.gather(context.Background())
	verdict, why := v.classify()
	require.Equal(t, verdictHung, verdict, "refused + live pid + past boot must stay lethal: %s", why)
}

// TestSocketVitals_RefusedBooting_RESPAWN: refused dial against a young
// child (last_restart_at within the 180s boot grace) is the respawn's
// port-not-yet-bound window — never the watchdog's to kill.
func TestSocketVitals_RefusedBooting_RESPAWN(t *testing.T) {
	f := newVitalsFixture(t, &procStateOverride{
		pid: 123, state: "running",
		lastRestartAt: time.Now().Add(-5 * time.Second),
	})
	require.NoError(t, f.ln.Close())

	v := f.g.gather(context.Background())
	verdict, _ := v.classify()
	require.Equal(t, verdictRespawn, verdict)
}

// TestSocketVitals_RefusedPidGone_RESPAWN: no child (child_pid 0) with a
// refused dial means crash recovery owns the lifecycle.
func TestSocketVitals_RefusedPidGone_RESPAWN(t *testing.T) {
	f := newVitalsFixture(t, &procStateOverride{pid: 0, state: "stopped"})
	require.NoError(t, f.ln.Close())

	v := f.g.gather(context.Background())
	verdict, _ := v.classify()
	require.Equal(t, verdictRespawn, verdict)
}

// TestSocketVitals_OpenPort_UnknownSuppress: a listener that answers but
// no CPU evidence (cross-container) must suppress — killing without
// evidence is banned (#892).
func TestSocketVitals_OpenPort_UnknownSuppress(t *testing.T) {
	f := newVitalsFixture(t, &procStateOverride{
		pid: 123, state: "running",
		lastRestartAt: time.Now().Add(-10 * time.Minute),
	})
	// Port stays OPEN.

	v := f.g.gather(context.Background())
	require.False(t, v.cpuKnown)
	verdict, _ := v.classify()
	require.Equal(t, verdictUnknown, verdict)
}

// TestSocketVitals_SocketDown_NeverLethal: supervisor unreachable (status
// fails) — no pid evidence at all, so no verdict may be HUNG regardless
// of the dial outcome. Suppression is the only lawful answer.
func TestSocketVitals_SocketDown_NeverLethal(t *testing.T) {
	// Agent port refused AND socket dead: the worst-evidence case.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	agentAddr := ln.Addr().String()
	require.NoError(t, ln.Close())

	deadPort := freeTCPPort(t)
	cc := newControlClient(fmt.Sprintf("127.0.0.1:%d", deadPort))
	cc.timeout = 300 * time.Millisecond
	g := newSocketVitalsGatherer(agentAddr, cc)

	v := g.gather(context.Background())
	verdict, why := v.classify()
	require.NotEqual(t, verdictHung, verdict,
		"no status evidence must never be lethal (banned: kill without evidence): %s", why)
	require.NotEqual(t, verdictFlat, verdict)
}

// TestSocketVitals_LethalMatrixParity is the parity pin: for every
// (tcpRefused, pidAlive, booting) combination, socket-mode classification
// matches the in-container procVitalsGatherer semantics (constructed from
// the same vitalSigns fields).
func TestSocketVitals_LethalMatrixParity(t *testing.T) {
	cases := []struct {
		name           string
		refused, alive bool
		booting        bool
		want           verdict
	}{
		{"refused alive past boot", true, true, false, verdictHung},
		{"refused alive booting", true, true, true, verdictRespawn},
		{"refused dead", true, false, false, verdictRespawn},
		{"open no cpu evidence", false, true, false, verdictUnknown},
		{"open dead", false, false, false, verdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := vitalSigns{
				tcpOpen:    !tc.refused,
				tcpRefused: tc.refused,
				pidGone:    !tc.alive,
				booting:    tc.booting,
				cpuKnown:   false,
			}
			got, _ := v.classify()
			require.Equal(t, tc.want, got)
		})
	}
}
