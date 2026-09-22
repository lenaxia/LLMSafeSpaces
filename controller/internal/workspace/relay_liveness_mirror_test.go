// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// relay_liveness_mirror_test.go — US-72.4 (design 0058 §4.5/§4.6): the
// controller's mirror of agentd's relay-only liveness slice into the
// SecretsDelivery surface the US-72.3 staging classification reads
// (classifyRelayStale / relayRouterRejection / the §4.2 lineage
// conjunct).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestCheckAgentHealth_MirrorsRelayLivenessDegrade: a relay degrade
// code rides DegradedReason (the classifier's input) and the applied
// relay revision rides RelayRevision (the lineage-conjunct input) —
// with the relay reason WINNING over a co-present spawn-env reason
// (class-critical codes must not be masked).
func TestCheckAgentHealth_MirrorsRelayLivenessDegrade(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		SpawnEnv: &agentd.SpawnEnvHealth{
			SpawnedRev: "9:abc:def",
			Degraded:   true,
			Reason:     "spawn_env_unavailable",
		},
		Relay: &agentd.RelayHealth{
			Present: true, Reachable: false, DegradedReason: "token_expired",
			RouterURL:       "http://llm-relay-router.llm-relay.svc.cluster.local",
			AppliedRevision: "rEXPIRE01",
		},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "token_expired", ws.Status.SecretsDelivery.DegradedReason,
		"the relay reason wins over the co-present spawn-env reason (class-critical, not maskable)")
	assert.Equal(t, "9:abc:def", ws.Status.SecretsDelivery.SpawnedRev, "spawn evidence is preserved")
	assert.Equal(t, "rEXPIRE01", ws.Status.SecretsDelivery.RelayRevision,
		"the applied relay revision feeds the §4.2 lineage conjunct")
}

// TestCheckAgentHealth_RelayWithoutSpawnEnv: the relay slice stands
// alone when no spawn-env evidence exists — evidence from a live pod.
func TestCheckAgentHealth_RelayWithoutSpawnEnv(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		Relay: &agentd.RelayHealth{
			Present: true, Reachable: false, DegradedReason: "relay_unreachable",
		},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "relay_unreachable", ws.Status.SecretsDelivery.DegradedReason)
}

// TestCheckAgentHealth_RelayHealthyLeavesSpawnReason: a HEALTHY relay
// slice must not erase a real spawn-env degrade (delivery is still
// broken even though the relay path answers).
func TestCheckAgentHealth_RelayHealthyLeavesSpawnReason(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		SpawnEnv: &agentd.SpawnEnvHealth{
			SpawnedRev: "9:abc:def",
			Degraded:   true,
			Reason:     "spawn_env_unavailable",
		},
		Relay: &agentd.RelayHealth{Present: true, Reachable: true, AppliedRevision: "rOK0001"},
	})

	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.SecretsDelivery)
	assert.Equal(t, "spawn_env_unavailable", ws.Status.SecretsDelivery.DegradedReason)
	assert.Equal(t, "rOK0001", ws.Status.SecretsDelivery.RelayRevision)
}

// TestRelayLineage_SatisfiedByAppliedRelayRevision: the §4.2 lineage
// conjunct is live through RelayRevision (US-72.4's signal) — and a
// pre-US-72.4 runtime reporting ONLY SpawnedRev never satisfies it
// (escalation stays structurally suppressed pre-flip).
func TestRelayLineage_SatisfiedByAppliedRelayRevision(t *testing.T) {
	ws := makeRelayWorkspace("ws-lineage")
	ws.Annotations[relayStagedRevisionAnnotation] = "rLIN0001"
	desired := []relayDesiredProvider{{pd: secrets.LLMProviderData{Slug: "openai"}}}
	staged := map[string]relayStagedProviderState{"openai": {KeyID: "hpke-g1"}}

	r := &WorkspaceReconciler{}

	// Post-US-72.4 runtime: applied relay revision matches.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{RelayRevision: "rLIN0001"}
	assert.True(t, r.relayLineageIntact(ws, desired, staged, "hpke-g1"))

	// Fresh token not yet applied (revision behind staged).
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{RelayRevision: "rOLD0000"}
	assert.False(t, r.relayLineageIntact(ws, desired, staged, "hpke-g1"),
		"a pod applying an older staged revision has pending delivery — the #852 window never escalates")

	// Pre-US-72.4 runtime: SpawnedRev alone (even when string-equal to
	// the staged revision) is not the conjunct's signal.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: "rLIN0001"}
	assert.False(t, r.relayLineageIntact(ws, desired, staged, "hpke-g1"),
		"pre-US-72.4 pods never satisfy the conjunct (structurally suppressed escalation)")

	// KeyID drift breaks it regardless.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{RelayRevision: "rLIN0001"}
	assert.False(t, r.relayLineageIntact(ws, desired, staged, "hpke-g2"))
}
