// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1889 F3 — reaped-with-phrase observability. When a session_index row
// carrying a MINTED phrase is deleted (ghost reap post-grace, the
// read-path reap, the platform delete-effects tail), the deletion must
// record that a phrase was orphaned: a `reaped_with_phrase` disposition
// on session_index_reconcile_outcomes_total + a Warn log. Pre-#1889
// nothing recorded it — a recurrence of the re-phrase class was
// reconstructable only from repro tables, invisible in ops dashboards.

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
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestReconcile_ReapedRowWithPhrase_IsObservable: the convergence pass
// reaps a phrase-bearing row → the reaped_with_phrase outcome fires.
func TestReconcile_ReapedRowWithPhrase_IsObservable(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRowPhrase("ws-1", "ses_ghost", "amber-falcon-glides", 0) // count 0: guard admits

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	runReconcile(t, h, "ws-1")
	runReconcile(t, h, "ws-1") // miss 2 → probe (default not-found) confirms → reap

	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "the phrase-bearing ghost row is reaped as before")
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")), before,
		"reaping a MINTED phrase must be observable (reaped_with_phrase disposition, #1889 F3)")
}

// TestReconcile_ReapedPhraselessRow_NoOutcome: a row with NO minted
// phrase orphaned nothing — the outcome must not fire (the counter is
// the phrase-orphaning signal, not a reap counter).
func TestReconcile_ReapedPhraselessRow_NoOutcome(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_alive": true}, nil)
	idx.seedRow("ws-1", "ses_ghost") // no ShortID

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	runReconcile(t, h, "ws-1")
	runReconcile(t, h, "ws-1")

	assert.True(t, idx.deletedTree["ws-1/ses_ghost"])
	assert.Equal(t, before, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")),
		"a phrase-less reap must not fire the phrase-orphaning outcome")
}

// TestReadPathReap_RowWithPhrase_IsObservable: reapSessionIfGone (the
// typed-410 read-path reap) deleting a phrase-bearing row fires the
// same outcome.
func TestReadPathReap_RowWithPhrase_IsObservable(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowPhrase("ws-1", "ses_ghost", "amber-falcon-glides", 0)
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return nil, fmt.Errorf("%w: GET /session/ses_ghost returned 404", agent.ErrSessionNotFound)
		},
	}

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	h.GetSession(c)

	require.Equal(t, http.StatusGone, w.Code)
	assert.True(t, idx.deletedTree["ws-1/ses_ghost"])
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")), before,
		"the read-path reap of a MINTED phrase must be observable")
}

// seedRowPhraseChild seeds an index row carrying a MINTED phrase and a
// parent link (the tree-walk fixtures: a child's ParentID is the
// parent's phrase, degrading to the parent's harness id when the
// parent row has no phrase).
func (m *mockSessionIndex) seedRowPhraseChild(workspaceID, sessionID, phrase, parentRef string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[workspaceID] = append(m.rows[workspaceID], types.SessionListItem{
		ID:           sessionID,
		InternalID:   sessionID,
		Title:        sessionID,
		ShortID:      phrase,
		ParentID:     parentRef,
		MessageCount: count,
	})
}

// TestApplySessionDeleteEffects_PhraseBearingRow_IsObservable: the
// platform delete-effects tail (user hard delete, engine teardown,
// carrier cleanup) orphaning a minted phrase fires the outcome.
func TestApplySessionDeleteEffects_PhraseBearingRow_IsObservable(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowPhrase("ws-1", "ses_del", "amber-falcon-glides", 3)
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.userBroker = eventbroker.NewUserEventBroker()

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	h.ApplySessionDeleteEffects(context.Background(), "ws-1", "ses_del")

	assert.True(t, idx.deletedTree["ws-1/ses_del"])
	assert.Greater(t, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")), before,
		"a platform delete orphaning a MINTED phrase must be observable")
}

// TestApplySessionDeleteEffects_PhraselessRow_NoOutcome: the mirror —
// no phrase, no outcome.
func TestApplySessionDeleteEffects_PhraselessRow_NoOutcome(t *testing.T) {
	idx := newMockSessionIndex()
	idx.seedRowCounted("ws-1", "ses_del", 3) // no ShortID
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.userBroker = eventbroker.NewUserEventBroker()

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	h.ApplySessionDeleteEffects(context.Background(), "ws-1", "ses_del")

	assert.True(t, idx.deletedTree["ws-1/ses_del"])
	assert.Equal(t, before, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")),
		"a phrase-less delete must not fire the phrase-orphaning outcome")
}

// TestApplySessionDeleteEffects_DescendantPhrasesObserved (r2
// robustness finding 1): DeleteSessionTree is RECURSIVE — every
// descendant row dies with the root, and each can carry its own minted
// phrase. A phrase-less root deleting a phrased child AND grandchild
// must observe BOTH orphaned phrases (transitive walk over the
// listing's parent links).
func TestApplySessionDeleteEffects_DescendantPhrasesObserved(t *testing.T) {
	idx := newMockSessionIndex()
	// Phrase-less root (its own observation fires nothing)…
	idx.seedRowCounted("ws-1", "ses_root", 3)
	// …phrased child (parent link by harness id — the degraded form
	// for a phrase-less parent)…
	idx.seedRowPhraseChild("ws-1", "ses_child", "linen-orchard-chirps", "ses_root", 2)
	// …and phrased grandchild (parent link by the CHILD's PHRASE — the
	// minted form).
	idx.seedRowPhraseChild("ws-1", "ses_grand", "mossy-tabby-cheers", "linen-orchard-chirps", 1)
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.userBroker = eventbroker.NewUserEventBroker()

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	h.ApplySessionDeleteEffects(context.Background(), "ws-1", "ses_root")

	assert.True(t, idx.deletedTree["ws-1/ses_root"])
	assert.Equal(t, before+2, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")),
		"BOTH the child's and the grandchild's phrases must be observed (the tree delete orphans every phrase it carries)")
}

// TestReconcile_TreeReapObservesLiveDescendantPhrase: the reconcile's
// tree variant of the same gap — a ghost root (absent at the harness,
// reaped post-probe) whose DESCENDANT is PRESENT and live: the
// descendant is not in the reap plan, but the tree delete kills its
// row too (production DeleteSessionTree is recursive), so its phrase
// must be observed.
func TestReconcile_TreeReapObservesLiveDescendantPhrase(t *testing.T) {
	h, idx := reconcileTestHandler(t, map[string]bool{"ses_descendant": true}, nil) // descendant PRESENT at harness
	idx.seedRowPhrase("ws-1", "ses_ghost", "amber-falcon-glides", 0)                // count 0: guard admits
	idx.seedRowPhraseChild("ws-1", "ses_descendant", "linen-orchard-chirps", "amber-falcon-glides", 4)

	before := promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase"))

	runReconcile(t, h, "ws-1")
	runReconcile(t, h, "ws-1") // root's miss 2 → probe (default not-found) confirms → tree reap

	assert.True(t, idx.deletedTree["ws-1/ses_ghost"], "the ghost root is reaped as before")
	assert.NotContains(t, idx.deletedTree, "ws-1/ses_descendant",
		"the live descendant is not independently reaped (present at the harness — its row dies only via the tree delete)")
	assert.Equal(t, before+2, promtestutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("reaped_with_phrase")),
		"the tree reap observes BOTH the root's and the live descendant's orphaned phrases")
}
