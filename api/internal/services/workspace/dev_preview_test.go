// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestSetDevPreview_EnablesFlag(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess != nil && ws.Spec.NetworkAccess.DevPreview == true
	})).Return(crd, nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

// fakeDevPreviewPusher records the live pushes (#1617).
type fakeDevPreviewPusher struct {
	calls []devPreviewPush
	err   error
}

type devPreviewPush struct {
	userID      string
	workspaceID string
	enabled     bool
}

func (p *fakeDevPreviewPusher) PushDevPreviewState(ctx context.Context, userID, workspaceID string, enabled bool) error {
	p.calls = append(p.calls, devPreviewPush{userID: userID, workspaceID: workspaceID, enabled: enabled})
	return p.err
}

func TestSetDevPreview_PushesLiveStateToRunningPod(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	pusher := &fakeDevPreviewPusher{}
	f.svc.SetDevPreviewPusher(pusher)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess != nil && ws.Spec.NetworkAccess.DevPreview == true
	})).Return(crd, nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.NoError(t, err)
	require.Len(t, pusher.calls, 1, "a successful toggle must push the new state to the running pod")
	assert.Equal(t, devPreviewPush{userID: "user1", workspaceID: "ws-1", enabled: true}, pusher.calls[0])
}

func TestSetDevPreview_PushFailureDoesNotFailToggle(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	pusher := &fakeDevPreviewPusher{err: assertError("pod unreachable")}
	f.svc.SetDevPreviewPusher(pusher)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.Anything).Return(crd, nil)

	// The CRD is the source of truth; the push only shrinks the in-pod
	// tool surface's latency. A suspended workspace must not fail the
	// toggle.
	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.NoError(t, err)
}

func TestSetDevPreview_NoPushWhenCRDUpdateFails(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	pusher := &fakeDevPreviewPusher{}
	f.svc.SetDevPreviewPusher(pusher)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.Anything).Return(nil, assertError("conflict"))

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.Error(t, err)
	assert.Empty(t, pusher.calls, "no live push when the CRD write failed — the push must never claim a state the CRD does not hold")
}

func TestSetDevPreview_NilPusherTolerated(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.Anything).Return(crd, nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.NoError(t, err, "nil pusher (unwired deployments, unit tests) must be a no-op")
}

func TestSetDevPreview_DisablesFlag(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{DevPreview: true}
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	f.ws.On("Update", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.NetworkAccess != nil && ws.Spec.NetworkAccess.DevPreview == false
	})).Return(crd, nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", false)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestSetDevPreview_WrongOwner(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user2", "my-ws", "10Gi"), nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.Error(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestSetDevPreview_WorkspaceNotFound(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(nil, nil)

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.Error(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestSetDevPreview_CRDGetFails(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(nil, assertError("k8s error"))

	err := f.svc.SetDevPreview(ctx, "user1", "ws-1", true)
	assert.Error(t, err)
	f.ws.AssertNotCalled(t, "Update")
}

func TestGetWorkspace_ReturnsDevPreviewEnabled(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{DevPreview: true}
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	ws, err := f.svc.GetWorkspace(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.True(t, ws.DevPreviewEnabled, "GetWorkspace should populate DevPreviewEnabled from CRD")
}

func TestGetWorkspace_DevPreviewDefaultFalse(t *testing.T) {
	f := newDefaultsFixture(t, nil)
	ctx := context.Background()

	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	ws, err := f.svc.GetWorkspace(ctx, "user1", "ws-1")
	assert.NoError(t, err)
	assert.False(t, ws.DevPreviewEnabled, "DevPreviewEnabled should default to false")
}

type errAssert struct{ msg string }

func (e *errAssert) Error() string { return e.msg }

func assertError(msg string) error { return &errAssert{msg: msg} }
