// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// staging_dr_test.go — dr_window_reconcile_terminates (design 0058 §4.2,
// the controller-homed reconcile predicate): the pub-generation change →
// re-seal leg; the corruption/wrong-pub-class stale + intact spawn-layer
// lineage + unchanged generation → anti-storm-bounded rotate escalation
// leg; and the SUPPRESSION matrix — escalation never fires for
// revocation-class, delivery-class (incl. the #852 busy-session deferral
// window), or token-expiry-class staleness.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// drEnv bundles the DR-test rig: a staged workspace whose agent reports the
// corruption-class degrade, with the router's rotate hook advancing the pub
// Secret exactly like the real two-Secret update does.
type drEnv struct {
	r      *WorkspaceReconciler
	ws     *v1.Workspace
	router *fakeRouterClient
	src    *fakeProviderSource
	kp1    *secrets.HPKEKeyPairPayload // generation-1 keypair (initial)
}

func newDREnv(t *testing.T, name string, degradedReason string, spawnedMatchesStaged bool) *drEnv {
	t.Helper()
	pub1, kp1 := makePubSecret(t, 1)
	ws := makeRelayWorkspace(name)
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "dr-key")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pub1, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	stagedRev := ws.Annotations[relayStagedRevisionAnnotation]
	require.NotEmpty(t, stagedRev)

	// The agent reports a degrade; spawned_rev decides the lineage conjunct.
	spawned := "some-other-revision"
	if spawnedMatchesStaged {
		spawned = stagedRev
	}
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: spawned, DegradedReason: degradedReason}
	// One keypair for generation 2: the rotate hook publishes it to the pub
	// Secret (the router's two-Secret update) and the receipt carries its
	// RAW public key bytes (the real router returns
	// rotateReceipt{PublicKey: pub.PublicKey}).
	kp2, err := secrets.GenerateHPKEKeyPairPayload(2)
	require.NoError(t, err)
	kp2Pub := kp2.Public()
	pub2Data, err := kp2Pub.Marshal()
	require.NoError(t, err)
	router.onRotate = func(*testing.T) {
		require.NoError(t, r.Update(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: relayTestNamespace},
			Data:       map[string][]byte{secrets.RelayPubDataKey: pub2Data},
		}))
	}
	router.receipt = RelayRotateReceipt{KeyID: "hpke-g2", Generation: 2, PublicKey: kp2.Public().PublicKey}
	return &drEnv{r: r, ws: ws, router: router, src: src, kp1: kp1}
}

// Leg 1: corruption/wrong-pub-class stale, lineage intact, generation
// unchanged → the rotate escalation fires and the re-seal completes (the
// window TERMINATES — bounded, not merely promised).
func TestDrWindowReconcileTerminates_EscalatesOnCorruptionWithIntactLineage(t *testing.T) {
	env := newDREnv(t, "ws-dr1", "credential_stale", true)

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))

	env.router.mu.Lock()
	rotates := env.router.rotates
	env.router.mu.Unlock()
	assert.Equal(t, 1, rotates, "corruption-class stale under intact lineage must escalate to POST /internal/v1/keys/rotate")

	// The envelope is re-sealed under the NEW generation (keyID metadata
	// alone — the controller's write-acks, never a decrypt).
	envlope := string(mustGet(t, env.r, relayTestNamespace, envelopeSecretName("ws-dr1", "openai")).Data[secrets.RelayEnvDataKey])
	_, keyID, err := secrets.InspectStagingEnvelope(envlope)
	require.NoError(t, err)
	assert.Equal(t, "hpke-g2", keyID)
	assert.Equal(t, "2", env.ws.Annotations[relaySealedGenerationAnnotation])

	// Anti-storm bound recorded on the cluster-global mint-key Secret.
	mk := mustGet(t, env.r, relayTestNamespace, secrets.RelayMintKeyName)
	last := mk.Annotations[relayLastRotateEscalationAnnotation]
	_, err = time.Parse(time.RFC3339, last)
	assert.NoError(t, err, "rotate escalation must be timestamped")
}

// Leg 2 (anti-storm): a follow-up escalation within the 10m floor is
// suppressed — including from a DIFFERENT workspace (the bound is
// cluster-global on the mint-key Secret).
func TestDrWindowReconcileTerminates_AntiStormBoundIsClusterGlobal(t *testing.T) {
	env := newDREnv(t, "ws-dr2", "credential_stale", true)
	env.router.onRotate = nil // a second rotate must NOT fire

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
	env.router.mu.Lock()
	assert.Equal(t, 1, env.router.rotates)
	env.router.mu.Unlock()

	// A second workspace, same cluster, same corruption-class stale: the
	// mint-key annotation (written minutes ago) suppresses its escalation.
	pub1, _ := makePubSecret(t, 1)
	ws2 := makeRelayWorkspace("ws-dr2b")
	src2 := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k")}}
	router2 := &fakeRouterClient{}
	r2 := stagingReconciler(t, src2, router2, nil, pub1, ws2, mustGet(t, env.r, relayTestNamespace, secrets.RelayMintKeyName))
	require.NoError(t, r2.reconcileRelayStaging(context.Background(), ws2))
	ws2.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: ws2.Annotations[relayStagedRevisionAnnotation], DegradedReason: "credential_stale"}
	require.NoError(t, r2.reconcileRelayStaging(context.Background(), ws2))

	router2.mu.Lock()
	assert.Equal(t, 0, router2.rotates, "escalation within the 10m floor must be suppressed (anti-storm)")
	router2.mu.Unlock()
}

// Leg 3 (suppression): token-expiry-class staleness NEVER escalates —
// renewal owns it; rotation re-seals envelopes and never mints tokens.
func TestDrWindowReconcileTerminates_SuppressesTokenExpiryClass(t *testing.T) {
	env := newDREnv(t, "ws-dr3", "token_expired", true)

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
	env.router.mu.Lock()
	assert.Equal(t, 0, env.router.rotates, "token-expiry-class must not escalate")
	env.router.mu.Unlock()
	stale := conditionOf(env.ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, v1.ReasonStaleTokenExpired, stale.Reason)
}

// Leg 4 (suppression): delivery-class staleness (relay unreachable, batch
// deferral, unknown codes) NEVER escalates — a fault or wait rotation
// cannot repair.
func TestDrWindowReconcileTerminates_SuppressesDeliveryClass(t *testing.T) {
	for _, code := range []string{"relay_unreachable", "spawn_env_pending", ""} {
		env := newDREnv(t, "ws-dr4-"+nonEmptyOr(code, "none"), code, true)
		require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
		env.router.mu.Lock()
		assert.Equal(t, 0, env.router.rotates, "delivery-class %q must not escalate", code)
		env.router.mu.Unlock()
	}
}

// Leg 5 (the #852 pin): corruption-class stale with a BROKEN lineage — the
// child has NOT spawned with the staged revision (the restart is deferred
// behind busy sessions) — reads as pending-delivery and NEVER escalates.
// spawned_rev is the terminal signal, not the batch-apply anchor.
func TestDrWindowReconcileTerminates_SuppressesWhenLineageBroken(t *testing.T) {
	env := newDREnv(t, "ws-dr5", "credential_stale", false)

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
	env.router.mu.Lock()
	assert.Equal(t, 0, env.router.rotates, "the #852 deferral window must never escalate")
	env.router.mu.Unlock()
	stale := conditionOf(env.ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, v1.ReasonStaleWrongPubLineage, stale.Reason, "still surfaced loudly — just not escalated")
}

// Leg 6 (suppression): revocation-class staleness NEVER escalates — the
// envelope deletion is D2's designed terminal state.
func TestDrWindowReconcileTerminates_SuppressesRevocationClass(t *testing.T) {
	env := newDREnv(t, "ws-dr6", "", true)
	// Unbind after staging: the next pass performs the revocation.
	env.src.mu.Lock()
	env.src.providers = nil
	env.src.mu.Unlock()

	// Seed the mint-key anti-storm annotation far in the past so the bound
	// itself could not be the suppressor.
	mk := mustGet(t, env.r, relayTestNamespace, secrets.RelayMintKeyName)
	mk.Annotations = map[string]string{relayLastRotateEscalationAnnotation: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	require.NoError(t, env.r.Update(context.Background(), mk))

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
	env.router.mu.Lock()
	assert.Equal(t, 0, env.router.rotates, "revocation-class must not escalate (D2 terminal)")
	env.router.mu.Unlock()
	stale := conditionOf(env.ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, v1.ReasonStaleRevoked, stale.Reason)
}

// Leg 7 (rejected surfacing): router rejection telemetry maps to
// CredentialRejected + an operator event (§4.7 — never silent).
func TestDrWindowReconcileTerminates_RejectedSurfacesConditionAndEvent(t *testing.T) {
	env := newDREnv(t, "ws-dr7", "scope_violation", true)

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))

	rej := conditionOf(env.ws, v1.WorkspaceConditionCredentialRejected)
	require.NotNil(t, rej)
	assert.Equal(t, "True", rej.Status)
	assert.Equal(t, v1.ReasonCredentialRouterRejected, rej.Reason)
	stale := conditionOf(env.ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, rej)
	if stale != nil {
		assert.Equal(t, v1.ReasonStaleDeliveryDeferred, stale.Reason)
	}
	rec := env.r.Recorder.(*record.FakeRecorder)
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "RouterRejected")
	default:
		t.Fatal("a rejection must emit an operator-visible event")
	}
}

// Leg 8 (review r2 combination): a revocation co-present with a
// corruption-class degrade must classify as REVOCATION — the degrade is
// most plausibly about the just-revoked provider (its token fails closed
// by design), and escalation is forbidden for revocation-class (§4.2).
func TestDrWindowReconcileTerminates_RevocationWinsOverCorruptionDegrade(t *testing.T) {
	env := newDREnv(t, "ws-dr8", "credential_stale", true)
	env.src.mu.Lock()
	env.src.providers = nil // unbind the only provider this pass
	env.src.mu.Unlock()

	require.NoError(t, env.r.reconcileRelayStaging(context.Background(), env.ws))
	env.router.mu.Lock()
	assert.Equal(t, 0, env.router.rotates, "a co-present revocation must suppress escalation")
	env.router.mu.Unlock()
	stale := conditionOf(env.ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, v1.ReasonStaleRevoked, stale.Reason)
}

func mustGet(t *testing.T, r *WorkspaceReconciler, ns, name string) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, sec); err != nil {
		t.Fatalf("get secret %s/%s: %v", ns, name, err)
	}
	return sec
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
