// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// health_agent_supervisor_test.go — #1760: the controller's mirror of
// agentd's supervisor restart slice (/v1/healthz supervisor) into the
// Workspace CRD — status.agentSupervisor + the AgentRestarted condition
// + the exactly-once event per restart.

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func supervisorHealthz(restarts int, reason, verdict string, at time.Time, suppressed int, lastSuppressedVerdict string) *agentd.HealthzResponse {
	return &agentd.HealthzResponse{
		Healthy: true,
		Supervisor: &agentd.SupervisorStatus{
			AgentRestarts: restarts,
			LastAgentRestart: &agentd.AgentRestartEvent{
				Reason:    reason,
				Verdict:   verdict,
				LastError: "Get /global/health: context deadline exceeded",
				At:        at,
			},
			SuppressedRestarts:    suppressed,
			LastSuppressedVerdict: lastSuppressedVerdict,
		},
	}
}

// TestMirrorAgentSupervisor_RestartSurfaces: a watchdog restart in the
// healthz scrape lands in the CRD status AND sets the AgentRestarted
// condition (True / WatchdogRestarted) with the verdict in the message.
func TestMirrorAgentSupervisor_RestartSurfaces(t *testing.T) {
	r, ws, _ := setupSupervisorHealthTest(t, supervisorHealthz(1, "health_watchdog", "hung", time.Now(), 78, "starved"))

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.AgentSupervisor)
	assert.Equal(t, int32(1), ws.Status.AgentSupervisor.AgentRestarts)
	require.NotNil(t, ws.Status.AgentSupervisor.LastAgentRestart)
	assert.Equal(t, "health_watchdog", ws.Status.AgentSupervisor.LastAgentRestart.Reason)
	assert.Equal(t, "hung", ws.Status.AgentSupervisor.LastAgentRestart.Verdict)
	assert.Equal(t, int32(78), ws.Status.AgentSupervisor.SuppressedRestarts)
	assert.Equal(t, "starved", ws.Status.AgentSupervisor.LastSuppressedVerdict)

	var cond *v1.WorkspaceCondition
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionAgentRestarted {
			cond = &ws.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond, "AgentRestarted condition must be set")
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, v1.ReasonWatchdogRestarted, cond.Reason)
	assert.Contains(t, cond.Message, "restarted by the supervisor health watchdog")
	assert.Contains(t, cond.Message, "hung")
	assert.NotContains(t, cond.Message, ";", "condition messages must stay semicolon-free (warning-suffix parser contract)")
}

// TestMirrorAgentSupervisor_MixedFleetKeepsPrior: a healthy scrape from
// a pre-#1760 runtime (no supervisor field) must NOT clear a previously
// mirrored value — same policy as SecretsDelivery (W15 mixed fleet).
func TestMirrorAgentSupervisor_MixedFleetKeepsPrior(t *testing.T) {
	at := metav1.NewTime(time.Now().Add(-time.Minute))
	r, ws, _ := setupSupervisorHealthTest(t, &agentd.HealthzResponse{Healthy: true}) // no Supervisor field
	ws.Status.AgentSupervisor = &v1.AgentSupervisorStatus{
		AgentRestarts:      3,
		SuppressedRestarts: 5,
		LastAgentRestart:   &v1.AgentRestartEvent{Reason: "health_watchdog", Verdict: "hung", At: &at},
	}

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.AgentSupervisor, "absent field on healthy scrape must keep prior evidence")
	assert.Equal(t, int32(3), ws.Status.AgentSupervisor.AgentRestarts)
}

// TestMirrorAgentSupervisor_UnreachableClears: a dead pod's stale
// supervisor state must not survive (evidence rule: no evidence from a
// dead pod beats stale evidence).
func TestMirrorAgentSupervisor_UnreachableClears(t *testing.T) {
	r, ws, srv := setupSupervisorHealthTest(t, supervisorHealthz(1, "health_watchdog", "hung", time.Now(), 0, ""))
	r.checkAgentHealth(context.Background(), ws)
	require.NotNil(t, ws.Status.AgentSupervisor)
	srv.Close() // pod goes unreachable

	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.AgentSupervisor, "unreachable pod must clear the supervisor state")
}

// TestMirrorAgentSupervisor_EventOncePerRestart: the K8s Warning event
// fires exactly once per distinct restart (annotation-stamped), not on
// every 15s re-scrape of the same restart.
func TestMirrorAgentSupervisor_EventOncePerRestart(t *testing.T) {
	at1 := time.Now().Add(-2 * time.Minute)
	r, ws, _ := setupSupervisorHealthTest(t, supervisorHealthz(1, "health_watchdog", "hung", at1, 0, ""))
	recorder := record.NewFakeRecorder(16)
	r.Recorder = recorder

	r.checkAgentHealth(context.Background(), ws)
	r.checkAgentHealth(context.Background(), ws) // re-scrape, same restart

	close(recorder.Events)
	count := 0
	for range recorder.Events {
		count++
	}
	assert.Equal(t, 1, count, "same restart re-scraped must not re-fire the event")

	// A NEW restart (count climbed, newer At) re-fires once.
	at2 := time.Now()
	r2, ws2, _ := setupSupervisorHealthTest(t, supervisorHealthz(2, "health_watchdog_unknown_episode", "unknown", at2, 3, "unknown"))
	recorder2 := record.NewFakeRecorder(16)
	r2.Recorder = recorder2
	r2.checkAgentHealth(context.Background(), ws2)
	r2.checkAgentHealth(context.Background(), ws2)
	close(recorder2.Events)
	count2 := 0
	for range recorder2.Events {
		count2++
	}
	assert.Equal(t, 1, count2)
}

// TestMirrorAgentSupervisor_NoRestartNoCondition: suppressions-only
// evidence (no fired restart) still mirrors the counts but never sets
// the AgentRestarted condition.
func TestMirrorAgentSupervisor_NoRestartNoCondition(t *testing.T) {
	r, ws, _ := setupSupervisorHealthTest(t, &agentd.HealthzResponse{
		Healthy: true,
		Supervisor: &agentd.SupervisorStatus{
			AgentRestarts:         0,
			SuppressedRestarts:    74,
			LastSuppressedVerdict: "starved",
		},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.AgentSupervisor)
	assert.Equal(t, int32(74), ws.Status.AgentSupervisor.SuppressedRestarts)
	for _, c := range ws.Status.Conditions {
		assert.NotEqual(t, v1.WorkspaceConditionAgentRestarted, c.Type,
			"no fired restart must not set the condition")
	}
}

// setupSupervisorHealthTest reuses the spawn-env harness: an ephemeral
// healthz server wired in as the admin port.
func setupSupervisorHealthTest(t *testing.T, healthzBody *agentd.HealthzResponse) (*WorkspaceReconciler, *v1.Workspace, *httptest.Server) {
	t.Helper()
	r, ws, srv := setupSpawnEnvHealthTest(t, *healthzBody)
	return r, ws, srv
}
