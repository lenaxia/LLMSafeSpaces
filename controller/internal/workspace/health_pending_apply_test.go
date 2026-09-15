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

// TestCheckAgentHealth_DeadPodScrapes_ClearPendingCondition pins the
// three dead-pod branches (unreachable, undecodable, unhealthy): no
// evidence from a dead pod beats stale evidence — a previously-set
// CredentialsApplyPending condition must be REMOVED, not left as a
// stale "pending" claim on a pod that cannot report.
func TestCheckAgentHealth_DeadPodScrapes_ClearPendingCondition(t *testing.T) {
	pendingResp := &agentd.HealthzResponse{
		Healthy:       true,
		PendingApply:  &agentd.PendingApplyHealth{Reason: "credential_change", BusySessions: 1},
		UptimeSeconds: 42,
	}
	r, ws := setupPendingApplyHealthTest(t, pendingResp)
	origInterval := healthCheckInterval
	healthCheckInterval = 0
	t.Cleanup(func() { healthCheckInterval = origInterval })

	// Baseline: the condition exists.
	r.checkAgentHealth(context.Background(), ws)
	require.NotNil(t, credentialsApplyPendingCondition(t, ws))

	// Sub-case: transport unreachable — a port with nothing listening.
	deadPort := freeTCPPortHealthy(t)
	agentdAdminPort = deadPort
	ws.Status.LastHealthCheckAt = nil
	r.checkAgentHealth(context.Background(), ws)
	assert.Nil(t, credentialsApplyPendingCondition(t, ws),
		"unreachable pod — condition cleared (no evidence)")

	// Sub-case: undecodable body (200 with garbage). Re-seed the
	// condition first so each branch is proven on its own.
	require.NotNil(t, seedConditionAgain(t, r, ws, pendingResp))
	badSrv := serveRaw(t, "not-json{")
	t.Cleanup(badSrv.Close)
	retargetHealthCheck(t, ws, badSrv)
	r.checkAgentHealth(context.Background(), ws)
	assert.Nil(t, credentialsApplyPendingCondition(t, ws),
		"undecodable response — condition cleared")

	// Sub-case: agent reports unhealthy.
	require.NotNil(t, seedConditionAgain(t, r, ws, pendingResp))
	sickSrv := serveRaw(t, `{"healthy":false}`)
	t.Cleanup(sickSrv.Close)
	retargetHealthCheck(t, ws, sickSrv)
	r.checkAgentHealth(context.Background(), ws)
	assert.Nil(t, credentialsApplyPendingCondition(t, ws),
		"unhealthy agent — condition cleared")
}

// freeTCPPortHealthy reserves then releases a local port so connections
// to it are refused (the unreachable-pod shape) without racing a rebind.
func freeTCPPortHealthy(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func serveRaw(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
}

// retargetHealthCheck points the workspace's next health check at srv.
func retargetHealthCheck(t *testing.T, ws *v1.Workspace, srv *httptest.Server) {
	t.Helper()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)
	agentdAdminPort = port
	ws.Status.PodIP = "127.0.0.1"
	ws.Status.LastHealthCheckAt = nil
}

// seedConditionAgain re-establishes the pending condition via one good
// scrape (and resets the failure counter) so the next dead-pod branch
// is proven from a standing start.
func seedConditionAgain(t *testing.T, r *WorkspaceReconciler, ws *v1.Workspace, resp *agentd.HealthzResponse) *v1.WorkspaceCondition {
	t.Helper()
	good := serveJSON(t, resp)
	t.Cleanup(good.Close)
	retargetHealthCheck(t, ws, good)
	r.checkAgentHealth(context.Background(), ws)
	return credentialsApplyPendingCondition(t, ws)
}

func serveJSON(t *testing.T, v any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}))
}
