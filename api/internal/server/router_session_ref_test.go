// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// The #1663 regression ids (live evidence): 30-char live id vs the
// one-char-truncated variant the caller's model re-typed.
const (
	refLiveID  = "ses_ed9183a46ffeRiNw2PddpL5dAA"
	refTruncID = "ses_ed9183a46ffeRiNw2PddpL5dA"
	refShortID = "amber-falcon-glides"
	refShortUp = "Amber-Falcon-Glides"
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

	// #1757: the workspace-ref layer probes the (non-UUID, non-phrase)
	// :id form against the name index before the access gate — the
	// fixture's ws-1 returns no match (passthrough).
	db.On("FindWorkspaceIDByName", mock.Anything, "test-user", "ws-1").Return("", nil).Maybe()

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

	// Ruling 8's exact row shape on the ambiguity payload too (review
	// r1 finding 3): title/shortId/sessionId/workspace/lastActive —
	// the workspace is the route's own, never "".
	var body struct {
		Error       string             `json:"error"`
		Suggestions []types_Suggestion `json:"suggestions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Contains(t, body.Error, "ambiguous")
	require.Len(t, body.Suggestions, 2, "both colliding sessions must be offered")
	for _, s := range body.Suggestions {
		assert.Equal(t, "ws-1", s.Workspace, "workspace must be the route's own id")
		// #1756 §1: the payload's re-copy affordance is the PHRASE;
		// the harness id never serializes.
		assert.NotEmpty(t, s.ShortID)
	}
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
		{ID: refShortID, InternalID: refLiveID, ShortID: refShortID, Title: "Swift Heron — follow-up"},
		{ID: "linen-orchard-chirps", InternalID: refOtherID, ShortID: "linen-orchard-chirps", Title: "Board reconciler"},
	}

	t.Run("title_substring", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=heron")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, refShortID, got[0].ID, "the wire id IS the phrase (#1756 short-only emission)")
	})

	t.Run("short_id_prefix", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=amber-falcon")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, refShortID, got[0].ID)
	})

	t.Run("legacy_id_prefix", func(t *testing.T) {
		router, ws, _ := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		// legacy-id prefix: matched via the INTERNAL harness key; the
		// emitted row is still the phrase.
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q="+refOtherID[:12])
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got []types.SessionListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Len(t, got, 1)
		assert.Equal(t, "linen-orchard-chirps", got[0].ID)
	})

	t.Run("id_like_miss_carries_suggestions", func(t *testing.T) {
		router, ws, db := newRouterFixtureWithDB(t)
		ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Once()
		// near-miss short id (one substitution) must be suggested
		db.On("ListSessionIndex", mock.Anything, "ws-1").Return(list, nil).Once()
		rec := doJSON(t, router, http.MethodGet, "/api/v1/workspaces/ws-1/sessions?q=amber-falcon-glided")
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		var body struct {
			Error       string             `json:"error"`
			Suggestions []types_Suggestion `json:"suggestions"`
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

// types_Suggestion mirrors the suggestion payload row (design 0066 §4.6).
type types_Suggestion struct {
	Title      string     `json:"title"`
	ShortID    string     `json:"shortId"`
	Workspace  string     `json:"workspace"`
	LastActive *time.Time `json:"lastActive"`
}

// Review r2 finding B's pins: a DEGRADED alias lookup (DB error) never
// lets title text reach the write paths — seam-invalid refs answer the
// honest availability 503 (no suggestions, no ghost-row mint), while
// seam-charset-valid refs keep the passthrough degrade.
func TestSessionRef_DegradedAliasLookup_SeamInvalidRefAnswers503(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	// The alias lookup errors (transient DB failure)…
	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "swift heron follow-ups").
		Return(nil, assert.AnError).Once()
	// …and NO write may happen: MarkSessionSeen is never expected.

	rec := doJSON(t, router, http.MethodPut,
		"/api/v1/workspaces/ws-1/sessions/swift%20heron%20follow-ups/seen")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "suggestions", "a 5xx never suggests")
	ws.AssertNotCalled(t, "MarkSessionSeen", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestSessionRef_DegradedAliasLookup_CharsetValidRefStillPassesThrough(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	// Degraded lookup on a seam-charset-valid ref (the "ses-two"
	// class): passthrough must survive — the seam keeps the verdict.
	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "ses-two").
		Return(nil, assert.AnError).Once()
	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", "ses-two").Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/ses-two/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	ws.AssertExpectations(t)
}

// Pins the /archived route's suggestions-bearing 404s: the
// middleware's alias-miss 404 (this test) and the service's
// unindexed-session 404 (TestArchivedRoute_Unindexed404CarriesSuggestions
// beside it — #1773: the r3 companion comment wrongly claimed this
// half was already pinned). The archived READ gate itself is the
// typed 409 session_archived with no suggestions, as documented.
func TestArchivedRoute_AliasMissCarriesSuggestions(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindSessionsByAlias", mock.Anything, "ws-1", "sunny-wolf").Return(nil, nil).Once()
	db.On("ListSessionIndex", mock.Anything, "ws-1").Return([]types.SessionListItem{
		{ID: refLiveID, ShortID: refShortID, Title: "Sunny Wolf — #1729 lane"},
	}, nil).Once()
	db.On("ListWorkspaceIDsByOwner", mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	body := `{"archived":true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sunny-wolf/archived", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"suggestions"`, "the archived route's alias-miss 404 carries the payload")
}

// TestArchivedRoute_Unindexed404CarriesSuggestions is #1745's missing
// pin (#1773): an id-form ref that passes through the middleware and
// hits SetSessionArchived's update-only NotFound answers the
// structured 404 WITH suggestions — never the bare respondWithError
// body. A regression in this branch (dropping the
// suggestions key, or passing the wrong workspace to SuggestionsFor)
// previously passed the entire suite.
func TestArchivedRoute_Unindexed404CarriesSuggestions(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	// Legacy-form (ses_ prefix): the middleware passes through with no
	// alias lookup — the service's update-only NotFound is the verdict.
	ghost := "ses_gho00000000000000000A"
	ws.On("SetSessionArchived", mock.Anything, "test-user", "ws-1", ghost, true).
		Return(apierrors.NewNotFoundError("session", ghost, nil)).Once()
	// SuggestionsFor ranks the workspace's live rows: a one-edit
	// neighbor must be offered. Post-#1756 row shape (finding 8):
	// the id-leg ranks against InternalID — a phrase-keyed rank
	// never fires for a ses_-shaped input and the payload goes
	// silent.
	db.On("ListSessionIndex", mock.Anything, "ws-1").Return([]types.SessionListItem{
		{ID: "amber-falcon-glides", InternalID: "ses_gho00000000000000000B", ShortID: "amber-falcon-glides", Title: "Ghost Neighbor"},
	}, nil).Once()
	db.On("ListWorkspaceIDsByOwner", mock.Anything, mock.Anything).Return(nil, nil).Maybe()

	body := `{"archived":true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+ghost+"/archived", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"suggestions"`, "the unindexed-session 404 must carry the did-you-mean payload")
	// #1756 §1: the payload's re-copy affordance is the PHRASE; the
	// harness id appears only inside the service's error TEXT (an
	// input echo, not an identifier field) — and the CANDIDATE's
	// harness id never leaks into the payload.
	assert.Contains(t, rec.Body.String(), "amber-falcon-glides", "the neighbor's PHRASE is the re-copy affordance")
	assert.NotContains(t, rec.Body.String(), "ses_gho00000000000000000B",
		"the candidate's harness id must not leak — the phrase is the only copyable id")
	ws.AssertExpectations(t)
}

// #1756 §2 (post-union review finding 1): a ses_ full-id input is the
// PRIMARY deprecated form — the response must carry the notice:
// Deprecation: true plus an RFC 8594 HTTP-date Sunset (free text is
// not a valid Sunset). The pre-remediation tree flagged DeprecatedInput
// only on a RESOLVING hash id; this pin holds the passthrough flag.
func TestSessionRef_LegacyInputCarriesDeprecationNotice(t *testing.T) {
	router, ws, _ := newRouterFixtureWithDB(t)

	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refLiveID+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "true", rec.Header().Get("Deprecation"),
		"a ses_ full-id input must attach the one-release notice")
	sunset := rec.Header().Get("Sunset")
	assert.NotEmpty(t, sunset, "Sunset header must be present on deprecated input")
	if _, err := http.ParseTime(sunset); err != nil {
		t.Fatalf("Sunset must be an RFC 8594 HTTP-date, got %q: %v", sunset, err)
	}
}

// #1756 §2 (post-union review finding 3): the #1745 hash form is a
// deprecated input EVEN ON THE MISS PATH — an oc-… ref that fails to
// resolve must still passthrough with the notice attached (the flag
// lives on the resolution, not the happy path).
func TestSessionRef_HashMissPassthroughCarriesDeprecationNotice(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	missHash := "oc-deadbeef"
	// The hash misses the index: passthrough byte-identical.
	db.On("FindSessionByShortID", mock.Anything, "ws-1", missHash).Return("", nil).Once()
	// The adapter then arbitrates the unknown id: not found.
	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", missHash).
		Return(apierrors.NewNotFoundError("session", missHash, nil)).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+missHash+"/seen")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, "true", rec.Header().Get("Deprecation"),
		"an oc- hash input is deprecated input even when it misses")
	sunset := rec.Header().Get("Sunset")
	assert.NotEmpty(t, sunset, "Sunset header must be present on deprecated input")
	if _, err := http.ParseTime(sunset); err != nil {
		t.Fatalf("Sunset must be an RFC 8594 HTTP-date, got %q: %v", sunset, err)
	}
}

// #1816: a RESOLVED ref (a phrase or alias REWRITE — never the
// byte-identical deprecated-id passthrough, which resolves nothing)
// tells the caller which canonical id it mapped to via
// X-Session-Ref-Resolved. The platform MCP client consumes it as one
// key of its dual-space contract-stream match — post-#1824 frames are
// phrase-keyed on refreshed pods (harness additive) and harness-keyed
// on heal-window/legacy pods — so a single-space filter would burn the
// wait timeout on one regime or the other. Correlation for a caller
// that just addressed the session under authz — not a new
// default-output surface (#1756 §1 governs session-bearing payload
// bodies; this rides the same response the caller already receives).
func TestSessionRef_ResolvedRefCarriesCanonicalHeader(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	t.Run("phrase", func(t *testing.T) {
		db.On("FindSessionByShortID", mock.Anything, "ws-1", refShortUp).Return(refLiveID, nil).Once()
		ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

		rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refShortUp+"/seen")
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, refLiveID, rec.Header().Get("X-Session-Ref-Resolved"),
			"a phrase-addressed rewrite must surface the canonical id")
	})

	t.Run("alias", func(t *testing.T) {
		db.On("FindSessionsByAlias", mock.Anything, "ws-1", "resolve-me").Return([]types.SessionListItem{
			{ID: refLiveID, ShortID: refShortID, Title: "resolve-me"},
		}, nil).Once()
		ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

		rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/resolve-me/seen")
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, refLiveID, rec.Header().Get("X-Session-Ref-Resolved"),
			"an alias-addressed rewrite must surface the canonical id")
	})
}

// #1816: a byte-identical passthrough (the caller already used the
// canonical id) sets no header — the bridge only reports a RESOLUTION.
func TestSessionRef_UnresolvedPassthroughCarriesNoHeader(t *testing.T) {
	router, ws, _ := newRouterFixtureWithDB(t)

	ws.On("MarkSessionSeen", mock.Anything, "test-user", "ws-1", refLiveID).Return(nil).Once()

	rec := doJSON(t, router, http.MethodPut, "/api/v1/workspaces/ws-1/sessions/"+refLiveID+"/seen")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("X-Session-Ref-Resolved"),
		"a canonical-id input resolves nothing; no header")
}
