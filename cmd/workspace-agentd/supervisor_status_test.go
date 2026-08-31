// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_status_test.go — US-70.1 (design 0057 I10/I13): the
// supervisor-status store/poller projection into healthz, and the
// healthz spawnEnv field + machine-readable warning.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/stretchr/testify/require"
)

func TestSupervisorStatusStore_NoEvidenceIsNotADegrade(t *testing.T) {
	store := &supervisorStatusStore{}
	require.Nil(t, store.spawnEnvHealth(), "before the first successful poll, healthz omits the field")
}

func TestSupervisorStatusStore_ProjectsDegradedAndHealthy(t *testing.T) {
	store := &supervisorStatusStore{}
	store.set(&controlStatus{
		SpawnedRev:       "rev-1",
		SpawnEnvDegraded: true,
		SpawnEnvReason:   spawnEnvReasonUnavailable,
	})
	h := store.spawnEnvHealth()
	require.NotNil(t, h)
	require.True(t, h.Degraded)
	require.Equal(t, spawnEnvReasonUnavailable, h.Reason)
	require.Equal(t, "rev-1", h.SpawnedRev)

	store.set(&controlStatus{SpawnedRev: "rev-2"})
	h = store.spawnEnvHealth()
	require.NotNil(t, h)
	require.False(t, h.Degraded, "a healthy report clears the degrade")
	require.Empty(t, h.Reason)
	require.Equal(t, "rev-2", h.SpawnedRev)
}

func TestStartSupervisorStatusPoller_MirrorsSupervisorState(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	degraded := spawnEnvStateReport{SpawnedRev: "rev-boot", Degraded: true, Reason: spawnEnvReasonUnavailable}
	proc.overrideSpawnEnv.Store(&degraded)
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	store := &supervisorStatusStore{}
	wg := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	startSupervisorStatusPoller(ctx, wg, newControlClient(srv.addr()), store)

	require.Eventually(t, func() bool {
		h := store.spawnEnvHealth()
		return h != nil && h.Degraded && h.Reason == spawnEnvReasonUnavailable
	}, 5*time.Second, 50*time.Millisecond,
		"the immediate first poll must surface a boot-time degrade without waiting an interval")

	healthy := spawnEnvStateReport{SpawnedRev: "rev-ok"}
	proc.overrideSpawnEnv.Store(&healthy)
	require.Eventually(t, func() bool {
		h := store.spawnEnvHealth()
		return h != nil && !h.Degraded && h.SpawnedRev == "rev-ok"
	}, 20*time.Second, 100*time.Millisecond,
		"the periodic poll must mirror recovery (15s interval)")

	cancel()
	wg.Wait()
}

func TestStartSupervisorStatusPoller_FailedPollKeepsLastSnapshot(t *testing.T) {
	withTestLogger(t)
	proc := &fakeRestartProc{}
	healthy := spawnEnvStateReport{SpawnedRev: "rev-keep"}
	proc.overrideSpawnEnv.Store(&healthy)
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	store := &supervisorStatusStore{}
	wg := &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	startSupervisorStatusPoller(ctx, wg, newControlClient(srv.addr()), store)
	require.Eventually(t, func() bool {
		return store.snapshot() != nil
	}, 5*time.Second, 50*time.Millisecond)

	// Kill the socket: subsequent polls fail; the store must keep the
	// last-known snapshot rather than flapping to no-evidence.
	_ = srv.close()
	time.Sleep(50 * time.Millisecond)
	h := store.spawnEnvHealth()
	require.NotNil(t, h, "a failed poll keeps the last snapshot — supervisor restarts must not flap healthz")
	require.Equal(t, "rev-keep", h.SpawnedRev)

	cancel()
	wg.Wait()
}

func TestSpawnEnvWarning_MachineReadable(t *testing.T) {
	require.Equal(t, "", spawnEnvWarning(nil))
	require.Equal(t, "", spawnEnvWarning(&agentd.SpawnEnvHealth{SpawnedRev: "r"}))
	require.Equal(t, "degraded:spawn_env_unavailable",
		spawnEnvWarning(&agentd.SpawnEnvHealth{Degraded: true, Reason: spawnEnvReasonUnavailable}))
}

func TestHealthzHandler_SpawnEnvFieldAndWarning(t *testing.T) {
	handlerFor := func(fn func() *agentd.SpawnEnvHealth) agentd.HealthzResponse {
		h := healthzHandler(time.Now(), "", "", fn)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))
		var resp agentd.HealthzResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	t.Run("no snapshot fn — field absent (single-container mode)", func(t *testing.T) {
		resp := handlerFor(nil)
		require.Nil(t, resp.SpawnEnv)
		require.True(t, resp.Healthy)
	})

	t.Run("degraded — field set, warning appended, liveness untouched", func(t *testing.T) {
		resp := handlerFor(func() *agentd.SpawnEnvHealth {
			return &agentd.SpawnEnvHealth{SpawnedRev: "r1", Degraded: true, Reason: spawnEnvReasonUnavailable}
		})
		require.NotNil(t, resp.SpawnEnv)
		require.True(t, resp.SpawnEnv.Degraded)
		require.Equal(t, spawnEnvReasonUnavailable, resp.SpawnEnv.Reason)
		require.Contains(t, resp.Warnings, "degraded:spawn_env_unavailable")
		require.True(t, resp.Healthy, "a secrets degrade must not cascade to liveness/pod-kill")
	})

	t.Run("no evidence yet — field absent, not a degrade", func(t *testing.T) {
		resp := handlerFor(func() *agentd.SpawnEnvHealth { return nil })
		require.Nil(t, resp.SpawnEnv)
		require.Empty(t, resp.Warnings)
	})

	t.Run("healthy — field set, no warning", func(t *testing.T) {
		resp := handlerFor(func() *agentd.SpawnEnvHealth {
			return &agentd.SpawnEnvHealth{SpawnedRev: "r2"}
		})
		require.NotNil(t, resp.SpawnEnv)
		require.False(t, resp.SpawnEnv.Degraded)
		require.Empty(t, resp.Warnings)
	})
}

// spawnEnvHealth projects the files-family fields (R2b, #1165): filesRev
// mirrors through, and the warning renders the files reason when it is
// the active degrade.
func TestSpawnEnvHealth_ProjectsFilesRevAndFilesReason(t *testing.T) {
	var store supervisorStatusStore
	store.set(&controlStatus{
		SpawnedRev:       "rev-env",
		FilesRev:         "rev-files",
		SpawnFilesReason: spawnFilesReasonUnavailable,
	})
	h := store.spawnEnvHealth()
	require.NotNil(t, h)
	require.Equal(t, "rev-files", h.FilesRev)
	require.True(t, h.FilesDegraded)
	require.Equal(t, spawnFilesReasonUnavailable, h.FilesReason)
	require.Equal(t, "degraded:"+spawnFilesReasonUnavailable, spawnEnvWarning(h),
		"a files degrade must render the same machine-readable warning line")
}

// A healthy files state with no env degrade renders no warning.
func TestSpawnEnvWarning_HealthyFilesQuiet(t *testing.T) {
	h := &agentd.SpawnEnvHealth{SpawnedRev: "a", FilesRev: "b"}
	require.Empty(t, spawnEnvWarning(h))
}
