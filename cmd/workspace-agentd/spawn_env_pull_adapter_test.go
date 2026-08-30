// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// US-70.1 (design 0057 R2/I4) adapter tests: the spawn-time pull state
// machine (fresh-pull-wins, failure-keeps-last-good + degrade), terminal
// spawned_rev derivation (effective delta at the point of consumption —
// never the server-advertised rev), and the control-socket status
// reporting of the degrade state.

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// spawnEnvTestServer aliases the httptest server type so the adapter
// tests can close it (revoking a once-healthy pull source).
type spawnEnvTestServer = httptest.Server

func fastTiming() (bound, attempt, retryGap time.Duration) {
	return 500 * time.Millisecond, 100 * time.Millisecond, 20 * time.Millisecond
}

func staticEnv(env ...string) func() []string {
	return func() []string { return env }
}

func mkAdapterWithPuller(t *testing.T, delta map[string]string, password string) (*managedProcAdapter, *spawnEnvTestServer) {
	t.Helper()
	srv := serveSpawnEnv(t, password, delta)
	p := newSpawnEnvPuller(hostOf(t, srv), password)
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		baseCmdFactory: mkFactoryEnv(staticEnv("PLATFORM=1", "PATH=/bin")),
		puller:         p,
		pullCtx:        context.Background(),
	}
	return a, srv
}

func TestAdapterPreSpawn_PullSuccessComposesAndClearsDegrade(t *testing.T) {
	a, _ := mkAdapterWithPuller(t, map[string]string{"PULLED": "fresh"}, "pw")
	a.degradedReason = spawnEnvReasonUnavailable

	a.preSpawn()
	cmd := a.composeChild()

	require.Contains(t, cmd.Env, "PULLED=fresh")
	require.Contains(t, cmd.Env, "PLATFORM=1")
	st := a.SpawnEnvState()
	require.False(t, st.Degraded)
	require.Empty(t, st.Reason)
	require.Equal(t, spawnDeltaRev(map[string]string{"PULLED": "fresh"}), st.SpawnedRev)
}

func TestAdapterPreSpawn_FailureKeepsLastGoodAndDegrades(t *testing.T) {
	a, srv := mkAdapterWithPuller(t, map[string]string{"KEEP": "last-good"}, "pw")
	a.preSpawn()
	srv.Close()

	a.preSpawn() // pull now unreachable → last-good + degrade
	cmd := a.composeChild()

	require.Contains(t, cmd.Env, "KEEP=last-good", "last-good delta must survive a failed pull (memory-only cache)")
	st := a.SpawnEnvState()
	require.True(t, st.Degraded)
	require.Equal(t, spawnEnvReasonUnavailable, st.Reason)
	require.Equal(t, spawnDeltaRev(map[string]string{"KEEP": "last-good"}), st.SpawnedRev)
}

func TestAdapterPreSpawn_FirstBootDeadPullerIsPlatformEnvOnly(t *testing.T) {
	p := newSpawnEnvPuller("127.0.0.1:1", "pw")
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		baseCmdFactory: mkFactoryEnv(staticEnv("PLATFORM=1")),
		puller:         p,
		pullCtx:        context.Background(),
	}

	a.preSpawn()
	cmd := a.composeChild()

	require.Equal(t, []string{"PLATFORM=1"}, cmd.Env, "first boot with a dead sidecar: platform env only")
	st := a.SpawnEnvState()
	require.True(t, st.Degraded)
	require.Equal(t, spawnEnvReasonUnavailable, st.Reason)
}

func TestAdapterPreSpawn_EmptyPullSupersedesLastGood(t *testing.T) {
	// I12: revocation is absence — a successful pull of an empty delta
	// must clear a previously non-empty one, not "preserve" it.
	a, srv := mkAdapterWithPuller(t, map[string]string{"REVOKED": "soon"}, "pw")
	a.preSpawn()
	srv.Close()
	empty := serveSpawnEnv(t, "pw", map[string]string{})
	a.puller = newSpawnEnvPuller(hostOf(t, empty), "pw")
	a.puller.bound, a.puller.attempt, a.puller.retryGap = fastTiming()

	a.preSpawn()
	cmd := a.composeChild()

	require.NotContains(t, cmd.Env, "REVOKED=soon")
	require.Equal(t, spawnDeltaRev(map[string]string{}), a.SpawnEnvState().SpawnedRev)
	require.False(t, a.SpawnEnvState().Degraded, "empty delta from a healthy pull is the quiet 'no secrets bound' state")
}

func TestAdapterComposeChild_RecordsEffectiveRevNotServerRev(t *testing.T) {
	// I4: spawned_rev is measured at the env the child actually spawns
	// with. The server-advertised rev is deliberately wrong here; a
	// platform-shadowed key must also drop out of the effective rev.
	srv := serveSkewedSpawnEnv(t, "pw",
		map[string]string{"A": "1", "PLATFORM": "shadow-attempt"},
		"deliberately-stale-server-rev")
	p := newSpawnEnvPuller(hostOf(t, srv), "pw")
	p.bound, p.attempt, p.retryGap = fastTiming()
	a := &managedProcAdapter{
		baseCmdFactory: mkFactoryEnv(staticEnv("PLATFORM=fixed")),
		puller:         p,
		pullCtx:        context.Background(),
	}

	a.preSpawn()
	cmd := a.composeChild()

	require.Contains(t, cmd.Env, "A=1")
	require.Contains(t, cmd.Env, "PLATFORM=fixed", "platform vars win over the delta")
	st := a.SpawnEnvState()
	require.NotEqual(t, "deliberately-stale-server-rev", st.SpawnedRev)
	require.Equal(t, spawnDeltaRev(map[string]string{"A": "1"}), st.SpawnedRev,
		"the shadowed key must not count into the effective revision")
}

func TestAdapterSetSpawnEnv_StillComposesAtNextSpawn(t *testing.T) {
	// Legacy push path (demolition lands with US-70.5): the socket
	// spawn_env store still lands in the next spawn's env.
	a := &managedProcAdapter{baseCmdFactory: mkFactoryEnv(staticEnv("PLATFORM=1"))}
	a.SetSpawnEnv(map[string]string{"PUSHED": "legacy"})
	cmd := a.composeChild()
	require.Contains(t, cmd.Env, "PUSHED=legacy")
	require.Contains(t, cmd.Env, "PLATFORM=1")
}

func TestControlSocketStatus_ReportsSpawnEnvState(t *testing.T) {
	a := &managedProcAdapter{p: &managedProcess{}, baseCmdFactory: mkFactoryEnv(staticEnv("PLATFORM=1"))}
	a.SetSpawnEnv(map[string]string{"K": "v"})
	a.composeChild()
	a.degradedReason = spawnEnvReasonUnavailable

	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", a)
	go srv.serve()
	t.Cleanup(func() { _ = srv.close() })

	cc := newControlClient(srv.addr())
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, spawnDeltaRev(map[string]string{"K": "v"}), st.SpawnedRev)
	require.True(t, st.SpawnEnvDegraded)
	require.Equal(t, spawnEnvReasonUnavailable, st.SpawnEnvReason)
}
