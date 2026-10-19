// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// The #1663 regression ids (live evidence): 30-char live id vs the
// one-char-truncated variant the caller's model re-typed.
const (
	refLiveID  = "ses_ed9183a46ffeRiNw2PddpL5dAA"
	refTruncID = "ses_ed9183a46ffeRiNw2PddpL5dA"
	refShortID = "oc-ab12cd34"
	refShortUp = "OC-AB12CD34"
	refOtherID = "ses_eaef7aee4ffem2ma1jE6ox9Edq"
)

// newRouterFixtureWithDB is newRouterFixture with a database service
// mock wired (design 0066's resolution layer is backed by it).
func newRouterFixtureWithDB(t *testing.T) (*gin.Engine, *imocks.MockWorkspaceService, *imocks.MockDatabaseService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}
	db := &imocks.MockDatabaseService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()

	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	svc := &mockServices{auth: auth, metrics: met, workspace: ws, database: db}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})
	return router, ws, db
}

func doJSON(t *testing.T, router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Full legacy IDs address byte-identically: the resolver NEVER
// rewrites, never DB-gates, never even consults the index for a
// ses_-form ref (backward compat is absolute).
func TestSessionRef_LegacyIDByteIdentical(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refLiveID+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	ws.AssertExpectations(t)
	db.AssertNotCalled(t, "FindSessionByShortID", mock.Anything, mock.Anything, mock.Anything)
	db.AssertNotCalled(t, "FindSessionsByAlias", mock.Anything, mock.Anything, mock.Anything)
}

// The #1663 class: a one-char-truncated id must NOT silently resolve
// to a wrong session — it passes through byte-identical (nothing
// "repaired" it anywhere) and the downstream surface arbitrates.
func TestSessionRef_TruncatedIDNeverResolved(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	// The truncated id arrives at the service EXACTLY as typed.
	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refTruncID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refTruncID+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	db.AssertNotCalled(t, "FindSessionByShortID", mock.Anything, mock.Anything, mock.Anything)
	db.AssertNotCalled(t, "FindSessionsByAlias", mock.Anything, mock.Anything, mock.Anything)
}

// Short-id acceptance: case-insensitive, resolved to the canonical
// harness id before the service sees it.
func TestSessionRef_ShortIDCaseInsensitive(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindSessionByShortID", mock.Anything, "ws-1", refShortUp).Return(refLiveID, nil).Once()
	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refShortUp+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// Alias happy path: an unambiguous current title resolves.
func TestSessionRef_AliasUnambiguous(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "worker: #1691").Return([]types.SessionListItem{
		{ID: refLiveID, ShortID: refShortID, Title: "Worker: #1691"},
	}, nil).Once()
	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut,
		"/api/v1/workspaces/ws-1/sessions/"+strings.ReplaceAll("worker: #1691", " ", "%20")+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// Alias collision: two live sessions share the title — NEVER
// auto-resolve; the caller gets 404 + both as suggestions.
func TestSessionRef_AliasCollisionRefusesWithDidYouMean(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "mighty-harbor").Return([]types.SessionListItem{
		{ID: "ses_new", ShortID: "oc-aaaaaaaa", Title: "mighty-harbor"},
		{ID: "ses_old", ShortID: "oc-bbbbbbbb", Title: "mighty-harbor"},
	}, nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/mighty-harbor/seen")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var body struct {
		Error       string                   `json:"error"`
		Suggestions []map[string]interface{} `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "ambiguous")
	require.Len(t, body.Suggestions, 2, "both colliding sessions must be offered")
}

// Unresolvable input answers suggestions, not silence.
func TestSessionRef_UnresolvableAliasSuggests(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "sunny-wolf").Return(nil, nil).Once()
	db.On("ListSessionIndex", mock.Anything, "ws-1").Return([]types.SessionListItem{
		{ID: refLiveID, ShortID: refShortID, Title: "Sunny Wolf — #1729 lane"},
	}, nil).Once()
	// Own-workspace token overlap scores, so the cross-workspace
	// expansion must NOT fire (the pin asserts the scope boundary).
	db.On("ListWorkspaceIDsByOwner", mock.Anything, mock.Anything).
		Return(nil, nil).Maybe()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sunny-wolf/seen")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	var body struct {
		Error       string          `json:"error"`
		Suggestions json.RawMessage `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "session not found", body.Error)
	assert.NotEmpty(t, string(body.Suggestions), "the payload must not be silent")
}

// The q filter: substring/prefix matching over the same list, and an
// id-like miss carries the suggestions payload (ruling 8's one
// response shape). No q ⇒ the unfiltered list.
func TestSessionsQFilter(t *testing.T) {
	list := []types.SessionListItem{
		{ID: refLiveID, ShortID: refShortID, Title: "Swift Heron — follow-up"},
		{ID: refOtherID, ShortID: "oc-22222222", Title: "Board reconciler"},
	}

	t.Run("title_substring", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=heron")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, refLiveID, got[0].ID)
		assert.Equal(t, refShortID, got[0].ShortID, "list rows carry the short id")
	})

	t.Run("short_id_prefix", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=oc-ab12")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, refLiveID, got[0].ID)
	})

	t.Run("legacy_id_prefix", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q="+refOtherID[:12])
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, refOtherID, got[0].ID)
	})

	t.Run("id_like_miss_carries_suggestions", func(t *testing.T) {
		router, ws, db := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		// near-miss short id (one substitution) must be suggested
		db.On("ListSessionIndex", mock.Anything, "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=oc-ab12cd35")
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		var body struct {
			Error       string       `json:"error"`
			Suggestions []types_Sugg `json:"suggestions"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "session not found", body.Error)
		require.Len(t, body.Suggestions, 1)
		assert.Equal(t, refShortID, body.Suggestions[0].ShortID)
	})

	t.Run("no_q_is_bare_array", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "["),
			"the no-q listing stays a bare array (byte-identical shape)")
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Len(t, got, 2)
	})
}

// types_Sugg mirrors the suggestion payload row (design 0066 §4.6).
type types_Sugg struct {
	Title     string `json:"title"`
	ShortID   string `json:"shortId"`
	SessionID string `json:"sessionId"`
	Workspace string `json:"workspace"`
}
