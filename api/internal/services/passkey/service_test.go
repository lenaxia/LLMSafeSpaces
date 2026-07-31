// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func init() {
	// Lower the bcrypt cost for tests to avoid CI timeouts. Production uses
	// cost 12 (~250ms/hash); tests need 40 hashes × cost 4 (~1ms each) to
	// stay well under the 5-minute CI timeout under coverage instrumentation.
	recoveryBcryptCost = 4
}

// --- fakes ---

type memSessionStore struct {
	data map[string][]byte
}

func newMemSessionStore() *memSessionStore {
	return &memSessionStore{data: make(map[string][]byte)}
}

func (s *memSessionStore) SaveChallenge(_ context.Context, token string, data []byte, _ time.Duration) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	s.data[token] = cp
	return nil
}

func (s *memSessionStore) ConsumeChallenge(_ context.Context, token string) ([]byte, error) {
	data, ok := s.data[token]
	delete(s.data, token)
	if !ok {
		return nil, nil
	}
	return data, nil
}

type memStore struct {
	creds         []Credential
	recoveryCodes []RecoveryCode
}

func (s *memStore) ListCredentials(_ context.Context, _ string) ([]Credential, error) {
	return s.creds, nil
}
func (s *memStore) GetCredentialByCredentialID(_ context.Context, _ []byte) (*Credential, error) {
	return nil, nil
}
func (s *memStore) CreateCredential(_ context.Context, c *Credential) error {
	s.creds = append(s.creds, *c)
	return nil
}
func (s *memStore) UpdateCredentialAfterLogin(_ context.Context, id uuid.UUID, signCount uint32, lastUsed time.Time) error {
	for i := range s.creds {
		if s.creds[i].ID == id {
			s.creds[i].SignCount = signCount
			s.creds[i].LastUsedAt = &lastUsed
			return nil
		}
	}
	return nil
}
func (s *memStore) DeleteCredential(_ context.Context, _ string, _ uuid.UUID) error { return nil }
func (s *memStore) CountCredentials(_ context.Context, _ string) (int, error) {
	return len(s.creds), nil
}
func (s *memStore) CreateCredentialAndRecoveryCodes(_ context.Context, c *Credential, hashes []string) error {
	s.creds = append(s.creds, *c)
	for _, h := range hashes {
		s.recoveryCodes = append(s.recoveryCodes, RecoveryCode{CodeHash: h})
	}
	return nil
}
func (s *memStore) CreateRecoveryCodes(_ context.Context, _ string, hashes []string) error {
	for _, h := range hashes {
		s.recoveryCodes = append(s.recoveryCodes, RecoveryCode{CodeHash: h})
	}
	return nil
}
func (s *memStore) ListAvailableRecoveryCodes(_ context.Context, _ string) ([]RecoveryCode, error) {
	return s.recoveryCodes, nil
}
func (s *memStore) ConsumeRecoveryCode(_ context.Context, _ string, codeHash string) error {
	for i := range s.recoveryCodes {
		if s.recoveryCodes[i].CodeHash == codeHash {
			now := time.Now()
			s.recoveryCodes[i].UsedAt = &now
			return nil
		}
	}
	return ErrRecoveryCodeNotFound
}

type fakeUserLookup struct {
	users map[string]*types.User
}

func (f *fakeUserLookup) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	return f.users[email], nil
}

// --- test service factory ---

func newTestService(t *testing.T) (*Service, *memStore, *memSessionStore, *fakeUserLookup) {
	t.Helper()
	store := &memStore{}
	sessions := newMemSessionStore()
	users := &fakeUserLookup{users: make(map[string]*types.User)}
	svc, err := New(ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     store,
		Users:     users,
		Sessions:  sessions,
	})
	require.NoError(t, err)
	return svc, store, sessions, users
}

// --- constructor tests ---

func TestNew_RequiresRPID(t *testing.T) {
	_, err := New(ServiceConfig{
		RPOrigins: []string{"https://localhost"},
		Store:     &memStore{},
		Users:     &fakeUserLookup{},
		Sessions:  newMemSessionStore(),
	})
	require.Error(t, err)
}

func TestNew_RequiresRPOrigins(t *testing.T) {
	_, err := New(ServiceConfig{
		RPID:     "localhost",
		Store:    &memStore{},
		Users:    &fakeUserLookup{},
		Sessions: newMemSessionStore(),
	})
	require.Error(t, err)
}

func TestNew_RequiresStore(t *testing.T) {
	_, err := New(ServiceConfig{
		RPID:      "localhost",
		RPOrigins: []string{"https://localhost"},
		Users:     &fakeUserLookup{},
		Sessions:  newMemSessionStore(),
	})
	require.Error(t, err)
}

func TestNew_RequiresUsers(t *testing.T) {
	_, err := New(ServiceConfig{
		RPID:      "localhost",
		RPOrigins: []string{"https://localhost"},
		Store:     &memStore{},
		Sessions:  newMemSessionStore(),
	})
	require.Error(t, err)
}

func TestNew_RequiresSessions(t *testing.T) {
	_, err := New(ServiceConfig{
		RPID:      "localhost",
		RPOrigins: []string{"https://localhost"},
		Store:     &memStore{},
		Users:     &fakeUserLookup{},
	})
	require.Error(t, err)
}

// --- recovery code tests ---

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	require.NoError(t, err)
	assert.Len(t, codes, RecoveryCodeCount)
	assert.Len(t, hashes, RecoveryCodeCount)

	for i, code := range codes {
		assert.Len(t, code, RecoveryCodeLen)
		err := bcrypt.CompareHashAndPassword([]byte(hashes[i]), []byte(code))
		assert.NoError(t, err, "hash %d must match code %d", i, i)
	}

	seen := make(map[string]bool)
	for _, c := range codes {
		assert.False(t, seen[c], "recovery code must not repeat: %s", c)
		seen[c] = true
	}
}

func TestGenerateRecoveryCodes_AlphabetNoAmbiguous(t *testing.T) {
	codes, _, _ := generateRecoveryCodes(RecoveryCodeCount)
	for _, code := range codes {
		for _, c := range code {
			assert.NotContains(t, "01OoilI", string(c), "code must not contain ambiguous chars")
		}
	}
}

func TestRandomRecoveryCode_Distinct(t *testing.T) {
	c1, _ := randomRecoveryCode()
	c2, _ := randomRecoveryCode()
	assert.NotEqual(t, c1, c2)
}

func TestRandomRecoveryCode_CorrectLength(t *testing.T) {
	code, err := randomRecoveryCode()
	require.NoError(t, err)
	assert.Len(t, code, RecoveryCodeLen)
}

func TestRandInt_Unbiased(t *testing.T) {
	counts := make(map[int]int)
	for i := 0; i < 10000; i++ {
		v, err := randInt(31)
		require.NoError(t, err)
		counts[v]++
	}
	for i := 0; i < 31; i++ {
		assert.Greater(t, counts[i], 200, "bucket %d severely underrepresented: %d", i, counts[i])
	}
}

// --- challenge lifecycle tests ---

func TestBeginRegistration_GeneratesChallenge(t *testing.T) {
	svc, _, sessions, _ := newTestService(t)

	opts, err := svc.BeginRegistration(context.Background(), "user-1", "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, opts.SessionToken)
	assert.NotNil(t, opts.Options)

	data, err := sessions.ConsumeChallenge(context.Background(), opts.SessionToken)
	require.NoError(t, err)
	assert.NotNil(t, data, "challenge data must be stored")
}

func TestConsumeChallenge_SingleUse(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	opts, err := svc.BeginRegistration(ctx, "u1", "bob")
	require.NoError(t, err)

	emptyMap := map[string]any{}
	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "bob", "Bob Key", emptyMap)
	require.Error(t, err)

	// Second call with same token must fail with ErrChallengeExpired.
	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "bob", "Bob Key", emptyMap)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

// --- login error path tests ---

func TestBeginLogin_UserNotFound(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["nobody@example.com"] = nil

	_, _, err := svc.BeginLogin(context.Background(), "nobody@example.com")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestBeginLogin_NoPasskeyRegistered(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["alice@example.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@example.com"}

	_, _, err := svc.BeginLogin(context.Background(), "alice@example.com")
	assert.ErrorIs(t, err, ErrNoPasskeyRegistered)
}

func TestFinishRegistration_ExpiredChallenge(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	emptyMap := map[string]any{}
	_, err := svc.FinishRegistration(context.Background(), "never-existed", "bob", "Bob Key", emptyMap)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

func TestFinishLogin_ExpiredChallenge(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["alice@test.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@test.com"}
	emptyMap := map[string]any{}
	_, err := svc.FinishLogin(context.Background(), "never-existed", "alice@test.com", emptyMap)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

// --- recovery code consumption tests ---

func TestConsumeRecoveryCode_InvalidCode(t *testing.T) {
	svc, store, _, lookup := newTestService(t)
	lookup.users["alice@example.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@example.com"}
	store.recoveryCodes = []RecoveryCode{{CodeHash: "$2a$12$somehash"}}

	_, err := svc.ConsumeRecoveryCode(context.Background(), "alice@example.com", "WRONG-CODE")
	assert.ErrorIs(t, err, ErrRecoveryCodeNotFound)
}

func TestConsumeRecoveryCode_ValidCode_Consumes(t *testing.T) {
	svc, store, _, lookup := newTestService(t)
	lookup.users["alice@example.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@example.com"}

	code := "TESTCODE1234567890AB"
	hash, _ := bcrypt.GenerateFromPassword([]byte(code), 4)
	store.recoveryCodes = []RecoveryCode{{UserID: "u1", CodeHash: string(hash)}}

	userID, err := svc.ConsumeRecoveryCode(context.Background(), "alice@example.com", code)
	require.NoError(t, err)
	assert.Equal(t, "u1", userID)
	assert.NotNil(t, store.recoveryCodes[0].UsedAt)
}

func TestConsumeRecoveryCode_UserNotFound(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["nobody@example.com"] = nil

	_, err := svc.ConsumeRecoveryCode(context.Background(), "nobody@example.com", "ANY-CODE")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

// --- ceremony happy-path tests (using the test authenticator) ---

// TestFinishRegistration_HappyPath is the highest-value test: a real WebAuthn
// attestation is generated by the test authenticator, verified by go-webauthn,
// and the returned credential + recovery codes are checked for correctness.
// This exercises the full ceremony path: challenge matching, origin checking,
// RP ID hash validation, P-256 public key extraction.
func TestFinishRegistration_HappyPath(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()
	auth := newTestAuthenticator()

	// Begin registration to get the challenge.
	opts, err := svc.BeginRegistration(ctx, "user-happy", "alice")
	require.NoError(t, err)

	// Extract the challenge from the options (base64url-encoded).
	challenge, ok := opts.Options["publicKey"].(map[string]any)
	require.True(t, ok)
	challengeB64, ok := challenge["challenge"].(string)
	require.True(t, ok, "options must contain a challenge string")

	// Generate a valid attestation response using the test authenticator.
	parsed, err := auth.generateRegistrationResponse(challengeB64)
	require.NoError(t, err)

	// Finish registration — should verify the attestation successfully.
	result, err := svc.FinishRegistration(ctx, opts.SessionToken, "alice", "Alice Passkey", parsed)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, "user-happy", result.Credential.UserID)
	assert.NotEmpty(t, result.Credential.CredentialID)
	assert.NotEmpty(t, result.Credential.PublicKey)
	assert.Len(t, result.RecoveryCodes, RecoveryCodeCount)
	assert.Len(t, result.RecoveryCodeHashes, RecoveryCodeCount)

	// Verify recovery codes are valid bcrypt hashes.
	for i, hash := range result.RecoveryCodeHashes {
		err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(result.RecoveryCodes[i]))
		assert.NoError(t, err, "recovery code hash %d must match plaintext", i)
	}
}

// TestFinishRegistration_ConsumesChallenge_OnFailure proves single-use: a failed
// ceremony consumes the challenge so it cannot be retried with the same token.
func TestFinishRegistration_ConsumesChallenge_OnFailure(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := context.Background()

	opts, err := svc.BeginRegistration(ctx, "user-1", "alice")
	require.NoError(t, err)

	emptyMap := map[string]any{}
	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "alice", "Alice Key", emptyMap)
	require.Error(t, err)

	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "alice", "Alice Key", emptyMap)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

// TestFinishLogin_HappyPath is the login ceremony happy path: register a
// credential, then perform a real assertion that go-webauthn verifies. This
// exercises challenge matching, origin checking, RP ID hash validation, and
// ECDSA P-256 signature verification.
func TestFinishLogin_HappyPath(t *testing.T) {
	svc, store, _, lookup := newTestService(t)
	ctx := context.Background()
	auth := newTestAuthenticator()

	// Phase 1: register a credential (happy path).
	regOpts, err := svc.BeginRegistration(ctx, "user-login", "bob")
	require.NoError(t, err)
	pkOpts := regOpts.Options["publicKey"].(map[string]any)
	regChallenge := pkOpts["challenge"].(string)
	regParsed, err := auth.generateRegistrationResponse(regChallenge)
	require.NoError(t, err)
	regResult, err := svc.FinishRegistration(ctx, regOpts.SessionToken, "bob", "Bob Passkey", regParsed)
	require.NoError(t, err)

	// Persist the credential so BeginLogin can find it.
	store.creds = append(store.creds, regResult.Credential)
	lookup.users["bob@test.com"] = &types.User{ID: "user-login", Username: "bob", Email: "bob@test.com"}

	// Phase 2: perform a login assertion.
	loginOpts, _, err := svc.BeginLogin(ctx, "bob@test.com")
	require.NoError(t, err)
	loginPkOpts := loginOpts.Options["publicKey"].(map[string]any)
	loginChallenge := loginPkOpts["challenge"].(string)
	loginParsed, err := auth.generateAssertionResponse(loginChallenge)
	require.NoError(t, err)

	userID, err := svc.FinishLogin(ctx, loginOpts.SessionToken, "bob@test.com", loginParsed)
	require.NoError(t, err)
	assert.Equal(t, "user-login", userID)

	// Sign count must have been updated to the authenticator's value.
	require.NotEmpty(t, store.creds)
	assert.GreaterOrEqual(t, store.creds[0].SignCount, uint32(1), "sign count must be updated after login")
}

// TestFinishLogin_ConsumesChallenge_OnFailure proves single-use for login.
func TestFinishLogin_ConsumesChallenge_OnFailure(t *testing.T) {
	svc, store, _, lookup := newTestService(t)
	ctx := context.Background()
	lookup.users["alice@test.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@test.com"}
	store.creds = []Credential{{UserID: "u1", CredentialID: []byte("cred-1")}}

	opts, _, err := svc.BeginLogin(ctx, "alice@test.com")
	require.NoError(t, err)

	emptyMap := map[string]any{}
	_, err = svc.FinishLogin(ctx, opts.SessionToken, "alice@test.com", emptyMap)
	require.Error(t, err)

	// The challenge must be consumed (single-use).
	_, err = svc.FinishLogin(ctx, opts.SessionToken, "alice@test.com", emptyMap)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

// --- session store round-trip test ---

func TestSessionStore_RoundTrip(t *testing.T) {
	store := newMemSessionStore()
	ctx := context.Background()
	data := []byte(`{"challenge":"abc"}`)

	require.NoError(t, store.SaveChallenge(ctx, "tok-1", data, time.Minute))
	got, err := store.ConsumeChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, data, got)

	// Second consume must return nil (already consumed).
	got2, err := store.ConsumeChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Nil(t, got2, "consumed challenge must return nil")
}
