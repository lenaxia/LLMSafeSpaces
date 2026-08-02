// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"time"
)

// PostgresSecretProvider implements SecretProvider using the KeyService and SecretStore.
type PostgresSecretProvider struct {
	keys  *KeyService
	store SecretStore
}

// NewPostgresSecretProvider creates a new PostgresSecretProvider.
func NewPostgresSecretProvider(keys *KeyService, store SecretStore) *PostgresSecretProvider {
	return &PostgresSecretProvider{keys: keys, store: store}
}

func (p *PostgresSecretProvider) Encrypt(ctx context.Context, owner SecretOwner, plaintext []byte) ([]byte, int, error) {
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return nil, 0, fmt.Errorf("no active session for encryption")
	}

	dek, err := p.keys.GetDEK(ctx, sessionID, matchedSigningKeyFromContext(ctx))
	if err != nil {
		return nil, 0, err
	}

	ciphertext, err := EncryptSecret(dek, plaintext)
	if err != nil {
		return nil, 0, err
	}

	record, err := p.keys.store.GetUserKey(ctx, owner.ID)
	if err != nil || record == nil {
		return nil, 0, fmt.Errorf("user key not found")
	}

	return ciphertext, record.KeyVersion, nil
}

func (p *PostgresSecretProvider) Decrypt(ctx context.Context, owner SecretOwner, ciphertext []byte, keyVersion int) ([]byte, error) {
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return nil, fmt.Errorf("no active session for decryption")
	}

	dek, err := p.keys.GetDEK(ctx, sessionID, matchedSigningKeyFromContext(ctx))
	if err != nil {
		return nil, err
	}

	return DecryptSecret(dek, ciphertext)
}

func (p *PostgresSecretProvider) RotateKey(ctx context.Context, owner SecretOwner) (int, error) {
	// Rotation requires password — this interface method cannot be used directly.
	// Use KeyService.RotateKeyWithPassword instead.
	return 0, fmt.Errorf("use RotateKeyWithPassword for password-confirmed rotation")
}

func (p *PostgresSecretProvider) DEKAvailable(ctx context.Context, owner SecretOwner) bool {
	sessionID := sessionIDFromContext(ctx)
	if sessionID == "" {
		return false
	}
	return p.keys.DEKAvailable(ctx, sessionID)
}

// contextKey for session ID in context.
type contextKeyType string

const contextKeySessionID contextKeyType = "secretsSessionID"
const contextKeyMatchedSigningKey contextKeyType = "secretsMatchedSigningKey"

// ContextWithSessionID adds a session ID to the context for the SecretProvider.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, contextKeySessionID, sessionID)
}

// ContextWithMatchedSigningKey carries the JWT signing key that validated
// the caller's token, so PostgresSecretProvider.Encrypt/Decrypt can pass
// it through to KeyService.GetDEK for durable-DEK rehydrate (Epic 56).
// Pass nil for API-key / sessionless paths — KeyService.GetDEK will
// surface ErrDEKUnavailable when rehydrate is needed.
//
// Note: this is the SecretProvider's own typed-context key, NOT the
// gin.Context value set by AuthMiddleware. Handlers extract from gin and
// stash here before invoking provider methods.
//
// Current state (PR #421): no production caller sets this value because
// PostgresSecretProvider itself has no production caller — *SecretService
// (not *PostgresSecretProvider) is the live path for user-secret CRUD.
// The matched-key plumbing is preserved on the SecretProvider interface
// for symmetry with the design doc and so a future revival of the
// SecretProvider path inherits Epic 56 rehydrate by setting this key;
// without that revival it is harmless dead code, not a regression.
func ContextWithMatchedSigningKey(ctx context.Context, matchedSigningKey []byte) context.Context {
	return context.WithValue(ctx, contextKeyMatchedSigningKey, matchedSigningKey)
}

func sessionIDFromContext(ctx context.Context) string {
	v := ctx.Value(contextKeySessionID)
	if v == nil {
		return ""
	}
	return v.(string)
}

func matchedSigningKeyFromContext(ctx context.Context) []byte {
	v := ctx.Value(contextKeyMatchedSigningKey)
	if v == nil {
		return nil
	}
	b, _ := v.([]byte)
	return b
}

// Ensure PostgresSecretProvider implements SecretProvider at compile time.
var _ SecretProvider = (*PostgresSecretProvider)(nil)

// Ensure KeyService satisfies the auth integration interface at compile time.
var _ interface {
	InitializeUserKeysServerKEK(ctx context.Context, userID, dekSource string) error
	UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error
	HasKeys(ctx context.Context, userID string) (bool, error)
} = (*KeyService)(nil)
