// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Integration test for the SSO login → DEK provisioning wiring that is the
// load-bearing integration this epic introduces. Unlike the unit tests
// (sso_keymanager_test.go uses a fakeKeyManager; auth_serverkek_test.go uses a
// fakeKeyService), this test wires a REAL auth.Service (constructed with a fake
// key service) as the sso.UserKeyManager, so the behavioral contract at the
// seam — does sso.issueSession actually trigger provisioning + unlock through
// the real auth.Service? — is exercised at the service layer. Lives in package
// sso (internal) so it can call the private issueSession directly; there is no
// import cycle (auth does not import sso).
package sso

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// integrationKeyService is a minimal auth.KeyServiceInterface fake that records
// every call. It is local to this test (not the auth package's fakeKeyService)
// so this integration test stays independent of auth's test internals.
type integrationKeyService struct {
	hasKeysUserID        string
	hasKeysResult        bool
	hasKeysErr           error
	serverKEKInitUserIDs []string
	serverKEKInitErr     error
	unlockCalls          []string
}

func (r *integrationKeyService) InitializeUserKeysServerKEK(_ context.Context, userID, _ string) error {
	r.serverKEKInitUserIDs = append(r.serverKEKInitUserIDs, userID)
	return r.serverKEKInitErr
}
func (r *integrationKeyService) UnlockDEK(_ context.Context, userID string, _ []byte, _ string, _ time.Duration) error {
	r.unlockCalls = append(r.unlockCalls, userID)
	return nil
}
func (r *integrationKeyService) UnlockDEKWithSigningKey(ctx context.Context, userID string, _ []byte, _ string, _ time.Duration, _ []byte) error {
	return r.UnlockDEK(ctx, userID, nil, "", 0)
}
func (r *integrationKeyService) DeleteDurableSessionsForUser(_ context.Context, _ string) error {
	return nil
}
func (r *integrationKeyService) HasKeys(_ context.Context, userID string) (bool, error) {
	r.hasKeysUserID = userID
	return r.hasKeysResult, r.hasKeysErr
}
func (r *integrationKeyService) GetDEK(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return nil, secrets.ErrDEKUnavailable
}
func (r *integrationKeyService) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}

func newRealAuthService(t *testing.T) *auth.Service {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	svc, err := auth.New(cfg, log, nil, nil)
	require.NoError(t, err)
	return svc
}

// TestIntegration_SSOIssueSession_ProvisionsAndUnlocks_ThroughRealAuthService
// is the load-bearing integration test: a real auth.Service is wired as the
// sso.UserKeyManager, and sso.issueSession must (1) route through it (not the
// plain issuer), (2) trigger server-KEK provisioning when the user has no keys,
// (3) unlock the new session. This catches interface drift, wiring bugs, or
// orchestration issues at the seam that the both-sides-faked unit tests miss.
func TestIntegration_SSOIssueSession_ProvisionsAndUnlocks_ThroughRealAuthService(t *testing.T) {
	authSvc := newRealAuthService(t)
	keys := &integrationKeyService{hasKeysResult: false} // brand-new SSO user
	authSvc.SetKeyService(keys)

	svc := newIntegrationSSOService(t, authSvc)

	tok, err := svc.issueSession(context.Background(), "user-sso-int-1")
	require.NoError(t, err)
	require.NotEmpty(t, tok, "a valid session token must come back through the real auth.Service")

	assert.Equal(t, "user-sso-int-1", keys.hasKeysUserID, "auth.Service must have consulted HasKeys")
	assert.Equal(t, []string{"user-sso-int-1"}, keys.serverKEKInitUserIDs,
		"the real auth.Service must have triggered server-KEK provisioning via the key service")
	require.Len(t, keys.unlockCalls, 1, "the new session must be unlocked")
	assert.Equal(t, "user-sso-int-1", keys.unlockCalls[0])
}

// TestIntegration_SSOIssueSession_HasKeysError_DoesNotProvision is the
// integration-level guard for the data-loss fix: a HasKeys error must not
// trigger provisioning through the real auth.Service wired as the key manager.
func TestIntegration_SSOIssueSession_HasKeysError_DoesNotProvision(t *testing.T) {
	authSvc := newRealAuthService(t)
	keys := &integrationKeyService{
		hasKeysResult: false,
		hasKeysErr:    ssoHasKeysErr("transient DB error"),
	}
	authSvc.SetKeyService(keys)

	svc := newIntegrationSSOService(t, authSvc)

	tok, err := svc.issueSession(context.Background(), "user-sso-int-2")
	require.NoError(t, err, "login must not fail on a HasKeys error")
	assert.NotEmpty(t, tok)
	assert.Empty(t, keys.serverKEKInitUserIDs,
		"provisioning must NOT run when HasKeys errored — it would overwrite an existing DEK")
}

func newIntegrationSSOService(t *testing.T, authSvc *auth.Service) *Service {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	kp, err := secrets.NewStaticKeyProvider(key)
	require.NoError(t, err)
	svc, err := New(&fakeOrgStore{}, &fakeUserStore{}, ServiceConfig{
		TokenIssuer: integrationIssuer{},
		KeyManager:  authSvc,
		KeyProvider: kp,
		StateKey:    []byte("integration-test-state-key-0123456"),
		TokenTTL:    time.Hour,
	})
	require.NoError(t, err)
	return svc
}

// TestIntegration_SSOIssueSession_RealKeyService_DEKRoundTripsSecret is the
// highest-value integration test: it wires a REAL secrets.KeyService (with the
// actual master-KEK crypto + a real cache) behind the real auth.Service, drives
// sso.issueSession, and asserts the provisioned DEK can actually decrypt a
// personal secret. This closes the "session can decrypt a personal secret"
// gap with no fakes on the crypto path.
func TestIntegration_SSOIssueSession_RealKeyService_DEKRoundTripsSecret(t *testing.T) {
	authSvc := newRealAuthService(t)

	// Real KeyService with in-memory store + cache + a static master-KEK provider.
	realKeyStore := newRealKeyStore(t)
	realCache := newRealDEKCache()
	ks := secrets.NewKeyService(realKeyStore, realCache)
	testProv, _ := secrets.NewStaticKeyProvider(make([]byte, 32))
	ks.SetAPIKeyStore(nil, testProv)
	masterKey := make([]byte, 32)
	for i := range masterKey {
		masterKey[i] = byte(i + 7)
	}
	masterProv, err := secrets.NewStaticKeyProvider(masterKey)
	require.NoError(t, err)
	ks.SetAPIKeyStore(nil, masterProv) // wires KeyService.rootKeyProvider
	authSvc.SetKeyService(realKeyServiceAdapter{ks})

	svc := newIntegrationSSOService(t, authSvc)

	tok, err := svc.issueSession(context.Background(), "user-sso-crypto")
	require.NoError(t, err)
	require.NotEmpty(t, tok)

	// The provisioned user_keys row must be server_kek-tier.
	rec, err := realKeyStore.GetUserKey(context.Background(), "user-sso-crypto")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "server_kek", rec.DEKSource, "the provisioned DEK must be server-KEK-wrapped")
	assert.Nil(t, rec.Salt, "server_kek rows have no Argon2 salt")

	// The DEK the real KeyService unwrapped at login must round-trip a secret.
	dek := realCache.DEKForToken(t, tok)
	require.Len(t, dek, 32, "the cached DEK must be a full 32-byte AES key")
	plaintext := []byte(`{"kind":"openai","apiKey":"sk-personal-secret"}`)
	ciphertext, err := secrets.EncryptSecret(dek, plaintext)
	require.NoError(t, err)
	decrypted, err := secrets.DecryptSecret(dek, ciphertext)
	require.NoError(t, err, "the login-unlocked DEK must decrypt a personal secret")
	assert.Equal(t, plaintext, decrypted)
}

// --- real-crypto test helpers ---

// realKeyStore is a KeyStore backed by an in-memory map. It is a thin shim over
// pkg/secrets' test mockKeyStore (unexported) — re-implemented here in the sso
// package so the integration test does not reach into secrets' test internals.
type realKeyStore struct {
	rec *secrets.UserKeyRecord
}

func newRealKeyStore(t *testing.T) *realKeyStore {
	t.Helper()
	return &realKeyStore{}
}

func (s *realKeyStore) GetUserKey(_ context.Context, userID string) (*secrets.UserKeyRecord, error) {
	if s.rec == nil || s.rec.UserID != userID {
		return nil, nil
	}
	cp := *s.rec
	return &cp, nil
}
func (s *realKeyStore) CreateUserKey(_ context.Context, record *secrets.UserKeyRecord) error {
	cp := *record
	s.rec = &cp
	return nil
}
func (s *realKeyStore) UpdateWrappedDEK(_ context.Context, _ string, _ []byte, _ []byte, _ int) error {
	return nil
}
func (s *realKeyStore) UpdateWrappedDEKAndSource(_ context.Context, _ string, _ []byte, _ []byte, _ int, _ string) error {
	return nil
}
func (s *realKeyStore) UpdateWrappedDEKRecovery(_ context.Context, _ string, _ []byte, _ []byte) error {
	return nil
}

// realDEKCache records the (sessionID → DEK) mapping so the test can recover
// the DEK the login path cached and assert it round-trips a secret.
type realDEKCache struct {
	deks map[string][]byte
}

func newRealDEKCache() *realDEKCache {
	return &realDEKCache{deks: map[string][]byte{}}
}

func (c *realDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	cp := make([]byte, len(dek))
	copy(cp, dek)
	c.deks[sessionID] = cp
	return nil
}
func (c *realDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	return c.deks[sessionID], nil
}
func (c *realDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	delete(c.deks, sessionID)
	return nil
}

// DEKForToken extracts the jti (session id) from the JWT the auth service
// issued, then returns the DEK cached under that session at login.
func (c *realDEKCache) DEKForToken(t *testing.T, tok string) []byte {
	t.Helper()
	jti := jtiFromToken(t, tok)
	dek, ok := c.deks[jti]
	require.True(t, ok, "a DEK must be cached under the issued token's jti")
	cp := make([]byte, len(dek))
	copy(cp, dek)
	return cp
}

// realKeyServiceAdapter adapts *secrets.KeyService to auth.KeyServiceInterface
// by implementing the subset of methods auth.Service calls, delegating to the
// real KeyService. The methods NOT used by IssueTokenAndUnlockDEK
// (InitializeUserKeys, DeleteDurableSessionsForUser, GetDEK, CacheDEK) return
// benign values — they are not on the path under test.
type realKeyServiceAdapter struct{ inner *secrets.KeyService }

func (a realKeyServiceAdapter) InitializeUserKeysServerKEK(ctx context.Context, userID, dekSource string) error {
	return a.inner.InitializeUserKeysServerKEK(ctx, userID, dekSource)
}
func (a realKeyServiceAdapter) UnlockDEK(ctx context.Context, userID string, pw []byte, sid string, ttl time.Duration) error {
	return a.inner.UnlockDEKWithSigningKey(ctx, userID, pw, sid, ttl, nil)
}
func (a realKeyServiceAdapter) UnlockDEKWithSigningKey(ctx context.Context, userID string, pw []byte, sid string, ttl time.Duration, sk []byte) error {
	return a.inner.UnlockDEKWithSigningKey(ctx, userID, pw, sid, ttl, sk)
}
func (a realKeyServiceAdapter) DeleteDurableSessionsForUser(_ context.Context, _ string) error {
	return nil
}
func (a realKeyServiceAdapter) HasKeys(ctx context.Context, userID string) (bool, error) {
	return a.inner.HasKeys(ctx, userID)
}
func (a realKeyServiceAdapter) GetDEK(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return nil, secrets.ErrDEKUnavailable
}
func (a realKeyServiceAdapter) CacheDEK(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}

type integrationIssuer struct{}

func (integrationIssuer) GenerateToken(_ string) (string, error) {
	return "SHOULD-NOT-BE-USED", nil
}

type ssoHasKeysErr string

func (e ssoHasKeysErr) Error() string { return string(e) }

// jtiFromToken parses a JWT (UNVERIFIED — test-only) and returns its jti claim.
// The auth.Service signs with a test secret known to the test; we skip
// signature verification here because the test only needs the jti to look up
// the cached DEK, and the token was just minted by the real auth.Service under
// test — not received from an untrusted source.
func jtiFromToken(t *testing.T, tok string) string {
	t.Helper()
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := jwt.MapClaims{}
	_, _, err := parser.ParseUnverified(tok, &claims)
	require.NoError(t, err)
	jti, ok := claims["jti"].(string)
	require.True(t, ok && jti != "", "issued token must carry a non-empty jti")
	return jti
}
