// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

// JWT iss/aud claims + validation. Today's tokens carry only sub/jti/exp/iat.
// Two gaps close here:
//
//  1. Minted tokens do not include iss/aud, so any service sharing the same
//     HMAC secret could mint accepted tokens. With explicit iss/aud, tokens
//     are bound to this issuer + audience and rejected if either mismatches.
//  2. Validation does not enforce iss/aud, so even if a token included them
//     they would be ignored.
//
// Configuration via cfg.Auth.JWTIssuer / cfg.Auth.JWTAudience. Default
// "llmsafespaces" for both. Backwards-compatibility: tokens minted before
// this change have no iss/aud — they fail validation, but tokens are
// short-lived (24h default) so rotation is fast.

func newJWTServiceWithClaims(t *testing.T, issuer, audience string) *Service {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-claims"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.JWTIssuer = issuer
	cfg.Auth.JWTAudience = audience
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	return svc
}

func TestGenerateToken_IncludesISSandAUD(t *testing.T) {
	svc := newJWTServiceWithClaims(t, "llmsafespaces", "llmsafespaces")
	token, err := svc.GenerateToken("user-1")
	require.NoError(t, err)

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected method")
		}
		return svc.jwtSecret, nil
	})
	require.NoError(t, err)

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	assert.Equal(t, "llmsafespaces", claims["iss"], "issuer must be minted")
	assert.Equal(t, "llmsafespaces", claims["aud"], "audience must be minted")
}

func TestValidateToken_RejectsMissingISS(t *testing.T) {
	svc := newJWTServiceWithClaims(t, "llmsafespaces", "llmsafespaces")

	// Forge a token WITHOUT iss/aud (the pre-fix shape).
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"jti": "test-jti",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	tokenString, err := tok.SignedString(svc.jwtSecret)
	require.NoError(t, err)

	mockCache := new(mocks.MockCacheService)
	mockCache.On("Get",
		mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return true }),
	).Return("", errors.New("not found")).Maybe()
	svc.cacheService = mockCache

	_, err = svc.ValidateToken(context.Background(), tokenString)
	require.Error(t, err, "token missing iss/aud must be rejected by validation")
}

func TestValidateToken_RejectsWrongISS(t *testing.T) {
	svc := newJWTServiceWithClaims(t, "llmsafespaces", "llmsafespaces")

	// Token with the wrong issuer.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"jti": "test-jti",
		"iss": "evil-service",
		"aud": "llmsafespaces",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	tokenString, err := tok.SignedString(svc.jwtSecret)
	require.NoError(t, err)

	mockCache := new(mocks.MockCacheService)
	mockCache.On("Get",
		mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return true }),
	).Return("", errors.New("not found")).Maybe()
	svc.cacheService = mockCache

	_, err = svc.ValidateToken(context.Background(), tokenString)
	require.Error(t, err, "token with wrong iss must be rejected")
}

func TestValidateToken_RejectsWrongAUD(t *testing.T) {
	svc := newJWTServiceWithClaims(t, "llmsafespaces", "llmsafespaces")

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"jti": "test-jti",
		"iss": "llmsafespaces",
		"aud": "evil-audience",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	})
	tokenString, err := tok.SignedString(svc.jwtSecret)
	require.NoError(t, err)

	mockCache := new(mocks.MockCacheService)
	mockCache.On("Get",
		mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return true }),
	).Return("", errors.New("not found")).Maybe()
	svc.cacheService = mockCache

	_, err = svc.ValidateToken(context.Background(), tokenString)
	require.Error(t, err, "token with wrong aud must be rejected")
}

func TestValidateToken_AcceptsCorrectISSandAUD(t *testing.T) {
	svc := newJWTServiceWithClaims(t, "llmsafespaces", "llmsafespaces")

	// Mint via the production path so claims match exactly.
	token, err := svc.GenerateToken("user-1")
	require.NoError(t, err)

	// Match any context + any cache key + any value — we just want
	// cache miss → full parse path. The ValidateToken happy path will
	// then re-cache; ignore that Set call.
	mockCache := new(mocks.MockCacheService)
	mockCache.On("Get",
		mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return true }),
	).Return("", errors.New("not found")).Maybe()
	mockCache.On("Set",
		mock.MatchedBy(func(ctx context.Context) bool { return true }),
		mock.MatchedBy(func(key string) bool { return true }),
		mock.Anything, mock.Anything,
	).Return(nil).Maybe()
	svc.cacheService = mockCache

	uid, err := svc.ValidateToken(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, "user-1", uid)
}

func TestGenerateToken_DefaultsWhenConfigEmpty(t *testing.T) {
	// When the operator doesn't set iss/aud, the defaults "llmsafespaces"
	// are used. This is the chart's out-of-the-box behaviour.
	svc := newJWTServiceWithClaims(t, "", "")
	token, err := svc.GenerateToken("user-1")
	require.NoError(t, err)

	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected method")
		}
		return svc.jwtSecret, nil
	})
	require.NoError(t, err)
	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	assert.Equal(t, config.DefaultJWTIssuer, claims["iss"])
	assert.Equal(t, config.DefaultJWTAudience, claims["aud"])
}
