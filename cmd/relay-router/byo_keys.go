// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const (
	byoKeyPairSecretName = secrets.RelayKeyPairSecretName
	byoPubSecretName     = secrets.RelayPubSecretName
	byoPayloadKey        = "payload"
	byoDefaultRetention  = 10 * time.Minute
)

// byoSecrets is the narrow Secret surface the keypair machinery needs.
// Production wraps a CoreV1 client scoped to llm-relay; tests wrap the
// client-go fake. NOTE: the fake does NOT enforce resourceVersion on
// Update — concurrent-writer serialization is pinned via the generation
// precondition; the real API server additionally enforces resourceVersion.
type byoSecrets interface {
	Get(ctx context.Context, name string) (*corev1.Secret, error)
	Create(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error)
	Update(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error)
}

type coreV1Secrets struct {
	client interface {
		Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Secret, error)
		Create(ctx context.Context, secret *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
		Update(ctx context.Context, secret *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error)
	}
}

func (c coreV1Secrets) Get(ctx context.Context, name string) (*corev1.Secret, error) {
	return c.client.Get(ctx, name, metav1.GetOptions{})
}

func (c coreV1Secrets) Create(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error) {
	return c.client.Create(ctx, sec, metav1.CreateOptions{})
}

func (c coreV1Secrets) Update(ctx context.Context, sec *corev1.Secret) (*corev1.Secret, error) {
	return c.client.Update(ctx, sec, metav1.UpdateOptions{})
}

var (
	errUnknownStagingKey  = errors.New("no resolver for envelope keyID (pre-re-seal, post-retention, or rotated out)")
	errRotatePrecondition = errors.New("rotate precondition failed")
)

type errKeypairCorrupt struct {
	generation int64
	inner      error
}

func (e errKeypairCorrupt) Error() string {
	return fmt.Sprintf("hpke keypair secret corrupt at generation %d: %v", e.generation, e.inner)
}

// byoKeyManager owns the router side of the HPKE keypair machinery
// (design 0058 §4.2): create-or-adopt first boot, watch-driven key loading
// with the self-contained assert on every load, the dual-key resolve window
// with time-bounded prior-key retention, controller-requested rotation, and
// disaster recovery via self-issued rotate.
//
// Replica symmetry: no replica holds privately-generated authoritative
// state — every load comes from the Secrets and re-runs the assert; the API
// server's create/update semantics (resourceVersion) serialize concurrent
// rotations and DR attempts onto a single lineage. Only the replica whose
// private-key update wins writes the pub Secret; the loser aborts before
// pub and re-adopts the winner's payload on its next watch delivery.
type byoKeyManager struct {
	store     byoSecrets
	retention time.Duration
	clock     func() time.Time
	redaction secrets.StagedKeyRedactor

	mu        sync.RWMutex
	resolvers map[string]*secrets.HPKEStagingResolver // keyID → resolver
	priorDrop map[string]time.Time                    // keyID → drop deadline (prior keys only)
	highwater int64                                   // highest generation loaded
}

func newByoKeyManager(store byoSecrets, retention time.Duration, redaction secrets.StagedKeyRedactor) *byoKeyManager {
	if retention <= 0 {
		retention = byoDefaultRetention
	}
	return &byoKeyManager{
		store:     store,
		retention: retention,
		clock:     time.Now,
		redaction: redaction,
		resolvers: map[string]*secrets.HPKEStagingResolver{},
		priorDrop: map[string]time.Time{},
	}
}

// Bootstrap is the create-or-adopt first boot: attempt to create the
// keypair Secret with a freshly generated keypair; on AlreadyExists read
// and adopt the existing key, run the assert, and heal a missing pub
// Secret. Both replicas converge on one keypair — the API server's create
// semantics are the serializer; there is no leader election. A corrupt
// adopted Secret triggers the same self-issued rotate the watch path uses,
// so a running fleet converges on bounded recovery without a restart.
func (m *byoKeyManager) Bootstrap(ctx context.Context) error {
	// Seed from the loaded high-water mark, not a constant: a RUNNING
	// replica recovering from keypair-Secret loss regenerates at N+1,
	// keeping keyIDs strictly monotonic (dead-key envelopes keep failing
	// as the clean unknown-keyID signal; pub never rewinds). A fresh
	// replica (highwater 0) seeds at generation 1.
	kp, err := secrets.GenerateHPKEKeyPairPayload(m.loadedGeneration() + 1)
	if err != nil {
		return err
	}
	created, err := m.createKeyPairSecret(ctx, kp)
	if err == nil {
		if err := m.applyLoaded(created); err != nil {
			return err
		}
		return m.publishPub(ctx, kp)
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating hpke keypair secret: %w", err)
	}

	existing, err := m.store.Get(ctx, byoKeyPairSecretName)
	if err != nil {
		return fmt.Errorf("adopting hpke keypair secret: %w", err)
	}
	if err := m.applyLoadedOrRecover(ctx, existing); err != nil {
		return err
	}
	if _, err := m.store.Get(ctx, byoPubSecretName); apierrors.IsNotFound(err) {
		payload, perr := m.currentPayload(ctx)
		if perr != nil {
			return fmt.Errorf("healing pub secret: %w", perr)
		}
		return m.publishPub(ctx, payload)
	}
	return nil
}

func (m *byoKeyManager) currentPayload(ctx context.Context) (*secrets.HPKEKeyPairPayload, error) {
	sec, err := m.store.Get(ctx, byoKeyPairSecretName)
	if err != nil {
		return nil, err
	}
	return secrets.ParseHPKEKeyPairPayload(sec.Data[byoPayloadKey])
}

func (m *byoKeyManager) createKeyPairSecret(ctx context.Context, kp *secrets.HPKEKeyPairPayload) (*corev1.Secret, error) {
	data, err := kp.Marshal()
	if err != nil {
		return nil, err
	}
	return m.store.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: byoKeyPairSecretName},
		Data:       map[string][]byte{byoPayloadKey: data},
	})
}

func (m *byoKeyManager) publishPub(ctx context.Context, kp *secrets.HPKEKeyPairPayload) error {
	pub := kp.Public()
	data, err := pub.Marshal()
	if err != nil {
		return err
	}
	existing, err := m.store.Get(ctx, byoPubSecretName)
	if apierrors.IsNotFound(err) {
		_, err = m.store.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: byoPubSecretName},
			Data:       map[string][]byte{byoPayloadKey: data},
		})
		if apierrors.IsAlreadyExists(err) {
			// A simultaneous cold-start peer won the create race: adopt its
			// Secret instead of crash-looping (create-or-adopt symmetry).
			existing, err = m.store.Get(ctx, byoPubSecretName)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else if err != nil {
		return err
	}
	if string(existing.Data[byoPayloadKey]) == string(data) {
		return nil
	}
	existing.Data[byoPayloadKey] = data
	_, err = m.store.Update(ctx, existing)
	return err
}

// ApplyWatchUpdate is the Secret-watch delivery path: parse, assert
// (self-contained, generation non-strict ≥ the high-water mark so informer
// re-lists pass), and load. An assert failure is keypair corruption: the
// manager performs a self-issued rotate bounded to the observed generation
// (DR).
func (m *byoKeyManager) ApplyWatchUpdate(ctx context.Context, sec *corev1.Secret) {
	if sec.Name != byoKeyPairSecretName {
		return
	}
	_ = m.applyLoadedOrRecover(ctx, sec)
}

func (m *byoKeyManager) applyLoadedOrRecover(ctx context.Context, sec *corev1.Secret) error {
	err := m.applyLoaded(sec)
	if err == nil {
		return nil
	}
	var corrupt errKeypairCorrupt
	if errors.As(err, &corrupt) {
		// nextGeneration keeps keyIDs strictly monotonic even when the
		// corrupt payload could not name its generation: a reused keyID
		// would make dead-key envelopes fail as AEAD auth errors instead
		// of the clean unknown-key credential_stale signal.
		next := corrupt.generation + 1
		if loaded := m.loadedGeneration() + 1; loaded > next {
			next = loaded
		}
		if _, rerr := m.rotateTo(ctx, next, corrupt.generation, true); rerr != nil {
			return fmt.Errorf("dr rotate: %w", rerr)
		}
		return nil
	}
	return err
}

func (m *byoKeyManager) applyLoaded(sec *corev1.Secret) error {
	raw, ok := sec.Data[byoPayloadKey]
	if !ok || len(raw) == 0 {
		return errKeypairCorrupt{generation: 0, inner: errors.New("missing payload")}
	}
	kp, err := secrets.ParseHPKEKeyPairPayload(raw)
	if err != nil {
		return errKeypairCorrupt{generation: 0, inner: err}
	}
	m.mu.RLock()
	prev := m.highwater
	m.mu.RUnlock()
	if err := secrets.AssertHPKEKeyPair(kp, prev); err != nil {
		return errKeypairCorrupt{generation: kp.Generation, inner: err}
	}

	keyID := secrets.HPKEKeyID(kp.Generation)
	resolver, err := secrets.NewHPKEStagingResolver(kp.PrivateKey, keyID, m.redaction)
	if err != nil {
		return errKeypairCorrupt{generation: kp.Generation, inner: err}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepExpiredLocked()
	if kp.Generation > m.highwater {
		// New generation observed: every other loaded key enters its
		// retention window (replica-local, uniform interval — no
		// cross-replica ack protocol, design §4.2).
		dropAt := m.clock().Add(m.retention)
		for id := range m.resolvers {
			if _, hasDeadline := m.priorDrop[id]; !hasDeadline {
				m.priorDrop[id] = dropAt
			}
		}
		m.highwater = kp.Generation
	}
	m.resolvers[keyID] = resolver
	return nil
}

func (m *byoKeyManager) sweepExpiredLocked() {
	now := m.clock()
	for id, dropAt := range m.priorDrop {
		if now.After(dropAt) {
			delete(m.resolvers, id)
			delete(m.priorDrop, id)
		}
	}
}

// Resolve dispatches an envelope to the resolver its keyID names — the
// current key or a prior key still inside its retention window.
func (m *byoKeyManager) Resolve(ctx context.Context, envelope string) ([]byte, error) {
	_, keyID, err := secrets.InspectStagingEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	resolver, ok := m.resolvers[keyID]
	dropAt, prior := m.priorDrop[keyID]
	expired := prior && m.clock().After(dropAt)
	m.mu.RUnlock()
	if !ok || expired {
		m.mu.Lock()
		m.sweepExpiredLocked()
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", errUnknownStagingKey, keyID)
	}
	return resolver.Resolve(ctx, envelope)
}

// Rotate serves the controller's POST /internal/v1/keys/rotate: generate
// the next generation, update the private-key Secret FIRST
// (generation-preconditioned: current generation must equal the locally
// loaded high-water mark, so a concurrent rotate or DR attempt loses the
// write and aborts before touching pub), then publish the pub Secret.
// Watch delivery loads the new key on every replica, each retaining its
// previously loaded key as prior.
func (m *byoKeyManager) Rotate(ctx context.Context) (secrets.HPKEPubPayload, error) {
	return m.rotateTo(ctx, m.loadedGeneration()+1, m.loadedGeneration(), false)
}

func (m *byoKeyManager) loadedGeneration() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.highwater
}

// rotateTo performs the generation-preconditioned two-Secret update.
// expectedCurrent is the generation the caller observed (the locally loaded
// high-water mark for operator rotation; the observed-failing generation
// for DR — design §4.2's "current generation == the generation I observed
// failing"), so a concurrent rotate/DR attempt that won the private-key
// write in between aborts this one before pub. DR passes
// tolerateUnparseable: an unparseable private-key Secret cannot yield its
// generation, so the resourceVersion precondition alone serializes
// concurrent recoveries — the single-lineage property.
func (m *byoKeyManager) rotateTo(ctx context.Context, nextGeneration, expectedCurrent int64, tolerateUnparseable bool) (secrets.HPKEPubPayload, error) {
	current, err := m.store.Get(ctx, byoKeyPairSecretName)
	if err != nil {
		return secrets.HPKEPubPayload{}, fmt.Errorf("rotate: reading keypair secret: %w", err)
	}
	existing, parseErr := secrets.ParseHPKEKeyPairPayload(current.Data[byoPayloadKey])
	if parseErr != nil {
		if !tolerateUnparseable {
			return secrets.HPKEPubPayload{}, fmt.Errorf("rotate: keypair secret unparseable: %w", parseErr)
		}
		existing = nil
	} else if existing.Generation != expectedCurrent {
		return secrets.HPKEPubPayload{}, fmt.Errorf("%w: secret generation %d, expected %d", errRotatePrecondition, existing.Generation, expectedCurrent)
	}

	kp, err := secrets.GenerateHPKEKeyPairPayload(nextGeneration)
	if err != nil {
		return secrets.HPKEPubPayload{}, err
	}
	data, err := kp.Marshal()
	if err != nil {
		return secrets.HPKEPubPayload{}, err
	}
	// Private-key Secret first — the torn-write pin. The update carries the
	// object's resourceVersion, so a concurrent writer loses here and
	// aborts before pub.
	current.Data[byoPayloadKey] = data
	if _, err := m.store.Update(ctx, current); err != nil {
		return secrets.HPKEPubPayload{}, fmt.Errorf("rotate: updating keypair secret: %w", err)
	}
	if err := m.publishPub(ctx, kp); err != nil {
		return secrets.HPKEPubPayload{}, fmt.Errorf("rotate: publishing pub secret: %w", err)
	}
	// Apply locally too — the watch redelivers the same object idempotently
	// (non-strict ≥ generation assert).
	if err := m.applyLoaded(current); err != nil {
		return secrets.HPKEPubPayload{}, err
	}
	return kp.Public(), nil
}

// loadedKeyIDs snapshots the loaded resolver keyIDs (tests + diagnostics).
func (m *byoKeyManager) loadedKeyIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.resolvers))
	for id := range m.resolvers {
		ids = append(ids, id)
	}
	return ids
}
