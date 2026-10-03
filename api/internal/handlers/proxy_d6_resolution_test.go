// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fakeSessionAlerts records Record/Resolve calls so the resolution
// flow (persisted resolved_at + SSE alert_resolved + cooldown drop)
// is observable without a database.
type fakeSessionAlerts struct {
	recorded []string // "record:<ws>:<session>" / "resolve:<ws>"
}

func (f *fakeSessionAlerts) RecordAlert(workspaceID, sessionID, _ string, _ int) {
	f.recorded = append(f.recorded, "record:"+workspaceID+":"+sessionID)
}

func (f *fakeSessionAlerts) ResolveWorkspace(workspaceID string) {
	f.recorded = append(f.recorded, "resolve:"+workspaceID)
}

func (f *fakeSessionAlerts) ListByWorkspace(_ context.Context, _ string, _ int) ([]types.SessionAlert, error) {
	return nil, nil
}

func (f *fakeSessionAlerts) Start() error { return nil }
func (f *fakeSessionAlerts) Stop() error  { return nil }

// TestEscalateHungs_ResolvesWhenRecovered pins the full resolution
// flow: after an alert, a sweep that observes the hang ended emits
// workspace.alert_resolved, persists the resolution, and drops the
// cooldown — so a fresh hang re-alerts immediately and reloading
// clients never re-latch the badge from history.
func TestEscalateHungs_ResolvesWhenRecovered(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour // deterministic: cooldown spans the test
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	hung := true
	var alerts fakeSessionAlerts
	env, broker := newD6Env(t, func(w http.ResponseWriter, _ *http.Request) {
		seconds := 10
		if hung {
			seconds = int((busyAlertOlderThan + 5*time.Minute).Seconds())
		}
		w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{
			Healthy:           true,
			OldestBusySeconds: seconds,
			BusyAges:          map[string]int{"ses-x": seconds},
		})
	})
	env.handler.sessionAlerts = &alerts

	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	// Sweep 1: hung — alert + persist.
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert", evt.Type, "sweep 1 must alert")
	default:
		t.Fatal("hung workspace must alert")
	}
	require.Len(t, alerts.recorded, 1)
	assert.True(t, strings.HasPrefix(alerts.recorded[0], "record:ws-1:"), alerts.recorded[0])

	// Sweep 2: recovered — resolve over SSE + persist + drop cooldown.
	hung = false
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert_resolved", evt.Type)
		assert.Equal(t, "session_hung", evt.Status)
	default:
		t.Fatal("recovered workspace must emit workspace.alert_resolved")
	}
	require.Len(t, alerts.recorded, 2)
	assert.Equal(t, "resolve:ws-1", alerts.recorded[1])

	// Sweep 3: hung AGAIN — the stale cooldown must not suppress the
	// fresh hang (resolution dropped it).
	hung = true
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert", evt.Type, "fresh hang after resolution must re-alert immediately")
	default:
		t.Fatal("cooldown must have been dropped at resolution")
	}
	require.Len(t, alerts.recorded, 3)
	assert.True(t, strings.HasPrefix(alerts.recorded[2], "record:ws-1:"), alerts.recorded[2])
}

// TestEscalateHungs_StillHungInsideCooldownNeitherAlertsNorResolves:
// the through-cooldown poll exists to detect recovery; while the hang
// persists it must stay silent in both directions.
func TestEscalateHungs_StillHungInsideCooldownNeitherAlertsNorResolves(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	var alerts fakeSessionAlerts
	env, broker := newD6Env(t, hungStatusz(int((busyAlertOlderThan+5*time.Minute).Seconds())))
	env.handler.sessionAlerts = &alerts

	sub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case <-sub.Ch: // drain sweep 1's alert
	default:
		t.Fatal("hung workspace must alert")
	}
	require.Len(t, alerts.recorded, 1)

	// Second sweep, still hung, inside cooldown: no re-alert, no resolve.
	env.handler.escalateHungs([]string{"ws-1"})
	select {
	case evt := <-sub.Ch:
		t.Fatalf("cooldown must suppress both directions: got %+v", evt)
	default:
	}
	assert.Len(t, alerts.recorded, 1)
}

// TestEscalateHungs_NeverAlertedNeverResolves: resolution only fires
// for workspaces that alerted (nothing to resolve otherwise) — no
// per-tick resolve writes for healthy fleets.
func TestEscalateHungs_NeverAlertedNeverResolves(t *testing.T) {
	var alerts fakeSessionAlerts
	env, _ := newD6Env(t, hungStatusz(10))
	env.handler.sessionAlerts = &alerts

	env.handler.escalateHungs([]string{"ws-1"})
	env.handler.escalateHungs([]string{"ws-1"})

	assert.Empty(t, alerts.recorded)
	assert.False(t, strings.Contains(strings.Join(alerts.recorded, ","), "resolve"))
}
