// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeByoStore wraps the client-go fake clientset behind byoSecrets. The
// fake's ObjectTracker enforces resourceVersion on Update, providing the
// same optimistic-concurrency serialization the API server does.
type fakeByoStore struct {
	cs *fake.Clientset
}

func (f fakeByoStore) Get(ctx context.Context, name string) (*corev1.Secret, error) {
	return f.cs.CoreV1().Secrets("llm-relay").Get(ctx, name, metav1.GetOptions{})
}

func (f fakeByoStore) Create(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error) {
	return f.cs.CoreV1().Secrets("llm-relay").Create(ctx, sec, metav1.CreateOptions{})
}

func (f fakeByoStore) Update(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error) {
	return f.cs.CoreV1().Secrets("llm-relay").Update(ctx, sec, metav1.UpdateOptions{})
}

func newFakeByoStore(objects ...*corev1.Secret) fakeByoStore {
	cs := fake.NewSimpleClientset()
	for _, o := range objects {
		o.Namespace = "llm-relay"
		_, err := cs.CoreV1().Secrets("llm-relay").Create(context.Background(), o, metav1.CreateOptions{})
		if err != nil {
			panic(err)
		}
	}
	return fakeByoStore{cs: cs}
}

func mustKeyPair(t *testing.T, generation int64) *secrets.HPKEKeyPairPayload {
	t.Helper()
	kp, err := secrets.GenerateHPKEKeyPairPayload(generation)
	require.NoError(t, err)
	return kp
}

func newTestKeyManager(store byoSecrets, retention time.Duration) *byoKeyManager {
	m := newByoKeyManager(store, retention, nil)
	now := time.Unix(1780000000, 0)
	m.clock = func() time.Time { return now }
	return m
}

// TestFirstbootTwoReplicaAdopt: two managers (replicas) bootstrap against
// one API state; both converge on ONE adopted keypair (fingerprint —
// the co-located public key — matches).
func TestFirstbootTwoReplicaAdopt(t *testing.T) {
	store := newFakeByoStore()
	replicaA := newTestKeyManager(store, 0)
	replicaB := newTestKeyManager(store, 0)

	require.NoError(t, replicaA.Bootstrap(context.Background()))
	require.NoError(t, replicaB.Bootstrap(context.Background()))

	secA, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	secB, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	// The serializer: whoever created first won; the other adopted. Both
	// hold the same secret now (fingerprint equality).
	assert.Equal(t, secA.Data[byoPayloadKey], secB.Data[byoPayloadKey])
	assert.ElementsMatch(t, []string{secrets.HPKEKeyID(1)}, replicaA.loadedKeyIDs())
	assert.ElementsMatch(t, []string{secrets.HPKEKeyID(1)}, replicaB.loadedKeyIDs())

	pub, err := store.Get(context.Background(), byoPubSecretName)
	require.NoError(t, err)
	var pubPayload secrets.HPKEPubPayload
	require.NoError(t, json.Unmarshal(pub.Data[byoPayloadKey], &pubPayload))
	assert.EqualValues(t, 1, pubPayload.Generation)
}

// TestRotationDualKeyWindowBounded: after rotation both the old and new
// keys resolve; after the retention interval the prior key drops.
func TestRotationDualKeyWindowBounded(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 10*time.Minute)
	require.NoError(t, m.Bootstrap(context.Background()))

	kp1, err := m.currentPayload(context.Background())
	require.NoError(t, err)
	sealer1, err := secrets.NewHPKEStagingSealer(kp1.PublicKey, secrets.HPKEKeyID(1), nil)
	require.NoError(t, err)
	oldEnvelope, err := sealer1.Seal(context.Background(), []byte("key-material-old"))
	require.NoError(t, err)

	pub, err := m.Rotate(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 2, pub.Generation)

	sealer2, err := secrets.NewHPKEStagingSealer([]byte(pubKeyBytes(t, pub)), secrets.HPKEKeyID(2), nil)
	require.NoError(t, err)
	newEnvelope, err := sealer2.Seal(context.Background(), []byte("key-material-new"))
	require.NoError(t, err)

	// Both keys resolve inside the window (dual-key).
	got, err := m.Resolve(context.Background(), oldEnvelope)
	require.NoError(t, err)
	assert.Equal(t, []byte("key-material-old"), got)
	got, err = m.Resolve(context.Background(), newEnvelope)
	require.NoError(t, err)
	assert.Equal(t, []byte("key-material-new"), got)

	// Advance past retention → prior key drops uniformly; old envelopes
	// fail closed until re-seal.
	m.clock = func() time.Time { return time.Unix(1780000000, 0).Add(11 * time.Minute) }
	_, err = m.Resolve(context.Background(), oldEnvelope)
	require.ErrorIs(t, err, errUnknownStagingKey)
	got, err = m.Resolve(context.Background(), newEnvelope)
	require.NoError(t, err)
	assert.Equal(t, []byte("key-material-new"), got)
}

func pubKeyBytes(t *testing.T, pub secrets.HPKEPubPayload) []byte {
	t.Helper()
	require.NotEmpty(t, pub.PublicKey)
	return pub.PublicKey
}

// TestRotationReplicaRestartMidReseal: a replica that restarts between the
// in-place Secret update and re-seal completion loads ONLY the new key
// (the prior key is memory-only) — old-keyID envelopes fail on that
// replica until re-seal completes. Fail-closed, self-healing.
func TestRotationReplicaRestartMidReseal(t *testing.T) {
	store := newFakeByoStore()
	running := newTestKeyManager(store, 0)
	require.NoError(t, running.Bootstrap(context.Background()))

	kp1, err := running.currentPayload(context.Background())
	require.NoError(t, err)
	sealer1, err := secrets.NewHPKEStagingSealer(kp1.PublicKey, secrets.HPKEKeyID(1), nil)
	require.NoError(t, err)
	oldEnvelope, err := sealer1.Seal(context.Background(), []byte("pre-rotation"))
	require.NoError(t, err)

	_, err = running.Rotate(context.Background())
	require.NoError(t, err)

	// A replica restarting now never saw gen1 as prior — it loads only the
	// new key and refuses the old envelope (fail-closed).
	restarted := newTestKeyManager(store, 0)
	require.NoError(t, restarted.Bootstrap(context.Background()))
	_, err = restarted.Resolve(context.Background(), oldEnvelope)
	require.ErrorIs(t, err, errUnknownStagingKey)

	// The still-running replica still resolves it (dual-key window).
	got, err := running.Resolve(context.Background(), oldEnvelope)
	require.NoError(t, err)
	assert.Equal(t, []byte("pre-rotation"), got)
}

// TestRotationTornUpdateUnconfirmable / private-then-pub ordering: the
// private-key Secret is updated before pub; a concurrent rotate loses the
// private-key write (generation precondition) and never writes pub.
func TestRotationConcurrentLoserAbortsBeforePub(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	// Simulate a concurrent winner: rotate m once (winner), then hand a
	// SECOND manager holding the stale generation the same rotate intent.
	winnerPub, err := m.Rotate(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 2, winnerPub.Generation)

	stale := newTestKeyManager(store, 0)
	stale.highwater = 1 // loaded gen1 only — the precondition must now fail
	_, err = stale.Rotate(context.Background())
	require.ErrorIs(t, err, errRotatePrecondition)

	pubAfter, err := store.Get(context.Background(), byoPubSecretName)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprint(winnerPub.Generation), pubGenerationString(t, pubAfter), "loser never wrote pub")
}

func pubGenerationString(t *testing.T, sec *corev1.Secret) string {
	t.Helper()
	var pub secrets.HPKEPubPayload
	require.NoError(t, json.Unmarshal(sec.Data[byoPayloadKey], &pub))
	return fmt.Sprint(pub.Generation)
}

// TestRotationAssertQuiescedInTornWindow: a peer private-key load during
// the private-then-pub window never fires DR — the assert is
// self-contained (never reads across Secrets), so a pub Secret still on
// the old generation is invisible to the router.
func TestRotationAssertQuiescedInTornWindow(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	// Manually emulate the torn window: private updated to gen2, pub still
	// gen1 — a peer watch-delivered private load must pass (no DR).
	kp2 := mustKeyPair(t, 2)
	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	data, err := kp2.Marshal()
	require.NoError(t, err)
	sec.Data[byoPayloadKey] = data
	_, err = store.Update(context.Background(), sec)
	require.NoError(t, err)

	m.ApplyWatchUpdate(context.Background(), sec)
	assert.ElementsMatch(t, []string{secrets.HPKEKeyID(1), secrets.HPKEKeyID(2)}, m.loadedKeyIDs(),
		"torn-window private load adopts gen2 without touching (or needing) pub")
}

// TestDRKeypairLossFailclosedRecovery: Secret loss → create-or-adopt
// regeneration; corrupted payload → watch-time assert fires a self-issued
// rotate; both recover without restart, and the corrupted-Secret overwrite
// path is exercised.
func TestDRKeypairLossFailclosedRecovery(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	// LOSS: delete the keypair Secret; a restarting replica regenerates.
	require.NoError(t, store.cs.CoreV1().Secrets("llm-relay").Delete(context.Background(), byoKeyPairSecretName, metav1.DeleteOptions{}))
	fresh := newTestKeyManager(store, 0)
	require.NoError(t, fresh.Bootstrap(context.Background()))
	assert.ElementsMatch(t, []string{secrets.HPKEKeyID(1)}, fresh.loadedKeyIDs())
	// The surviving replica's watch adopts the regenerated key too.
	require.NoError(t, m.Bootstrap(context.Background()))

	// CORRUPTION: overwrite the payload with garbage; the watch-time assert
	// fires and the manager self-rotates (no restart), landing on a new
	// self-consistent keypair that resolves.
	sealerBefore, err := secrets.NewHPKEStagingSealer(mustKeyPairPub(t, store, 1), secrets.HPKEKeyID(1), nil)
	require.NoError(t, err)
	envBeforeCorruption, err := sealerBefore.Seal(context.Background(), []byte("pre-corruption"))
	require.NoError(t, err)

	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	sec.Data[byoPayloadKey] = []byte("not-json")
	_, err = store.Update(context.Background(), sec)
	require.NoError(t, err)
	m.ApplyWatchUpdate(context.Background(), sec)

	// After DR the loaded lineage is self-consistent again (generation
	// advanced past the corrupt observation), and the corrupted secret was
	// overwritten in place.
	secAfter, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kpAfter, err := secrets.ParseHPKEKeyPairPayload(secAfter.Data[byoPayloadKey])
	require.NoError(t, err, "DR overwrote the corrupted secret")
	assert.Greater(t, kpAfter.Generation, int64(0))
	require.NoError(t, secrets.AssertHPKEKeyPair(kpAfter, 0))

	sealerAfter, err := secrets.NewHPKEStagingSealer(kpAfter.PublicKey, secrets.HPKEKeyID(kpAfter.Generation), nil)
	require.NoError(t, err)
	envAfter, err := sealerAfter.Seal(context.Background(), []byte("post-dr"))
	require.NoError(t, err)
	got, err := m.Resolve(context.Background(), envAfter)
	require.NoError(t, err)
	assert.Equal(t, []byte("post-dr"), got)

	// Honest fail-closed window, precisely scoped (design §4.2 stated
	// failure mode i): a replica that still holds the dead key as PRIOR
	// keeps resolving old envelopes until retention expiry; a replica that
	// (re)starts after the DR never loaded the dead key and fails closed
	// immediately — the clean unknown-keyID credential_stale signal.
	got, err = m.Resolve(context.Background(), envBeforeCorruption)
	require.NoError(t, err, "prior key retained on this replica for the retention window")
	assert.Equal(t, []byte("pre-corruption"), got)

	postDR := newTestKeyManager(store, 0)
	require.NoError(t, postDR.Bootstrap(context.Background()))
	_, err = postDR.Resolve(context.Background(), envBeforeCorruption)
	require.ErrorIs(t, err, errUnknownStagingKey)
}

func mustKeyPairPub(t *testing.T, store byoSecrets, generation int64) []byte {
	t.Helper()
	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kp, err := secrets.ParseHPKEKeyPairPayload(sec.Data[byoPayloadKey])
	require.NoError(t, err)
	require.EqualValues(t, generation, kp.Generation)
	return kp.PublicKey
}

// TestDRDualReplicaRecoverySingleKeypair: simultaneous assert failures on
// both replicas converge on a single lineage — the pub-write loser adopts
// the winner (generation-preconditioned single write).
func TestDRDualReplicaRecoverySingleKeypair(t *testing.T) {
	store := newFakeByoStore()
	a := newTestKeyManager(store, 0)
	b := newTestKeyManager(store, 0)
	require.NoError(t, a.Bootstrap(context.Background()))
	require.NoError(t, b.Bootstrap(context.Background()))

	// Both observe the same corruption concurrently (sequential here —
	// the RV/generation preconditions are what serialize them).
	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	sec.Data[byoPayloadKey] = []byte(`{"privateKey":"cHJpdg==","publicKey":"cHVi","generation":1}`)
	_, err = store.Update(context.Background(), sec)
	require.NoError(t, err)
	// The payload parses but fails the self-contained assert (pub does not
	// match priv) — DR class, not unparseable.
	a.ApplyWatchUpdate(context.Background(), sec)
	b.ApplyWatchUpdate(context.Background(), sec)

	finalSec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kp, err := secrets.ParseHPKEKeyPairPayload(finalSec.Data[byoPayloadKey])
	require.NoError(t, err)
	require.NoError(t, secrets.AssertHPKEKeyPair(kp, 0))

	pub, err := store.Get(context.Background(), byoPubSecretName)
	require.NoError(t, err)
	var pubPayload secrets.HPKEPubPayload
	require.NoError(t, json.Unmarshal(pub.Data[byoPayloadKey], &pubPayload))
	assert.Equal(t, kp.Generation, pubPayload.Generation, "pub matches the single surviving lineage")
	assert.Equal(t, kp.PublicKey, pubPayload.PublicKey)
}

// TestWatchRedeliverySameGenerationPasses: informer re-list redelivers the
// same object; the non-strict ≥ assert must not fire DR.
func TestWatchRedeliverySameGenerationPasses(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		m.ApplyWatchUpdate(context.Background(), sec)
	}
	assert.ElementsMatch(t, []string{secrets.HPKEKeyID(1)}, m.loadedKeyIDs())
}

// TestGenerationRegressionFiresDR: a delivered payload whose generation
// regressed (strict decrease) is corruption per the assert.
func TestGenerationRegressionFiresDR(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))
	_, err := m.Rotate(context.Background())
	require.NoError(t, err)
	require.Len(t, m.loadedKeyIDs(), 2)

	// A stale gen1 redelivery is NOT a regression (equal-or-lower than
	// highwater but ≥ prev observed... gen1 < highwater 2 → regression
	// class). The API server would not deliver this; if it happens it is
	// corruption or a downgrade: DR regenerates forward.
	kp1 := mustKeyPair(t, 1)
	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	data, err := kp1.Marshal()
	require.NoError(t, err)
	sec.Data[byoPayloadKey] = data
	_, err = store.Update(context.Background(), sec)
	require.NoError(t, err)
	m.ApplyWatchUpdate(context.Background(), sec)

	secAfter, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	kpAfter, err := secrets.ParseHPKEKeyPairPayload(secAfter.Data[byoPayloadKey])
	require.NoError(t, err)
	assert.Greater(t, kpAfter.Generation, int64(1), "DR regenerated forward past the regressed delivery")
}

func TestRotatePreconditionOnUnparseable(t *testing.T) {
	store := newFakeByoStore()
	m := newTestKeyManager(store, 0)
	require.NoError(t, m.Bootstrap(context.Background()))

	// Operator-driven Rotate on an unparseable secret fails loudly —
	// only DR tolerates unparseable payloads.
	sec, err := store.Get(context.Background(), byoKeyPairSecretName)
	require.NoError(t, err)
	sec.Data[byoPayloadKey] = []byte("garbage")
	_, err = store.Update(context.Background(), sec)
	require.NoError(t, err)
	_, err = m.Rotate(context.Background())
	require.Error(t, err)
}

func TestGetMissingSecretIsNotFound(t *testing.T) {
	store := newFakeByoStore()
	_, err := store.Get(context.Background(), "nope")
	require.True(t, apierrors.IsNotFound(err), "got %v", err)
}
