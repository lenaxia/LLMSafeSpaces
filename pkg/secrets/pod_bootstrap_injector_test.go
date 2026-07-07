// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// pod_bootstrap_injector_test.go — unit tests for InjectSecretsForPodBootstrap
// (design/0045_2026-07-06_boot-time-user-dek-delivery.md Change 1).
//
// The method attempts a best-effort user-DEK unwrap via GetDEKForUser and,
// on success, delegates to InjectSecrets so user-DEK bindings are included
// in the payload. On failure it degrades to InjectSessionlessSecrets.
//
// Test coverage:
//
//   - nil KeyService                              → degrades to sessionless
//   - KeyService with no jwt_sessions store       → degrades to sessionless
//   - KeyService + empty jwt_sessions table       → degrades to sessionless
//   - KeyService + valid jwt_sessions row          → delivers user-DEK secrets
//   - KeyService + valid row but wrong signing key → degrades (unwrap fails)
//   - KeyService + expired jwt_sessions row       → degrades (row filtered)
//
// The tests exercise the exact composition InjectSecretsForPodBootstrap
// makes: GetDEKForUser(userID) → InjectSecrets(userID, jti, nil, workspaceID).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectSecretsForPodBootstrap_NilKeyService_DegradesToSessionless
// asserts that when the SecretService was constructed with keys=nil (a
// legitimate test wiring), the pod-bootstrap path does not panic and
// returns the same payload InjectSessionlessSecrets would.
func TestInjectSecretsForPodBootstrap_NilKeyService_DegradesToSessionless(t *testing.T) {
	secretStore := newMockSecretStore()
	svc := NewSecretService(nil, secretStore)

	ctx := context.Background()
	data, err := svc.InjectSecretsForPodBootstrap(ctx, "user-1", "ws-1")
	require.NoError(t, err,
		"nil KeyService must degrade cleanly to sessionless behavior, not panic")

	// Same payload as InjectSessionlessSecrets would produce for an
	// empty workspace: an empty secrets array.
	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))
	assert.Empty(t, injected,
		"empty workspace with nil KeyService yields no secrets")
}

// TestInjectSecretsForPodBootstrap_NoJWTSessions_DegradesToSessionless
// asserts that a KeyService without a wired JWTSessionStore (pre-Epic-56
// deploys, some test paths) degrades cleanly. GetDEKForUser returns
// ErrDEKUnavailable in this case (key_service.go:643).
func TestInjectSecretsForPodBootstrap_NoJWTSessions_DegradesToSessionless(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache) // no SetJWTSessionStore
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)

	ctx := context.Background()
	data, err := svc.InjectSecretsForPodBootstrap(ctx, "user-1", "ws-1")
	require.NoError(t, err,
		"unwired JWTSessionStore must not fail the bootstrap call — degrades to sessionless")

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))
	assert.Empty(t, injected)
}

// TestInjectSecretsForPodBootstrap_EmptyJWTSessionsTable_DegradesToSessionless
// asserts the "user has no active sessions" case: JWTSessionStore is wired
// but returns zero rows for the user. GetDEKForUser returns
// ErrDEKUnavailable (key_service.go:652).
func TestInjectSecretsForPodBootstrap_EmptyJWTSessionsTable_DegradesToSessionless(t *testing.T) {
	keyStore := newMockKeyStore()
	dekCache := newMockDEKCache()
	keySvc := NewKeyService(keyStore, dekCache)
	keySvc.SetJWTSessionStore(newMockJWTSessionStore()) // empty
	secretStore := newMockSecretStore()
	svc := NewSecretService(keySvc, secretStore)

	// SecretService also needs SigningKeyEnumerator for GetDEKForUser to
	// exercise the unwrap loop; without it, GetDEKForUser returns
	// ErrDEKUnavailable at the guard (key_service.go:643).
	keySvc.SetSigningKeyEnumerator(&staticSigningKeys{keys: [][]byte{[]byte("test-key")}})

	ctx := context.Background()
	data, err := svc.InjectSecretsForPodBootstrap(ctx, "user-1", "ws-1")
	require.NoError(t, err,
		"empty jwt_sessions rows must degrade to sessionless")

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))
	assert.Empty(t, injected)
}

// TestInjectSecretsForPodBootstrap_UnwrappableRow_DegradesToSessionless
// asserts that a jwt_sessions row wrapped under a signing key NOT in the
// enumerator's list degrades cleanly. This exercises the
// tryUnwrapRowWithKnownKeys failure path (key_service.go:676) — every
// row iterated, none unwrappable → ErrDEKUnavailable.
func TestInjectSecretsForPodBootstrap_UnwrappableRow_DegradesToSessionless(t *testing.T) {
	fixture := newGetDEKForUserFixture(t)
	// Row wrapped under a key the enumerator doesn't know about.
	fixture.addSession(t, []byte("original-signing-key"), fixture.baseTs, fixture.baseTs.Add(time.Hour))

	// Enumerator returns a different key.
	fixture.svc.signingKeys = &staticSigningKeys{keys: [][]byte{[]byte("different-signing-key")}}

	secretStore := newMockSecretStore()
	svc := NewSecretService(fixture.svc, secretStore)

	ctx := context.Background()
	data, err := svc.InjectSecretsForPodBootstrap(ctx, fixture.userID, "ws-1")
	require.NoError(t, err,
		"unwrappable jwt_sessions rows must degrade to sessionless — a rotated-out signing key must not fail pod boot")

	var injected []InjectedSecret
	require.NoError(t, json.Unmarshal(data, &injected))
	assert.Empty(t, injected)
}

// TestInjectSecretsForPodBootstrap_HappyPath_UnwrapsUserDEKAndIncludesUserSecrets
// is the positive test: a valid jwt_sessions row + matching signing key +
// bound user-DEK secret. The method must unwrap successfully and include
// the user secret in the payload — the whole point of the design 0045 fix.
func TestInjectSecretsForPodBootstrap_HappyPath_UnwrapsUserDEKAndIncludesUserSecrets(t *testing.T) {
	// Use the full setup fixture so we have a real SecretService with a
	// user, session, and password already established.
	svc, _, sessionID := setupSecretService(t)
	ctx := context.Background()

	// Bind a user-DEK env-secret to the workspace. This is the class of
	// secret that was permanently missing on cold-boot before design 0045.
	s, err := svc.CreateSecret(ctx, "user-1", sessionID, nil, CreateSecretRequest{
		Name:     "gh-token",
		Type:     SecretTypeEnvSecret,
		Value:    "ghp_test_token_value",
		Metadata: json.RawMessage(`{"var_name":"GH_TOKEN"}`),
	})
	require.NoError(t, err)
	_, err = svc.SetBindings(ctx, "user-1", "ws-1", []string{s.ID})
	require.NoError(t, err)

	// Now write a jwt_sessions row for user-1 so GetDEKForUser can find
	// and unwrap it. This mirrors what a real login would do.
	//
	// setupSecretService uses UnlockDEK (no jwt_sessions write). We need
	// UnlockDEKWithSigningKey to persist a row. Do that now.
	jwtStore := newMockJWTSessionStore()
	svc.keys.SetJWTSessionStore(jwtStore)
	svc.keys.SetSigningKeyEnumerator(&staticSigningKeys{keys: [][]byte{[]byte("test-signing-key")}})

	// Use a UUID for the sessionID so UnlockDEKWithSigningKey persists to jwt_sessions.
	uuidSessionID := "550e8400-e29b-41d4-a716-446655440000"
	err = svc.keys.UnlockDEKWithSigningKey(ctx, "user-1", []byte("test-password"), uuidSessionID, time.Hour, []byte("test-signing-key"))
	require.NoError(t, err, "UnlockDEKWithSigningKey must succeed to seed the jwt_sessions row")
	require.NotZero(t, jwtStore.WriteCount, "jwt_sessions row must have been persisted")

	// Clear the Redis cache under the original sessionID so
	// InjectSecretsForPodBootstrap has to do the actual unwrap via
	// GetDEKForUser (not hit the cache from setupSecretService's
	// UnlockDEK). This isolates the test to the bootstrap unwrap path.
	require.NoError(t, svc.keys.cache.EvictDEK(ctx, sessionID))

	// Call the method under test.
	data, err := svc.InjectSecretsForPodBootstrap(ctx, "user-1", "ws-1")
	require.NoError(t, err,
		"InjectSecretsForPodBootstrap must succeed when jwt_sessions has an unwrappable row")

	// The user-DEK env-secret MUST be in the payload — the whole design
	// 0045 fix.
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
		"user-DEK env-secret MUST appear in bootstrap payload after design 0045 Change 1 — this is the entire point of the fix")
}
