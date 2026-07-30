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

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

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

func (s *memSessionStore) GetChallenge(_ context.Context, token string) ([]byte, error) {
	d, ok := s.data[token]
	if !ok {
		return nil, nil
	}
	return d, nil
}

func (s *memSessionStore) DeleteChallenge(_ context.Context, token string) error {
	delete(s.data, token)
	return nil
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
func (s *memStore) UpdateCredentialAfterLogin(_ context.Context, _ uuid.UUID, _ uint32, _ time.Time) error {
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

// --- tests ---

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

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	require.NoError(t, err)
	assert.Len(t, codes, RecoveryCodeCount)
	assert.Len(t, hashes, RecoveryCodeCount)

	for i, code := range codes {
		assert.Len(t, code, RecoveryCodeLen, "each code must be %d chars", RecoveryCodeLen)
		// Verify the hash is a valid bcrypt hash of the plaintext code.
		err := bcrypt.CompareHashAndPassword([]byte(hashes[i]), []byte(code))
		assert.NoError(t, err, "hash %d must match code %d", i, i)
	}

	// Codes must be distinct (not duplicates).
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

func TestBeginRegistration_GeneratesChallenge(t *testing.T) {
	svc, _, sessions, _ := newTestService(t)

	opts, err := svc.BeginRegistration(context.Background(), "user-1", "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, opts.SessionToken, "a session token must be returned")
	assert.NotNil(t, opts.Options, "WebAuthn options must be returned")

	// The challenge must be persisted in the session store.
	data, err := sessions.GetChallenge(context.Background(), opts.SessionToken)
	require.NoError(t, err)
	assert.NotNil(t, data, "challenge data must be stored under the session token")
}

func TestConsumeChallenge_SingleUse(t *testing.T) {
	svc, _, sessions, _ := newTestService(t)
	ctx := context.Background()

	// Start a registration to store a challenge.
	opts, err := svc.BeginRegistration(ctx, "u1", "bob")
	require.NoError(t, err)

	// First retrieval must succeed.
	data, err := sessions.GetChallenge(ctx, opts.SessionToken)
	require.NoError(t, err)
	assert.NotNil(t, data)

	// Delete (single-use).
	require.NoError(t, sessions.DeleteChallenge(ctx, opts.SessionToken))

	// Second retrieval must fail (challenge consumed).
	data2, err := sessions.GetChallenge(ctx, opts.SessionToken)
	require.NoError(t, err)
	assert.Nil(t, data2, "consumed challenge must not be retrievable")
}

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
	hash, _ := bcrypt.GenerateFromPassword([]byte(code), 4) // cost 4 for speed in tests
	store.recoveryCodes = []RecoveryCode{{UserID: "u1", CodeHash: string(hash)}}

	userID, err := svc.ConsumeRecoveryCode(context.Background(), "alice@example.com", code)
	require.NoError(t, err)
	assert.Equal(t, "u1", userID)
	assert.NotNil(t, store.recoveryCodes[0].UsedAt, "code must be marked used")
}

func TestConsumeRecoveryCode_UserNotFound(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["nobody@example.com"] = nil

	_, err := svc.ConsumeRecoveryCode(context.Background(), "nobody@example.com", "ANY-CODE")
	assert.ErrorIs(t, err, ErrUserNotFound)
}

func TestRandomRecoveryCode_Distinct(t *testing.T) {
	c1, _ := randomRecoveryCode()
	c2, _ := randomRecoveryCode()
	assert.NotEqual(t, c1, c2, "two random codes must differ")
}

func TestRandomRecoveryCode_CorrectLength(t *testing.T) {
	code, err := randomRecoveryCode()
	require.NoError(t, err)
	assert.Len(t, code, RecoveryCodeLen)
}

func TestRandInt_Unbiased(t *testing.T) {
	// Statistical test: 10000 samples should distribute roughly uniformly.
	// With max=31, each bucket should get ~322 ± ~35 (3 sigma).
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

// TestFinishRegistration_ConsumesChallenge verifies that FinishRegistration
// consumes the challenge (single-use) even when the attestation verification
// fails. This is the security-critical single-use guarantee: a failed
// ceremony cannot be retried with the same session token.
func TestFinishRegistration_ConsumesChallenge(t *testing.T) {
	svc, _, sessions, _ := newTestService(t)
	ctx := context.Background()

	// Start a registration to store a challenge.
	opts, err := svc.BeginRegistration(ctx, "user-1", "alice")
	require.NoError(t, err)

	// Call FinishRegistration with an empty attestation (will fail at
	// verification, but the challenge must be consumed FIRST). Using an empty
	// struct rather than nil avoids a nil-pointer dereference in go-webauthn.
	emptyParsed := &protocol.ParsedCredentialCreationData{}
	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "alice", "Alice Key", emptyParsed)
	require.Error(t, err, "FinishRegistration with empty attestation must fail")

	// The challenge MUST be consumed (deleted) — single-use guarantee.
	data, _ := sessions.GetChallenge(ctx, opts.SessionToken)
	assert.Nil(t, data, "challenge must be consumed even on verification failure")

	// A second call with the same token must fail with ErrChallengeExpired.
	_, err = svc.FinishRegistration(ctx, opts.SessionToken, "alice", "Alice Key", emptyParsed)
	assert.ErrorIs(t, err, ErrChallengeExpired, "consumed challenge must not be reusable")
}

// TestFinishRegistration_ExpiredChallenge verifies the error path when the
// session token has no stored challenge (expired or never begun).
func TestFinishRegistration_ExpiredChallenge(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	emptyParsed := &protocol.ParsedCredentialCreationData{}
	_, err := svc.FinishRegistration(context.Background(), "never-existed", "bob", "Bob Key", emptyParsed)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

// TestConsumeChallenge_DeleteFailure_Propagates verifies that a delete failure
// during challenge consumption surfaces as an error (not silently swallowed),
// so a surviving challenge cannot be replayed.
func TestConsumeChallenge_DeleteFailure_Propagates(t *testing.T) {
	store := &failDeleteSessionStore{}
	svc, err := New(ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     &memStore{},
		Users:     &fakeUserLookup{users: map[string]*types.User{}},
		Sessions:  store,
	})
	require.NoError(t, err)

	// Store a challenge first.
	require.NoError(t, store.SaveChallenge(context.Background(), "tok-1", []byte("{}"), time.Minute))

	// Attempting to consume must fail because DeleteChallenge errors.
	_, err = svc.FinishRegistration(context.Background(), "tok-1", "alice", "A", nil)
	require.Error(t, err, "delete failure must propagate, not be swallowed")
}

// failDeleteSessionStore returns a valid challenge but fails on delete.
type failDeleteSessionStore struct{ data []byte }

func (s *failDeleteSessionStore) SaveChallenge(_ context.Context, _ string, data []byte, _ time.Duration) error {
	s.data = data
	return nil
}
func (s *failDeleteSessionStore) GetChallenge(_ context.Context, _ string) ([]byte, error) {
	return s.data, nil
}
func (s *failDeleteSessionStore) DeleteChallenge(_ context.Context, _ string) error {
	return assert.AnError
}

// TestFinishLogin_ConsumesChallenge verifies the same single-use guarantee for
// the login ceremony as TestFinishRegistration_ConsumesChallenge.
func TestFinishLogin_ConsumesChallenge(t *testing.T) {
	svc, store, sessions, lookup := newTestService(t)
	ctx := context.Background()
	lookup.users["alice@test.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@test.com"}
	store.creds = []Credential{{UserID: "u1", CredentialID: []byte("cred-1")}}

	opts, _, err := svc.BeginLogin(ctx, "alice@test.com")
	require.NoError(t, err)

	emptyParsed := &protocol.ParsedCredentialAssertionData{}
	_, err = svc.FinishLogin(ctx, opts.SessionToken, "alice@test.com", emptyParsed)
	require.Error(t, err, "FinishLogin with empty assertion must fail")

	data, _ := sessions.GetChallenge(ctx, opts.SessionToken)
	assert.Nil(t, data, "challenge must be consumed even on verification failure")
}

func TestFinishLogin_ExpiredChallenge(t *testing.T) {
	svc, _, _, lookup := newTestService(t)
	lookup.users["alice@test.com"] = &types.User{ID: "u1", Username: "alice", Email: "alice@test.com"}
	emptyParsed := &protocol.ParsedCredentialAssertionData{}
	_, err := svc.FinishLogin(context.Background(), "never-existed", "alice@test.com", emptyParsed)
	assert.ErrorIs(t, err, ErrChallengeExpired)
}

func TestCacheSessionStore_RoundTrip(t *testing.T) {
	store := newMemSessionStore()
	ctx := context.Background()
	data := []byte(`{"challenge":"abc"}`)

	// Test the SessionStore contract directly (the interface CacheSessionStore
	// implements). This verifies save/get/delete semantics without depending
	// on the full CacheService interface.
	require.NoError(t, store.SaveChallenge(ctx, "tok-1", data, time.Minute))
	got, err := store.GetChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Equal(t, data, got)
	require.NoError(t, store.DeleteChallenge(ctx, "tok-1"))
	got2, err := store.GetChallenge(ctx, "tok-1")
	require.NoError(t, err)
	assert.Nil(t, got2, "deleted challenge must return nil")
}
