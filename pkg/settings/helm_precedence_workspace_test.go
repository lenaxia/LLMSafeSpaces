// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the workspace.defaultStorageClass Helm-override glue
// added in the v0.2.2 chart. The generic SetHelmOverrides mechanism is
// covered in helm_precedence_test.go; these tests specifically guard the
// key literal used in api/internal/app/app.go so a typo (e.g. renaming
// the schema key but forgetting the app.go string) would fail here rather
// than silently causing SetHelmOverrides to warn-and-ignore.
//
// This addresses the PR #509 automated reviewer finding that the app.go
// glue block was structurally correct but not directly exercised.

// TestWorkspaceDefaultStorageClass_HelmOverride_MarksReadOnly verifies
// that pinning workspace.defaultStorageClass via SetHelmOverrides (as
// app.go does when cfg.Workspace.DefaultStorageClass is non-empty)
// marks the key ReadOnly in the schema so the admin UI will disable it.
func TestWorkspaceDefaultStorageClass_HelmOverride_MarksReadOnly(t *testing.T) {
	svc := newTestInstanceService()
	// Mirrors api/internal/app/app.go SetHelmOverrides call.
	svc.SetHelmOverrides(map[string]any{
		"workspace.defaultStorageClass": "longhorn-2r",
	})

	found := false
	for _, def := range svc.Schema() {
		if def.Key == "workspace.defaultStorageClass" {
			assert.True(t, def.ReadOnly,
				"workspace.defaultStorageClass must be ReadOnly after SetHelmOverrides so the admin UI disables the input")
			found = true
			break
		}
	}
	require.True(t, found, "workspace.defaultStorageClass must be in schema")
}

// TestWorkspaceDefaultStorageClass_HelmOverride_ServesHelmValue verifies
// that after SetHelmOverrides, GetString returns the Helm value even if
// the DB has a different value. This is the core semantic that lets
// operators declare a value in values.yaml and know an admin cannot
// override it via the settings API.
func TestWorkspaceDefaultStorageClass_HelmOverride_ServesHelmValue(t *testing.T) {
	store := &stubStore{data: map[string]json.RawMessage{
		"workspace.defaultStorageClass": json.RawMessage(`"admin-set-value"`),
	}}
	svc := NewInstanceService(store, nil)
	svc.SetHelmOverrides(map[string]any{
		"workspace.defaultStorageClass": "longhorn-2r",
	})
	require.NoError(t, svc.Start())

	got, err := svc.GetString(context.Background(), "workspace.defaultStorageClass")
	require.NoError(t, err)
	assert.Equal(t, "longhorn-2r", got,
		"GetString must return the Helm value, not the DB value — operators cannot have their chart choice silently overridden")

	// Also verify the typed accessor path used by workspace_service.go.
	sc, err := svc.DefaultStorageClass(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "longhorn-2r", sc,
		"DefaultStorageClass typed accessor (used by workspace_service.go:1118) must also return the Helm value")
}

// TestWorkspaceDefaultStorageClass_HelmOverride_RejectsAdminSet verifies
// that a PUT /admin/settings/workspace.defaultStorageClass while Helm is
// managing the key returns ErrReadOnly (which the settings handler
// translates to HTTP 409).
func TestWorkspaceDefaultStorageClass_HelmOverride_RejectsAdminSet(t *testing.T) {
	svc := newTestInstanceService()
	svc.SetHelmOverrides(map[string]any{
		"workspace.defaultStorageClass": "longhorn-2r",
	})

	err := svc.Set(context.Background(), "workspace.defaultStorageClass", "admin-attempted-override")
	assert.ErrorIs(t, err, ErrReadOnly,
		"admin PUT to a Helm-managed key must return ErrReadOnly (→ HTTP 409)")
}
