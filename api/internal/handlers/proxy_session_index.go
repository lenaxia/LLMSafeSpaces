// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	agent "github.com/lenaxia/llmsafespaces/pkg/agent"
)

// sessionGoneHumanMessage is the #1340 typed gone-state body, house
// pattern (code + human message, mirroring the 422
// text_only_model_image_history surface). The code discriminator drives
// the frontend's gone-state rendering; the message is for humans.
const sessionGoneHumanMessage = "This session no longer exists on the agent — it may have been deleted. It has been removed from your session list."

func writeSessionGoneBody(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{
		"code":  "session_gone",
		"error": sessionGoneHumanMessage,
	})
}

// reapSessionIfGone classifies an adapter error from a session-scoped
// read: when the agent's definitive verdict is not-found
// (agent.ErrSessionNotFound), the session_index row is a stale ghost
// (#1340) — reap it (row + descendants) and answer with the typed 410.
// Any other error (transport, 5xx, timeouts) returns false so the
// caller keeps its generic path: an unreachable pod is not evidence of
// deletion.
func (h *ProxyHandler) reapSessionIfGone(c *gin.Context, workspaceID, sessionID string, err error, route string) bool {
	if !errors.Is(err, agent.ErrSessionNotFound) {
		return false
	}
	if h.sessionIndex != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if delErr := h.sessionIndex.DeleteSession(ctx, workspaceID, sessionID); delErr != nil {
			h.logger.Warn("session_gone: index row reap failed",
				"route", route, "workspaceID", workspaceID, "sessionID", sessionID, "error", delErr.Error())
		}
		cancel()
	}
	writeSessionGoneBody(c)
	return true
}

// reconcileCadence bounds how often a workspace runs the #1340
// convergence pass: the sidebar refreshes often, but one harness
// session-list round-trip per workspace per 30s is the S5b/L10 budget.
const reconcileCadence = 30 * time.Second

// reconcileMissThreshold is the consecutive-absent check count before a
// ghost row is deleted (issue #1340: N=2 survives one mid-restart
// snapshot miss).
const reconcileMissThreshold = 2

// lastReconcile gates the per-replica cadence (a rate limiter, not
// correctness state — counters live in the shared wsstate store).
var lastReconcile reconcileGate

type reconcileGate struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (g *reconcileGate) allow(workspaceID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = map[string]time.Time{}
	}
	if t, ok := g.seen[workspaceID]; ok && now.Sub(t) < reconcileCadence {
		return false
	}
	g.seen[workspaceID] = now
	return true
}

// ReconcileSessionIndex is the #1340 convergence pass, piggybacked on
// the sidebar's session list (the path that already overlays harness
// ground truth). Fire-and-forget: TTL-gated per workspace, diffs the
// index rows against the agent's authoritative session list, and deletes
// rows the agent has reported absent for N consecutive checks. The miss
// counters live in wsstate (cross-replica with decay) so N counts
// checks, not per-replica repetitions.
func (h *ProxyHandler) ReconcileSessionIndex(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil {
		return
	}
	if !lastReconcile.allow(workspaceID, time.Now()) {
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
	indexIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		indexIDs = append(indexIDs, r.ID)
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

	remove, next := sessionindex.PlanReconciliation(indexIDs, lister, prev, reconcileMissThreshold)
	h.state().SetReconcileMisses(ctx, workspaceID, next)
	for _, id := range remove {
		if err := h.sessionIndex.DeleteSession(ctx, workspaceID, id); err != nil {
			h.logger.Warn("session reconcile: ghost delete failed", "workspaceID", workspaceID, "sessionID", id, "error", err.Error())
			continue
		}
		h.logger.Info("session reconcile: reaped ghost index row",
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
