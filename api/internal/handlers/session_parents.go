// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// sessionParentEntry is a single cached parent lookup for a session.
type sessionParentEntry struct {
	parentID string // empty if session has no parent (i.e. is a root)
	cachedAt time.Time
}

// sessionParentCache resolves a sessionID → its root sessionID, walking the
// session.parentID chain the agent Adapter exposes (session.Session.ParentID).
//
// Why this exists: opencode's `task` tool spawns subagent sessions whose
// permission/question events carry the SUBTASK's sessionID, not the user's
// active session. Without resolution, the frontend filters those events out.
// The resolver looks up GET /session/:id once per session, caches the
// (id → parentID) mapping in-memory, and walks parents to find the root.
//
// Cache scope: per-workspace map. Entries are eternal within a process —
// a session's parentID is immutable once set by opencode. The cache is
// dropped when the workspace's password cache is invalidated (workspace
// suspended/restarted), see invalidate.
type sessionParentCache struct {
	mu      sync.RWMutex
	entries map[string]map[string]sessionParentEntry // workspaceID → sessionID → entry

	// fetcher fetches a session by ID from the agent pod.
	// Returns parentID ("" if root) and an error.
	fetcher sessionParentFetcher
}

// sessionParentFetcher fetches a single session's parentID from the workspace pod.
type sessionParentFetcher func(ctx context.Context, workspaceID, sessionID string) (parentID string, err error)

func newSessionParentCache(fetcher sessionParentFetcher) *sessionParentCache {
	return &sessionParentCache{
		entries: make(map[string]map[string]sessionParentEntry),
		fetcher: fetcher,
	}
}

// resolveRoot walks the parent chain starting at sessionID and returns the
// top-level (root) session — i.e. the ancestor with no parentID. If
// resolution fails at any point, the deepest known ancestor is returned
// (best-effort: better to bubble to the wrong session occasionally than to
// silently drop the prompt entirely).
//
// maxDepth bounds the walk to prevent runaway loops on a malformed parent
// chain (e.g. a cycle introduced by a bug in the agent). 16 is far above
// any realistic subagent nesting.
func (c *sessionParentCache) resolveRoot(ctx context.Context, workspaceID, sessionID string) string {
	const maxDepth = 16
	current := sessionID
	for i := 0; i < maxDepth; i++ {
		parent, err := c.lookup(ctx, workspaceID, current)
		if err != nil || parent == "" {
			return current
		}
		current = parent
	}
	return current
}

// lookup returns the parentID for the given session. Cached on first call.
// Returns "" if the session has no parent.
func (c *sessionParentCache) lookup(ctx context.Context, workspaceID, sessionID string) (string, error) {
	c.mu.RLock()
	if ws := c.entries[workspaceID]; ws != nil {
		if e, ok := ws[sessionID]; ok {
			c.mu.RUnlock()
			return e.parentID, nil
		}
	}
	c.mu.RUnlock()

	parentID, err := c.fetcher(ctx, workspaceID, sessionID)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	if c.entries[workspaceID] == nil {
		c.entries[workspaceID] = make(map[string]sessionParentEntry)
	}
	c.entries[workspaceID][sessionID] = sessionParentEntry{
		parentID: parentID,
		cachedAt: time.Now(),
	}
	c.mu.Unlock()

	return parentID, nil
}

// invalidate drops all cached entries for a workspace. Called when the
// workspace's password rotates (suspend/restart), since the session
// listing is no longer reachable with the prior credentials.
func (c *sessionParentCache) invalidate(workspaceID string) {
	c.mu.Lock()
	delete(c.entries, workspaceID)
	c.mu.Unlock()
}

// fetchSessionParent fetches a single session's parentID from the workspace
// pod. Used as the default fetcher for sessionParentCache; intentionally
// not a method so the cache can be unit-tested with a fake fetcher.
//
// Delegates to adapter.GetSession (typed session.Session with ParentID
// — no inline JSON parse, no raw HTTP, no hardcoded auth/port). The
// legacy raw-HTTP tail was deleted in #828 batch 4; a nil adapter is a
// wiring failure surfaced as an error (resolveRoot degrades to
// returning the session itself).
func (h *ProxyHandler) fetchSessionParent(ctx context.Context, workspaceID, sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("invalid sessionID: %w", err)
	}
	s, err := h.adapter.GetSession(ctx, "", workspaceID, sessionID)
	if err != nil {
		return "", fmt.Errorf("adapter GetSession: %w", err)
	}
	if s == nil {
		return "", nil
	}
	return s.ParentID, nil
}
