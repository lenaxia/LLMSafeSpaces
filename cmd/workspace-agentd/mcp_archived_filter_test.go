// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1735: archived sessions are opt-in on the listing surfaces ------
//
// session_list and session_metadata(all_sessions) default to
// non-archived sessions only; include_archived:true is the explicit
// opt-in. A single-ID metadata lookup resolves an archived session
// regardless (the filter applies to listings/aggregates only).
//
// The pins ride the dispatch wire (callMCPTool) so they compile — and
// fail RED — against the pre-change tool surface: the dispatcher today
// ignores include_archived entirely and both tools ship every session.

// setupArchivedFilterEnv builds the mixed workspace: two live sessions
// plus one archived, with the platform fake answering the archived-set
// endpoint. Returns (live1, live2, archivedID).
func setupArchivedFilterEnv(t *testing.T) (string, string, string) {
	t.Helper()
	f := newFakeAgent()
	live1 := f.newSession("live one")
	live2 := f.newSession("live two")
	archived := f.newSession("dead weight")
	withAgentServer(t, f.handler(t))
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/internal/v1/session-archived", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"archived":["` + archived + `"]}`))
	}))
	t.Cleanup(platform.Close)
	setupMetadataEnv(t, platform)
	return live1, live2, archived
}

// sessionIDs decodes the session_list raw array to its ids.
func sessionIDs(t *testing.T, body string) []string {
	t.Helper()
	var elems []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &elems))
	ids := make([]string, 0, len(elems))
	for _, e := range elems {
		ids = append(ids, e.ID)
	}
	return ids
}

type metadataResult struct {
	Sessions []struct {
		SessionID string `json:"session_id"`
		Archived  bool   `json:"archived"`
	} `json:"sessions"`
}

func TestSessionList_DefaultExcludesArchived(t *testing.T) {
	live1, live2, archived := setupArchivedFilterEnv(t)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_list", map[string]any{})
	require.NoError(t, err)
	ids := sessionIDs(t, out)
	assert.Len(t, ids, 2, "default listing excludes archived sessions (#1735): expected only the two live sessions")
	assert.Contains(t, ids, live1)
	assert.Contains(t, ids, live2)
	assert.NotContains(t, ids, archived)
}

func TestSessionList_IncludeArchivedOptIn(t *testing.T) {
	_, _, archived := setupArchivedFilterEnv(t)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_list", map[string]any{
		"include_archived": true,
	})
	require.NoError(t, err)
	ids := sessionIDs(t, out)
	assert.Len(t, ids, 3, "include_archived:true restores the every-session behavior")
	assert.Contains(t, ids, archived, "the dispatcher must feed include_archived into session_list")
}

func TestSessionMetadata_AllSessions_DefaultExcludesArchived(t *testing.T) {
	live1, live2, archived := setupArchivedFilterEnv(t)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_metadata", map[string]any{
		"all_sessions": true,
	})
	require.NoError(t, err)
	var res metadataResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Sessions, 2, "the all_sessions aggregate defaults to non-archived (#1735): expected only the two live sessions")
	got := []string{res.Sessions[0].SessionID, res.Sessions[1].SessionID}
	assert.Contains(t, got, live1)
	assert.Contains(t, got, live2)
	assert.NotContains(t, got, archived)
}

func TestSessionMetadata_AllSessions_IncludeArchivedOptIn(t *testing.T) {
	_, _, archived := setupArchivedFilterEnv(t)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_metadata", map[string]any{
		"all_sessions":     true,
		"include_archived": true,
	})
	require.NoError(t, err)
	var res metadataResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Sessions, 3, "include_archived:true includes the archived session in the aggregate")
	for _, s := range res.Sessions {
		if s.SessionID == archived {
			assert.True(t, s.Archived, "the archived entry still carries its archived marker")
		}
	}
}

// The filter applies to LISTINGS/aggregates only: a single-ID lookup of
// an archived session must keep resolving (read-only history stays
// addressable for session_metadata itself).
func TestSessionMetadata_ArchivedSingleIDResolves(t *testing.T) {
	_, _, archived := setupArchivedFilterEnv(t)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_metadata", map[string]any{
		"session_id": archived,
	})
	require.NoError(t, err)
	var res metadataResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Sessions, 1, "an explicit archived session_id resolves — no flag needed")
	assert.Equal(t, archived, res.Sessions[0].SessionID)
	assert.True(t, res.Sessions[0].Archived)
}

// --- handler-level pins (direct signatures) -----------------------------

func TestMCPSessionList_HandlerDefaultsAndOptIn(t *testing.T) {
	_, _, archived := setupArchivedFilterEnv(t)

	out, err := mcpSessionList(context.Background(), mcpTestPassword, false)
	require.NoError(t, err)
	assert.NotContains(t, sessionIDs(t, out), archived)

	out, err = mcpSessionList(context.Background(), mcpTestPassword, true)
	require.NoError(t, err)
	assert.Contains(t, sessionIDs(t, out), archived)
}

// The advisory posture (#1627 §4): a platform fetch failure must never
// fail or silently shrink the listing — with no archived set in hand
// the filter degrades to a no-op.
func TestMCPSessionList_PlatformUnreachable_ListsAll(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	f.newSession("two")
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)
	t.Setenv("LLMSAFESPACE_API_URL", "http://127.0.0.1:1")

	out, err := mcpSessionList(context.Background(), mcpTestPassword, false)
	require.NoError(t, err, "the archived filter is advisory — an unreachable platform must not fail session_list")
	assert.Len(t, sessionIDs(t, out), 2)
}

// Byte-exact preservation: the default filter must DROP archived
// entries without rewriting the survivors (the raw shape is the tool's
// output contract). Deterministic unit seam — the archived set is
// pre-seeded into the package cache (no platform fetch to race).
func TestMCPSessionList_FilterPreservesElementBytes(t *testing.T) {
	archivedSetCacheMu.Lock()
	archivedSetCacheVal = map[string]bool{"ses_dead": true}
	archivedSetCacheAt = time.Now()
	archivedSetCacheMu.Unlock()

	body := `[` + strings.Join([]string{
		`{"id":"ses_live_1","title":"a","unknownField":{"nested":[1,2]}}`,
		`{"id":"ses_dead","title":"b"}`,
		`{"id":"ses_live_2","title":"c","time":{"created":1789274402332}}`,
	}, ",") + `]`

	filtered := filterArchivedFromSessionList(context.Background(), []byte(body))
	rawFiltered := splitJSONArray(t, filtered)
	assert.Equal(t, []string{
		`{"id":"ses_live_1","title":"a","unknownField":{"nested":[1,2]}}`,
		`{"id":"ses_live_2","title":"c","time":{"created":1789274402332}}`,
	}, rawFiltered, "the filtered body is the verbatim body minus the archived element, byte-exact — unknown fields intact")
}

// splitJSONArray splits a JSON array body into its raw element texts
// (whitespace-trimmed fragments between top-level commas).
func splitJSONArray(t *testing.T, body string) []string {
	t.Helper()
	var elems []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &elems))
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		out = append(out, string(e))
	}
	return out
}
