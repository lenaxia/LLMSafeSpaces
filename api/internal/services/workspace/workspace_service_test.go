// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fixture wires up all centralized mocks and a real Service under test.
type fixture struct {
	svc     *Service
	k8s     *kmocks.MockKubernetesClient
	v1iface *kmocks.MockLLMSafespacesV1Interface
	ws      *kmocks.MockWorkspaceInterface
	db      *imocks.MockDatabaseService
	cache   *imocks.MockCacheService
	metrics *imocks.MockMetricsService
	log     *lmocks.MockLogger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()
	log.On("Sync").Return(nil).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	ws := kmocks.NewMockWorkspaceInterface()
	db := &imocks.MockDatabaseService{}
	cache := &imocks.MockCacheService{}
	met := &imocks.MockMetricsService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(ws)

	svc, err := New(log, k8s, db, cache, met, &Config{Namespace: "default"})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return &fixture{svc: svc, k8s: k8s, v1iface: v1i, ws: ws, db: db, cache: cache, metrics: met, log: log}
}

// fixtureWithFakeClientset extends fixture with an in-memory K8s
// Clientset so tests can observe Secret operations without standing up
// a real apiserver.
type fixtureWithFakeClientset struct {
	*fixture
	fakeCS *k8sfake.Clientset
}

func newFixtureWithFakeClientset(t *testing.T) *fixtureWithFakeClientset {
	t.Helper()
	f := newFixture(t)
	cs := k8sfake.NewSimpleClientset()
	f.k8s.On("Clientset").Return(k8s.Interface(cs))
	return &fixtureWithFakeClientset{fixture: f, fakeCS: cs}
}

func crdWorkspace(name, ns, userID, storageSize string) *v1.Workspace {
	return &v1.Workspace{
		TypeMeta:   metav1.TypeMeta{Kind: "Workspace", APIVersion: "llmsafespaces.dev/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: userID},
			Storage: v1.WorkspaceStorageConfig{Size: storageSize},
		},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhasePending},
	}
}

func dbWorkspace(id, userID, name, storageSize string) *types.WorkspaceMetadata {
	return &types.WorkspaceMetadata{
		ID:          id,
		UserID:      userID,
		Name:        name,
		Runtime:     "python:3.10",
		StorageSize: storageSize,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
}

// ===== New() =====

func TestNew_NilLogger_ReturnsError(t *testing.T) {
	_, err := New(nil, kmocks.NewMockKubernetesClient(), &imocks.MockDatabaseService{}, nil, &imocks.MockMetricsService{}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "logger")
}

func TestNew_NilK8s_ReturnsError(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("With", mock.Anything).Return(log).Maybe()
	_, err := New(log, nil, &imocks.MockDatabaseService{}, nil, &imocks.MockMetricsService{}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes")
}

func TestNew_NilDB_ReturnsError(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("With", mock.Anything).Return(log).Maybe()
	_, err := New(log, kmocks.NewMockKubernetesClient(), nil, nil, &imocks.MockMetricsService{}, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "database")
}

func TestNew_NilConfig_UsesDefaults(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("With", mock.Anything).Return(log).Maybe()
	svc, err := New(log, kmocks.NewMockKubernetesClient(), &imocks.MockDatabaseService{}, nil, &imocks.MockMetricsService{}, nil)
	assert.NoError(t, err)
	assert.NotNil(t, svc)
}

// ===== RestartWorkspace (Epic 21 Change A) =====
//
// RestartWorkspace bumps spec.restartGeneration and writes the spec via
// Update (not UpdateStatus, which the controller uses to flip phase).
// The controller's handleFailed and handleActive both observe the bump
// and walk back to Pending (or delete the running pod for Active).

func TestRestartWorkspace_FromFailed_BumpsRestartGeneration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	failedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	failedCrd.Status.Phase = v1.WorkspacePhaseFailed
	failedCrd.Spec.RestartGeneration = 2
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(failedCrd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.RestartGeneration == 3
	})).Return(failedCrd, nil)

	err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
	// MUST NOT touch status — that's the controller's job after the spec bump.
	f.ws.AssertNotCalled(t, "UpdateStatus")
}

func TestRestartWorkspace_FromActive_BumpsRestartGeneration(t *testing.T) {
	// Restart from any non-terminal phase is allowed; this lets users
	// recover from "stuck" Active workspaces (where the agent is hung
	// but the controller hasn't given up yet) without waiting for the
	// transient-failure budget to exhaust.
	f := newFixture(t)
	ctx := context.Background()

	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Spec.RestartGeneration = 0
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.RestartGeneration == 1
	})).Return(activeCrd, nil)

	err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestRestartWorkspace_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	f.ws.AssertNotCalled(t, "Update")
}

func TestRestartWorkspace_FromTerminating_Rejected(t *testing.T) {
	// Terminating/Terminated are genuinely terminal; restarting them
	// would race with finalizer logic. Reject explicitly with conflict.
	for _, phase := range []v1.WorkspacePhase{v1.WorkspacePhaseTerminating, v1.WorkspacePhaseTerminated} {
		t.Run(string(phase), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
			crd.Status.Phase = phase
			f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

			err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
			assert.Error(t, err)
			f.ws.AssertNotCalled(t, "Update")
		})
	}
}

func TestRestartWorkspace_K8sGetFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), errors.New("boom"))

	err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
	assert.Error(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestRestartWorkspace_K8sUpdateFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	failedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	failedCrd.Status.Phase = v1.WorkspacePhaseFailed
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(failedCrd, nil)
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return((*v1.Workspace)(nil), errors.New("etcd unavailable"))

	err := f.svc.RestartWorkspace(ctx, "user1", "ws-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_restart_failed")
}

// ===== CreateWorkspace =====

func TestCreateWorkspace_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(
		crdWorkspace("ws-1", "default", "user1", "10Gi"), nil,
	)
	f.db.On("CreateWorkspace", ctx, mock.MatchedBy(func(m *types.WorkspaceMetadata) bool {
		return m.UserID == "user1" && m.StorageSize == "10Gi"
	})).Return(nil)

	req := types.CreateWorkspaceRequest{
		Name:        "my-workspace",
		Runtime:     "python:3.10",
		StorageSize: "10Gi",
	}
	result, err := f.svc.CreateWorkspace(ctx, "user1", req)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "user1", result.UserID)
	assert.Equal(t, "10Gi", result.StorageSize)
	f.ws.AssertExpectations(t)
	f.db.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyName_FailsValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	req := types.CreateWorkspaceRequest{StorageSize: "10Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation_error")
	f.ws.AssertNotCalled(t, "Create")
}

func TestCreateWorkspace_EmptyStorageSize_FailsValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	req := types.CreateWorkspaceRequest{Name: "my-workspace"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "validation_error")
	f.ws.AssertNotCalled(t, "Create")
}

func TestCreateWorkspace_K8sCreateFails_ReturnsInternal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return((*v1.Workspace)(nil), errors.New("k8s unavailable"))

	req := types.CreateWorkspaceRequest{Name: "my-workspace", StorageSize: "10Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_creation_failed")
	f.db.AssertNotCalled(t, "CreateWorkspace")
}

func TestCreateWorkspace_DBCreateFails_CleansUpK8s(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-x", "default", "user1", "10Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(errors.New("db write failed"))
	f.ws.On("Delete", mock.Anything, "ws-x", mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "my-workspace", StorageSize: "10Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metadata_creation_failed")
	f.ws.AssertCalled(t, "Delete", mock.Anything, "ws-x", mock.Anything)
}

// ===== GetWorkspace =====

func TestGetWorkspace_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crdWorkspace("ws-1", "default", "user1", "10Gi"), nil)

	result, err := f.svc.GetWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	assert.Equal(t, "ws-1", result.ID)
	assert.Equal(t, "user1", result.UserID)
}

func TestGetWorkspace_NotFound_ReturnsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "missing").Return((*types.WorkspaceMetadata)(nil), nil)

	_, err := f.svc.GetWorkspace(ctx, "user1", "missing")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not_found")
}

func TestGetWorkspace_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	_, err := f.svc.GetWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

func TestGetWorkspace_DBError_ReturnsInternal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return((*types.WorkspaceMetadata)(nil), errors.New("db down"))

	_, err := f.svc.GetWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_retrieval_failed")
}

// ===== ListWorkspaces =====
//
// Phase is owned by the Workspace CRD; the DB stores immutable metadata only.
// ListWorkspaces issues one label-scoped CRD list per call and joins phase by
// name. These tests pin that contract.

func TestListWorkspaces_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()

	metas := []*types.WorkspaceMetadata{
		{ID: "ws-1", UserID: "user1", Name: "ws1", StorageSize: "10Gi", CreatedAt: now},
		{ID: "ws-2", UserID: "user1", Name: "ws2", StorageSize: "5Gi", CreatedAt: now.Add(-time.Hour)},
	}
	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return(metas, &types.PaginationMetadata{Total: 2, Limit: 10}, nil)

	crdList := &v1.WorkspaceList{Items: []v1.Workspace{
		{ObjectMeta: metav1.ObjectMeta{Name: "ws-1"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ws-2"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseSuspended}},
	}}
	f.ws.On("List", mock.Anything, metav1.ListOptions{LabelSelector: "user-id=user1"}).Return(crdList, nil)

	result, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})

	assert.NoError(t, err)
	assert.Len(t, result.Items, 2)
	assert.Equal(t, "ws-1", result.Items[0].ID)
	assert.Equal(t, "Active", result.Items[0].Phase, "phase comes from the CRD")
	assert.Equal(t, "Suspended", result.Items[1].Phase, "phase comes from the CRD")
	assert.Equal(t, 2, result.Pagination.Total)
}

// TestListWorkspaces_PropagatesOrgID is a regression test for
// LLMSafeSpaces#477. WorkspaceMetadata carries OrgID (Epic 11; nullable —
// personal workspaces have no org). The conversion to WorkspaceListItem
// originally dropped this field, so the frontend's Workspace Settings
// drawer never knew which workspaces were org-scoped and could not render
// the prompt-customization Lock UI when the org admin had disabled
// allow_user_prompt. The backend correctly returned 403 on the eventual
// PUT /prompt, but the user only saw a generic "Save failed" — the lock
// state was invisible to them. See #477.
//
// This test asserts the list response carries OrgID for an org-scoped
// workspace (and stays nil for a personal one).
func TestListWorkspaces_PropagatesOrgID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()

	orgID := "org-acme"
	metas := []*types.WorkspaceMetadata{
		// Org-scoped workspace — the frontend needs OrgID here so the
		// Workspace Settings drawer can fetch the org's allow_user_prompt
		// policy and gate the Custom Instructions textarea on it.
		{ID: "ws-org", UserID: "user1", Name: "org-ws", StorageSize: "10Gi", CreatedAt: now, OrgID: &orgID},
		// Personal workspace — OrgID is nil; the lock check correctly
		// short-circuits.
		{ID: "ws-personal", UserID: "user1", Name: "personal", StorageSize: "5Gi", CreatedAt: now},
	}
	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return(metas, &types.PaginationMetadata{Total: 2, Limit: 10}, nil)

	crdList := &v1.WorkspaceList{Items: []v1.Workspace{
		{ObjectMeta: metav1.ObjectMeta{Name: "ws-org"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
		{ObjectMeta: metav1.ObjectMeta{Name: "ws-personal"}, Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive}},
	}}
	f.ws.On("List", mock.Anything, metav1.ListOptions{LabelSelector: "user-id=user1"}).Return(crdList, nil)

	result, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})
	assert.NoError(t, err)
	assert.Len(t, result.Items, 2)

	// The org-scoped workspace MUST carry its OrgID in the list response.
	// Without this, the frontend (which only has the list response — it
	// does not separately fetch each workspace) cannot determine the org
	// and therefore cannot render the prompt-customization Lock UI.
	require.NotNil(t, result.Items[0].OrgID,
		"WorkspaceListItem.OrgID must be populated for org-scoped workspaces "+
			"(LLMSafeSpaces#477 — without this the prompt-customization Lock UI "+
			"in WorkspaceSettingsDrawer.tsx never renders, members can type "+
			"a custom prompt that the backend will then 403 on PUT /prompt)")
	assert.Equal(t, "org-acme", *result.Items[0].OrgID,
		"OrgID must match the source WorkspaceMetadata's OrgID")

	// Personal workspaces must keep OrgID nil so the frontend's
	// `if (workspace.orgId)` check correctly skips the org policy fetch.
	assert.Nil(t, result.Items[1].OrgID,
		"personal workspaces must have nil OrgID in the list response")
}

func TestListWorkspaces_Empty_ReturnsEmptyList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return([]*types.WorkspaceMetadata{}, &types.PaginationMetadata{Total: 0, Limit: 10}, nil)
	f.ws.On("List", mock.Anything, metav1.ListOptions{LabelSelector: "user-id=user1"}).Return(&v1.WorkspaceList{}, nil)

	result, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})

	assert.NoError(t, err)
	assert.Empty(t, result.Items)
}

func TestListWorkspaces_DBFails_ReturnsInternal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return(
		([]*types.WorkspaceMetadata)(nil), (*types.PaginationMetadata)(nil), errors.New("db down"),
	)

	_, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_list_failed")
}

// When the kube-apiserver is unavailable we cannot determine phase. The list
// endpoint must NOT fail the request — it returns the items with empty phase
// so the rest of the dashboard still loads. The platform is unusable in this
// state regardless (every other endpoint also needs k8s) but failing the list
// page would compound the outage.
func TestListWorkspaces_K8sListFails_ReturnsItemsWithEmptyPhase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()

	metas := []*types.WorkspaceMetadata{
		{ID: "ws-1", UserID: "user1", Name: "ws1", StorageSize: "10Gi", CreatedAt: now},
	}
	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return(metas, &types.PaginationMetadata{Total: 1, Limit: 10}, nil)
	f.ws.On("List", mock.Anything, metav1.ListOptions{LabelSelector: "user-id=user1"}).
		Return((*v1.WorkspaceList)(nil), errors.New("apiserver down"))

	result, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})

	assert.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, "", result.Items[0].Phase, "no CRD => no phase")
}

// A DB row that has no matching CRD (e.g. mid-deletion) is still returned with
// empty phase rather than dropped from the response.
func TestListWorkspaces_CRDMissing_PhaseEmpty(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()

	metas := []*types.WorkspaceMetadata{
		{ID: "ws-1", UserID: "user1", Name: "ws1", StorageSize: "10Gi", CreatedAt: now},
	}
	f.db.On("ListWorkspaces", ctx, "user1", 10, 0).Return(metas, &types.PaginationMetadata{Total: 1, Limit: 10}, nil)
	f.ws.On("List", mock.Anything, metav1.ListOptions{LabelSelector: "user-id=user1"}).Return(&v1.WorkspaceList{}, nil)

	result, err := f.svc.ListWorkspaces(ctx, "user1", types.ListOptions{Limit: 10, Offset: 0})

	assert.NoError(t, err)
	assert.Len(t, result.Items, 1)
	assert.Equal(t, "", result.Items[0].Phase)
}

// ===== DeleteWorkspace =====

func TestDeleteWorkspace_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Delete", mock.Anything, "ws-1", mock.Anything).Return(nil)
	done := make(chan struct{})
	f.db.On("MarkWorkspaceDeleted", ctx, "ws-1").Run(func(_ mock.Arguments) { close(done) })

	err := f.svc.DeleteWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MarkWorkspaceDeleted")
	}
	f.db.AssertExpectations(t)
}

func TestDeleteWorkspace_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	err := f.svc.DeleteWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
	f.ws.AssertNotCalled(t, "Delete")
}

func TestDeleteWorkspace_NotFound_ReturnsNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return((*types.WorkspaceMetadata)(nil), nil)

	err := f.svc.DeleteWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not_found")
}

// ===== SuspendWorkspace =====

func TestSuspendWorkspace_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	// US-23.3: Suspend writes Spec.Suspend=true via Update, not Status.Phase via UpdateStatus.
	var captured *v1.Workspace
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Run(func(args mock.Arguments) { captured = args.Get(1).(*v1.Workspace) }).
		Return(activeCrd, nil)

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
	require.NotNil(t, captured)
	require.NotNil(t, captured.Spec.Suspend, "Spec.Suspend must be set (non-nil pointer)")
	assert.True(t, *captured.Spec.Suspend, "Spec.Suspend must be set to true")
}

func TestSuspendWorkspace_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

// ===== ActivateWorkspace (phase transition + credential injection) =====

func TestActivateWorkspace_HappyPath_TransitionsToResuming(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	suspendTrue := true
	suspendedCrd.Spec.Suspend = &suspendTrue
	staleActivity := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	suspendedCrd.Status.LastActivityAt = &staleActivity

	var captured *v1.Workspace
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)
	// US-23.3: Activate writes Spec.Suspend=false + annotation via Update.
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Run(func(args mock.Arguments) { captured = args.Get(1).(*v1.Workspace) }).
		Return(suspendedCrd, nil)
	// enforceMaxActiveWorkspaces calls List
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)

	resp, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "ws-1", resp.Resumed)
	require.NotNil(t, captured, "Update must be called")
	require.NotNil(t, captured.Spec.Suspend, "Spec.Suspend must be set (non-nil pointer)")
	assert.False(t, *captured.Spec.Suspend, "Spec.Suspend must be cleared to false")
	require.NotNil(t, captured.Annotations, "Annotations must be set")
	annot, ok := captured.Annotations[v1.AnnotationLastActivityAt]
	require.True(t, ok, "LastActivityAt annotation must be set")
	parsed, err := time.Parse(time.RFC3339, annot)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), parsed, 5*time.Second,
		"annotation must be a recent time, was %v", parsed)
}

func TestActivateWorkspace_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

func TestActivateWorkspace_K8sGetFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), errors.New("k8s unavailable"))

	_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_get_failed")
	f.ws.AssertNotCalled(t, "Update")
}

func TestActivateWorkspace_K8sUpdateFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Return((*v1.Workspace)(nil), errors.New("k8s update failed"))

	_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_resume_failed")
}

// TestActivateWorkspace_SpecSuspendPruned reproduces the failure mode from
// worklog 0465 (2026-06-19) where a deployed CRD missing spec.suspend
// caused the apiserver to silently prune the field on Update. The Update
// returned 200, the API logged "Workspace activated", but the persisted
// object had Spec.Suspend=nil so the controller never observed a transition
// and the workspace stayed Suspended forever — frontend showed nothing.
//
// The post-write read-back assertion reads the object the apiserver
// returned (which reflects post-pruning storage state) and rejects the
// activate when Spec.Suspend was not actually persisted as &false.
func TestActivateWorkspace_SpecSuspendPruned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)

	// Simulate apiserver field pruning: Update succeeds but the returned
	// object has Spec.Suspend=nil because the deployed CRD schema lacks
	// `spec.suspend`.
	prunedReturn := suspendedCrd.DeepCopy()
	prunedReturn.Spec.Suspend = nil
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Return(prunedReturn, nil)

	_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	require.Error(t, err, "must reject activate when spec.suspend was pruned")
	assert.Contains(t, err.Error(), "workspace_resume_failed",
		"must use the resume_failed error code so callers see a concrete failure, not a phantom 200")
	assert.Contains(t, err.Error(), "spec.suspend",
		"error must name the pruned field so operators can correlate to CRD schema drift")
}

// TestActivateWorkspace_SpecSuspendPersistedAsTrue defends against an
// apiserver writing back &true (e.g. because the CRD has a default of true,
// which would be an admin misconfiguration). Field is present but the
// wrong value still means the controller will not resume.
func TestActivateWorkspace_SpecSuspendPersistedAsTrue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)

	wrongReturn := suspendedCrd.DeepCopy()
	suspendTrue := true
	wrongReturn.Spec.Suspend = &suspendTrue
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).
		Return(wrongReturn, nil)

	_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_resume_failed")
	assert.Contains(t, err.Error(), "spec.suspend")
}

// ===== GetWorkspaceStatus =====

func TestGetWorkspaceStatus_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.PVCName = "workspace-ws-1"
	activeCrd.Status.ActiveSessions = 2
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	assert.Equal(t, "Active", result.Phase)
	assert.Equal(t, "workspace-ws-1", result.PVCName)
	assert.Equal(t, 2, result.ActiveSessions)
}

func TestGetWorkspaceStatus_IncludesSessionContextUsed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.Sessions = []v1.AgentSessionStatus{
		{ID: "ses_1", Title: "main", Status: "idle", ContextUsed: 42000},
		{ID: "ses_2", Title: "other", Status: "busy", ContextUsed: 99000},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	require.Len(t, result.Sessions, 2)
	assert.Equal(t, int64(42000), result.Sessions[0].ContextUsed, "ses_1 ContextUsed threaded to API response")
	assert.Equal(t, int64(99000), result.Sessions[1].ContextUsed, "ses_2 ContextUsed threaded to API response")
}

func TestGetWorkspaceStatus_IncludesContextTotal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.ContextUsed = 0
	activeCrd.Status.ContextTotal = 200000
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	assert.Equal(t, int64(0), result.ContextUsed, "ContextUsed threaded to API response")
	assert.Equal(t, int64(200000), result.ContextTotal, "ContextTotal threaded to API response")
}

func TestGetWorkspaceStatus_ContextTotal_ZeroNotDropped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.ContextUsed = 0
	activeCrd.Status.ContextTotal = 0
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.NoError(t, err)

	raw, jsonErr := json.Marshal(result)
	require.NoError(t, jsonErr)
	assert.Contains(t, string(raw), `"contextUsed":0`, "omitempty removed — zero contextUsed must appear in JSON")
	assert.Contains(t, string(raw), `"contextTotal":0`, "omitempty removed — zero contextTotal must appear in JSON")
}

func TestGetWorkspaceStatus_WrongOwner_ReturnsForbidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "other-user", "my-ws", "10Gi"), nil)

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

// ===== SetCredentials =====

func TestStart_Stop_NoError(t *testing.T) {
	f := newFixture(t)
	assert.NoError(t, f.svc.Start())
	assert.NoError(t, f.svc.Stop())
}

// ===== SuspendWorkspace unhappy paths =====

func TestSuspendWorkspace_K8sGetFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), errors.New("k8s unavailable"))

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_get_failed")
	f.ws.AssertNotCalled(t, "Update")
}

func TestSuspendWorkspace_K8sUpdateFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return((*v1.Workspace)(nil), errors.New("k8s update failed"))

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_suspend_failed")
}

// ===== GetWorkspaceStatus unhappy paths =====

func TestGetWorkspaceStatus_K8sGetFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), errors.New("k8s unavailable"))

	_, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_get_failed")
}

// ===========================================================================
// E2E tests: Suspend/Resume phase validation (GAP-7 fix verification)
// ===========================================================================

func TestE2E_SuspendWorkspace_OnlyActiveAllowed(t *testing.T) {
	tests := []struct {
		name    string
		phase   v1.WorkspacePhase
		wantErr bool
	}{
		{"Active_allowed", v1.WorkspacePhaseActive, false},
		{"Resuming_rejected", v1.WorkspacePhaseResuming, true},
		{"Pending_rejected", v1.WorkspacePhasePending, true},
		{"Terminating_rejected", v1.WorkspacePhaseTerminating, true},
		{"Terminated_rejected", v1.WorkspacePhaseTerminated, true},
		{"Failed_rejected", v1.WorkspacePhaseFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)

			crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
			crd.Status.Phase = tt.phase
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
			if !tt.wantErr {
				f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(crd, nil)
			}

			err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestE2E_ActivateWorkspace_OnlySuspendedOrActiveAllowed(t *testing.T) {
	tests := []struct {
		name    string
		phase   v1.WorkspacePhase
		wantErr bool
	}{
		{"Suspended_allowed", v1.WorkspacePhaseSuspended, false},
		{"Active_idempotent", v1.WorkspacePhaseActive, false},
		{"Resuming_idempotent", v1.WorkspacePhaseResuming, false},
		{"Creating_idempotent", v1.WorkspacePhaseCreating, false},
		{"Pending_rejected", v1.WorkspacePhasePending, true},
		{"Terminating_rejected", v1.WorkspacePhaseTerminating, true},
		{"Terminated_rejected", v1.WorkspacePhaseTerminated, true},
		{"Failed_rejected", v1.WorkspacePhaseFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
			f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{}, nil)

			crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
			crd.Status.Phase = tt.phase
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
			if tt.phase == v1.WorkspacePhaseSuspended {
				f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(crd, nil)
			}

			_, err := f.svc.ActivateWorkspace(ctx, "user1", "ws-1")

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestSuspendWorkspace_Idempotent_AlreadySuspended(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendedCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendedCrd, nil)

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestSuspendWorkspace_Idempotent_AlreadySuspending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	suspendingCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	suspendingCrd.Status.Phase = v1.WorkspacePhaseSuspending
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(suspendingCrd, nil)

	err := f.svc.SuspendWorkspace(ctx, "user1", "ws-1")

	assert.NoError(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

// NeutralizeUserWorkspaces is the password-reset workspace cleanup: it
// suspends Active pods (so live in-memory/tmpfs keys die with the pod)
// and scrubs the workspace-secrets-* K8s Secret for every workspace so a
// later resume cannot re-materialize stale plaintext.
func TestNeutralizeUserWorkspaces_SuspendsActiveAndScrubsAllSecrets(t *testing.T) {
	f := newFixtureWithFakeClientset(t)
	ctx := context.Background()

	// Two workspaces: one Active (must be suspended), one already
	// Suspended (suspend is a no-op). Both must be scrubbed regardless
	// of phase — the stale Secret on a Suspended workspace is exactly
	// what would be re-cp'd into /sandbox-cfg on resume.
	activeCrd := crdWorkspace("ws-active", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	suspendedCrd := crdWorkspace("ws-sus", "default", "user1", "10Gi")
	suspendedCrd.Status.Phase = v1.WorkspacePhaseSuspended

	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{*activeCrd, *suspendedCrd},
	}, nil)
	f.db.On("GetWorkspace", ctx, "ws-active").Return(dbWorkspace("ws-active", "user1", "active", "10Gi"), nil)
	f.db.On("GetWorkspace", ctx, "ws-sus").Return(dbWorkspace("ws-sus", "user1", "suspended", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-active", mock.Anything).Return(activeCrd, nil)
	f.ws.On("Get", mock.Anything, "ws-sus", mock.Anything).Return(suspendedCrd, nil)

	suspended := false
	f.ws.On("Update", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Run(func(args mock.Arguments) {
		w := args.Get(1).(*v1.Workspace)
		if w.Name == "ws-active" && w.Spec.Suspend != nil && *w.Spec.Suspend {
			suspended = true
		}
	}).Return(activeCrd, nil)

	secrets := f.fakeCS.CoreV1().Secrets("default")
	for _, name := range []string{"ws-active", "ws-sus"} {
		_, err := secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "workspace-secrets-" + name},
			Data:       map[string][]byte{"secrets.json": []byte("stale-plaintext")},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	err := f.svc.NeutralizeUserWorkspaces(ctx, "user1")
	require.NoError(t, err)

	for _, name := range []string{"ws-active", "ws-sus"} {
		_, gErr := secrets.Get(ctx, "workspace-secrets-"+name, metav1.GetOptions{})
		assert.True(t, k8serrors.IsNotFound(gErr),
			"workspace-secrets-%s must be scrubbed regardless of phase", name)
	}
	assert.True(t, suspended, "the Active workspace must be suspended (Spec.Suspend=true)")
}

func TestNeutralizeUserWorkspaces_NoWorkspaces_Noop(t *testing.T) {
	f := newFixtureWithFakeClientset(t)
	ctx := context.Background()

	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{Items: []v1.Workspace{}}, nil)

	err := f.svc.NeutralizeUserWorkspaces(ctx, "user1")
	require.NoError(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestNeutralizeUserWorkspaces_NonActiveConflictIsNotNoisy(t *testing.T) {
	// A workspace in Creating/Resuming returns a conflict from
	// SuspendWorkspace; neutralize must treat it as expected (not error,
	// not abort) and still scrub its Secret.
	f := newFixtureWithFakeClientset(t)
	ctx := context.Background()

	resumingCrd := crdWorkspace("ws-res", "default", "user1", "10Gi")
	resumingCrd.Status.Phase = v1.WorkspacePhaseResuming
	f.ws.On("List", mock.Anything, mock.Anything).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{*resumingCrd},
	}, nil)
	f.db.On("GetWorkspace", ctx, "ws-res").Return(dbWorkspace("ws-res", "user1", "resuming", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-res", mock.Anything).Return(resumingCrd, nil)

	_, err := f.fakeCS.CoreV1().Secrets("default").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-secrets-ws-res"},
		Data:       map[string][]byte{"secrets.json": []byte("stale")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	err = f.svc.NeutralizeUserWorkspaces(ctx, "user1")
	require.NoError(t, err, "conflict on a non-Active workspace must not fail neutralize")

	_, gErr := f.fakeCS.CoreV1().Secrets("default").Get(ctx, "workspace-secrets-ws-res", metav1.GetOptions{})
	assert.True(t, k8serrors.IsNotFound(gErr), "Secret must still be scrubbed for non-Active workspaces")
}

func TestE2E_CreateWorkspace_SetsOwnerAndStorageInCRD(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(
		crdWorkspace("ws-new", "default", "user1", "10Gi"), nil,
	)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{
		Name:         "e2e-workspace",
		Runtime:      "python:3.11",
		StorageSize:  "10Gi",
		StorageClass: "fast-ssd",
	}

	result, err := f.svc.CreateWorkspace(ctx, "user1", req)
	require.NoError(t, err)
	assert.Equal(t, "user1", result.UserID)
	assert.Equal(t, "10Gi", result.StorageSize)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Owner.UserID == "user1" &&
			crd.Spec.Storage.Size == "10Gi" &&
			crd.Spec.Storage.StorageClassName == "fast-ssd" &&
			crd.Spec.Runtime == "python:3.11"
	}))
}

func TestCredStateFromConditions(t *testing.T) {
	tests := []struct {
		name       string
		conditions []v1.WorkspaceCondition
		expected   types.CredentialStateResult
	}{
		{"valid", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionCredentialsAvailable, Status: "True", Reason: v1.ReasonCredentialsValid}}, types.CredentialStateResult{Available: true, Reason: v1.ReasonCredentialsValid}},
		{"not found", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionCredentialsAvailable, Status: "False", Reason: v1.ReasonCredentialSecretNotFound, Message: "No secret"}}, types.CredentialStateResult{Available: false, Reason: v1.ReasonCredentialSecretNotFound, Message: "No secret"}},
		{"empty", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionCredentialsAvailable, Status: "False", Reason: v1.ReasonCredentialEmpty}}, types.CredentialStateResult{Available: false, Reason: v1.ReasonCredentialEmpty}},
		{"no condition", nil, types.CredentialStateResult{Available: false, Reason: "NotChecked"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, credStateFromConditions(tt.conditions))
		})
	}
}

func TestAgentHealthFromConditions(t *testing.T) {
	past := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	tests := []struct {
		name       string
		conditions []v1.WorkspaceCondition
		lastCheck  *metav1.Time
		expected   types.AgentHealthResult
	}{
		{"healthy", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionAgentHealthy, Status: "True", Reason: v1.ReasonAgentHealthy, Message: "connected=[opencode] configured=2 sessions=2 version=1.2.27"}}, &past, types.AgentHealthResult{Status: "Healthy", Message: "connected=[opencode] configured=2 sessions=2 version=1.2.27", Connected: []string{"opencode"}, ProvidersConfigured: 2, AgentVersion: "1.2.27", LastCheckedAt: past.Format(time.RFC3339)}},
		{"healthy without configured (legacy controller)", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionAgentHealthy, Status: "True", Reason: v1.ReasonAgentHealthy, Message: "connected=[opencode] sessions=2 version=1.2.27"}}, &past, types.AgentHealthResult{Status: "Healthy", Message: "connected=[opencode] sessions=2 version=1.2.27", Connected: []string{"opencode"}, AgentVersion: "1.2.27", LastCheckedAt: past.Format(time.RFC3339)}},
		{"degraded", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionAgentHealthy, Status: "False", Reason: v1.ReasonAgentDegraded, Message: "no providers connected (configured=1, connected=[])"}}, nil, types.AgentHealthResult{Status: "Degraded", Message: "no providers connected (configured=1, connected=[])", ProvidersConfigured: 1}},
		{"unhealthy", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionAgentHealthy, Status: "False", Reason: v1.ReasonAgentUnhealthy, Message: "agent dead"}}, nil, types.AgentHealthResult{Status: "Unhealthy", Message: "agent dead"}},
		{"check failed", []v1.WorkspaceCondition{{Type: v1.WorkspaceConditionAgentHealthy, Status: "Unknown", Reason: v1.ReasonHealthCheckFailed, Message: "refused"}}, nil, types.AgentHealthResult{Status: "Unknown", Message: "refused"}},
		{"no condition", nil, nil, types.AgentHealthResult{Status: "Unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, agentHealthFromConditions(tt.conditions, tt.lastCheck))
		})
	}
}

// ===== LlmsafespacesV1() error path in callers =====
//
// LlmsafespacesV1() on the k8s client can return an error (e.g. the typed REST
// client failed to construct from the rest.Config — the exact bug fixed in
// US-38.11). Every caller of workspaceCRDClient() must surface that as a clean
// internal error and never panic. These tests pin that contract so a future
// refactor that swallows the error (e.g. by ignoring it and dereferencing a nil
// client) is caught.

// newSvcWithLlmsafespacesV1Error builds a Service whose k8s client returns the
// given error from LlmsafespacesV1() on every call. Used to exercise caller
// error paths without the happy-path stubs wired up by newFixture.
func newSvcWithLlmsafespacesV1Error(t *testing.T, v1Err error) (*Service, *kmocks.MockKubernetesClient) {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()
	log.On("Sync").Return(nil).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	// Every call returns the error; the nil interface is intentional so the
	// mock returns (nil, v1Err) — mirroring the real Client.LlmsafespacesV1().
	k8s.On("LlmsafespacesV1").Return(nil, v1Err)

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc, err := New(log, k8s, &imocks.MockDatabaseService{}, &imocks.MockCacheService{}, met, &Config{Namespace: "default"})
	require.NoError(t, err)
	return svc, k8s
}

func TestCreateWorkspace_LlmsafespacesV1Error_ReturnsInternal(t *testing.T) {
	svc, k8s := newSvcWithLlmsafespacesV1Error(t, errors.New("no kind Workspace registered"))

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "10Gi"}
	_, err := svc.CreateWorkspace(context.Background(), "user1", req)

	require.Error(t, err, "LlmsafespacesV1 failure must surface as an error, not a panic")
	assert.Contains(t, err.Error(), "workspace_creation_failed",
		"error must be wrapped with the workspace_creation_failed code")
	assert.Contains(t, err.Error(), "LLMSafespacesV1",
		"underlying LlmsafespacesV1 error must be preserved for diagnosis")
	// The workspace interface must never have been reached.
	k8s.AssertNumberOfCalls(t, "LlmsafespacesV1", 1)
}

func TestGetWorkspaceStatus_LlmsafespacesV1Error_ReturnsInternal(t *testing.T) {
	// GetWorkspaceStatus verifies ownership (DB) first, then constructs the
	// CRD client. An LlmsafespacesV1 error after a successful owner check must
	// surface as workspace_get_failed rather than nil-deref the workspace client.
	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()
	log.On("Sync").Return(nil).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	k8s.On("LlmsafespacesV1").Return(nil, errors.New("rest client construction failed"))

	db := &imocks.MockDatabaseService{}
	db.On("GetWorkspace", mock.Anything, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc, err := New(log, k8s, db, &imocks.MockCacheService{}, met, &Config{Namespace: "default"})
	require.NoError(t, err)

	_, err = svc.GetWorkspaceStatus(context.Background(), "user1", "ws-1")

	require.Error(t, err, "LlmsafespacesV1 failure must surface as an error, not a panic")
	assert.Contains(t, err.Error(), "workspace_get_failed")
	assert.Contains(t, err.Error(), "LLMSafespacesV1")
}

func TestDeleteWorkspace_LlmsafespacesV1Error_ReturnsInternal(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()
	log.On("Sync").Return(nil).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	k8s.On("LlmsafespacesV1").Return(nil, errors.New("scheme missing"))

	db := &imocks.MockDatabaseService{}
	db.On("GetWorkspace", mock.Anything, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc, err := New(log, k8s, db, &imocks.MockCacheService{}, met, &Config{Namespace: "default"})
	require.NoError(t, err)

	err = svc.DeleteWorkspace(context.Background(), "user1", "ws-1")

	require.Error(t, err, "LlmsafespacesV1 failure must surface as an error, not a panic")
	assert.Contains(t, err.Error(), "workspace_deletion_failed")
	assert.Contains(t, err.Error(), "LLMSafespacesV1")
}
