// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// adapter_v1.go — the V1-path adapter (#1314 refactor).
//
// V1 is the delivery path that includes MCP tools in the model's
// function definitions AND persists messages to the V1 history store
// (GET /session/:id/message). This adapter reads from the SAME store
// it writes to — the invariant the mixed v2Store flag broke.
//
// The shared Adapter provides all non-store methods (sessions, model,
// stream, credentials). This type ONLY overrides the store-dependent
// surface: GetHistory, GetHistoryPage, and delivery verification.

import (
	"context"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// AdapterV1 is the V1-path adapter. Construct via NewAdapterV1.
type AdapterV1 struct {
	*Adapter
}

// NewAdapterV1 constructs a V1-path adapter from a shared Adapter.
func NewAdapterV1(base *Adapter) *AdapterV1 {
	return &AdapterV1{Adapter: base}
}

// GetHistory returns the full message list from the V1 store.
func (a *AdapterV1) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	return a.getHistoryV1(ctx, userID, workspaceID, sessionID, 0)
}

// GetHistoryPage returns the newest limit messages from the V1 store.
func (a *AdapterV1) GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	return a.getHistoryV1(ctx, userID, workspaceID, sessionID, limit)
}

// VerifyDelivery checks the V1 store for the delivered text.
func (a *AdapterV1) VerifyDelivery(ctx context.Context, userID, workspaceID, sessionID, text string, since time.Time) (delivered, ambiguous bool, err error) {
	return a.verifyDeliveryV1StoreResolved(ctx, userID, workspaceID, sessionID, text, since)
}
