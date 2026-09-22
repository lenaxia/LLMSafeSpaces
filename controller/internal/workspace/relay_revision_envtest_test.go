//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// relay_revision_envtest_test.go — US-72.4 review amendment (PR #1529,
// required item 3): a REAL apiserver round-trip for
// status.secretsDelivery.relayRevision. The unit suite pokes the
// in-memory struct, which passes even when the CRD schema lacks the
// property — and an apiextensions v1 structural schema silently PRUNES
// undeclared fields on every status write, which would leave the §4.2
// lineage conjunct (relayLineageIntact → sd.RelayRevision) structurally
// dead in every real cluster. This test drives the exact production
// write path (status subresource update through envtest, which installs
// helm/crds/) and asserts persistence — the regression net for the
// drift class pkg/repolint's SecretsDeliveryStatus binding now guards.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestRelayRevision
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestEnvtestRelayRevision_SurvivesStatusWrite: the controller's own
// write shape (mirrored relay slice: degrade code + applied relay
// revision) persists through a real API server — and a refetch from the
// apiserver (not the local object) still carries both halves the
// staging pass reads.
func TestEnvtestRelayRevision_SurvivesStatusWrite(t *testing.T) {
	cfg := startEnvtest(t)
	sch := testScheme(t)
	dyn, err := client.New(cfg, client.Options{Scheme: sch})
	require.NoError(t, err)
	ctx := context.Background()

	ws := makeRelayWorkspace("ws-relayrev")
	require.NoError(t, dyn.Create(ctx, ws))
	// Status is settable only through the subresource AFTER create (the
	// create response replaces the local object's status with the
	// server's empty one).
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{
		DegradedReason: "token_expired",
		RelayRevision:  "rENVTEST01",
	}
	require.NoError(t, dyn.Status().Update(ctx, ws))

	fetched := &v1.Workspace{}
	require.NoError(t, dyn.Get(ctx, client.ObjectKeyFromObject(ws), fetched))
	require.NotNil(t, fetched.Status.SecretsDelivery,
		"secretsDelivery itself must persist (the pre-existing fields' contract)")
	assert.Equal(t, "token_expired", fetched.Status.SecretsDelivery.DegradedReason)
	assert.Equal(t, "rENVTEST01", fetched.Status.SecretsDelivery.RelayRevision,
		"relayRevision must survive the apiserver's structural-schema pruning — a failure here means helm/crds/workspace.yaml lacks the property and the §4.2 lineage conjunct is dead in production")

	// The conjunct's own inputs, read exactly as relayLineageIntact reads
	// them: staged revision annotation vs the persisted RelayRevision.
	fetched.Annotations = map[string]string{relayStagedRevisionAnnotation: "rENVTEST01"}
	r := &WorkspaceReconciler{}
	desired := []relayDesiredProvider{{pd: openaiPD("openai", "sk-envtest")}}
	staged := map[string]relayStagedProviderState{"openai": {KeyID: "hpke-g1"}}
	assert.True(t, r.relayLineageIntact(fetched, desired, staged, "hpke-g1"),
		"the conjunct must hold end-to-end on a real apiserver write with matching revisions")
}
