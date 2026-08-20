// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// US-1 adapter tests: managedProcAdapter maps Appendix-A vocabulary onto
// managedProcess 1:1 — state readout, memory-only spawn env applied at
// next factory call, restart passthrough.

func TestManagedProcAdapter_State(t *testing.T) {
	adapter := &managedProcAdapter{p: &managedProcess{}}

	pid, state, restarts, last := adapter.State()
	assert.Equal(t, 0, pid, "no child yet")
	assert.Equal(t, "stopped", state)
	assert.Equal(t, 0, restarts)
	assert.True(t, last.IsZero())
}

func TestManagedProcAdapter_SetSpawnEnvMemoryOnlyNextFactory(t *testing.T) {
	adapter := &managedProcAdapter{p: &managedProcess{}}

	adapter.SetSpawnEnv(map[string]string{"GH_TOKEN": "ghp_x", "Y": "2"})

	// The NEXT factory call returns the handed-over env verbatim.
	factory := adapter.p.cmdFactory
	require.NotNil(t, factory, "SetSpawnEnv installs the factory wrapper")
	cmd := factory()
	assert.ElementsMatch(t, []string{"GH_TOKEN=ghp_x", "Y=2"}, cmd.Env)

	// Memory-only (A.2/A.4): the adapter exposes no getter — enforce
	// structurally: spawnEnv is only consumed via the factory closure.
	// (The negative capability socket test covers the wire side.)
}

func TestManagedProcAdapter_LastWriteWins(t *testing.T) {
	adapter := &managedProcAdapter{p: &managedProcess{}}

	adapter.SetSpawnEnv(map[string]string{"A": "1"})
	adapter.SetSpawnEnv(map[string]string{"B": "2"})

	cmd := adapter.p.cmdFactory()
	assert.Equal(t, []string{"B=2"}, cmd.Env, "reload replaces the whole env (A.3 last-write-wins)")
}

// The full end-to-end supervisor mode is exercised in the boot-gate
// integration tests' environment (writable /sandbox-cfg); here the
// adapter-level contract suffices — the socket tests already pin the
// wire, and managedProcess's own suite pins the supervision loop.
func TestManagedProcAdapter_ReasonPassthrough(t *testing.T) {
	adapter := &managedProcAdapter{p: &managedProcess{}}
	// restart() on a not-started supervisor is a no-op (returns without
	// blocking) — the adapter must not panic or block either.
	done := make(chan struct{})
	go func() {
		adapter.Restart("manual", 30)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Restart on an unstarted supervisor must not block")
	}
}
