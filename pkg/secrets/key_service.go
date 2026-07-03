// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// JWTSessionKEKInfo is the HKDF `info` constant used to derive the KEK
// that wraps the durable per-JWT DEK. Pinned here so the rehydrate path
// and the login durable-write path produce byte-identical KEKs.
const JWTSessionKEKInfo = "llmsafespaces-jwt-session-dek-kek"

// UserKeyRecord represents a row in the user_keys table.
type UserKeyRecord struct {
	UserID             string
	KeyVersion         int
	WrappedDEK         []byte
	WrappedDEKRecovery []byte // nil if user opted out
	Salt               []byte
	RecoverySalt       []byte // nil if user opted out
	CreatedAt          time.Time
	RotatedAt          *time.Time
}

// KeyStore abstracts database operations for user keys.
type KeyStore interface {
	GetUserKey(ctx context.Context, userID string) (*UserKeyRecord, error)
	CreateUserKey(ctx context.Context, record *UserKeyRecord) error
	UpdateWrappedDEK(ctx context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error
	UpdateWrappedDEKRecovery(ctx context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error
}

// DEKCache abstracts session-based DEK caching (Redis).
type DEKCache interface {
	CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error
	GetDEK(ctx context.Context, sessionID string) ([]byte, error)
	EvictDEK(ctx context.Context, sessionID string) error
}

// KeyService manages user key lifecycle.
type KeyService struct {
	store           KeyStore
	cache           DEKCache
	secretStore     SecretStore
	logger          pkginterfaces.LoggerInterface
	apiKeyStore     APIKeyStore
	rootKeyProvider RootKeyProvider
	// jwtSessions is the durable per-JWT DEK store. Optional — when nil,
	// GetDEK behaves as before (Redis-only). When set, GetDEK falls back
	// to durable rehydrate on Redis miss. Wired by app.go after Epic 56
	// migration 000045 has run.
	jwtSessions JWTSessionStore
	// signingKeys enumerates active JWT signing keys (primary + previous)
	// so GetDEKForUser can iterate them against a durable jwt_sessions
	// row without needing a caller-supplied matchedSigningKey. Optional —
	// when nil, GetDEKForUser returns ErrDEKUnavailable (background-
	// caller paths degrade the same way as "no session"). Wired by
	// app.go once auth.Service is constructed (which is where the
	// active + previous keys live).
	signingKeys SigningKeyEnumerator
}

// SigningKeyEnumerator exposes the API's active JWT signing keys to
// callers that need to unwrap a durable DEK on behalf of a user in a
// background context (workspace watcher, controller-triggered auto-
// push, etc.). Implemented by auth.Service via a wrapper that iterates
// s.jwtSecret followed by s.jwtPreviousSecrets.
//
// The callback contract: `fn` returns TRUE to continue iteration or
// FALSE to stop (typical: stop after first successful unwrap). Bytes
// passed to `fn` MUST NOT be retained by the callback — implementations
// may reuse a single backing buffer, or copy from internal state and
// zero on return. Callers that need to retain a key past the callback
// call must copy.
type SigningKeyEnumerator interface {
	EachSigningKey(fn func(key []byte) bool)
}

// SetSigningKeyEnumerator installs the signing-key enumerator. Optional
// setter (not New arg) because auth.Service is constructed later in
// app.New; setter-DI is the existing pattern for these late-arrival
// deps.
func (s *KeyService) SetSigningKeyEnumerator(e SigningKeyEnumerator) {
	s.signingKeys = e
}

// APIKeyRecord is the subset of API key data needed for DEK re-wrap.
type APIKeyRecord struct {
	ID            string
	WrappedDEK    []byte
	KekSalt       []byte
	KeyCiphertext []byte
	DecryptAccess bool
}

// APIKeyStore abstracts database operations for API key DEK re-wrap.
type APIKeyStore interface {
	ListAPIKeysWithDecrypt(ctx context.Context, userID string) ([]*APIKeyRecord, error)
	UpdateAPIKeyDEK(ctx context.Context, keyID string, wrappedDEK, kekSalt []byte, synced bool) error
}

// NewKeyService creates a new KeyService.
func NewKeyService(store KeyStore, cache DEKCache) *KeyService {
	return &KeyService{store: store, cache: cache}
}

// SetAPIKeyStore wires the API key store for DEK re-wrap on rotation.
func (s *KeyService) SetAPIKeyStore(store APIKeyStore, provider RootKeyProvider) {
	s.apiKeyStore = store
	s.rootKeyProvider = provider
}

// SetJWTSessionStore wires the durable jwt_sessions table backing the
// GetDEK rehydrate path. Optional — tests and pre-Epic-56 callers may
// leave it nil; GetDEK then behaves Redis-only (cache miss ⇒ error).
//
// Like SetSecretStore, silent rebinding to a different store is refused:
// the durable rehydrate would otherwise read from a store that holds no
// rows for the active session set, surfacing as a wave of
// ErrDEKUnavailable across all live JWTs. Idempotent same-store calls
// are allowed.
func (s *KeyService) SetJWTSessionStore(store JWTSessionStore) {
	if s.jwtSessions != nil && s.jwtSessions != store {
		panic("KeyService.SetJWTSessionStore called twice with different stores; refusing to silently rebind")
	}
	s.jwtSessions = store
}

// JWTSessionStoreSet reports whether a JWT-session store has been wired.
// Exposed so app.go wiring + tests can assert post-init invariants
// without reaching into private state.
func (s *KeyService) JWTSessionStoreSet() bool {
	return s.jwtSessions != nil
}

// SetLogger installs the logger used to surface non-fatal failures
// (e.g. cache-evict errors during password change). Optional; if
// nil, those events are silent. Validator pass-5 finding N-3.
//
// Note: ChangePassword's evict-failure log includes the sessionID
// (JWT jti). The jti is sensitive — an attacker with log read
// access can correlate user activity across requests, though it
// does NOT enable token replay (the JWT signature is never logged).
// Volume is bounded to Redis-outage events. If the log retention
// crosses a tenant boundary, hash sessionID before logging.
func (s *KeyService) SetLogger(l pkginterfaces.LoggerInterface) {
	s.logger = l
}

// SetSecretStore wires the SecretStore used by RotateKeyWithPassword to
// re-encrypt every user_secrets row under the new DEK. Without this, the
// rotate endpoint refuses to run rather than orphan secret rows under a
// discarded DEK (Bug 9 in worklog 0085).
//
// Once set, the store cannot be silently reassigned: a silent
// reassignment would mean RotateKeyWithPassword ignores secrets owned
// by an abandoned store — exactly the Bug 9 hazard. Calling
// SetSecretStore twice with different stores panics; calling with the
// same store (idempotent re-init) is allowed.
func (s *KeyService) SetSecretStore(store SecretStore) {
	if s.secretStore != nil && s.secretStore != store {
		panic("KeyService.SetSecretStore called twice with different stores; refusing to silently rebind")
	}
	s.secretStore = store
}

// InitializeUserKeys generates a DEK and wraps it with the user's password-derived KEK.
// Called during account creation or first secret creation for existing users.
// Returns the recovery key (hex-encoded) that must be displayed to the user once.
func (s *KeyService) InitializeUserKeys(ctx context.Context, userID string, password []byte) (recoveryKeyHex string, err error) {
	dek, err := GenerateDEK()
	if err != nil {
		return "", fmt.Errorf("generate DEK: %w", err)
	}

	salt, err := GenerateSalt()
	if err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	kek, err := DeriveKEKFromPassword(password, salt)
	if err != nil {
		return "", fmt.Errorf("derive KEK: %w", err)
	}
	defer zeroBytes(kek)

	wrappedDEK, err := WrapDEK(kek, dek)
	if err != nil {
		return "", fmt.Errorf("wrap DEK: %w", err)
	}

	// Generate recovery key
	recoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		return "", fmt.Errorf("generate recovery key: %w", err)
	}

	recoverySalt, err := GenerateSalt()
	if err != nil {
		return "", fmt.Errorf("generate recovery salt: %w", err)
	}

	recoveryKEK, err := DeriveKEKFromKey(recoveryKey, recoverySalt, recInfo)
	if err != nil {
		return "", fmt.Errorf("derive recovery KEK: %w", err)
	}
	defer zeroBytes(recoveryKEK)

	wrappedDEKRecovery, err := WrapDEK(recoveryKEK, dek)
	if err != nil {
		return "", fmt.Errorf("wrap DEK with recovery: %w", err)
	}

	record := &UserKeyRecord{
		UserID:             userID,
		KeyVersion:         1,
		WrappedDEK:         wrappedDEK,
		WrappedDEKRecovery: wrappedDEKRecovery,
		Salt:               salt,
		RecoverySalt:       recoverySalt,
		CreatedAt:          time.Now(),
	}

	if err := s.store.CreateUserKey(ctx, record); err != nil {
		return "", fmt.Errorf("store user key: %w", err)
	}

	return hex.EncodeToString(recoveryKey), nil
}

// UnlockDEK derives the KEK from the password, unwraps the DEK, and caches it.
// Called during login. sessionID is the JWT's jti claim.
//
// This is the pre-Epic-56 entry point — Redis cache only. Use
// UnlockDEKWithSigningKey from the login site to additionally write the
// durable jwt_sessions row (Epic 56). Internal callers (auth.Login)
// always go through the With-SigningKey variant; tests and Register
// (which has no JWT yet at the point of call) use this one.
func (s *KeyService) UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error {
	return s.UnlockDEKWithSigningKey(ctx, userID, password, sessionID, ttl, nil)
}

// UnlockDEKWithSigningKey is UnlockDEK + durable jwt_sessions write
// (Epic 56). The durable row is wrapped under a KEK derived from
// activeSigningKey || jti via HKDF-SHA256; the rehydrate path
// (rehydrateDEKFromJWTSession) re-derives the same KEK from the
// MATCHED signing key recovered from a presented JWT.
//
// Behavior matrix:
//
//   - activeSigningKey == nil       → Redis cache only; no durable write.
//     This is the path tests and Register take. The legacy
//     UnlockDEK delegates here with nil.
//
//   - sessionID is not a UUID       → Redis cache only. API-key sessions
//     ("apikey:hash") and legacy non-UUID sessionIDs don't belong in
//     jwt_sessions; the api_keys.WrappedDEK design covers API-key DEK
//     durability separately.
//
//   - jwtSessions store not wired   → Redis cache only. Pre-Epic-56
//     deploys and tests without SetJWTSessionStore.
//
//   - durable write fails           → NOT returned as an error. The
//     Redis cache succeeded, so the JWT is functional for its remaining
//     lifetime; only the durable rehydrate-on-Valkey-restart property
//     is degraded. Log Warn so operators see the loss of resilience.
//     Login MUST NOT fail on a transient PG hiccup.
//
// "activeSigningKey" name is precise: at login the JWT we just issued is
// signed with s.jwtSecret (active), so we derive against the active key.
// The rehydrate path may match a previous key if rotation happens
// between issue and use — that's expected; what matters is the KEY at
// JWT-validation time, surfaced via parseTokenAcceptingRotatedKeys.
func (s *KeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, activeSigningKey []byte) error {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		// User has no keys yet (legacy user who hasn't created secrets)
		return nil
	}

	kek, err := DeriveKEKFromPassword(password, record.Salt)
	if err != nil {
		return fmt.Errorf("derive KEK: %w", err)
	}
	defer zeroBytes(kek)

	dek, err := UnwrapDEK(kek, record.WrappedDEK)
	if err != nil {
		return fmt.Errorf("unwrap DEK: %w", err)
	}

	if err := s.cache.CacheDEK(ctx, sessionID, dek, ttl); err != nil {
		return fmt.Errorf("cache DEK: %w", err)
	}

	// Epic 56: best-effort durable write so the DEK survives Valkey
	// restart for the JWT's remaining lifetime. Skipped when any of
	// (store / signing key / valid jti) is missing.
	s.writeDurableDEK(ctx, userID, sessionID, dek, ttl, activeSigningKey)
	return nil
}

// writeDurableDEK persists the unlocked DEK to jwt_sessions. Best-effort:
// every failure path is logged at Warn and returns without propagating.
// Login MUST stay green even if PG is degraded.
func (s *KeyService) writeDurableDEK(ctx context.Context, userID, sessionID string, dek []byte, ttl time.Duration, activeSigningKey []byte) {
	if s.jwtSessions == nil || activeSigningKey == nil {
		return
	}
	jti, perr := uuid.Parse(sessionID)
	if perr != nil {
		// API-key or legacy non-UUID session — not our table.
		return
	}

	kekSalt, sErr := GenerateSalt()
	if sErr != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK write: salt generation failed", "jti", jti.String(), "error", sErr.Error())
		}
		return
	}

	keyMaterial := make([]byte, 0, len(activeSigningKey)+36)
	keyMaterial = append(keyMaterial, activeSigningKey...)
	keyMaterial = append(keyMaterial, []byte(jti.String())...)
	kek, dErr := DeriveKEKFromKey(keyMaterial, kekSalt, JWTSessionKEKInfo)
	zeroBytes(keyMaterial)
	if dErr != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK write: KEK derive failed", "jti", jti.String(), "error", dErr.Error())
		}
		return
	}
	defer zeroBytes(kek)

	wrapped, eErr := EncryptSecret(kek, dek)
	if eErr != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK write: encrypt failed", "jti", jti.String(), "error", eErr.Error())
		}
		return
	}

	row := &JWTSession{
		JTI:        jti,
		UserID:     userID,
		WrappedDEK: wrapped,
		KEKSalt:    kekSalt,
		CreatedAt:  time.Now(),
		ExpiresAt:  time.Now().Add(ttl),
	}
	if wErr := s.jwtSessions.WriteJWTSession(ctx, row); wErr != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK write: jwt_sessions upsert failed (Redis cache still valid)",
				"jti", jti.String(), "error", wErr.Error())
		}
	}
}

// EvictDEK removes the cached DEK for a session AND the durable
// jwt_sessions row (Epic 56). Called on logout / explicit revocation.
// Non-JTI sessionIDs (API-key sessions like "apikey:hash") only evict
// the Redis cache — the api_keys table is the durable home for those.
func (s *KeyService) EvictDEK(ctx context.Context, sessionID string) error {
	if err := s.cache.EvictDEK(ctx, sessionID); err != nil {
		return err
	}
	s.deleteDurableSession(ctx, sessionID)
	return nil
}

// deleteDurableSession removes a single jwt_sessions row for the
// session, if the session is a JWT (UUID jti). Best-effort: an error
// is logged but not returned — the Redis evict has already succeeded,
// the JWT is functionally revoked from the rehydrate path's perspective
// once the cache miss happens, and the row will be pruned by the
// janitor at expires_at anyway.
func (s *KeyService) deleteDurableSession(ctx context.Context, sessionID string) {
	if s.jwtSessions == nil {
		return
	}
	jti, err := uuid.Parse(sessionID)
	if err != nil {
		// API-key or legacy non-UUID — not our table.
		return
	}
	if err := s.jwtSessions.DeleteJWTSession(ctx, jti); err != nil && s.logger != nil {
		s.logger.Warn("durable session delete failed (janitor will eventually prune)",
			"jti", jti.String(), "error", err.Error())
	}
}

// DeleteDurableSessionsForUser removes every jwt_sessions row for a
// user. Called by auth.Service.RevokeAllUserSessions (password reset,
// admin force-logout) so a stolen JWT cannot rehydrate the DEK from
// the durable store after the user has explicitly invalidated every
// outstanding session. Best-effort: failure is logged but does not
// propagate — the Redis revocation markers are already in place and
// the JWT itself is functionally dead.
//
// Returns nil even on failure — callers do not need to handle the
// error path; the contract is "drive jwt_sessions toward consistency
// with the auth-layer revocation, log if we can't".
func (s *KeyService) DeleteDurableSessionsForUser(ctx context.Context, userID string) error {
	if s.jwtSessions == nil {
		return nil
	}
	if _, err := s.jwtSessions.DeleteJWTSessionsForUser(ctx, userID); err != nil {
		if s.logger != nil {
			s.logger.Warn("durable sessions delete-for-user failed (janitor will eventually prune)",
				"userID", userID, "error", err.Error())
		}
	}
	return nil
}

// CacheDEK stores a DEK in the session cache. Used by API key auth to cache
// an unwrapped DEK under a deterministic sessionID.
func (s *KeyService) CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error {
	return s.cache.CacheDEK(ctx, sessionID, dek, ttl)
}

// GetDEK retrieves the DEK for a session.
//
// Resolution order (Epic 56):
//
//  1. Redis cache hit → return cached DEK (fast path; no DB).
//  2. Redis cache miss + matchedSigningKey supplied + sessionID is a UUID
//     → attempt durable rehydrate from jwt_sessions:
//     a. Row missing → ErrDEKUnavailable (soft-unlock will backfill).
//     b. Row expired → ErrDEKUnavailable (janitor will prune; client
//     should re-login since the JWT is itself near/past expiry).
//     c. Unwrap failure → ErrDEKUnavailable (post-rotation, US-50.4
//     DEK rotation, or row corruption — soft-unlock recovers).
//     d. Success → re-cache to Redis, return DEK.
//  3. Anything else → ErrDEKUnavailable.
//
// matchedSigningKey is the JWT signing key that validated the caller's
// token. Pass nil for non-JWT auth (API keys, controller-internal
// callers); those paths cannot rehydrate (no KEK material) and will
// surface ErrDEKUnavailable — the correct behavior, since the API-key
// auth has its own DEK persistence (api_keys.WrappedDEK) and
// controller-internal callers do not need user-DEK content.
//
// Redis errors (other than miss) are logged at Warn but DO NOT block
// the rehydrate attempt: in a Redis-outage + valid-durable-row scenario,
// rehydrate is exactly the resilience the epic provides. The previous
// "fail closed on any cache error" behavior is preserved only for the
// "no rehydrate available" sub-case.
func (s *KeyService) GetDEK(ctx context.Context, sessionID string, matchedSigningKey []byte) ([]byte, error) {
	dek, err := s.cache.GetDEK(ctx, sessionID)
	if err != nil {
		// Redis returned an error (not a miss). Log it and fall through
		// to durable rehydrate if possible; this is the resilience
		// property the epic introduces.
		if s.logger != nil {
			s.logger.Warn("Redis DEK lookup failed; attempting durable rehydrate", "error", err.Error())
		}
	} else if dek != nil {
		return dek, nil
	}

	return s.rehydrateDEKFromJWTSession(ctx, sessionID, matchedSigningKey)
}

// rehydrateDEKFromJWTSession reconstructs the DEK from the durable
// jwt_sessions row. Returns ErrDEKUnavailable for every failure case
// callers should treat as "soft-unlock can recover" — concrete causes
// are differentiated only in the structured log so operators can
// distinguish a missing row (expected at backfill time) from an unwrap
// failure (signing-key rotation outside the rotation window, US-50.4,
// or row corruption).
func (s *KeyService) rehydrateDEKFromJWTSession(ctx context.Context, sessionID string, matchedSigningKey []byte) ([]byte, error) {
	if s.jwtSessions == nil {
		// Pre-Epic-56 deploys, or tests that don't wire a store.
		return nil, ErrDEKUnavailable
	}
	if matchedSigningKey == nil {
		// API-key auth, controller-internal callers, or middleware that
		// did not surface the matched key (legacy cache hit). These
		// cannot rehydrate; surface the same error as Redis miss so the
		// caller falls through to soft-unlock.
		return nil, ErrDEKUnavailable
	}
	// API-key sessions use "apikey:<hash>" — their durable counterpart is
	// api_keys.WrappedDEK, not jwt_sessions. Skip without DB load.
	if strings.HasPrefix(sessionID, "apikey:") {
		return nil, ErrDEKUnavailable
	}
	jti, err := uuid.Parse(sessionID)
	if err != nil {
		// Non-UUID sessionIDs are legacy tests or non-JWT sessions; not
		// our table. Surface the same error so the caller falls through.
		return nil, ErrDEKUnavailable
	}

	row, err := s.jwtSessions.GetJWTSession(ctx, jti)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK rehydrate: lookup failed", "jti", jti.String(), "error", err.Error())
		}
		return nil, ErrDEKUnavailable
	}
	if row == nil {
		// Pre-feature JWT, soft-unlock not yet performed, or janitor
		// already pruned an expired row. Soft-unlock recovers.
		return nil, ErrDEKUnavailable
	}
	if !row.ExpiresAt.After(time.Now()) {
		// Race: row about to be pruned. Treat as gone.
		if s.logger != nil {
			s.logger.Warn("durable DEK rehydrate: row expired (janitor will prune)", "jti", jti.String())
		}
		return nil, ErrDEKUnavailable
	}

	// Derive KEK from matched_signing_key || jti.String() per design doc.
	// matchedSigningKey is mutated only via copy — we append into a fresh
	// slice so the caller's key bytes are not aliased.
	keyMaterial := make([]byte, 0, len(matchedSigningKey)+36)
	keyMaterial = append(keyMaterial, matchedSigningKey...)
	keyMaterial = append(keyMaterial, []byte(jti.String())...)
	kek, derr := DeriveKEKFromKey(keyMaterial, row.KEKSalt, JWTSessionKEKInfo)
	if derr != nil {
		if s.logger != nil {
			s.logger.Warn("durable DEK rehydrate: KEK derive failed", "jti", jti.String(), "error", derr.Error())
		}
		return nil, ErrDEKUnavailable
	}
	defer zeroBytes(kek)
	defer zeroBytes(keyMaterial)

	dek, uerr := DecryptSecret(kek, row.WrappedDEK)
	if uerr != nil {
		// Causes: signing key rotated out of window (JWT itself would
		// have failed validation already, so we shouldn't get here);
		// US-50.4 rewrote DEK and durable wrap is now stale; row
		// corruption. Soft-unlock handles all three.
		if s.logger != nil {
			s.logger.Warn("durable DEK rehydrate: unwrap failed (soft-unlock recovers)",
				"jti", jti.String(), "error", uerr.Error())
		}
		return nil, ErrDEKUnavailable
	}

	// Re-cache so subsequent calls in this JWT's lifetime are fast.
	// Use the row's remaining lifetime so the cache TTL never exceeds
	// the durable TTL.
	cacheTTL := time.Until(row.ExpiresAt)
	if cacheTTL > 0 {
		if cerr := s.cache.CacheDEK(ctx, sessionID, dek, cacheTTL); cerr != nil && s.logger != nil {
			s.logger.Warn("durable DEK rehydrate: re-cache failed; will rehydrate again next call",
				"jti", jti.String(), "error", cerr.Error())
		}
	}
	return dek, nil
}

// DEKAvailable checks if a DEK is cached for the given session.
func (s *KeyService) DEKAvailable(ctx context.Context, sessionID string) bool {
	dek, err := s.cache.GetDEK(ctx, sessionID)
	return err == nil && dek != nil
}

// GetDEKForUser retrieves the user's DEK without requiring a specific
// sessionID or matchedSigningKey from the caller. Designed for
// background paths (workspace watcher, controller-triggered auto-push
// after pod recreation, etc.) that need to deliver user-DEK content
// but do not run in an authenticated user-request context.
//
// Resolution order (worklog 0590):
//
//  1. jwtSessions.ListActiveJWTSessionsForUser(userID, LIMIT) →
//     candidate rows. If empty → ErrDEKUnavailable (no live session
//     for the user; caller falls back to SessionlessInject or logs).
//  2. For each row (most-recent first), check the Redis cache under
//     the row's jti. On hit → return the cached DEK (fast path;
//     avoids KDF + AEAD-decrypt).
//  3. On cache miss for that jti, iterate signingKeys.EachSigningKey.
//     For each candidate key, derive KEK = HKDF(key || jti, kekSalt,
//     JWTSessionKEKInfo) and attempt UnwrapDEK. First success →
//     write-back to Redis under this jti so subsequent GetDEK(jti,
//     matchedKey) calls hit the fast path, and return the DEK.
//  4. If NO signing key can unwrap the most-recent row: continue to
//     next row (older sessions may have been wrapped under an even
//     older signing key that this API instance still knows). If all
//     rows exhausted → ErrDEKUnavailable.
//
// Cache-hit short-circuit (step 2) is what makes this safe to call
// repeatedly for the same user: after the first successful call, all
// subsequent calls hit Redis in O(1). Only cold-Redis or genuine
// cache-miss paths do PG+KDF work.
//
// Rows are bounded (LIMIT jwtSessionUserLookupLimit) to prevent
// pathological unwrap-loops if a user has thousands of sessions.
//
// Errors: ErrDEKUnavailable is used for every legitimate "no user
// context available" case (no active session, no signing key
// unwraps, no jwtSessions or signingKeys wired). Genuine
// infrastructure errors (PG connection failure, cache client fault)
// are returned verbatim so operators can distinguish debug-worthy
// outages from expected "user logged out" cases.
func (s *KeyService) GetDEKForUser(ctx context.Context, userID string) ([]byte, error) {
	// Wiring pre-conditions. Both are optional deps (setter-DI);
	// missing either at call time is a wiring bug for the caller's
	// use case but must not panic — degrade to the same sentinel
	// the "no session" case uses.
	if s.jwtSessions == nil || s.signingKeys == nil {
		return nil, ErrDEKUnavailable
	}

	rows, err := s.jwtSessions.ListActiveJWTSessionsForUser(ctx, userID, jwtSessionUserLookupLimit)
	if err != nil {
		return nil, fmt.Errorf("list active jwt_sessions for user: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrDEKUnavailable
	}

	for _, row := range rows {
		jtiStr := row.JTI.String()

		// Fast path: Redis has the DEK cached under this jti from a
		// prior request-context lookup. Reuse it.
		//
		// Redis errors (not misses) are logged and treated as miss —
		// same resilience pattern as GetDEK. A Redis outage should
		// degrade the API to PG+KDF fallback, not fail the DEK
		// retrieval; the caller (background auto-push) will still
		// deliver secrets.
		if cached, cErr := s.cache.GetDEK(ctx, jtiStr); cErr != nil {
			if s.logger != nil {
				s.logger.Warn("GetDEKForUser: Redis DEK lookup failed; falling back to unwrap",
					"jti", jtiStr, "error", cErr.Error())
			}
		} else if cached != nil {
			return cached, nil
		}

		// Slow path: iterate signing keys, try each.
		if dek := s.tryUnwrapRowWithKnownKeys(ctx, row); dek != nil {
			return dek, nil
		}
	}

	// All rows exhausted without a successful unwrap. Every failure
	// mode (rotated-past-retention-window, corrupted wrap) surfaces as
	// ErrDEKUnavailable so callers handle uniformly.
	return nil, ErrDEKUnavailable
}

// tryUnwrapRowWithKnownKeys iterates the enumerator's signing keys and
// attempts to unwrap the row's WrappedDEK under each. Returns the DEK
// on the first success (and populates the Redis cache), or nil if no
// key succeeds. Errors are logged at Warn but not returned — a single
// row's failure is expected during rotation; the caller iterates rows.
//
// Callback contract with EachSigningKey: the enumerator implementation
// may pass a slice backed by internal state; we copy the derived
// keyMaterial into our own buffer before calling out to KDF/decrypt,
// and let the enumerator zero its bytes after return. We use a
// captured-variable pattern (rather than storing keys into a slice
// and iterating after) to keep the retention window minimal.
func (s *KeyService) tryUnwrapRowWithKnownKeys(ctx context.Context, row *JWTSession) []byte {
	var out []byte
	s.signingKeys.EachSigningKey(func(key []byte) bool {
		keyMaterial := make([]byte, 0, len(key)+36)
		keyMaterial = append(keyMaterial, key...)
		keyMaterial = append(keyMaterial, []byte(row.JTI.String())...)

		kek, dErr := DeriveKEKFromKey(keyMaterial, row.KEKSalt, JWTSessionKEKInfo)
		zeroBytes(keyMaterial)
		if dErr != nil {
			// KDF failure is not "wrong key" — it's a config bug.
			// Log Warn but continue to next key so a single bad
			// input doesn't wedge every user's auto-push.
			if s.logger != nil {
				s.logger.Warn("GetDEKForUser: KEK derive failed",
					"jti", row.JTI.String(), "error", dErr.Error())
			}
			return true
		}
		dek, uErr := UnwrapDEK(kek, row.WrappedDEK)
		zeroBytes(kek)
		if uErr != nil {
			// Wrong key — expected during rotation. Continue.
			return true
		}
		// Success. Write-back to Redis so the next request-context
		// GetDEK(jti, matchedKey) call hits the fast path. Best-
		// effort: cache errors don't fail the return.
		//
		// Guard against negative TTLs: the row was queried as
		// expires_at > NOW() at the top of GetDEKForUser, but some
		// milliseconds may have elapsed between that filter and
		// this write. If the remaining lifetime is <= 0, Redis
		// SETEX errors — skip the write rather than log a spurious
		// warning. Mirrors the pattern in rehydrateDEKFromJWTSession
		// (key_service.go, cacheTTL > 0 guard).
		if cacheTTL := time.Until(row.ExpiresAt); cacheTTL > 0 {
			if cErr := s.cache.CacheDEK(ctx, row.JTI.String(), dek, cacheTTL); cErr != nil {
				if s.logger != nil {
					s.logger.Warn("GetDEKForUser: cache write-back failed (DEK still returned)",
						"jti", row.JTI.String(), "error", cErr.Error())
				}
			}
		}
		out = dek
		return false // stop enumeration
	})
	return out
}

// jwtSessionUserLookupLimit caps how many jwt_sessions rows GetDEKForUser
// examines for a single user. A well-behaved user has 1-3 concurrent
// sessions (web + mobile + workstation). The limit guards against a
// pathological "user has 10k sessions" scenario from bogging down the
// unwrap-loop. Set intentionally low: once we've tried the 5 most-
// recent sessions without a successful unwrap, the rotation window is
// clearly outside our known keys and further rows won't help.
const jwtSessionUserLookupLimit = 5

// ChangePassword re-wraps the DEK with a new password-derived KEK.
// Requires the old password to unwrap first. After the wrap is
// updated, the cached DEK for sessionID (the caller's current
// session) is evicted so the next request must re-Unlock with the
// new password — without this eviction a thief who has the JWT
// continues to read secrets via the cached DEK even after the user
// "rotates the password to be safe" (validator pass-3 finding P-1).
//
// LIMITATION: this only evicts the caller's session. A user with
// multiple active sessions on different devices retains those
// cached DEKs until they expire naturally. We document the
// limitation in the API rather than rebuild the cache for cross-
// session enumeration.
//
// sessionID may be empty (e.g. tests, internal callers without a
// session); eviction is then a no-op.
func (s *KeyService) ChangePassword(ctx context.Context, userID, sessionID string, oldPassword, newPassword []byte) error {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		return ErrUserKeysMissing
	}

	// Unwrap with old password
	oldKEK, err := DeriveKEKFromPassword(oldPassword, record.Salt)
	if err != nil {
		return fmt.Errorf("derive old KEK: %w", err)
	}
	defer zeroBytes(oldKEK)
	dek, err := UnwrapDEK(oldKEK, record.WrappedDEK)
	if err != nil {
		// Invalid password: uniform failure code so the handler can
		// map to 403 via errors.Is. We deliberately drop the wrapped
		// AEAD/bcrypt diagnostic so a future log-formatter that
		// prints the error verbatim does not leak the underlying
		// failure mode (validator pass-3 finding NEW-7).
		return ErrInvalidPassword
	}
	defer zeroBytes(dek)

	// Re-wrap with new password
	newSalt, err := GenerateSalt()
	if err != nil {
		return fmt.Errorf("generate new salt: %w", err)
	}
	newKEK, err := DeriveKEKFromPassword(newPassword, newSalt)
	if err != nil {
		return fmt.Errorf("derive new KEK: %w", err)
	}
	defer zeroBytes(newKEK)
	newWrappedDEK, err := WrapDEK(newKEK, dek)
	if err != nil {
		return fmt.Errorf("wrap DEK with new password: %w", err)
	}

	// Evict the cached DEK BEFORE the wrap update commits. Order
	// matters: if we evicted after the commit, a concurrent request
	// from the same JWT landing between the commit and the evict
	// would still get a cache hit and run with the discarded DEK
	// (validator pass-4 finding NEW-4). Pre-commit eviction means
	// the worst case is the user has to re-Unlock with their OLD
	// password — which still works because user_keys.wrapped_dek
	// hasn't changed yet.
	//
	// Cache-evict errors are non-fatal but observable: a Redis
	// outage that silently leaves the cached DEK in place re-opens
	// the race window the reorder closed. Log Warn so operators see
	// the degradation (validator pass-5 finding N-3).
	if sessionID != "" {
		// Cache-evict errors are non-fatal but observable. Skipped when
		// no cache is wired (test-only path); production always wires
		// one.
		if s.cache != nil {
			if err := s.cache.EvictDEK(ctx, sessionID); err != nil && s.logger != nil {
				s.logger.Warn("ChangePassword: DEK evict failed; cached DEK may be stale until TTL",
					"userID", userID, "sessionID", sessionID, "error", err.Error())
			}
		}
		// Epic 56: also delete the durable jwt_sessions row. Without
		// this, an attacker who has the old JWT (signing key valid)
		// could rehydrate the OLD DEK from PG after the password change
		// — defeating the point of the change.
		//
		// Decoupled from `s.cache != nil` (PR #421 review pass 1): the
		// durable delete is logically independent of cache presence and
		// must run whenever we know which session to invalidate. The
		// previous coupling was latent rather than live (cache is always
		// wired in production), but the literal-meaning fix is one
		// line and prevents a future cache-less unit test from
		// silently bypassing the durable invalidation.
		s.deleteDurableSession(ctx, sessionID)
	}

	if err := s.store.UpdateWrappedDEK(ctx, userID, newWrappedDEK, newSalt, record.KeyVersion); err != nil {
		return err
	}
	return nil
}

// ResetWithRecoveryKey unwraps the DEK using the recovery key and re-wraps with a new password.
// Returns a new recovery key (hex-encoded).
func (s *KeyService) ResetWithRecoveryKey(ctx context.Context, userID string, recoveryKeyHex string, newPassword []byte) (newRecoveryKeyHex string, err error) {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		return "", ErrUserKeysMissing
	}
	if record.WrappedDEKRecovery == nil || record.RecoverySalt == nil {
		return "", errors.New("no recovery key configured for this user")
	}

	recoveryKey, err := hex.DecodeString(recoveryKeyHex)
	if err != nil {
		return "", errors.New("invalid recovery key format")
	}

	recoveryKEK, err := DeriveKEKFromKey(recoveryKey, record.RecoverySalt, recInfo)
	if err != nil {
		return "", fmt.Errorf("derive recovery KEK: %w", err)
	}
	defer zeroBytes(recoveryKEK)

	dek, err := UnwrapDEK(recoveryKEK, record.WrappedDEKRecovery)
	if err != nil {
		return "", fmt.Errorf("%w: recovery key did not unwrap", ErrInvalidPassword)
	}
	defer zeroBytes(dek)

	// Re-wrap with new password
	newSalt, err := GenerateSalt()
	if err != nil {
		return "", fmt.Errorf("generate new salt: %w", err)
	}
	newKEK, err := DeriveKEKFromPassword(newPassword, newSalt)
	if err != nil {
		return "", fmt.Errorf("derive new KEK: %w", err)
	}
	defer zeroBytes(newKEK)
	newWrappedDEK, err := WrapDEK(newKEK, dek)
	if err != nil {
		return "", fmt.Errorf("wrap DEK: %w", err)
	}

	if err := s.store.UpdateWrappedDEK(ctx, userID, newWrappedDEK, newSalt, record.KeyVersion); err != nil {
		return "", fmt.Errorf("update wrapped DEK: %w", err)
	}

	// Generate new recovery key
	newRecoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		return "", fmt.Errorf("generate new recovery key: %w", err)
	}
	newRecoverySalt, err := GenerateSalt()
	if err != nil {
		return "", fmt.Errorf("generate new recovery salt: %w", err)
	}
	newRecoveryKEK, err := DeriveKEKFromKey(newRecoveryKey, newRecoverySalt, recInfo)
	if err != nil {
		return "", fmt.Errorf("derive new recovery KEK: %w", err)
	}
	defer zeroBytes(newRecoveryKEK)
	newWrappedDEKRecovery, err := WrapDEK(newRecoveryKEK, dek)
	if err != nil {
		return "", fmt.Errorf("wrap DEK with new recovery: %w", err)
	}

	if err := s.store.UpdateWrappedDEKRecovery(ctx, userID, newWrappedDEKRecovery, newRecoverySalt); err != nil {
		return "", fmt.Errorf("update recovery key: %w", err)
	}

	return hex.EncodeToString(newRecoveryKey), nil
}

// HasKeys checks if a user has key material initialized.
func (s *KeyService) HasKeys(ctx context.Context, userID string) (bool, error) {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return false, err
	}
	return record != nil, nil
}

// RotationResult is what RotateKeyWithPassword returns. NewKeyVersion
// is the bumped key_version; NewRecoveryKeyHex is a freshly-issued
// recovery key (the previous one wraps the now-discarded old DEK and
// is invalid after rotation). Callers MUST surface NewRecoveryKeyHex
// to the user once — the API does not store it anywhere recoverable.
type RotationResult struct {
	NewKeyVersion     int
	NewRecoveryKeyHex string
}

// RotateKeyWithPassword rotates the user's DEK and eagerly re-encrypts
// every secret row under the new DEK in a single transaction.
//
// The flow is:
//
//  1. Verify the password by unwrapping the current DEK with the derived KEK.
//  2. Generate a new random DEK.
//  3. Generate a new recovery key + salt; the old recovery key wraps
//     the old (about-to-be-discarded) DEK and would be useless after
//     rotation. Without this, ResetWithRecoveryKey post-rotate would
//     unwrap a DEK that no longer matches user_secrets.
//  4. Walk all user_secrets rows; decrypt each with the old DEK and
//     re-encrypt with the new DEK. The store implementation runs this
//     under a single atomic operation so partial failures cannot leave
//     orphaned rows.
//  5. Wrap the new DEK with the same KEK and bump key_version,
//     INSIDE the same tx (commit closure). Wrap newDEK with the new
//     recoveryKEK and update user_keys.wrapped_dek_recovery in the
//     same tx.
//  6. Refresh the session DEK cache.
//
// If any step in 4 or 5 fails, the entire tx rolls back: secrets stay
// at the old key_version, user_keys keeps the old wrapped DEK, the
// old recovery key still works. The rotation is a no-op from the
// client's perspective (modulo the function's error return).
//
// SetSecretStore must be called before RotateKeyWithPassword; otherwise
// the function refuses to run.
func (s *KeyService) RotateKeyWithPassword(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) (RotationResult, error) {
	if s.secretStore == nil {
		return RotationResult{}, errors.New("rotate-key not configured: secret store missing")
	}

	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return RotationResult{}, fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		return RotationResult{}, ErrUserKeysMissing
	}

	kek, err := DeriveKEKFromPassword(password, record.Salt)
	if err != nil {
		return RotationResult{}, fmt.Errorf("derive KEK: %w", err)
	}
	defer zeroBytes(kek)

	oldDEK, err := UnwrapDEK(kek, record.WrappedDEK)
	if err != nil {
		return RotationResult{}, ErrInvalidPassword
	}
	defer zeroBytes(oldDEK)

	newDEK, err := GenerateDEK()
	if err != nil {
		return RotationResult{}, fmt.Errorf("generate new DEK: %w", err)
	}
	defer zeroBytes(newDEK)

	newVersion := record.KeyVersion + 1

	newWrappedDEK, err := WrapDEK(kek, newDEK)
	if err != nil {
		return RotationResult{}, fmt.Errorf("wrap new DEK: %w", err)
	}

	// Generate a fresh recovery key and re-wrap the new DEK with it.
	// The previous recovery key wrapped the OLD DEK; without this
	// step, ResetWithRecoveryKey post-rotation would yield the old
	// DEK and every secret (now encrypted with the new DEK) would be
	// undecryptable — exactly the data-loss class of bug Bug 9 fixed
	// for the password path. Argued as A2 in the worklog 0094 pass-2
	// audit.
	newRecoveryKey, err := GenerateRecoveryKey()
	if err != nil {
		return RotationResult{}, fmt.Errorf("generate new recovery key: %w", err)
	}
	defer zeroBytes(newRecoveryKey)
	newRecoverySalt, err := GenerateSalt()
	if err != nil {
		return RotationResult{}, fmt.Errorf("generate new recovery salt: %w", err)
	}
	newRecoveryKEK, err := DeriveKEKFromKey(newRecoveryKey, newRecoverySalt, recInfo)
	if err != nil {
		return RotationResult{}, fmt.Errorf("derive new recovery KEK: %w", err)
	}
	defer zeroBytes(newRecoveryKEK)
	newWrappedDEKRecovery, err := WrapDEK(newRecoveryKEK, newDEK)
	if err != nil {
		return RotationResult{}, fmt.Errorf("wrap new DEK with recovery KEK: %w", err)
	}

	transform := func(oldCT []byte) ([]byte, error) {
		plaintext, derr := DecryptSecret(oldDEK, oldCT)
		if derr != nil {
			return nil, fmt.Errorf("decrypt with old DEK: %w", derr)
		}
		defer zeroBytes(plaintext)
		newCT, eerr := EncryptSecret(newDEK, plaintext)
		if eerr != nil {
			return nil, fmt.Errorf("encrypt with new DEK: %w", eerr)
		}
		return newCT, nil
	}
	commit := func(txCtx context.Context) error {
		// Both writes run inside the same tx via withTx/txFromContext.
		// If the recovery-wrap update fails, the entire rotation
		// rolls back: secrets stay at old key_version, user_keys
		// stays at old wrapped DEK + old recovery wrap.
		if err := s.store.UpdateWrappedDEK(txCtx, userID, newWrappedDEK, record.Salt, newVersion); err != nil {
			return err
		}
		return s.store.UpdateWrappedDEKRecovery(txCtx, userID, newWrappedDEKRecovery, newRecoverySalt)
	}
	if err := s.secretStore.ReEncryptUserSecrets(ctx, userID, newVersion, transform, commit); err != nil {
		return RotationResult{}, fmt.Errorf("re-encrypt user secrets: %w", err)
	}

	if err := s.cache.CacheDEK(ctx, sessionID, newDEK, ttl); err != nil {
		return RotationResult{}, fmt.Errorf("cache new DEK: %w", err)
	}

	// Epic 56: delete the durable jwt_sessions row. The wrapped_dek on
	// it encrypts the OLD DEK; user_secrets are now re-encrypted under
	// the new DEK. A rehydrate via that row would yield a DEK that
	// can't decrypt anything — strictly worse than rehydrate-fails ⇒
	// soft-unlock. The user's next request on a fresh request after
	// this rotation will either hit the in-process Redis cache (just
	// repopulated) or, after Valkey restart, surface ErrDEKUnavailable
	// and prompt soft-unlock.
	s.deleteDurableSession(ctx, sessionID)

	s.rewrapAPIKeyDEKs(ctx, userID, newDEK)

	return RotationResult{
		NewKeyVersion:     newVersion,
		NewRecoveryKeyHex: hex.EncodeToString(newRecoveryKey),
	}, nil
}

func (s *KeyService) rewrapAPIKeyDEKs(ctx context.Context, userID string, newDEK []byte) {
	if s.apiKeyStore == nil || s.rootKeyProvider == nil {
		return
	}

	keys, err := s.apiKeyStore.ListAPIKeysWithDecrypt(ctx, userID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("rewrapAPIKeyDEKs: failed to list API keys", "userID", userID, "error", err.Error())
		}
		return
	}

	for _, key := range keys {
		if !key.DecryptAccess || len(key.KeyCiphertext) == 0 {
			continue
		}

		rawKey, decErr := s.rootKeyProvider.Decrypt(ctx, key.KeyCiphertext)
		if decErr != nil {
			if s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to decrypt key_ciphertext",
					"keyID", key.ID, "error", decErr.Error())
			}
			if err := s.apiKeyStore.UpdateAPIKeyDEK(ctx, key.ID, nil, nil, false); err != nil && s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to mark key as decrypt_access=false", "keyID", key.ID, "error", err.Error())
			}
			continue
		}

		apiKEK, deriveErr := DeriveKEKFromKey(rawKey, key.KekSalt, "llmsafespaces-apikey-kek")
		zeroBytes(rawKey)
		if deriveErr != nil {
			if s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to derive API KEK",
					"keyID", key.ID, "error", deriveErr.Error())
			}
			if err := s.apiKeyStore.UpdateAPIKeyDEK(ctx, key.ID, nil, nil, false); err != nil && s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to mark key as decrypt_access=false", "keyID", key.ID, "error", err.Error())
			}
			continue
		}

		wrappedDEK, wrapErr := EncryptSecret(apiKEK, newDEK)
		zeroBytes(apiKEK)
		if wrapErr != nil {
			if s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to wrap new DEK",
					"keyID", key.ID, "error", wrapErr.Error())
			}
			if err := s.apiKeyStore.UpdateAPIKeyDEK(ctx, key.ID, nil, nil, false); err != nil && s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to mark key as decrypt_access=false", "keyID", key.ID, "error", err.Error())
			}
			continue
		}

		if updateErr := s.apiKeyStore.UpdateAPIKeyDEK(ctx, key.ID, wrappedDEK, key.KekSalt, true); updateErr != nil {
			if s.logger != nil {
				s.logger.Warn("rewrapAPIKeyDEKs: failed to update wrapped DEK in DB",
					"keyID", key.ID, "error", updateErr.Error())
			}
		}
	}
}

// zeroBytes overwrites b with zeros to reduce the time secret material
// lingers in memory after the function that owned it returns.
//
// The Go specification does NOT formally guarantee that this write
// cannot be eliminated by the compiler. In practice the current Go
// compiler does not elide it (the slice escapes via the caller), and
// the runtime.KeepAlive call below explicitly defeats any future
// elimination by extending b's lifetime past the loop. This is
// best-effort defense-in-depth, not a confidentiality boundary —
// callers must not rely on this for timing-channel resistance, and
// the underlying memory may have been swapped to disk before the wipe
// runs anyway.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}
