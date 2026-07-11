// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Epic 56 (Step 2): parseTokenAcceptingRotatedKeys must surface the
// matched signing key alongside the parsed token so downstream callers
// (auth middleware → KeyService.GetDEK rehydrate path) can derive the
// per-session KEK from the SAME key the JWT validated under. Wrapping
// the durable DEK with the active key when validation matched a previous
// key produces unwrap failure exactly when auto-recovery should work —
// the [HIGH] finding from PR #411 review pass 1.

// claimsForSvc builds a MapClaims map carrying the configured iss/aud for
// the given service. Use this in every test that forges a token meant to
// pass validation — the auth service rejects tokens missing iss/aud.
func claimsForSvc(svc *Service, sub, jti string) jwt.MapClaims {
	c := jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	if jti != "" {
		c["jti"] = jti
	}
	if svc != nil && svc.config != nil {
		if iss := svc.config.Auth.JWTIssuer; iss != "" {
			c["iss"] = iss
		}
		if aud := svc.config.Auth.JWTAudience; aud != "" {
			c["aud"] = aud
		}
	}
	return c
}

func TestParseTokenAcceptingRotatedKeys_ReturnsMatchedActiveKey(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1",
		"jti": "jti-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	parsed, matched, idx, err := svc.parseTokenAcceptingRotatedKeys(signed)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.True(t, parsed.Valid)
	assert.Equal(t, []byte("active-key"), matched, "matched key should equal active key")
	assert.Equal(t, 0, idx, "idx 0 = active key")
}

func TestParseTokenAcceptingRotatedKeys_ReturnsMatchedPreviousKey(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.jwtSecret = []byte("current-key")
	// First previous key (idx=1), second previous key (idx=2)
	svc.jwtPreviousSecrets = [][]byte{[]byte("prev-key-1"), []byte("prev-key-2")}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-2",
		"jti": "jti-2",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("prev-key-2"))
	require.NoError(t, err)

	parsed, matched, idx, err := svc.parseTokenAcceptingRotatedKeys(signed)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	require.True(t, parsed.Valid)
	assert.Equal(t, []byte("prev-key-2"), matched, "matched key should equal the 2nd previous key")
	assert.Equal(t, 2, idx, "idx 2 = jwtPreviousSecrets[1] (active=0, prev[0]=1, prev[1]=2)")
}

func TestParseTokenAcceptingRotatedKeys_UnknownKeyRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.jwtSecret = []byte("current-key")
	svc.jwtPreviousSecrets = [][]byte{[]byte("prev-1")}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-3",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("attacker-key"))
	require.NoError(t, err)

	parsed, matched, idx, err := svc.parseTokenAcceptingRotatedKeys(signed)
	require.Error(t, err, "token signed with unknown key must be rejected")
	assert.Nil(t, parsed)
	assert.Nil(t, matched, "no key matches → nil matched key")
	assert.Equal(t, -1, idx, "unmatched → idx = -1 sentinel")
}

func TestParseTokenAcceptingRotatedKeys_ExpiredTokenRejected(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-expired",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	_, _, _, err = svc.parseTokenAcceptingRotatedKeys(signed)
	require.Error(t, err, "expired token must be rejected")
}

// Regression: ValidateToken must still work after the signature change
// (RevokeToken and ValidateTokenWithClientIP both call this function and
// ignore the new return values).
func TestValidateToken_StillWorksAfterSignatureChange(t *testing.T) {
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsForSvc(svc, "u-regression", "jti-regression"))
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	userID, err := svc.ValidateToken(context.Background(), signed)
	require.NoError(t, err)
	assert.Equal(t, "u-regression", userID)
}

// Regression: RevokeToken must still work after the signature change.
func TestRevokeToken_StillWorksAfterSignatureChange(t *testing.T) {
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil
	mockCache.On("Set", mock.Anything, mock.Anything, "revoked", mock.Anything).Return(nil)
	mockCache.On("Set", mock.Anything, mock.Anything, "revoked", mock.Anything).Return(nil)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-rev",
		"jti": "jti-rev",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	err = svc.RevokeToken(context.Background(), signed)
	require.NoError(t, err)
}

// Epic 56 (Step 3): the token-validation cache value must carry the
// matched-key index alongside the userID so a cache hit can surface the
// matched key without re-parsing the JWT.
//
// Format: "userID|matchedKeyIndex" (new), bare "userID" (legacy / pre-deploy).
// Sentinel "revoked" remains unchanged.

func TestParseValidationCacheValue(t *testing.T) {
	cases := []struct {
		name      string
		cacheVal  string
		wantUID   string
		wantIdx   int
		wantOK    bool
		isRevoked bool
	}{
		{"new format active key", "user-1|0", "user-1", 0, true, false},
		{"new format previous key", "user-2|3", "user-2", 3, true, false},
		{"legacy format (pre-deploy)", "user-3", "user-3", -1, true, false},
		{"empty string is miss", "", "", -1, false, false},
		{"revoked sentinel", "revoked", "", -1, false, true},
		{"malformed delimiter", "user-4|", "user-4", -1, true, false},
		{"malformed non-int index", "user-5|abc", "user-5", -1, true, false},
		{"too many delimiters", "user-6|1|2", "user-6", -1, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uid, idx, ok, revoked := parseValidationCacheValue(tc.cacheVal)
			if revoked != tc.isRevoked {
				t.Errorf("revoked = %v, want %v", revoked, tc.isRevoked)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && uid != tc.wantUID {
				t.Errorf("uid = %q, want %q", uid, tc.wantUID)
			}
			if ok && idx != tc.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tc.wantIdx)
			}
		})
	}
}

func TestFormatValidationCacheValue(t *testing.T) {
	got := formatValidationCacheValue("user-1", 0)
	if got != "user-1|0" {
		t.Errorf("active-key format: got %q, want user-1|0", got)
	}
	got = formatValidationCacheValue("user-2", 7)
	if got != "user-2|7" {
		t.Errorf("previous-key format: got %q, want user-2|7", got)
	}
}

func TestValidateTokenWithClientIP_NewCacheFormatIsWritten(t *testing.T) {
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil
	// First Get returns empty (cache miss). Token validates. The Set writes
	// the new format "userID|0".
	mockCache.On("Get", mock.Anything, mock.MatchedBy(func(k string) bool {
		return len(k) > 6 && k[:6] == "token:"
	})).Return("", nil).Once()
	mockCache.On("Get", mock.Anything, mock.MatchedBy(func(k string) bool {
		return len(k) > 6 && k[:6] == "token:"
	})).Return("", nil).Maybe() // jti revocation check
	mockCache.On("Set", mock.Anything, mock.Anything, "u-cache-write|0", mock.Anything).Return(nil)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsForSvc(svc, "u-cache-write", "jti-cache-write"))
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	uid, err := svc.ValidateTokenWithClientIP(context.Background(), signed, "")
	require.NoError(t, err)
	assert.Equal(t, "u-cache-write", uid)
	mockCache.AssertCalled(t, "Set", mock.Anything, mock.Anything, "u-cache-write|0", mock.Anything)
}

func TestValidateTokenWithClientIP_HandlesLegacyCacheFormat(t *testing.T) {
	// A cache entry written by the pre-Epic-56 code still contains plain
	// "userID". The validator must accept it (returning userID) and not
	// reject it as malformed.
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil
	mockCache.On("Get", mock.Anything, mock.Anything).Return("u-legacy", nil).Once()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-legacy",
		"jti": "jti-legacy",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	uid, err := svc.ValidateTokenWithClientIP(context.Background(), signed, "")
	require.NoError(t, err)
	assert.Equal(t, "u-legacy", uid)
}

func TestValidateTokenWithClientIP_RevokedSentinelStillHonored(t *testing.T) {
	svc, _, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	mockCache.On("Get", mock.Anything, mock.Anything).Return("revoked", nil).Once()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-revoked",
		"jti": "jti-revoked",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	_, err = svc.ValidateTokenWithClientIP(context.Background(), signed, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoked")
}

// Epic 56 Step 4: AuthMiddleware must surface the matched signing key on
// the gin context so handlers (CreateAPIKey, UserProviderCredentials,
// CredentialProbe, …) can forward it to KeyService.GetDEK for durable-DEK
// rehydrate. The middleware sets two keys:
//
//   jwt_signing_key       []byte — the matched key bytes (nil for API-key auth)
//   jwt_signing_key_index int    — matched index (-1 when unknown / API-key)

func TestAuthMiddleware_SetsMatchedSigningKey_OnFreshParse(t *testing.T) {
	svc, mockDB, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key-32-bytes-padding-here")
	svc.jwtPreviousSecrets = nil

	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	user := &types.User{ID: "u-mid", Active: true, Status: types.UserStatusActive, Role: "user"}
	mockDB.On("GetUser", mock.Anything, "u-mid").Return(user, nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsForSvc(svc, "u-mid", "jti-mid"))
	signed, err := tok.SignedString([]byte("active-key-32-bytes-padding-here"))
	require.NoError(t, err)

	var capturedKey []byte
	var capturedIdx int
	r := gin.New()
	r.GET("/probe", svc.AuthMiddleware(), func(c *gin.Context) {
		if v, ok := c.Get("jwt_signing_key"); ok {
			capturedKey, _ = v.([]byte)
		}
		if v, ok := c.Get("jwt_signing_key_index"); ok {
			capturedIdx, _ = v.(int)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, []byte("active-key-32-bytes-padding-here"), capturedKey, "matched signing key must be on context after fresh parse")
	assert.Equal(t, 0, capturedIdx, "active key → idx 0")
}

func TestAuthMiddleware_SetsMatchedSigningKey_OnPreviousKey(t *testing.T) {
	svc, mockDB, mockCache := newTestService(t)
	svc.jwtSecret = []byte("current-key")
	svc.jwtPreviousSecrets = [][]byte{[]byte("prev-key-bytes-32-padded----")}

	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	user := &types.User{ID: "u-rot", Active: true, Status: types.UserStatusActive, Role: "user"}
	mockDB.On("GetUser", mock.Anything, "u-rot").Return(user, nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsForSvc(svc, "u-rot", "jti-rot"))
	signed, err := tok.SignedString([]byte("prev-key-bytes-32-padded----"))
	require.NoError(t, err)

	var capturedKey []byte
	var capturedIdx int
	r := gin.New()
	r.GET("/probe", svc.AuthMiddleware(), func(c *gin.Context) {
		if v, ok := c.Get("jwt_signing_key"); ok {
			capturedKey, _ = v.([]byte)
		}
		if v, ok := c.Get("jwt_signing_key_index"); ok {
			capturedIdx, _ = v.(int)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, []byte("prev-key-bytes-32-padded----"), capturedKey, "previous key bytes should be on context")
	assert.Equal(t, 1, capturedIdx, "first previous key → idx 1")
}

func TestAuthMiddleware_SetsMatchedSigningKey_OnCacheHit(t *testing.T) {
	// Cache hit must NOT skip matched-key resolution: the new cache format
	// "userID|idx" lets the middleware avoid a re-parse.
	svc, mockDB, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")
	svc.jwtPreviousSecrets = nil

	// Token cache returns the new-format value; suspension-marker check
	// returns empty (user not suspended).
	mockCache.On("Get", mock.Anything, mock.MatchedBy(func(k string) bool { return len(k) >= 6 && k[:6] == "token:" })).Return("u-cache|0", nil)
	mockCache.On("Get", mock.Anything, mock.MatchedBy(func(k string) bool { return len(k) >= 15 && k[:15] == "user_suspended:" })).Return("", nil).Maybe()
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	user := &types.User{ID: "u-cache", Active: true, Status: types.UserStatusActive, Role: "user"}
	mockDB.On("GetUser", mock.Anything, "u-cache").Return(user, nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-cache",
		"jti": "jti-cache",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	var capturedKey []byte
	r := gin.New()
	r.GET("/probe", svc.AuthMiddleware(), func(c *gin.Context) {
		if v, ok := c.Get("jwt_signing_key"); ok {
			capturedKey, _ = v.([]byte)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, []byte("active-key"), capturedKey, "cache hit with new format should still surface the matched key")
}

func TestAuthMiddleware_LegacyCacheHit_LeavesMatchedKeyNil(t *testing.T) {
	// Legacy cache entry (bare "userID") gives matchedIdx = -1, which
	// signingKeyByIndex resolves to nil. GetDEK callers see nil and fall
	// through to soft-unlock instead of crashing — graceful degrade.
	svc, mockDB, mockCache := newTestService(t)
	svc.jwtSecret = []byte("active-key")

	mockCache.On("Get", mock.Anything, mock.MatchedBy(func(k string) bool { return len(k) >= 6 && k[:6] == "token:" })).Return("u-legacy", nil)
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	user := &types.User{ID: "u-legacy", Active: true, Status: types.UserStatusActive, Role: "user"}
	mockDB.On("GetUser", mock.Anything, "u-legacy").Return(user, nil).Maybe()

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-legacy",
		"jti": "jti-legacy",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("active-key"))
	require.NoError(t, err)

	var hasKey bool
	var capturedIdx int
	r := gin.New()
	r.GET("/probe", svc.AuthMiddleware(), func(c *gin.Context) {
		if v, ok := c.Get("jwt_signing_key"); ok {
			b, _ := v.([]byte)
			hasKey = b != nil
		}
		if v, ok := c.Get("jwt_signing_key_index"); ok {
			capturedIdx, _ = v.(int)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.False(t, hasKey, "legacy cache hit should leave jwt_signing_key nil")
	assert.Equal(t, -1, capturedIdx, "legacy cache hit idx = -1 sentinel")
}

func TestAuthMiddleware_APIKey_LeavesMatchedKeyNil(t *testing.T) {
	// API-key auth does not have a JWT signing key; the context flags
	// stay nil/-1 so KeyService.GetDEK rehydrate skips the JWT path.
	svc, mockDB, mockCache := newTestService(t)

	apiKey := svc.config.Auth.APIKeyPrefix + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mockCache.On("Get", mock.Anything, mock.Anything).Return("", nil).Maybe()
	mockCache.On("Set", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	user := &types.User{ID: "u-api", Active: true, Status: types.UserStatusActive, Role: "user"}
	mockDB.On("GetUserByAPIKey", mock.Anything, mock.Anything).Return(user, nil)
	mockDB.On("GetUser", mock.Anything, "u-api").Return(user, nil).Maybe()

	var hasKey bool
	var capturedIdx int
	r := gin.New()
	r.GET("/probe", svc.AuthMiddleware(), func(c *gin.Context) {
		if v, ok := c.Get("jwt_signing_key"); ok {
			b, _ := v.([]byte)
			hasKey = b != nil
		}
		if v, ok := c.Get("jwt_signing_key_index"); ok {
			capturedIdx, _ = v.(int)
		} else {
			capturedIdx = -1
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.False(t, hasKey, "API-key auth should leave jwt_signing_key nil")
	assert.Equal(t, -1, capturedIdx)
}

// (end of file)
