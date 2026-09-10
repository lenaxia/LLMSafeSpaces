// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// adapter_v2.go — the V2-path adapter (#1314 refactor).
//
// V2 is the delivery path that persists to the V2 store
// (GET /api/session/:id/message) and uses session.next.* SSE events.
// On opencode 1.18.15, the V2 session runner does NOT include MCP
// tools in the model's function definitions — use AdapterV1 for
// MCP-tooled delivery. This adapter exists for V2-native consumers
// (the outbox terminus path when V2 delivery is explicitly enabled).
//
// The shared Adapter provides all non-store methods. This type ONLY
// overrides the store-dependent surface.

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// AdapterV2 is the V2-path adapter. Construct via NewAdapterV2.
type AdapterV2 struct {
	*Adapter
}

// NewAdapterV2 constructs a V2-path adapter from a shared Adapter.
func NewAdapterV2(base *Adapter) *AdapterV2 {
	return &AdapterV2{Adapter: base}
}

// GetHistory returns the full message list from the V2 store.
func (a *AdapterV2) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	return a.getHistoryV2Store(ctx, userID, workspaceID, sessionID, 0)
}

// GetHistoryPage returns the newest limit messages from the V2 store.
func (a *AdapterV2) GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	return a.getHistoryV2Store(ctx, userID, workspaceID, sessionID, limit)
}

// VerifyDelivery checks the V2 store for the delivered text.
func (a *AdapterV2) VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (delivered, ambiguous bool, err error) {
	return a.verifyDeliveryV2StoreResolved(ctx, userID, workspaceID, sessionID, text, since)
}
