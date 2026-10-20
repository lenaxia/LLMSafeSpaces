// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
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
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", uuid).Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/"+uuid+"/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// r1: asserted AFTER ServeHTTP — a pre-flight AssertNotCalled is
	// trivially true and verifies nothing.
	db.AssertNotCalled(t, "FindWorkspaceIDByPhrase", mock.Anything, mock.Anything)
	db.AssertNotCalled(t, "FindWorkspaceIDByName", mock.Anything, mock.Anything, mock.Anything)
}

func TestWorkspaceRef_UnambiguousNameRewrites(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByName", mock.Anything, "test-user", "my-space").Return("11111111-1111-1111-1111-111111111111", nil).Once()
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "11111111-1111-1111-1111-111111111111").Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/my-space/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// r1: the name form is in its #1757 deprecation window — the
	// rewrite attaches the one-release notice on the response.
	assert.Equal(t, "true", rec.Header().Get("Deprecation"))
	assert.NotEmpty(t, rec.Header().Get("Sunset"), "the Sunset header must point at the phrase form")
}

// r1 §4.3 masking: a phrase that resolves to a REAL but FOREIGN
// workspace answers the byte-identical 404 the unknown phrase gets —
// Forbidden is not-found-equivalent on the phrase surface, or the
// distinction enumerates the fleet's live phrase space. Also pins
// that the backfill NEVER runs for a denied caller (post-authz only).
// dropCheckOwnership removes the fixture's permissive CheckOwnership
// stand-in so a per-test Forbidden verdict can register cleanly
// (testify matches the first registered expectation — the fixture's
// Maybe() would otherwise always win).
func dropCheckOwnership(ws *imocks.MockWorkspaceService) {
	kept := ws.ExpectedCalls[:0]
	for _, e := range ws.ExpectedCalls {
		if e.Method != "CheckOwnership" {
			kept = append(kept, e)
		}
	}
	ws.ExpectedCalls = kept
}

func TestWorkspaceRef_ForeignPhraseMasksAsByteIdentical404(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)
	dropCheckOwnership(ws)

	db.On("FindWorkspaceIDByPhrase", mock.Anything, "amber-river-7").Return("99999999-9999-9999-9999-999999999999", nil).Once()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).
		Return(apierrors.NewForbiddenError("workspace access denied", errors.New("foreign"))).Once()

	rec := doGet(t, router, "/api/v1/workspaces/amber-river-7/sessions")
	require.Equal(t, http.StatusNotFound, rec.Code,
		"foreign phrase must answer the masked 404, not 403: %s", rec.Body.String())
	assert.JSONEq(t, `{"error":"workspace not found: no workspace holds the phrase amber-river-7"}`,
		rec.Body.String(), "byte-identical to the unknown-phrase body (§4.3)")
	ws.AssertNotCalled(t, "EnsureWorkspacePhraseRegistered", mock.Anything, mock.Anything,
		"a denied caller must never trigger the backfill write")
}

// Shape precedence (r1 robustness): a ref shaped word-word-digit is a
// PHRASE, always — even when the caller owns a workspace literally
// NAMED that way. The phrase arm resolves; the name index is never
// consulted for the same ref.
func TestWorkspaceRef_PhraseShapeWinsOverName(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)
	dropCheckOwnership(ws)

	db.On("FindWorkspaceIDByPhrase", mock.Anything, "solar-valley-3").Return("99999999-9999-9999-9999-999999999999", nil).Once()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).
		Return(apierrors.NewForbiddenError("workspace access denied", errors.New("foreign"))).Once()

	rec := doGet(t, router, "/api/v1/workspaces/solar-valley-3/sessions")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	db.AssertNotCalled(t, "FindWorkspaceIDByName", mock.Anything, mock.Anything, mock.Anything,
		"a phrase-shaped ref must never reach the name index")
}

// 5xx-never-suggests (r1): a degraded phrase lookup answers the
// availability 503 — an infra fault is never a confident miss.
func TestWorkspaceRef_PhraseLookupErrorIs503(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByPhrase", mock.Anything, "amber-river-7").
		Return("", errors.New("db unavailable")).Once()

	rec := doGet(t, router, "/api/v1/workspaces/amber-river-7/sessions")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"degraded lookup must 503, not 404: %s", rec.Body.String())
}

func TestWorkspaceRef_NameLookupErrorIs503(t *testing.T) {
	router, _, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByName", mock.Anything, "test-user", "my-space").
		Return("", errors.New("db unavailable")).Once()

	rec := doGet(t, router, "/api/v1/workspaces/my-space/sessions")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"degraded name lookup must 503, not a passthrough miss: %s", rec.Body.String())
}

// Ambiguity never resolves (r1): an absent/ambiguous name passes
// through UNREWRITTEN — the gate and handler see the raw name and the
// ordinary 404 flow keeps its truth (no suggestions on the workspace
// surface).
func TestWorkspaceRef_AmbiguousNamePassesThroughUnrewritten(t *testing.T) {
	router, ws, db := newRouterFixtureWithDB(t)

	db.On("FindWorkspaceIDByName", mock.Anything, "test-user", "my-space").Return("", nil).Once()
	// The gate + handler see the RAW name (never rewritten, never
	// guessed): ListWorkspaceSessions is called with "my-space".
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "my-space").
		Return([]types.SessionListItem{}, nil).Once()

	rec := doGet(t, router, "/api/v1/workspaces/my-space/sessions")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
