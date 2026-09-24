// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Design 0061 §6 (M4): the batch-build outcome surfaces on the Workspace
// CRD as the EXISTING CredentialsStaged condition — False/<reason> on a
// degraded batch, True on the next clean batch — via the same status
// path the controller's conditions ride (UpdateStatus). The reason is an
// INPUT (relay_staging_not_ready today; M2's relay_fallback_delivery
// slots in without touching this writer).
func TestReportRelayBatchOutcome_DegradeWritesFalseWithReason(t *testing.T) {
	f := newStagedCondFixture(t)
	crd := stagedCondCRD("ws-1")
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	var persisted *v1.Workspace
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*v1.Workspace) }).
		Return(nil, nil).Once()

	err := f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "relay_staging_not_ready")
	require.NoError(t, err)
	require.NotNil(t, persisted)

	cond := findStagedCond(t, persisted)
	assert.Equal(t, "False", cond.Status)
	assert.Equal(t, "relay_staging_not_ready", cond.Reason)
	assert.Contains(t, cond.Message, "relay_staging_not_ready",
		"the message answers 'why does my workspace have no models'")
	assert.False(t, cond.LastTransitionTime.IsZero())
}

func TestReportRelayBatchOutcome_CleanBatchWritesTrue(t *testing.T) {
	f := newStagedCondFixture(t)
	crd := stagedCondCRD("ws-1")
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	var persisted *v1.Workspace
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*v1.Workspace) }).
		Return(nil, nil).Once()

	err := f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "")
	require.NoError(t, err)

	cond := findStagedCond(t, persisted)
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, v1.ReasonCredentialsStaged, cond.Reason)
}

// The controller's setCondition semantics (health.go): a repeated
// same-state write refreshes the message WITHOUT bumping
// LastTransitionTime — no spurious transition flapping between the
// controller's staging writes and the API's batch writes.
func TestReportRelayBatchOutcome_SameStatePreservesTransitionTime(t *testing.T) {
	f := newStagedCondFixture(t)
	first := metav1.NewTime(metav1.Now().Add(-time.Hour))
	crd := stagedCondCRD("ws-1")
	crd.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionCredentialsStaged, Status: "False",
		Reason: "relay_staging_not_ready", Message: "old message",
		LastTransitionTime: first,
	}}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	var persisted *v1.Workspace
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*v1.Workspace) }).
		Return(nil, nil).Once()

	require.NoError(t, f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "relay_staging_not_ready"))

	cond := findStagedCond(t, persisted)
	assert.Equal(t, first, cond.LastTransitionTime,
		"same status+reason must not bump LastTransitionTime")
	assert.NotEqual(t, "old message", cond.Message, "message still refreshes")
}

// A status write racing the controller retries on conflict — the
// service's established RetryOnConflict pattern (resume path).
func TestReportRelayBatchOutcome_RetriesOnConflict(t *testing.T) {
	f := newStagedCondFixture(t)
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "llmsafespaces.io", Resource: "workspaces"},
		"ws-1", assert.AnError)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(stagedCondCRD("ws-1"), nil)
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).Return(nil, conflict).Once()
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).Return(nil, nil).Once()

	err := f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "relay_staging_not_ready")
	require.NoError(t, err, "the conflict must be retried to success")
	f.ws.AssertNumberOfCalls(t, "UpdateStatus", 2)
}

func TestReportRelayBatchOutcome_MissingWorkspaceIsAnError(t *testing.T) {
	f := newStagedCondFixture(t)
	f.ws.On("Get", mock.Anything, "ws-404", mock.Anything).
		Return(nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "llmsafespaces.io", Resource: "workspaces"}, "ws-404"))

	err := f.svc.ReportRelayBatchOutcome(context.Background(), "ws-404", "relay_staging_not_ready")
	assert.Error(t, err, "a missing CR is a loud error, not a silent skip")
}

// --- fixture ---------------------------------------------------------------

func newStagedCondFixture(t *testing.T) struct {
	svc *Service
	ws  *kmocks.MockWorkspaceInterface
} {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	ws := kmocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(ws)
	k8s.On("Clientset").Return(k8sfake.NewSimpleClientset())

	db := &imocks.MockDatabaseService{}
	cache := &imocks.MockCacheService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc, err := New(log, k8s, db, cache, met, &Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	return struct {
		svc *Service
		ws  *kmocks.MockWorkspaceInterface
	}{svc: svc, ws: ws}
}

func stagedCondCRD(name string) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
}

func findStagedCond(t *testing.T, ws *v1.Workspace) v1.WorkspaceCondition {
	t.Helper()
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionCredentialsStaged {
			return c
		}
	}
	t.Fatal("CredentialsStaged condition not found on persisted status")
	return v1.WorkspaceCondition{}
}

// r1 arms — the flip paths and the no-op/preservation contracts.

// False→True (and True→False) on an existing condition bumps
// LastTransitionTime — the transition clock is real state, not an
// artifact of append-order.
func TestReportRelayBatchOutcome_FalseToTrueBumpsTransitionTime(t *testing.T) {
	f := newStagedCondFixture(t)
	first := metav1.NewTime(metav1.Now().Add(-time.Hour))
	crd := stagedCondCRD("ws-1")
	crd.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionCredentialsStaged, Status: "False",
		Reason: "relay_staging_not_ready", Message: "degraded",
		LastTransitionTime: first,
	}}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	var persisted *v1.Workspace
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*v1.Workspace) }).
		Return(nil, nil).Once()

	require.NoError(t, f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", ""))
	cond := findStagedCond(t, persisted)
	assert.Equal(t, "True", cond.Status)
	assert.True(t, cond.LastTransitionTime.After(first.Time),
		"a False→True heal is a real transition")
}

func TestReportRelayBatchOutcome_TrueToFalseBumpsTransitionTime(t *testing.T) {
	f := newStagedCondFixture(t)
	first := metav1.NewTime(metav1.Now().Add(-time.Hour))
	crd := stagedCondCRD("ws-1")
	crd.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionCredentialsStaged, Status: "True",
		Reason: v1.ReasonCredentialsStaged, Message: "3 provider(s) staged at revision r7",
		LastTransitionTime: first,
	}}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	var persisted *v1.Workspace
	f.ws.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*v1.Workspace) }).
		Return(nil, nil).Once()

	require.NoError(t, f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "relay_staging_not_ready"))
	cond := findStagedCond(t, persisted)
	assert.Equal(t, "False", cond.Status)
	assert.True(t, cond.LastTransitionTime.After(first.Time),
		"a True→False degrade is a real transition")
}

// The revision-in-message contract (workspace_types.go: "True carries
// the staged revision in its message"): True already standing at
// ReasonCredentialsStaged is a NO-OP — no UpdateStatus at all, so the
// controller's revision-carrying message stands until its next pass.
// The API's True exists to heal a prior False, not to re-assert.
func TestReportRelayBatchOutcome_AlreadyTrueIsANoWrite(t *testing.T) {
	f := newStagedCondFixture(t)
	crd := stagedCondCRD("ws-1")
	crd.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionCredentialsStaged, Status: "True",
		Reason: v1.ReasonCredentialsStaged, Message: "3 provider(s) staged at revision r7",
		LastTransitionTime: metav1.NewTime(metav1.Now().Add(-time.Minute)),
	}}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	// No UpdateStatus expectation at all: any call fails the test.

	require.NoError(t, f.svc.ReportRelayBatchOutcome(context.Background(), "ws-1", ""))
	f.ws.AssertNumberOfCalls(t, "UpdateStatus", 0)
}

// The client-initialization failure propagates as a loud error, not a
// silent skip (the workspaceCRDClient error path).
func TestReportRelayBatchOutcome_ClientInitFailurePropagates(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	k8s.On("Clientset").Return(k8sfake.NewSimpleClientset())
	k8s.On("LlmsafespacesV1").Return(nil, assert.AnError) // init fails

	svc, err := New(log, k8s, &imocks.MockDatabaseService{}, &imocks.MockCacheService{},
		&imocks.MockMetricsService{}, &Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	err = svc.ReportRelayBatchOutcome(context.Background(), "ws-1", "relay_staging_not_ready")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "initialize workspace client")
}
