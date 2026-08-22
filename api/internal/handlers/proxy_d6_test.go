// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// D6 (#998) escalation tests: a watched Active workspace whose statusz
// reports oldest_busy_seconds past the threshold gets exactly ONE
// workspace.alert/session_hung event per cooldown window; below
// threshold and cooldown-active workspaces stay silent. Notify-only:
// nothing restarts, nothing aborts.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
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
