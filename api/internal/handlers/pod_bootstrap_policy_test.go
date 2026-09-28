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

// policyAwareInjector records the per-step call order (policy sync,
// manifest, build) so tests can pin that global-default convergence
// runs BEFORE the manifest tier derives the live hash — otherwise a
// boot-time sync would mint a hash the same request already compared
// against.
type policyAwareInjector struct {
	fakeBootstrapInjector
	events    *[]string
	syncErr   error
	syncCalls int
}

func (p *policyAwareInjector) BuildWorkspaceBatch(ctx context.Context, u, w string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	*p.events = append(*p.events, "build")
	return p.fakeBootstrapInjector.BuildWorkspaceBatch(ctx, u, w)
}

func (p *policyAwareInjector) ManifestFor(ctx context.Context, u, w string) (string, error) {
	*p.events = append(*p.events, "manifest")
	return p.fakeBootstrapInjector.ManifestFor(ctx, u, w)
}

func (p *policyAwareInjector) SyncGlobalDefaultBindings(_ context.Context, _, _ string) ([]string, []string, error) {
	p.syncCalls++
	*p.events = append(*p.events, "policy-sync")
	if p.syncErr != nil {
		return nil, nil, p.syncErr
	}
	return nil, nil, nil
}

func newPolicyBootstrapTest(t *testing.T) (*gin.Engine, *policyAwareInjector, *[]string) {
	t.Helper()
	var events []string
	injector := &policyAwareInjector{fakeBootstrapInjector: fakeBootstrapInjector{
		manifestHash: "hash-v1",
		currentSeq:   1, currentHash: "hash-v1", currentOK: true,
	}, events: &events}
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	return router, injector, &events
}

// TestPodBootstrap_PolicySyncRunsBeforeManifest: a v2 conditional pull
// converges global-default bindings first, so the manifest hash this
// request computes (and 304-compares) already reflects any materialized
// policy rows.
func TestPodBootstrap_PolicySyncRunsBeforeManifest(t *testing.T) {
	router, injector, events := newPolicyBootstrapTest(t)

	w := doBootstrap(t, router, "valid-token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"hash-v1"}`)

	require.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, 1, injector.syncCalls)
	require.Len(t, *events, 2)
	assert.Equal(t, []string{"policy-sync", "manifest"}, *events)
}

// TestPodBootstrap_PolicySyncFailureIsBestEffort: a failing policy sync
// must NOT brick the pod boot — it is logged and the request proceeds
// (the reconcile loop re-converges on its next pass).
func TestPodBootstrap_PolicySyncFailureIsBestEffort(t *testing.T) {
	router, injector, events := newPolicyBootstrapTest(t)
	injector.syncErr = context.DeadlineExceeded

	w := doBootstrap(t, router, "valid-token", `{"workspaceID":"ws-abc"}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, injector.syncCalls)
	require.Len(t, *events, 2)
	assert.Equal(t, []string{"policy-sync", "build"}, *events)
}

// TestPodBootstrap_PolicySyncSkippedForLegacyInjector: an injector
// without the policy seam (pre-upgrade API replica, test fakes) keeps
// the pre-policy behavior — no sync, straight to build.
func TestPodBootstrap_PolicySyncSkippedForLegacyInjector(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	injector := &legacyOnlyInjector{entry: &secrets.BatchEntry{
		SecretID: "sec-1", Version: 1, Type: secrets.SecretTypeEnvSecret, Name: "n", Value: "v",
	}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "valid-token", `{"workspaceID":"ws-abc"}`)

	require.Equal(t, http.StatusOK, w.Code)
}
