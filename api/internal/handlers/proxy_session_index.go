// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
)

func (h *ProxyHandler) SetSessionIndex(si interfaces.SessionIndexService) {
	h.sessionIndex = si
}

func (h *ProxyHandler) fetchAndPersistTitle(workspaceID, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Typed session.Session via the Adapter (#828 batch 4: the raw-HTTP
	// legacy tail is deleted; a nil adapter — dev/test wiring — has no
	// way to fetch a title).
	if h.adapter == nil {
		return
	}
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
	if h.sessionIndex == nil || h.adapter == nil {
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

	// Typed []session.Session via the Adapter (#828 batch 4: the raw
	// SessionListPath legacy tail is deleted). Unreachable via the
	// production caller (BackfillSessionParents' gate returns first) —
	// defense-in-depth for direct/test callers.
	if h.adapter == nil {
		h.logger.Debug("Backfill skipped: no agent adapter", "workspaceID", workspaceID)
		return
	}
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
