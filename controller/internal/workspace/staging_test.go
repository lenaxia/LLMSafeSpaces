// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// staging_test.go — US-72.3 red-first suite (design 0058 §4.1/§4.2/§4.6/
// §4.9). Covers the story's named controller legs: stage/unbind/rotate
// (rotate completion confirmed by envelope keyID metadata alone — the
// controller's own write-acks, never decrypt/read-back), the seal-time
// pub-generation validation, and the mixed-fleet W15 pin (flag off =
// byte-identical legacy behavior, zero staging artifacts). The DR-window
// terminator matrix lives in staging_dr_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const relayTestNamespace = "llm-relay"

// --- fakes -------------------------------------------------------------

type fakeProviderSource struct {
	mu        sync.Mutex
	providers []secrets.LLMProviderData
	err       error
	calls     int
}

func (f *fakeProviderSource) LLMProviders(_ context.Context, _, _ string) ([]secrets.LLMProviderData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.providers, f.err
}

type fakeRouterClient struct {
	mu       sync.Mutex
	mints    int
	rotates  int
	lastMint RelayMintRequest
	mintErr  error
	receipt  RelayRotateReceipt
	onRotate func(t *testing.T)
}

func (f *fakeRouterClient) MintToken(_ context.Context, req RelayMintRequest) (string, error) {
	f.mu.Lock()
	f.mints++
	n := f.mints
	f.lastMint = req
	f.mu.Unlock()
	if f.mintErr != nil {
		return "", f.mintErr
	}
	return fmt.Sprintf("lrt-fake-%d", n), nil
}

func (f *fakeRouterClient) RotateKeys(ctx context.Context) (RelayRotateReceipt, error) {
	f.mu.Lock()
	f.rotates++
	hook := f.onRotate
	f.mu.Unlock()
	if hook != nil {
		hook(nil)
	}
	return f.receipt, nil
}

// recordingRedactor records register/unregister calls (the §4.9 lifecycle
// assertions read these).
type recordingRedactor struct {
	mu           sync.Mutex
	registered   []string
	unregistered []string
}

func (f *recordingRedactor) RegisterStagedKey(id string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, id)
	return nil
}

func (f *recordingRedactor) UnregisterStagedKey(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, id)
}

// --- helpers -------------------------------------------------------------

func makeRelayWorkspace(name string) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			UID:         types.UID(name + "-uid"),
			Annotations: map[string]string{},
		},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-1"},
			Runtime: "python:3.11",
		},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
}

// makePubSecret generates a real keypair and returns the pub Secret plus
// the keypair payload (tests use the private half to prove envelopes sealed
// correctly against the pub — test-side resolution, never the controller's).
func makePubSecret(t *testing.T, generation int64) (*corev1.Secret, *secrets.HPKEKeyPairPayload) {
	t.Helper()
	kp, err := secrets.GenerateHPKEKeyPairPayload(generation)
	require.NoError(t, err)
	pub := kp.Public()
	data, err := pub.Marshal()
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: relayTestNamespace},
		Data:       map[string][]byte{secrets.RelayPubDataKey: data},
	}, kp
}

func stagingReconciler(t *testing.T, src *fakeProviderSource, router *fakeRouterClient, redactor *recordingRedactor, objs ...runtime.Object) *WorkspaceReconciler {
	t.Helper()
	r := reconcilerFor(t, objs...)
	if redactor == nil {
		redactor = &recordingRedactor{}
	}
	cfg, err := NewRelayStagingConfig("http://llm-relay-router.llm-relay.svc.cluster.local", relayTestNamespace, time.Hour, src, router, redactor, r.Client)
	require.NoError(t, err)
	r.RelayStaging = cfg
	r.Recorder = record.NewFakeRecorder(64)
	return r
}

func getSecret(t *testing.T, r *WorkspaceReconciler, ns, name string) *corev1.Secret {
	t.Helper()
	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, sec))
	return sec
}

func decodeHandoff(t *testing.T, data []byte) relayHandoff {
	t.Helper()
	var h relayHandoff
	require.NoError(t, json.Unmarshal(data, &h))
	return h
}

func conditionOf(ws *v1.Workspace, t v1.WorkspaceConditionType) *v1.WorkspaceCondition {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == t {
			return &ws.Status.Conditions[i]
		}
	}
	return nil
}

func openaiPD(slug, key string) secrets.LLMProviderData {
	return secrets.LLMProviderData{Kind: "openai", Slug: slug, APIKey: key, Models: []secrets.LLMModelConfig{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}}}
}

// --- stage leg -----------------------------------------------------------

func TestStaging_StagesEnvelopesAndHandoff(t *testing.T) {
	pubSec, kp := makePubSecret(t, 2)
	ws := makeRelayWorkspace("ws-a")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{
		openaiPD("openai", "sk-live-stage"),
		{Kind: "openai_compatible", Slug: "custom", APIKey: "org-key", BaseURL: "https://up.example.com/v1", Models: []secrets.LLMModelConfig{{ID: "m1"}}},
	}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	// Envelope Secrets in llm-relay, under the router informer contract.
	for _, slug := range []string{"openai", "custom"} {
		sec := getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-a", slug))
		assert.Equal(t, "ws-a", sec.Labels[secrets.RelayEnvWorkspaceLabel])
		assert.Equal(t, slug, sec.Labels[secrets.RelayEnvProviderLabel])
		envelope := string(sec.Data[secrets.RelayEnvDataKey])
		assert.True(t, strings.HasPrefix(envelope, "stg:v1:hpke:hpke-g2:"), "envelope for %s: %s", slug, envelope[:min(40, len(envelope))])
		var models []string
		require.NoError(t, json.Unmarshal(sec.Data[secrets.RelayEnvModelsKey], &models))
		if slug == "openai" {
			assert.Equal(t, []string{"gpt-4o", "gpt-4o-mini"}, models)
		} else {
			assert.Equal(t, []string{"m1"}, models)
		}
		// No raw key plaintext in the envelope Secret.
		assert.NotContains(t, string(sec.Data[secrets.RelayEnvModelsKey]), "sk-live-stage")
	}

	// The envelope actually seals the key against THIS pub (test-side
	// resolve — the controller never does this).
	resolver, err := secrets.NewHPKEStagingResolver(kp.PrivateKey, secrets.HPKEKeyID(2), nil)
	require.NoError(t, err)
	plain, err := resolver.Resolve(context.Background(), string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-a", "openai")).Data[secrets.RelayEnvDataKey]))
	require.NoError(t, err)
	assert.Equal(t, "sk-live-stage", string(plain))

	// Handoff Secret in the WORKSPACE namespace with the documented shape
	// (worklog D2).
	ho := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-a")).Data[relayHandoffDataKey])
	require.Len(t, ho.Providers, 2)
	assert.Equal(t, "http://llm-relay-router.llm-relay.svc.cluster.local", ho.RouterURL)
	assert.NotEmpty(t, ho.Revision)
	bySlug := map[string]relayHandoffEntry{}
	for _, p := range ho.Providers {
		bySlug[p.ProviderSlug] = p
	}
	oc := bySlug["openai"]
	assert.Equal(t, "lrt-fake-1", oc.Token)
	assert.Equal(t, "/w/ws-a/openai/v1", oc.RouterPath)
	assert.Equal(t, "https://api.openai.com/v1", oc.BaseURL, "first-party kind resolves the default upstream")
	assert.Equal(t, "hpke-g2", oc.KeyID)
	assert.Equal(t, []string{"gpt-4o", "gpt-4o-mini"}, oc.ModelAllowlist)
	_, err = time.Parse(time.RFC3339, oc.ExpiresAt)
	assert.NoError(t, err)
	cu := bySlug["custom"]
	assert.Equal(t, "https://up.example.com/v1", cu.BaseURL, "explicit BaseURL passes through")

	// Mint request shape (scope: workspace+slug+baseURL+allowlist+ttl+keyID).
	router.mu.Lock()
	mint := router.lastMint
	router.mu.Unlock()
	assert.Equal(t, "ws-a", mint.WorkspaceID)
	assert.Equal(t, int64(3600), mint.TTLSec)
	assert.Equal(t, "hpke-g2", mint.KeyID)

	// Conditions + annotations.
	require.NotNil(t, conditionOf(ws, v1.WorkspaceConditionCredentialsStaged))
	assert.Equal(t, "True", conditionOf(ws, v1.WorkspaceConditionCredentialsStaged).Status)
	assert.Equal(t, v1.ReasonCredentialsStaged, conditionOf(ws, v1.WorkspaceConditionCredentialsStaged).Reason)
	assert.Contains(t, conditionOf(ws, v1.WorkspaceConditionCredentialsStaged).Message, ho.Revision)
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialStale))
	assert.Equal(t, "2", ws.Annotations[relaySealedGenerationAnnotation])
	assert.Equal(t, ho.Revision, ws.Annotations[relayStagedRevisionAnnotation])

	// Mint key created (adopt-if-exists semantics with the router).
	mk := getSecret(t, r, relayTestNamespace, secrets.RelayMintKeyName)
	assert.NotEmpty(t, string(mk.Data[secrets.RelayMintKeyDataKey]))
}

// TestStaging_FlagOff_ZeroBehaviorChange is the mixed_fleet_batches W15
// pin's flag-off leg: with RelayStaging nil the pass is a no-op — no
// llm-relay Secrets, no handoff, no conditions — so the legacy raw-key
// batch path is untouched (byte-identical batches).
func TestStaging_FlagOff_ZeroBehaviorChange(t *testing.T) {
	ws := makeRelayWorkspace("ws-off")
	r := reconcilerFor(t, ws)
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "sk-raw-key")}}
	_ = src // deliberately NOT wired

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	list := &corev1.SecretList{}
	require.NoError(t, r.List(context.Background(), list))
	assert.Empty(t, list.Items, "flag off must not create any Secret")
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialsStaged))
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialStale))
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialRejected))
	assert.Empty(t, ws.Annotations)
}

// TestMixedFleetBatches: the same deployment serves the legacy bare-key
// batch to pre-flip semantics and the token handoff post-flip — the W15
// mixed-fleet pin. Flag on stages tokens WITHOUT the raw key ever entering
// the handoff; the legacy provider rendering from the same provider data
// still carries the raw key (that rendering is what pre-flip pods consume).
func TestMixedFleetBatches(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-mixed")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "sk-legacy-raw")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	hoRaw := getSecret(t, r, "default", handoffSecretName("ws-mixed")).Data[relayHandoffDataKey]
	assert.NotContains(t, string(hoRaw), "sk-legacy-raw", "the raw provider key must never enter the token handoff")
	assert.Contains(t, string(hoRaw), "lrt-fake-")

	envRaw := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-mixed", "openai")).Data[secrets.RelayEnvDataKey])
	assert.NotContains(t, envRaw, "sk-legacy-raw", "raw key only travels sealed")

	// Pre-flip semantics on the SAME provider data: the legacy config
	// rendering (what pre-flip pods consume — the batch's llm-provider
	// entries decrypt to exactly these providers, rendered by the
	// unchanged formatter) still carries the raw key. One deployment,
	// both fleets.
	legacy, err := opencode.FormatOpenCodeConfig(src.providers)
	require.NoError(t, err)
	assert.Contains(t, string(legacy), "sk-legacy-raw")
}

func TestStaging_NonStageableKindsSkippedAndSurfaced(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-kinds")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{
		openaiPD("openai", "k1"),
		{Kind: "bedrock", Slug: "aws", APIKey: "k2"},
		{Kind: "opencode", Slug: "zen", APIKey: "k3"},
	}}
	r := stagingReconciler(t, src, &fakeRouterClient{}, nil, pubSec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	sec := &corev1.Secret{}
	assert.True(t, r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-kinds", "openai")}, sec) == nil)
	assert.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-kinds", "aws")}, sec), "bedrock must not be staged")
	assert.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-kinds", "zen")}, sec), "zen must not be staged")

	ho := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-kinds")).Data[relayHandoffDataKey])
	assert.Len(t, ho.Providers, 1)

	staged := conditionOf(ws, v1.WorkspaceConditionCredentialsStaged)
	require.NotNil(t, staged)
	assert.Contains(t, staged.Message, "bedrock", "skipped kinds must be surfaced in the staged message")
	assert.Contains(t, staged.Message, "zen")
}

// --- unbind leg (revocation = Secret deletion, D2) -----------------------

func TestStaging_UnbindDeletesEnvelopeAndUnregisters(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-unbind")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1"), {Kind: "openai_compatible", Slug: "custom", APIKey: "k2", BaseURL: "https://up.example.com/v1"}}}
	red := &recordingRedactor{}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, red, pubSec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	oldEnvelope := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-unbind", "custom")).Data[secrets.RelayEnvDataKey])
	oldID := secrets.StagedKeyRedactionID(oldEnvelope)
	red.mu.Lock()
	red.registered, red.unregistered = nil, nil
	red.mu.Unlock()

	// Unbind: the provider disappears from the resolved set.
	src.mu.Lock()
	src.providers = []secrets.LLMProviderData{openaiPD("openai", "k1")}
	src.mu.Unlock()
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	sec := &corev1.Secret{}
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-unbind", "custom")}, sec)),
		"envelope Secret must be deleted (revocation = Secret deletion)")

	red.mu.Lock()
	unreg := append([]string(nil), red.unregistered...)
	red.mu.Unlock()
	assert.Contains(t, unreg, oldID, "the superseded envelope's redaction group must be unregistered in the same pass (§4.9)")

	ho := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-unbind")).Data[relayHandoffDataKey])
	assert.Len(t, ho.Providers, 1, "revoked provider leaves the handoff")

	stale := conditionOf(ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, "True", stale.Status)
	assert.Equal(t, v1.ReasonStaleRevoked, stale.Reason)

	// One-pass visibility (worklog D7): the next clean pass clears it.
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialStale))
}

// --- rotate leg (generation change → re-seal; §4.2) ----------------------

func TestStaging_RotateLeg_GenerationChangeReseals(t *testing.T) {
	pub2Sec, _ := makePubSecret(t, 2)
	pub3Sec, kp3 := makePubSecret(t, 3)
	ws := makeRelayWorkspace("ws-rot")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	red := &recordingRedactor{}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, red, pub2Sec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	oldEnvelope := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-rot", "openai")).Data[secrets.RelayEnvDataKey])
	oldID := secrets.StagedKeyRedactionID(oldEnvelope)

	// Router rotates: the pub Secret's generation moves 2 → 3.
	require.NoError(t, r.Update(context.Background(), pub3Sec))
	red.mu.Lock()
	red.unregistered = nil
	red.mu.Unlock()

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	// Completion confirmed by envelope keyID METADATA alone: the stored
	// envelope now names hpke-g3 (the controller's own write-ack is its
	// confirmation; this test READS to prove what was written).
	newEnvelope := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-rot", "openai")).Data[secrets.RelayEnvDataKey])
	_, keyID, err := secrets.InspectStagingEnvelope(newEnvelope)
	require.NoError(t, err)
	assert.Equal(t, "hpke-g3", keyID)

	// The new envelope resolves under the NEW keypair (seal correctness).
	resolver, err := secrets.NewHPKEStagingResolver(kp3.PrivateKey, "hpke-g3", nil)
	require.NoError(t, err)
	plain, err := resolver.Resolve(context.Background(), newEnvelope)
	require.NoError(t, err)
	assert.Equal(t, "k1", string(plain))

	// The old envelope no longer resolves under the new keypair.
	_, err = resolver.Resolve(context.Background(), oldEnvelope)
	assert.Error(t, err, "old-generation envelope must not resolve after rotation (dual-key window is router-side)")

	red.mu.Lock()
	unreg := append([]string(nil), red.unregistered...)
	red.mu.Unlock()
	assert.Contains(t, unreg, oldID, "re-seal unregisters the superseded envelope's group in the same pass (§4.9)")

	// Tokens re-minted under the new keyID.
	ho := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-rot")).Data[relayHandoffDataKey])
	require.Len(t, ho.Providers, 1)
	assert.Equal(t, "hpke-g3", ho.Providers[0].KeyID)
	assert.Equal(t, "3", ws.Annotations[relaySealedGenerationAnnotation])
}

// --- renewal --------------------------------------------------------------

func TestStaging_TokenRenewedPastHalfTTL(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-renew")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	base := time.Now()
	r.RelayStaging.Now = func() time.Time { return base }

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	router.mu.Lock()
	mintsAfterFirst := router.mints
	router.mu.Unlock()
	assert.Equal(t, 1, mintsAfterFirst)

	// Before TTL/2: no re-mint.
	r.RelayStaging.Now = func() time.Time { return base.Add(20 * time.Minute) }
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	router.mu.Lock()
	assert.Equal(t, 1, router.mints, "token within TTL/2 must be reused")
	router.mu.Unlock()

	// Past TTL/2 (TTL=1h): re-minted.
	r.RelayStaging.Now = func() time.Time { return base.Add(40 * time.Minute) }
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	router.mu.Lock()
	assert.Equal(t, 2, router.mints, "token past TTL/2 must be renewed")
	router.mu.Unlock()
}

// --- seal-time generation validation (§4.2) — pub_sealtime_generation_validated

func TestPubSealtimeGenerationValidated_ReceiptMismatchRefusesSeal(t *testing.T) {
	pub2Sec, _ := makePubSecret(t, 2)
	ws := makeRelayWorkspace("ws-pubval")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pub2Sec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	before := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-pubval", "openai")).Data[secrets.RelayEnvDataKey])

	// Escalation fires (corruption-class stale + intact lineage): the
	// rotate receipt names generation 3, but the pub Secret still says 2
	// (torn rotation) — NOTHING may be sealed against the receipt.
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{
		SpawnedRev:     ws.Annotations[relayStagedRevisionAnnotation],
		DegradedReason: "credential_stale",
	}
	receiptPub, err := pubKeyForGeneration(t, 3)
	require.NoError(t, err)
	router.receipt = RelayRotateReceipt{KeyID: "hpke-g3", Generation: 3, PublicKey: receiptPub}

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	after := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-pubval", "openai")).Data[secrets.RelayEnvDataKey])
	assert.Equal(t, before, after, "an envelope must NEVER be sealed against a pub whose generation ≠ the rotate receipt")

	stale := conditionOf(ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Equal(t, "True", stale.Status)
	assert.Equal(t, v1.ReasonStaleWrongPubLineage, stale.Reason)
}

func TestPubSealtimeGenerationValidated_ShapeInvalidPubFailsLoudly(t *testing.T) {
	badPub := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: relayTestNamespace},
		Data:       map[string][]byte{secrets.RelayPubDataKey: []byte("}{not-json")},
	}
	ws := makeRelayWorkspace("ws-badpub")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, badPub, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	sec := &corev1.Secret{}
	assert.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-badpub", "openai")}, sec),
		"shape-invalid pub bytes must fail seal-time parsing loudly — nothing sealed")
	staged := conditionOf(ws, v1.WorkspaceConditionCredentialsStaged)
	require.NotNil(t, staged)
	assert.Equal(t, "False", staged.Status)
	assert.Equal(t, v1.ReasonStalePubUnreadable, staged.Reason)
}

// --- construction pin (§4.9 amendment) ------------------------------------

func TestNewRelayStagingConfig_RefusesNilRedactor(t *testing.T) {
	_, err := NewRelayStagingConfig("http://router", "llm-relay", time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil redactor")
}

// TestNewRelayStagingConfig_RefusesNilAPIReader: the direct reader is
// REQUIRED at construction — there is deliberately no cached-client
// fallback, so the production wiring (mgr.GetAPIReader()) cannot be
// silently dropped (review r4 missing-test item).
func TestNewRelayStagingConfig_RefusesNilAPIReader(t *testing.T) {
	_, err := NewRelayStagingConfig("http://router", "llm-relay", time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil API reader")
}

// --- termination cleanup ----------------------------------------------------

func TestStaging_TerminationDeletesEnvelopes(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-term")
	ws.Finalizers = append(ws.Finalizers, WorkspaceFinalizer)
	ws.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	red := &recordingRedactor{}
	r := stagingReconciler(t, src, &fakeRouterClient{}, red, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	oldID := secrets.StagedKeyRedactionID(string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-term", "openai")).Data[secrets.RelayEnvDataKey]))
	red.mu.Lock()
	red.unregistered = nil
	red.mu.Unlock()

	r.relayDeleteEnvelopes(context.Background(), ws)

	sec := &corev1.Secret{}
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-term", "openai")}, sec)))
	red.mu.Lock()
	unreg := append([]string(nil), red.unregistered...)
	red.mu.Unlock()
	assert.Contains(t, unreg, oldID)
}

// --- helpers for tests -----------------------------------------------------

func pubKeyForGeneration(t *testing.T, generation int64) ([]byte, error) {
	t.Helper()
	kp, err := secrets.GenerateHPKEKeyPairPayload(generation)
	if err != nil {
		return nil, err
	}
	pub := kp.Public()
	return pub.PublicKey, nil
}

// TestStaging_SteadyStateZeroWrites: a no-change pass performs NO
// envelope/handoff writes and NO re-mints (the second pass's handoff bytes
// are identical — the revision did not move, no apiserver churn).
func TestStaging_SteadyStateZeroWrites(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-steady")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	base := time.Now()
	r.RelayStaging.Now = func() time.Time { return base }

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	ho1 := append([]byte(nil), getSecret(t, r, "default", handoffSecretName("ws-steady")).Data[relayHandoffDataKey]...)
	env1 := append([]byte(nil), getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-steady", "openai")).Data[secrets.RelayEnvDataKey]...)
	rv1 := ws.ResourceVersion

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	ho2 := getSecret(t, r, "default", handoffSecretName("ws-steady")).Data[relayHandoffDataKey]
	env2 := getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-steady", "openai")).Data[secrets.RelayEnvDataKey]
	assert.Equal(t, string(ho1), string(ho2), "handoff must not be rewritten at steady state")
	assert.Equal(t, string(env1), string(env2), "envelope must not be re-sealed at steady state")
	router.mu.Lock()
	assert.Equal(t, 1, router.mints, "no re-mint at steady state")
	router.mu.Unlock()
	assert.Equal(t, rv1, ws.ResourceVersion, "no workspace metadata/status write at steady state")
}

// TestStaging_ModelsChangeReseals: an allowlist edit (models list) on an
// unchanged keypair re-seals the envelope with the new catalog — the router
// serves GET /models from the envelope Secret, so a stale catalog would
// serve the wrong model list forever.
func TestStaging_ModelsChangeReseals(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-models")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	before := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-models", "openai")).Data[secrets.RelayEnvModelsKey])
	assert.Contains(t, before, "gpt-4o")

	src.mu.Lock()
	src.providers = []secrets.LLMProviderData{{
		Kind: "openai", Slug: "openai", APIKey: "k1",
		Models: []secrets.LLMModelConfig{{ID: "gpt-5"}},
	}}
	src.mu.Unlock()
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	after := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-models", "openai")).Data[secrets.RelayEnvModelsKey])
	assert.Contains(t, after, "gpt-5")
	assert.NotContains(t, after, "gpt-4o")
}

// TestStaging_CredentialValueRotationReseals (review r2, Correctness #2):
// a credential VALUE rotation under a stable slug/keyID/models must re-seal
// the envelope — the key digest in the staging bookkeeping closes the gap
// where the old key would otherwise stay live in llm-relay indefinitely.
func TestStaging_CredentialValueRotationReseals(t *testing.T) {
	pubSec, kp1 := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-valrot")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "sk-old-value")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	resolver1, err := secrets.NewHPKEStagingResolver(kp1.PrivateKey, "hpke-g1", nil)
	require.NoError(t, err)
	env1 := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-valrot", "openai")).Data[secrets.RelayEnvDataKey])
	plain1, err := resolver1.Resolve(context.Background(), env1)
	require.NoError(t, err)
	require.Equal(t, "sk-old-value", string(plain1), "pre-condition: old key sealed")

	// Rotate the credential VALUE: same slug, same kind, same models.
	src.mu.Lock()
	src.providers = []secrets.LLMProviderData{openaiPD("openai", "sk-new-value")}
	src.mu.Unlock()
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	env2 := string(getSecret(t, r, relayTestNamespace, envelopeSecretName("ws-valrot", "openai")).Data[secrets.RelayEnvDataKey])
	assert.NotEqual(t, env1, env2, "the envelope must be re-sealed on a credential value change")
	plain2, err := resolver1.Resolve(context.Background(), env2)
	require.NoError(t, err)
	assert.Equal(t, "sk-new-value", string(plain2), "the re-sealed envelope must carry the NEW key")
	assert.NotContains(t, env2, "sk-old-value")
}

// TestStaging_MintFailureRetainsUnexpiredToken (review r2, Correctness #3):
// a transient mint failure at the TTL/2 renewal boundary must NOT drop the
// still-valid cached token from the handoff — the handoff Secret is the
// only token store.
func TestStaging_MintFailureRetainsUnexpiredToken(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-mintfail")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	base := time.Now()
	r.RelayStaging.Now = func() time.Time { return base }
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	ho1 := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-mintfail")).Data[relayHandoffDataKey])
	require.Len(t, ho1.Providers, 1)
	firstToken := ho1.Providers[0].Token

	// Past TTL/2 the renewal mint fails (transient router error).
	r.RelayStaging.Now = func() time.Time { return base.Add(40 * time.Minute) }
	router.mu.Lock()
	router.mintErr = fmt.Errorf("router overloaded")
	router.mu.Unlock()
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	ho2 := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-mintfail")).Data[relayHandoffDataKey])
	require.Len(t, ho2.Providers, 1, "the still-valid token must be RETAINED, not dropped")
	assert.Equal(t, firstToken, ho2.Providers[0].Token)
	assert.Nil(t, conditionOf(ws, v1.WorkspaceConditionCredentialStale), "a retained unexpired token is not stale")

	// A fully expired token + mint failure IS dropped, loudly.
	r.RelayStaging.Now = func() time.Time { return base.Add(2 * time.Hour) }
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	ho3 := decodeHandoff(t, getSecret(t, r, "default", handoffSecretName("ws-mintfail")).Data[relayHandoffDataKey])
	assert.Empty(t, ho3.Providers, "an expired token with a failed mint must not linger")
	require.NotNil(t, conditionOf(ws, v1.WorkspaceConditionCredentialStale))
	assert.Equal(t, v1.ReasonStageFailed, conditionOf(ws, v1.WorkspaceConditionCredentialStale).Reason)
}

// failingReadClient simulates the production cache topology for the
// llm-relay namespace: a cached client that can serve NOTHING from
// llm-relay (out-of-cache-scope reads fail without an API call). All
// llm-relay reads must go through the injected API reader instead
// (review r3 finding 1).
type failingReadClient struct {
	client.Client
}

func (f failingReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Namespace == relayTestNamespace {
		return fmt.Errorf("unable to get %s: unknown namespace for the cache", key)
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

// TestStaging_LLMRelayReadsGoThroughAPIReader (review r3 finding 1): the
// staging pass must complete with a reconciler client whose llm-relay
// reads fail cache-style — pub/mint-key reads are served by the injected
// APIReader (mgr.GetAPIReader() in production), never by the cached
// client. This is the only topology the charted Role permits.
func TestStaging_LLMRelayReadsGoThroughAPIReader(t *testing.T) {
	pubSec, _ := makePubSecret(t, 2)
	ws := makeRelayWorkspace("ws-reader")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}

	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	// Wrap the reconciler client so llm-relay reads fail like the
	// production cache, then point the staging config's reader at the raw
	// client (the APIReader stand-in).
	raw := r.Client
	r.Client = failingReadClient{Client: raw}
	r.RelayStaging.APIReader = raw

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	// The pass completed THROUGH the direct reader: envelope + handoff +
	// staged condition all landed despite the cache client being unable
	// to serve any llm-relay read. (Verified via the RAW client — the
	// wrapped reconciler client legitimately cannot read llm-relay.)
	env := &corev1.Secret{}
	require.NoError(t, raw.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: envelopeSecretName("ws-reader", "openai")}, env))
	assert.True(t, strings.HasPrefix(string(env.Data[secrets.RelayEnvDataKey]), "stg:v1:hpke:hpke-g2:"))
	require.NotNil(t, conditionOf(ws, v1.WorkspaceConditionCredentialsStaged))
	assert.Equal(t, "True", conditionOf(ws, v1.WorkspaceConditionCredentialsStaged).Status)
}

// TestStaging_StagedProvidersAnnotationLossConverges (review r3 finding
// 2): losing ONLY the staged-providers bookkeeping (sealed generation and
// tokens intact) must re-seal once AND persist the rebuilt annotation —
// the next pass returns to zero writes. The persist gate covers the
// annotation.
func TestStaging_StagedProvidersAnnotationLossConverges(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-annloss")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	require.NotEmpty(t, ws.Annotations[relayStagedProvidersAnnotation])

	// Wipe ONLY the bookkeeping annotation (fresh token, sealed generation
	// intact) — STORE-LEVEL (via the client), so the test exercises the
	// real loss shape (etcd lost the value; the in-memory refetch must see
	// it gone). An in-memory-only delete is a false-green: pass-1's stored
	// write satisfies the stored assertion without the fix (review r4
	// finding 2).
	storedWiped := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-annloss", Namespace: "default"}, storedWiped))
	delete(storedWiped.Annotations, relayStagedProvidersAnnotation)
	require.NoError(t, r.Update(context.Background(), storedWiped))
	*ws = *storedWiped.DeepCopy()

	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	assert.NotEmpty(t, ws.Annotations[relayStagedProvidersAnnotation],
		"the rebuilt bookkeeping must be PERSISTED, not just recomputed in memory")
	assert.Contains(t, ws.Annotations[relayStagedProvidersAnnotation], "openai")

	// The stored workspace object carries it too (converged on the API
	// server, not only in-memory).
	stored := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-annloss", Namespace: "default"}, stored))
	assert.Contains(t, stored.Annotations[relayStagedProvidersAnnotation], "openai")

	// And the NEXT pass is a no-op (steady state restored).
	router.mu.Lock()
	mints := router.mints
	router.mu.Unlock()
	rv := ws.ResourceVersion
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	router.mu.Lock()
	assert.Equal(t, mints, router.mints, "converged pass must not re-mint")
	router.mu.Unlock()
	assert.Equal(t, rv, ws.ResourceVersion, "converged pass must not write the workspace")
}

// TestStaging_RevocationDeleteFailureRetriesAndReports (review r3 finding
// 4): a failed envelope delete is NOT reported as a completed revocation,
// keeps the envelope in the staged set, and propagates an error so the
// caller requeues.
func TestStaging_RevocationDeleteFailureRetriesAndReports(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-revfail")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	r := stagingReconciler(t, src, router, nil, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	// Unbind, and make llm-relay Secret deletes fail (RBAC regression
	// shape).
	src.mu.Lock()
	src.providers = nil
	src.mu.Unlock()
	original := r.Client
	r.Client = deleteFailingClient{Client: original, ns: relayTestNamespace}

	err := r.reconcileRelayStaging(context.Background(), ws)
	require.Error(t, err, "an incomplete revocation must propagate so the caller requeues")
	assert.Contains(t, err.Error(), "revocation incomplete")

	stale := conditionOf(ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Contains(t, stale.Message, "INCOMPLETE", "the message must distinguish pending from completed revocation")
	assert.Contains(t, stale.Message, "RETAINED")

	// The staged bookkeeping still carries the slug → the retry (delete
	// healed) completes the revocation on the next pass.
	assert.Contains(t, ws.Annotations[relayStagedProvidersAnnotation], "openai")
	r.Client = original
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	assert.NotContains(t, ws.Annotations[relayStagedProvidersAnnotation], "openai",
		"the retried pass completes the revocation")
}

// deleteFailingClient fails Secret deletes in the given namespace.
type deleteFailingClient struct {
	client.Client
	ns string
}

func (d deleteFailingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if d.ns != "" && obj.GetNamespace() == d.ns {
		if _, ok := obj.(*corev1.Secret); ok {
			return fmt.Errorf("secrets is forbidden: User cannot delete resource in namespace %s", d.ns)
		}
	}
	return d.Client.Delete(ctx, obj, opts...)
}

// TestStaging_ConcurrentPassesRaceFree (review r4 finding 1): two
// workspaces' staging passes run CONCURRENTLY under -race. The pending-
// revocation state must be pass-local (parameter-threaded), never on the
// shared reconciler — the deployment default is maxConcurrentReconciles=4.
func TestStaging_ConcurrentPassesRaceFree(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	router := &fakeRouterClient{}
	wsA := makeRelayWorkspace("ws-race-a")
	wsB := makeRelayWorkspace("ws-race-b")
	r := stagingReconciler(t, src, router, nil, pubSec, wsA, wsB)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, ws := range []*v1.Workspace{wsA, wsB} {
		wg.Add(1)
		go func(i int, ws *v1.Workspace) {
			defer wg.Done()
			errs[i] = r.reconcileRelayStaging(context.Background(), ws)
		}(i, ws)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
}

// TestStaging_AllDeletesFailedMessageHonest (review r5 finding 2): with
// EVERY revocation delete failed (pending non-empty, revoked empty) the
// condition message must not claim any envelope was deleted.
func TestStaging_AllDeletesFailedMessageHonest(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-allfail")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	r := stagingReconciler(t, src, &fakeRouterClient{}, nil, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))

	src.mu.Lock()
	src.providers = nil
	src.mu.Unlock()
	original := r.Client
	r.Client = deleteFailingClient{Client: original, ns: relayTestNamespace}
	err := r.reconcileRelayStaging(context.Background(), ws)
	require.Error(t, err)

	stale := conditionOf(ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	assert.Contains(t, stale.Message, "INCOMPLETE")
	assert.Contains(t, stale.Message, "RETAINED")
	assert.NotContains(t, stale.Message, "envelope deleted",
		"no envelope was deleted — the message must not say one was")
}

// TestStaging_MessageOnlyConditionChangePersists (review r5 finding 2):
// the condition dirty-signature carries the MESSAGE — a reason-preserving
// message-only change (the INCOMPLETE → healed revocation wording) must
// persist a status write instead of leaving stale wording on the API
// server for an extra pass.
func TestStaging_MessageOnlyConditionChangePersists(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ws := makeRelayWorkspace("ws-msg")
	src := &fakeProviderSource{providers: []secrets.LLMProviderData{openaiPD("openai", "k1")}}
	r := stagingReconciler(t, src, &fakeRouterClient{}, nil, pubSec, ws)
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{SpawnedRev: ws.Annotations[relayStagedRevisionAnnotation], DegradedReason: "token_expired"}
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	stale := conditionOf(ws, v1.WorkspaceConditionCredentialStale)
	require.NotNil(t, stale)
	require.Equal(t, v1.ReasonStaleTokenExpired, stale.Reason)

	// Same status+reason, DIFFERENT message (the degrade detail changes):
	// the stored condition must move to the new message in THIS pass.
	ws.Status.SecretsDelivery.DegradedReason = "token_expired_renewal_failed"
	require.NoError(t, r.reconcileRelayStaging(context.Background(), ws))
	stored := &v1.Workspace{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "ws-msg", Namespace: "default"}, stored))
	var got *v1.WorkspaceCondition
	for i := range stored.Status.Conditions {
		if stored.Status.Conditions[i].Type == v1.WorkspaceConditionCredentialStale {
			got = &stored.Status.Conditions[i]
		}
	}
	require.NotNil(t, got, "the stale condition must be persisted at all")
	assert.Equal(t, "staged material not applied by the running pod: token_expired_renewal_failed (a wait or fault rotation cannot repair)", got.Message,
		"a message-only change must persist")
}
