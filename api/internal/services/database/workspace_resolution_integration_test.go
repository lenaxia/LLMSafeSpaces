// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

// Workspace resolution contract — the exact surface the US-70.0 harness
// fix (pool AC-1 failure) depends on. Production mints workspaceID =
// uuid.New() and the CR name IS that UUID, so every API workspace route
// resolves WHERE workspaces.id = $1 against a uuid column. These pins
// document both historical failure modes of the harness (non-UUID CR
// names; CR-only workspaces with no metadata row) as executable truth.

func newWorkspaceServiceForTest(t *testing.T) *Service {
	t.Helper()
	h := testharness.New(t)
	return &Service{DB: h.SQLDB()}
}

func TestIntegration_GetWorkspace_UUIDNameWithRow_Resolves(t *testing.T) {
	svc := newWorkspaceServiceForTest(t)
	ctx := context.Background()

	wsID := "8e65d000-0000-4000-8000-000000000001"
	defer func() { _, _ = svc.DB.ExecContext(ctx, "DELETE FROM workspaces WHERE id = $1", wsID) }()

	_, err := svc.DB.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size)
		VALUES ($1, 'us70-harness-pin', 'e2e-sd-user', 'llmsafespaces', 'python:3.11', '1Gi')
		ON CONFLICT (id) DO UPDATE SET user_id = EXCLUDED.user_id, deleted_at = NULL
	`, wsID)
	require.NoError(t, err, "seed the metadata row the way the harness does")

	meta, err := svc.GetWorkspace(ctx, wsID)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, "e2e-sd-user", meta.UserID, "ownership resolves through the seeded row")
}

func TestIntegration_GetWorkspace_NonUUIDName_CannotResolve(t *testing.T) {
	svc := newWorkspaceServiceForTest(t)
	ctx := context.Background()

	_, err := svc.GetWorkspace(ctx, "e2esd0000-0000-0000-0000-0000000000001")
	require.Error(t, err,
		"a non-UUID CR name hits the uuid column comparison and errors (22P02 upstream of the 500) — "+
			"this is the 2026-08-31 pool AC-1 failure mode: DNS-prefix workspace names can never resolve")
}

func TestIntegration_GetWorkspace_UUIDNoRow_IsNotFound(t *testing.T) {
	svc := newWorkspaceServiceForTest(t)
	ctx := context.Background()

	meta, err := svc.GetWorkspace(ctx, "8e65d000-0000-4000-8000-00000000dead")
	require.NoError(t, err)
	assert.Nil(t, meta,
		"a valid UUID with no metadata row resolves to not-found (nil) — the handler maps 404; "+
			"a kubectl-applied CR without the seeded row is the second AC-1 failure mode")
}
