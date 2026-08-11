// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestListWorkspaceSessions_BackfillsContextUsedFromCRD verifies that
// sessions with NULL context_used in the DB are enriched from the
// workspace CRD status (which has fresh per-session values from agentd).
// This fixes the data-freshness gap caused by API pod restarts: when the
// SSE tracker reconnects after a restart, step.ended events during the
// downtime are missed, leaving context_used NULL in session_index.
func TestListWorkspaceSessions_BackfillsContextUsedFromCRD(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle"},
		{ID: "ses_b", Title: "B", MessageCount: 10, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			Sessions: []v1.AgentSessionStatus{
				{ID: "ses_a", Status: "idle", ContextUsed: 75043},
				{ID: "ses_b", Status: "idle", ContextUsed: 325355},
			},
		},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	si.On("UpsertContextUsed", mock.Anything, "ws-1", mock.Anything, mock.Anything).Return(nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, result, 2)

	require.NotNil(t, result[0].ContextUsed, "ses_a contextUsed must be backfilled from CRD")
	assert.Equal(t, int64(75043), *result[0].ContextUsed)

	require.NotNil(t, result[1].ContextUsed, "ses_b contextUsed must be backfilled from CRD")
	assert.Equal(t, int64(325355), *result[1].ContextUsed)
}

// TestListWorkspaceSessions_PreservesDBContextUsed verifies that
// sessions that already have context_used in the DB are NOT overwritten
// by the CRD value. The DB value (from the most recent live SSE event)
// is authoritative when present.
func TestListWorkspaceSessions_PreservesDBContextUsed(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	dbValue := int64(99999)
	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle", ContextUsed: &dbValue},
		{ID: "ses_b", Title: "B", MessageCount: 10, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			Sessions: []v1.AgentSessionStatus{
				{ID: "ses_a", Status: "idle", ContextUsed: 75043},
				{ID: "ses_b", Status: "idle", ContextUsed: 325355},
			},
		},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	si.On("UpsertContextUsed", mock.Anything, "ws-1", "ses_b", int64(325355)).Return(nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, result, 2)

	require.NotNil(t, result[0].ContextUsed)
	assert.Equal(t, int64(99999), *result[0].ContextUsed, "DB value must be preserved, not overwritten by CRD")

	require.NotNil(t, result[1].ContextUsed)
	assert.Equal(t, int64(325355), *result[1].ContextUsed, "NULL DB value must be backfilled from CRD")
}

// TestListWorkspaceSessions_AllHaveContext_SkipsCRDFetch verifies
// that the CRD is NOT fetched when all sessions already have
// context_used in the DB. This avoids unnecessary K8s API calls in
// the steady state.
func TestListWorkspaceSessions_AllHaveContext_SkipsCRDFetch(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	val1 := int64(1000)
	val2 := int64(2000)
	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle", ContextUsed: &val1},
		{ID: "ses_b", Title: "B", MessageCount: 10, Status: "idle", ContextUsed: &val2},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	// CRD Get should NOT be called
	f.ws.AssertNotCalled(t, "Get", mock.Anything, mock.Anything, mock.Anything)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Len(t, result, 2)
}

// TestListWorkspaceSessions_CRDFetchFails_ReturnsDBValuesFailOpen
// verifies that a CRD fetch failure (K8s API error, network issue) is
// fail-open: the DB values are returned as-is without error. The
// context_used values remain NULL but the session list still renders.
func TestListWorkspaceSessions_CRDFetchFails_ReturnsDBValuesFailOpen(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return((*v1.Workspace)(nil), assertError("crd fetch failed"))

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "CRD fetch failure must be fail-open")
	require.Len(t, result, 1)
	assert.Nil(t, result[0].ContextUsed, "contextUsed stays NULL when CRD is unreachable")
}

// TestListWorkspaceSessions_CRDHasNoSessionData verifies that a CRD
// with no per-session status (e.g., a freshly created workspace where
// agentd hasn't reported yet) doesn't cause errors.
func TestListWorkspaceSessions_CRDHasNoSessionData(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 0, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Nil(t, result[0].ContextUsed, "contextUsed stays NULL when CRD has no session data")
}

// TestListWorkspaceSessions_BackfillPersistsToDB verifies that
// backfilled values are persisted to the DB so subsequent calls skip
// the CRD fetch. This is the self-healing behavior.
func TestListWorkspaceSessions_BackfillPersistsToDB(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			Sessions: []v1.AgentSessionStatus{
				{ID: "ses_a", Status: "idle", ContextUsed: 42335},
			},
		},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	si.On("UpsertContextUsed", mock.Anything, "ws-1", "ses_a", int64(42335)).Return(nil)

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.NotNil(t, result[0].ContextUsed)
	assert.Equal(t, int64(42335), *result[0].ContextUsed)

	si.AssertCalled(t, "UpsertContextUsed", mock.Anything, "ws-1", "ses_a", int64(42335))
}

// TestListWorkspaceSessions_BackfillUpsertFails_NoError verifies that
// a DB persistence failure during backfill doesn't cause the API to
// error — the enriched value is still returned to the caller.
func TestListWorkspaceSessions_BackfillUpsertFails_NoError(t *testing.T) {
	f := newFixture(t)
	si := &mockSessionIndex{}
	f.svc.SetSessionIndex(si)

	f.db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{
		ID: "ws-1", UserID: "user-1",
	}, nil)

	sessionsFromDB := []types.SessionListItem{
		{ID: "ses_a", Title: "A", MessageCount: 5, Status: "idle"},
	}
	si.On("ListByWorkspace", mock.Anything, "ws-1").Return(sessionsFromDB, nil)

	crd := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			Sessions: []v1.AgentSessionStatus{
				{ID: "ses_a", Status: "idle", ContextUsed: 42335},
			},
		},
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)
	si.On("UpsertContextUsed", mock.Anything, "ws-1", "ses_a", int64(42335)).Return(assertError("db write failed"))

	result, err := f.svc.ListWorkspaceSessions(context.Background(), "user-1", "ws-1")
	require.NoError(t, err, "UpsertContextUsed failure must not cause API error")
	require.Len(t, result, 1)
	require.NotNil(t, result[0].ContextUsed, "enriched value still returned even if persist failed")
}
