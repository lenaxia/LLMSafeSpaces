//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// staging_envtest_test.go — US-72.3 envtest conditions matrix: the staging
// pass + its §4.6 conditions against a REAL API server. What the API
// server adds over the fake-client suite: true status-subresource
// semantics (the two-phase relayPersist — status write, then re-applied
// metadata write — must survive real optimistic-concurrency handling),
// real Secret label validation, and real owner-reference admission for the
// handoff Secret.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestRelayStaging
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// envtestStagingRig wires a reconciler whose client is the envtest API
// server but whose source/router are fakes (hermetic: no network beyond
// the API server itself).
func envtestStagingRig(t *testing.T, objs ...client.Object) (*WorkspaceReconciler, *fakeProviderSource, *fakeRouterClient) {
	t.Helper()
	cfg := startEnvtest(t)
	sch := testScheme(t)
	dyn, err := client.New(cfg, client.Options{Scheme: sch})
	require.NoError(t, err)

	relayNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: relayTestNamespace}}
	require.NoError(t, dyn.Create(context.Background(), relayNS))
	for _, o := range objs {
		require.NoError(t, dyn.Create(context.Background(), o))
	}

	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "sk-envtest")}}
	router := &fakeRouterClient{}
	r := &WorkspaceReconciler{
		Client:   dyn,
		Scheme:   sch,
		Recorder: record.NewFakeRecorder(64),
	}
	staging, err := NewRelayStagingConfig("http://llm-relay-router.llm-relay.svc.cluster.local", relayTestNamespace, 0, src, router, &recordingRedactor{}, dyn)
	require.NoError(t, err)
	r.RelayStaging = staging
	return r, src, router
}

// TestEnvtestRelayStaging_ConditionsMatrix drives the §4.6 surface through
// the full lifecycle against the real API server: staged → every stale
// cause class → rejected → recovery back to staged.
func TestEnvtestRelayStaging_ConditionsMatrix(t *testing.T) {
	pubSec, _ := makePubSecret(t, 5)
	ws := makeRelayWorkspace("ws-envtest")
	ws.Namespace = "default"
	// Explicit image reference so no RuntimeEnvironment CRD is required
	// (the newWorkspaceForSecurity precedent).
	ws.Spec.Runtime = "ghcr.io/lenaxia/llmsafespaces/runtimes/base:test"
	r, src, _ := envtestStagingRig(t, pubSec, ws)
	ctx := context.Background()
	stagedRev := func() string { return ws.Annotations[relayStagedRevisionAnnotation] }

	// Leg 1 — staged: envelope + handoff + conditions land through real
	// admission (labels, owner ref) and real status subresource writes.
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	stored := &v1.Workspace{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: "ws-envtest", Namespace: "default"}, stored))
	require.NotNil(t, findCond(stored, v1.WorkspaceConditionCredentialsStaged))
	assert.Equal(t, "True", findCond(stored, v1.WorkspaceConditionCredentialsStaged).Status)
	assert.Equal(t, "5", stored.Annotations[relaySealedGenerationAnnotation])

	envSec := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-envtest", "openai")}, envSec))
	assert.Equal(t, "ws-envtest", envSec.Labels[secrets.RelayEnvWorkspaceLabel])

	ho := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: handoffSecretName("ws-envtest")}, ho))
	require.NotEmpty(t, ho.OwnerReferences, "handoff must be owner-ref'd (GC on workspace delete)")

	// Leg 2 — pub corruption (shape-invalid): staged flips False with the
	// PubUnreadable cause, loudly, nothing re-sealed.
	ws.Status.SecretsDelivery = nil
	badPub := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: relayTestNamespace},
		Data:       map[string][]byte{secrets.RelayPubDataKey: []byte("}{garbage")},
	}
	require.NoError(t, r.Update(ctx, badPub))
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	require.NotNil(t, findCond(ws, v1.WorkspaceConditionCredentialsStaged))
	assert.Equal(t, v1.ReasonStalePubUnreadable, findCond(ws, v1.WorkspaceConditionCredentialsStaged).Reason)

	// Leg 3 — recovery: pub restored; staged returns True. The restore
	// must re-get first: leg 2's real Update bumped the resourceVersion,
	// and the real API server rejects the stale in-memory pubSec with a
	// Conflict. (The fake client enforces RV conflicts too — the reason
	// this never surfaced is the //go:build envtest tag: the file never
	// compiled into any untagged run, and no CI workflow EXECUTED this
	// suite until it was wired into the envtest workflow; the -run
	// filters execution, not compilation.)
	freshPub := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: relayTestNamespace, Name: secrets.RelayPubSecretName}, freshPub))
	freshPub.Data = pubSec.Data
	require.NoError(t, r.Update(ctx, freshPub))
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	assert.Equal(t, v1.ReasonCredentialsStaged, findCond(ws, v1.WorkspaceConditionCredentialsStaged).Reason)

	// Leg 4 — delivery-class (#852 deferral shape): spawned_rev behind the
	// staged revision → DeliveryDeferred, never escalation.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: "older-rev", DegradedReason: "relay_unreachable"}
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	assert.Equal(t, v1.ReasonStaleDeliveryDeferred, findCond(ws, v1.WorkspaceConditionCredentialStale).Reason)

	// Leg 5 — token-expiry-class.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: stagedRev(), DegradedReason: "token_expired"}
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	assert.Equal(t, v1.ReasonStaleTokenExpired, findCond(ws, v1.WorkspaceConditionCredentialStale).Reason)

	// Leg 6 — corruption/wrong-pub class (§4.2's escalating cause) with
	// intact lineage: surfaced loudly; escalation itself is proven in the
	// dr_window unit suite.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: stagedRev(), DegradedReason: "credential_stale"}
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	assert.Equal(t, v1.ReasonStaleWrongPubLineage, findCond(ws, v1.WorkspaceConditionCredentialStale).Reason)

	// Leg 7 — rejection telemetry → CredentialRejected + event.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: stagedRev(), DegradedReason: "sanitization_refused"}
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	rej := findCond(ws, v1.WorkspaceConditionCredentialRejected)
	require.NotNil(t, rej)
	assert.Equal(t, v1.ReasonCredentialRouterRejected, rej.Reason)

	// Leg 8 — clean pass clears stale + rejected, staged stays True.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: stagedRev()}
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	assert.Nil(t, findCond(ws, v1.WorkspaceConditionCredentialStale))
	assert.Nil(t, findCond(ws, v1.WorkspaceConditionCredentialRejected))
	assert.Equal(t, "True", findCond(ws, v1.WorkspaceConditionCredentialsStaged).Status)

	// Leg 9 — revocation: unbind deletes the envelope through the real API
	// server (D2) and surfaces the one-pass revocation stale.
	src.mu.Lock()
	src.providers = nil
	src.mu.Unlock()
	require.NoError(t, r.reconcileRelayStaging(ctx, ws))
	err := r.Get(ctx, types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-envtest", "openai")}, &corev1.Secret{})
	assert.Error(t, err, "revocation must delete the envelope Secret")
	assert.Equal(t, v1.ReasonStaleRevoked, findCond(ws, v1.WorkspaceConditionCredentialStale).Reason)
}

func findCond(ws *v1.Workspace, t v1.WorkspaceConditionType) *v1.WorkspaceCondition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == t {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}
