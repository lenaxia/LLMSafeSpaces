// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mockJWTSessionStore is the in-memory JWTSessionStore used by unit
// tests in this package. Tracks call counts and supports an injected
// error per-operation so tests can exercise failure paths without a
// real Postgres. Concurrency-safe.
type mockJWTSessionStore struct {
	mu          sync.Mutex
	rows        map[uuid.UUID]*JWTSession
	deleteErr   error
	deleteForUE error
	expireErr   error
	listErr     error

	// now overrides time.Now for ListActiveJWTSessionsForUser boundary
	// tests. Zero → uses time.Now.
	now time.Time

	// Call counters
	DeleteCount      int
	DeleteForUserCnt int
	ExpireCount      int
	ListCount        int
}

func newMockJWTSessionStore() *mockJWTSessionStore {
	return &mockJWTSessionStore{rows: make(map[uuid.UUID]*JWTSession)}
}

// seed inserts a row directly — tests construct pre-existing
// (pre-US-70.5) state; no production writer remains.
func (m *mockJWTSessionStore) seed(s *JWTSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	m.rows[s.JTI] = &cp
}

func (m *mockJWTSessionStore) has(jti uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.rows[jti]
	return ok
}

func (m *mockJWTSessionStore) DeleteJWTSession(_ context.Context, jti uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteCount++
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.rows, jti)
	return nil
}

func (m *mockJWTSessionStore) DeleteJWTSessionsForUser(_ context.Context, userID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DeleteForUserCnt++
	if m.deleteForUE != nil {
		return 0, m.deleteForUE
	}
	var n int64
	for jti, row := range m.rows {
		if row.UserID == userID {
			delete(m.rows, jti)
			n++
		}
	}
	return n, nil
}

func (m *mockJWTSessionStore) DeleteExpiredJWTSessions(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ExpireCount++
	if m.expireErr != nil {
		return 0, m.expireErr
	}
	var n int64
	for jti, row := range m.rows {
		if row.ExpiresAt.Before(before) {
			delete(m.rows, jti)
			n++
		}
	}
	return n, nil
}

// ListActiveJWTSessionsForUser satisfies JWTSessionStore. Semantics:
// return all rows for userID whose expires_at is strictly AFTER
// the store's clock (or time.Now() if the test hasn't overridden it).
// Ordered created_at DESC. Bounded by limit; limit<=0 means unbounded.
func (m *mockJWTSessionStore) ListActiveJWTSessionsForUser(_ context.Context, userID string, limit int) ([]*JWTSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListCount++
	if m.listErr != nil {
		return nil, m.listErr
	}
	nowT := m.now
	if nowT.IsZero() {
		nowT = time.Now()
	}
	out := make([]*JWTSession, 0)
	for _, row := range m.rows {
		if row.UserID != userID {
			continue
		}
		if !row.ExpiresAt.After(nowT) {
			continue
		}
		cp := *row
		out = append(out, &cp)
	}
	// Sort by created_at DESC.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- Tests ---

func TestMockJWTSessionStore_DeleteJWTSession(t *testing.T) {
	store := newMockJWTSessionStore()
	jti := uuid.New()
	store.seed(&JWTSession{JTI: jti, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: time.Now().Add(time.Hour)})

	if err := store.DeleteJWTSession(context.Background(), jti); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if store.has(jti) {
		t.Errorf("expected row deleted, still present")
	}
}

func TestMockJWTSessionStore_DeleteJWTSession_NonexistentIsIdempotent(t *testing.T) {
	store := newMockJWTSessionStore()
	if err := store.DeleteJWTSession(context.Background(), uuid.New()); err != nil {
		t.Errorf("delete of missing row should not error: %v", err)
	}
}

func TestMockJWTSessionStore_DeleteJWTSessionsForUser(t *testing.T) {
	store := newMockJWTSessionStore()
	now := time.Now()
	keep := uuid.New()
	drop1 := uuid.New()
	drop2 := uuid.New()
	store.seed(&JWTSession{JTI: keep, UserID: "u-other", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(time.Hour)})
	store.seed(&JWTSession{JTI: drop1, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(time.Hour)})
	store.seed(&JWTSession{JTI: drop2, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(time.Hour)})

	n, err := store.DeleteJWTSessionsForUser(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("delete-for-user: %v", err)
	}
	if n != 2 {
		t.Errorf("rows affected = %d, want 2", n)
	}
	// Other user's row preserved
	if !store.has(keep) {
		t.Errorf("expected other user's row preserved")
	}
}

func TestMockJWTSessionStore_DeleteExpiredJWTSessions(t *testing.T) {
	store := newMockJWTSessionStore()
	now := time.Now()
	expired := uuid.New()
	active := uuid.New()
	store.seed(&JWTSession{JTI: expired, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(-time.Hour)})
	store.seed(&JWTSession{JTI: active, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(time.Hour)})

	n, err := store.DeleteExpiredJWTSessions(context.Background(), now)
	if err != nil {
		t.Fatalf("delete-expired: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d, want 1", n)
	}
	if store.has(expired) {
		t.Errorf("expected expired row removed")
	}
	if !store.has(active) {
		t.Errorf("expected active row preserved")
	}
}

func TestMockJWTSessionStore_PropagatesInjectedErrors(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*mockJWTSessionStore)
		run   func(context.Context, *mockJWTSessionStore) error
	}{
		{
			name:  "Delete error",
			setup: func(m *mockJWTSessionStore) { m.deleteErr = errors.New("delete fail") },
			run: func(ctx context.Context, m *mockJWTSessionStore) error {
				return m.DeleteJWTSession(ctx, uuid.New())
			},
		},
		{
			name:  "DeleteForUser error",
			setup: func(m *mockJWTSessionStore) { m.deleteForUE = errors.New("delete-for-user fail") },
			run: func(ctx context.Context, m *mockJWTSessionStore) error {
				_, err := m.DeleteJWTSessionsForUser(ctx, "u-1")
				return err
			},
		},
		{
			name:  "DeleteExpired error",
			setup: func(m *mockJWTSessionStore) { m.expireErr = errors.New("expire fail") },
			run: func(ctx context.Context, m *mockJWTSessionStore) error {
				_, err := m.DeleteExpiredJWTSessions(ctx, time.Now())
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockJWTSessionStore()
			tc.setup(store)
			if err := tc.run(context.Background(), store); err == nil {
				t.Errorf("expected injected error to surface")
			}
		})
	}
}

// --- ListActiveJWTSessionsForUser tests ---
//
// Contract:
// - Returns non-expired rows for userID.
// - Ordered created_at DESC (most-recent first).
// - Bounded by limit (0 = unlimited-per-caller-convention).
// - Empty (nil or []) for unknown userID.
// - Returns injected error verbatim.

// TestListActive_ExcludesExpired proves the boundary condition: a row
// exactly at expires_at MUST be treated as expired (SQL semantics use
// expires_at > NOW(), strict inequality). Without this, a session
// that expired at the exact microsecond we query would be returned,
// and the caller would consult a cache entry the janitor-bound expiry
// has already invalidated.
func TestListActive_ExcludesExpired(t *testing.T) {
	store := newMockJWTSessionStore()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store.now = base

	active := &JWTSession{
		JTI:        uuid.New(),
		UserID:     "user-1",
		WrappedDEK: []byte{1},
		KEKSalt:    []byte{2},
		CreatedAt:  base.Add(-1 * time.Hour),
		ExpiresAt:  base.Add(1 * time.Hour),
	}
	expiredBoundary := &JWTSession{
		JTI:        uuid.New(),
		UserID:     "user-1",
		WrappedDEK: []byte{3},
		KEKSalt:    []byte{4},
		CreatedAt:  base.Add(-2 * time.Hour),
		ExpiresAt:  base, // exact-tick expiration
	}
	expiredPast := &JWTSession{
		JTI:        uuid.New(),
		UserID:     "user-1",
		WrappedDEK: []byte{5},
		KEKSalt:    []byte{6},
		CreatedAt:  base.Add(-3 * time.Hour),
		ExpiresAt:  base.Add(-1 * time.Minute),
	}
	for _, s := range []*JWTSession{active, expiredBoundary, expiredPast} {
		store.seed(s)
	}

	got, err := store.ListActiveJWTSessionsForUser(context.Background(), "user-1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 active row (boundary is expired); got %d", len(got))
	}
	if got[0].JTI != active.JTI {
		t.Errorf("wrong row returned")
	}
}

// TestListActive_OrdersMostRecentFirst locks in the ORDER BY created_at
// DESC semantics. GetCachedDEKForUser consults the first row's cache
// entry first; the newest session is the most likely to hold a warm
// cache entry.
func TestListActive_OrdersMostRecentFirst(t *testing.T) {
	store := newMockJWTSessionStore()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store.now = base

	oldest := &JWTSession{
		JTI: uuid.New(), UserID: "user-1",
		WrappedDEK: []byte{1}, KEKSalt: []byte{2},
		CreatedAt: base.Add(-3 * time.Hour), ExpiresAt: base.Add(1 * time.Hour),
	}
	middle := &JWTSession{
		JTI: uuid.New(), UserID: "user-1",
		WrappedDEK: []byte{3}, KEKSalt: []byte{4},
		CreatedAt: base.Add(-2 * time.Hour), ExpiresAt: base.Add(1 * time.Hour),
	}
	newest := &JWTSession{
		JTI: uuid.New(), UserID: "user-1",
		WrappedDEK: []byte{5}, KEKSalt: []byte{6},
		CreatedAt: base.Add(-1 * time.Hour), ExpiresAt: base.Add(1 * time.Hour),
	}
	for _, s := range []*JWTSession{middle, oldest, newest} { // insert out of order
		store.seed(s)
	}

	got, err := store.ListActiveJWTSessionsForUser(context.Background(), "user-1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].JTI != newest.JTI || got[1].JTI != middle.JTI || got[2].JTI != oldest.JTI {
		t.Errorf("order wrong: [%v %v %v]", got[0].JTI, got[1].JTI, got[2].JTI)
	}
}

// TestListActive_RespectsLimit verifies that limit caps the result
// after sorting so we consistently get "the most recent N."
func TestListActive_RespectsLimit(t *testing.T) {
	store := newMockJWTSessionStore()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store.now = base

	// 5 rows, ages 1..5 hours old.
	all := make([]*JWTSession, 5)
	for i := 0; i < 5; i++ {
		all[i] = &JWTSession{
			JTI: uuid.New(), UserID: "user-1",
			WrappedDEK: []byte{byte(i)}, KEKSalt: []byte{byte(i + 100)},
			CreatedAt: base.Add(-time.Duration(i+1) * time.Hour),
			ExpiresAt: base.Add(1 * time.Hour),
		}
		store.seed(all[i])
	}

	got, err := store.ListActiveJWTSessionsForUser(context.Background(), "user-1", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 (limited); got %d", len(got))
	}
	// The 2 most-recent are index 0 and 1 (1-hour and 2-hour old).
	if !bytes.Equal(got[0].WrappedDEK, []byte{0}) || !bytes.Equal(got[1].WrappedDEK, []byte{1}) {
		t.Errorf("limit did not preserve most-recent-first")
	}
}

// TestListActive_UnknownUserReturnsEmpty ensures the "no rows" case is
// nil-error, not an error. Callers use empty to signal "no live
// session" (ErrDEKUnavailable).
func TestListActive_UnknownUserReturnsEmpty(t *testing.T) {
	store := newMockJWTSessionStore()
	got, err := store.ListActiveJWTSessionsForUser(context.Background(), "no-such-user", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty for unknown user; got %d rows", len(got))
	}
}

// TestListActive_ScopedToUser proves the WHERE user_id = ? predicate.
// Rows for other users must NOT be visible.
func TestListActive_ScopedToUser(t *testing.T) {
	store := newMockJWTSessionStore()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store.now = base

	mine := &JWTSession{
		JTI: uuid.New(), UserID: "user-1",
		WrappedDEK: []byte{1}, KEKSalt: []byte{2},
		CreatedAt: base.Add(-1 * time.Hour), ExpiresAt: base.Add(1 * time.Hour),
	}
	other := &JWTSession{
		JTI: uuid.New(), UserID: "user-2",
		WrappedDEK: []byte{3}, KEKSalt: []byte{4},
		CreatedAt: base.Add(-1 * time.Hour), ExpiresAt: base.Add(1 * time.Hour),
	}
	for _, s := range []*JWTSession{mine, other} {
		store.seed(s)
	}

	got, err := store.ListActiveJWTSessionsForUser(context.Background(), "user-1", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "user-1" {
		t.Errorf("cross-user leak: got %+v", got)
	}
}

// TestListActive_PropagatesInjectedError proves errors bubble up
// without translation, per the JWTSessionStore doc contract.
func TestListActive_PropagatesInjectedError(t *testing.T) {
	store := newMockJWTSessionStore()
	store.listErr = errors.New("boom")
	_, err := store.ListActiveJWTSessionsForUser(context.Background(), "user-1", 0)
	if err == nil || err.Error() != "boom" {
		t.Errorf("expected injected error to surface, got %v", err)
	}
}
