// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_restarts_handlers_test.go — #1760: the REAL handler wiring
// carries the supervisor slice on /v1/healthz and /v1/statusz, from
// cached state only (US-22.1), and omits it when unwired (mixed fleet).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func TestHealthzHandler_SupervisorSlice(t *testing.T) {
	log := newAgentRestartLog()
	at := time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC)
	log.record("health_watchdog", "hung", "context deadline exceeded", at)
	log.recordSuppression("starved")

	cache := newHealthzCache()
	cache.storeResolved(&healthzCacheSnapshot{}, &healthzCacheSnapshot{
		Initialized:         true,
		Healthy:             false,
		ConsecutiveFailures: 78,
		LastError:           "context deadline exceeded",
	})

	handler := healthzHandler(time.Now(), "", nil, nil, nil, nil, supervisorSnapshotFunc(log, cache))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Supervisor, "wired log must surface the supervisor slice")
	assert.Equal(t, 1, resp.Supervisor.AgentRestarts)
	require.NotNil(t, resp.Supervisor.LastAgentRestart)
	assert.Equal(t, "hung", resp.Supervisor.LastAgentRestart.Verdict)
	assert.Equal(t, 78, resp.Supervisor.ConsecutiveFailures, "the failure run rides the same snapshot")
	assert.Equal(t, "context deadline exceeded", resp.Supervisor.LastHealthError)
	assert.Equal(t, 1, resp.Supervisor.SuppressedRestarts)
	assert.Equal(t, "starved", resp.Supervisor.LastSuppressedVerdict)
	assert.True(t, resp.Healthy, "a restart record must NEVER flip healthz liveness (US-22.1)")
}

func TestHealthzHandler_SupervisorSliceOmittedWhenUnwired(t *testing.T) {
	handler := healthzHandler(time.Now(), "", nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"supervisor"`,
		"unwired topology must omit the field entirely (mixed-fleet contract)")
}

func TestStatuszHandler_SupervisorSlice(t *testing.T) {
	log := newAgentRestartLog()
	log.record("health_watchdog_unknown_episode", "unknown", "", time.Now())
	cache := newHealthzCache()
	// statusz calls the agent health endpoint inline; point it at a
	// dead local port so IsHealthy fails fast without a server.
	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: time.Second}}
	tracker := newSessionStatusTracker()

	handler := buildStatuszHandler(client, &providerCache{}, tracker, nil, newMemoryPressureMonitor(), time.Now(), "", sysMetricsSource{}, nil, nil, nil, supervisorSnapshotFunc(log, cache))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/statusz", nil))

	// statusz's healthy path calls the agent (dead here → unhealthy but
	// still encodes); the supervisor slice must ride regardless.
	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Supervisor)
	assert.Equal(t, 1, resp.Supervisor.AgentRestarts)
	require.NotNil(t, resp.Supervisor.LastAgentRestart)
	assert.Equal(t, "health_watchdog_unknown_episode", resp.Supervisor.LastAgentRestart.Reason)
}

func TestStatuszHandler_SupervisorSliceOmittedWhenUnwired(t *testing.T) {
	client := &OpenCodeClient{password: "test", client: &http.Client{Timeout: time.Second}}
	handler := buildStatuszHandler(client, &providerCache{}, newSessionStatusTracker(), nil, newMemoryPressureMonitor(), time.Now(), "", sysMetricsSource{}, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/statusz", nil))

	assert.NotContains(t, rec.Body.String(), `"supervisor"`)
}
