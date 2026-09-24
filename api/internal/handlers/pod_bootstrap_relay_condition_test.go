// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	crdv1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Design 0061 §6 (M4): the pod-bootstrap batch outcome writes the
// CredentialsStaged condition on the Workspace CRD. The hook is
// flag-gated (relay-only off ⇒ never writes — the W15 condition-absent
// contract) and sink-gated (no writer wired ⇒ byte-identical pre-M4
// handler). The degrade reason passes through verbatim: M2's
// relay_fallback_delivery lands in the same seam, uninterpreted.

// relayInjector wraps the standard fixture with the relay-flag seam
// (production injectors are *secrets.SecretService, which implements
// RelayOnlyEnabled; the plain fake does not — that absence IS the
// flag-off legacy shape).
type relayInjector struct {
	fakeBootstrapInjector
	relayEnabled bool
}

func (f *relayInjector) RelayOnlyEnabled() bool { return f.relayEnabled }

// captureSink records the outcome the handler would persist.
type captureSink struct {
	workspaceID string
	reason      string
	calls       int
	err         error
}

func (s *captureSink) ReportRelayBatchOutcome(_ context.Context, workspaceID, reason string) error {
	s.calls++
	s.workspaceID = workspaceID
	s.reason = reason
	return s.err
}

// newRelayCondRouter builds the bootstrap router with the outcome sink
// wired (the shared helper does not expose its handler for the setter).
func newRelayCondRouter(t *testing.T, inj bootstrapInjector, sink *captureSink) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-cond"},
		inj,
		&fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-cond", UserID: "u1"}},
		nil, testBootstrapNamespace)
	h.SetRelayOutcomeSink(sink)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	return r
}

func doRelayCondRequest(t *testing.T, r *gin.Engine) int {
	t.Helper()
	w := doBootstrap(t, r, "tok", `{"workspaceID":"ws-cond","contractVersion":2}`)
	return w.Code
}

func TestPodBootstrap_RelayCondition_FlagOnDegradeWritesReason(t *testing.T) {
	inj := &relayInjector{relayEnabled: true}
	inj.degrade = &secrets.BuildDegrade{Reason: "relay_staging_not_ready"}
	sink := &captureSink{}

	r := newRelayCondRouter(t, inj, sink)

	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, sink.calls)
	assert.Equal(t, "ws-cond", sink.workspaceID)
	assert.Equal(t, "relay_staging_not_ready", sink.reason,
		"the degrade reason passes through verbatim — M2's relay_fallback_delivery lands unmodified")
}

func TestPodBootstrap_RelayCondition_FlagOnCleanBatchWritesEmptyReason(t *testing.T) {
	inj := &relayInjector{relayEnabled: true}
	sink := &captureSink{}

	r := newRelayCondRouter(t, inj, sink)

	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, sink.calls)
	assert.Equal(t, "", sink.reason, "clean batch → empty reason → condition True")
}

func TestPodBootstrap_RelayCondition_FlagOffNeverWrites(t *testing.T) {
	inj := &relayInjector{relayEnabled: false}
	inj.degrade = &secrets.BuildDegrade{Reason: "relay_staging_not_ready"}
	sink := &captureSink{}

	r := newRelayCondRouter(t, inj, sink)

	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	assert.Zero(t, sink.calls, "flag-off: the condition stays absent (W15)")
}

// The plain fixture (no RelayOnlyEnabled method) is the legacy
// injector shape — the type assertion fails and nothing writes, exactly
// as pre-M4.
func TestPodBootstrap_RelayCondition_LegacyInjectorNeverWrites(t *testing.T) {
	inj := &fakeBootstrapInjector{degrade: &secrets.BuildDegrade{Reason: "relay_staging_not_ready"}}
	sink := &captureSink{}

	r := newRelayCondRouter(t, inj, sink)

	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	assert.Zero(t, sink.calls, "no flag seam ⇒ no write")
}

// Best-effort by design: a condition-write failure never fails the
// bootstrap — the batch already delivered.
func TestPodBootstrap_RelayCondition_SinkErrorStillBootstraps(t *testing.T) {
	inj := &relayInjector{relayEnabled: true}
	inj.degrade = &secrets.BuildDegrade{Reason: "relay_staging_not_ready"}
	sink := &captureSink{err: assert.AnError}

	r := newRelayCondRouter(t, inj, sink)

	code := doRelayCondRequest(t, r)
	assert.Equal(t, http.StatusOK, code, "a visibility write failure must not gate the boot")
}

// Finding 1 arm (r1): a DEK-tier degrade under flag-on writes NEITHER
// arm — the condition mirrors the relay tier only. The builder's
// IsRelayDegrade owns the vocabulary; a non-relay degrade's signal
// rides the existing degrade log + audit, not a relay-staging
// condition whose message would be factually false (admin/org
// providers were still relay-delivered).
func TestPodBootstrap_RelayCondition_NonRelayDegradeNeverWrites(t *testing.T) {
	inj := &relayInjector{relayEnabled: true}
	inj.degrade = &secrets.BuildDegrade{Reason: "dek_unwrap_failed"}
	sink := &captureSink{}

	r := newRelayCondRouter(t, inj, sink)
	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	assert.Zero(t, sink.calls,
		"a DEK-tier degrade must not surface as a relay-staging condition")
}

// Real-wiring arm (r1): handler → REAL *workspace.Service → mocked
// WorkspaceInterface — the seam the unit layers each fake separately
// (Rule 0: "every layer mocked the next" shipped the org-provider
// regression). A flag-on degraded bootstrap must land the condition
// through the production writer, not a captureSink.
func TestPodBootstrap_RelayCondition_RealServiceWritesCondition(t *testing.T) {
	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	wsIface := kmocks.NewMockWorkspaceInterface()
	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(wsIface)
	k8s.On("Clientset").Return(k8sfake.NewSimpleClientset())
	wsSvc, err := workspace.New(log, k8s, &imocks.MockDatabaseService{},
		&imocks.MockCacheService{}, &imocks.MockMetricsService{},
		&workspace.Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	wsIface.On("Get", mock.Anything, "ws-cond", mock.Anything).
		Return(&crdv1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "ws-cond", Namespace: "default"},
			Status:     crdv1.WorkspaceStatus{Phase: crdv1.WorkspacePhaseActive},
		}, nil)
	var persisted *crdv1.Workspace
	wsIface.On("UpdateStatus", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) { persisted = args.Get(1).(*crdv1.Workspace) }).
		Return(nil, nil).Once()

	inj := &relayInjector{relayEnabled: true}
	inj.degrade = &secrets.BuildDegrade{Reason: "relay_staging_not_ready"}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-cond"},
		inj,
		&fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-cond", UserID: "u1"}},
		nil, testBootstrapNamespace)
	h.SetRelayOutcomeSink(wsSvc) // the REAL service
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	code := doRelayCondRequest(t, r)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, persisted, "the real writer must persist via UpdateStatus")
	for _, c := range persisted.Status.Conditions {
		if c.Type == crdv1.WorkspaceConditionCredentialsStaged {
			assert.Equal(t, "False", c.Status)
			assert.Equal(t, "relay_staging_not_ready", c.Reason)
			return
		}
	}
	t.Fatal("CredentialsStaged condition not written through the real service")
}
