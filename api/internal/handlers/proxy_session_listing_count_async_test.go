// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Listing-latency work — the async count-walk redesign of the #1880
// serve-through plus the walk-failure retirement:
//
//   A (async walks): the listing request selects drift candidates and
//   DISPATCHES the harness count walks; the walks run on a detached
//   context after the response is served, persisting through the same
//   rebuild seam. The wire serves the index value immediately; the
//   count converges in the index for the next poll. The request path
//   never waits on a pod round trip.
//
//   B (retirement): a row whose walk fails with the definitive
//   session-not-found verdict (agent.ErrSessionNotFound — the
//   httpSessionError 404 classifier) is a confirmed-absent miss; after
//   countServeRetireAfterMisses consecutive misses the row stops
//   consuming walk budget for countServeRetireWindow (it re-arms after
//   expiry, and a successful walk resets the streak). Transient
//   failures (pod not running, 5xx, timeouts) NEVER count toward
//   retirement: a suspended pod's PVC still holds the session, and a
//   404 from a running pod says nothing about a DOWN pod's sessions.
//   The row itself is never deleted here — deletion stays with the
//   operator-reviewed reap path.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// asyncCountTestHandler is the async-safe twin of
// listingCountTestHandler: the walk spy is mutex-guarded (the default
// dispatch runs the walks on a goroutine) and can sleep per walk.
type asyncCountTestHandler struct {
	h        *ProxyHandler
	idx      *mockSessionIndex
	mu       sync.Mutex
	counted  []string
	counts   map[string]int
	walkErrs map[string]error
	walkNap  time.Duration
}

func newAsyncCountTestHandler(t *testing.T, counts map[string]int, walkErrs map[string]error) *asyncCountTestHandler {
	f := &asyncCountTestHandler{counts: counts, walkErrs: walkErrs}
	f.idx = newMockSessionIndex()
	f.h = newProxyHandlerForAdapterTest(t)
	f.h.sessionIndex = f.idx
	f.h.adapter = &mockAdapter{
		countMessagesFn: func(_ context.Context, _, _, sessionID string) (int, error) {
			if f.walkNap > 0 {
				time.Sleep(f.walkNap)
			}
			f.mu.Lock()
			f.counted = append(f.counted, sessionID)
			f.mu.Unlock()
			if err, ok := f.walkErrs[sessionID]; ok {
				return 0, err
			}
			return f.counts[sessionID], nil
		},
	}
	return f
}

func (f *asyncCountTestHandler) walked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.counted...)
}

func (f *asyncCountTestHandler) clearWalkErr(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.walkErrs, sessionID)
}

// syncDispatch pins the dispatch seam to run inline — every
// deterministic ordering/retirement assertion uses this.
func syncDispatch(h *ProxyHandler) {
	h.SetCountWalkDispatchForTest(func(fn func()) { fn() })
}

// TestListingCount_WalksRunOffTheRequestPath — the headline A pin: a
// listing whose walks are slow must NOT wait for them. The walk costs
// 200ms per row; the call must return well inside that (the wire gets
// the index value), and the walk must still converge in the index
// afterward (the detached pass persists through the rebuild seam).
func TestListingCount_WalksRunOffTheRequestPath(t *testing.T) {
	f := newAsyncCountTestHandler(t, map[string]int{"ses_slow": 389}, nil)
	f.walkNap = 500 * time.Millisecond
	sessions := []types.SessionListItem{driftRow("ses_slow", 2*time.Hour)}

	start := time.Now()
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 100*time.Millisecond,
		"the listing must not wait on the harness count walks — they run detached")

	require.Eventually(t, func() bool {
		f.idx.mu.Lock()
		defer f.idx.mu.Unlock()
		return f.idx.rebuiltCounts["ws-1/ses_slow"] == 389
	}, 2*time.Second, 10*time.Millisecond,
		"the detached walk persists the walked truth through the rebuild seam")
	require.Equal(t, []string{"ses_slow"}, f.walked())
	assert.Equal(t, 0, sessions[0].MessageCount,
		"the wire serves the index value on this pass — the correction lands in the index for the next poll")
}

// TestListingCount_InflightDedupeSkipsRow — a row already being walked
// (an earlier pass's detached goroutine) is not re-dispatched by the
// next listing: the outcome is counted and the budget is not spent.
func TestListingCount_InflightDedupeSkipsRow(t *testing.T) {
	f := newAsyncCountTestHandler(t, map[string]int{"ses_busy": 5}, nil)
	syncDispatch(f.h)
	key := "ws-1/ses_busy"
	require.True(t, f.h.countServeState().tryMarkInflight(key),
		"sanity: the first mark takes")

	inflightBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_inflight"))
	f.h.ScheduleHarnessMessageCounts("ws-1",
		[]types.SessionListItem{driftRow("ses_busy", time.Hour)})
	assert.Equal(t, inflightBefore+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_inflight")))
	assert.Empty(t, f.walked(),
		"an in-flight row is not re-walked by a concurrent pass")

	f.h.countServeState().clearInflight(key)
	f.h.ScheduleHarnessMessageCounts("ws-1",
		[]types.SessionListItem{driftRow("ses_busy", time.Hour)})
	require.Equal(t, []string{"ses_busy"}, f.walked(),
		"cleared in-flight state re-arms the walk")
	require.Equal(t, map[string]int{"ws-1/ses_busy": 5}, f.idx.rebuiltCounts)
}

// TestListingCount_InflightRowDoesNotDisplaceBudget — the review r1
// docs-truth pin: the inflight claim runs IN SELECTION, before the
// budget cut, so an in-flight row never consumes one of the pass's
// three slots. Four eligible rows, budget 3, one pre-inflight: the
// pass walks THREE non-inflight rows (the inflight row displaces
// nobody; the cut falls on the fourth never-walked row).
func TestListingCount_InflightRowDoesNotDisplaceBudget(t *testing.T) {
	f := newAsyncCountTestHandler(t,
		map[string]int{"ses_busy": 1, "ses_a": 1, "ses_b": 1, "ses_c": 1, "ses_d": 1}, nil)
	syncDispatch(f.h)
	require.True(t, f.h.countServeState().tryMarkInflight("ws-1/ses_busy"),
		"sanity: the earlier pass holds ses_busy")
	sessions := []types.SessionListItem{
		driftRow("ses_busy", time.Hour),
		driftRow("ses_a", time.Hour), driftRow("ses_b", time.Hour),
		driftRow("ses_c", time.Hour), driftRow("ses_d", time.Hour),
	}

	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)

	assert.ElementsMatch(t, []string{"ses_a", "ses_b", "ses_c"}, f.walked(),
		"the inflight row spends no budget — three walkable rows walk")
}

// TestCountServeTracker_CapResetBoundsRetirementMaps — the review r1
// robustness pin: the tracker's maps are bounded by countServeCap
// wholeness resets, and the MISS map carries its own — ghost rows
// fail walks forever (they never reach record()), so the
// rotation-reset trigger alone would never bound this population.
// Lifetime-distinct drift rows must not grow the tracker without
// bound (the map's own documented invariant).
func TestCountServeTracker_CapResetBoundsRetirementMaps(t *testing.T) {
	tr := &countServeTracker{}
	base := time.Now()

	for i := 0; i < countServeCap+50; i++ {
		key := fmt.Sprintf("ws-1/ses_%05d", i)
		neverRetired := tr.recordMiss(key, base)
		require.False(t, neverRetired,
			"one miss per key never retires (streak 1 < 3)")
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.Less(t, len(tr.misses), countServeCap,
		"the miss map's own cap reset bounded it — cap+50 distinct keys cannot exceed the cap")
	require.LessOrEqual(t, len(tr.retiredAt), len(tr.misses),
		"retirement stamps reset with the miss map (rotation state)")
}

// TestCountServeTracker_SuccessWalkResetAlsoBounds — record()'s
// rotation reset (lastAt-triggered) clears the miss and retirement
// maps with it: the two reset triggers are independent, and both
// leave the tracker bounded.
func TestCountServeTracker_SuccessWalkResetAlsoBounds(t *testing.T) {
	tr := &countServeTracker{}
	base := time.Now()

	for i := 0; i < countServeRetireAfterMisses; i++ {
		retired := tr.recordMiss("ws-1/ses_ghost", base)
		require.Equal(t, i == countServeRetireAfterMisses-1, retired,
			"the streak retires exactly on the third miss")
	}
	tr.mu.Lock()
	require.Len(t, tr.retiredAt, 1, "sanity: the streak retired the row")
	tr.mu.Unlock()

	for i := 0; i < countServeCap+1; i++ {
		tr.record(fmt.Sprintf("ws-1/ses_live_%04d", i), i, base.Add(time.Duration(i)*time.Second))
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.LessOrEqual(t, len(tr.lastAt), countServeCap,
		"the rotation reset bounded lastAt")
	require.LessOrEqual(t, len(tr.misses), 1,
		"record()'s reset cleared the miss map; only the post-reset key remains")
	require.Empty(t, tr.retiredAt,
		"record()'s reset cleared the retirement stamps — bounded again")
}

// TestListingCount_RetiresAfterRepeatedConfirmedMisses — the B pin:
// three consecutive confirmed-absent walks (the ErrSessionNotFound 404
// classifier) retire the row from walk candidacy. The next listing
// neither walks it nor burns budget on it; the retirement outcome is
// observable. The ROW stays on the wire (index value) — retirement is
// about walk budget, never deletion.
func TestListingCount_RetiresAfterRepeatedConfirmedMisses(t *testing.T) {
	walkErrs := map[string]error{"ses_gone": agent.ErrSessionNotFound}
	f := newAsyncCountTestHandler(t, nil, walkErrs)
	syncDispatch(f.h)
	sessions := []types.SessionListItem{driftRow("ses_gone", 2*time.Hour)}

	missBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss"))
	for i := 0; i < countServeRetireAfterMisses; i++ {
		f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	}
	require.Len(t, f.walked(), countServeRetireAfterMisses,
		"each pass walks the row until the miss streak retires it")
	require.Equal(t, missBefore+countServeRetireAfterMisses,
		promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss")))

	retiredBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_retired"))
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	assert.Empty(t, f.walked()[countServeRetireAfterMisses:],
		"past the streak the row is retired — no further walks")
	assert.Equal(t, retiredBefore+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_retired")),
		"the retirement outcome is observable, not a silent skip")
}

// TestListingCount_TransientMissesNeverRetire — the failure-class
// fence: pod-not-running (and every other non-404 error) is NOT a
// confirmed absence. A suspended pod's PVC still holds the session;
// the walk must keep re-arming forever, never retiring.
func TestListingCount_TransientMissesNeverRetire(t *testing.T) {
	walkErrs := map[string]error{"ses_suspended": agent.ErrNoRunningPod}
	f := newAsyncCountTestHandler(t, nil, walkErrs)
	syncDispatch(f.h)
	sessions := []types.SessionListItem{driftRow("ses_suspended", 2*time.Hour)}

	errBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error"))
	missBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss"))
	retiredBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_retired"))

	for i := 0; i < countServeRetireAfterMisses+2; i++ {
		f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	}

	require.Len(t, f.walked(), countServeRetireAfterMisses+2,
		"transient failures re-arm every pass — the row is never retired on them")
	assert.Equal(t, errBefore+countServeRetireAfterMisses+2,
		promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error")))
	assert.Equal(t, missBefore, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss")))
	assert.Equal(t, retiredBefore, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_retired")))
}

// TestListingCount_RetirementWindowExpires — retirement is a window,
// not a tombstone: past countServeRetireWindow the row re-arms (the
// session may have reappeared — harness reset, snapshot restore).
func TestListingCount_RetirementWindowExpires(t *testing.T) {
	walkErrs := map[string]error{"ses_gone": agent.ErrSessionNotFound}
	f := newAsyncCountTestHandler(t, nil, walkErrs)
	syncDispatch(f.h)
	sessions := []types.SessionListItem{driftRow("ses_gone", 2*time.Hour)}

	for i := 0; i < countServeRetireAfterMisses; i++ {
		f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	}
	require.Len(t, f.walked(), countServeRetireAfterMisses)

	tr := f.h.countServeState()
	key := "ws-1/ses_gone"
	tr.mu.Lock()
	stamp, ok := tr.retiredAt[key]
	require.True(t, ok, "the streak retired the row (stamp recorded)")
	tr.retiredAt[key] = stamp.Add(-2 * countServeRetireWindow)
	tr.mu.Unlock()

	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	assert.Len(t, f.walked(), countServeRetireAfterMisses+1,
		"past the window the row re-arms — retirement is never permanent")
}

// TestListingCount_SuccessfulWalkResetsMissStreak — the streak is
// CONSECUTIVE confirmed misses: one successful walk between miss runs
// restarts the count (a flapping session must not accumulate a
// retirement across unrelated failures).
func TestListingCount_SuccessfulWalkResetsMissStreak(t *testing.T) {
	walkErrs := map[string]error{"ses_flap": agent.ErrSessionNotFound}
	counts := map[string]int{"ses_flap": 3}
	f := newAsyncCountTestHandler(t, counts, walkErrs)
	syncDispatch(f.h)
	sessions := []types.SessionListItem{driftRow("ses_flap", 2*time.Hour)}

	// Two misses, then a healed walk, then three fresh misses: the
	// streak restarts at the success, so the retirement fires on the
	// SIXTH walk overall (2 misses + 1 success + the 3rd post-success
	// miss), never earlier.
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	require.Len(t, f.walked(), 2)

	f.clearWalkErr("ses_flap")
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	require.Len(t, f.walked(), 3)
	require.Equal(t, map[string]int{"ws-1/ses_flap": 3}, f.idx.rebuiltCounts,
		"the healed walk persists")

	f.mu.Lock()
	f.walkErrs["ses_flap"] = agent.ErrSessionNotFound
	f.mu.Unlock()
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	require.Len(t, f.walked(), 5,
		"the post-success streak is fresh — two misses do not retire")

	// The THIRD post-success miss IS the retirement walk: it runs
	// (its failure is what completes the streak), and the NEXT pass
	// finds the row retired.
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	require.Len(t, f.walked(), 6,
		"the third post-success miss walks — its confirmed absence retires the row")

	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)
	assert.Len(t, f.walked(), 6,
		"past the streak the row is retired — no further walks")
}

// TestListingCount_MissClassificationIsTyped — the failure-class
// discriminator is errors.Is against the adapter's sentinels, never
// string matching: the definitive 404 verdict classifies as a miss,
// everything else (HTTP-status errors, resolve failures) classifies as
// a transient walk error.
func TestListingCount_MissClassificationIsTyped(t *testing.T) {
	notFound := fmt.Errorf("agent http status: %w: GET /session/x/message returned 404", agent.ErrSessionNotFound)
	transient := fmt.Errorf("GET /session/x/message: %w", agent.ErrHTTPStatus)

	f := newAsyncCountTestHandler(t, nil, map[string]error{"ses_nf": notFound, "ses_tr": transient})
	syncDispatch(f.h)
	sessions := []types.SessionListItem{driftRow("ses_nf", time.Hour), driftRow("ses_tr", time.Hour)}

	missBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss"))
	errBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error"))
	f.h.ScheduleHarnessMessageCounts("ws-1", sessions)

	assert.Equal(t, missBefore+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_miss")),
		"a wrapped ErrSessionNotFound classifies as a confirmed miss")
	assert.Equal(t, errBefore+1, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("listing_count_walk_error")),
		"a wrapped ErrHTTPStatus classifies as transient")
	assert.False(t, errors.Is(transient, agent.ErrSessionNotFound),
		"sanity: the sentinels are distinct")
}
