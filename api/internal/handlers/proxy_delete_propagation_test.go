// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1882 delete propagation, proxy side: (1) the platform-initiated
// delete effects (engine teardown, authorized deletes, ephemeral
// carrier cleanup) ride the SAME effect sequence as the user route —
// wsstate tombstone + durable session_index death mark + row delete +
// session.status:deleted SSE; (2) the #1340 reconcile reaps
// session_origins-marked carrier rows (the DefaultReapGuard history
// exception must not keep platform carriers alive forever), with the
// #1850 by-ID probe confirmation intact.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// stubOriginLister feeds the #1901 carrier-mark lookup (carrier-class
// ids only — the seam's contract).
type stubOriginLister struct {
	marks map[string]bool // carrier-class session ids
	err   error
}

func (s *stubOriginLister) ListCarrierOriginMarks(_ context.Context, _ string) (map[string]bool, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.marks, nil
}

// TestDeletedSessionChecker_ReflectsLiveStateStore — #1901 item (c)
// wiring contract: the drainer's tombstone consult is exposed as a
// closure that must resolve the LIVE state store per call (a checker
// captured against a pre-swap store would silently miss every
// tombstone after a SetStateStore swap — the redis-migration class).
func TestDeletedSessionChecker_ReflectsLiveStateStore(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	checker := h.DeletedSessionChecker()

	assert.False(t, checker(context.Background(), "ws-1", "ses_x"),
		"no tombstone → the consult must answer false (event applies)")

	h.state().MarkSessionDeleted(context.Background(), "ws-1", "ses_x")
	assert.True(t, checker(context.Background(), "ws-1", "ses_x"),
		"the closure must see the live store's tombstone (event dropped)")
}

// TestApplySessionDeleteEffects_EmitsAllEffects pins the engine-facing
// effect tail: tombstone (late-event suppression), the durable index
// death mark (read-path filter), the row delete, and the SSE deleted
// event (frontend sidebar clear). This is byte-for-byte the effect set
// the user route emits at proxy_handlers.go HardDeleteSession.
func TestApplySessionDeleteEffects_EmitsAllEffects(t *testing.T) {
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.userBroker = eventbroker.NewUserEventBroker()

	wsSub, err := h.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeWorkspace("ws-1", wsSub)

	h.ApplySessionDeleteEffects(context.Background(), "ws-1", "ses_carrier")

	assert.True(t, h.isSessionDeleted("ws-1", "ses_carrier"),
		"the tombstone must be set (late pod events suppressed)")
	assert.True(t, idx.markedDeleted["ws-1/ses_carrier"],
		"the durable session_index death mark must land (read-path filter)")
	assert.True(t, idx.deletedTree["ws-1/ses_carrier"],
		"the session_index row must be removed (sidebar ghost gone)")
	evt := recvWithTimeout(t, wsSub, "session.status")
	assert.Equal(t, "deleted", evt.Status,
		"the deleted status event clears the frontend sidebar — the exact event the frontend already handles")
}

// TestHardDeleteSession_UserRouteMarksDeletedAt: the user route's tail
// (HardDeleteSession) must ALSO write the durable death mark — a
// failed row-delete there must not leave a renderable row either.
func TestHardDeleteSession_UserRouteMarksDeletedAt(t *testing.T) {
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		deleteSessionFn: func(context.Context, string, string, string) error { return nil },
	}

	require.NoError(t, h.HardDeleteSession(context.Background(), "ws-1", "ses_userdel"))

	assert.True(t, idx.markedDeleted["ws-1/ses_userdel"],
		"the user route must set the durable death mark before the row delete")
	assert.True(t, idx.deletedTree["ws-1/ses_userdel"])
}

// TestSessionIndexReconcile_OriginMarkedCarrierReaped is the #1882
// self-heal pin: a history-bearing session_index row whose session is
// session_origins-marked (a platform routine carrier) and absent at the
// harness is REAPED — the DefaultReapGuard history exception does not
// keep platform carriers alive forever. The #1850 by-ID probe
// confirmation still gates the delete (default harness answers
// not-found).
func TestSessionIndexReconcile_OriginMarkedCarrierReaped(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	h.SetSessionOriginLister(&stubOriginLister{marks: map[string]bool{
		"ses_carrier": true,
	}})
	idx.seedRow("ws-1", "ses_alive")
	idx.seedRowCounted("ws-1", "ses_carrier", 7) // history-bearing: DefaultReapGuard refuses
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_carrier": 1})

	reapedCarriersBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_carrier"))

	runReconcile(t, h, "ws-1") // miss 2 → guard bypass admits → probe confirms gone
	runReconcile(t, h, "ws-1")

	assert.True(t, idx.deletedTree["ws-1/ses_carrier"],
		"an origin-marked carrier row with history must be reaped (#1882)")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_alive")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_carrier")), reapedCarriersBefore,
		"the carrier reap is observable (reaped_carrier outcome)")
}

// TestSessionIndexReconcile_PreservedSessionMarkNotAdmitted is the
// #1901 item-b invariant: a PRESERVED routine session (its persistent
// origin mark, kind='session — invisible through the carrier-marks
// seam BY CONTRACT) absent at the harness keeps the operator-review
// guard. A harness store reset is exactly the kept_for_operator
// scenario for it — the blanket bypass (pre-#1901: every
// session_origins mark admitted) would have reaped its history row.
// The carrier in the same pass still reaps: one workspace, two fates,
// one discriminator.
func TestSessionIndexReconcile_PreservedSessionMarkNotAdmitted(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	// Carrier-class marks: ONLY the carrier. The preserved session's
	// origin row exists (kind='session') but never appears here.
	h.SetSessionOriginLister(&stubOriginLister{marks: map[string]bool{
		"ses_carrier": true,
	}})
	idx.seedRowCounted("ws-1", "ses_carrier", 7)    // history-bearing carrier: policy-deleted
	idx.seedRowCounted("ws-1", "ses_preserved", 11) // history-bearing preserved: store-reset candidate
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{
		"ses_carrier":   1,
		"ses_preserved": 1,
	})

	keptBefore := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator"))

	runReconcile(t, h, "ws-1") // miss 2 for both rows
	runReconcile(t, h, "ws-1")

	assert.True(t, idx.deletedTree["ws-1/ses_carrier"],
		"the carrier-class mark still admits the carrier's reap")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_preserved",
		"a preserved session's mark must NEVER admit its row past the guard (#1901 b)")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator")), keptBefore,
		"the preserved row surfaces kept_for_operator — the operator-review signal")
}

// TestSessionIndexReconcile_OriginMarkedCarrier_ProbeVetoStillApplies:
// the #1865 semantics are unchanged for marked rows — a live-by-ID
// verdict vetoes the reap (the #1452 list-visibility gap protects
// platform carriers exactly as it protects spawns).
func TestSessionIndexReconcile_OriginMarkedCarrier_ProbeVetoStillApplies(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	h.SetSessionOriginLister(&stubOriginLister{marks: map[string]bool{
		"ses_carrier_live": true,
	}})
	h.adapter.(*mockAdapter).getSessionFn = func(_ context.Context, _, _, sid string) (*session.Session, error) {
		return &session.Session{ID: sid}, nil // alive by ID
	}
	idx.seedRowCounted("ws-1", "ses_carrier_live", 3)
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_carrier_live": 1})

	runReconcile(t, h, "ws-1")

	assert.NotContains(t, idx.deletedTree, "ws-1/ses_carrier_live",
		"a live-by-ID carrier must never be reaped off the list gap alone (#1850)")
	assert.NotContains(t, h.state().GetReconcileMisses(context.Background(), "ws-1"), "ses_carrier_live",
		"a live probe verdict resets the miss counter (presence semantics)")
}

// TestSessionIndexReconcile_OriginListerError_FailsSafe: an origins
// lookup failure must degrade to the DefaultReapGuard semantics (the
// history-bearing row stays kept_for_operator) — never widen a reap on
// uncertainty.
func TestSessionIndexReconcile_OriginListerError_FailsSafe(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{}, nil)
	h.SetSessionOriginLister(&stubOriginLister{err: fmt.Errorf("origins store down")})
	idx.seedRowCounted("ws-1", "ses_maybe_carrier", 5)
	h.state().SetReconcileMisses(context.Background(), "ws-1", map[string]int{"ses_maybe_carrier": 1})

	runReconcile(t, h, "ws-1")

	assert.Empty(t, idx.deletedTree, "an origins lookup failure must not admit history-bearing rows")
}

// TestReapSessionIfGone_OriginMarkedCarrierReaped: the READ-path reap
// (the owner's click on a ghost) admits origin-marked carriers with
// history — the row is reaped and the typed 410 answers sessionGoneReaped
// instead of sessionGoneKept ("pending review" is a lie for a carrier).
func TestReapSessionIfGone_OriginMarkedCarrierReaped(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_ghost_carrier", 4) // history: guard alone refuses
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.SetSessionOriginLister(&stubOriginLister{marks: map[string]bool{
		"ses_ghost_carrier": true,
	}})
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, goneAdapterErr()
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_ghost_carrier"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	assert.Contains(t, w.Body.String(), "It has been removed from your session list.",
		"a marked carrier's gone-body must claim removal, not pending-review")
	assert.True(t, idx.deletedTree["ws-1/ses_ghost_carrier"],
		"the marked carrier's row must be reaped on the read path (the owner's click)")
}
