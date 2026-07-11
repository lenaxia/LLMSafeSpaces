// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/utilities"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	pkgutil "github.com/lenaxia/llmsafespaces/pkg/utilities"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestNew(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Test successful creation
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)

	service, err := New(cfg, log, mockDb, mockCache)
	assert.NoError(t, err)
	assert.NotNil(t, service)
	assert.Equal(t, log, service.logger)
	assert.Equal(t, cfg, service.config)
	assert.Equal(t, mockDb, service.dbService)
	assert.Equal(t, mockCache, service.cacheService)
	assert.Equal(t, []byte("test-secret"), service.jwtSecret)
	assert.Equal(t, 24*time.Hour, service.tokenDuration)

	// Test missing JWT secret
	cfg.Auth.JWTSecret = ""
	service, err = New(cfg, log, mockDb, mockCache)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "JWT secret is required")
	assert.Nil(t, service)
}

func TestAuthenticateAPIKey(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	// Create service with mocks
	service, _ := New(cfg, log, mockDbService, mockCacheService)

	// Test case: Valid API key (legacy plaintext — "valid-key" is 9 chars, not 64)
	user := &types.User{
		ID: "user123",
	}
	validKeyHash := "cc358b85b8b74a6f82504e141c2cb5c45c70711c2420cc98bcc230843f8def6d"
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), validKeyHash).Return((*types.User)(nil), nil).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), "valid-key").Return(user, nil).Once()
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("valid-key")).Return("", errors.New("not found")).Once()
	mockCacheService.On("Set", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("valid-key"), "user123", mock.Anything).Return(nil).Once()

	userID, err := service.AuthenticateAPIKey(context.Background(), "valid-key")
	assert.NoError(t, err)
	assert.Equal(t, "user123", userID)

	// Test case: Invalid API key (both hash and plaintext return nil)
	invalidKeyHash := "8bc840e01de28327e34b85011d17c86ce620d1057b712118fe049c647bc128bd"
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), invalidKeyHash).Return((*types.User)(nil), nil).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), "invalid-key").Return((*types.User)(nil), nil).Once()
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("invalid-key")).Return("", errors.New("not found")).Once()

	userID, err = service.AuthenticateAPIKey(context.Background(), "invalid-key")
	assert.Error(t, err)
	assert.Equal(t, "", userID)
	assert.Contains(t, err.Error(), "invalid API key")

	// Test case: Database error on hash lookup
	errorKeyHash := "92d7e67cf10926961e3555e408956ae0f6645e15f7a2d832b30c1e31339a255c"
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), errorKeyHash).Return((*types.User)(nil), errors.New("database error")).Once()
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("error-key")).Return("", errors.New("not found")).Once()

	userID, err = service.AuthenticateAPIKey(context.Background(), "error-key")
	assert.Error(t, err)
	assert.Equal(t, "", userID)
	assert.Contains(t, err.Error(), "database error")

	// Test case: Cached API key
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("cached-key")).Return("cached-user", nil).Once()

	userID, err = service.AuthenticateAPIKey(context.Background(), "cached-key")
	assert.NoError(t, err)
	assert.Equal(t, "cached-user", userID)

	mockDbService.AssertExpectations(t)
	// Test case: API key validation via ValidateToken
	apiKey := "api_test_key"
	apiKeyHash := "731ca82ae829a7ff88824fdeccb48aceddaf9f2ff07127b5d28a329b12413440"
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString(apiKey)).
		Return("", errors.New("not found")).Once()

	user = &types.User{
		ID: "api_user",
	}
	// hash-first returns nil, then plaintext fallback (len("api_test_key") = 12 != 64)
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), apiKeyHash).
		Return((*types.User)(nil), nil).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), apiKey).
		Return(user, nil).Once()
	mockCacheService.On("Set", mock.MatchedBy(func(ctx context.Context) bool { return true }),
		"apikey:"+pkgutil.HashString(apiKey), "api_user", mock.Anything).Return(nil).Once()

	// Configure the service to recognize API keys
	service.config.Auth.APIKeyPrefix = "api_"

	userID, err = service.ValidateToken(context.Background(), apiKey)
	assert.NoError(t, err)
	assert.Equal(t, "api_user", userID)

	mockCacheService.AssertExpectations(t)
	mockDbService.AssertExpectations(t)
}
func TestGenerateToken(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	// Create service with mocks
	service, _ := New(cfg, log, mockDbService, mockCacheService)

	// Test token generation
	userID := "user123"
	token, err := service.GenerateToken(userID)
	assert.NoError(t, err)
	assert.NotEmpty(t, token)

	// Verify token
	parsedToken, err := jwt.Parse(token, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return service.jwtSecret, nil
	})
	assert.NoError(t, err)
	assert.True(t, parsedToken.Valid)

	// Check claims
	claims, ok := parsedToken.Claims.(jwt.MapClaims)
	assert.True(t, ok)
	assert.Equal(t, userID, claims["sub"])
	assert.NotEmpty(t, claims["exp"])
	assert.NotEmpty(t, claims["iat"])
}

// TestValidateToken_LegacyCases covers the original ValidateToken paths.
// The G18 fix made cache call counts harder to assert with mock matchers
// (an additional jti-keyed Get fires on the success path), so this test now
// uses the in-memory cache from auth_revocation_test.go for clarity. The
// dedicated G18 regression tests live in auth_revocation_test.go.
func TestValidateToken(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "api_"

	mockDB := new(mocks.MockDatabaseService)
	cache := newMemCache()

	service, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	userID := "user123"
	token, err := service.GenerateToken(userID)
	require.NoError(t, err)

	// Valid token round-trip.
	got, err := service.ValidateToken(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, userID, got)

	// Invalid token format (treated as JWT but parses fail; not API key).
	mockDB.On("GetUserByAPIKey", mock.Anything, "invalid-token").
		Return((*types.User)(nil), errors.New("invalid API key")).Maybe()
	got, err = service.ValidateToken(context.Background(), "invalid-token")
	require.Error(t, err)
	require.Empty(t, got)

	// Expired token.
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
	})
	expiredString, _ := expired.SignedString(service.jwtSecret)
	mockDB.On("GetUserByAPIKey", mock.Anything, expiredString).
		Return((*types.User)(nil), errors.New("invalid API key")).Maybe()
	got, err = service.ValidateToken(context.Background(), expiredString)
	require.Error(t, err)
	require.Empty(t, got)
	require.Contains(t, err.Error(), "token is expired")
}

// ctxPropKey is a sentinel used to prove ValidateTokenWithClientIP propagates
// the caller's context.Context down to the cache service (US-46.5 / issue #224).
// A derived ctx (context.WithTimeout(parent, …)) preserves the parent's values,
// so the matcher seeing the sentinel proves propagation; context.Background()
// would leave it absent and the matcher would fail.
type ctxPropKey struct{}

func TestValidateTokenWithClientIP_PropagatesContext(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "api_"

	mockDB := new(mocks.MockDatabaseService)
	cache := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	userID := "user123"
	token, err := service.GenerateToken(userID)
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), ctxPropKey{}, "present")
	cacheKey := "token:" + pkgutil.HashString(token)
	matchesPropagated := func(c context.Context) bool { return c.Value(ctxPropKey{}) == "present" }
	// Cache hit: the call returns userID immediately (no jti/Set follow-up),
	// so the single Get is the only cache interaction — and its ctx matcher is
	// the load-bearing propagation assertion.
	cache.On("Get", mock.MatchedBy(matchesPropagated), cacheKey).Return(userID, nil).Once()

	got, err := service.ValidateTokenWithClientIP(ctx, token, "")
	require.NoError(t, err)
	require.Equal(t, userID, got)
	cache.AssertExpectations(t)
}

// TestValidateAPIKey_PropagatesContext is the API-key-path companion to the
// JWT test above. validateAPIKey has its OWN independent context.WithTimeout
// derivation (auth.go), so a regression reverting only that line would not be
// caught by the JWT test. The sentinel matcher closes that gap.
func TestValidateAPIKey_PropagatesContext(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "api_"

	mockDB := new(mocks.MockDatabaseService)
	cache := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	apiKey := "api_propagated"
	userID := "user789"
	ctx := context.WithValue(context.Background(), ctxPropKey{}, "present")
	matchesPropagated := func(c context.Context) bool { return c.Value(ctxPropKey{}) == "present" }
	// Cache hit on the apikey: key — the only cache interaction, so its ctx
	// matcher is the load-bearing propagation assertion.
	cache.On("Get", mock.MatchedBy(matchesPropagated), "apikey:"+pkgutil.HashString(apiKey)).Return(userID, nil).Once()

	got, err := service.validateAPIKey(ctx, apiKey, "")
	require.NoError(t, err)
	require.Equal(t, userID, got)
	cache.AssertExpectations(t)
}

func TestRevokeToken_PropagatesContext(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "api_"
	mockDB := new(mocks.MockDatabaseService)
	cache := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	token, err := service.GenerateToken("user-p2")
	require.NoError(t, err)

	// RevokeToken writes both token:<hash> and token:<jti> via Set. The ctx
	// matcher is load-bearing: a context.Background() regression leaves the
	// sentinel absent, the Sets become unexpected, and the mock fails.
	ctx := context.WithValue(context.Background(), ctxPropKey{}, "present")
	matchesPropagated := func(c context.Context) bool { return c.Value(ctxPropKey{}) == "present" }
	cache.On("Set", mock.MatchedBy(matchesPropagated), mock.Anything, "revoked", mock.Anything).Return(nil).Twice()

	require.NoError(t, service.RevokeToken(ctx, token))
	cache.AssertExpectations(t)
}

func TestRevokeToken(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, err := New(cfg, log, mockDbService, mockCacheService)
	assert.NoError(t, err)

	// Generate a valid token
	token, err := service.GenerateToken("user123")
	assert.NoError(t, err)

	// Parse the token to get its claims (for the expiration check below).
	parsedToken, _ := jwt.Parse(token, func(token *jwt.Token) (interface{}, error) {
		return service.jwtSecret, nil
	})
	claims := parsedToken.Claims.(jwt.MapClaims)

	// Get and validate expiration time
	expClaim, ok := claims["exp"]
	if !ok {
		t.Fatal("token missing expiration claim")
	}

	if _, ok := expClaim.(float64); !ok {
		t.Fatal("invalid expiration time format in token")
	}

	// Test token revocation: G18 fix writes BOTH token:<hash> and token:<jti>.
	mockCacheService.On("Set", mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return len(key) > 6 && key[:6] == "token:" }),
		"revoked", mock.Anything).Return(nil).Twice()

	err = service.RevokeToken(context.Background(), token)
	assert.NoError(t, err)

	mockCacheService.AssertExpectations(t)
}

// TestRevokeAllUserSessions_PropagatesContext mirrors the RevokeToken test for
// its sibling: RevokeAllUserSessions has its OWN independent ctx derivation
// (auth.go:953), so a single-line regression there would not be caught by the
// RevokeToken test. The sentinel matcher on GetObject/Set/Delete is load-bearing.
func TestRevokeAllUserSessions_PropagatesContext(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	mockDB := new(mocks.MockDatabaseService)
	cache := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDB, cache)
	require.NoError(t, err)

	userID := "user-prop2"
	ctx := context.WithValue(context.Background(), ctxPropKey{}, "present")
	matchesPropagated := func(c context.Context) bool { return c.Value(ctxPropKey{}) == "present" }
	entries := []string{"jti-x|token:hashx"} // one entry -> 2 Sets (jti + hash) + 1 Delete
	cache.On("GetObject", mock.MatchedBy(matchesPropagated), "user-sessions:"+userID, mock.Anything).
		Run(func(args mock.Arguments) { *args.Get(2).(*[]string) = entries }).Return(nil).Once()
	cache.On("Set", mock.MatchedBy(matchesPropagated), mock.Anything, "revoked", mock.Anything).Return(nil).Twice()
	cache.On("Delete", mock.MatchedBy(matchesPropagated), "user-sessions:"+userID).Return(nil).Once()

	require.NoError(t, service.RevokeAllUserSessions(ctx, userID))
	cache.AssertExpectations(t)
}

// TestRevokeAllUserSessions_RevokesAllTrackedSessions verifies that
// RevokeAllUserSessions reads the tracked session entries and writes
// "revoked" under both the jti key and the hash key for each entry.
// This is the OWASP-mandated session-invalidation primitive for
// password-reset (US-49.5).
func TestRevokeAllUserSessions_RevokesAllTrackedSessions(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, err := New(cfg, log, mockDbService, mockCacheService)
	require.NoError(t, err)

	userID := "user-reset-1"
	// Simulate two tracked sessions: jti1|hashKey1 and jti2|hashKey2
	entries := []string{"jti-aaa|token:hashaaa", "jti-bbb|token:hashbbb"}
	mockCacheService.On("GetObject", mock.Anything, "user-sessions:"+userID, mock.Anything).
		Run(func(args mock.Arguments) {
			dst := args.Get(2).(*[]string)
			*dst = entries
		}).Return(nil)
	// Each entry → 2 Set calls (jti key + hash key) + 1 Delete at the end
	mockCacheService.On("Set", mock.Anything, "token:jti-aaa", "revoked", mock.Anything).Return(nil)
	mockCacheService.On("Set", mock.Anything, "token:hashaaa", "revoked", mock.Anything).Return(nil)
	mockCacheService.On("Set", mock.Anything, "token:jti-bbb", "revoked", mock.Anything).Return(nil)
	mockCacheService.On("Set", mock.Anything, "token:hashbbb", "revoked", mock.Anything).Return(nil)
	mockCacheService.On("Delete", mock.Anything, "user-sessions:"+userID).Return(nil)

	err = service.RevokeAllUserSessions(context.Background(), userID)
	require.NoError(t, err)
	mockCacheService.AssertExpectations(t)
}

// TestRevokeAllUserSessions_NoTrackedSessions_Noop verifies the method
// is safe when no sessions are tracked (empty or missing key).
func TestRevokeAllUserSessions_NoTrackedSessions_Noop(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, err := New(cfg, log, mockDbService, mockCacheService)
	require.NoError(t, err)

	// GetObject returns error (key not found) → no sessions to revoke
	mockCacheService.On("GetObject", mock.Anything, "user-sessions:ghost", mock.Anything).
		Return(errors.New("redis: nil"))

	err = service.RevokeAllUserSessions(context.Background(), "ghost")
	require.NoError(t, err, "must be nil-safe when no sessions tracked")
	// No Set/Delete calls expected
	mockCacheService.AssertNotCalled(t, "Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mockCacheService.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything)
}

// TestRevokeAllUserSessions_CascadesToDurableSessions pins the Epic 56
// auth-layer wiring: when the auth service revokes a user's Redis-tracked
// sessions, it MUST also cascade into the durable jwt_sessions store via
// keyService.DeleteDurableSessionsForUser. Without this, an attacker who
// has the victim's old JWT could rehydrate the old DEK from PG after the
// victim "logged out everywhere" — exactly the rehydrate path Epic 56 added.
//
// PR #421 review pass 2 noted that the three KeyService-level revocation
// paths (EvictDEK, ChangePassword, RotateKeyWithPassword) all had
// key_service_revocation_test.go coverage, but the fourth — the
// auth-layer call at auth.go:1112 — was exercised only via stub mocks
// that didn't record the call. A regression that deletes that one line
// would have silently passed every other test. This regression test
// uses the fakeKeyService's deleteDurableSessionsForUserIDs recorder
// to assert the call IS made with the correct userID.
func TestRevokeAllUserSessions_CascadesToDurableSessions(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDbService, mockCacheService)
	require.NoError(t, err)

	ks := &fakeKeyService{}
	service.SetKeyService(ks)

	userID := "user-cascade"
	entries := []string{"jti-1|token:hash-1"}
	mockCacheService.On("GetObject", mock.Anything, "user-sessions:"+userID, mock.Anything).
		Run(func(args mock.Arguments) {
			dst := args.Get(2).(*[]string)
			*dst = entries
		}).Return(nil)
	mockCacheService.On("Set", mock.Anything, mock.Anything, "revoked", mock.Anything).Return(nil)
	mockCacheService.On("Delete", mock.Anything, "user-sessions:"+userID).Return(nil)

	require.NoError(t, service.RevokeAllUserSessions(context.Background(), userID))

	// The contract: exactly one DeleteDurableSessionsForUser call with
	// the same userID we revoked.
	require.Len(t, ks.deleteDurableSessionsForUserIDs, 1,
		"RevokeAllUserSessions must cascade to durable jwt_sessions store")
	assert.Equal(t, userID, ks.deleteDurableSessionsForUserIDs[0],
		"cascade must be scoped to the correct user")
}

// TestRevokeAllUserSessions_CascadesToDurableSessions_NoKeyService verifies
// the cascade is a no-op when no key service is wired (the auth service is
// usable without a key service for callers that don't care about
// user-DEK content). Belt-and-braces for nil-safety.
func TestRevokeAllUserSessions_CascadesToDurableSessions_NoKeyService(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)
	service, err := New(cfg, log, mockDbService, mockCacheService)
	require.NoError(t, err)
	// Intentionally do NOT call SetKeyService.

	mockCacheService.On("GetObject", mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("redis: nil"))

	require.NoError(t, service.RevokeAllUserSessions(context.Background(), "u-no-ks"),
		"must be nil-safe when no key service is wired")
}

// TestRevokeAllUserSessions_UsesMaxTTL verifies the revocation TTL covers
// remember-me tokens (30d), not just the default tokenDuration (24h).
func TestRevokeAllUserSessions_UsesMaxTTL(t *testing.T) {
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.RememberMeDuration = 30 * 24 * time.Hour // 720h

	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, err := New(cfg, log, mockDbService, mockCacheService)
	require.NoError(t, err)

	userID := "user-ttl-1"
	entries := []string{"jti-x|token:hashx"}
	mockCacheService.On("GetObject", mock.Anything, "user-sessions:"+userID, mock.Anything).
		Run(func(args mock.Arguments) {
			dst := args.Get(2).(*[]string)
			*dst = entries
		}).Return(nil)
	// Assert the TTL passed to Set is >= 720h (remember-me), not 24h
	mockCacheService.On("Set", mock.Anything, "token:jti-x", "revoked", mock.MatchedBy(func(d time.Duration) bool {
		return d >= 30*24*time.Hour
	})).Return(nil)
	mockCacheService.On("Set", mock.Anything, "token:hashx", "revoked", mock.MatchedBy(func(d time.Duration) bool {
		return d >= 30*24*time.Hour
	})).Return(nil)
	mockCacheService.On("Delete", mock.Anything, "user-sessions:"+userID).Return(nil)

	err = service.RevokeAllUserSessions(context.Background(), userID)
	require.NoError(t, err)
	mockCacheService.AssertExpectations(t)
}

func TestCheckResourceAccess(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	// Create service with mocks
	service, err := New(cfg, log, mockDbService, mockCacheService)
	assert.NoError(t, err)

	// Create a mock gin context
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("userID", "user123")

	// Test case: User owns the resource
	mockDbService.On("CheckResourceOwnership", "user123", "sandbox", "sb-12345").Return(true, nil).Once()

	hasAccess := service.CheckResourceAccess("user123", "sandbox", "sb-12345", "read")
	assert.True(t, hasAccess)

	// Test case: User doesn't own the resource but has permission
	mockDbService.On("CheckResourceOwnership", "user123", "sandbox", "sb-67890").Return(false, nil).Once()
	mockDbService.On("CheckPermission", "user123", "sandbox", "sb-67890", "read").Return(true, nil).Once()

	hasAccess = service.CheckResourceAccess("user123", "sandbox", "sb-67890", "read")
	assert.True(t, hasAccess)

	// Test case: User doesn't own the resource and doesn't have permission
	mockDbService.On("CheckResourceOwnership", "user123", "sandbox", "sb-noaccess").Return(false, nil).Once()
	mockDbService.On("CheckPermission", "user123", "sandbox", "sb-noaccess", "read").Return(false, nil).Once()

	hasAccess = service.CheckResourceAccess("user123", "sandbox", "sb-noaccess", "read")
	assert.False(t, hasAccess)

	// Test case: Database error during ownership check
	mockDbService.On("CheckResourceOwnership", "user123", "sandbox", "sb-error").Return(false, errors.New("database error")).Once()

	hasAccess = service.CheckResourceAccess("user123", "sandbox", "sb-error", "read")
	assert.False(t, hasAccess)

	// Test case: Database error during permission check
	mockDbService.On("CheckResourceOwnership", "user123", "sandbox", "sb-permerror").Return(false, nil).Once()
	mockDbService.On("CheckPermission", "user123", "sandbox", "sb-permerror", "read").Return(false, errors.New("database error")).Once()

	hasAccess = service.CheckResourceAccess("user123", "sandbox", "sb-permerror", "read")
	assert.False(t, hasAccess)

	mockDbService.AssertExpectations(t)
}

func TestGetUserFromContext(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, _ := New(cfg, log, mockDbService, mockCacheService)

	// Test case: User ID in context
	c := &gin.Context{}
	c.Set("userID", "user123")

	userID := service.GetUserID(c)
	assert.Equal(t, "user123", userID)

	// Test case: No user ID in context
	c, _ = gin.CreateTestContext(nil)

	userID = service.GetUserID(c)
	assert.Equal(t, "", userID)
}

func TestValidateAPIKey(t *testing.T) {
	// Create test dependencies
	log, _ := logger.New(true, "debug", "console")

	// Create service
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "api_"

	// Create mock service instances
	mockDbService := new(mocks.MockDatabaseService)
	mockCacheService := new(mocks.MockCacheService)

	service, _ := New(cfg, log, mockDbService, mockCacheService)

	// Test case: Valid API key (cached) — cache hit before hash lookup
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("api_valid")).Return("user123", nil).Once()

	userID, err := service.validateAPIKey(context.Background(), "api_valid", "")
	assert.NoError(t, err)
	assert.Equal(t, "user123", userID)

	// Test case: Valid API key (not cached)
	// "api_new" is 7 chars (< 64) -> hash-first returns nil, plaintext fallback finds user
	apiNewHash := "ab766fd96c8caf6ec48189978caa414e915b64c943e0f7a3415eca46039c71fa"
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("api_new")).Return("", errors.New("not found")).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), apiNewHash).Return((*types.User)(nil), nil).Once()

	user := &types.User{
		ID: "user456",
	}
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), "api_new").Return(user, nil).Once()
	mockCacheService.On("Set", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("api_new"), "user456", mock.Anything).Return(nil).Once()

	userID, err = service.validateAPIKey(context.Background(), "api_new", "")
	assert.NoError(t, err)
	assert.Equal(t, "user456", userID)

	// Test case: Invalid API key (both hash and plaintext return nil)
	apiInvalidHash := "d1f57960469d39912c4b69202e9429d64beb40025e5bdcffc3b5bc1923399148"
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("api_invalid")).Return("", errors.New("not found")).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), apiInvalidHash).Return((*types.User)(nil), nil).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), "api_invalid").Return((*types.User)(nil), nil).Once()

	userID, err = service.validateAPIKey(context.Background(), "api_invalid", "")
	assert.Error(t, err)
	assert.Equal(t, "", userID)
	assert.Contains(t, err.Error(), "invalid API key")

	// Test case: Database error on hash lookup
	apiErrorHash := "8265edf37575deab074c210c887db2ae4899af1e11abb0f87cbac3f26b55d352"
	mockCacheService.On("Get", mock.MatchedBy(func(ctx context.Context) bool { return true }), "apikey:"+pkgutil.HashString("api_error")).Return("", errors.New("not found")).Once()
	mockDbService.On("GetUserByAPIKey", mock.MatchedBy(func(ctx context.Context) bool { return true }), apiErrorHash).Return((*types.User)(nil), errors.New("database error")).Once()

	userID, err = service.validateAPIKey(context.Background(), "api_error", "")
	assert.Error(t, err)
	assert.Equal(t, "", userID)
	assert.Contains(t, err.Error(), "database error")

	mockCacheService.AssertExpectations(t)
	mockDbService.AssertExpectations(t)
}
func TestIsAPIKey(t *testing.T) {
	testCases := []struct {
		name     string
		token    string
		prefix   string
		expected bool
	}{
		{"Valid API key", "api_12345", "api_", true},
		{"Not an API key", "jwt_token", "api_", false},
		{"Empty token", "", "api_", false},
		{"Empty prefix", "api_12345", "", false},
		{"Prefix only", "api_", "api_", true},
		{"Prefix with separator", "api_12345", "api_", true},
		{"Case sensitive", "API_12345", "api_", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := utilities.IsAPIKey(tc.token, tc.prefix)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func newTestService(t *testing.T) (*Service, *mocks.MockDatabaseService, *mocks.MockCacheService) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	return svc, mockDb, mockCache
}

func TestRegister_Success(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	// G8 (Epic 17): Register no longer calls CountUsers; the role
	// decision is atomic in CreateUser via the SQL CTE that runs
	// the count and the insert in the same statement. Tests that
	// formerly mocked CountUsers must instead simulate the
	// CreateUser side-effect (set user.Role = "admin" if empty
	// system, "user" otherwise).
	mockDb.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.MatchedBy(func(u *types.User) bool {
		return u.Email == "new@example.com" && u.Username == "newuser" && u.PasswordHash != "" && u.Active && u.Role == "user"
	})).Return(nil)
	// US-49.6: Register auto-verifies in dev mode (no email verifier wired)
	// by calling UpdateUser to persist email_verified=true.
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.Anything).Return(nil).Maybe()

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "newuser",
		Email:    "new@example.com",
		Password: "securepassword123",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotEmpty(t, resp.Token, "response must include a JWT")
	assert.Equal(t, "newuser", resp.User.Username)
	assert.Equal(t, "new@example.com", resp.User.Email)
	assert.True(t, resp.User.EmailVerified, "dev-mode register must auto-verify")
	assert.Empty(t, resp.User.PasswordHash, "password hash must not be in response")
	mockDb.AssertExpectations(t)
}

// TestRegister_DevMode_AutoVerifiesAndLoginWorks is the end-to-end regression
// test for the login-gate model: Register without an email verifier (dev mode)
// → email_verified persisted → Login succeeds. Catches the bug where Register
// set the in-memory flag but never persisted to DB, locking the user out.
func TestRegister_DevMode_AutoVerifiesAndLoginWorks(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("securepassword123"), bcrypt.DefaultCost)

	// Register creates the user with email_verified=false in CreateUser,
	// then Register auto-verifies via UpdateUser (dev mode, no verifier).
	createdUser := &types.User{EmailVerified: false}
	mockDb.On("GetUserByEmail", ctx, "e2e@test.com").Return(nil, nil).Once()
	mockDb.On("CreateUser", ctx, mock.MatchedBy(func(u *types.User) bool {
		createdUser.ID = u.ID
		createdUser.Email = u.Email
		createdUser.Username = u.Username
		createdUser.Active = u.Active
		createdUser.Role = u.Role
		createdUser.PasswordHash = string(hash)
		return true
	})).Return(nil).Once()
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.MatchedBy(func(u types.UserUpdates) bool {
		if u.EmailVerified != nil {
			createdUser.EmailVerified = *u.EmailVerified
		}
		return true
	})).Return(nil).Once()

	regResp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "e2euser",
		Email:    "e2e@test.com",
		Password: "securepassword123",
	})
	require.NoError(t, err)
	assert.True(t, regResp.User.EmailVerified, "register response must show verified in dev mode")

	// Now login — must succeed because email_verified was persisted.
	mockDb.On("GetUserByEmail", ctx, "e2e@test.com").Return(createdUser, nil).Once()

	loginResp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "e2e@test.com",
		Password: "securepassword123",
	})
	require.NoError(t, err, "login must succeed after dev-mode auto-verify")
	assert.NotEmpty(t, loginResp.Token)
}

// TestRegister_WithVerifier_SendsEmail verifies the production path: when an
// EmailVerifier is wired (SES), Register creates an unverified account and
// calls SendVerification.
func TestRegister_WithVerifier_SendsEmail(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	verifier := &fakeVerifier{}
	svc.SetEmailVerifier(verifier)

	mockDb.On("GetUserByEmail", ctx, "verified@test.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(nil)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "produser",
		Email:    "verified@test.com",
		Password: "securepassword123",
	})

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.False(t, resp.User.EmailVerified, "production register with verifier must leave user unverified")
	assert.Equal(t, 1, verifier.calls, "SendVerification must be called exactly once")
	assert.Equal(t, "verified@test.com", verifier.lastEmail)
}

// TestRegister_WithVerifier_SendFailure_NonFatal verifies that a verification
// email send failure does NOT abort registration. The user can resend later.
func TestRegister_WithVerifier_SendFailure_NonFatal(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	verifier := &fakeVerifier{err: errors.New("SES down")}
	svc.SetEmailVerifier(verifier)

	mockDb.On("GetUserByEmail", ctx, "fail@test.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(nil)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "failuser",
		Email:    "fail@test.com",
		Password: "securepassword123",
	})

	require.NoError(t, err, "registration must not fail on email send error")
	assert.NotNil(t, resp)
	assert.False(t, resp.User.EmailVerified, "user must stay unverified when send fails")
}

// fakeVerifier implements auth.EmailVerifier for testing.
type fakeVerifier struct {
	calls     int
	lastID    string
	lastEmail string
	err       error
}

func (f *fakeVerifier) SendVerification(_ context.Context, userID, email string) error {
	f.calls++
	f.lastID = userID
	f.lastEmail = email
	return f.err
}

// TestRegister_FirstUserBecomesAdmin verifies that the first user registered
// in a fresh installation is auto-promoted to admin. The promotion happens
// inside CreateUser (atomic via SQL CTE — see G8 fix), so the test mocks
// CreateUser to set user.Role="admin" as the DB would.
func TestRegister_FirstUserBecomesAdmin(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "first@example.com").Return(nil, nil)
	// Simulate the SQL CTE returning role="admin" because the users
	// table was empty at insert time. The mock mutates u.Role to
	// match the production DB behavior.
	mockDb.On("CreateUser", ctx, mock.MatchedBy(func(u *types.User) bool {
		return u.Email == "first@example.com"
	})).Run(func(args mock.Arguments) {
		u := args.Get(1).(*types.User)
		u.Role = "admin"
	}).Return(nil)
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.Anything).Return(nil).Maybe()

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "founder",
		Email:    "first@example.com",
		Password: "securepassword123",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "admin", resp.User.Role, "first user must be promoted to admin")
	mockDb.AssertExpectations(t)
}

// TestRegister_SubsequentUsersAreNotAdmin verifies subsequent registrations
// get role="user". The mock returns u.Role="user" verbatim (no DB-side
// promotion) because the SQL CTE only promotes when count==0.
func TestRegister_SubsequentUsersAreNotAdmin(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "second@example.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.MatchedBy(func(u *types.User) bool {
		return u.Email == "second@example.com" && u.Role == "user"
	})).Return(nil)
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.Anything).Return(nil).Maybe()

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "regular",
		Email:    "second@example.com",
		Password: "securepassword123",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "user", resp.User.Role)
	mockDb.AssertExpectations(t)
}

// TestRegister_CreateUserError_FailsClosed verifies that a CreateUser
// failure blocks registration. (G8: was TestRegister_CountUsersError;
// CountUsers is no longer called.)
func TestRegister_CreateUserError_FailsClosed(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "any@example.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(errors.New("db down"))

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "any",
		Email:    "any@example.com",
		Password: "securepassword123",
	})

	assert.Error(t, err)
	assert.Nil(t, resp)
	mockDb.AssertExpectations(t)
}

func TestRegister_DuplicateEmail(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "taken@example.com").Return(&types.User{ID: "existing"}, nil)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "newuser",
		Email:    "taken@example.com",
		Password: "securepassword123",
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "registration failed")
	assert.Nil(t, resp)
	mockDb.AssertExpectations(t)
}

func TestRegister_GetUserByEmailError(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "error@example.com").Return(nil, errors.New("db down"))

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "user",
		Email:    "error@example.com",
		Password: "securepassword123",
	})

	assert.Error(t, err)
	assert.Nil(t, resp)
}

func TestRegister_CreateUserError(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "fail@example.com").Return(nil, nil)
	mockDb.On("CountUsers", ctx).Return(2, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(errors.New("insert failed"))

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "user",
		Email:    "fail@example.com",
		Password: "securepassword123",
	})

	assert.Error(t, err)
	assert.Nil(t, resp)
}

// fakeKeyService is a minimal in-process KeyServiceInterface for asserting
// on the Register/Login DEK lifecycle without spinning up a real key service.
type fakeKeyService struct {
	initCalls                       []fakeKeyInitCall
	unlockCalls                     []fakeKeyUnlockCall
	deleteDurableSessionsForUserIDs []string // Epic 56: records userIDs of every DeleteDurableSessionsForUser call
	hasKeysFn                       func(ctx context.Context, userID string) (bool, error)
	initErr                         error
	unlockErr                       error
	recoveryKey                     string
}

type fakeKeyInitCall struct {
	UserID   string
	Password string
}

type fakeKeyUnlockCall struct {
	UserID    string
	Password  string
	SessionID string
	TTL       time.Duration
}

func (f *fakeKeyService) InitializeUserKeys(ctx context.Context, userID string, password []byte) (string, error) {
	f.initCalls = append(f.initCalls, fakeKeyInitCall{UserID: userID, Password: string(password)})
	if f.initErr != nil {
		return "", f.initErr
	}
	if f.recoveryKey == "" {
		return "deadbeefcafef00d", nil
	}
	return f.recoveryKey, nil
}

func (f *fakeKeyService) UnlockDEK(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration) error {
	f.unlockCalls = append(f.unlockCalls, fakeKeyUnlockCall{
		UserID:    userID,
		Password:  string(password),
		SessionID: sessionID,
		TTL:       ttl,
	})
	return f.unlockErr
}

func (f *fakeKeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, password []byte, sessionID string, ttl time.Duration, _ []byte) error {
	return f.UnlockDEK(ctx, userID, password, sessionID, ttl)
}

func (f *fakeKeyService) DeleteDurableSessionsForUser(_ context.Context, userID string) error {
	// Epic 56: records the call so RevokeAllUserSessions tests can
	// assert that the auth-layer revocation correctly cascades into
	// the durable jwt_sessions store.
	f.deleteDurableSessionsForUserIDs = append(f.deleteDurableSessionsForUserIDs, userID)
	return nil
}

func (f *fakeKeyService) HasKeys(ctx context.Context, userID string) (bool, error) {
	if f.hasKeysFn != nil {
		return f.hasKeysFn(ctx, userID)
	}
	return true, nil
}

func (f *fakeKeyService) GetDEK(ctx context.Context, sessionID string, matchedSigningKey []byte) ([]byte, error) {
	return nil, errors.New("not implemented in fake")
}

func (f *fakeKeyService) CacheDEK(ctx context.Context, sessionID string, dek []byte, ttl time.Duration) error {
	return nil
}

// TestRegister_UnlocksDEKAndReturnsRecoveryKey is the regression test for
// Bug 5 (Register must UnlockDEK so the new user can immediately CreateSecret)
// and Bug 10 (Register must surface the recovery key one time so the user
// can save it; the API does not store it anywhere recoverable).
func TestRegister_UnlocksDEKAndReturnsRecoveryKey(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "fresh@example.com").Return(nil, nil)
	mockDb.On("CountUsers", ctx).Return(5, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(nil)
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.Anything).Return(nil).Maybe()

	ks := &fakeKeyService{recoveryKey: "feedfacecafebabe1234567890abcdef"}
	svc.SetKeyService(ks)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "fresh",
		Email:    "fresh@example.com",
		Password: "securepassword123",
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	// Bug 5: UnlockDEK must be invoked with the JWT's jti so the issued
	// token can be used for secret operations without a re-login.
	require.Len(t, ks.initCalls, 1, "InitializeUserKeys must be called exactly once")
	require.Len(t, ks.unlockCalls, 1, "UnlockDEK must be called exactly once on register")
	assert.Equal(t, ks.initCalls[0].UserID, ks.unlockCalls[0].UserID)
	assert.Equal(t, "securepassword123", ks.unlockCalls[0].Password)
	assert.NotEmpty(t, ks.unlockCalls[0].SessionID, "UnlockDEK sessionID must be the JWT jti")
	assert.Equal(t, utilities.ExtractJTI(resp.Token), ks.unlockCalls[0].SessionID,
		"UnlockDEK sessionID must match the issued token's jti")
	assert.Equal(t, svc.tokenDuration, ks.unlockCalls[0].TTL)

	// Bug 10: the recovery key produced by InitializeUserKeys must reach
	// the response. There is no other way for the user to obtain it.
	assert.Equal(t, "feedfacecafebabe1234567890abcdef", resp.RecoveryKey,
		"register response must include the one-time recovery key")
}

// TestRegister_KeyInitFailureFailsClosed verifies that a failure to
// initialize encryption keys aborts registration. Returning a JWT against
// a half-initialized user produces the same Bug 5 symptom (403 on every
// secret operation), so the failure is fatal.
func TestRegister_KeyInitFailureFailsClosed(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "fail@example.com").Return(nil, nil)
	mockDb.On("CountUsers", ctx).Return(5, nil)
	mockDb.On("CreateUser", ctx, mock.Anything).Return(nil)

	ks := &fakeKeyService{initErr: errors.New("key svc down")}
	svc.SetKeyService(ks)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "fail",
		Email:    "fail@example.com",
		Password: "securepassword123",
	})

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Empty(t, ks.unlockCalls, "UnlockDEK must not be called when InitializeUserKeys fails")
}

// TestLogin_OmitsRecoveryKey ensures the RecoveryKey field is never set on
// login responses. The recovery key is generated once at registration; it
// is not retrievable via login. Returning anything here would be a leak of
// stale or fabricated material.
func TestLogin_OmitsRecoveryKey(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("mypassword"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "user@example.com").Return(&types.User{
		ID: "u1", Username: "user", Email: "user@example.com",
		PasswordHash: string(hash), Active: true, EmailVerified: true,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email: "user@example.com", Password: "mypassword",
	})

	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Empty(t, resp.RecoveryKey, "login response must never include a recovery key")
}

func TestLogin_Success(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("mypassword"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "user@example.com").Return(&types.User{
		ID:           "u1",
		Username:     "user",
		Email:        "user@example.com",
		PasswordHash: string(hash),
		Active:       true, EmailVerified: true,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "user@example.com",
		Password: "mypassword",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.NotEmpty(t, resp.Token)
	assert.Equal(t, "u1", resp.User.ID)
	assert.Empty(t, resp.User.PasswordHash)
	mockDb.AssertExpectations(t)
}

func TestLogin_UserNotFound(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "nobody@example.com").Return(nil, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "nobody@example.com",
		Password: "whatever",
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid email or password")
	assert.Nil(t, resp)
}

func TestLogin_WrongPassword(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "user@example.com").Return(&types.User{
		ID:           "u1",
		PasswordHash: string(hash),
		Active:       true, EmailVerified: true,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "user@example.com",
		Password: "wrong",
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid email or password")
	assert.Nil(t, resp)
}

func TestLogin_InactiveUser(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "disabled@example.com").Return(&types.User{
		ID:           "u1",
		PasswordHash: string(hash),
		Active:       false,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "disabled@example.com",
		Password: "pass",
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid email or password")
	assert.Nil(t, resp)
}

// TestLogin_UnverifiedUser verifies US-49.6: correct credentials but
// email_verified=false → rejected with ErrEmailNotVerified. The message
// tells the user to verify (safe: they already proved they know the
// email + password). NOT recorded as a failed attempt (credentials valid).
func TestLogin_UnverifiedUser(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "unverified@example.com").Return(&types.User{
		ID:            "u1",
		PasswordHash:  string(hash),
		Active:        true,
		EmailVerified: false,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "unverified@example.com",
		Password: "pass",
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEmailNotVerified)
	assert.Nil(t, resp)
}

// TestLogin_SuspendedUser verifies D19 user-level suspension: a user whose
// status='suspended' cannot log in, even with the correct password and the
// legacy active flag still true. The error is the explicit "account suspended"
// message (the password check already passed, so this is not an enumeration
// vector).
func TestLogin_SuspendedUser(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "suspended@example.com").Return(&types.User{
		ID:           "u1",
		PasswordHash: string(hash),
		Active:       true, EmailVerified: true, // legacy flag still true — status is authoritative
		Status: types.UserStatusSuspended,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "suspended@example.com",
		Password: "pass",
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "account suspended")
	assert.Nil(t, resp)
	mockDb.AssertExpectations(t)
}

// TestLogin_ActiveUser_StatusActive verifies the status gate does not block a
// normal active user.
func TestLogin_ActiveUser_StatusActive(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "ok@example.com").Return(&types.User{
		ID:           "u1",
		Username:     "ok",
		Email:        "ok@example.com",
		PasswordHash: string(hash),
		Active:       true, EmailVerified: true,
		Status: types.UserStatusActive,
	}, nil)

	resp, err := svc.Login(ctx, types.LoginRequest{
		Email:    "ok@example.com",
		Password: "pass",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "u1", resp.User.ID)
	mockDb.AssertExpectations(t)
}

func TestCreateAPIKey_Success(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("CreateAPIKey", ctx, mock.MatchedBy(func(k *types.APIKey) bool {
		return k.UserID == "user-1" && k.Name == "my-key" && k.Active && len(k.Key) > 4
	})).Return(nil)

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{Name: "my-key"}, "", nil)

	assert.NoError(t, err)
	assert.NotNil(t, apiKey)
	assert.Equal(t, "my-key", apiKey.Name)
	assert.True(t, len(apiKey.Key) > 32, "API key must be long enough")
	assert.True(t, len(apiKey.Key) > 4 && apiKey.Key[:4] == "lsp_", "API key must have lsp_ prefix")
	mockDb.AssertExpectations(t)
}

func TestCreateAPIKey_DBError(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("CreateAPIKey", ctx, mock.Anything).Return(errors.New("db error"))

	apiKey, err := svc.CreateAPIKey(ctx, "user-1", types.CreateAPIKeyRequest{Name: "my-key"}, "", nil)

	assert.Error(t, err)
	assert.Nil(t, apiKey)
}

func TestListAPIKeys_Success(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("ListAPIKeys", ctx, "user-1").Return([]*types.APIKey{
		{ID: "k1", Name: "key-one", Prefix: "lsp_", Active: true, Key: "lsp_secret"},
		{ID: "k2", Name: "key-two", Prefix: "lsp_", Active: true, Key: "lsp_secret2"},
	}, nil)

	keys, err := svc.ListAPIKeys(ctx, "user-1")

	assert.NoError(t, err)
	assert.Len(t, keys, 2)
	assert.Empty(t, keys[0].Key, "listed keys must not expose the secret")
	assert.Empty(t, keys[1].Key, "listed keys must not expose the secret")
	mockDb.AssertExpectations(t)
}

func TestListAPIKeys_DBError(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("ListAPIKeys", ctx, "user-1").Return(nil, errors.New("db error"))

	keys, err := svc.ListAPIKeys(ctx, "user-1")

	assert.Error(t, err)
	assert.Nil(t, keys)
}

func TestDeleteAPIKey_Success(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetAPIKey", ctx, "user-1", "key-1").Return(&types.APIKey{ID: "key-1"}, nil)
	mockDb.On("DeleteAPIKey", ctx, "user-1", "key-1").Return(nil)

	err := svc.DeleteAPIKey(ctx, "user-1", "key-1")

	assert.NoError(t, err)
	mockDb.AssertExpectations(t)
}

func TestDeleteAPIKey_NotFound(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetAPIKey", ctx, "user-1", "nonexistent").Return(nil, nil)

	err := svc.DeleteAPIKey(ctx, "user-1", "nonexistent")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	mockDb.AssertExpectations(t)
}

func TestDeleteAPIKey_DBError(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetAPIKey", ctx, "user-1", "key-1").Return(nil, errors.New("db error"))

	err := svc.DeleteAPIKey(ctx, "user-1", "key-1")

	assert.Error(t, err)
}

func TestDeleteAPIKey_DeleteFails(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetAPIKey", ctx, "user-1", "key-1").Return(&types.APIKey{ID: "key-1"}, nil)
	mockDb.On("DeleteAPIKey", ctx, "user-1", "key-1").Return(errors.New("delete failed"))

	err := svc.DeleteAPIKey(ctx, "user-1", "key-1")

	assert.Error(t, err)
	mockDb.AssertExpectations(t)
}

// --- Account Lockout ---

func newLockoutService(t *testing.T) (*Service, *mocks.MockDatabaseService, *mocks.MockCacheService) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	cfg.Auth.LockoutEnabled = true
	cfg.Auth.LockoutAttempts = 3
	cfg.Auth.LockoutDuration = 15 * time.Minute
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	return svc, mockDb, mockCache
}

func TestLogin_LockoutAfterFailedAttempts(t *testing.T) {
	svc, mockDb, mockCache := newLockoutService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	user := &types.User{
		ID: "u1", Email: "lock@e.com", PasswordHash: string(hash), Active: true, EmailVerified: true,
	}

	attemptCount := 0
	mockCache.On("Get", ctx, "lockout:lock@e.com").Return("", errors.New("not found"))
	mockCache.On("Set", ctx, "lockout:lock@e.com", mock.MatchedBy(func(v string) bool {
		attemptCount++
		return true
	}), mock.Anything).Return(nil)

	for i := 0; i < 3; i++ {
		mockDb.On("GetUserByEmail", ctx, "lock@e.com").Return(user, nil).Once()
		_, err := svc.Login(ctx, types.LoginRequest{Email: "lock@e.com", Password: "wrong"})
		assert.Error(t, err, "attempt %d should fail", i+1)
	}
	assert.Equal(t, 3, attemptCount, "should have recorded 3 failed attempts")

	mockDb.AssertExpectations(t)
}

func TestLogin_LockoutBlocksAfterMaxAttempts(t *testing.T) {
	svc, _, mockCache := newLockoutService(t)
	ctx := context.Background()

	mockCache.On("Get", ctx, "lockout:locked@e.com").Return("3", nil)

	_, err := svc.Login(ctx, types.LoginRequest{Email: "locked@e.com", Password: "pass"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "temporarily locked")

	mockCache.AssertExpectations(t)
}

func TestLogin_SuccessResetsLockout(t *testing.T) {
	svc, mockDb, mockCache := newLockoutService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.DefaultCost)
	user := &types.User{
		ID: "u1", Email: "reset@e.com", PasswordHash: string(hash), Active: true, EmailVerified: true,
	}
	mockDb.On("GetUserByEmail", ctx, "reset@e.com").Return(user, nil)
	mockCache.On("Get", ctx, mock.MatchedBy(func(k string) bool {
		return strings.HasPrefix(k, "lockout:")
	})).Return("2", nil).Once()
	mockCache.On("Delete", ctx, mock.MatchedBy(func(k string) bool {
		return strings.HasPrefix(k, "lockout:")
	})).Return(nil).Once()

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "reset@e.com", Password: "pass"})
	assert.NoError(t, err)
	assert.NotNil(t, resp)

	mockCache.AssertExpectations(t)
}

func TestLogin_LockoutDisabled(t *testing.T) {
	svc, _, _ := newTestService(t)
	_ = context.Background()

	assert.False(t, svc.config.Auth.LockoutEnabled, "default service should have lockout disabled")
}

func TestExtractToken(t *testing.T) {
	// Test cases
	testCases := []struct {
		name     string
		setup    func(*gin.Context)
		expected string
	}{
		{
			"Bearer token",
			func(c *gin.Context) {
				c.Request.Header.Set("Authorization", "Bearer token123")
			},
			"token123",
		},
		{
			"Plain token",
			func(c *gin.Context) {
				c.Request.Header.Set("Authorization", "token123")
			},
			"token123",
		},
		{
			"Query parameter disabled by default",
			func(c *gin.Context) {
				c.Request.URL.RawQuery = "token=token123"
			},
			"",
		},
		{
			"No token",
			func(c *gin.Context) {},
			"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request, _ = http.NewRequest("GET", "/", nil)
			tc.setup(c)

			result := utilities.ExtractToken(c)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// =============================================================================
// F1.7.5 — JWT signing-key rotation
// =============================================================================
//
// Operators rotate by:
//   1. Move current JWTSecret to JWTPreviousSecrets[0].
//   2. Set JWTSecret to a fresh random string.
//   3. Restart API. Old sessions stay valid until they expire.
//
// The rotation contract: ValidateToken accepts tokens signed with the
// current key OR any previous key. New tokens are always signed with
// the current key.

func TestF175_TokenSignedWithPreviousKeyValidates(t *testing.T) {
	// Old secret created the token; current secret is different;
	// validate must succeed because old secret is in JWTPreviousSecrets.
	svc, _, mockCache := newTestService(t)
	oldSecret := []byte("old-secret-was-rotated")
	svc.jwtSecret = []byte("new-current-secret")
	svc.jwtPreviousSecrets = [][]byte{oldSecret}
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-id-123",
		"jti": "jti-abc",
		"iss": svc.config.Auth.JWTIssuer,
		"aud": svc.config.Auth.JWTAudience,
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	signed, err := tok.SignedString(oldSecret)
	require.NoError(t, err)

	userID, err := svc.ValidateToken(context.Background(), signed)
	require.NoError(t, err, "token signed with previous key must validate")
	assert.Equal(t, "user-id-123", userID)
}

func TestF175_TokenSignedWithUnknownKeyRejected(t *testing.T) {
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("current-secret")
	svc.jwtPreviousSecrets = [][]byte{[]byte("old-secret-1"), []byte("old-secret-2")}
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-id-evil",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("attacker-supplied-secret"))
	require.NoError(t, err)

	_, err = svc.ValidateToken(context.Background(), signed)
	require.Error(t, err, "token signed with unknown key must be rejected")
}

func TestF175_NewTokensSignedWithCurrentKeyOnly(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.jwtSecret = []byte("current-secret")
	svc.jwtPreviousSecrets = [][]byte{[]byte("old-secret")}

	signed, err := svc.GenerateToken("user-x")
	require.NoError(t, err)

	// Decode unverified to inspect; assert signature verifies with
	// current secret but NOT old secret.
	_, errCurrent := jwt.Parse(signed, func(t *jwt.Token) (interface{}, error) {
		return []byte("current-secret"), nil
	})
	require.NoError(t, errCurrent, "newly-issued token must verify against current secret")

	_, errOld := jwt.Parse(signed, func(t *jwt.Token) (interface{}, error) {
		return []byte("old-secret"), nil
	})
	require.Error(t, errOld, "newly-issued token must NOT verify against rotated-out secret")
}

// ---- Epic 34 US-34.1: Remember Me tests ----

// newTestServiceWithRememberMe creates a Service with RememberMeDuration configured.
func newTestServiceWithRememberMe(t *testing.T, rememberDur time.Duration) (*Service, *mocks.MockDatabaseService, *mocks.MockCacheService) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.RememberMeDuration = rememberDur
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	return svc, mockDb, mockCache
}

func TestGenerateTokenWithDuration_CorrectExpiry(t *testing.T) {
	svc, _, _ := newTestService(t)
	dur := 720 * time.Hour
	token, err := svc.GenerateTokenWithDuration("user-1", dur)
	require.NoError(t, err)
	parsed, _, err := new(jwt.Parser).ParseUnverified(token, jwt.MapClaims{})
	require.NoError(t, err)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	exp := time.Unix(int64(claims["exp"].(float64)), 0)
	diff := exp.Sub(time.Now().Add(dur))
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("exp should be ~now+%v, diff=%v", dur, diff)
	}
}

func TestGenerateToken_DelegatesWithTokenDuration(t *testing.T) {
	svc, _, _ := newTestService(t)
	t1, err1 := svc.GenerateToken("u1")
	t2, err2 := svc.GenerateTokenWithDuration("u1", 24*time.Hour)
	require.NoError(t, err1)
	require.NoError(t, err2)
	// Both tokens should have exp ~now+24h
	parse := func(tok string) float64 {
		p, _, _ := new(jwt.Parser).ParseUnverified(tok, jwt.MapClaims{})
		return p.Claims.(jwt.MapClaims)["exp"].(float64)
	}
	diff := parse(t1) - parse(t2)
	if diff < -2 || diff > 2 {
		t.Errorf("GenerateToken and GenerateTokenWithDuration(24h) should produce same-TTL tokens, exp diff=%v", diff)
	}
}

// setupLoginUser wires a mock db to return a valid bcrypt-hashed user for login.
func setupLoginUser(t *testing.T, mockDb *mocks.MockDatabaseService, userID, email, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	user := &types.User{ID: userID, Email: email, PasswordHash: string(hash), Active: true, EmailVerified: true, Role: "user"}
	mockDb.On("GetUserByEmail", mock.Anything, email).Return(user, nil)
}

func TestLogin_RememberMe_True_Generates30dJWT(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: true})
	require.NoError(t, err)

	parsed, _, err := new(jwt.Parser).ParseUnverified(resp.Token, jwt.MapClaims{})
	require.NoError(t, err)
	exp := time.Unix(int64(parsed.Claims.(jwt.MapClaims)["exp"].(float64)), 0)
	diff := exp.Sub(time.Now().Add(720 * time.Hour))
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("RememberMe=true: exp should be ~now+720h, diff=%v", diff)
	}
}

func TestLogin_RememberMe_False_Generates24hJWT(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: false})
	require.NoError(t, err)

	parsed, _, _ := new(jwt.Parser).ParseUnverified(resp.Token, jwt.MapClaims{})
	exp := time.Unix(int64(parsed.Claims.(jwt.MapClaims)["exp"].(float64)), 0)
	diff := exp.Sub(time.Now().Add(24 * time.Hour))
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("RememberMe=false: exp should be ~now+24h, diff=%v", diff)
	}
}

func TestLogin_RememberMe_Absent_DefaultsFalse(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	// Zero-value LoginRequest (RememberMe defaults to false)
	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123"})
	require.NoError(t, err)

	parsed, _, _ := new(jwt.Parser).ParseUnverified(resp.Token, jwt.MapClaims{})
	exp := time.Unix(int64(parsed.Claims.(jwt.MapClaims)["exp"].(float64)), 0)
	diff := exp.Sub(time.Now().Add(24 * time.Hour))
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("RememberMe absent: exp should be ~now+24h (same as false), diff=%v", diff)
	}
}

func TestLogin_RememberMeDurationZero_FallsBackToTokenDuration(t *testing.T) {
	// RememberMeDuration=0 means feature disabled; RememberMe:true falls back to tokenDuration
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 0)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: true})
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, resp.TokenTTL, "RememberMeDuration=0: TokenTTL should fall back to tokenDuration")
}

func TestLogin_TokenTTLPopulated(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: false})
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, resp.TokenTTL, "TokenTTL should equal tokenDuration when RememberMe=false")
}

func TestLogin_TokenTTLPopulated_RememberMe(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	resp, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: true})
	require.NoError(t, err)
	assert.Equal(t, 720*time.Hour, resp.TokenTTL, "TokenTTL should equal rememberMeDuration when RememberMe=true")
}

func TestLogin_RememberMe_DEKTTLIs30d(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	ks := &fakeKeyService{}
	svc.keyService = ks

	_, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: true})
	require.NoError(t, err)
	require.Len(t, ks.unlockCalls, 1, "UnlockDEK must be called once")
	assert.Equal(t, 720*time.Hour, ks.unlockCalls[0].TTL, "DEK TTL should equal rememberMeDuration")
}

func TestLogin_NoRememberMe_DEKTTLIsStandard(t *testing.T) {
	svc, mockDb, mockCache := newTestServiceWithRememberMe(t, 720*time.Hour)
	ctx := context.Background()
	setupLoginUser(t, mockDb, "u1", "alice@test.com", "password123")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil)
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)

	ks := &fakeKeyService{}
	svc.keyService = ks

	_, err := svc.Login(ctx, types.LoginRequest{Email: "alice@test.com", Password: "password123", RememberMe: false})
	require.NoError(t, err)
	require.Len(t, ks.unlockCalls, 1, "UnlockDEK must be called once")
	assert.Equal(t, 24*time.Hour, ks.unlockCalls[0].TTL, "DEK TTL should equal tokenDuration")
}

func TestRegister_TokenTTLPopulated(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()
	mockDb.On("GetUserByEmail", ctx, "new@example.com").Return(nil, nil)
	mockDb.On("CreateUser", ctx, mock.MatchedBy(func(u *types.User) bool {
		return u.Email == "new@example.com"
	})).Return(nil)
	mockDb.On("UpdateUser", ctx, mock.Anything, mock.Anything).Return(nil).Maybe()
	mockDb.On("GetUser", ctx, mock.AnythingOfType("string")).Return(&types.User{
		ID: "u-new", Email: "new@example.com", Username: "newuser",
		PasswordHash: "$2a$10$dummy", Active: true, EmailVerified: true, Role: "user",
	}, nil)

	resp, err := svc.Register(ctx, types.RegisterRequest{
		Username: "newuser", Email: "new@example.com", Password: "securepassword123",
	})
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, resp.TokenTTL, "Register should set TokenTTL = tokenDuration")
}

// ---- Epic 34 US-34.1: auth.New warn tests ----

func TestNew_RememberMeShorterThanToken_LogsWarning(t *testing.T) {
	log, logs := logger.NewObserved()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.RememberMeDuration = 1 * time.Minute // shorter than tokenDuration
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	_, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	if logs.Len() == 0 {
		t.Fatal("expected Warn when rememberMeDuration < tokenDuration")
	}
	entry := logs.All()[0]
	if !strings.Contains(entry.Message, "rememberMeDuration") {
		t.Errorf("Warn message should mention rememberMeDuration, got: %q", entry.Message)
	}
}

func TestNew_RememberMeZero_NoWarning(t *testing.T) {
	log, logs := logger.NewObserved()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.RememberMeDuration = 0 // disabled
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	_, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	if logs.Len() != 0 {
		t.Errorf("expected no Warn when rememberMeDuration=0 (disabled), got %d entries", logs.Len())
	}
}

func TestNew_RememberMeLongerThanToken_NoWarning(t *testing.T) {
	log, logs := logger.NewObserved()
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.RememberMeDuration = 720 * time.Hour // 30 days — correct
	cfg.Auth.APIKeyPrefix = "lsp_"
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	_, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	if logs.Len() != 0 {
		t.Errorf("expected no Warn for normal config, got %d entries", logs.Len())
	}
}

// ---- Auth failure metric wiring tests ----

func TestLogin_WrongPassword_RecordsAuthFailureMetric(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "user@example.com").Return(&types.User{
		ID:           "u1",
		PasswordHash: string(hash),
		Active:       true, EmailVerified: true,
	}, nil)

	before := gatherAuthFailureCount(t, "wrong_password")
	_, _ = svc.Login(ctx, types.LoginRequest{Email: "user@example.com", Password: "wrong"})
	after := gatherAuthFailureCount(t, "wrong_password")
	assert.Equal(t, before+1, after)
}

func TestLogin_UserNotFound_RecordsAuthFailureMetric(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	mockDb.On("GetUserByEmail", ctx, "noone@example.com").Return(nil, nil)

	before := gatherAuthFailureCount(t, "user_not_found")
	_, _ = svc.Login(ctx, types.LoginRequest{Email: "noone@example.com", Password: "pw"})
	after := gatherAuthFailureCount(t, "user_not_found")
	assert.Equal(t, before+1, after)
}

func TestLogin_InactiveUser_RecordsAuthFailureMetric(t *testing.T) {
	svc, mockDb, _ := newTestService(t)
	ctx := context.Background()

	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.DefaultCost)
	mockDb.On("GetUserByEmail", ctx, "disabled@example.com").Return(&types.User{
		ID:           "u2",
		PasswordHash: string(hash),
		Active:       false,
	}, nil)

	before := gatherAuthFailureCount(t, "account_inactive")
	_, _ = svc.Login(ctx, types.LoginRequest{Email: "disabled@example.com", Password: "pw"})
	after := gatherAuthFailureCount(t, "account_inactive")
	assert.Equal(t, before+1, after)
}

// gatherAuthFailureCount reads the current value of llmsafespaces_auth_failures_total
// for a specific reason label from the default Prometheus registry.
func gatherAuthFailureCount(t *testing.T, reason string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "llmsafespaces_auth_failures_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "reason" && lp.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
