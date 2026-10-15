// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// #1685 (owner ruling, issue comment 6202575356): the 'busy 15 min may be
// hung' nudge fired on long-but-healthy turns — this fleet's turns
// regularly run 30-60+ min. The alert now requires MESSAGE-LEVEL evidence:
// no new messages from either side (LLM-completed or user-sent) for the
// no-progress window, in ADDITION to the 15-min busy floor (kept as a
// semantic guard; in practice the 30-min window dominates). Stream chunks,
// tool execution, and heartbeats do NOT count as progress.

// evidenceStatusz builds a statusz with independent busy-age and
// message-silence clocks — the two-window shape the gate reads.
func evidenceStatusz(busySeconds, lastProgressSeconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{
			Healthy:             true,
			OldestBusySeconds:   busySeconds,
			BusyAges:            map[string]int{"ses-x": busySeconds},
			LastProgressSeconds: lastProgressSeconds,
		})
	}
}

func mustSubscribeWorkspace(t *testing.T, broker interface {
	SubscribeWorkspace(string) (*eventbroker.Subscriber, error)
	UnsubscribeWorkspace(string, *eventbroker.Subscriber)
}, workspaceID string) (*eventbroker.Subscriber, func()) {
	t.Helper()
	sub, err := broker.SubscribeWorkspace(workspaceID)
	require.NoError(t, err)
	return sub, func() { broker.UnsubscribeWorkspace(workspaceID, sub) }
}

func expectWorkspaceAlertEvt(t *testing.T, sub *eventbroker.Subscriber) map[string]any {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		require.Equal(t, "workspace.alert", evt.Type)
		require.Equal(t, "session_hung", evt.Status)
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok)
		return data
	default:
		t.Fatal("expected workspace.alert/session_hung on the subscription")
		return nil
	}
}

func expectNoWorkspaceAlertEvt(t *testing.T, sub *eventbroker.Subscriber) {
	t.Helper()
	select {
	case evt := <-sub.Ch:
		t.Fatalf("must not alert: got %+v", evt)
	default:
	}
}

// TestEscalateHungs_EvidenceGate pins the two-window gate: an alert fires
// only when the busy floor AND the message-silence window are BOTH met.
// RED against the pre-#1685 single-window gate: the owner's-bash-case and
// boundary arms fail (busy ≥ floor alone alerted).
func TestEscalateHungs_EvidenceGate(t *testing.T) {
	origCooldown := busyAlertCooldown
	busyAlertCooldown = time.Hour
	t.Cleanup(func() { busyAlertCooldown = origCooldown })

	t.Run("both windows met: busy 15m + no messages 30m alerts", func(t *testing.T) {
		env, broker := newD6Env(t, evidenceStatusz(900, 1800))
		sub, done := mustSubscribeWorkspace(t, broker, "ws-1")
		defer done()

		env.handler.escalateHungs([]string{"ws-1"})
		data := expectWorkspaceAlertEvt(t, sub)
		assert.Equal(t, "notify_only", data["policy"])
		// The evidence rides the payload so consumers can state it honestly.
		assert.Contains(t, data, "last_progress_seconds")
	})

	t.Run("owner's bash case: last message 20m ago + long tool running is NOT a hang", func(t *testing.T) {
		// busy 20m, message silence 20m: the silence window (30m) is not
		// met — a long tool execution with recent-enough messages is a
		// healthy long turn, not a hang. Pre-#1685 this alerted (busy ≥
		// 15m alone); the ruling makes this the canonical NO-alert case.
		env, broker := newD6Env(t, evidenceStatusz(1200, 1200))
		sub, done := mustSubscribeWorkspace(t, broker, "ws-1")
		defer done()

		env.handler.escalateHungs([]string{"ws-1"})
		expectNoWorkspaceAlertEvt(t, sub)
	})

	t.Run("silence window met but busy floor not: no alert", func(t *testing.T) {
		// busy 10m, no messages for an hour: the floor (semantic guard)
		// holds — freshly-busy does not alert regardless of quiet.
		env, broker := newD6Env(t, evidenceStatusz(600, 3600))
		sub, done := mustSubscribeWorkspace(t, broker, "ws-1")
		defer done()

		env.handler.escalateHungs([]string{"ws-1"})
		expectNoWorkspaceAlertEvt(t, sub)
	})

	t.Run("boundary: no-message silence 1799s does not alert, 1800s does", func(t *testing.T) {
		env, broker := newD6Env(t, evidenceStatusz(int((busyAlertOlderThan+time.Hour).Seconds()), 1799))
		sub, done := mustSubscribeWorkspace(t, broker, "ws-1")
		defer done()

		env.handler.escalateHungs([]string{"ws-1"})
		expectNoWorkspaceAlertEvt(t, sub)

		env2, broker2 := newD6Env(t, evidenceStatusz(int((busyAlertOlderThan+time.Hour).Seconds()), 1800))
		sub2, done2 := mustSubscribeWorkspace(t, broker2, "ws-1")
		defer done2()
		env2.handler.escalateHungs([]string{"ws-1"})
		expectWorkspaceAlertEvt(t, sub2)
	})

	t.Run("resumed messages resolve a live alert", func(t *testing.T) {
		// The statusz is a mutable closure (the resolution-test pattern):
		// sweep 1 hangs on both clocks; sweep 2 keeps the turn busy but a
		// message just arrived — the evidence is gone, the alert resolves.
		quiet := true
		env, broker := newD6Env(t, func(w http.ResponseWriter, _ *http.Request) {
			progress := 30
			if quiet {
				progress = 3600
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{
				Healthy:             true,
				OldestBusySeconds:   3600,
				BusyAges:            map[string]int{"ses-x": 3600},
				LastProgressSeconds: progress,
			})
		})
		sub, done := mustSubscribeWorkspace(t, broker, "ws-1")
		defer done()

		env.handler.escalateHungs([]string{"ws-1"})
		expectWorkspaceAlertEvt(t, sub)

		quiet = false
		env.handler.escalateHungs([]string{"ws-1"})
		select {
		case evt := <-sub.Ch:
			require.Equal(t, "workspace.alert_resolved", evt.Type, "resumed messages must resolve the alert")
		default:
			t.Fatal("evidence-cleared workspace must emit workspace.alert_resolved")
		}
	})
}
