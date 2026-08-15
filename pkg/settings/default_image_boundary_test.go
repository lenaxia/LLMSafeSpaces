// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

import (
	"context"
	"strings"
	"testing"
)

// Regression for the 2026-08-14 incident: the seeded workspace.defaultImage
// floating tag let a registry mirror serve a 4-day-stale image digest to
// new workspaces. The write boundary must now reject mutable-tag refs while
// still accepting pinned refs, digests, RTE names, and the empty value
// (cleared setting = chart-pinned base RuntimeEnvironment).
func TestInstanceService_Set_WorkspaceDefaultImage_RejectsFloatingTag(t *testing.T) {
	reject := []string{
		"ghcr.io/lenaxia/llmsafespaces/base:latest",
		"ghcr.io/lenaxia/llmsafespaces/base:main",
		"ghcr.io/lenaxia/llmsafespaces/base", // untagged = implicit :latest
		"registry.local:5000/team/img:dev",
	}
	for _, v := range reject {
		store := newMockStore()
		svc := newTestService(store)
		err := svc.Set(context.Background(), "workspace.defaultImage", v)
		if err == nil {
			t.Errorf("Set(%q) must be rejected", v)
			continue
		}
		if !strings.Contains(err.Error(), "mutable") && !strings.Contains(err.Error(), ":latest") {
			t.Errorf("Set(%q) error should explain the pinning requirement, got: %v", v, err)
		}
	}
}

func TestInstanceService_Set_WorkspaceDefaultImage_AcceptsPinned(t *testing.T) {
	accept := []string{
		"",
		"base",
		"ghcr.io/lenaxia/llmsafespaces/base:0.15.5",
		"ghcr.io/lenaxia/llmsafespaces/base:sha-9cf7947",
		"ghcr.io/lenaxia/llmsafespaces/base@sha256:808af6bbafc2df306e6a2fcf8945536668af23b6233de0a05a895f05b8671aa0",
		"  ghcr.io/lenaxia/llmsafespaces/base:0.15.5  ", // normalized (trimmed)
	}
	for _, v := range accept {
		store := newMockStore()
		svc := newTestService(store)
		if err := svc.Set(context.Background(), "workspace.defaultImage", v); err != nil {
			t.Errorf("Set(%q) must be accepted, got: %v", v, err)
		}
	}
}

func TestInstanceService_Get_WorkspaceDefaultImage_DefaultFallsThroughToEmpty(t *testing.T) {
	store := newMockStore()
	svc := newTestService(store)
	got, err := svc.GetString(context.Background(), "workspace.defaultImage")
	if err != nil {
		t.Fatalf("GetString failed: %v", err)
	}
	if got != "" {
		t.Errorf("schema default must be empty (RTE fallback), got %q", got)
	}
}
