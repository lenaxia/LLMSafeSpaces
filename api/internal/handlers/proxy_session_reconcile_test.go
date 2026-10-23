// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1340 S5b: the reconciliation ORCHESTRATION (the pure decision matrix
// lives in sessionindex.PlanReconciliation's own tests). These pins
// prove the wiring: index rows → harness list → wsstate counters →
// DeleteSession after N consecutive absences; harness errors never reap.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func reconcileTestHandler(t *testing.T, harnessSessions map[string]bool, harnessErr error) (*ProxyHandler, *mockSessionIndex) {
	t.Helper()
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			if harnessErr != nil {
				return nil, harnessErr
			}
			out := make([]session.Session, 0, len(harnessSessions))
			for id := range harnessSessions {
				out = append(out, session.Session{ID: id})
			}
			return out, nil
		},
		// #1850: the reap's by-ID confirmation probe. Default answers
		// not-found so the pre-probe reap semantics (the ghost rows the
		// original pins seeded) still reap; the #1850 pins override it.
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, agent.ErrSessionNotFound
		},
	}
	return h, idx
}

func runReconcile(t *testing.T, h *ProxyHandler, workspaceID string) {
	t.Helper()
	h.runSessionIndexReconciliation(workspaceID)
}

func TestSessionIndexReconcile_ReapsAfterTwoConsecutiveAbsences(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_alive")
	idx.seedRow("ws-1", "ses_ghost") // count 0: guard admits

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "first absence increments, never reaps")
	assert.Equal(t, map[string]int{"ses_ghost": 1},
		h.state().GetReconcileMisses(context.Background(), "ws-1"))

	runReconcile(t, h, "ws-1")
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "second consecutive absence reaps the count-zero ghost")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_alive")
	assert.Empty(t, h.state().GetReconcileMisses(context.Background(), "ws-1"),
		"reaped + present rows leave no counters behind")
}

// The triage safety guard: a history-bearing row the harness reports
// gone is NEVER auto-deleted — kept for operator review (the read path
// still answers the typed gone-state, so the UX self-heals).
func TestSessionIndexReconcile_HistoryBearingGhostKeptForOperator(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	idx.seedRowCounted("ws-1", "ses_vanished", 12)
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_vanished": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "message_count>0 rows are never auto-deleted")
	// The kept row's counter pins AT the threshold (r2): every
	// subsequent pass re-detects it — the operator signal is
	// consistent, never the r2-found 1→2→1 oscillation.
	assert.Equal(t, map[string]int{"ses_vanished": reconcileMissThreshold},
		h.state().GetReconcileMisses(context.Background(), "ws-1"))
}

func TestSessionIndexReconcile_PresenceResetsMiss(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_flap": true}, nil)
	idx.seedRow("ws-1", "ses_flap")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_flap": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "a present session never reaps regardless of prior misses")
	assert.Empty(t, h.state().GetReconcileMisses(context.Background(), "ws-1"))
}

func TestSessionIndexReconcile_HarnessErrorNeverReaps(t *testing.T) {
	h, idx := reconcileTestHandler(t, nil, fmt.Errorf("pod unreachable"))
	idx.seedRow("ws-1", "ses_maybe")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_maybe": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "an unreachable pod is not evidence of deletion")
	assert.Equal(t, map[string]int{"ses_maybe": 1},
		h.state().GetReconcileMisses(context.Background(), "ws-1"), "counters frozen on error")
}

func TestSessionIndexReconcile_CadenceGate(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h, idx := reconcileTestHandler(t, nil, nil)
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil, nil
		},
	}
	// Rows must exist or runSessionIndexReconciliation returns before
	// the adapter call (the empty-index fast path).
	idx.seedRow("ws-1", "ses_a")
	idx.seedRow("ws-2", "ses_b")

	ctx := context.Background()
	h.ReconcileSessionIndex(ctx, "ws-1")
	h.ReconcileSessionIndex(ctx, "ws-1")
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	}, 3*time.Second, 50*time.Millisecond, "the cross-replica turn claim must coalesce bursts")

	// The claim is per workspace: a different workspace still runs.
	h.ReconcileSessionIndex(ctx, "ws-2")
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 2
	}, 3*time.Second, 50*time.Millisecond)
}

func TestSessionIndexReconcile_EmptyIndexSkipsHarnessCall(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil) // lister would reap everything
	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree)
}

// r4: the pass's delete-failure leg — the pass CONTINUES (no abort,
// later deletions still run) and every disposition is counted.
func TestSessionIndexReconcile_DeleteFailedContinuesAndCounts(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_alive")          // present
	idx.seedRowCounted("ws-1", "ses_dies", 0) // guard-admitted ghost
	idx.seedRowCounted("ws-1", "ses_kept", 9) // history-bearing ghost
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{
		"ses_dies": 1, "ses_kept": 1,
	})

	reapedBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped"))
	keptBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator"))
	failedBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("delete_failed"))

	idx.failDelete = true
	runReconcile(t, h, "ws-1")

	assert.Empty(t, idx.deletedTree, "the failing delete removed nothing")
	// The pass continued through the failure: the kept_for_operator
	// disposition was still recorded in the SAME pass.
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("delete_failed")), failedBefore,
		"the delete failure must be counted")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator")), keptBefore,
		"the pass must continue past a failed deletion")

	// Recover: the same pass next window reaps cleanly and counts it.
	idx.failDelete = false
	runReconcile(t, h, "ws-1")
	assert.True(t, idx.deletedTree["ws-1/ses_dies"], "a later pass must still reap")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped")), reapedBefore)
}

// --- #1850: the reap's by-ID confirmation -----------------------------------
//
// The harness LIST is not a liveness authority for Act-path sessions
// (#1452: sessions created through the harness path — exactly the
// create_session spawn class — can be live and readable by ID while
// never appearing in the list). A fresh spawn's index row sits at
// message_count==0 for the whole (long) first turn, so two 30s-cadence
// passes that miss it in the list reaped the row — and the first
// turn-boundary event re-registered the session, MINTING A NEW PHRASE
// (the #1850 re-phrase: old phrase dead, sends to it 500). The reap
// must confirm gone by ID (the SessionExists precedent: immune to the
// list-visibility gap) before deleting.

// TestSessionIndexReconcile_ListInvisibleLiveSessionNotReaped is the
// #1850 red-first pin: a session the LIST misses twice but the by-ID
// probe finds ALIVE must keep its index row (and with it its phrase).
func TestSessionIndexReconcile_ListInvisibleLiveSessionNotReaped(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_other": true}, nil)
	// The spawn: list-invisible (absent from the list above), alive by ID.
	h.adapter.(*mockAdapter).getSessionFn = func(_ context.Context, _, _, sid string) (*session.Session, error) {
		assert.Equal(t, "ses_spawn", sid, "the probe must target the row up for reaping")
		return &session.Session{ID: sid, Title: "worker-1"}, nil
	}
	idx.seedRow("ws-1", "ses_spawn") // count 0: the guard alone would admit
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_spawn": 1})

	runReconcile(t, h, "ws-1") // miss 2 → the plan says remove
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_spawn",
		"a live (by-ID) session must never be reaped off the list gap alone — the row's phrase dies with it (#1850)")
	// A live verdict is presence: the miss counter resets, so the next
	// pass starts fresh instead of re-accusing immediately.
	assert.NotContains(t, h.state().GetReconcileMisses(context.Background(), "ws-1"), "ses_spawn",
		"a live probe verdict resets the miss counter (presence semantics)")
}

// TestSessionIndexReconcile_GhostReapedWhenByIDProbeConfirmsGone keeps
// the true-deletion leg honest: the by-ID not-found verdict reaps
// exactly as before (the probe gates, it does not neuter).
func TestSessionIndexReconcile_GhostReapedWhenByIDProbeConfirmsGone(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_alive")
	idx.seedRow("ws-1", "ses_ghost")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_ghost": 1})

	runReconcile(t, h, "ws-1")
	runReconcile(t, h, "ws-1")
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "list-absent twice + by-ID not-found still reaps")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_alive")
}

// TestSessionIndexReconcile_ProbeIndeterminateDefersReap: a probe
// transport error is uncertainty, never a gone-verdict — the row
// survives the pass and the counter pins at the threshold so the next
// pass retries (the harness-error-never-reaps discipline, applied to
// the probe).
func TestSessionIndexReconcile_ProbeIndeterminateDefersReap(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	h.adapter.(*mockAdapter).getSessionFn = func(_ context.Context, _, _, _ string) (*session.Session, error) {
		return nil, fmt.Errorf("pod unreachable")
	}
	idx.seedRow("ws-1", "ses_maybe")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_maybe": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "an indeterminate probe must defer the reap")
	assert.Equal(t, map[string]int{"ses_maybe": reconcileMissThreshold},
		h.state().GetReconcileMisses(context.Background(), "ws-1"),
		"the counter pins at threshold — the next pass retries the probe")
}

// TestSessionIndexReconcile_Sessionless2xxProbeDefersReap pins the
// round-1 review finding-2 leg: ParseSessionWire answers a NON-NIL
// parse with an empty session (nil error) for empty 2xx bodies — a
// sessionless probe verdict that is neither alive nor a gone-verdict.
// Uncertainty never deletes: the row survives, the counter pins.
func TestSessionIndexReconcile_Sessionless2xxProbeDefersReap(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	h.adapter.(*mockAdapter).getSessionFn = func(_ context.Context, _, _, _ string) (*session.Session, error) {
		return nil, nil // sessionless 2xx: (false, nil) — the old code fell through to delete
	}
	idx.seedRow("ws-1", "ses_empty2xx")
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_empty2xx": 1})

	runReconcile(t, h, "ws-1")
	assert.Empty(t, idx.deletedTree, "a sessionless probe verdict must defer the reap (uncertainty never deletes)")
	assert.Equal(t, map[string]int{"ses_empty2xx": reconcileMissThreshold},
		h.state().GetReconcileMisses(context.Background(), "ws-1"),
		"the counter pins at threshold — the next pass retries the probe")
}

// --- #1882 r1: the reconcile re-birth race ----------------------------------
//
// The propagation teardown's design premise is that late row-creating
// writers find the tombstone (#1881 made every row-creating upsert
// mint a phrase on INSERT — a re-birth is a re-MINT, a fresh ghost).
// The reconcile is the one carrier-relevant writer that never looked:
// its count-rebuild walks the harness (240–490ms/page network calls)
// BETWEEN the harness-list snapshot and its Register +
// RebuildMessageCount writes — a carrier whose confirmed delete lands
// in that window would be re-born with message_count=1 and refused by
// DefaultReapGuard forever (kept_for_operator — the exact harm #1882
// reports, at lower rate). Same exposure for the backfill sweep's
// Register.

// TestSessionIndexReconcile_CountRebuildSkipsTombstonedRow: the
// carrier is present at the list snapshot; its delete propagates
// DURING the count walk (tombstone + row removal mid-walk — the
// validated window); the rebuild's row-creating writes must not
// re-mint the deleted row.
func TestSessionIndexReconcile_CountRebuildSkipsTombstonedRow(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_carrier": true}, nil)
	// Phrase-bearing row (ShortID set — the backfill sweep must skip
	// it too) with a stale count so the rebuild write would fire.
	idx.mu.Lock()
	idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
		ID: "ses_carrier", Title: "900-word lighthouse keeper story",
		ShortID: "oc-00000001", MessageCount: 1,
	})
	idx.mu.Unlock()
	// The count walk IS the race window: the carrier's confirmed delete
	// (tombstone + row removal — the propagation tail) lands mid-walk.
	h.adapter.(*mockAdapter).countMessagesFn = func(ctx context.Context, _, _, sid string) (int, error) {
		//nolint:contextcheck // the mid-walk propagation tail is store-local by design
		h.MarkSessionDeletedForTest("ws-1", sid)
		_ = idx.DeleteSession(ctx, "ws-1", sid)
		return 2, nil // != seeded 1 — the rebuild write would fire
	}

	runReconcile(t, h, "ws-1")

	assert.True(t, idx.deletedTree["ws-1/ses_carrier"],
		"sanity: the propagation's row removal landed")
	assert.Equal(t, 0, idx.registerCalls["ws-1/ses_carrier"],
		"a tombstoned session must not be re-registered (re-birth = re-mint, #1882 r1)")
	assert.NotContains(t, idx.rebuiltCounts, "ws-1/ses_carrier",
		"a tombstoned session's count rebuild must not re-create its row")
}

// TestSessionIndexReconcile_BackfillSkipsTombstonedRow: the backfill
// sweep's Register is a row-creating mint for phrase-less rows — a
// deleted carrier in the pre-pass snapshot must not be re-registered.
func TestSessionIndexReconcile_BackfillSkipsTombstonedRow(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_deadnull": true}, nil)
	idx.seedRow("ws-1", "ses_deadnull") // count 0, no ShortID — backfill target
	// Deleted before the pass (tombstone only — the row still rides the
	// stale snapshot the reconcile already listed).
	h.MarkSessionDeletedForTest("ws-1", "ses_deadnull")

	runReconcile(t, h, "ws-1")

	assert.Equal(t, 0, idx.registerCalls["ws-1/ses_deadnull"],
		"a tombstoned session must not be backfilled (Register re-creates the row, #1882 r1)")
}
