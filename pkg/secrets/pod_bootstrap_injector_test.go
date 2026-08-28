// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// pod_bootstrap_injector_test.go — unit tests for InjectSecretsForPodBootstrap.
//
// Contract under test ("if the pod exists, it has its secrets"): the
// bootstrap payload includes user-DEK bindings whenever the master
// RootKeyProvider can unwrap the owner's user_keys record — with NO
// dependence on jwt_sessions state. The former GetDEKForUser session
// walk is no longer consulted by this path at all.
//
// Test coverage:
//
//   - nil KeyService                        → degrades to sessionless
//   - KeyService without RootKeyProvider    → degrades to sessionless
//   - no user_keys record                   → degrades to sessionless
//     (owner has no DEK-encrypted secrets; nothing is missing)
//   - no jwt_sessions store wired           → DELIVERS user secrets
//   - empty jwt_sessions table              → DELIVERS user secrets
//   - only unwrappable jwt_sessions rows    → DELIVERS user secrets
//     (rotated-out signing keys are irrelevant to the server unwrap)
//   - happy path                            → DELIVERS user secrets
//
// The session-independence rows are the #1087 regression gates: a
// suspend/resume that outlives every jwt_sessions row must not strip
// the workspace's bound credentials.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bindGHTokenEnvSecret creates and binds a user-DEK env-secret (the
// exact class of secret that vanished on resume in the #1087 incident)
// to workspace ws-1 for user-1, using the authenticated session the
// fixture established.
func bindGHTokenEnvSecret(t *testing.T, svc *SecretService, sessionID string) {
	t.Helper()
	ctx := context.Background()
	s, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "gh-token",
		Type:     SecretTypeEnvSecret,
		Value:    "ghp_test_token_value",
		Metadata: json.RawMessage(`{"var_name":"GH_TOKEN"}`),
	})
	require.NoError(t, err, "CreateSecret must succeed under the fixture session")
	_, err = svc.SetBindings(ctx, "user-1", "ws-1", []string{s.ID})
	require.NoError(t, err, "SetBindings must bind the env-secret to ws-1")
}

// assertGHTokenDelivered asserts the payload contains the env-secret
// with its plaintext intact — the round-trip proof through the
// server-side DEK unwrap.
func assertGHTokenDelivered(t *testing.T, data []byte) {
	t.Helper()
	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))

	var found bool
	for _, item := range injected {
		if item.Type == SecretTypeEnvSecret && item.Name == "gh-token" {
			found = true
			assert.Equal(t, "ghp_test_token_value", item.Plaintext,
				"env-secret plaintext must survive round-trip through DEK unwrap")
		}
	}
	assert.True(t, found,
		"user-DEK env-secret MUST appear in bootstrap payload — a pod that exists has its secrets")
}

// assertSessionlessEmpty asserts the degrade shape: no error, empty
// payload for an empty workspace.
func assertSessionlessEmpty(t *testing.T, data []byte, err error) {
	t.Helper()
	require.NoError(t, err, "bootstrap degrade must never fail the call")
	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))
	assert.Empty(t, injected)
}

// TestInjectSecretsForPodBootstrap_NilKeyService_DegradesToSessionless
// asserts that when the SecretService was constructed with keys=nil (a
// legitimate test wiring), the pod-bootstrap path does not panic and
// returns the same payload InjectSessionlessSecrets would.
func TestInjectSecretsForPodBootstrap_NilKeyService_DegradesToSessionless(t *testing.T) {
	secretStore := newMockSecretStore()
	svc := NewSecretService(nil, secretStore)

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	assertSessionlessEmpty(t, data, err)
}

// TestInjectSecretsForPodBootstrap_NoRootProvider_DegradesToSessionless
// asserts the wiring guard: a KeyService whose RootKeyProvider was never
// set cannot unwrap server-side, so the call degrades cleanly instead of
// panicking or failing the boot.
func TestInjectSecretsForPodBootstrap_NoRootProvider_DegradesToSessionless(t *testing.T) {
	keySvc := NewKeyService(newMockKeyStore(), newMockDEKCache()) // no SetAPIKeyStore
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	assertSessionlessEmpty(t, data, err)
}

// TestInjectSecretsForPodBootstrap_NoUserKeyRecord_DegradesToSessionless
// asserts the "owner never created secrets" case: no user_keys row means
// no DEK-encrypted bindings exist, so the sessionless payload is already
// complete — the degrade is a no-op in content terms.
func TestInjectSecretsForPodBootstrap_NoUserKeyRecord_DegradesToSessionless(t *testing.T) {
	keySvc := NewKeyService(newMockKeyStore(), newMockDEKCache())
	keySvc.SetAPIKeyStore(nil, &recordingProvider{}) // rootKeyProvider wired, but no InitializeUserKeysServerKEK
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	assertSessionlessEmpty(t, data, err)
}

// TestInjectSecretsForPodBootstrap_NoJWTSessions_DeliversUserSecrets is
// the core #1087 regression gate: no JWTSessionStore wired at all
// (pre-Epic-56 shape, or a resume where every session row expired) —
// the bound env-secret still delivers via the server-side unwrap.
func TestInjectSecretsForPodBootstrap_NoJWTSessions_DeliversUserSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)
	// Deliberately NO SetJWTSessionStore / SetSigningKeyEnumerator.

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assertGHTokenDelivered(t, data)
}

// TestInjectSecretsForPodBootstrap_EmptyJWTSessionsTable_DeliversUserSecrets
// asserts delivery when the jwt_sessions table is wired but has zero
// rows for the user — the exact state a suspend/resume leaves when the
// owner's sessions TTL'd out mid-suspend.
func TestInjectSecretsForPodBootstrap_EmptyJWTSessionsTable_DeliversUserSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)
	svc.keys.SetJWTSessionStore(newMockJWTSessionStore()) // wired, empty
	svc.keys.SetSigningKeyEnumerator(&staticSigningKeys{keys: [][]byte{[]byte("test-signing-key")}})

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assertGHTokenDelivered(t, data)
}

// TestInjectSecretsForPodBootstrap_UnwrappableRows_DeliversUserSecrets
// asserts delivery when the only jwt_sessions rows are wrapped under
// signing keys outside the enumerator's retention window — under the old
// GetDEKForUser path this degraded to sessionless; the server-side
// unwrap must not care.
func TestInjectSecretsForPodBootstrap_UnwrappableRows_DeliversUserSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)

	jwtStore := newMockJWTSessionStore()
	svc.keys.SetJWTSessionStore(jwtStore)
	svc.keys.SetSigningKeyEnumerator(&staticSigningKeys{keys: [][]byte{[]byte("enumerator-knows-this")}})

	// Seed a durable row wrapped under a signing key the enumerator
	// does NOT know — hostile session state by construction.
	err := svc.keys.UnlockDEKWithSigningKey(context.Background(), "user-1", nil,
		"550e8400-e29b-41d4-a716-446655440001", time.Hour, []byte("rotated-out-signing-key"))
	require.NoError(t, err, "seeding the hostile jwt_sessions row must succeed")
	require.NotZero(t, jwtStore.WriteCount, "hostile jwt_sessions row must exist")

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assertGHTokenDelivered(t, data)
}

// TestInjectSecretsForPodBootstrap_HappyPath_UnwrapsUserDEKAndIncludesUserSecrets
// is the plain positive path: keys initialized, secret bound, no session
// machinery involved whatsoever.
func TestInjectSecretsForPodBootstrap_HappyPath_UnwrapsUserDEKAndIncludesUserSecrets(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err,
		"InjectSecretsForPodBootstrap must succeed via the server-side unwrap")
	assertGHTokenDelivered(t, data)
}

// TestInjectSecretsForPodBootstrap_LegacyRowHeal_DeliversAndRewraps is the
// 2026-08-28 incident as a unit test: the user_keys row is wrapped under a
// key the current provider cannot unwrap (the June-era legacy blob), but a
// live jwt_sessions row still carries the DEK. GetDEKServerSide must (a)
// recover the DEK from the session source, (b) deliver the bound secrets,
// and (c) re-wrap the row at the active version with verify-after-write —
// so the NEXT bootstrap decrypts directly and the heal is one-shot.
func TestInjectSecretsForPodBootstrap_LegacyRowHeal_DeliversAndRewraps(t *testing.T) {
	svc, _, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)

	// Wire a durable session row carrying the REAL DEK (mirrors what the
	// owner's Aug-02 login left behind on the incident user).
	jwtStore := newMockJWTSessionStore()
	svc.keys.SetJWTSessionStore(jwtStore)
	signingKey := []byte("legacy-era-signing-key")
	svc.keys.SetSigningKeyEnumerator(&staticSigningKeys{keys: [][]byte{signingKey}})
	legacyJTI := "550e8400-e29b-41d4-a716-446655440002"
	require.NoError(t, svc.keys.UnlockDEKWithSigningKey(context.Background(), "user-1", nil,
		legacyJTI, time.Hour, signingKey), "seeding the durable session row must succeed")

	// Corrupt the user_keys row into the legacy shape AND put a REAL crypto
	// provider behind the master seat. The fixture's recordingProvider is a
	// keyless XOR fake that "decrypts" anything — useless for failure
	// paths. The alien wrap comes from a different AES key, so the real
	// master provider's Decrypt genuinely fails (auth-tag mismatch), which
	// is the June-era-blob precondition.
	realProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xE1))
	require.NoError(t, err)
	alienProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xEE))
	require.NoError(t, err)
	realDEK, err := svc.keys.cache.GetDEK(context.Background(), legacyJTI)
	require.NoError(t, err)
	alienWrap, err := alienProv.Encrypt(context.Background(), realDEK)
	require.NoError(t, err)
	keyStore := newMockKeyStore()
	keyStore.CreateUserKey(context.Background(), &UserKeyRecord{
		UserID: "user-1", KeyVersion: 1, WrappedDEK: alienWrap, DEKSource: "server_kek",
	})
	svc.keys.store = keyStore
	svc.keys.SetAPIKeyStore(nil, realProv)

	// Sanity: the master provider genuinely cannot unwrap the alien wrap.
	rec, _ := keyStore.GetUserKey(context.Background(), "user-1")
	_, derr := svc.keys.rootKeyProvider.Decrypt(context.Background(), rec.WrappedDEK)
	require.Error(t, derr, "precondition: legacy row must be unwrappable by the master provider")

	// The call under test: degrade is NOT acceptable — the session source
	// must heal it.
	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assertGHTokenDelivered(t, data)

	// The row must now be re-wrapped under the MASTER provider at its
	// active version, and the re-wrap must round-trip (verify-after-write).
	healed, err := keyStore.GetUserKey(context.Background(), "user-1")
	require.NoError(t, err)
	roundTripped, rerr := svc.keys.rootKeyProvider.Decrypt(context.Background(), healed.WrappedDEK)
	require.NoError(t, rerr, "healed row must decrypt under the master provider")
	require.True(t, bytes.Equal(roundTripped, realDEK), "healed row must wrap the same DEK")
	require.Equal(t, ActiveVersionOf(svc.keys.rootKeyProvider), healed.KeyVersion,
		"healed row must carry the provider's active key version")

	// One-shot: a second bootstrap (cache cold for the session row) now
	// unwraps DIRECTLY — no session needed anymore.
	require.NoError(t, svc.keys.cache.EvictDEK(context.Background(), legacyJTI))
	data2, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assertGHTokenDelivered(t, data2)
}

// TestInjectSecretsForPodBootstrap_LegacyRowNoSession_DegradesAndAudits pins
// the loud-degrade contract: unwrappable row AND no session source → the
// sessionless batch returns, AND the failure is audited with the underlying
// error (the 2026-08-28 lesson — silence was the diagnosis cost).
func TestInjectSecretsForPodBootstrap_LegacyRowNoSession_DegradesAndAudits(t *testing.T) {
	svc, store, sessionID := setupSecretService(t)
	bindGHTokenEnvSecret(t, svc, sessionID)

	// Corrupt the row; wire NO jwt session store at all. Real crypto on the
	// master seat (see the heal test for why the XOR fake cannot fail).
	realProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xE2))
	require.NoError(t, err)
	alienProv, err := NewStaticKeyProvider(deterministicTestKey(t, 0xEF))
	require.NoError(t, err)
	garbage, err := alienProv.Encrypt(context.Background(), []byte("not-the-dek"))
	require.NoError(t, err)
	keyStore := newMockKeyStore()
	keyStore.CreateUserKey(context.Background(), &UserKeyRecord{
		UserID: "user-1", KeyVersion: 1, WrappedDEK: garbage, DEKSource: "server_kek",
	})
	svc.keys.store = keyStore
	svc.keys.SetAPIKeyStore(nil, realProv)

	store.mu.Lock()
	store.audit = nil
	store.mu.Unlock()

	data, err := svc.InjectSecretsForPodBootstrap(context.Background(), "user-1", "ws-1")
	assertSessionlessEmpty(t, data, err)

	store.mu.Lock()
	entries := append([]*AuditEntry(nil), store.audit...)
	store.mu.Unlock()

	found := false
	for _, e := range entries {
		if e.Action != "pod_bootstrap_dek_failed" {
			continue
		}
		found = true
		require.NotNil(t, e.WorkspaceID, "audit must name the workspace")
		var meta map[string]string
		require.NoError(t, json.Unmarshal(e.Metadata, &meta))
		require.Contains(t, meta["error"], "server-kek unwrap DEK",
			"audit must carry the underlying unwrap error")
	}
	assert.True(t, found,
		"the degrade MUST be audited — silent degrade is a review-failing regression (0052 Phase 1)")
}

// deterministicTestKey derives a stable 32-byte test key.
func deterministicTestKey(t *testing.T, seed byte) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed
	}
	return k
}
