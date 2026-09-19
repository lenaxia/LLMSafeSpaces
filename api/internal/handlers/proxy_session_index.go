// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
)

// sessionGone messages are the #1340 typed gone-state body, house
// pattern (code + human message, mirroring the 422
// text_only_model_image_history surface). The code discriminator drives
// the frontend's gone-state rendering; the message is for humans and
// MUST be true for the guard outcome: a reaped row is gone from the
// list; a history-bearing row the guard kept is still listed pending
// operator review (r2 finding — one message for both was a user-visible
// untruth in the kept case).
const (
	sessionGoneReaped = "This session no longer exists on the agent — it may have been deleted. It has been removed from your session list."
	sessionGoneKept   = "This session no longer exists on the agent — it may have been deleted. Its history entry remains in your session list pending review."
)

func writeSessionGoneBody(c *gin.Context, message string) {
	c.JSON(http.StatusGone, gin.H{
		"code":  "session_gone",
		"error": message,
	})
}

// reapSessionIfGone classifies an adapter error from a session-scoped
// read: when the agent's definitive verdict is not-found
// (agent.ErrSessionNotFound), the session_index row is a stale ghost
// (#1340). The TYPED 410 always answers (the session is unreadable
// either way); the row itself is reaped only when the #1340 triage
// safety guard admits it (no message history — nothing of value). A
// history-bearing row the harness lost stays for operator attention
// (a vanished session WITH history may indicate a harness store reset).
// Any other error (transport, 5xx, timeouts) returns false so the
// caller keeps its generic path: an unreachable pod is not evidence of
// deletion.
func (h *ProxyHandler) reapSessionIfGone(c *gin.Context, workspaceID, sessionID string, err error, route string) bool {
	if !errors.Is(err, agent.ErrSessionNotFound) {
		return false
	}
	if h.sessionIndex != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		rows, listErr := h.sessionIndex.ListByWorkspace(ctx, workspaceID)
		if listErr != nil {
			h.logger.Warn("session_gone: index lookup failed; row left for the convergence pass",
				"route", route, "workspaceID", workspaceID, "sessionID", sessionID, "error", listErr.Error())
		}
		for _, row := range rows {
			if row.ID != sessionID {
				continue
			}
			if sessionindex.DefaultReapGuard(row) {
				if delErr := h.sessionIndex.DeleteSession(ctx, workspaceID, sessionID); delErr != nil {
					h.logger.Warn("session_gone: index row reap failed",
						"route", route, "workspaceID", workspaceID, "sessionID", sessionID, "error", delErr.Error())
				}
				writeSessionGoneBody(c, sessionGoneReaped)
			} else {
				h.logger.Warn("session_gone: history-bearing row kept for operator review",
					"route", route, "workspaceID", workspaceID, "sessionID", sessionID,
					"messageCount", row.MessageCount)
				writeSessionGoneBody(c, sessionGoneKept)
			}
			cancel()
			return true
		}
		cancel()
	}
	writeSessionGoneBody(c, sessionGoneReaped)
	return true
}

// reconcileCadence bounds how often a workspace runs the #1340
// convergence pass: the sidebar refreshes often, but one harness
// session-list round-trip per workspace per 30s is the S5b/L10 budget.
// The claim is CROSS-REPLICA (wsstate SETNX-style): one replica runs
// each window, so the counter read-modify-write below has a single
// writer — the "N counts checks, not per-replica repetitions" invariant
// is enforced, not approximated.
const reconcileCadence = 30 * time.Second

// reconcileMissThreshold is the consecutive-absent check count before a
// ghost row is deleted (issue #1340: N=2 survives one mid-restart
// snapshot miss).
const reconcileMissThreshold = 2

// ReconcileSessionIndex is the #1340 convergence pass, piggybacked on
// the sidebar's session list (the path that already overlays harness
// ground truth). Fire-and-forget: cadence-claimed per workspace
// (cross-replica), diffs the index rows against the agent's
// authoritative session list, and deletes guard-admitted rows the agent
// has reported absent for N consecutive checks. The miss counters live
// in wsstate (cross-replica with decay).
func (h *ProxyHandler) ReconcileSessionIndex(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil {
		return
	}
	if !h.state().ClaimReconcileTurn(ctx, workspaceID, reconcileCadence) {
		return
	}
	go h.runSessionIndexReconciliation(workspaceID) //nolint:gosec,contextcheck // G118: intentional fire-and-forget detach (BackfillSessionParents precedent)
}

// runSessionIndexReconciliation deliberately uses a detached
// context.Background() bounded by a 15s timeout: the pass benefits
// future requests, not this one, and must survive client disconnect.
func (h *ProxyHandler) runSessionIndexReconciliation(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := h.sessionIndex.ListByWorkspace(ctx, workspaceID)
	if err != nil {
		h.logger.Debug("session reconcile: index list failed", "workspaceID", workspaceID, "error", err.Error())
		return
	}
	if len(rows) == 0 {
		return
	}

	prev := h.state().GetReconcileMisses(ctx, workspaceID)
	lister := func() (map[string]bool, error) {
		sessions, err := h.adapter.ListSessions(ctx, "", workspaceID)
		if err != nil {
			return nil, err
		}
		present := make(map[string]bool, len(sessions))
		for _, s := range sessions {
			present[s.ID] = true
		}
		return present, nil
	}

	remove, keptForOperator, next := sessionindex.PlanReconciliation(rows, lister, prev, reconcileMissThreshold, sessionindex.DefaultReapGuard)
	// keptForOperator rows keep a threshold-level counter so every
	// subsequent pass re-detects them — the operator signal fires
	// consistently, never every-other-pass (r2 finding).
	for _, id := range keptForOperator {
		next[id] = reconcileMissThreshold
	}
	h.state().SetReconcileMisses(ctx, workspaceID, next)
	for _, id := range remove {
		if err := h.sessionIndex.DeleteSession(ctx, workspaceID, id); err != nil {
			sessionindex.ReconcileOutcome("delete_failed")
			h.logger.Warn("session reconcile: ghost delete failed", "workspaceID", workspaceID, "sessionID", id, "error", err.Error())
			continue
		}
		sessionindex.ReconcileOutcome("reaped")
		h.logger.Info("session reconcile: reaped ghost index row",
			"workspaceID", workspaceID, "sessionID", id)
	}
	for _, id := range keptForOperator {
		// #1340 triage guard: history-bearing rows the harness reports
		// gone are NOT auto-deleted — metric + Warn for operator review
		// (a vanished session with history may indicate a harness store
		// reset; the read path still answers the typed gone-state).
		sessionindex.ReconcileOutcome("kept_for_operator")
		h.logger.Warn("session reconcile: history-bearing row absent at harness, kept for operator review",
			"workspaceID", workspaceID, "sessionID", id)
	}
}

func (h *ProxyHandler) SetSessionIndex(si interfaces.SessionIndexService) {
	h.sessionIndex = si
}

func (h *ProxyHandler) fetchAndPersistTitle(workspaceID, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := h.adapter.GetSession(ctx, "", workspaceID, sessionID)
	if err != nil || s == nil {
		return
	}
	h.persistSessionMeta(ctx, workspaceID, sessionID, s.Title, s.ParentID)
}

// persistSessionMeta writes the title and parentID to the session index
// from the Adapter's typed session.Session.
func (h *ProxyHandler) persistSessionMeta(ctx context.Context, workspaceID, sessionID, title, parentID string) {
	if title != "" {
		if err := h.sessionIndex.UpsertTitle(ctx, workspaceID, sessionID, title); err != nil {
			h.logger.Error("Failed to persist session title", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
	if parentID != "" {
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, sessionID, parentID); err != nil {
			h.logger.Error("Failed to persist session parent", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
}

func (h *ProxyHandler) BackfillSessionParents(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil {
		return
	}
	if h.state().GetParentBackfilled(ctx, workspaceID) {
		return
	}
	h.state().SetParentBackfilled(ctx, workspaceID)

	// Fire-and-forget: the backfill benefits future requests, not this one, so
	// it must survive client disconnect. runParentBackfill uses a detached
	// context.Background() bounded by a 15s timeout (not the request ctx).
	go h.runParentBackfill(workspaceID) //nolint:gosec,contextcheck // G118: intentional fire-and-forget detach
}

// runParentBackfill deliberately uses a detached context.Background() (bounded
// by a 15s timeout): the backfill must survive client disconnect (the request
// ctx is canceled once the response is flushed), which would abort the
// parent_session_id persistence. It takes no ctx so contextcheck does not
// expect propagation from the (request-scoped) caller.
func (h *ProxyHandler) runParentBackfill(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sessions, err := h.adapter.ListSessions(ctx, "", workspaceID)
	if err != nil {
		h.logger.Debug("Backfill: adapter ListSessions failed", "workspaceID", workspaceID, "error", err)
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}
	written := 0
	for _, s := range sessions {
		if s.ID == "" || s.ParentID == "" {
			continue
		}
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, s.ID, s.ParentID); err != nil {
			h.logger.Debug("Backfill: upsert parent failed", "workspaceID", workspaceID, "sessionID", s.ID, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		h.logger.Info("Backfilled session parents", "workspaceID", workspaceID, "count", written)
	}
}
