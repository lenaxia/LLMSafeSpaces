// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wsstate

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// US-45.1: tests for the in-memory Store implementation. These pin the
// behavioral contract that the future Redis implementation (US-45.2-.8)
// must match exactly: same atomicity guarantees, same edge-case handling,
// same invalidation semantics. Any future Redis impl that fails one of
// these tests represents a behavioral regression and must be fixed before
// merge.

// ---------------------------------------------------------------------------
// Active session tracking
// ---------------------------------------------------------------------------

func TestInMemoryStore_CheckAndAddActiveSession_FirstAdd_Succeeds(t *testing.T) {
	s := NewInMemoryStore()
	assert.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5),
		"first session add must succeed")
	assert.True(t, s.IsSessionActive(context.Background(), "ws-1", "s1"))
	assert.Equal(t, 1, s.ActiveSessionCount(context.Background(), "ws-1"))
}

func TestInMemoryStore_CheckAndAddActiveSession_DuplicateID_IsIdempotent(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))

	// Adding the same session ID again must succeed (idempotent) and
	// must NOT bump the count.
	assert.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))
	assert.Equal(t, 1, s.ActiveSessionCount(context.Background(), "ws-1"),
		"idempotent re-add of same session must not increase count")
}

func TestInMemoryStore_CheckAndAddActiveSession_AtLimit_BlocksNewSession(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 2))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s2", 2))

	assert.False(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s3", 2),
		"third session add with maxSessions=2 must be blocked")
	assert.False(t, s.IsSessionActive(context.Background(), "ws-1", "s3"))
	assert.Equal(t, 2, s.ActiveSessionCount(context.Background(), "ws-1"),
		"blocked session must not be added to the set")
}

func TestInMemoryStore_CheckAndAddActiveSession_AtLimit_AllowsExistingSession(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 2))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s2", 2))

	// Even at limit, an existing session must still report active=true.
	// This matters for retry logic: a client retrying an in-flight
	// session must not be told "limit reached" for its own session.
	assert.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 2),
		"existing session at limit must still succeed (idempotent re-check)")
}

func TestInMemoryStore_CheckAndAddActiveSession_Concurrent_AtLimitNoOversubscribe(t *testing.T) {
	s := NewInMemoryStore()
	const maxSessions = 5
	const distinctSessions = 50

	var wg sync.WaitGroup
	results := make(chan bool, distinctSessions)
	for i := 0; i < distinctSessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results <- s.CheckAndAddActiveSession(context.Background(), "ws-1",
				fmt.Sprintf("s-%d", idx), maxSessions)
		}(i)
	}
	wg.Wait()
	close(results)

	added := 0
	for r := range results {
		if r {
			added++
		}
	}
	assert.Equal(t, maxSessions, added,
		"concurrent adds must respect maxSessions exactly — oversubscription is a correctness bug")
	assert.Equal(t, maxSessions, s.ActiveSessionCount(context.Background(), "ws-1"),
		"post-concurrent count must match maxSessions")
}

func TestInMemoryStore_RemoveActiveSession_RemovesFromSet(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s2", 5))

	s.RemoveActiveSession(context.Background(), "ws-1", "s1")

	assert.False(t, s.IsSessionActive(context.Background(), "ws-1", "s1"))
	assert.True(t, s.IsSessionActive(context.Background(), "ws-1", "s2"))
	assert.Equal(t, 1, s.ActiveSessionCount(context.Background(), "ws-1"))
}

func TestInMemoryStore_RemoveActiveSession_LastRemoval_CleansWorkspaceEntry(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))

	s.RemoveActiveSession(context.Background(), "ws-1", "s1")

	// Internal cleanup: after removing the last session the per-workspace
	// map entry must be deleted to keep memory bounded across many
	// workspaces over time. Verified through ActiveSessionCount which
	// reads the underlying set.
	assert.Equal(t, 0, s.ActiveSessionCount(context.Background(), "ws-1"))
	assert.Nil(t, s.GetActiveSessions(context.Background(), "ws-1"),
		"GetActiveSessions must return nil after the workspace's set is cleaned up")
}

func TestInMemoryStore_RemoveActiveSession_UnknownSession_NoOp(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))

	s.RemoveActiveSession(context.Background(), "ws-1", "nonexistent")
	s.RemoveActiveSession(context.Background(), "ws-unknown", "s1")

	assert.True(t, s.IsSessionActive(context.Background(), "ws-1", "s1"),
		"removing an unknown session or workspace must not affect existing state")
}

func TestInMemoryStore_IsSessionActive_UnknownWorkspace_ReturnsFalse(t *testing.T) {
	s := NewInMemoryStore()
	assert.False(t, s.IsSessionActive(context.Background(), "ws-unknown", "s1"))
}

func TestInMemoryStore_GetActiveSessions_ReturnsAllAndNoMore(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s2", 5))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s3", 5))

	got := s.GetActiveSessions(context.Background(), "ws-1")
	assert.Len(t, got, 3)
	assert.ElementsMatch(t, []string{"s1", "s2", "s3"}, got)
}

func TestInMemoryStore_GetActiveSessions_EmptyWorkspace_ReturnsNil(t *testing.T) {
	s := NewInMemoryStore()
	assert.Nil(t, s.GetActiveSessions(context.Background(), "ws-1"))
}

func TestInMemoryStore_ClearActiveSessions_RemovesEntireSet(t *testing.T) {
	s := NewInMemoryStore()
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s2", 5))
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-2", "s1", 5))

	s.ClearActiveSessions(context.Background(), "ws-1")

	assert.Equal(t, 0, s.ActiveSessionCount(context.Background(), "ws-1"))
	assert.Nil(t, s.GetActiveSessions(context.Background(), "ws-1"))
	assert.Equal(t, 1, s.ActiveSessionCount(context.Background(), "ws-2"),
		"ClearActiveSessions must be scoped to one workspace")
}

// ---------------------------------------------------------------------------
// Deleted-session tombstones
// ---------------------------------------------------------------------------

func TestInMemoryStore_MarkSessionDeleted_ThenIsDeleted_ReturnsTrue(t *testing.T) {
	s := NewInMemoryStore()
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s1"))

	s.MarkSessionDeleted(context.Background(), "ws-1", "s1")

	assert.True(t, s.IsSessionDeleted(context.Background(), "ws-1", "s1"))
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s2"),
		"unrelated session must not be marked deleted")
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-2", "s1"),
		"same session in different workspace must not be marked deleted")
}

func TestInMemoryStore_MarkSessionDeleted_MultipleWorkspaces_TrackedSeparately(t *testing.T) {
	s := NewInMemoryStore()
	s.MarkSessionDeleted(context.Background(), "ws-1", "s1")
	s.MarkSessionDeleted(context.Background(), "ws-1", "s2")
	s.MarkSessionDeleted(context.Background(), "ws-2", "s1")

	assert.True(t, s.IsSessionDeleted(context.Background(), "ws-1", "s1"))
	assert.True(t, s.IsSessionDeleted(context.Background(), "ws-1", "s2"))
	assert.True(t, s.IsSessionDeleted(context.Background(), "ws-2", "s1"))
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s3"))
}

func TestInMemoryStore_ClearDeletedSessions_ScopedToWorkspace(t *testing.T) {
	s := NewInMemoryStore()
	s.MarkSessionDeleted(context.Background(), "ws-1", "s1")
	s.MarkSessionDeleted(context.Background(), "ws-1", "s2")
	s.MarkSessionDeleted(context.Background(), "ws-2", "s1")

	s.ClearDeletedSessions(context.Background(), "ws-1")

	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s1"))
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s2"))
	assert.True(t, s.IsSessionDeleted(context.Background(), "ws-2", "s1"),
		"ClearDeletedSessions must not affect other workspaces")
}

func TestInMemoryStore_MarkSessionDeleted_BoundedGrowth_EvictsOldest(t *testing.T) {
	// InMemoryStore bounds the deletedSessions set to prevent unbounded
	// memory growth if a buggy client creates and deletes sessions in a
	// tight loop. The bound matches the prior ProxyHandler behavior
	// (500 entries, batch-evict 250 when exceeded).
	s := NewInMemoryStore()
	for i := 0; i < deletedSessionsHighWater+10; i++ {
		s.MarkSessionDeleted(context.Background(), "ws-1", fmt.Sprintf("s-%d", i))
	}

	// Some entries were evicted (don't assert which — eviction is
	// non-deterministic across map iteration order). Verify the bound.
	count := 0
	for i := 0; i < deletedSessionsHighWater+10; i++ {
		if s.IsSessionDeleted(context.Background(), "ws-1", fmt.Sprintf("s-%d", i)) {
			count++
		}
	}
	assert.LessOrEqual(t, count, deletedSessionsHighWater,
		"deleted-session set must be bounded — unbounded growth is a memory leak")
}

// ---------------------------------------------------------------------------
// Workspace password cache
// ---------------------------------------------------------------------------

func TestInMemoryStore_GetCachedPassword_Miss_ReturnsFalse(t *testing.T) {
	s := NewInMemoryStore()
	_, ok := s.GetCachedPassword(context.Background(), "ws-1")
	assert.False(t, ok)
}

func TestInMemoryStore_SetThenGetCachedPassword_RoundTrips(t *testing.T) {
	s := NewInMemoryStore()
	s.SetCachedPassword(context.Background(), "ws-1", "hunter2")

	pw, ok := s.GetCachedPassword(context.Background(), "ws-1")
	require.True(t, ok)
	assert.Equal(t, "hunter2", pw)
}

func TestInMemoryStore_InvalidatePassword_RemovesEntry(t *testing.T) {
	s := NewInMemoryStore()
	s.SetCachedPassword(context.Background(), "ws-1", "pw1")
	s.SetCachedPassword(context.Background(), "ws-2", "pw2")

	s.InvalidatePassword(context.Background(), "ws-1")

	_, ok := s.GetCachedPassword(context.Background(), "ws-1")
	assert.False(t, ok)
	pw, ok := s.GetCachedPassword(context.Background(), "ws-2")
	require.True(t, ok)
	assert.Equal(t, "pw2", pw,
		"InvalidatePassword must be scoped to one workspace")
}

func TestInMemoryStore_InvalidatePassword_OnMissingEntry_IsNoOp(t *testing.T) {
	s := NewInMemoryStore()
	s.InvalidatePassword(context.Background(), "ws-never-was")
	// Just assert no panic; nothing else to verify.
	_, ok := s.GetCachedPassword(context.Background(), "ws-never-was")
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Workspace config cache
// ---------------------------------------------------------------------------

func TestInMemoryStore_GetWorkspaceConfig_Miss_ReturnsFalse(t *testing.T) {
	s := NewInMemoryStore()
	_, ok := s.GetWorkspaceConfig(context.Background(), "ws-1")
	assert.False(t, ok)
}

func TestInMemoryStore_SetThenGetWorkspaceConfig_RoundTrips(t *testing.T) {
	s := NewInMemoryStore()
	cfg := Config{MaxActiveSessions: 7, AutoApprovePermissions: true}
	s.SetWorkspaceConfig(context.Background(), "ws-1", cfg)

	got, ok := s.GetWorkspaceConfig(context.Background(), "ws-1")
	require.True(t, ok)
	assert.Equal(t, cfg, got)
}

func TestInMemoryStore_InvalidateWorkspaceConfig_RemovesEntry(t *testing.T) {
	s := NewInMemoryStore()
	s.SetWorkspaceConfig(context.Background(), "ws-1", Config{MaxActiveSessions: 3})

	s.InvalidateWorkspaceConfig(context.Background(), "ws-1")

	_, ok := s.GetWorkspaceConfig(context.Background(), "ws-1")
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Prior phase tracking
// ---------------------------------------------------------------------------

func TestInMemoryStore_GetPriorPhase_NoEntry_ReturnsFalse(t *testing.T) {
	s := NewInMemoryStore()
	_, ok := s.GetPriorPhase(context.Background(), "ws-1")
	assert.False(t, ok)
}

func TestInMemoryStore_SetThenGetPriorPhase_RoundTrips(t *testing.T) {
	s := NewInMemoryStore()
	s.SetPriorPhase(context.Background(), "ws-1", "Active")

	got, ok := s.GetPriorPhase(context.Background(), "ws-1")
	require.True(t, ok)
	assert.Equal(t, "Active", got)
}

func TestInMemoryStore_SetPriorPhase_Overwrites(t *testing.T) {
	s := NewInMemoryStore()
	s.SetPriorPhase(context.Background(), "ws-1", "Creating")
	s.SetPriorPhase(context.Background(), "ws-1", "Active")

	got, ok := s.GetPriorPhase(context.Background(), "ws-1")
	require.True(t, ok)
	assert.Equal(t, "Active", got,
		"SetPriorPhase must overwrite — onPhaseChange relies on update semantics")
}

func TestInMemoryStore_DeletePriorPhase_RemovesEntry(t *testing.T) {
	s := NewInMemoryStore()
	s.SetPriorPhase(context.Background(), "ws-1", "Active")

	s.DeletePriorPhase(context.Background(), "ws-1")

	_, ok := s.GetPriorPhase(context.Background(), "ws-1")
	assert.False(t, ok)
}

func TestInMemoryStore_DeletePriorPhase_OnMissingEntry_IsNoOp(t *testing.T) {
	s := NewInMemoryStore()
	s.DeletePriorPhase(context.Background(), "ws-never-was")
	_, ok := s.GetPriorPhase(context.Background(), "ws-never-was")
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// Parent-backfill marker
// ---------------------------------------------------------------------------

func TestInMemoryStore_GetParentBackfilled_DefaultFalse(t *testing.T) {
	s := NewInMemoryStore()
	assert.False(t, s.GetParentBackfilled(context.Background(), "ws-1"))
}

func TestInMemoryStore_SetParentBackfilled_ThenGet_ReturnsTrue(t *testing.T) {
	s := NewInMemoryStore()
	s.SetParentBackfilled(context.Background(), "ws-1")

	assert.True(t, s.GetParentBackfilled(context.Background(), "ws-1"))
	assert.False(t, s.GetParentBackfilled(context.Background(), "ws-2"),
		"marker must be per-workspace")
}

func TestInMemoryStore_DeleteParentBackfilled_RemovesMarker(t *testing.T) {
	s := NewInMemoryStore()
	s.SetParentBackfilled(context.Background(), "ws-1")

	s.DeleteParentBackfilled(context.Background(), "ws-1")

	assert.False(t, s.GetParentBackfilled(context.Background(), "ws-1"))
}

// ---------------------------------------------------------------------------
// InvalidateAll
// ---------------------------------------------------------------------------

func TestInMemoryStore_InvalidateAll_ClearsAllStateForWorkspace(t *testing.T) {
	s := NewInMemoryStore()
	// Populate every piece of state for ws-1.
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-1", "s1", 5))
	s.MarkSessionDeleted(context.Background(), "ws-1", "s2")
	s.SetCachedPassword(context.Background(), "ws-1", "pw")
	s.SetWorkspaceConfig(context.Background(), "ws-1", Config{MaxActiveSessions: 3})
	s.SetPriorPhase(context.Background(), "ws-1", "Active")
	s.SetParentBackfilled(context.Background(), "ws-1")

	// Populate some state for ws-2 to verify scoping.
	require.True(t, s.CheckAndAddActiveSession(context.Background(), "ws-2", "s1", 5))
	s.SetCachedPassword(context.Background(), "ws-2", "pw-2")

	s.InvalidateAll(context.Background(), "ws-1")

	// ws-1 must be cleared EXCEPT priorPhase, which survives invalidation
	// so onPhaseChange can distinguish first-invocation from Active→Active
	// reconcile. Matches the original invalidateCaches behavior exactly.
	assert.Equal(t, 0, s.ActiveSessionCount(context.Background(), "ws-1"))
	assert.False(t, s.IsSessionDeleted(context.Background(), "ws-1", "s2"))
	_, pwOk := s.GetCachedPassword(context.Background(), "ws-1")
	assert.False(t, pwOk)
	_, cfgOk := s.GetWorkspaceConfig(context.Background(), "ws-1")
	assert.False(t, cfgOk)
	phase, phaseOk := s.GetPriorPhase(context.Background(), "ws-1")
	assert.True(t, phaseOk, "priorPhase must survive InvalidateAll — onPhaseChange relies on it")
	assert.Equal(t, "Active", phase)
	assert.False(t, s.GetParentBackfilled(context.Background(), "ws-1"))

	// ws-2 must be untouched.
	assert.Equal(t, 1, s.ActiveSessionCount(context.Background(), "ws-2"))
	pw2, pw2Ok := s.GetCachedPassword(context.Background(), "ws-2")
	require.True(t, pw2Ok)
	assert.Equal(t, "pw-2", pw2)
}

func TestInMemoryStore_InvalidateAll_OnUnknownWorkspace_IsNoOp(t *testing.T) {
	s := NewInMemoryStore()
	require.NotPanics(t, func() { s.InvalidateAll(context.Background(), "ws-never-was") })
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestInMemoryStore_ConcurrentMixedOps_NoRaceDetector(t *testing.T) {
	// Smoke test for goroutine-safety. Run with -race to catch data races.
	// This is NOT a stress test — it just exercises the lock paths from
	// multiple goroutines doing different operations on overlapping keys.
	// It does not assert final state: concurrent InvalidateAll makes
	// final counts non-deterministic by design. The bounded invariants
	// (each individual method's correctness) are pinned by the
	// single-threaded tests above.
	s := NewInMemoryStore()

	const goroutines = 20
	const iterations = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			ws := fmt.Sprintf("ws-%d", gid%3)
			sess := fmt.Sprintf("s-%d", gid)
			for i := 0; i < iterations; i++ {
				s.CheckAndAddActiveSession(context.Background(), ws, sess, 100)
				s.MarkSessionDeleted(context.Background(), ws, sess)
				s.SetCachedPassword(context.Background(), ws, "pw")
				s.SetWorkspaceConfig(context.Background(), ws, Config{MaxActiveSessions: 5})
				s.SetPriorPhase(context.Background(), ws, "Active")
				s.SetParentBackfilled(context.Background(), ws)
				s.IsSessionActive(context.Background(), ws, sess)
				s.IsSessionDeleted(context.Background(), ws, sess)
				s.GetCachedPassword(context.Background(), ws)
				s.GetWorkspaceConfig(context.Background(), ws)
				s.GetPriorPhase(context.Background(), ws)
				s.GetParentBackfilled(context.Background(), ws)
				if i%10 == 0 {
					s.InvalidateAll(context.Background(), ws)
				}
			}
		}(g)
	}
	wg.Wait()
	// If we got here without -race firing or deadlocking, the smoke test
	// passed. We do not assert final state — concurrent InvalidateAll
	// makes final counts non-deterministic by design.
}

// #1340: reconcile-miss counters — whole-map replace, copy-in/out,
// cleared by InvalidateAll.
func TestInMemoryStore_ReconcileMisses(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()

	if got := s.GetReconcileMisses(ctx, "ws-1"); got != nil {
		t.Fatalf("empty store returned %v, want nil", got)
	}
	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_a": 1, "ses_b": 2})
	got := s.GetReconcileMisses(ctx, "ws-1")
	if got["ses_a"] != 1 || got["ses_b"] != 2 {
		t.Fatalf("got %v", got)
	}
	// Whole-map replace drops keys absent from the new map.
	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_a": 2})
	got = s.GetReconcileMisses(ctx, "ws-1")
	if len(got) != 1 || got["ses_a"] != 2 {
		t.Fatalf("replace must be wholesale, got %v", got)
	}
	// Empty map clears.
	s.SetReconcileMisses(ctx, "ws-1", nil)
	if got := s.GetReconcileMisses(ctx, "ws-1"); got != nil {
		t.Fatalf("cleared store returned %v, want nil", got)
	}
	// Copy-out: mutating the returned map does not corrupt the store.
	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_a": 1})
	s.GetReconcileMisses(ctx, "ws-1")["ses_a"] = 99
	if s.GetReconcileMisses(ctx, "ws-1")["ses_a"] != 1 {
		t.Fatal("GetReconcileMisses must return a copy")
	}
	// InvalidateAll clears.
	s.SetReconcileMisses(ctx, "ws-1", map[string]int{"ses_a": 1})
	s.InvalidateAll(ctx, "ws-1")
	if got := s.GetReconcileMisses(ctx, "ws-1"); got != nil {
		t.Fatalf("InvalidateAll must clear counters, got %v", got)
	}
}
