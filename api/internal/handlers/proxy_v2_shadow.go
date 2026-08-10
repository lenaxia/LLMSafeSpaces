// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

// V2QueueShadow is a lightweight Redis-backed view-cache of pending V2
// queue messages per session. Written by the SSE bridge (US-63.5) on
// PromptAdmitted events; cleared on Prompted events; read by ListQueue
// for fresh-load visibility (US-63.10).
//
// This is NOT the deleted msgqueue.Service. It holds only {messageID,
// text, enqueuedAt} tuples derived from opencode's own events as a
// view-cache. opencode's SQLite is the authoritative source; the shadow
// is best-effort and may diverge on edge cases (replica crash mid-event).
// Cross-replica: Redis-backed so any replica can serve GET /queue.
type V2QueueShadow struct {
	client *redis.Client
}

// NewV2QueueShadow creates a shadow marker backed by the given Redis client.
// The client is borrowed — its lifecycle is managed by the caller (app.go).
// Returns nil if client is nil (V2 shadow is disabled; ListQueue returns empty).
func NewV2QueueShadow(client *redis.Client) *V2QueueShadow {
	if client == nil {
		return nil
	}
	return &V2QueueShadow{client: client}
}

// v2ShadowEntry is one pending message in the shadow marker.
type v2ShadowEntry struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	EnqueuedAt int64  `json:"enqueued_at"`
}

func shadowKey(workspaceID, sessionID string) string {
	return fmt.Sprintf("v2queue:%s:%s", workspaceID, sessionID)
}

// Add records a pending V2 queue message. Called from the SSE bridge when
// a PromptAdmitted event with delivery:"queue" fires.
func (s *V2QueueShadow) Add(ctx context.Context, workspaceID, sessionID, messageID, text string) {
	if s == nil {
		return
	}
	entry := v2ShadowEntry{ID: messageID, Text: text, EnqueuedAt: time.Now().Unix()}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_ = s.client.HSet(ctx, shadowKey(workspaceID, sessionID), messageID, data).Err()
}

// Remove clears a message from the shadow. Called when Prompted fires
// (message was promoted/drained) or on explicit dismissal.
func (s *V2QueueShadow) Remove(ctx context.Context, workspaceID, sessionID, messageID string) {
	if s == nil {
		return
	}
	_ = s.client.HDel(ctx, shadowKey(workspaceID, sessionID), messageID).Err()
}

// List returns all pending messages for a session. Called by ListQueue
// under the V2 flag for fresh-load pill visibility.
func (s *V2QueueShadow) List(ctx context.Context, workspaceID, sessionID string) []v2ShadowEntry {
	if s == nil {
		return nil
	}
	raw, err := s.client.HGetAll(ctx, shadowKey(workspaceID, sessionID)).Result()
	if err != nil {
		return nil
	}
	out := make([]v2ShadowEntry, 0, len(raw))
	for _, v := range raw {
		var entry v2ShadowEntry
		if json.Unmarshal([]byte(v), &entry) == nil {
			out = append(out, entry)
		}
	}
	return out
}

// ClearAll removes all pending entries for a session (e.g. on session delete).
func (s *V2QueueShadow) ClearAll(ctx context.Context, workspaceID, sessionID string) {
	if s == nil {
		return
	}
	_ = s.client.Del(ctx, shadowKey(workspaceID, sessionID)).Err()
}
