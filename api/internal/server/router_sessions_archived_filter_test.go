// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- GET /api/v1/workspaces/:id/sessions archived filter (#1735) -------
//
// Archived sessions are opt-in on the listing endpoint: excluded by
// default, included only on the explicit include_archived=true query
// flag. Single-session lookups resolve archived sessions regardless.

func mixedArchivedSessions() []types.SessionListItem {
	return []types.SessionListItem{
		{ID: "sess-live-1", Title: "Live chat", MessageCount: 5},
		{ID: "sess-arch-1", Title: "Shelved lane", MessageCount: 40, Archived: true},
		{ID: "sess-live-2", Title: "Another live", MessageCount: 1},
	}
}

func TestListWorkspaceSessions_DefaultExcludesArchived(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		mixedArchivedSessions(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 2, "default listing excludes archived sessions (#1735)")
	assert.Equal(t, "sess-live-1", items[0].ID)
	assert.Equal(t, "sess-live-2", items[1].ID)
}

func TestListWorkspaceSessions_IncludeArchivedParam(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		mixedArchivedSessions(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions?include_archived=true", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	require.Len(t, items, 3, "include_archived=true restores the every-session behavior")
	assert.True(t, items[1].Archived, "the archived entry still carries its marker when included")
}

func TestListWorkspaceSessions_ExplicitFalseExcludesArchived(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		mixedArchivedSessions(), nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions?include_archived=false", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
	assert.Len(t, items, 2, "explicit include_archived=false is the same as the default")
}

// Review r1 missing-case 5: an all-archived workspace under the default
// filter must render an EMPTY ARRAY, not a null body — the frontend and
// SDK consumers index into the slice.
func TestListWorkspaceSessions_AllArchived_DefaultIsEmptyArray(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{
			{ID: "sess-arch-1", Title: "Shelved lane", Archived: true},
			{ID: "sess-arch-2", Title: "Older lane", Archived: true},
		}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", rec.Body.String(), "the empty listing must be an empty array, not null")

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions?include_archived=true", nil)
	req2.Header.Set("Authorization", "Bearer token")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
	var items []types.SessionListItem
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &items))
	assert.Len(t, items, 2, "the opt-in still returns every archived row")
}
