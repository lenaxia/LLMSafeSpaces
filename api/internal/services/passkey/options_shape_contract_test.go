// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestOptionsShape_ContractWithSimpleWebAuthn is a contract test that verifies
// the options returned by BeginRegistration/BeginLogin have the exact shape
// expected by @simplewebauthn/browser v13's startRegistration/startAuthentication.
//
// This test exists because the options-shape mismatch bug (PR #610) was the
// most dangerous bug in the passkey feature — it would have broken every
// ceremony in production, and none of our other tests caught it. The service-
// level test authenticator bypasses @simplewebauthn/browser entirely, so it
// would not detect a shape mismatch.
//
// The contract:
//   - Registration: options MUST contain top-level `challenge`, `rp`, `user`,
//     `pubKeyCredParams` (NOT wrapped in { publicKey: { ... } }).
//   - Login: options MUST contain top-level `challenge`, `rpId` (NOT wrapped).
func TestOptionsShape_ContractWithSimpleWebAuthn(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	t.Run("registration options match PublicKeyCredentialCreationOptionsJSON", func(t *testing.T) {
		opts, err := svc.BeginRegistration(ctx, "user-contract", "alice")
		require.NoError(t, err)

		// Marshal to JSON to simulate what the browser receives.
		raw, err := json.Marshal(opts.Options)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))

		// @simplewebauthn/browser v13 expects these top-level keys
		// (the flat PublicKeyCredentialCreationOptionsJSON shape).
		assert.Contains(t, m, "challenge", "options MUST have top-level 'challenge'")
		assert.Contains(t, m, "rp", "options MUST have top-level 'rp'")
		assert.Contains(t, m, "user", "options MUST have top-level 'user'")
		assert.Contains(t, m, "pubKeyCredParams", "options MUST have top-level 'pubKeyCredParams'")

		// MUST NOT have a 'publicKey' wrapper (that was the PR #610 bug).
		assert.NotContains(t, m, "publicKey", "options MUST NOT be wrapped in { publicKey: ... }")

		// challenge must be a base64url string.
		challenge, ok := m["challenge"].(string)
		assert.True(t, ok && len(challenge) > 0, "challenge must be a non-empty string")
	})

	t.Run("login options match PublicKeyCredentialRequestOptionsJSON", func(t *testing.T) {
		// Seed a credential so BeginLogin can proceed.
		store := &memStore{}
		store.creds = []Credential{{UserID: "u1", CredentialID: []byte("cred-1")}}
		users := &fakeUserLookup{users: map[string]*types.User{
			"login@test.com": {ID: "u1", Username: "alice", Email: "login@test.com"},
		}}
		mr, err := miniredis.Run()
		require.NoError(t, err)
		defer mr.Close()
		loginSvc, err := New(ServiceConfig{
			RPID: "localhost", RPName: "T", RPOrigins: []string{"https://localhost"},
			Store: store, Users: users,
			Sessions: NewCacheSessionStore(redis.NewClient(&redis.Options{Addr: mr.Addr()})),
		})
		require.NoError(t, err)

		opts, _, err := loginSvc.BeginLogin(ctx, "login@test.com")
		require.NoError(t, err)

		raw, err := json.Marshal(opts.Options)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))

		// @simplewebauthn/browser v13 expects these top-level keys.
		assert.Contains(t, m, "challenge", "login options MUST have top-level 'challenge'")
		assert.Contains(t, m, "rpId", "login options MUST have top-level 'rpId'")

		// MUST NOT have a 'publicKey' wrapper.
		assert.NotContains(t, m, "publicKey", "login options MUST NOT be wrapped in { publicKey: ... }")
	})
}
