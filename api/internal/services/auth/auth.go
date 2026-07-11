// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/utilities"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
)

// KeyServiceInterface abstracts the key service for DEK lifecycle.
type KeyServiceInterface interface {
	InitializeUserKeys(ctx context.Context, userID string, password []byte) (recoveryKeyHex string, err error)
	UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error
	// UnlockDEKWithSigningKey is UnlockDEK + durable jwt_sessions write
	// (Epic 56). Login calls this with the active signing key (s.jwtSecret)
	// so the unlocked DEK survives Valkey restart / LRU eviction for the
	// JWT's remaining lifetime. Pass nil to fall back to Redis-only.
	UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, activeSigningKey []byte) error
	HasKeys(ctx context.Context, userID string) (bool, error)
	// GetDEK (Epic 56) takes the matched signing key so the rehydrate path
	// can derive the per-session KEK from the same key the JWT validated
	// under. Pass nil for API-key callers — rehydrate is skipped and
	// ErrDEKUnavailable is returned (correct: API keys have their own
	// durable DEK path via api_keys.WrappedDEK).
	GetDEK(ctx context.Context, sessionID string, matchedSigningKey []byte) ([]byte, error)
	CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error
	// DeleteDurableSessionsForUser (Epic 56) removes every jwt_sessions
	// row for a user. Called by RevokeAllUserSessions to keep the
	// durable store consistent with the Redis revocation markers — without
	// this, a stolen JWT could still rehydrate the DEK from PG after the
	// victim resets their password.
	DeleteDurableSessionsForUser(ctx context.Context, userID string) error
}

// SetKeyService sets the optional key service for secret management.
func (s *Service) SetKeyService(ks KeyServiceInterface) {
	s.keyService = ks
}

// EmailVerifier creates and sends email-verification tokens for new users.
// When set, Register creates an unverified account (email_verified=false)
// and calls Verify to send the verification link. When nil, Register marks
// the account email_verified=true immediately (dev/air-gapped mode — no
// email provider to verify with).
type EmailVerifier interface {
	SendVerification(ctx context.Context, userID, email string) error
}

// ErrEmailNotVerified is returned by Login when the credentials are correct
// but the user has not verified their email address. The caller (handler)
// maps this to 403 with a clear message directing the user to check their
// email. Not recorded as a failed login attempt (the credentials are valid).
var ErrEmailNotVerified = errors.New("please verify your email address before logging in")

// SetEmailVerifier wires the email-verification hook. Optional — nil means
// Register auto-verifies (dev mode without an email provider).
func (s *Service) SetEmailVerifier(v EmailVerifier) {
	s.emailVerifier = v
}

// SetInstanceSettings injects the instance settings service for runtime config reads.
func (s *Service) SetInstanceSettings(svc interfaces.SettingsReader) {
	s.instanceSettings = svc
}

// SetMasterKey sets the server master key used for encrypting API key ciphertext
// (enabling DEK re-wrap on rotation). Derived from LLMSAFESPACES_MASTER_SECRET.
func (s *Service) SetMasterKey(key []byte) {
	provider, err := secrets.NewStaticKeyProvider(key)
	if err != nil {
		return
	}
	s.rootKeyProvider = provider
}

// SetRootKeyProvider sets the RootKeyProvider for API key at-rest encryption.
func (s *Service) SetRootKeyProvider(provider secrets.RootKeyProvider) {
	s.rootKeyProvider = provider
}

const defaultAPIKeyDEKTTL = 24 * time.Hour

func (s *Service) apiKeyDEKTTL() time.Duration {
	if s.config.Auth.APIKeyDEKTTL > 0 {
		return s.config.Auth.APIKeyDEKTTL
	}
	return defaultAPIKeyDEKTTL
}

// lockoutConfig reads lockout configuration from instance settings (if available),
// falling back to static config values.
func (s *Service) lockoutConfig(ctx context.Context) (enabled bool, attempts int, duration time.Duration) {
	enabled = s.config.Auth.LockoutEnabled
	attempts = s.config.Auth.LockoutAttempts
	duration = s.config.Auth.LockoutDuration

	if s.instanceSettings == nil {
		return
	}
	if v, err := s.instanceSettings.GetBool(ctx, settings.KeyAuthLockoutEnabled.Name()); err == nil {
		enabled = v
	}
	if v, err := s.instanceSettings.GetInt(ctx, settings.KeyAuthLockoutAttempts.Name()); err == nil && v > 0 {
		attempts = v
	}
	if v, err := s.instanceSettings.GetInt(ctx, settings.KeyAuthLockoutDurationMinutes.Name()); err == nil && v > 0 {
		duration = time.Duration(v) * time.Minute
	}
	return
}

// Service handles authentication and authorization
type Service struct {
	logger       *logger.Logger
	config       *config.Config
	dbService    interfaces.DatabaseService
	cacheService interfaces.CacheService
	// jwtSecret is the active signing key. New tokens are always
	// signed with this key.
	jwtSecret []byte
	// jwtPreviousSecrets are previous signing keys retained for
	// validation only. Tokens signed with any of these are still
	// accepted (so existing sessions don't get logged out at the
	// moment of key rotation), but only `jwtSecret` is used for
	// new tokens. F1.7.5 (Epic 17): operator-driven key rotation.
	jwtPreviousSecrets [][]byte
	tokenDuration      time.Duration
	// maxTokenTTL is the maximum lifetime of any token the service issues
	// (max(tokenDuration, rememberMeDuration)). Used as the TTL for the F4
	// user-suspension marker so it outlives EVERY outstanding token — including
	// remember-me tokens (720h default), which outlast standard tokens (24h).
	maxTokenTTL      time.Duration
	keyService       KeyServiceInterface
	emailVerifier    EmailVerifier
	instanceSettings interfaces.SettingsReader
	rootKeyProvider  secrets.RootKeyProvider
}

// Start initializes the auth service
func (s *Service) Start() error {
	return nil
}

// Stop cleans up the auth service
func (s *Service) Stop() error {
	return nil
}

func (s *Service) AuthenticateAPIKey(ctx context.Context, apiKey string) (string, error) {
	// Check if API key is cached
	cacheKey := fmt.Sprintf("apikey:%s", pkgutil.HashString(apiKey))

	// Try to get from cache first
	cachedStatus, err := s.cacheService.Get(ctx, cacheKey)
	if err == nil && cachedStatus != "" {
		if cachedStatus == "revoked" {
			return "", errors.New("token has been revoked")
		}
		return cachedStatus, nil
	}

	// Hash-first lookup (new keys). Fall back to plaintext for legacy keys. (Epic 10 US-10.13)
	h := sha256.Sum256([]byte(apiKey))
	keyHash := hex.EncodeToString(h[:])
	user, err := s.dbService.GetUserByAPIKey(ctx, keyHash)
	if err != nil {
		return "", fmt.Errorf("failed to authenticate API key: %w", err)
	}
	if user == nil {
		// Legacy plaintext fallback — only for pre-000017 keys (short tokens).
		// Real API tokens are 64-char hex hashes, not plaintext.
		if len(apiKey) != 64 {
			user, err = s.dbService.GetUserByAPIKey(ctx, apiKey)
			if err != nil {
				return "", fmt.Errorf("failed to authenticate API key: %w", err)
			}
			if user != nil {
				s.logger.Warn("Authenticated via legacy plaintext API key — user should rotate", "user_id", user.ID)
			}
		}
	}

	if user == nil {
		return "", errors.New("invalid API key")
	}

	// Cache the API key for 15 minutes
	err = s.cacheService.Set(ctx, cacheKey, user.ID, 15*time.Minute)
	if err != nil {
		s.logger.Error("Failed to cache API key", err, "user_id", user.ID)
		// Continue even if caching fails
	}

	return user.ID, nil
}

// Note: The redundant AuthMiddleware method has been removed as it duplicates
// functionality in the middleware package

// New creates a new auth service
func New(cfg *config.Config, log *logger.Logger, dbService interfaces.DatabaseService, cacheService interfaces.CacheService) (*Service, error) {
	if cfg.Auth.JWTSecret == "" {
		return nil, errors.New("JWT secret is required")
	}

	// Default iss/aud. Production deploys go through Load(), which applies
	// these defaults at config-load time. Tests construct Service directly
	// and would otherwise see empty values. Either way the values must be
	// set before the Service is constructed.
	if cfg.Auth.JWTIssuer == "" {
		cfg.Auth.JWTIssuer = config.DefaultJWTIssuer
	}
	if cfg.Auth.JWTAudience == "" {
		cfg.Auth.JWTAudience = config.DefaultJWTAudience
	}

	// Warn when rememberMeDuration is set but shorter than tokenDuration —
	// this means remember-me sessions would expire sooner than standard sessions,
	// almost certainly a misconfiguration. We allow it (could be intentional
	// during incident response) but make it visible at startup.
	if cfg.Auth.RememberMeDuration > 0 && cfg.Auth.RememberMeDuration < cfg.Auth.TokenDuration {
		log.Warn("auth: rememberMeDuration is shorter than tokenDuration; "+
			"remember-me sessions will expire sooner than standard sessions — check your configuration",
			"rememberMeDuration", cfg.Auth.RememberMeDuration,
			"tokenDuration", cfg.Auth.TokenDuration)
	}

	prev := make([][]byte, 0, len(cfg.Auth.JWTPreviousSecrets))
	for _, p := range cfg.Auth.JWTPreviousSecrets {
		if p != "" {
			prev = append(prev, []byte(p))
		}
	}

	return &Service{
		logger:             log,
		config:             cfg,
		dbService:          dbService,
		cacheService:       cacheService,
		jwtSecret:          []byte(cfg.Auth.JWTSecret),
		jwtPreviousSecrets: prev,
		tokenDuration:      cfg.Auth.TokenDuration,
		// F4: the suspension marker must outlive the longest-lived token so it
		// stays enforceable for remember-me sessions too. rememberMeDuration
		// defaults to 720h vs tokenDuration's 24h; take the max so the marker
		// never expires before a still-valid token would.
		maxTokenTTL: suspensionMarkerTTL(cfg.Auth.TokenDuration, cfg.Auth.RememberMeDuration),
	}, nil
}

// suspensionMarkerTTL returns the TTL the F4 revocation marker must use so it
// covers every outstanding token. It is max(tokenDuration, rememberMeDuration)
// so remember-me sessions (which outlast standard tokens) stay gated until the
// marker is explicitly cleared by UnsuspendUser or natural token expiry.
func suspensionMarkerTTL(tokenDuration, rememberMeDuration time.Duration) time.Duration {
	if rememberMeDuration > tokenDuration {
		return rememberMeDuration
	}
	return tokenDuration
}

// GetUserID gets the user ID from the context
func (s *Service) GetUserID(c *gin.Context) string {
	userID, exists := c.Get("userID")
	if !exists {
		return ""
	}
	return userID.(string)
}

// userSuspendedKey is the Redis key holding the per-user revocation marker
// written by MarkUserSuspended. A live value means "deny this user's currently
// issued tokens immediately, without a DB lookup" (F4, US-43.19).
func userSuspendedKey(userID string) string { return "user_suspended:" + userID }

// MarkUserSuspended writes a per-user revocation marker so the auth middleware
// rejects the user's existing JWTs/API keys the instant the admin suspends them,
// without waiting for the next per-request GetUser or depending on the DB (which
// may be briefly unavailable). The TTL is max(tokenDuration, rememberMeDuration)
// so the marker outlives every outstanding token — including remember-me
// sessions (720h default), which outlast standard tokens (24h). Unsuspends call
// ClearUserSuspended for an immediate recovery (no TTL wait).
func (s *Service) MarkUserSuspended(ctx context.Context, userID string) error {
	if err := s.cacheService.Set(ctx, userSuspendedKey(userID), "1", s.maxTokenTTL); err != nil {
		return fmt.Errorf("failed to mark user suspended: %w", err)
	}
	return nil
}

// ClearUserSuspended removes the revocation marker so an unsuspended user's
// existing tokens work again immediately (no TTL wait).
func (s *Service) ClearUserSuspended(ctx context.Context, userID string) error {
	if err := s.cacheService.Delete(ctx, userSuspendedKey(userID)); err != nil {
		return fmt.Errorf("failed to clear user suspended marker: %w", err)
	}
	return nil
}

// isUserSuspendedCached reports whether a live revocation marker exists for the
// user. A miss (or Redis error) returns false — the authoritative GetUser check
// in the middleware still runs and fail-closes on DB error. This is purely a
// fast-path + DB-outage-resilience layer, never the sole enforcement.
func (s *Service) isUserSuspendedCached(ctx context.Context, userID string) bool {
	v, err := s.cacheService.Get(ctx, userSuspendedKey(userID))
	return err == nil && v != ""
}

// RevokeToken revokes a JWT token. ctx propagates the caller's
// deadline/cancellation into the cache calls (US-46.5 / #224 P2); the 5s cap
// is retained as a per-call safety bound derived from ctx.
func (s *Service) RevokeToken(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Parse token (accepts active key or any previous key for F1.7.5).
	// RevokeToken only needs claims; the matched-key + index returns added
	// for Epic 56 are intentionally discarded here.
	parsedToken, _, _, err := s.parseTokenAcceptingRotatedKeys(token)

	if err != nil {
		return fmt.Errorf("failed to parse token: %w", err)
	}

	// Get claims
	claims, ok := parsedToken.Claims.(jwt.MapClaims)
	if !ok {
		return errors.New("invalid token claims")
	}

	// Get token ID with proper validation
	jti, _ := claims["jti"].(string)
	if jti == "" {
		if sub, ok := claims["sub"].(string); ok && sub != "" {
			jti = sub
		} else {
			return errors.New("token missing valid jti or sub claim")
		}
	}

	// Get expiration time
	exp, ok := claims["exp"].(float64)
	if !ok {
		return errors.New("invalid expiration time in token")
	}

	// Calculate remaining time until expiration
	expTime := time.Unix(int64(exp), 0)
	remainingTime := time.Until(expTime)

	if remainingTime <= 0 {
		return errors.New("token has already expired")
	}

	// G18 (Epic 17): Add token to blacklist under BOTH cache keys so the
	// revocation is visible to:
	//   1. ValidateToken's hash-based cache fast-path (token:<hash(token)>)
	//   2. ValidateToken's jti-based revocation check (token:<jti>)
	// Without writing both, ValidateToken's fast-path would still return the
	// cached userID and revocation would be silently ignored. See worklog 0078
	// and `auth_revocation_test.go` for the regression that locks this in.
	hashKey := "token:" + pkgutil.HashString(token)
	jtiKey := "token:" + jti
	if err := s.cacheService.Set(ctx, hashKey, "revoked", remainingTime); err != nil {
		return fmt.Errorf("failed to revoke token (hash key): %w", err)
	}
	if err := s.cacheService.Set(ctx, jtiKey, "revoked", remainingTime); err != nil {
		// Best-effort cleanup of the hash key so we don't leak a half-revoked
		// state. If the cleanup itself fails, log it; the hash key has the
		// same TTL as the JWT so it will expire on its own.
		if cleanupErr := s.cacheService.Delete(ctx, hashKey); cleanupErr != nil {
			s.logger.Error("Failed to cleanup hash-key after jti-key revoke failure",
				cleanupErr, "hash_key", hashKey)
		}
		return fmt.Errorf("failed to revoke token (jti key): %w", err)
	}

	return nil
}

// CheckResourceAccess checks if a user has access to a resource
func (s *Service) CheckResourceAccess(userID, resourceType, resourceID, action string) bool {
	// CheckResourceAccess has no production callers (interface-satisfying only);
	// it uses context.Background() here pending either deletion or a future
	// ctx-propagating signature. The live CheckResourceOwnership caller in
	// handlers/usage.go uses the request ctx.
	isOwner, err := s.dbService.CheckResourceOwnership(context.Background(), userID, resourceType, resourceID)
	if err != nil {
		s.logger.Error("Failed to check resource ownership", err,
			"user_id", userID,
			"resource_type", resourceType,
			"resource_id", resourceID,
		)
		return false
	}

	if isOwner {
		return true
	}

	// Check RBAC permissions
	hasPermission, err := s.dbService.CheckPermission(context.Background(), userID, resourceType, resourceID, action)
	if err != nil {
		s.logger.Error("Failed to check permission", err,
			"user_id", userID,
			"resource_type", resourceType,
			"resource_id", resourceID,
			"action", action,
		)
		return false
	}

	return hasPermission
}

// GenerateToken generates a JWT token for a user using the configured tokenDuration.
// It delegates to GenerateTokenWithDuration, which is the canonical implementation.
func (s *Service) GenerateToken(userID string) (string, error) {
	return s.GenerateTokenWithDuration(userID, s.tokenDuration)
}

// GenerateTokenWithDuration generates a JWT token for a user with an explicit TTL.
// This is the canonical token-generation implementation; GenerateToken delegates here.
// Not exposed on the AuthService interface — callers outside the auth package use
// GenerateToken, which always uses the configured tokenDuration.
func (s *Service) GenerateTokenWithDuration(userID string, duration time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"sub": userID,
		"jti": uuid.New().String(),
		"exp": time.Now().Add(duration).Unix(),
		"iat": time.Now().Unix(),
	}
	if iss := s.config.Auth.JWTIssuer; iss != "" {
		claims["iss"] = iss
	}
	if aud := s.config.Auth.JWTAudience; aud != "" {
		claims["aud"] = aud
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	tokenString, err := token.SignedString(s.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return tokenString, nil
}

// ValidateToken validates a JWT token or API key.
func (s *Service) ValidateToken(ctx context.Context, tokenString string) (string, error) {
	return s.ValidateTokenWithClientIP(ctx, tokenString, "")
}

// ValidateTokenWithClientIP validates a JWT token or API key, enforcing
// allowed_cidrs when clientIP is non-empty. ctx propagates the caller's
// deadline/cancellation into the cache + DB calls (US-46.5 / issue #224);
// the 5s cap is retained as a per-call safety bound derived from ctx.
//
// The token-validation cache value uses the format "userID|matchedKeyIndex"
// (Epic 56 Step 3) so a cache hit can surface the matched signing-key
// index without re-parsing the JWT. Legacy entries (pre-deploy) are bare
// "userID"; the reader treats them as matchedKeyIndex = -1 ("unknown,
// caller must re-parse if it needs the key"). The "revoked" sentinel
// keeps its original meaning.
func (s *Service) ValidateTokenWithClientIP(ctx context.Context, tokenString, clientIP string) (string, error) {
	uid, _, err := s.validateTokenAndMatchedKey(ctx, tokenString, clientIP)
	return uid, err
}

// validateTokenAndMatchedKey is the full-fat validation entry point used
// by AuthMiddleware (Epic 56 Step 4) when it needs the matched signing-key
// index alongside the userID. Returns (userID, matchedKeyIdx, err); a
// matchedKeyIdx of -1 means "unknown" — either the token is an API key
// (matched-key concept doesn't apply) or the legacy cache hit didn't
// preserve it. Callers that need the actual signing-key bytes must
// resolve idx → key via s.signingKeyByIndex().
func (s *Service) validateTokenAndMatchedKey(ctx context.Context, tokenString, clientIP string) (string, int, error) {
	if utilities.IsAPIKey(tokenString, s.config.Auth.APIKeyPrefix) {
		uid, err := s.validateAPIKey(ctx, tokenString, clientIP)
		return uid, -1, err
	}

	// Check if token is cached
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cacheKey := fmt.Sprintf("token:%s", pkgutil.HashString(tokenString))

	// Try to get from cache first
	if cachedValue, err := s.cacheService.Get(ctx, cacheKey); err == nil && cachedValue != "" {
		uid, idx, ok, revoked := parseValidationCacheValue(cachedValue)
		if revoked {
			return "", -1, errors.New("token has been revoked")
		}
		if ok {
			return uid, idx, nil
		}
		// Unparseable cache value — log + fall through to full parse.
		s.logger.Warn("Unparseable token cache value; falling through to JWT parse",
			"raw_value_len", len(cachedValue))
	}

	// Parse token (accepts active key or any previous key for F1.7.5).
	// Epic 56: capture matched key index so downstream rehydrate paths
	// can derive the per-session KEK from the same key the JWT validated
	// under (the [HIGH] regression case from #411 review pass 1).
	token, _, matchedIdx, err := s.parseTokenAcceptingRotatedKeys(tokenString)
	if err != nil {
		return "", -1, fmt.Errorf("failed to parse token: %w", err)
	}

	// Validate token
	if !token.Valid {
		return "", -1, errors.New("invalid token")
	}

	// Get claims
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", -1, errors.New("invalid token claims")
	}

	// Get user ID
	userID, ok := claims["sub"].(string)
	if !ok {
		return "", -1, errors.New("invalid user ID in token")
	}

	// iss/aud enforcement: tokens must carry the configured issuer and
	// audience. Pre-fix tokens (no iss/aud) are rejected — backward-
	// compatibility break, but tokens are short-lived (24h default) so
	// rotation is fast. Defense against any other service sharing the
	// same HMAC secret minting accepted tokens.
	if want := s.config.Auth.JWTIssuer; want != "" {
		iss, _ := claims["iss"].(string)
		if iss != want {
			return "", -1, fmt.Errorf("token issuer %q does not match expected %q", iss, want)
		}
	}
	if want := s.config.Auth.JWTAudience; want != "" {
		// aud may be a string or []string; accept either shape.
		switch aud := claims["aud"].(type) {
		case string:
			if aud != want {
				return "", -1, fmt.Errorf("token audience %q does not match expected %q", aud, want)
			}
		case []any:
			matched := false
			for _, a := range aud {
				if s, ok := a.(string); ok && s == want {
					matched = true
					break
				}
			}
			if !matched {
				return "", -1, fmt.Errorf("token audience %v does not include expected %q", aud, want)
			}
		default:
			return "", -1, fmt.Errorf("token missing or invalid audience claim; expected %q", want)
		}
	}

	// G18 (Epic 17): Defense-in-depth revocation check by jti AFTER parsing.
	// RevokeToken stores under both token:<hash> (fast-path above) AND
	// token:<jti> (this check). The jti check protects against eviction of
	// the hash-key cache entry (e.g., Redis memory pressure) — without it,
	// revocation could be silently bypassed under cache pressure.
	if jti, ok := claims["jti"].(string); ok && jti != "" {
		if status, gerr := s.cacheService.Get(ctx, "token:"+jti); gerr == nil && status == "revoked" {
			return "", -1, errors.New("token has been revoked")
		}
	}

	// Get expiration time
	exp, ok := claims["exp"].(float64)
	if !ok {
		return "", -1, errors.New("invalid expiration time in token")
	}

	// Calculate remaining time until expiration
	expTime := time.Unix(int64(exp), 0)
	remainingTime := time.Until(expTime)

	// Cache the token if it's valid
	if remainingTime > 0 {
		// Cache for the remaining time of the token, but not more than 1 hour
		cacheDuration := remainingTime
		if cacheDuration > time.Hour {
			cacheDuration = time.Hour
		}

		err = s.cacheService.Set(ctx, cacheKey, formatValidationCacheValue(userID, matchedIdx), cacheDuration)
		if err != nil {
			s.logger.Error("Failed to cache token", err, "user_id", userID)
			// Continue even if caching fails
		}
	}

	return userID, matchedIdx, nil
}

// signingKeyByIndex resolves a matched-key index back to the actual
// signing-key bytes. Returns nil for idx == -1 (unknown), API-key paths,
// or an out-of-range index (which can happen on a Helm rotation that
// shortens jwtPreviousSecrets while pre-rotation tokens are still valid;
// the caller's auto-rehydrate path must then fall through to soft-unlock).
//
// The returned slice is a defensive copy — callers cannot mutate
// s.jwtSecret or s.jwtPreviousSecrets via the return value.
func (s *Service) signingKeyByIndex(idx int) []byte {
	if idx < 0 {
		return nil
	}
	if idx == 0 {
		out := make([]byte, len(s.jwtSecret))
		copy(out, s.jwtSecret)
		return out
	}
	prev := idx - 1
	if prev >= len(s.jwtPreviousSecrets) {
		return nil
	}
	out := make([]byte, len(s.jwtPreviousSecrets[prev]))
	copy(out, s.jwtPreviousSecrets[prev])
	return out
}

// EachSigningKey satisfies secrets.SigningKeyEnumerator so KeyService's
// GetDEKForUser (used by background/auto-push paths) can iterate the
// same set of active + previous signing keys that parseTokenAcceptingRotatedKeys
// uses at JWT validation time. Primary key first, then previous keys
// in most-recent-rotation-first order.
//
// The callback receives a FRESH COPY of each key on every invocation;
// implementations that store or retain the bytes past the callback
// return must copy again. Zeroed slice is not returned — callers zero
// their own copies.
func (s *Service) EachSigningKey(fn func(key []byte) bool) {
	for i := 0; ; i++ {
		k := s.signingKeyByIndex(i)
		if k == nil {
			return
		}
		if !fn(k) {
			return
		}
	}
}

// formatValidationCacheValue produces the cache value for a successful
// token validation. Format: "userID|matchedKeyIdx". Both fields are
// non-secret — the userID is already public to the request, the matched
// key index is a small integer ≤ len(jwtPreviousSecrets).
func formatValidationCacheValue(userID string, matchedIdx int) string {
	return fmt.Sprintf("%s|%d", userID, matchedIdx)
}

// parseValidationCacheValue inverts formatValidationCacheValue and
// transparently handles three legacy / sentinel cases:
//
//   - bare "userID" (pre-Epic-56 entries) → idx = -1 (unknown)
//   - "revoked" → revoked = true, ok = false
//   - "" → ok = false (cache miss)
//   - "userID|N" where N is not an integer → idx = -1 (recoverable)
//   - "userID|x|y" extra delimiters → idx = -1 (recoverable)
//
// "ok" means "the value identifies an authenticated user"; "revoked"
// means "the cached entry explicitly says this token is revoked"; the
// two are mutually exclusive.
func parseValidationCacheValue(raw string) (userID string, matchedIdx int, ok bool, revoked bool) {
	if raw == "" {
		return "", -1, false, false
	}
	if raw == "revoked" {
		return "", -1, false, true
	}
	// strings.Cut returns ok=false when there's no separator → legacy format.
	uid, rest, found := strings.Cut(raw, "|")
	if !found {
		return raw, -1, true, false
	}
	// New format. We accept and surface uid even when the index suffix is
	// malformed — the userID is still authenticated, the caller just has
	// no matched-key hint (forcing a re-parse on rehydrate paths).
	if rest == "" {
		return uid, -1, true, false
	}
	// Additional pipes → not our format; treat as unknown index but still
	// surface the uid prefix (defensive — value was set by us, schema drift
	// is the only way to get here).
	if strings.Contains(rest, "|") {
		return uid, -1, true, false
	}
	idx, err := strconv.Atoi(rest)
	if err != nil {
		return uid, -1, true, false
	}
	return uid, idx, true, false
}

// validateAPIKey validates an API key (internal method).
// clientIP is optional; when provided, allowed_cidrs is enforced. ctx
// propagates the caller's deadline/cancellation (US-46.5 / issue #224).
func (s *Service) validateAPIKey(ctx context.Context, apiKey, clientIP string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cacheKey := fmt.Sprintf("apikey:%s", pkgutil.HashString(apiKey))

	if cachedUserID, err := s.cacheService.Get(ctx, cacheKey); err == nil && cachedUserID != "" {
		if cachedUserID == "revoked" {
			return "", errors.New("token has been revoked")
		}
		if clientIP != "" && s.rootKeyProvider != nil && utilities.IsAPIKey(apiKey, s.config.Auth.APIKeyPrefix) {
			h := sha256.Sum256([]byte(apiKey))
			keyHash := hex.EncodeToString(h[:])
			keyRec, dbErr := s.dbService.GetAPIKeyRecordByHash(ctx, keyHash)
			if dbErr == nil && keyRec != nil && len(keyRec.AllowedCIDRs) > 0 {
				if !ipInAnyCIDR(clientIP, keyRec.AllowedCIDRs) {
					return "", errors.New("request source IP not in allowed ranges for this key")
				}
			}
		}
		return cachedUserID, nil
	}

	h := sha256.Sum256([]byte(apiKey))
	keyHash := hex.EncodeToString(h[:])
	user, err := s.dbService.GetUserByAPIKey(ctx, keyHash)
	if err != nil {
		return "", fmt.Errorf("failed to get user by API key: %w", err)
	}
	if user == nil && len(apiKey) != 64 {
		user, err = s.dbService.GetUserByAPIKey(ctx, apiKey)
		if err != nil {
			return "", fmt.Errorf("failed to get user by API key: %w", err)
		}
	}

	if user == nil {
		return "", errors.New("invalid API key")
	}

	if s.rootKeyProvider != nil && utilities.IsAPIKey(apiKey, s.config.Auth.APIKeyPrefix) {
		keyRec, dbErr := s.dbService.GetAPIKeyRecordByHash(ctx, keyHash)
		if dbErr != nil {
			s.logger.Error("Failed to get API key record", dbErr, "key_hash", keyHash)
		} else if keyRec != nil {
			if len(keyRec.AllowedCIDRs) > 0 && clientIP != "" {
				if !ipInAnyCIDR(clientIP, keyRec.AllowedCIDRs) {
					return "", errors.New("request source IP not in allowed ranges for this key")
				}
			}

			if len(keyRec.KeyCiphertext) > 0 {
				storedRaw, decErr := s.rootKeyProvider.Decrypt(ctx, keyRec.KeyCiphertext)
				if decErr != nil {
					s.logger.Error("Failed to decrypt key_ciphertext", decErr, "key_id", keyRec.ID)
				} else {
					if subtle.ConstantTimeCompare(storedRaw, []byte(apiKey)) != 1 {
						zeroBytes(storedRaw)
						return "", errors.New("invalid API key")
					}
					zeroBytes(storedRaw)
				}
			}

			if keyRec.DecryptAccess && len(keyRec.WrappedDEK) > 0 && len(keyRec.KekSalt) > 0 {
				if !keyRec.DekSynced {
					s.logger.Warn("API key DEK re-sync in progress", "key_id", keyRec.ID)
				} else {
					apiKEK, deriveErr := secrets.DeriveKEKFromKey([]byte(apiKey), keyRec.KekSalt, "llmsafespaces-apikey-kek")
					if deriveErr != nil {
						s.logger.Error("Failed to derive API KEK", deriveErr)
					} else {
						dek, decErr := secrets.DecryptSecret(apiKEK, keyRec.WrappedDEK)
						if decErr != nil {
							s.logger.Error("Failed to unwrap DEK for API key", decErr, "key_id", keyRec.ID)
						} else {
							sessionID := "apikey:" + pkgutil.HashString(apiKey)
							if cacheErr := s.keyService.CacheDEK(ctx, sessionID, dek, s.apiKeyDEKTTL()); cacheErr != nil {
								s.logger.Error("Failed to cache DEK for API key session", cacheErr, "session_id", sessionID)
							}
						}
					}
				}
			}
		}
	} else if s.keyService != nil && utilities.IsAPIKey(apiKey, s.config.Auth.APIKeyPrefix) {
		keyRec, dbErr := s.dbService.GetAPIKeyRecordByHash(ctx, keyHash)
		if dbErr != nil {
			s.logger.Error("Failed to get API key record for DEK check", dbErr, "key_hash", keyHash)
		} else if keyRec != nil && keyRec.DecryptAccess && len(keyRec.WrappedDEK) > 0 && len(keyRec.KekSalt) > 0 {
			apiKEK, deriveErr := secrets.DeriveKEKFromKey([]byte(apiKey), keyRec.KekSalt, "llmsafespaces-apikey-kek")
			if deriveErr != nil {
				s.logger.Error("Failed to derive API KEK", deriveErr)
			} else {
				dek, decErr := secrets.DecryptSecret(apiKEK, keyRec.WrappedDEK)
				if decErr != nil {
					s.logger.Error("Failed to unwrap DEK for API key", decErr, "key_id", keyRec.ID)
				} else {
					sessionID := "apikey:" + pkgutil.HashString(apiKey)
					if cacheErr := s.keyService.CacheDEK(ctx, sessionID, dek, s.apiKeyDEKTTL()); cacheErr != nil {
						s.logger.Error("Failed to cache DEK for API key session", cacheErr, "session_id", sessionID)
					}
				}
			}
		}
	}

	err = s.cacheService.Set(ctx, cacheKey, user.ID, 15*time.Minute)
	if err != nil {
		s.logger.Error("Failed to cache API key", err, "user_id", user.ID)
	}

	return user.ID, nil
}

const bcryptCost = 12

func (s *Service) Register(ctx context.Context, req types.RegisterRequest) (*types.AuthResponse, error) {
	existing, err := s.dbService.GetUserByEmail(ctx, req.Email)
	if err != nil {
		s.logger.Error("Register: failed to check existing user", err)
		return nil, errors.New("registration failed")
	}
	if existing != nil {
		s.logger.Warn("Register: duplicate email attempt", "email", req.Email)
		return nil, apierrors.NewConflictError("user", "email", fmt.Errorf("registration failed"))
	}

	// G8 (Epic 17): role assignment is now atomic in CreateUser via
	// the SQL CTE that counts existing users in the same statement
	// as the INSERT. We pass "user" as the desired role; the database
	// promotes to "admin" if and only if the user count is 0 at the
	// moment of insert. This eliminates the count-then-insert race
	// where two concurrent Register() calls could both observe count=0
	// and both end up admin.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, errors.New("registration failed")
	}

	userID := uuid.New().String()
	user := &types.User{
		ID:           userID,
		Username:     strings.TrimSpace(req.Username),
		Email:        strings.ToLower(strings.TrimSpace(req.Email)),
		PasswordHash: string(hash),
		Active:       true,
		Role:         "user",
	}

	if err := s.dbService.CreateUser(ctx, user); err != nil {
		s.logger.Error("Register: failed to create user", err)
		return nil, errors.New("registration failed")
	}

	// Initialize encryption keys for secret management (Epic 10).
	//
	// Key initialisation MUST succeed: a half-initialized user (row exists,
	// no DEK) cannot perform any secret operation and login itself cannot
	// recover from this state without re-deriving the KEK from the
	// password (which requires `user_keys` to exist). We therefore fail
	// the entire registration when key init fails.
	//
	// We also unlock the DEK in the same call so the JWT issued below is
	// usable for secret operations immediately. Without this, the new user
	// would receive a token whose jti has no DEK in cache and every secret
	// call would return 403 until they re-logged in (Bug 5, worklog 0085).
	var recoveryKey string
	if s.keyService != nil {
		recoveryKey, err = s.keyService.InitializeUserKeys(ctx, userID, []byte(req.Password))
		if err != nil {
			s.logger.Error("Register: failed to initialize user keys", err, "user_id", userID)
			return nil, errors.New("registration failed")
		}
	}

	token, err := s.GenerateToken(userID)
	if err != nil {
		return nil, errors.New("registration failed")
	}

	if s.keyService != nil {
		jti := utilities.ExtractJTI(token)
		if jti == "" {
			s.logger.Error("Register: issued token has empty jti; refusing registration",
				fmt.Errorf("empty jti"), "user_id", userID)
			return nil, errors.New("registration failed")
		}
		// Epic 56: register also gets durable jwt_sessions write, so a
		// Valkey restart between registration and the user's first
		// secret operation does not force a soft-unlock. The JWT was
		// just generated above and is signed with s.jwtSecret, so that
		// is by definition the matched key the rehydrate path will
		// find. PR #421 review pass 2 flagged the previous nil-signing-
		// key call as inconsistent with login.
		if err := s.keyService.UnlockDEKWithSigningKey(ctx, userID, []byte(req.Password), jti, s.tokenDuration, s.jwtSecret); err != nil {
			s.logger.Error("Register: failed to unlock DEK", err, "user_id", userID)
			return nil, errors.New("registration failed")
		}
	}

	// US-49.6: Send email verification. When an email verifier is wired
	// (SES in production), the user starts unverified and must click the
	// link before they can log in. When no verifier is wired (dev/air-gapped),
	// persist email_verified=true immediately so Login (which reads from DB)
	// doesn't permanently lock the user out.
	if s.emailVerifier != nil {
		if err := s.emailVerifier.SendVerification(ctx, userID, user.Email); err != nil {
			s.logger.Warn("Register: failed to send verification email", "user_id", userID, "error", err.Error())
		}
		user.EmailVerified = false
	} else {
		verified := true
		if err := s.dbService.UpdateUser(ctx, userID, types.UserUpdates{EmailVerified: &verified}); err != nil {
			s.logger.Error("Register: failed to persist email_verified", err, "user_id", userID)
			user.EmailVerified = false
		} else {
			user.EmailVerified = true
		}
	}

	user.PasswordHash = ""
	return &types.AuthResponse{Token: token, User: *user, RecoveryKey: recoveryKey, TokenTTL: s.tokenDuration}, nil
}

// dummyBcryptHash is a real, well-formed bcrypt hash (cost 12) of an
// arbitrary password the system never accepts. We use a real hash
// rather than a hand-rolled string of zeros because the bcrypt
// library validates the hash form (length, version prefix, salt
// charset) BEFORE running the KDF; an invalid hash short-circuits in
// microseconds and re-opens the user-enumeration timing channel
// (validator finding N5 in worklog 0094 pass-2 audit).
//
// This hash has the canonical 60-byte length, a $2a$12$ prefix, and
// 22 bcrypt-base64 salt chars + 31 hash chars. CompareHashAndPassword
// against any password runs the full cost-12 KDF before failing.
const dummyBcryptHash = "$2a$12$7c6XjTynpWE0yY.2/uC1IufZqmLuVCoJSv3MFVWCPBaWVDaPPwXj."

// VerifyPassword checks the supplied password against the stored
// bcrypt hash for userID. Returns nil on match, ErrInvalidPassword on
// any mismatch / not-found / DB error. The error returned is
// uniform — callers must NOT differentiate between "wrong password"
// and "user does not exist" because doing so leaks user-existence
// status (the same reason Login returns the generic "invalid
// credentials" message).
//
// bcrypt.CompareHashAndPassword runs in constant time relative to the
// hash cost, so timing-channel leakage is bounded by the bcrypt cost
// (12 in this codebase) regardless of password length.
func (s *Service) VerifyPassword(ctx context.Context, userID string, password []byte) error {
	user, err := s.dbService.GetUser(ctx, userID)
	if err != nil || user == nil {
		// Run a dummy bcrypt compare so the response time is
		// indistinguishable from the real-user-wrong-password
		// branch. The constant cost prevents user enumeration via
		// timing. Hash is real (60 chars, $2a$12$ prefix) so bcrypt
		// runs the full KDF before failing.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), password)
		return secrets.ErrInvalidPassword
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), password); err != nil {
		return secrets.ErrInvalidPassword
	}
	return nil
}

func (s *Service) Login(ctx context.Context, req types.LoginRequest) (*types.AuthResponse, error) {
	email := strings.ToLower(strings.TrimSpace(req.Email))

	lockoutEnabled, lockoutAttempts, _ := s.lockoutConfig(ctx)
	if lockoutEnabled {
		lockoutKey := fmt.Sprintf("lockout:%s", email)
		if countStr, err := s.cacheService.Get(ctx, lockoutKey); err == nil && countStr != "" {
			var count int
			if _, err := fmt.Sscanf(countStr, "%d", &count); err == nil && count >= lockoutAttempts {
				// A locked-out attempt is still an attempt from
				// the dashboard's perspective: include it in
				// the auth_attempts_total denominator so the
				// failure ratio reflects reality.
				metrics.RecordAuthAttempt("password", "failure")
				return nil, errors.New("account temporarily locked due to too many failed attempts")
			}
		}
	}

	user, err := s.dbService.GetUserByEmail(ctx, email)
	if err != nil {
		s.logger.Error("Login: db error", err)
		// G27 (Epic 17 worklog 0089 RT-4.10): run a dummy bcrypt
		// compare so a DB error path takes the same observable time
		// as a successful user lookup with wrong password.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(req.Password))
		metrics.RecordAuthAttempt("password", "failure")
		return nil, errors.New("invalid email or password")
	}
	if user == nil {
		s.recordFailedAttempt(ctx, email)
		metrics.RecordAuthFailure("user_not_found")
		metrics.RecordAuthAttempt("password", "failure")
		// G27: same as VerifyPassword — burn the bcrypt cycles so
		// no-such-user takes ~226ms instead of ~16ms.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(req.Password))
		return nil, errors.New("invalid email or password")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		s.recordFailedAttempt(ctx, email)
		metrics.RecordAuthFailure("wrong_password")
		metrics.RecordAuthAttempt("password", "failure")
		return nil, errors.New("invalid email or password")
	}

	if user.Status == types.UserStatusSuspended {
		s.recordFailedAttempt(ctx, email)
		metrics.RecordAuthFailure("account_suspended")
		metrics.RecordAuthAttempt("password", "failure")
		return nil, errors.New("account suspended")
	}

	if !user.Active {
		s.recordFailedAttempt(ctx, email)
		metrics.RecordAuthFailure("account_inactive")
		metrics.RecordAuthAttempt("password", "failure")
		return nil, errors.New("invalid email or password")
	}

	// US-49.6: Unverified users cannot log in. The credentials are correct
	// (we checked bcrypt above), so it's safe to tell them WHY — they need
	// to verify their email. This does not create an enumeration vector:
	// the attacker already knows the email AND the password to reach this
	// branch.
	if !user.EmailVerified {
		metrics.RecordAuthFailure("email_not_verified")
		metrics.RecordAuthAttempt("password", "failure")
		return nil, ErrEmailNotVerified
	}

	s.clearFailedAttempts(ctx, email)

	// Determine effective token TTL: use rememberMeDuration when the user
	// opts in and the feature is configured, otherwise use tokenDuration.
	tokenDur := s.tokenDuration
	if req.RememberMe && s.config.Auth.RememberMeDuration > 0 {
		tokenDur = s.config.Auth.RememberMeDuration
	}

	token, err := s.GenerateTokenWithDuration(user.ID, tokenDur)
	if err != nil {
		metrics.RecordAuthAttempt("password", "failure")
		return nil, errors.New("login failed")
	}

	// Extract jti once — used for both DEK unlock and session tracking.
	jti := utilities.ExtractJTI(token)

	// Unlock DEK for secret management (Epic 10 + Epic 56 durable write).
	// We pass the active signing key (s.jwtSecret) — by definition this is
	// the key the fresh JWT was just signed with, so the rehydrate path
	// can later re-derive the wrapping KEK from the matched validation
	// key (which will be either s.jwtSecret or some entry of
	// s.jwtPreviousSecrets depending on whether rotation happens in
	// between).
	if s.keyService != nil {
		if jti != "" {
			// Auto-initialize keys for pre-Epic 10 users on first login
			hasKeys, _ := s.keyService.HasKeys(ctx, user.ID)
			if !hasKeys {
				if _, err := s.keyService.InitializeUserKeys(ctx, user.ID, []byte(req.Password)); err != nil {
					s.logger.Warn("Login: failed to auto-init keys", "user_id", user.ID, "error", err.Error())
				}
			}
			if err := s.keyService.UnlockDEKWithSigningKey(ctx, user.ID, []byte(req.Password), jti, tokenDur, s.jwtSecret); err != nil {
				s.logger.Warn("Login: failed to unlock DEK", "user_id", user.ID, "error", err.Error())
			}
		}
	}

	// US-49.5: Track the jti for bulk session invalidation on password
	// reset. Best-effort: if Redis is unavailable, login still succeeds;
	// the session just won't be revocable in bulk (the token TTL bounds
	// the exposure).
	if jti != "" {
		s.trackUserSession(ctx, user.ID, jti, token, tokenDur)
	}

	user.PasswordHash = ""
	metrics.RecordAuthAttempt("password", "success")
	return &types.AuthResponse{Token: token, User: *user, TokenTTL: tokenDur}, nil
}

// trackUserSession records the session's Redis keys for bulk revocation.
// Stores both the jti key and the hash key so RevokeAllUserSessions can
// write "revoked" under both — matching RevokeToken's approach (the hash-key
// fast-path in ValidateToken must also see the revocation). Best-effort:
// errors AND panics are swallowed (test mocks for cacheService panic on
// unexpected calls; login must never fail because session tracking is
// unavailable). The set is capped at 50 entries.
func (s *Service) trackUserSession(ctx context.Context, userID, jti, token string, ttl time.Duration) {
	if jti == "" {
		return
	}
	defer func() {
		_ = recover() // best-effort: tracking must never break login
	}()
	key := "user-sessions:" + userID
	hashKey := "token:" + pkgutil.HashString(token)
	entry := jti + "|" + hashKey
	var entries []string
	_ = s.cacheService.GetObject(ctx, key, &entries)
	entries = append(entries, entry)
	if len(entries) > 50 {
		entries = entries[len(entries)-50:]
	}
	storeTTL := s.maxSessionRevocationTTL()
	if err := s.cacheService.SetObject(ctx, key, entries, storeTTL); err != nil && s.logger != nil {
		s.logger.Warn("Login: failed to track session for revocation", "user_id", userID)
	}
}

// maxSessionRevocationTTL returns the longest possible token TTL so the
// revocation entry outlives the token. Uses RememberMeDuration if configured
// (up to 30d), otherwise TokenDuration.
func (s *Service) maxSessionRevocationTTL() time.Duration {
	ttl := s.tokenDuration
	if s.config.Auth.RememberMeDuration > ttl {
		ttl = s.config.Auth.RememberMeDuration
	}
	return ttl
}

// RevokeAllUserSessions revokes all outstanding JWTs for a user by writing
// "revoked" under each tracked jti key AND hash key (both paths that
// ValidateToken checks). Used by password-reset confirm (US-49.5) so a
// stolen JWT stops working after the victim resets their password.
func (s *Service) RevokeAllUserSessions(ctx context.Context, userID string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	key := "user-sessions:" + userID
	var entries []string
	if err := s.cacheService.GetObject(ctx, key, &entries); err != nil || len(entries) == 0 {
		return nil
	}

	revokedTTL := s.maxSessionRevocationTTL()
	for _, entry := range entries {
		parts := strings.SplitN(entry, "|", 2)
		jti := parts[0]
		// Write jti key — catches the jti-based revocation check in ValidateToken.
		_ = s.cacheService.Set(ctx, "token:"+jti, "revoked", revokedTTL)
		// Write hash key — catches the hash-key fast-path in ValidateToken
		// (which returns the cached value as userID; "revoked" is not a valid
		// userID so the middleware rejects the request).
		if len(parts) > 1 {
			_ = s.cacheService.Set(ctx, parts[1], "revoked", revokedTTL)
		}
	}
	_ = s.cacheService.Delete(ctx, key)

	// Epic 56: keep durable jwt_sessions consistent with the Redis-side
	// revocation markers. Best-effort — the auth-layer revocation is
	// already authoritative; this is defense-in-depth against a future
	// rehydrate path bug.
	if s.keyService != nil {
		_ = s.keyService.DeleteDurableSessionsForUser(ctx, userID)
	}
	return nil
}

func (s *Service) recordFailedAttempt(ctx context.Context, email string) {
	enabled, _, duration := s.lockoutConfig(ctx)
	if !enabled {
		return
	}
	lockoutKey := fmt.Sprintf("lockout:%s", email)
	countStr, _ := s.cacheService.Get(ctx, lockoutKey)
	count := 0
	if countStr != "" {
		_, _ = fmt.Sscanf(countStr, "%d", &count)
	}
	count++
	if duration == 0 {
		duration = 15 * time.Minute
	}
	if err := s.cacheService.Set(ctx, lockoutKey, fmt.Sprintf("%d", count), duration); err != nil {
		s.logger.Error("Failed to record lockout attempt", err, "email", email)
	}
}

func (s *Service) clearFailedAttempts(ctx context.Context, email string) {
	enabled, _, _ := s.lockoutConfig(ctx)
	if !enabled {
		return
	}
	lockoutKey := fmt.Sprintf("lockout:%s", email)
	if err := s.cacheService.Delete(ctx, lockoutKey); err != nil {
		s.logger.Error("Failed to clear lockout", err, "email", email)
	}
}

// CreateAPIKey creates a new API key for the user. When req.DecryptAccess is
// true, the user's DEK is wrapped under the new API key's derived KEK so
// API-key auth can read encrypted user_secrets. matchedSigningKey is the
// JWT signing key that validated the caller's session (Epic 56); pass nil
// for API-key-authenticated callers (a key cannot be created from an API-key
// session anyway — the existing sessionID check requires a JWT).
func (s *Service) CreateAPIKey(ctx context.Context, userID string, req types.CreateAPIKeyRequest, sessionID string, matchedSigningKey []byte) (*types.APIKey, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("failed to generate api key: %w", err)
	}
	keyStr := s.config.Auth.APIKeyPrefix + hex.EncodeToString(raw)

	h := sha256.Sum256([]byte(keyStr))
	keyHash := hex.EncodeToString(h[:])
	keyPrefix := keyStr
	if len(keyPrefix) > 8 {
		keyPrefix = keyPrefix[:8]
	}

	apiKey := &types.APIKey{
		ID:           uuid.New().String(),
		UserID:       userID,
		Name:         req.Name,
		Key:          keyHash,
		Prefix:       keyPrefix,
		Active:       true,
		CreatedAt:    time.Now(),
		Legacy:       false,
		AllowedCIDRs: req.AllowedCIDRs,
	}

	if req.DecryptAccess {
		if s.rootKeyProvider == nil {
			return nil, errors.New("server root key not configured; decrypt_access keys unavailable")
		}
		if sessionID == "" {
			return nil, errors.New("JWT session required to create a key with decrypt_access=true")
		}
		if s.keyService == nil {
			return nil, errors.New("key service not configured; decrypt_access keys unavailable")
		}

		dek, err := s.keyService.GetDEK(ctx, sessionID, matchedSigningKey)
		if err != nil {
			return nil, fmt.Errorf("DEK not available for wrapping: %w", err)
		}

		kekSalt := make([]byte, 32)
		if _, err := rand.Read(kekSalt); err != nil {
			return nil, fmt.Errorf("failed to generate KEK salt: %w", err)
		}

		apiKEK, err := secrets.DeriveKEKFromKey([]byte(keyStr), kekSalt, "llmsafespaces-apikey-kek")
		if err != nil {
			return nil, fmt.Errorf("failed to derive API KEK: %w", err)
		}

		wrappedDEK, err := secrets.EncryptSecret(apiKEK, dek)
		if err != nil {
			return nil, fmt.Errorf("failed to wrap DEK: %w", err)
		}

		apiKey.DecryptAccess = true
		apiKey.KekSalt = kekSalt
		apiKey.WrappedDEK = wrappedDEK
		apiKey.DekSynced = true
	}

	if s.rootKeyProvider != nil {
		keyCiphertext, err := s.rootKeyProvider.Encrypt(ctx, []byte(keyStr))
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt key ciphertext: %w", err)
		}
		apiKey.KeyCiphertext = keyCiphertext
		apiKey.KeyVersion = secrets.ActiveVersionOf(s.rootKeyProvider)
	}

	if err := s.dbService.CreateAPIKey(ctx, apiKey); err != nil {
		return nil, fmt.Errorf("failed to store api key: %w", err)
	}

	apiKey.Key = keyStr
	return apiKey, nil
}
func (s *Service) ListAPIKeys(ctx context.Context, userID string) ([]*types.APIKey, error) {
	keys, err := s.dbService.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list api keys: %w", err)
	}
	for _, k := range keys {
		k.Key = ""
	}
	return keys, nil
}

func (s *Service) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	existing, err := s.dbService.GetAPIKey(ctx, userID, keyID)
	if err != nil {
		return fmt.Errorf("failed to get api key: %w", err)
	}
	if existing == nil {
		return errors.New("api key not found")
	}
	return s.dbService.DeleteAPIKey(ctx, userID, keyID)
}

// extractToken extracts the JWT or API-key token from the Authorization header
// or the configured session cookie. The cookie name is read from the service
// config (cfg.Auth.CookieName) with a fallback to "lsp_session".
func (s *Service) extractToken(c *gin.Context) string {
	name := s.config.Auth.CookieName
	if name == "" {
		name = "lsp_session"
	}
	return utilities.ExtractToken(c, utilities.TokenExtractorConfig{
		HeaderName: "Authorization",
		TokenType:  "Bearer",
		CookieName: name,
	})
}

// AuthMiddleware returns a middleware that validates JWT tokens
func (s *Service) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract token from request
		tokenString := s.extractToken(c)
		if tokenString == "" {
			c.JSON(401, gin.H{"error": "Authorization token required"})
			c.Abort()
			return
		}

		// Validate token. Epic 56: also surface the matched signing-key
		// index so the rehydrate path can derive the per-session KEK from
		// the same key the JWT validated under. For API-key auth, idx = -1
		// (no JWT) and signingKeyByIndex returns nil.
		userID, matchedIdx, err := s.validateTokenAndMatchedKey(c.Request.Context(), tokenString, c.ClientIP())
		if err != nil {
			c.JSON(401, gin.H{"error": "Invalid or expired token"})
			c.Abort()
			return
		}

		// Set user ID in context
		c.Set("userID", userID)

		// Epic 56 Step 4: expose the matched JWT signing key (and its
		// rotation-window index) for downstream KeyService.GetDEK callers.
		// We always set jwt_signing_key_index so handlers can distinguish
		// "no value" (key wasn't set at all, suggests an unrelated bug)
		// from "set to -1" (legitimately unknown — API key auth, legacy
		// cache hit, or out-of-rotation-window scenarios).
		c.Set("jwt_signing_key_index", matchedIdx)
		if key := s.signingKeyByIndex(matchedIdx); key != nil {
			c.Set("jwt_signing_key", key)
		}

		// Set session ID for DEK cache lookup in secret management.
		if jti := utilities.ExtractJTI(tokenString); jti != "" {
			c.Set("sessionID", jti)
			// Epic 56: stash the JWT's exp (unix timestamp) so soft-unlock
			// can size the durable row's TTL to actual remaining lifetime.
			// 0 ⇒ extraction failed (malformed token, somehow validated
			// without an exp claim — extremely unlikely); the handler
			// falls back to a 1h default. Set even on the AuthMiddleware
			// path because soft-unlock is the primary consumer.
			if exp := utilities.ExtractExp(tokenString); exp > 0 {
				c.Set("jwt_exp_unix", exp)
			}
		} else if utilities.IsAPIKey(tokenString, s.config.Auth.APIKeyPrefix) {
			c.Set("sessionID", "apikey:"+pkgutil.HashString(tokenString))
		}

		// Load user role into context for AdminGuard and authorization checks.
		// D19: also enforce user-level suspension here — this is the single
		// load-bearing gate that blocks a suspended user from EVERY
		// authenticated endpoint (all orgs + personal). A suspended user's
		// token/API key is still cryptographically valid; the status check is
		// what denies access.
		//
		// F3 (US-43.19): FAIL CLOSED on any GetUser error — the previous code
		// silently fell through to c.Next(), letting a suspended user regain
		// access during a DB blip. Denying legitimate users during a DB outage
		// is the correct security posture for an authz gate.
		//
		// F4 (US-43.19): the revocation marker set by SuspendUser lets us
		// report a precise 401 (not 503) for a suspended user EVEN when the DB
		// is unreachable, and lets us HEAL a stale marker left by an unsuspend
		// whose ClearUserSuspended failed (Redis blip) — otherwise an active
		// user would be falsely blocked until the marker TTL expired. GetUser
		// remains authoritative; the marker is only consulted on the DB-error
		// branch (resilience) and the active-user branch (healing).
		if s.dbService != nil {
			user, gerr := s.dbService.GetUser(c.Request.Context(), userID)
			if gerr != nil {
				if s.isUserSuspendedCached(c.Request.Context(), userID) {
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "account suspended"})
					return
				}
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "unable to verify account status"})
				return
			}
			if user == nil {
				// Token validated but no user row — the account was deleted
				// while the token was still cryptographically valid. Fail
				// closed rather than honoring a stale credential.
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "account not found"})
				return
			}
			if user.Status == types.UserStatusSuspended {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "account suspended"})
				return
			}
			// Active user. Clear any stale revocation marker (an unsuspend
			// whose ClearUserSuspended failed) so the next request is not
			// falsely flagged. Best-effort: a Redis failure here only leaves
			// the marker to expire on its own TTL.
			if s.isUserSuspendedCached(c.Request.Context(), userID) {
				_ = s.ClearUserSuspended(c.Request.Context(), userID)
			}
			c.Set("userRole", user.Role)
		}

		c.Next()
	}
}

// OptionalAuthMiddleware is like AuthMiddleware but never aborts. It sets
// "userID" in the context when a valid JWT/API key is present, and calls
// c.Next() unconditionally. Handlers that use this middleware must check
// the userID themselves and handle the unauthenticated case.
//
// D19: a suspended user is treated as unauthenticated here — no userID,
// sessionID, or role is set — so they cannot exercise any authenticated
// capability. They retain access only to the anonymous surface (the same
// surface any unauthenticated caller sees). The middleware still does not
// abort, preserving its contract for public+optional-auth endpoints.
func (s *Service) OptionalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenString := s.extractToken(c)
		if tokenString != "" {
			userID, matchedIdx, err := s.validateTokenAndMatchedKey(c.Request.Context(), tokenString, c.ClientIP())
			if err == nil && userID != "" {
				suspended := false
				// OptionalAuthMiddleware never aborts; it excludes a suspended
				// user by withholding userID (they get the anonymous surface).
				// On GetUser error it stays anonymous (optional endpoints must
				// keep working for unauthenticated callers during a DB blip);
				// the mandatory AuthMiddleware fail-closes, this one does not.
				// The F4 marker is intentionally NOT consulted here: it would
				// add a stale-marker false-positive risk for no benefit, since
				// GetUser already authoritatively resolves suspension.
				if s.dbService != nil {
					if user, gerr := s.dbService.GetUser(c.Request.Context(), userID); gerr == nil && user != nil {
						if user.Status == types.UserStatusSuspended {
							suspended = true
						} else {
							c.Set("userRole", user.Role)
						}
					}
				}
				if !suspended {
					c.Set("userID", userID)
					if jti := utilities.ExtractJTI(tokenString); jti != "" {
						c.Set("sessionID", jti)
						// Epic 56: stash JWT exp (unix) so handlers behind
						// OptionalAuthMiddleware (and AuthMiddleware via
						// the parallel path above) can size the soft-unlock
						// durable-row TTL to the JWT's remaining lifetime.
						// Symmetric with auth.go:1314-1317 — PR #421 review
						// pass 1 caught the missing setter here.
						if exp := utilities.ExtractExp(tokenString); exp > 0 {
							c.Set("jwt_exp_unix", exp)
						}
					} else if utilities.IsAPIKey(tokenString, s.config.Auth.APIKeyPrefix) {
						c.Set("sessionID", "apikey:"+pkgutil.HashString(tokenString))
					}
					// Epic 56 Step 4: same context keys as AuthMiddleware so
					// optional-auth handlers (e.g. GetUser, public-with-bonus)
					// can use the durable-DEK rehydrate path when present.
					c.Set("jwt_signing_key_index", matchedIdx)
					if key := s.signingKeyByIndex(matchedIdx); key != nil {
						c.Set("jwt_signing_key", key)
					}
				}
			}
		}
		c.Next()
	}
}

// parseTokenAcceptingRotatedKeys parses the token under the active
// signing key or any previous (rotation-window) key. Returns the parsed
// token along with the *matched* signing key and its index so callers
// that need to derive per-session crypto from the same key the JWT
// validated under (Epic 56: durable DEK rehydrate, soft-unlock backfill)
// can do so explicitly.
//
// Index convention:
//
//	 0 = active key (s.jwtSecret)
//	 1 = s.jwtPreviousSecrets[0]
//	 2 = s.jwtPreviousSecrets[1]
//	...
//	-1 = sentinel for "no key matched" (caller should also see a non-nil error)
//
// Callers that do not need the matched key (RevokeToken,
// ValidateTokenWithClientIP without rehydrate) can discard the matched
// key and index. The shape change is intentional: making the matched
// key always-available eliminates a class of subtle bugs where the
// caller assumed the active key was the matched key. See the [HIGH]
// finding from PR #411 review pass 1.
func (s *Service) parseTokenAcceptingRotatedKeys(token string) (*jwt.Token, []byte, int, error) {
	// Active-key first attempt
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtSecret, nil
	})
	if err == nil && parsed.Valid {
		// Return a defensive copy of the matched key so callers cannot
		// mutate s.jwtSecret through the returned slice. Cheap (32-byte
		// secrets) and removes a footgun.
		matched := make([]byte, len(s.jwtSecret))
		copy(matched, s.jwtSecret)
		return parsed, matched, 0, nil
	}

	// Previous keys
	var lastErr error
	for i, prev := range s.jwtPreviousSecrets {
		prevKey := prev
		alt, altErr := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return prevKey, nil
		})
		if altErr == nil && alt.Valid {
			matched := make([]byte, len(prevKey))
			copy(matched, prevKey)
			return alt, matched, i + 1, nil
		}
		lastErr = altErr
	}

	// Nothing matched. Surface the most-recent parse error if any,
	// else the active-key error, else a synthetic sentinel.
	if err != nil {
		return nil, nil, -1, err
	}
	if lastErr != nil {
		return nil, nil, -1, lastErr
	}
	return nil, nil, -1, errors.New("token signature does not verify against any active or previous key")
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

func ipInAnyCIDR(clientIP string, cidrs []string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
