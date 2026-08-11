// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// inMemorySettingsStore implements settings.InstanceStore for testing.
type inMemorySettingsStore struct {
	data map[string]json.RawMessage
}

func (s *inMemorySettingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return s.data, nil
}

func (s *inMemorySettingsStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	s.data[key] = value
	return nil
}

func newTestSettings(vals map[string]any) *settings.InstanceService {
	data := make(map[string]json.RawMessage)
	for k, v := range vals {
		raw, _ := json.Marshal(v)
		data[k] = raw
	}
	var log pkginterfaces.LoggerInterface = lmocks.NewMockLogger()
	svc := settings.NewInstanceService(&inMemorySettingsStore{data: data}, log)
	svc.Start()
	return svc
}

func newDefaultsFixture(t *testing.T, settingsData map[string]any) *fixture {
	t.Helper()
	f := newFixture(t)
	if settingsData != nil {
		f.svc.SetInstanceSettings(newTestSettings(settingsData))
	}
	return f
}

// === US-13.0: workspace.defaultImage ===

func TestCreateWorkspace_EmptyRuntime_UsesDefaultImage(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "ghcr.io/lenaxia/llmsafespaces/base:v2",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "ghcr.io/lenaxia/llmsafespaces/base:v2"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: ""}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_ExplicitRuntime_NotOverridden(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "ghcr.io/lenaxia/llmsafespaces/base:v2",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "python:3.11"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "python:3.11"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_NoSettings_EmptyRuntimeFallsBackToBase(t *testing.T) {
	f := newDefaultsFixture(t, nil) // no settings
	ctx := context.Background()

	// With the default-image hierarchy (user → org → platform → "base"),
	// an empty runtime with no settings falls back to "base".
	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "base"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
}

// === US-13.1: workspace.defaultStorageSize ===

func TestCreateWorkspace_EmptyStorageSize_UsesDefault(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageSize": "2Gi",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Storage.Size == "2Gi"
	})).Return(crdWorkspace("ws-1", "default", "user1", "2Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyStorageSize_NoSettings_FailsValidation(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	req := types.CreateWorkspaceRequest{Name: "test", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "storageSize")
}

// === US-13.2: workspace.defaultResources ===

func TestCreateWorkspace_DefaultResources_Applied(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu":    "1000m",
		"workspace.defaultResources.memory": "1Gi",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources != nil &&
			ws.Spec.Resources.CPU == "1000m" &&
			ws.Spec.Resources.Memory == "1Gi"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_NoResourceSettings_NilResources(t *testing.T) {
	f := newDefaultsFixture(t, nil) // no settings service at all
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources == nil
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
}

// === US-13.4: workspace.defaultSecurityLevel ===

func TestCreateWorkspace_DefaultSecurityLevel_Applied(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultSecurityLevel": "high",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.SecurityLevel == "high"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === US-13.5: workspace.defaultStorageClass ===

func TestCreateWorkspace_DefaultStorageClass_Applied(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageClass": "fast-ssd",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Storage.StorageClassName == "fast-ssd"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_ExplicitStorageClass_NotOverridden(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageClass": "fast-ssd",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Storage.StorageClassName == "slow-hdd"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base", StorageClass: "slow-hdd"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === US-13.6: workspace.autoSuspend + TTL ===

func TestCreateWorkspace_AutoSuspend_FromSettings(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.autoSuspend.enabled":            false,
		"workspace.autoSuspend.idleTimeoutMinutes": 30,
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.AutoSuspend != nil &&
			ws.Spec.AutoSuspend.Enabled == false &&
			ws.Spec.AutoSuspend.IdleTimeoutSeconds == 1800
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_TTLDays_ConvertedToSeconds(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.ttlDaysAfterSuspended": 7,
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.TTLSecondsAfterSuspended == 7*86400
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === US-13.7: workspace.defaultNetworkAccess ===

func TestCreateWorkspace_DefaultNetworkAccess_Applied(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultNetworkAccess.ingress":       true,
		"workspace.defaultNetworkAccess.egressDomains": []string{"api.openai.com", "api.anthropic.com"},
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess != nil &&
			ws.Spec.NetworkAccess.Ingress == true &&
			len(ws.Spec.NetworkAccess.Egress) == 2 &&
			ws.Spec.NetworkAccess.Egress[0].Domain == "api.openai.com"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === Epic 66: DevPreview field defaults + pass-through ===

func TestCreateWorkspace_DevPreview_DefaultsFalse(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess == nil || !ws.Spec.NetworkAccess.DevPreview
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// === Unhappy paths: settings service errors ===

func TestCreateWorkspace_SettingsError_GracefulDegradation(t *testing.T) {
	// Settings store that returns errors
	errStore := &errorSettingsStore{}
	var log pkginterfaces.LoggerInterface = lmocks.NewMockLogger()
	errSvc := settings.NewInstanceService(errStore, log)

	f := newFixture(t)
	f.svc.SetInstanceSettings(errSvc)
	ctx := context.Background()

	// Should still create workspace with request values (no defaults applied)
	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "base" && ws.Spec.Storage.Size == "1Gi"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
}

// errorSettingsStore always returns an error.
type errorSettingsStore struct{}

func (s *errorSettingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return nil, fmt.Errorf("database connection refused")
}
func (s *errorSettingsStore) SetInstanceSetting(_ context.Context, _ string, _ json.RawMessage) error {
	return fmt.Errorf("database connection refused")
}

// === Edge cases ===

func TestCreateWorkspace_PartialResources_OnlyCPU(t *testing.T) {
	// Only cpu is set in settings, memory uses schema default
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultResources.cpu": "2000m",
		// memory will come from schema default
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Resources != nil && ws.Spec.Resources.CPU == "2000m"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_TTLZero_NotSetOnCRD(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.ttlDaysAfterSuspended": 0,
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.TTLSecondsAfterSuspended == 0
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyEgressDomains_NoNetworkAccess(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultNetworkAccess.ingress":       false,
		"workspace.defaultNetworkAccess.egressDomains": []string{},
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess == nil
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_AutoSuspendTimeout_MinutesToSeconds(t *testing.T) {
	// Verify the minutes→seconds conversion for various values
	tests := []struct {
		name          string
		minutes       int
		expectSeconds int64
	}{
		{"1 minute", 1, 60},
		{"60 minutes", 60, 3600},
		{"1440 minutes (1 day)", 1440, 86400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDefaultsFixture(t, map[string]any{
				"workspace.autoSuspend.idleTimeoutMinutes": tt.minutes,
			})
			ctx := context.Background()

			f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
				return ws.Spec.AutoSuspend != nil &&
					ws.Spec.AutoSuspend.IdleTimeoutSeconds == tt.expectSeconds
			})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
			f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

			req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: "base"}
			_, err := f.svc.CreateWorkspace(ctx, "user1", req)
			assert.NoError(t, err)
			f.ws.AssertExpectations(t)
		})
	}
}

// === Integration: all defaults applied together ===

func TestCreateWorkspace_AllDefaults_AppliedTogether(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage":                       "custom:latest",
		"workspace.defaultStorageSize":                 "5Gi",
		"workspace.defaultStorageClass":                "premium",
		"workspace.defaultSecurityLevel":               "high",
		"workspace.defaultResources.cpu":               "2000m",
		"workspace.defaultResources.memory":            "2Gi",
		"workspace.autoSuspend.enabled":                true,
		"workspace.autoSuspend.idleTimeoutMinutes":     120,
		"workspace.ttlDaysAfterSuspended":              14,
		"workspace.defaultNetworkAccess.ingress":       true,
		"workspace.defaultNetworkAccess.egressDomains": []string{"api.openai.com"},
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "custom:latest" &&
			ws.Spec.Storage.Size == "5Gi" &&
			ws.Spec.Storage.StorageClassName == "premium" &&
			ws.Spec.SecurityLevel == "high" &&
			ws.Spec.Resources != nil &&
			ws.Spec.Resources.CPU == "2000m" &&
			ws.Spec.Resources.Memory == "2Gi" &&
			ws.Spec.AutoSuspend != nil &&
			ws.Spec.AutoSuspend.Enabled == true &&
			ws.Spec.AutoSuspend.IdleTimeoutSeconds == 7200 &&
			ws.Spec.TTLSecondsAfterSuspended == 14*86400 &&
			ws.Spec.NetworkAccess != nil &&
			ws.Spec.NetworkAccess.Ingress == true &&
			len(ws.Spec.NetworkAccess.Egress) == 1
	})).Return(crdWorkspace("ws-1", "default", "user1", "5Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	// Request with NO optional fields — all should come from settings
	req := types.CreateWorkspaceRequest{Name: "full-defaults-test"}
	result, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	assert.NotNil(t, result)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_ExplicitValues_OverrideAllDefaults(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage":        "default:latest",
		"workspace.defaultStorageSize":  "1Gi",
		"workspace.defaultStorageClass": "slow",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "python:3.11" &&
			ws.Spec.Storage.Size == "10Gi" &&
			ws.Spec.Storage.StorageClassName == "fast"
	})).Return(crdWorkspace("ws-1", "default", "user1", "10Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{
		Name:         "explicit",
		Runtime:      "python:3.11",
		StorageSize:  "10Gi",
		StorageClass: "fast",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// ===== MaxActiveSessions (Epic 13 US-13.3) =====

func TestCreateWorkspace_DefaultMaxActiveSessions_Applied(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageSize":       "10Gi",
		"workspace.defaultMaxActiveSessions": 8,
	})

	mockCRD := &v1.Workspace{}
	f.k8s.On("LlmsafespacesV1").Return(f.v1iface, nil)
	f.v1iface.On("Workspaces", mock.Anything).Return(f.ws)
	f.ws.On("Create", mock.Anything, mock.Anything).Return(mockCRD, nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)
	f.db.On("GetCredentialAutoApplyRules", mock.Anything).Return(nil, nil).Maybe()
	f.db.On("SeedWorkspaceCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "10Gi", Runtime: "python:3.10"}
	_, _ = f.svc.CreateWorkspace(context.Background(), "user-1", req)

	// The CRD passed to Create should have MaxActiveSessions = 8
	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.MaxActiveSessions == 8
	}))
}

func TestApplyWorkspaceDefaults_ExistingMaxActiveSessions_NotOverridden(t *testing.T) {
	// applyWorkspaceDefaults should not overwrite a non-zero MaxActiveSessions
	// already set on the CRD (e.g., from a future request field or controller).
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultMaxActiveSessions": 8,
	})

	crd := &v1.Workspace{}
	crd.Spec.MaxActiveSessions = 3 // pre-set

	f.svc.applyWorkspaceDefaults(context.Background(), crd)

	// The pre-set value of 3 must NOT be overwritten by the setting of 8.
	assert.Equal(t, int32(3), crd.Spec.MaxActiveSessions)
}

// ===== SecretAutoProvisioner (global-default secrets) =====

// fakeSecretProvisioner is a test double for SecretAutoProvisioner that
// records calls so tests can assert SeedGlobalDefaultSecrets was invoked.
type fakeSecretProvisioner struct {
	calls   []seedCall
	returns error
}

type seedCall struct {
	workspaceID string
	userID      string
}

func (f *fakeSecretProvisioner) SeedGlobalDefaultSecrets(_ context.Context, workspaceID, userID string) error {
	f.calls = append(f.calls, seedCall{workspaceID: workspaceID, userID: userID})
	return f.returns
}

// TestCreateWorkspace_InvokesSecretProvisioner verifies that CreateWorkspace
// calls SeedGlobalDefaultSecrets with the new workspace's ID and the owning
// user's ID when a SecretAutoProvisioner is wired.
func TestCreateWorkspace_InvokesSecretProvisioner(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageSize": "10Gi",
	})
	prov := &fakeSecretProvisioner{}
	f.svc.SetSecretAutoProvisioner(prov)

	mockCRD := crdWorkspace("ws-seed-target", "default", "user-1", "10Gi")
	f.k8s.On("LlmsafespacesV1").Return(f.v1iface, nil).Maybe()
	f.v1iface.On("Workspaces", mock.Anything).Return(f.ws)
	f.ws.On("Create", mock.Anything, mock.Anything).Return(mockCRD, nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)
	f.db.On("GetCredentialAutoApplyRules", mock.Anything).Return(nil, nil).Maybe()
	f.db.On("SeedWorkspaceCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "10Gi", Runtime: "python:3.10"}
	_, err := f.svc.CreateWorkspace(context.Background(), "user-1", req)
	assert.NoError(t, err)

	if len(prov.calls) != 1 {
		t.Fatalf("expected 1 SeedGlobalDefaultSecrets call, got %d", len(prov.calls))
	}
	if prov.calls[0].userID != "user-1" {
		t.Errorf("expected userID 'user-1', got %q", prov.calls[0].userID)
	}
	if prov.calls[0].workspaceID != "ws-seed-target" {
		t.Errorf("expected workspaceID 'ws-seed-target', got %q", prov.calls[0].workspaceID)
	}
}

// TestCreateWorkspace_SecretProvisionerFailure_BestEffort verifies that a
// SeedGlobalDefaultSecrets error does NOT roll back workspace creation —
// the workspace is still returned, and the error is only logged.
func TestCreateWorkspace_SecretProvisionerFailure_BestEffort(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageSize": "10Gi",
	})
	prov := &fakeSecretProvisioner{
		returns: fmt.Errorf("simulated provisioner outage"),
	}
	f.svc.SetSecretAutoProvisioner(prov)

	mockCRD := crdWorkspace("ws-seed-failok", "default", "user-1", "10Gi")
	f.k8s.On("LlmsafespacesV1").Return(f.v1iface, nil).Maybe()
	f.v1iface.On("Workspaces", mock.Anything).Return(f.ws)
	f.ws.On("Create", mock.Anything, mock.Anything).Return(mockCRD, nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)
	f.db.On("GetCredentialAutoApplyRules", mock.Anything).Return(nil, nil).Maybe()
	f.db.On("SeedWorkspaceCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "10Gi", Runtime: "python:3.10"}
	ws, err := f.svc.CreateWorkspace(context.Background(), "user-1", req)
	assert.NoError(t, err, "workspace creation must not fail when secret seeding fails")
	if ws == nil {
		t.Fatal("expected non-nil workspace despite provisioner failure")
	}
}

// TestCreateWorkspace_NoSecretProvisioner_NoPanic verifies that
// CreateWorkspace does not panic when no SecretAutoProvisioner is wired
// (the field is nil and the call is skipped).
func TestCreateWorkspace_NoSecretProvisioner_NoPanic(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultStorageSize": "10Gi",
	})
	// Deliberately do NOT call SetSecretAutoProvisioner.

	mockCRD := crdWorkspace("ws-no-prov", "default", "user-1", "10Gi")
	f.k8s.On("LlmsafespacesV1").Return(f.v1iface, nil).Maybe()
	f.v1iface.On("Workspaces", mock.Anything).Return(f.ws)
	f.ws.On("Create", mock.Anything, mock.Anything).Return(mockCRD, nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)
	f.db.On("GetCredentialAutoApplyRules", mock.Anything).Return(nil, nil).Maybe()
	f.db.On("SeedWorkspaceCredentials", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "10Gi", Runtime: "python:3.10"}
	ws, err := f.svc.CreateWorkspace(context.Background(), "user-1", req)
	assert.NoError(t, err)
	if ws == nil {
		t.Fatal("expected non-nil workspace")
	}
}
