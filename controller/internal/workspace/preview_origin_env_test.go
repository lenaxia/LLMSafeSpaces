// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for PREVIEW_ORIGIN_BASE_DOMAIN injection into workspace pods
// (Epic 66 Phase 1). When PreviewOriginBaseDomain is configured, the
// controller stamps it into the main container env; agentd's
// dev_preview_url MCP tool reads it and emits the per-workspace-origin
// bootstrap URL instead of the path-based URL. Empty (the chart default)
// leaves the env unset — path-based dev preview, unchanged behavior.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestBuildPod_PreviewOriginDomain_NotInjectedWhenEmpty: empty config
// (chart default, previewOrigin.enabled=false) → env absent.
func TestBuildPod_PreviewOriginDomain_NotInjectedWhenEmpty(t *testing.T) {
	ws := makeWorkspace("ws-no-pvod", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-no-pvod"
	pvc := makeBoundPVC("workspace-ws-no-pvod", "default", ws.UID)
	pw := makePasswordSecret("ws-no-pvod", "default")
	rte := makeRuntimeEnv("python-3.11")

	r := reconcilerFor(t, ws, pvc, pw, rte)
	r.PreviewOriginBaseDomain = ""

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := mainContainer(pod)
	require.NotNil(t, main)
	for _, e := range main.Env {
		assert.NotEqual(t, "PREVIEW_ORIGIN_BASE_DOMAIN", e.Name,
			"PREVIEW_ORIGIN_BASE_DOMAIN must not be set when PreviewOriginBaseDomain is empty")
	}
}

// TestBuildPod_PreviewOriginDomain_Injected: configured domain appears
// verbatim on the main container.
func TestBuildPod_PreviewOriginDomain_Injected(t *testing.T) {
	ws := makeWorkspace("ws-pvod", "default", v1.WorkspacePhaseCreating)
	ws.Status.PVCName = "workspace-ws-pvod"
	pvc := makeBoundPVC("workspace-ws-pvod", "default", ws.UID)
	pw := makePasswordSecret("ws-pvod", "default")
	rte := makeRuntimeEnv("python-3.11")

	r := reconcilerFor(t, ws, pvc, pw, rte)
	r.PreviewOriginBaseDomain = "safespaces.dev"

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := mainContainer(pod)
	require.NotNil(t, main)
	found := false
	for _, e := range main.Env {
		if e.Name == "PREVIEW_ORIGIN_BASE_DOMAIN" {
			found = true
			assert.Equal(t, "safespaces.dev", e.Value,
				"domain must be passed verbatim")
		}
	}
	assert.True(t, found, "PREVIEW_ORIGIN_BASE_DOMAIN must be set when PreviewOriginBaseDomain is configured")
}
