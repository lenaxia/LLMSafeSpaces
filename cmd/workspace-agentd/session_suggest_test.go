// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// --- #1663: near-miss session-id suggestions --------------------------------
//
// The incident: a caller's model re-typed a live 30-char session id with
// one character dropped (TWO different 29-char variants were emitted
// across attempts — a fixed-width clip cannot do that; model emission
// can). Every platform surface (agentd create response, opencode store,
// list, by-ID GET, fromSession sentinels) carried the full id; the bare
// "not found" error gave the caller no way to know a near-identical
// LIVE session existed — an hour was lost diagnosing a "zombie" session
// that was addressable the whole time. These pins hold the did-you-mean
// layer that converts that failure into an immediate self-correction.

// incidentID is the affected session's full id (byte-exact from the
// store, the raw GET /session list, and the persisted create_session
// tool-result part). The two drop variants below are the exact 29-char
// forms the caller emitted (persisted tool-input bytes).
const (
	incidentID        = "ses_ed9183a46ffeRiNw2PddpL5dDA" // len 30
	incidentDropFinal = "ses_ed9183a46ffeRiNw2PddpL5dD"  // len 29: final 'A' dropped
	incidentDropMid   = "ses_ed9183a46ffeRiNw2PddpL5dA"  // len 29: penultimate 'D' dropped
	otherID           = "ses_ed926c8bdffe83lZoDugWebm5V" // a real sibling (distance ≫ 2)
)

func TestSuggestSessionID_SingleCharDrop_Variants(t *testing.T) {
	ids := []string{otherID, incidentID}
	for _, variant := range []string{incidentDropFinal, incidentDropMid} {
		got := suggestSessionID(variant, ids)
		assert.Contains(t, got, incidentID, "drop variant %q (len %d) must surface the live id", variant, len(variant))
		assert.Contains(t, got, "did you mean", "the clause must read as a did-you-mean")
		assert.Contains(t, got, "1 character", "the edit count must be stated")
	}
}

// TestSuggestSessionID_BoundaryLengths pins the edit-distance-1 window
// around the fleet's 30-char id length in BOTH directions (29→30 drop,
// 31→30 spurious insert) plus the same-length substitution — the
// boundary the issue suspected of a fixed-width clip (±1 char).
func TestSuggestSessionID_BoundaryLengths(t *testing.T) {
	cases := []struct {
		name      string
		live      string
		requested string
	}{
		{"drop 30→29", incidentID, incidentDropFinal},
		{"insert 31→30", incidentID, incidentID + "Q"},
		{"substitute 30→30", incidentID, "ses_ed9183a46ffeRiNw2PddpL5dDX"},
		{"live 29, requested 28", "ses_ed8a10cceffeKXcf8mSy139y", "ses_ed8a10cceffeKXcf8mSy139y"[:27]},
		{"live 31, requested 31", incidentID + "Q", incidentID + "R"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestSessionID(tc.requested, []string{otherID, tc.live})
			assert.Contains(t, got, tc.live)
		})
	}
}

func TestSuggestSessionID_DistanceBounds(t *testing.T) {
	// Exactly 2 edits: still suggested (two adjacent drops happen).
	twoEdits := incidentID[:len(incidentID)-2]
	assert.Len(t, twoEdits, 28)
	assert.Contains(t, suggestSessionID(twoEdits, []string{incidentID}), incidentID, "distance 2 is within the bound")

	// Exactly 3 edits: silent.
	threeEdits := incidentID[:len(incidentID)-3]
	assert.Empty(t, suggestSessionID(threeEdits, []string{incidentID}), "distance 3 must not suggest")

	// Unrelated id of the same length: silent (distance ≫ 2).
	assert.Empty(t, suggestSessionID(incidentID, []string{otherID}))
}

func TestSuggestSessionID_ExactAndEmpty(t *testing.T) {
	assert.Empty(t, suggestSessionID(incidentID, []string{incidentID, otherID}),
		"an id that RESOLVES gets no suggestion — the caller errored elsewhere")
	assert.Empty(t, suggestSessionID("", []string{incidentID}), "empty request: silent")
	assert.Empty(t, suggestSessionID(incidentDropFinal, nil), "no inventory: silent, never a hard failure")
	assert.Empty(t, suggestSessionID(incidentDropFinal, []string{""}), "blank inventory rows are skipped")
}

func TestSuggestSessionID_AmbiguityNamesNoSingleGuess(t *testing.T) {
	// Two live ids both at distance 1 from the request: a single
	// did-you-mean would be a coin flip — the clause must NOT pick one.
	a := "ses_ed9183a46ffeRiNw2PddpL5dDA"
	b := "ses_ed9183a46ffeRiNw2PddpL5dDB"
	got := suggestSessionID("ses_ed9183a46ffeRiNw2PddpL5dD", []string{b, a})
	assert.Contains(t, got, "2 workspace sessions", "the ambiguity must be stated with the count")
	assert.Contains(t, got, a)
	assert.Contains(t, got, b)
	assert.NotContains(t, got, "did you mean", "no single-candidate phrasing on a tie")
}

func TestSuggestSessionID_Deterministic(t *testing.T) {
	ids := []string{otherID, "ses_ed9183a46ffeRiNw2PddpL5dDB", incidentID, "ses_ed9183a46ffeRiNw2PddpL5dD"}
	first := suggestSessionID(incidentDropFinal, ids)
	second := suggestSessionID(incidentDropFinal, ids)
	assert.Equal(t, first, second, "same input, same clause")
}

func TestIsSessionNotFoundErr(t *testing.T) {
	assert.True(t, isSessionNotFoundErr(&stubErr{"GET /session/x/message: status 404: {\"name\":\"NotFoundError\"}"}))
	assert.True(t, isSessionNotFoundErr(&stubErr{"POST /session/x/abort returned 404: nope"}))
	assert.False(t, isSessionNotFoundErr(&stubErr{"GET /session: status 502: bad gateway"}))
	assert.False(t, isSessionNotFoundErr(&stubErr{"Get \"http://x\": connection refused"}))
	assert.False(t, isSessionNotFoundErr(nil))
}

type stubErr struct{ msg string }

func (e *stubErr) Error() string { return e.msg }

// --- tool-level pins: the incident surfaces ---------------------------------

// withIncidentAgent serves a fake opencode holding the incident session
// (full id only — byte-faithful to the live store) and 404s every
// by-ID probe for ids not in it: the exact resolution behavior the
// caller's mistyped sends hit.
func withIncidentAgent(t *testing.T) *httptest.Server {
	t.Helper()
	known := map[string]bool{incidentID: true, otherID: true}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != mcpTestPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := r.URL.Path
		if r.Method == http.MethodGet && path == "/session" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": incidentID, "title": "Worker: #1648 mobile viewport fix", "agent": "build", "version": "1.18.15"},
				{"id": otherID, "title": "Worker: #1646 SSE staleness fix", "agent": "build", "version": "1.18.15"},
			})
			return
		}
		if r.Method == http.MethodGet && path == "/session/status" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasPrefix(path, "/session/") {
			id := strings.Split(strings.TrimPrefix(path, "/session/"), "/")[0]
			if !known[id] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"name":"NotFoundError","data":{"message":"Session not found: ` + id + `"}}`))
				return
			}
			if r.Method == http.MethodGet && strings.HasSuffix(path, "/message") {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(handler)
	withAgentServer(t, handler.ServeHTTP)
	return srv
}

// TestSendMessage_MistypedID_SuggestsLiveID is THE incident pin: a
// single-character-dropped id must fail (no auto-correct — nothing is
// ever delivered on a guess) AND name the live id it shadows.
func TestSendMessage_MistypedID_SuggestsLiveID(t *testing.T) {
	withIncidentAgent(t)
	_, err := mcpSendMessage(context.Background(), mcpTestPassword, incidentDropMid, "status ping", otherID, "")
	require.Error(t, err, "a mistyped id must NEVER auto-deliver")
	msg := err.Error()
	assert.Contains(t, msg, incidentDropMid, "the failing id is still named")
	assert.Contains(t, msg, incidentID, "the live near-miss id must be suggested")
	assert.Contains(t, msg, "did you mean")
}

// TestSendMessage_OriginMistyped_AlsoSuggests: the origin half of the
// by-ID validation gets the same clause (a mistyped from-id is the same
// slip class).
func TestSendMessage_OriginMistyped_AlsoSuggests(t *testing.T) {
	withIncidentAgent(t)
	_, err := mcpSendMessage(context.Background(), mcpTestPassword, incidentID, "hello", incidentDropFinal, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestSessionRead_MistypedID_SuggestsLiveID(t *testing.T) {
	withIncidentAgent(t)
	_, err := mcpSessionRead(context.Background(), mcpTestPassword, incidentDropMid, 5)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read session")
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestSessionMetadata_MistypedID_SuggestsLiveID(t *testing.T) {
	withIncidentAgent(t)
	_, err := mcpSessionMetadata(context.Background(), mcpTestPassword, incidentDropFinal, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestAbortSession_MistypedID_SuggestsLiveID(t *testing.T) {
	withIncidentAgent(t)
	_, err := mcpAbortSession(context.Background(), mcpTestPassword, incidentDropMid)
	require.Error(t, err)
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestSessionArchive_MistypedID_SuggestsLiveID(t *testing.T) {
	// Platform answers 404 (index lookup by the mistyped id); the pod's
	// live list still holds the near id — the clause must ride along.
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer platform.Close()
	setupPodSessionEnv(t, platform)
	withIncidentAgent(t)
	_, err := mcpSessionArchive(context.Background(), mcpTestPassword, incidentDropFinal, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestDeleteSession_MistypedID_SuggestsLiveID(t *testing.T) {
	// Sixth wired surface: delete refuses on the platform 404 (never
	// acts on a guess) and names the live id. No injected session and
	// no busy fallback → the self-delete guard is inert by design.
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer platform.Close()
	setupPodSessionEnv(t, platform)
	withIncidentAgent(t)
	_, err := mcpDeleteSession(context.Background(), mcpTestPassword, incidentDropMid, "")
	require.Error(t, err, "a mistyped id must NEVER delete on a guess")
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestRenameSession_MistypedID_SuggestsLiveID(t *testing.T) {
	// Seventh wired surface (r1 finding 1): rename is the same incident
	// class — an id-taking tool whose 404 previously read as a bare
	// failure. Refused either way; the clause names the live id.
	withIncidentAgent(t)
	_, err := mcpRenameSession(context.Background(), mcpTestPassword, incidentDropFinal, "New title")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to rename session")
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

func TestCompactIdle_MistypedID_SuggestsLiveID(t *testing.T) {
	// Eighth wired surface (r1 finding 1): the synchronous idle
	// compaction path. Explicit model skips the list-based model
	// resolution so the mistyped id reaches the summarize 404; the
	// session is idle in the fake (statuses empty).
	withIncidentAgent(t)
	_, err := mcpCompact(context.Background(), mcpTestPassword, incidentDropMid, "p/text")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compaction failed")
	assert.Contains(t, err.Error(), incidentID)
	assert.Contains(t, err.Error(), "did you mean")
}

// TestSessionSuggestClause_ListFailureStaysSilent: the suggestion fetch
// is best-effort — when the list endpoint itself fails, the failure path
// must degrade to the legacy bare error, never add noise or block.
func TestSessionSuggestClause_ListFailureStaysSilent(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			mu.Lock()
			listCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := opencode.NewLoopbackClient(srv.URL, mcpTestPassword)
	assert.Empty(t, sessionSuggestClause(context.Background(), client, incidentDropMid))
	assert.Equal(t, 1, listCalls, "exactly one bounded list fetch per suggestion")
}

// TestSessionRead_ServerError_NoSuggestion: only NOT-FOUND gets the
// clause. A 5xx (server trouble, not a typo) must keep the bare error —
// a suggestion there would train callers to ignore real failures (the
// isSessionNotFoundErr guard against an always-match mutation).
func TestSessionRead_ServerError_NoSuggestion(t *testing.T) {
	withAgentServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := mcpSessionRead(context.Background(), mcpTestPassword, incidentDropMid, 5)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "did you mean", "a 502 is not a typo — no suggestion")
}
