// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
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

// dekSourceServerKEK is the users.dek_source value indicating a server-KEK-
// wrapped DEK (SSO users under Epic 58). It matches the DB enum value and
// pkg/types.DEKSourceServerKEK; pkg/secrets keeps a local copy to stay
// decoupled from the API DTO layer.
const dekSourceServerKEK = "server_kek"

// dekSourcePasskey is the passkey-only user's dek_source value (Epic 59). The
// DEK is wrapped by the master-KEK provider identically to dekSourceServerKEK.
const dekSourcePasskey = "passkey"

// dekSourceIsServerWrapped reports whether a dek_source value means the user's
// DEK is wrapped by the master-KEK RootKeyProvider (server_kek via SSO, passkey
// via passkey-only login). Both share one unwrap path: rootKeyProvider.Decrypt.
func dekSourceIsServerWrapped(s string) bool {
	return s == dekSourceServerKEK || s == dekSourcePasskey
}

// UserKeyRecord represents a row in the user_keys table.
type UserKeyRecord struct {
	UserID             string
	KeyVersion         int
	WrappedDEK         []byte
	WrappedDEKRecovery []byte // nil if user opted out
	Salt               []byte // nil for server_kek rows (no Argon2id derivation)
	RecoverySalt       []byte // nil if user opted out
	// DEKSource is the encryption source, read from users.dek_source via
	// the PgKeyStore.GetUserKey JOIN. Always "server_kek" or "passkey" —
	// both unwrap via the master-KEK RootKeyProvider.
	DEKSource string
	CreatedAt time.Time
	RotatedAt *time.Time
}

// KeyStore abstracts database operations for user keys.
type KeyStore interface {
	GetUserKey(ctx context.Context, userID string) (*UserKeyRecord, error)
	CreateUserKey(ctx context.Context, record *UserKeyRecord) error
	UpdateWrappedDEK(ctx context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error
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
	// now is the injectable clock used for TTL math (Redis cache
	// write-back, expiry checks). Nil means use time.Now — production
	// callers never set this. Tests substitute a deterministic clock
	// so hardcoded time.Date fixtures don't roll off the wall clock
	// between commit and CI run.
	now func() time.Time
}

// nowOr returns the configured clock or time.Now when unset. Callers
// that need "current time" for TTL/expiry math should route through
// this so the clock is uniformly injectable for tests.
func (s *KeyService) nowOr() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// setClock installs a deterministic clock. Test-only helper (all
// pkg/secrets tests use `package secrets`, so the unexported name
// is accessible from tests). Rename to SetClock + export ONLY if a
// future external test package needs it, and update the docstring
// then to explain the external caller.
func (s *KeyService) setClock(now func() time.Time) {
	s.now = now
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
//
// Unlike SetJWTSessionStore / SetSecretStore, this setter intentionally
// has NO double-set panic guard. Rebinding the enumerator cannot cause
// silent data inconsistency the way rebinding a store can: the worst
// case is that a subsequent GetDEKForUser call fails to unwrap a row
// (because the new enumerator returned different keys than were used
// to wrap that row), which surfaces as ErrDEKUnavailable — the same
// sentinel the "no session" path uses. The caller falls back cleanly.
// A double-set panic here would forbid legitimate hot-swap scenarios
// (test harnesses, key-rotation live-reload) without a corresponding
// safety benefit.
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
// (e.g. cache-evict errors during session revocation). Optional; if
// nil, those events are silent. Validator pass-5 finding N-3.
//
// Note: the evict-failure log includes the sessionID
// (JWT jti). The jti is sensitive — an attacker with log read
// access can correlate user activity across requests, though it
// does NOT enable token replay (the JWT signature is never logged).
// Volume is bounded to Redis-outage events. If the log retention
// crosses a tenant boundary, hash sessionID before logging.
func (s *KeyService) SetLogger(l pkginterfaces.LoggerInterface) {
	s.logger = l
}

// SetSecretStore wires the SecretStore used for secret operations.
// Optional; without it secret operations will fail.
//
// Once set, the store cannot be silently reassigned: a silent
// SetSecretStore twice with different stores panics; calling with the
// same store (idempotent re-init) is allowed.
func (s *KeyService) SetSecretStore(store SecretStore) {
	if s.secretStore != nil && s.secretStore != store {
		panic("KeyService.SetSecretStore called twice with different stores; refusing to silently rebind")
	}
	s.secretStore = store
}

// InitializeUserKeysServerKEK provisions a DEK wrapped by the master-KEK
// RootKeyProvider. The dekSource ("server_kek" or "passkey") distinguishes
// the auth source; both share the same unwrap path (rootKeyProvider.Decrypt).
// The store atomically flips users.dek_source alongside the user_keys insert.
// Fail-closed when no provider is wired.
func (s *KeyService) InitializeUserKeysServerKEK(ctx context.Context, userID, dekSource string) error {
	if !dekSourceIsServerWrapped(dekSource) {
		return fmt.Errorf("invalid server-wrapped dek_source %q", dekSource)
	}
	if s.rootKeyProvider == nil {
		return ErrServerKEKUnavailable
	}
	dek, err := GenerateDEK()
	if err != nil {
		return fmt.Errorf("generate DEK: %w", err)
	}

	wrapped, err := s.rootKeyProvider.Encrypt(ctx, dek)
	if err != nil {
		return fmt.Errorf("server-kek wrap DEK: %w", err)
	}

	record := &UserKeyRecord{
		UserID:     userID,
		KeyVersion: ActiveVersionOf(s.rootKeyProvider),
		WrappedDEK: wrapped,
		// Salt + recovery wrap intentionally nil: no Argon2id derivation, no
		// recovery blob (DEK is recoverable from the master KEK).
		DEKSource: dekSource,
		CreatedAt: time.Now(),
	}
	if err := s.store.CreateUserKey(ctx, record); err != nil {
		return fmt.Errorf("store user key: %w", err)
	}
	return nil
}

// UnlockDEK unwraps the DEK via the master RootKeyProvider and caches it.
// The password parameter is ignored (server-KEK-only model).
// Called during login. sessionID is the JWT's jti claim.
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
func (s *KeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, _ []byte, sessionID string, ttl time.Duration, activeSigningKey []byte) error {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		// User has no keys yet (legacy user who hasn't created secrets)
		return nil
	}

	// All DEKs are now server-KEK-wrapped (master RootKeyProvider). The password
	// parameter is retained in the signature for call-site stability but ignored.
	if s.rootKeyProvider == nil {
		return ErrServerKEKUnavailable
	}
	dek, err := s.rootKeyProvider.Decrypt(ctx, record.WrappedDEK)
	if err != nil {
		return fmt.Errorf("server-kek unwrap DEK: %w", err)
	}
	// NOTE: dek is intentionally NOT zeroed here. It is cached by reference
	// (some caches store the slice without copying) and used downstream by the
	// durable write; zeroing it at return would corrupt the cached DEK. The
	// codebase convention is to zero the derived KEK, never the DEK itself.

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
	if !row.ExpiresAt.After(s.nowOr()) {
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
	cacheTTL := row.ExpiresAt.Sub(s.nowOr())
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
// Returns (dek, jti, error). The jti is the jwt_sessions row's
// primary key — callers use it as a sessionID when building an
// agentpush.WithAuth context so that InjectSecrets' subsequent
// GetDEK(sessionID, matchedSigningKey) call hits the Redis cache
// this method just populated. Without returning jti, the caller
// would have no way to reference the DEK just cached, and would
// re-execute the unwrap on every downstream call.
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
//     JWTSessionKEKInfo) and attempt DecryptSecret. First success →
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
func (s *KeyService) GetDEKForUser(ctx context.Context, userID string) (dek []byte, jti string, err error) {
	// Wiring pre-conditions. Both are optional deps (setter-DI);
	// missing either at call time is a wiring bug for the caller's
	// use case but must not panic — degrade to the same sentinel
	// the "no session" case uses.
	if s.jwtSessions == nil || s.signingKeys == nil {
		return nil, "", ErrDEKUnavailable
	}

	rows, err := s.jwtSessions.ListActiveJWTSessionsForUser(ctx, userID, jwtSessionUserLookupLimit)
	if err != nil {
		return nil, "", fmt.Errorf("list active jwt_sessions for user: %w", err)
	}
	if len(rows) == 0 {
		return nil, "", ErrDEKUnavailable
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
			return cached, jtiStr, nil
		}

		// Slow path: iterate signing keys, try each.
		if dek := s.tryUnwrapRowWithKnownKeys(ctx, row); dek != nil {
			return dek, jtiStr, nil
		}
	}

	// All rows exhausted without a successful unwrap. Every failure
	// mode (rotated-past-retention-window, corrupted wrap) surfaces as
	// ErrDEKUnavailable so callers handle uniformly.
	return nil, "", ErrDEKUnavailable
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
		dek, uErr := DecryptSecret(kek, row.WrappedDEK)
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
		//
		// Clock is routed through s.nowOr() so tests can inject a
		// deterministic time. Without this, tests that hardcode a
		// baseTs via time.Date() roll off wall-clock's "now" once
		// the calendar moves past the fixture date, breaking every
		// subsequent CI run for reasons unrelated to code changes.
		if cacheTTL := row.ExpiresAt.Sub(s.nowOr()); cacheTTL > 0 {
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

// serverSideDEKCacheTTL bounds the synthetic cache handle GetDEKServerSide
// writes. The handle only needs to outlive one request chain (bootstrap
// or push: unwrap → InjectSecrets → decryptBinding's GetDEK); five
// minutes is generous margin while keeping stray handles short-lived.
// The synthetic jti has no jwt_sessions row, so an expired handle can
// never be rehydrated — single-use by design.
const serverSideDEKCacheTTL = 5 * time.Minute

// GetDEKServerSide unwraps the user's DEK directly from the user_keys
// record via the master RootKeyProvider — no session, no jwt_sessions
// walk, no signing-key retention window.
//
// This is the pod-delivery accessor under the "if the pod exists, it
// has its secrets" contract: the server-KEK-only model made every user
// DEK server-recoverable at rest, so pod delivery no longer gates on an
// active session. Suspend/resume, expired jwt_sessions, and logged-out
// owners all still receive their bound secrets.
//
// Self-heal (legacy rows): when the master provider cannot unwrap
// record.WrappedDEK — a pre-US-57.1 un-prefixed blob, or a wrap from a
// key version the provider no longer derives — the method falls back to
// GetDEKForUser (warm session cache / jwt_sessions unwrap) for the DEK
// material and opportunistically re-wraps the row at the provider's
// active version via UpdateWrappedDEK. The heal is login-independent:
// any background path touching the row converges it to current format,
// which is exactly the gap that stranded pre-rotation users (their last
// login predated the re-wrap window, so a login-gated migration could
// never reach them). A failed heal write is logged, not returned — the
// DEK is still delivered this boot; the next attempt retries.
//
// Returns (dek, jti, error). The jti is a fresh UUID referencing the
// Redis cache entry this method populates; callers pass it as the
// sessionID to InjectSecrets so the downstream GetDEK(jti) call hits
// the cache — the same handoff pattern GetDEKForUser established.
//
// Failure semantics (each degrades the caller to sessionless):
//
//   - rootKeyProvider unwired  → ErrServerKEKUnavailable
//   - no user_keys record      → ErrDEKUnavailable (the user never
//     created secrets; the sessionless payload already contains
//     everything that exists for them)
//   - store/unwrap failure     → wrapped error (infra); includes the
//     session-fallback error when the self-heal path also found no
//     session DEK
//   - cache write failure      → error (without the handle,
//     InjectSecrets cannot reach the DEK; one clean degrade beats
//     per-binding audit noise)
func (s *KeyService) GetDEKServerSide(ctx context.Context, userID string) (dek []byte, jti string, err error) {
	if s.rootKeyProvider == nil {
		return nil, "", ErrServerKEKUnavailable
	}
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return nil, "", fmt.Errorf("get user key: %w", err)
	}
	if record == nil {
		return nil, "", ErrDEKUnavailable
	}
	dek, derr := s.rootKeyProvider.Decrypt(ctx, record.WrappedDEK)
	if derr != nil {
		recovered, _, serr := s.healLegacyDEK(ctx, userID, derr)
		if serr != nil {
			return nil, "", fmt.Errorf("server-kek unwrap DEK: %w (session fallback: %v)", derr, serr)
		}
		dek = recovered
	}
	jti = uuid.NewString()
	if cErr := s.cache.CacheDEK(ctx, jti, dek, serverSideDEKCacheTTL); cErr != nil {
		if s.logger != nil {
			s.logger.Warn("GetDEKServerSide: cache handle write failed; degrading",
				"userID", userID, "error", cErr.Error())
		}
		return nil, "", fmt.Errorf("cache dek handle: %w", cErr)
	}
	return dek, jti, nil
}

// healLegacyDEK recovers the DEK for an unwrappable user_keys row from
// the session path and re-wraps the row at the provider's active
// version. Returns the recovered DEK, or the session-path error when no
// session DEK exists either (caller degrades). The re-wrap is
// best-effort: a write failure is logged and the DEK is still returned.
func (s *KeyService) healLegacyDEK(ctx context.Context, userID string, unwrapErr error) ([]byte, string, error) {
	dek, _, err := s.GetDEKForUser(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	newWrap, werr := s.rootKeyProvider.Encrypt(ctx, dek)
	if werr != nil {
		if s.logger != nil {
			s.logger.Warn("GetDEKServerSide: legacy DEK row heal re-wrap failed (DEK still delivered)",
				"userID", userID, "error", werr.Error())
		}
		return dek, "", nil
	}
	// Verify-after-write (design 0052 §4.5): never commit a wrap that does
	// not round-trip under the provider that produced it. A corrupt wrap
	// here would lose the DEK — and every secret under it — permanently.
	if v, verr := s.rootKeyProvider.Decrypt(ctx, newWrap); verr != nil || !bytes.Equal(v, dek) {
		if s.logger != nil {
			s.logger.Warn("GetDEKServerSide: legacy DEK row heal verify-after-write failed (row untouched, DEK still delivered)",
				"userID", userID, "decryptError", fmt.Sprintf("%v", verr))
		}
		return dek, "", nil
	}
	if uerr := s.store.UpdateWrappedDEK(ctx, userID, newWrap, nil, ActiveVersionOf(s.rootKeyProvider)); uerr != nil {
		if s.logger != nil {
			s.logger.Warn("GetDEKServerSide: legacy DEK row heal write failed (DEK still delivered)",
				"userID", userID, "error", uerr.Error())
		}
		return dek, "", nil
	}
	if s.logger != nil {
		s.logger.Info("GetDEKServerSide: healed legacy user_keys DEK row (re-wrapped at active version)",
			"userID", userID, "keyVersion", ActiveVersionOf(s.rootKeyProvider), "unwrapError", unwrapErr.Error())
	}
	return dek, "", nil
}

// HasKeys checks if a user has key material initialized.
func (s *KeyService) HasKeys(ctx context.Context, userID string) (bool, error) {
	record, err := s.store.GetUserKey(ctx, userID)
	if err != nil {
		return false, err
	}
	return record != nil, nil
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
