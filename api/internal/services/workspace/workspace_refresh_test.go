// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// RefreshWorkspaceCompute re-syncs a workspace CRD with the platform's current
// defaults (resources, security level, storage class, max active sessions) and
// bumps spec.restartGeneration so the controller rebuilds the pod — picking up
// the latest image version (resolved from the RuntimeEnvironment CR at build
// time) and the refreshed resource requests.
//
// Tests cover: happy path (defaults applied + generation bumped), suspended
// workspace (allowed), no settings configured (generation bumped, spec
// unchanged), wrong owner (forbidden), terminal phases (rejected), and K8s
// get/update failures.

func TestRefreshWorkspaceCompute_Active_OverwritesResourcesAndBumpsGeneration(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu":     "1000m",
		"workspace.defaultResources.memory":  "2Gi",
		"workspace.defaultSecurityLevel":     "high",
		"workspace.defaultStorageClass":      "fast-ssd",
		"workspace.defaultMaxActiveSessions": 8,
	})
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.RestartGeneration = 4
	crd.Spec.Resources = &v1.ResourceRequirements{CPU: "500m", Memory: "512Mi"}
	crd.Spec.SecurityLevel = "standard"
	crd.Spec.MaxActiveSessions = 5
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.RestartGeneration == 5 &&
			ws.Spec.Resources != nil &&
			ws.Spec.Resources.CPU == "1000m" &&
			ws.Spec.Resources.Memory == "2Gi" &&
			ws.Spec.SecurityLevel == "high" &&
			ws.Spec.Storage.StorageClassName == "fast-ssd" &&
			ws.Spec.MaxActiveSessions == 8
	})).Return(crd, nil)

	res, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, int64(5), res.RestartGeneration)
	f.ws.AssertExpectations(t)
	f.ws.AssertNotCalled(t, "UpdateStatus")
}

func TestRefreshWorkspaceCompute_NilResources_InitializedFromDefaults(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu":    "1000m",
		"workspace.defaultResources.memory": "2Gi",
	})
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.Resources = nil
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources != nil &&
			ws.Spec.Resources.CPU == "1000m" &&
			ws.Spec.Resources.Memory == "2Gi"
	})).Return(crd, nil)

	res, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), res.RestartGeneration)
}

func TestRefreshWorkspaceCompute_NoSettings_BumpsGenerationOnly(t *testing.T) {
	// No instance settings configured: refresh degrades to a pure pod rebuild
	// (restartGeneration bump). Existing user-set resources are preserved.
	f := newFixture(t)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.Resources = &v1.ResourceRequirements{CPU: "2000m", Memory: "4Gi"}
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.RestartGeneration == 1 &&
			ws.Spec.Resources.CPU == "2000m" &&
			ws.Spec.Resources.Memory == "4Gi"
	})).Return(crd, nil)

	res, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), res.RestartGeneration)
}

func TestRefreshWorkspaceCompute_Suspended_AppliesDefaultsAndResumes(t *testing.T) {
	// A suspended workspace has no pod. handleSuspended does NOT observe
	// restartGeneration (only spec.suspend), so a generation bump alone is a
	// no-op. Refresh must therefore ALSO request a resume (spec.suspend=false)
	// so the controller builds a fresh pod carrying the refreshed spec.
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.memory": "2Gi",
	})
	ctx := context.Background()

	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended

	// ActivateWorkspace (the resume path) re-Gets and re-Updates, so Get is
	// called more than once. Returning the suspended CRD each time mirrors
	// the apiserver reading a not-yet-resumed object.
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.db.On("ListWorkspaces", mock.Anything, "user1", mock.Anything, mock.Anything).
		Return([]*types.WorkspaceMetadata{}, &types.PaginationMetadata{}, nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)
	// enforceMaxActiveWorkspaces (called by ActivateWorkspace) lists workspaces.
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)

	// Capture every Update so we can assert both the refresh write and the
	// resume write happened. The returned CRD must carry spec.suspend=false
	// to satisfy ActivateWorkspace's post-write persistence check.
	resumeCrd := suspendedCrd.DeepCopy()
	suspendFalse := false
	resumeCrd.Spec.Suspend = &suspendFalse
	var updates []*v1.Workspace
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Run(func(args mock.Arguments) { updates = append(updates, args.Get(1).(*v1.Workspace)) }).
		Return(resumeCrd, nil)

	res, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, int64(1), res.RestartGeneration)

	// One update reapplied the defaults + bumped the generation...
	sawRefresh := false
	// ...and one wrote the resume request (spec.suspend=false).
	sawResume := false
	for _, u := range updates {
		if u.Spec.RestartGeneration == 1 && u.Spec.Resources != nil && u.Spec.Resources.Memory == "2Gi" {
			sawRefresh = true
		}
		if u.Spec.Suspend != nil && !*u.Spec.Suspend {
			sawResume = true
		}
	}
	assert.True(t, sawRefresh, "refresh must write re-applied defaults + bumped generation")
	assert.True(t, sawResume, "refresh must request a resume (spec.suspend=false) when suspended")
}

func TestRefreshWorkspaceCompute_Suspended_ActivateFails_ReturnsError(t *testing.T) {
	// If the resume step fails (e.g. the active-workspace cap check can't read
	// the user's workspaces), the error surfaces. The spec refresh has already
	// persisted, which is the correct partial state — the next manual activate
	// picks up the new config.
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "1000m",
	})
	ctx := context.Background()

	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	// enforceMaxActiveWorkspaces → dbService.ListWorkspaces fails.
	f.db.On("ListWorkspaces", mock.Anything, "user1", mock.Anything, mock.Anything).
		Return([]*types.WorkspaceMetadata(nil), (*types.PaginationMetadata)(nil), errors.New("db unavailable"))
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)
	// The refresh spec write still succeeds (happens before the resume).
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(suspendedCrd, nil)

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.Error(t, err)
}

func TestRefreshWorkspaceCompute_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "1000m",
	})
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	f.ws.AssertNotCalled(t, "Update")
}

func TestRefreshWorkspaceCompute_TerminalPhases_Rejected(t *testing.T) {
	for _, phase := range []v1.WorkspacePhase{v1.WorkspacePhaseTerminating, v1.WorkspacePhaseTerminated} {
		t.Run(string(phase), func(t *testing.T) {
			f := newDefaultsFixture(t, map[string]any{
				"workspace.defaultResources.cpu": "1000m",
			})
			ctx := context.Background()

			crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
			crd.Status.Phase = phase
			f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

			_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
			assert.Error(t, err)
			f.ws.AssertNotCalled(t, "Update")
		})
	}
}

func TestRefreshWorkspaceCompute_K8sGetFails(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "1000m",
	})
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), errors.New("boom"))

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.Error(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestRefreshWorkspaceCompute_K8sUpdateFails(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "1000m",
	})
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return((*v1.Workspace)(nil), errors.New("etcd unavailable"))

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_refresh_failed")
}

// === Partial-settings coverage: exercises the conditional logic in ===
// reapplyComputeDefaults where only one of CPU/memory is explicitly configured.
// The settings service returns schema defaults for unconfigured keys (CPU
// default "500m", memory default "1Gi" — from schema.go, NOT registry.go),
// so refresh converges BOTH resources to the platform default: the configured
// one from the admin override, the unconfigured one from the schema default.

func TestRefreshWorkspaceCompute_OnlyCPUConfigured_MemoryGetsSchemaDefault(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "1000m",
		// memory default intentionally absent → settings returns schema default "1Gi"
	})
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.Resources = &v1.ResourceRequirements{CPU: "250m", Memory: "4Gi"}
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources.CPU == "1000m" && ws.Spec.Resources.Memory == "1Gi"
	})).Return(crd, nil)

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestRefreshWorkspaceCompute_OnlyMemoryConfigured_CPUGetsSchemaDefault(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		// cpu default intentionally absent → settings returns schema default "500m"
		"workspace.defaultResources.memory": "2Gi",
	})
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.Resources = &v1.ResourceRequirements{CPU: "2000m", Memory: "512Mi"}
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources.CPU == "500m" && ws.Spec.Resources.Memory == "2Gi"
	})).Return(crd, nil)

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === Non-terminal phase coverage: only Terminating/Terminated are rejected;
// every other phase is allowed (refresh updates spec for the next pod build).
// Active and Suspended are covered above; this covers the remaining phases.

func TestRefreshWorkspaceCompute_NonTerminalPhases_AllowedAndBumpsGeneration(t *testing.T) {
	for _, phase := range []v1.WorkspacePhase{
		v1.WorkspacePhasePending,
		v1.WorkspacePhaseCreating,
		v1.WorkspacePhaseResuming,
		v1.WorkspacePhaseFailed,
		v1.WorkspacePhaseSuspending,
	} {
		t.Run(string(phase), func(t *testing.T) {
			f := newDefaultsFixture(t, map[string]any{
				"workspace.defaultResources.cpu": "1000m",
			})
			ctx := context.Background()

			crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
			crd.Status.Phase = phase
			f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
			f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
				return ws.Spec.RestartGeneration == 1 &&
					ws.Spec.Resources != nil && ws.Spec.Resources.CPU == "1000m"
			})).Return(crd, nil)

			res, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
			assert.NoError(t, err)
			assert.Equal(t, int64(1), res.RestartGeneration)
		})
	}
}

// #1505: refresh-compute stamps the user-consent force marker keyed to
// the generation it just bumped — the controller's generation recycle
// bypasses the #761 session drain for exactly that generation.
func TestRefreshWorkspaceCompute_StampsForceMarkerKeyedToGeneration(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.RestartGeneration = 4
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.RestartGeneration == 5 &&
			ws.Annotations[v1.AnnotationForceRecycle] == "5"
	})).Return(crd, nil)

	_, err := f.svc.RefreshWorkspaceCompute(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// #1505 rework: the suspend-force variant is GONE (post-#1510 every
// suspend shares the single bounded SuspendWorkspace path). This pin
// guards the invariant that survived: suspend NEVER stamps the force
// marker — only RefreshWorkspaceCompute does.
func TestSuspendWorkspace_NeverStampsForceMarker(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Suspend != nil && *ws.Spec.Suspend &&
			ws.Annotations[v1.AnnotationForceRecycle] == ""
	})).Return(crd, nil)

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}
