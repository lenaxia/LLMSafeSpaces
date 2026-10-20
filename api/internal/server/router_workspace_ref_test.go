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

// #1757 router pins: the workspace phrase is the canonical :id
// address (resolved BEFORE the access gate), UUIDs pass through
// byte-identical, and unambiguous names rewrite during the
// deprecation window. Phrase misses answer the structured 404.

func doGet(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestWorkspaceRef_PhraseResolvesBeforeAccessGate(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByPhrase", mock.Anything, "amber-river-7").Return("11111111-1111-1111-1111-111111111111", nil).Once()
	// The access gate + handler see the UUID, not the phrase.
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "11111111-1111-1111-1111-111111111111").
		Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/amber-river-7/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestWorkspaceRef_PhraseMissIsStructured404(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByPhrase", mock.Anything, "opal-meadow-9").Return("", nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/opal-meadow-9/sessions")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "workspace not found")
}

func TestWorkspaceRef_UUIDPassesThroughByteIdentical(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	uuid := "11111111-1111-1111-1111-111111111111"
	// No phrase lookup for a UUID (the shape guard skips the probe);
	// no name lookup either.
	db.AssertNotCalled(t, "FindWorkspaceIDByPhrase", mock.Anything, mock.Anything)
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", uuid).Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/"+uuid+"/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestWorkspaceRef_UnambiguousNameRewrites(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByName", mock.Anything, "test-user", "my-space").Return("11111111-1111-1111-1111-111111111111", nil).Once()
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "11111111-1111-1111-1111-111111111111").Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/my-space/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
