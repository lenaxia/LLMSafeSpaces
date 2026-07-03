// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticSigningKeys satisfies SigningKeyEnumerator with a fixed list.
// Tests inject rotation scenarios by ordering keys — first entry is
// the "active" (most-recent) key, subsequent entries are "previous".
type staticSigningKeys struct {
	keys [][]byte
}

func (s *staticSigningKeys) EachSigningKey(fn func(key []byte) bool) {
	for _, k := range s.keys {
		// Callback contract: MUST NOT retain bytes. We copy to defend
		// against buggy callers, matching the real auth.Service impl.
		out := make([]byte, len(k))
		copy(out, k)
		if !fn(out) {
			return
		}
	}
}

// getDEKForUserFixture wires KeyService, mockJWTSessionStore, a fake
// DEKCache, and the SigningKeyEnumerator together for GetDEKForUser
// tests. Deterministic clock via store.now.
type getDEKForUserFixture struct {
	svc     *KeyService
	store   *mockJWTSessionStore
	cache   *fakeDEKCache
	userID  string
	baseTs  time.Time
	realDEK []byte // the plaintext DEK that all sessions wrap
}

func newGetDEKForUserFixture(t *testing.T) *getDEKForUserFixture {
	t.Helper()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store := newMockJWTSessionStore()
	store.now = base
	cache := &fakeDEKCache{data: map[string][]byte{}}
	svc := &KeyService{
		cache:       cache,
		jwtSessions: store,
	}
	return &getDEKForUserFixture{
		svc:     svc,
		store:   store,
		cache:   cache,
		userID:  "user-1",
		baseTs:  base,
		realDEK: []byte("this-is-a-32-byte-user-dek-abcde"),
	}
}

// addSession creates a jwt_sessions row wrapping realDEK under a KEK
// derived from signingKey || jti (matching KeyService's own login/write
// path). Returns the row so tests can assert on jti.
func (f *getDEKForUserFixture) addSession(t *testing.T, signingKey []byte, createdAt, expiresAt time.Time) *JWTSession {
	t.Helper()
	jti := uuid.New()

	kekSalt, err := GenerateSalt()
	require.NoError(t, err)

	// Derive KEK the same way UnlockDEKWithSigningKey does at login time:
	// keyMaterial = activeSigningKey || jti.String(); HKDF with kekSalt +
	// JWTSessionKEKInfo. Any change to this derivation MUST also change
	// KeyService's login/rehydrate paths — the two are coupled by the
	// on-disk wrapped_dek/kek_salt.
	keyMaterial := make([]byte, 0, len(signingKey)+36)
	keyMaterial = append(keyMaterial, signingKey...)
	keyMaterial = append(keyMaterial, []byte(jti.String())...)
	kek, err := DeriveKEKFromKey(keyMaterial, kekSalt, JWTSessionKEKInfo)
	require.NoError(t, err)
	defer zeroBytes(kek)

	wrapped, err := EncryptSecret(kek, f.realDEK)
	require.NoError(t, err)

	row := &JWTSession{
		JTI:        jti,
		UserID:     f.userID,
		WrappedDEK: wrapped,
		KEKSalt:    kekSalt,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}
	require.NoError(t, f.store.WriteJWTSession(context.Background(), row))
	return row
}

// TestGetDEKForUser_NoActiveSessions is the primary "no live user" case:
// the user has never logged in on this API, or all their sessions have
// been pruned. Must surface as ErrDEKUnavailable so callers know to
// fall back to sessionless behavior.
func TestGetDEKForUser_NoActiveSessions(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{[]byte("primary-key")}}

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"absent session must surface as ErrDEKUnavailable — same sentinel "+
			"as GetDEK's other 'no live DEK material' cases, so callers "+
			"can handle both uniformly")
}

// TestGetDEKForUser_HappyPathPrimaryKey covers the vast-majority
// scenario: the user has an active session issued under the current
// signing key. GetDEKForUser finds it, unwraps in one try, returns.
// Also verifies the Redis write-back so subsequent GetDEK(jti) calls
// hit fast-path.
func TestGetDEKForUser_HappyPathPrimaryKey(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	primary := []byte("primary-signing-key")
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{primary}}

	row := f.addSession(t, primary, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(24*time.Hour))

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek, "returned DEK must equal the plaintext used at login")

	cached, err := f.cache.GetDEK(context.Background(), row.JTI.String())
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, cached,
		"Redis write-back must have populated dek:<jti> so subsequent "+
			"GetDEK(jti, signingKey) calls avoid the PG round-trip")
}

// TestGetDEKForUser_PicksMostRecentSession locks in the session-
// selection contract. Old and new sessions coexist; we must pick the
// most-recent because it's most likely to unwrap under the primary
// (active) signing key.
func TestGetDEKForUser_PicksMostRecentSession(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	primary := []byte("primary-signing-key")
	previous := []byte("previous-signing-key")
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{primary, previous}}

	old := f.addSession(t, previous, f.baseTs.Add(-3*time.Hour), f.baseTs.Add(1*time.Hour))
	newRow := f.addSession(t, primary, f.baseTs.Add(-1*time.Hour), f.baseTs.Add(23*time.Hour))

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek)

	// Both rows wrap the same DEK, so the returned value doesn't
	// distinguish. But the cache-populate side effect targets a
	// specific jti — that's what we assert on.
	newCached, err := f.cache.GetDEK(context.Background(), newRow.JTI.String())
	require.NoError(t, err)
	assert.NotNil(t, newCached, "most-recent row's jti must be cached")
	oldCached, err := f.cache.GetDEK(context.Background(), old.JTI.String())
	require.NoError(t, err)
	assert.Nil(t, oldCached, "older row MUST NOT have been touched")
}

// TestGetDEKForUser_FallsBackToPreviousSigningKey covers post-rotation:
// the primary signing key was rotated recently, so the user's most-
// recent session is wrapped under the previous key. Iteration must
// find the correct key on try 2 (or later).
func TestGetDEKForUser_FallsBackToPreviousSigningKey(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	primary := []byte("primary-signing-key")
	previous := []byte("previous-signing-key")
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{primary, previous}}

	f.addSession(t, previous, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(23*time.Hour))

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek,
		"iteration must try primary then previous; second try must succeed")
}

// TestGetDEKForUser_UnwrappableSurfacesDEKUnavailable covers the case
// where NO signing key we know can unwrap the row. Only realistic
// failure: user rotated password since ALL currently-known signing
// keys were issued (i.e., rotated more times than the retention
// window), or the DEK was wrapped under an out-of-band-issued key.
// Must not silently succeed with garbage.
func TestGetDEKForUser_UnwrappableSurfacesDEKUnavailable(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	// Session wrapped under "old-key-not-in-rotation."
	unknown := []byte("unknown-key-not-in-rotation")
	f.addSession(t, unknown, f.baseTs.Add(-30*time.Minute), f.baseTs.Add(23*time.Hour))

	// Enumerator knows only current keys — none match.
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{
		[]byte("primary-signing-key"),
		[]byte("previous-signing-key"),
	}}

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"exhausted keys with no unwrap MUST surface as ErrDEKUnavailable, "+
			"not a generic error, so callers can treat it uniformly with "+
			"the 'no session' case")
}

// TestGetDEKForUser_UsesCachedDEKIfPresent proves the fast path: if
// Redis already has a DEK cached for one of the user's live jtis, we
// return that WITHOUT querying jwt_sessions or iterating signing keys.
// This is the perf-sensitive hot path when the same background caller
// is invoked repeatedly for the same user.
func TestGetDEKForUser_UsesCachedDEKIfPresent(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{[]byte("primary-signing-key")}}

	// The session exists (listed by store) AND the DEK is pre-cached.
	row := f.addSession(t, []byte("primary-signing-key"),
		f.baseTs.Add(-30*time.Minute), f.baseTs.Add(23*time.Hour))
	require.NoError(t, f.cache.CacheDEK(context.Background(), row.JTI.String(), f.realDEK, time.Hour))

	// Sanity: no unwrap should happen. We prove that by making the row's
	// stored WrappedDEK garbage — if the code path fell through to
	// unwrap, it would fail; the cache-hit path bypasses unwrap.
	f.store.mu.Lock()
	stored := f.store.rows[row.JTI]
	stored.WrappedDEK = []byte("not-a-real-wrap")
	f.store.mu.Unlock()

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	require.NoError(t, err)
	assert.Equal(t, f.realDEK, dek,
		"cache hit must short-circuit before unwrap; corrupted stored "+
			"WrappedDEK must not affect the return value")
}

// TestGetDEKForUser_ListErrorPropagates proves errors from the store
// bubble up, not silently converted to ErrDEKUnavailable — a genuine
// PG outage should be observable by the caller.
func TestGetDEKForUser_ListErrorPropagates(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	f.svc.signingKeys = &staticSigningKeys{keys: [][]byte{[]byte("primary")}}
	f.store.listErr = errors.New("connection refused")

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrDEKUnavailable,
		"genuine DB errors should NOT be flattened to ErrDEKUnavailable — "+
			"an operator debugging '3am secrets outage' needs to see the "+
			"underlying connection error, not a soft-unlock hint")
}

// TestGetDEKForUser_NoEnumeratorConfiguredIsDEKUnavailable protects
// against the wiring bug where the KeyService is used without a
// SigningKeyEnumerator installed. Must not panic; must surface as
// ErrDEKUnavailable.
func TestGetDEKForUser_NoEnumeratorConfiguredIsDEKUnavailable(t *testing.T) {
	f := newGetDEKForUserFixture(t)
	// f.svc.signingKeys intentionally NOT set.

	f.addSession(t, []byte("some-key"), f.baseTs.Add(-30*time.Minute), f.baseTs.Add(23*time.Hour))

	dek, err := f.svc.GetDEKForUser(context.Background(), f.userID)
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable,
		"missing enumerator (wiring bug) must not panic and must not "+
			"pretend to succeed — surface the same sentinel callers "+
			"already handle")
}

// TestGetDEKForUser_NoStoreConfiguredIsDEKUnavailable covers the
// pre-Epic-56 tests / dev configs that construct a KeyService without
// wiring the JWTSessionStore. Same contract as the enumerator case.
func TestGetDEKForUser_NoStoreConfiguredIsDEKUnavailable(t *testing.T) {
	cache := &fakeDEKCache{data: map[string][]byte{}}
	svc := &KeyService{
		cache: cache,
		// jwtSessions intentionally NOT set.
		signingKeys: &staticSigningKeys{keys: [][]byte{[]byte("primary")}},
	}

	dek, err := svc.GetDEKForUser(context.Background(), "user-1")
	assert.Nil(t, dek)
	assert.ErrorIs(t, err, ErrDEKUnavailable)
}

// fakeDEKCache is a minimal in-memory DEKCache for GetDEKForUser tests.
// The existing package tests use a mock via a testify pattern; kept
// local to avoid coupling to that mock's API surface.
type fakeDEKCache struct {
	data map[string][]byte
}

func (f *fakeDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	cp := make([]byte, len(dek))
	copy(cp, dek)
	f.data[sessionID] = cp
	return nil
}

func (f *fakeDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	v, ok := f.data[sessionID]
	if !ok {
		return nil, nil
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

func (f *fakeDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	delete(f.data, sessionID)
	return nil
}
