// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// proxy_agent_restart_event_test.go — #1760: the watcher's restart
// callback publishes the agent_restarted platform event to the owning
// user (the agent_died convention: bare snake_case type, payload in
// data).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func restartWorkspaceCRD(name string, count int32, at time.Time) *v1.Workspace {
	t := metav1.NewTime(at)
	ws := makeWorkspaceCRDWithStatus(name, "10.0.0.1", string(v1.WorkspacePhaseActive), name)
	ws.Status.AgentSupervisor = &v1.AgentSupervisorStatus{
		AgentRestarts: count,
		LastAgentRestart: &v1.AgentRestartEvent{
			Reason:  "health_watchdog",
			Verdict: "hung",
			At:      &t,
		},
		SuppressedRestarts:    78,
		LastSuppressedVerdict: "starved",
	}
	return ws
}

func TestOnAgentRestart_PublishesUserEvent(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, err := env.handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-1", sub)

	at := time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC)
	env.handler.onAgentRestart(restartWorkspaceCRD("ws-1", 1, at))

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent_restarted", evt.Type)
		assert.Equal(t, "ws-1", evt.WorkspaceID)
		data, ok := evt.Data.(map[string]string)
		require.True(t, ok, "payload rides Data per the agent_died convention")
		assert.Equal(t, "health_watchdog", data["reason"])
		assert.Equal(t, "hung", data["verdict"])
		assert.Equal(t, "1", data["agentRestarts"])
		assert.Equal(t, "2026-10-19T17:08:14Z", data["at"])
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent_restarted event on the user stream")
	}
}

func TestOnAgentRestart_NoRestartsNoEvent(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, err := env.handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-1", sub)

	// Suppression-only state (the incident's pre-kill shape): no fired
	// restart, no event.
	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	ws.Status.AgentSupervisor = &v1.AgentSupervisorStatus{SuppressedRestarts: 74, LastSuppressedVerdict: "starved"}
	env.handler.onAgentRestart(ws)

	select {
	case evt := <-sub.Ch:
		t.Fatalf("no event expected for suppression-only state, got %+v", evt)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOnAgentRestart_NilBrokerNoop(t *testing.T) {
	env := newTestEnv(t)
	// userBroker deliberately nil — must not panic.
	env.handler.onAgentRestart(restartWorkspaceCRD("ws-1", 1, time.Now()))
}

func TestOnAgentRestart_OptionalFieldsOmitted(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	sub, err := env.handler.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeUser("user-1", sub)

	// Uncorroborated crash restart: empty verdict → key omitted.
	at := metav1.NewTime(time.Now())
	ws := makeWorkspaceCRDWithStatus("ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	ws.Status.AgentSupervisor = &v1.AgentSupervisorStatus{
		AgentRestarts:    2,
		LastAgentRestart: &v1.AgentRestartEvent{Reason: "crash", At: &at},
	}
	env.handler.onAgentRestart(ws)

	select {
	case evt := <-sub.Ch:
		data, ok := evt.Data.(map[string]string)
		require.True(t, ok)
		assert.Equal(t, "crash", data["reason"])
		_, hasVerdict := data["verdict"]
		assert.False(t, hasVerdict, "empty verdict must be omitted, not sent empty")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent_restarted event")
	}
}
