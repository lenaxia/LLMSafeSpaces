// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// D6 (#998) escalation tests: a watched Active workspace whose statusz
// reports oldest_busy_seconds past the threshold gets exactly ONE
// workspace.alert/session_hung event per cooldown window; below
// threshold and cooldown-active workspaces stay silent. Notify-only:
// nothing restarts, nothing aborts.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakePhaseSourceD6 map[string]string

func (f fakePhaseSourceD6) GetAllKnownPhases() map[string]string { return f }

func newD6Env(t *testing.T, statusz func(w http.ResponseWriter, r *http.Request)) (*testEnv, *eventbroker.UserEventBroker) {
	t.Helper()
	// The test backend IS the statusz fake: every opencode-bound URL the
	// handler builds for ws-1 (pod IP from the default workspace fixture)
	// lands here. The default newTestEnv backend asserts Basic Auth on
	// all requests and fails the suite; the D6 fetch is bearer-only.
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		// Non-statusz opencode paths (none expected in these tests): 200
		// empties so incidental calls don't fail the backend's JSON.
		if !strings.HasSuffix(r.URL.Path, "/v1/statusz") {
			w.WriteHeader(http.StatusOK)
			return
		}
		statusz(w, r)
	})
	env.setupWorkspacePodWithT(t, "ws-1", backendHost(t, env), "Active", "ws-1")
	broker := eventbroker.NewUserEventBroker()
	env.handler.userBroker = broker
	env.handler.phaseSource = fakePhaseSourceD6{"ws-1": "Active"}
	return env, broker
}

// backendHost returns the test backend's host:port (the pod-IP shape the
// handler builds URLs from).
func backendHost(t *testing.T, env *testEnv) string {
	t.Helper()
	u, err := url.Parse(env.backend.URL)
	require.NoError(t, err)
	return net.JoinHostPort(u.Hostname(), u.Port())
}

func hungStatusz(seconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{
			Healthy:           true,
			OldestBusySeconds: seconds,
			BusyAges:          map[string]int{"ses-x": seconds},
		})
	}
}

func TestEscalateHungs_FiresOncePerCooldown(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour // deterministic: cooldown spans the test
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	env, broker := newD6Env(t, hungStatusz(int((busyAlertOlderThan + 5*time.Minute).Seconds())))

	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateHungs([]string{"ws-1"})

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "workspace.alert", evt.Type)
		assert.Equal(t, "session_hung", evt.Status)
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "notify_only", data["policy"])
		assert.Equal(t, "ses-x", evt.SessionID, "alert names the hung session")
	default:
		t.Fatal("hung workspace must emit workspace.alert/session_hung")
	}

	// Second sweep inside the cooldown: silent.
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("cooldown must suppress: got %+v", evt)
	default:
	}
}

func TestEscalateHungs_BelowThresholdSilent(t *testing.T) {
	env, broker := newD6Env(t, hungStatusz(10)) // 10s busy: healthy
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("short busy must not alert: got %+v", evt)
	default:
	}
}

func TestEscalateHungs_StatuszDownSilent(t *testing.T) {
	// A 500-ing statusz must not alert (a slow pod is covered by tracker
	// alerts; this path adds no load and no noise).
	env, broker := newD6Env(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("statusz failure must not alert: got %+v", evt)
	default:
	}
}

func TestEscalateHungs_CooldownExpiry(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = 50 * time.Millisecond
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	env, broker := newD6Env(t, hungStatusz(int((busyAlertOlderThan + time.Hour).Seconds())))
	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case <-sub.Ch:
	default:
		t.Fatal("first sweep must alert")
	}
	time.Sleep(80 * time.Millisecond)
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case <-sub.Ch:
	default:
		t.Fatal("alert must re-fire after cooldown expiry (session still hung)")
	}
}

// Notify-only pin: the escalation path never touches the restart path.
// A hung workspace sweeps with NO crash/watchdog restart side effects —
// assert the workspace CR is untouched via the mock (no Patch calls).
func TestEscalateHungs_NotifyOnlyNoRestart(t *testing.T) {
	var mu sync.Mutex
	patches := 0
	env, broker := newD6Env(t, hungStatusz(99999))
	_ = broker
	env.wsMock.On("Patch", mock.Anything, "ws-1", mock.Anything, mock.Anything, mock.Anything).
		Run(func(mock.Arguments) { mu.Lock(); patches++; mu.Unlock() }).
		Return(nil, nil).Maybe()

	env.handler.escalateHungs([]string{"ws-1"})

	mu.Lock()
	defer mu.Unlock()
	assert.Zero(t, patches, "D6 policy is notify-only: no workspace mutation")
}

// mockSessionAlerts is a testify mock of interfaces.SessionAlertsService.
type mockSessionAlerts struct {
	mock.Mock
}

func (m *mockSessionAlerts) RecordAlert(workspaceID, sessionID, alert string, oldestBusySeconds int) {
	m.Called(workspaceID, sessionID, alert, oldestBusySeconds)
}

func (m *mockSessionAlerts) ListByWorkspace(ctx context.Context, workspaceID string, limit int) ([]types.SessionAlert, error) {
	args := m.Called(ctx, workspaceID, limit)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.SessionAlert), args.Error(1)
}

func (m *mockSessionAlerts) Start() error { return nil }
func (m *mockSessionAlerts) Stop() error  { return nil }

// #998 finding 4: every published escalation must also be persisted for
// workflow surfaces (session history) — same args as the SSE payload.
func TestEscalateHungs_PersistsAlert(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	seconds := int((busyAlertOlderThan + 5*time.Minute).Seconds())
	env, broker := newD6Env(t, hungStatusz(seconds))
	_ = broker

	alerts := &mockSessionAlerts{}
	alerts.On("RecordAlert", "ws-1", "ses-x", "session_hung", seconds).Once()
	env.handler.SetSessionAlerts(alerts)

	env.handler.escalateHungs([]string{"ws-1"})

	alerts.AssertExpectations(t)
}

// Cooldown isolation: a cooling workspace must not mask a second hung
// workspace in the same sweep — the cooldown map is per-workspace.
func TestEscalateHungs_ConcurrentCooldownIsolation(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	seconds := int((busyAlertOlderThan + time.Hour).Seconds())
	env, broker := newD6Env(t, hungStatusz(seconds))
	env.setupWorkspacePodWithT(t, "ws-2", backendHost(t, env), "Active", "ws-2")
	env.handler.phaseSource = fakePhaseSourceD6{"ws-1": "Active", "ws-2": "Active"}

	sub1, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub1)
	sub2, err := broker.SubscribeWorkspace("ws-2")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-2", sub2)

	// ws-1 already alerted (inside cooldown); ws-2 never has.
	env.handler.markBusyAlerted("ws-1")

	env.handler.escalateHungs([]string{"ws-1", "ws-2"})

	select {
	case evt := <-sub1.Ch:
		t.Fatalf("ws-1 cooling must stay silent: got %+v", evt)
	default:
	}
	select {
	case evt := <-sub2.Ch:
		assert.Equal(t, "workspace.alert", evt.Type)
		assert.Equal(t, "session_hung", evt.Status)
	default:
		t.Fatal("ws-2 must alert despite ws-1 cooling")
	}
}

// Full integration through the REAL reconciler tick: phaseSource →
// EnsureWatching → escalateHungs → statusz fetch with live bearer auth.
// The fake statusz asserts the Authorization header so the test pins
// the production auth path (admin bearer candidates), not a bypass.
func TestEscalateHungs_ReconcilerTickIntegration(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour
	t.Cleanup(func() { busyAlertCooldown = origCooldown })
	origInterval := sseWatchReconcileInterval
	sseWatchReconcileInterval = 20 * time.Millisecond
	t.Cleanup(func() { sseWatchReconcileInterval = origInterval })

	var sawBearer atomic.Bool
	env, broker := newD6Env(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("statusz fetch must use bearer auth, got %q", auth)
			return
		}
		sawBearer.Store(true)
		hungStatusz(int((busyAlertOlderThan + time.Minute).Seconds()))(w, r)
	})
	// The statusz fetch authenticates with the admin-token bearer from
	// the workspace-pw secret — wire a real one so the production
	// candidate path (k8s secret → Bearer) is exercised end-to-end.
	secret := makePasswordSecret("ws-1", "test-password")
	secret.Data["admin-token"] = []byte("d6-admin-token")
	_, err := env.clientset.CoreV1().Secrets("default").Create(context.Background(), secret, metav1.CreateOptions{})
	require.NoError(t, err)

	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	tracker := sse.NewTracker(env.handler.httpClient, env.log, nil)
	env.handler.sseTracker = tracker

	if env.handler.stopCh == nil {
		env.handler.stopCh = make(chan struct{})
	}
	done := make(chan struct{}, 1)
	go func() { env.handler.sseWatchReconciler(sseWatchReconcileInterval); close(done) }()
	t.Cleanup(func() {
		env.handler.stopOnce.Do(func() { close(env.handler.stopCh) })
		tracker.Stop()
		<-done
	})

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "workspace.alert", evt.Type)
		assert.True(t, sawBearer.Load(), "statusz fetch must carry bearer auth")
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler tick must surface the hung session")
	}
}

// GET /workspaces/:id/alerts — the reconnect/workflow surface (#998
// finding 4). Valid list, invalid limit, and unconfigured service.
func TestGetWorkspaceAlerts(t *testing.T) {
	env, _ := newD6Env(t, hungStatusz(0))

	alerts := &mockSessionAlerts{}
	now := time.Now().UTC()
	alerts.On("ListByWorkspace", mock.Anything, "ws-1", 50).
		Return([]types.SessionAlert{
			{ID: "1", WorkspaceID: "ws-1", SessionID: "ses-x", Alert: "session_hung", OldestBusySeconds: 960, CreatedAt: now},
		}, nil).Once()
	env.handler.SetSessionAlerts(alerts)

	w := env.doRequestWithT(t, http.MethodGet, "/api/v1/workspaces/ws-1/alerts", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Alerts []types.SessionAlert `json:"alerts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Alerts, 1)
	assert.Equal(t, "session_hung", body.Alerts[0].Alert)
	assert.Equal(t, "ses-x", body.Alerts[0].SessionID)

	w = env.doRequestWithT(t, http.MethodGet, "/api/v1/workspaces/ws-1/alerts?limit=0", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = env.doRequestWithT(t, http.MethodGet, "/api/v1/workspaces/ws-1/alerts?limit=abc", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	alerts.AssertExpectations(t)
}

func TestGetWorkspaceAlerts_NotConfigured(t *testing.T) {
	env, _ := newD6Env(t, hungStatusz(0))
	w := env.doRequestWithT(t, http.MethodGet, "/api/v1/workspaces/ws-1/alerts", nil)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
