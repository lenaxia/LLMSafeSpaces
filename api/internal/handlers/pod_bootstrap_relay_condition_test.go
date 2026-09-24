// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
