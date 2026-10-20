// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Design 0066 §6 per-path wiring pins for the two workspace-service
// row-creating shapes (review r1 finding 1): last-seen (MarkSessionSeen
// → UpdateLastSeen) and title-rename (RenameSession → UpsertTitle) must
// route through the registration seam BEFORE their upsert — a seen-ping
// or rename for an unindexed id can never mint a short-id-less ghost
// row that only the sweep would heal.
func TestMarkSessionSeen_RegistersThroughSeamFirst(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)
	si.On("Register", mock.Anything, "ws-1", "s1", "opencode").Return("oc-00000001").Once()
	si.On("UpdateLastSeen", mock.Anything, "ws-1", "s1").Return(nil).Once()

	require.NoError(t, f.svc.MarkSessionSeen(context.Background(), "user-1", "ws-1", "s1"))
	si.AssertExpectations(t)
}

func TestRenameSession_RegistersThroughSeamFirst(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)
	si.On("Register", mock.Anything, "ws-1", "s1", "opencode").Return("oc-00000002").Once()
	si.On("UpsertTitle", mock.Anything, "ws-1", "s1", "New Title").Return(nil).Once()

	require.NoError(t, f.svc.RenameSession(context.Background(), "user-1", "ws-1", "s1", "New Title"))
	si.AssertExpectations(t)
}
