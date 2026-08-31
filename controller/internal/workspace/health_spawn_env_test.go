// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// health_spawn_env_test.go — US-70.1 (design 0057 I4/I10): the
// controller's mirror of the terminal-verified spawn-env delivery state
// from agentd's /v1/healthz into the Workspace CRD status.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// setupSpawnEnvHealthTest serves the given healthz response body on an
// ephemeral port wired in as the admin port.
func setupSpawnEnvHealthTest(t *testing.T, healthzBody agentd.HealthzResponse) (*WorkspaceReconciler, *v1.Workspace, *httptest.Server) {
	t.Helper()

	origAdminPort := agentdAdminPort
	t.Cleanup(func() { agentdAdminPort = origAdminPort })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(healthzBody)
		})},
	}
	server.Start()
	t.Cleanup(server.Close)

	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	agentdAdminPort, _ = strconv.Atoi(portStr)

	scheme := testScheme(t)
	ws := makeWorkspace("ws-spawn-env", "default", v1.WorkspacePhaseActive)
	past := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	ws.Status.StartTime = &past
	ws.Status.PodIP = "127.0.0.1"

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(ws).
		WithStatusSubresource(&v1.Workspace{}).
		Build()

	r := &WorkspaceReconciler{Client: fc, Scheme: scheme}

	origInterval := healthCheckInterval
	healthCheckInterval = 0
	t.Cleanup(func() { healthCheckInterval = origInterval })

	return r, ws, server
}

func agentHealthyCondition(t *testing.T, ws *v1.Workspace) *v1.WorkspaceCondition {
	t.Helper()
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionAgentHealthy {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func TestCheckAgentHealth_MirrorsDegradedSecretsDelivery(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy:       true,
		UptimeSeconds: 42,
		SpawnEnv: &agentd.SpawnEnvHealth{
			SpawnedRev: "rev-a",
			Degraded:   true,
			Reason:     "spawn_env_unavailable",
		},
		Warnings: []string{"degraded:spawn_env_unavailable"},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "rev-a", ws.Status.SecretsDelivery.SpawnedRev)
	assert.Equal(t, "spawn_env_unavailable", ws.Status.SecretsDelivery.DegradedReason)

	c := agentHealthyCondition(t, ws)
	require.NotNil(t, c)
	assert.Equal(t, "True", c.Status, "a secrets degrade is observability, never a liveness failure")
	assert.Contains(t, c.Message, "degraded:spawn_env_unavailable",
		"the machine-readable reason must reach the condition message (alert path)")
}

func TestCheckAgentHealth_MirrorsConvergedSecretsDelivery(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy:  true,
		SpawnEnv: &agentd.SpawnEnvHealth{SpawnedRev: "rev-b"},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "rev-b", ws.Status.SecretsDelivery.SpawnedRev)
	assert.Empty(t, ws.Status.SecretsDelivery.DegradedReason, "converged delivery carries no reason")
}

func TestCheckAgentHealth_FieldAbsentKeepsPreviousDelivery(t *testing.T) {
	// Mixed fleet (W15): a healthy pre-US-70.1 runtime omits spawnEnv —
	// the previously-reported value must survive (no flapping to nil).
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{Healthy: true})
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: "rev-old"}

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "rev-old", ws.Status.SecretsDelivery.SpawnedRev)
}

func TestCheckAgentHealth_UnreachableClearsSecretsDelivery(t *testing.T) {
	r, ws, server := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{Healthy: true})
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: "rev-stale"}
	server.Close()

	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.SecretsDelivery,
		"a stale value from an unreachable pod must not survive — no evidence beats stale evidence")
}

// TestCheckAgentHealth_MirrorsFilesRev (R2b, #1165): the file-class
// terminal revision mirrors into the CRD alongside spawnedRev, and a
// files-only degrade (env healthy) still surfaces its reason.
func TestCheckAgentHealth_MirrorsFilesRev(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		SpawnEnv: &agentd.SpawnEnvHealth{
			SpawnedRev:    "rev-env",
			FilesRev:      "rev-files",
			FilesDegraded: true,
			FilesReason:   "spawn_files_unavailable",
		},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "rev-env", ws.Status.SecretsDelivery.SpawnedRev)
	assert.Equal(t, "rev-files", ws.Status.SecretsDelivery.FilesRev)
	assert.Equal(t, "spawn_files_unavailable", ws.Status.SecretsDelivery.DegradedReason,
		"a files-only degrade still carries its machine-readable reason")
}

// TestCheckAgentHealth_FilesRevConverged (R2b): healthy file delivery
// mirrors the rev with no degrade.
func TestCheckAgentHealth_FilesRevConverged(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy:  true,
		SpawnEnv: &agentd.SpawnEnvHealth{SpawnedRev: "rev-a", FilesRev: "rev-f"},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "rev-f", ws.Status.SecretsDelivery.FilesRev)
	assert.Empty(t, ws.Status.SecretsDelivery.DegradedReason)
}
