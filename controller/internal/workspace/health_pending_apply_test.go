// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// health_pending_apply_test.go — #1342 item 4: the controller mirrors
// agentd's deferred-credential-apply signal into a Workspace condition
// so operators see why a credential change has not applied yet.

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

// setupPendingApplyHealthTest serves a MUTABLE healthz body (pointer) so
// one test can observe the pending → applied transition across scrapes.
func setupPendingApplyHealthTest(t *testing.T, resp *agentd.HealthzResponse) (*WorkspaceReconciler, *v1.Workspace) {
	t.Helper()

	origAdminPort := agentdAdminPort
	t.Cleanup(func() { agentdAdminPort = origAdminPort })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		})},
	}
	server.Start()
	t.Cleanup(server.Close)

	_, portStr, _ := net.SplitHostPort(server.Listener.Addr().String())
	agentdAdminPort, _ = strconv.Atoi(portStr)

	scheme := testScheme(t)
	ws := makeWorkspace("ws-pending-apply", "default", v1.WorkspacePhaseActive)
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

	return r, ws
}

func credentialsApplyPendingCondition(t *testing.T, ws *v1.Workspace) *v1.WorkspaceCondition {
	t.Helper()
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == v1.WorkspaceConditionCredentialsApplyPending {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func TestCheckAgentHealth_MirrorsPendingCredentialApply(t *testing.T) {
	r, ws := setupPendingApplyHealthTest(t, &agentd.HealthzResponse{
		Healthy:       true,
		UptimeSeconds: 42,
		PendingApply: &agentd.PendingApplyHealth{
			Reason:         "credential_change",
			WaitingSeconds: 300,
			BusySessions:   2,
		},
	})

	r.checkAgentHealth(context.Background(), ws)

	cond := credentialsApplyPendingCondition(t, ws)
	require.NotNil(t, cond, "a deferred credential apply must surface as a condition")
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, v1.ReasonCredentialsApplyDeferred, cond.Reason)
	assert.Contains(t, cond.Message, "2 busy session")
}

func TestCheckAgentHealth_PendingApplyConditionClearedAfterApply(t *testing.T) {
	resp := &agentd.HealthzResponse{
		Healthy:       true,
		UptimeSeconds: 42,
		PendingApply: &agentd.PendingApplyHealth{
			Reason:       "credential_change",
			BusySessions: 1,
		},
	}
	r, ws := setupPendingApplyHealthTest(t, resp)

	r.checkAgentHealth(context.Background(), ws)
	require.NotNil(t, credentialsApplyPendingCondition(t, ws))

	// The restart applied — the next scrape carries no pending apply.
	resp.PendingApply = nil
	ws.Status.LastHealthCheckAt = nil
	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, credentialsApplyPendingCondition(t, ws),
		"once the apply lands the condition must clear")
}

func TestCheckAgentHealth_NoPendingApply_NoCondition(t *testing.T) {
	r, ws := setupPendingApplyHealthTest(t, &agentd.HealthzResponse{Healthy: true})

	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, credentialsApplyPendingCondition(t, ws),
		"nothing pending — no condition")
}
