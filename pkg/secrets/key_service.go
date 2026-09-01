// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/google/uuid"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

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
	// jwtSessions is the durable per-JWT session store. Login no longer
	// writes rows (US-70.5 removed the durable DEK write half); the
	// store remains the enumeration source for GetCachedDEKForUser and
	// the cleanup target for EvictDEK / DeleteDurableSessionsForUser
	// while pre-demolition rows age out. Optional — when nil,
	// GetCachedDEKForUser returns ErrDEKUnavailable.
	jwtSessions JWTSessionStore
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
// GetCachedDEKForUser enumeration. Optional — tests and pre-Epic-56
// callers may leave it nil; GetCachedDEKForUser then returns
// ErrDEKUnavailable.
//
// Like SetSecretStore, silent rebinding to a different store is refused:
// the enumeration would otherwise read from a store that holds no rows
// for the active session set, surfacing as a wave of
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

// UnlockDEK unwraps the DEK via the master RootKeyProvider and caches it
// in Redis (K1). Called during login and by the soft-unlock endpoint.
// sessionID is the JWT's jti claim. The password parameter is ignored
// (server-KEK-only model) and retained for call-site stability.
//
// US-70.5 removed the durable jwt_sessions write half: login populates
// the Redis cache only. Background recovery is GetCachedDEKForUser
// (warm-cache walk); the jwt_sessions enumeration it uses ages out with
// pre-demolition rows.
func (s *KeyService) UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error {
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
	// (some caches store the slice without copying); zeroing it at return
	// would corrupt the cached DEK. The codebase convention is to zero the
	// derived KEK, never the DEK itself.

	if err := s.cache.CacheDEK(ctx, sessionID, dek, ttl); err != nil {
		return fmt.Errorf("cache DEK: %w", err)
	}
	return nil
}

// EvictDEK removes the cached DEK for a session AND the durable
// jwt_sessions row, if one exists (pre-US-70.5 rows age out via the
// janitor; login no longer writes them). Called on logout / explicit
// revocation. Non-JTI sessionIDs (API-key sessions like "apikey:hash")
// only evict the Redis cache — the api_keys table is the durable home
// for those.
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
// admin force-logout) so rows from pre-US-70.5 logins cannot outlive
// the user's explicit invalidation of every outstanding session.
// Best-effort: failure is logged but does not
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

// GetDEK retrieves the DEK for a session from the Redis cache (K1).
// US-70.5 deleted the durable jwt_sessions rehydrate fallback (K2):
// a cache miss or cache error surfaces ErrDEKUnavailable, and the
// client recovers via the soft-unlock endpoint (which re-derives the
// DEK from user_keys server-side and re-populates the cache).
//
// matchedSigningKey is retained in the signature for call-site
// stability and ignored — the same convention as UnlockDEK's password
// parameter.
func (s *KeyService) GetDEK(ctx context.Context, sessionID string, _ []byte) ([]byte, error) {
	dek, err := s.cache.GetDEK(ctx, sessionID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("Redis DEK lookup failed", "error", err.Error())
		}
		return nil, ErrDEKUnavailable
	}
	if dek == nil {
		return nil, ErrDEKUnavailable
	}
	return dek, nil
}

// DEKAvailable checks if a DEK is cached for the given session.
func (s *KeyService) DEKAvailable(ctx context.Context, sessionID string) bool {
	dek, err := s.cache.GetDEK(ctx, sessionID)
	return err == nil && dek != nil
}

// GetCachedDEKForUser retrieves the user's DEK from the Redis session
// cache (K1) without requiring a caller-supplied sessionID. Designed
// for background paths (keyrewrap recovery, GetDEKServerSide's legacy
// row heal) that do not run in an authenticated user-request context.
//
// Resolution order (US-70.5, the K3 demolition):
//
//  1. jwtSessions.ListActiveJWTSessionsForUser(userID, LIMIT) →
//     candidate jtis, most-recent first. If empty → ErrDEKUnavailable.
//  2. For each jti, read the Redis dek:<jti> cache. First hit → return
//     (dek, jti, nil).
//  3. No cache hit on any enumerated jti → ErrDEKUnavailable. The
//     method NEVER re-derives KEKs from durable jwt_sessions rows and
//     never unwraps — a row that could be unwrapped is not a source.
//
// Login populates the cache (K1 survives); login no longer writes
// jwt_sessions rows (K4 write half removed), so the enumeration
// reaches pre-demolition rows only — the warm-cache recovery window
// ages out with them, after which callers see ErrDEKUnavailable and
// surface their no-source outcome (keyrewrap: unwrappable_no_source).
//
// Rows are bounded (LIMIT jwtSessionUserLookupLimit).
//
// Errors: ErrDEKUnavailable covers every legitimate "no warm cache
// reachable" case. Genuine infrastructure errors (PG connection
// failure) are returned verbatim so operators can distinguish
// debug-worthy outages from expected "user logged out" cases.
func (s *KeyService) GetCachedDEKForUser(ctx context.Context, userID string) (dek []byte, jti string, err error) {
	if s.jwtSessions == nil {
		return nil, "", ErrDEKUnavailable
	}

	rows, err := s.jwtSessions.ListActiveJWTSessionsForUser(ctx, userID, jwtSessionUserLookupLimit)
	if err != nil {
		return nil, "", fmt.Errorf("list active jwt_sessions for user: %w", err)
	}

	for _, row := range rows {
		jtiStr := row.JTI.String()
		// Redis errors (not misses) are logged and treated as miss for
		// that jti — the walk continues to older rows, and an exhausted
		// walk surfaces ErrDEKUnavailable. There is no unwrap fallback.
		if cached, cErr := s.cache.GetDEK(ctx, jtiStr); cErr != nil {
			if s.logger != nil {
				s.logger.Warn("GetCachedDEKForUser: Redis DEK lookup failed",
					"jti", jtiStr, "error", cErr.Error())
			}
		} else if cached != nil {
			return cached, jtiStr, nil
		}
	}
	return nil, "", ErrDEKUnavailable
}

// jwtSessionUserLookupLimit caps how many jwt_sessions rows
// GetCachedDEKForUser examines for a single user. A well-behaved user
// has 1-3 concurrent sessions (web + mobile + workstation); the limit
// bounds the enumeration cost against pathological session counts.
const jwtSessionUserLookupLimit = 5

// serverSideDEKCacheTTL bounds the synthetic cache handle GetDEKServerSide
// writes. The handle only needs to outlive one request chain (bootstrap
// or push: unwrap → batch build); five
// minutes is generous margin while keeping stray handles short-lived.
// The synthetic jti has no jwt_sessions row, so an expired handle is
// unreachable after expiry — single-use by design.
const serverSideDEKCacheTTL = 5 * time.Minute

// GetDEKServerSide unwraps the user's DEK directly from the user_keys
// record via the master RootKeyProvider — no session, no jwt_sessions
// walk.
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
// GetCachedDEKForUser (warm session cache) for the DEK material and
// opportunistically re-wraps the row at the provider's active version
// via UpdateWrappedDEK. The heal is login-independent: any background
// path touching the row converges it to current format, which is
// exactly the gap that stranded pre-rotation users (their last login
// predated the re-wrap window, so a login-gated migration could never
// reach them). A failed heal write is logged, not returned — the DEK
// is still delivered this boot; the next attempt retries. No warm
// cache reachable → the unwrap error propagates and the caller
// degrades to sessionless (loudly).
//
// Returns (dek, jti, error). The jti is a fresh UUID referencing the
// Redis cache entry this method populates; callers that need a
// session-indexed handle may use it with GetDEK(jti). The batch
// builder consumes the returned dek directly.
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
//     session-indexed consumers cannot reach the DEK; one clean
//     degrade beats per-binding audit noise)
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
	dek, _, err := s.GetCachedDEKForUser(ctx, userID)
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
