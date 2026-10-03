// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

// seedAlert inserts one alert row with explicit created_at control (the
// stale-heal semantics depend on age, not insert time).
func seedAlert(t *testing.T, h *testharness.Harness, ws, ses string, createdAt, resolvedAt time.Time) {
	t.Helper()
	var resolved any
	if !resolvedAt.IsZero() {
		resolved = resolvedAt
	}
	_, err := h.Pool().Exec(context.Background(),
		`INSERT INTO session_alerts (workspace_id, session_id, alert, oldest_busy_seconds, created_at, resolved_at)
		 VALUES ($1, $2, 'session_hung', 960, $3, $4)`, ws, ses, createdAt, resolved)
	require.NoError(t, err, "seed alert row")
}

// TestIntegration_SessionAlerts_ResolveSemantics pins the resolve
// UPDATE (workspace-scoped, unresolved-only) and the ListSessionAlerts
// scan of the new resolved_at column (NULL and set — scan drift here
// 500s the alerts endpoint at runtime; same class as the
// bases_null_scan incident).
func TestIntegration_SessionAlerts_ResolveSemantics(t *testing.T) {
	h := testharness.New(t)
	ctx := h.NewContext()
	pool := h.Pool()
	svc := &Service{DB: h.SQLDB()}

	const wsA, wsB = "int-alert-ws-a", "int-alert-ws-b"
	_, _ = pool.Exec(ctx, "DELETE FROM session_alerts WHERE workspace_id IN ($1, $2)", wsA, wsB)

	now := time.Now().UTC()
	seedAlert(t, h, wsA, "ses-live", now.Add(-20*time.Minute), time.Time{})   // unresolved, fresh
	seedAlert(t, h, wsA, "ses-old", now.Add(-2*time.Hour), now.Add(-time.Hour)) // already resolved
	seedAlert(t, h, wsB, "ses-other", now.Add(-20*time.Minute), time.Time{})  // other workspace: must not move

	n, err := svc.ResolveSessionAlerts(ctx, wsA)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only wsA's live row resolves; the already-resolved row stays put")

	alerts, err := svc.ListSessionAlerts(ctx, wsA, 10)
	require.NoError(t, err)
	require.Len(t, alerts, 2)
	bySession := map[string]*struct {
		resolved bool
	}{}
	for _, a := range alerts {
		bySession[a.SessionID] = &struct{ resolved bool }{a.ResolvedAt != nil}
	}
	assert.True(t, bySession["ses-live"].resolved, "the live row is now resolved")
	assert.True(t, bySession["ses-old"].resolved, "previously resolved stays resolved")

	other, err := svc.ListSessionAlerts(ctx, wsB, 10)
	require.NoError(t, err)
	require.Len(t, other, 1)
	assert.Nil(t, other[0].ResolvedAt, "wsB's row must be untouched by wsA's resolve")
}

// TestIntegration_SessionAlerts_StaleHealSemantics pins the read-side
// heal's UPDATE: unresolved rows older than the cutoff resolve; fresh
// unresolved rows stay live; resolved rows are untouched.
func TestIntegration_SessionAlerts_StaleHealSemantics(t *testing.T) {
	h := testharness.New(t)
	ctx := h.NewContext()
	pool := h.Pool()
	svc := &Service{DB: h.SQLDB()}

	const ws = "int-alert-ws-stale"
	_, _ = pool.Exec(ctx, "DELETE FROM session_alerts WHERE workspace_id = $1", ws)

	now := time.Now().UTC()
	cutoff := now.Add(-time.Hour)
	seedAlert(t, h, ws, "ses-stale", now.Add(-90*time.Minute), time.Time{}) // orphan: older than cutoff
	seedAlert(t, h, ws, "ses-fresh", now.Add(-10*time.Minute), time.Time{}) // live hang: newer
	seedAlert(t, h, ws, "ses-done", now.Add(-3*time.Hour), now.Add(-2*time.Hour))

	n, err := svc.ResolveStaleSessionAlerts(ctx, ws, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the orphan older than the cutoff heals")

	alerts, err := svc.ListSessionAlerts(ctx, ws, 10)
	require.NoError(t, err)
	require.Len(t, alerts, 3)
	for _, a := range alerts {
		switch a.SessionID {
		case "ses-stale":
			assert.NotNil(t, a.ResolvedAt, "the orphan must be healed")
		case "ses-fresh":
			assert.Nil(t, a.ResolvedAt, "a live hang's fresh row must stay unresolved")
		case "ses-done":
			assert.NotNil(t, a.ResolvedAt, "already-resolved row untouched")
		}
	}
}
