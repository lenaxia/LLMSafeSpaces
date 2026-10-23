// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// --- #1879: archived-set degradation must never be silent ------------------
//
// The archived set behind session_list/session_metadata is index-only
// state fetched through a ~15s pod cache. Past the freshness window
// (platform outage) the filter degrades to the unfiltered list —
// correct per #1735 review r1 finding 1 (stale data must not keep
// hiding rows) — but the degradation was INVISIBLE: callers saw a
// normal-looking listing with archived sessions back in it, and
// re-archived already-archived rows ("archives not sticking", the
// 2026-10-23 03:30–04:45Z incident).
//
// This file pins the three disclosure/repair halves of the fix:
//
//  1. session_list stamps every row `archived_set_stale:true` when the
//     default filter could not prove freshness (the unfiltered list is
//     KNOWN-unfiltered, never silent). include_archived:true listings
//     take no marker — they are unfiltered by request.
//  2. session_metadata carries a top-level `archived_set_stale:true`
//     when the archived annotations behind the result cannot prove
//     freshness; absent = fresh.
//  3. session_archive, after a successful platform write, force-reads
//     the archived set back and reports `archived_visibility`:
//     "verified" (the fresh set agrees — and the pod's own cache is
//     updated, so this pod's listings converge immediately instead of
//     waiting out the TTL) or "unverified" (confirmation read failed
//     or disagreed — the state still holds, but listings may lag). A
//     failed confirmation never fails the tool and never fabricates
//     cache freshness.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staleEnv builds the warm-cache-outage shape: a working harness seam,
// an UNREACHABLE platform, and a cache whose last successful fetch is
// beyond the TTL (the incident's degraded window — deterministic, no
// fetch to race).
func staleEnv(t *testing.T, archivedIDs ...string) {
	t.Helper()
	f := newFakeAgent()
	for _, id := range archivedIDs {
		f.newSession(id)
	}
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)
	t.Setenv("LLMSAFESPACE_API_URL", "http://127.0.0.1:1")
	set := map[string]bool{}
	for _, id := range archivedIDs {
		set[id] = true
	}
	seedArchivedCache(t, set, time.Now().Add(-2*archivedSetCacheTTL))
}

// --- 1. session_list staleness marker --------------------------------------

func TestSessionList_StaleArchivedSet_StampedUnfiltered(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)
	t.Setenv("LLMSAFESPACE_API_URL", "http://127.0.0.1:1")
	seedArchivedCache(t, map[string]bool{s1: true}, time.Now().Add(-2*archivedSetCacheTTL))

	out, err := mcpSessionList(context.Background(), mcpTestPassword, false)
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 2, "the outage degrade is still the unfiltered list (#1735 r1 f1)")
	for _, row := range rows {
		assert.Equal(t, true, row["archived_set_stale"],
			"every row of a degraded listing carries the staleness marker (#1879): the caller must know archived rows may be present")
	}
}

func TestSessionList_FreshArchivedSet_NoMarker(t *testing.T) {
	live1, _, archived := setupArchivedFilterEnv(t) // platform fake answers fresh

	out, err := mcpSessionList(context.Background(), mcpTestPassword, false)
	require.NoError(t, err)
	assert.NotContains(t, out, "archived_set_stale",
		"a fresh-set listing carries no marker — absence must mean provably-fresh")
	ids := sessionIDs(t, out)
	assert.Len(t, ids, 2)
	assert.NotContains(t, ids, archived)
	assert.Contains(t, ids, live1)
}

func TestSessionList_IncludeArchivedStale_NeverStamped(t *testing.T) {
	staleEnv(t, "one")

	out, err := mcpSessionList(context.Background(), mcpTestPassword, true)
	require.NoError(t, err)
	assert.NotContains(t, out, "archived_set_stale",
		"include_archived:true is unfiltered BY REQUEST — no staleness marker rides it")
	assert.Len(t, sessionIDs(t, out), 1)
}

// Byte-exact stamping: the marker must be APPENDED without re-marshaling
// the row (the #1735 r2 f1 discipline — json.Marshal would compact and
// HTML-escape titles containing & <>; unknown fields must survive).
func TestStampArchivedSetStale_PreservesElementBytes(t *testing.T) {
	body := `[` + strings.Join([]string{
		`{ "id": "ses_live_1", "title": "Fix a & b <script>", "unknownField": { "nested": [1, 2] } }`,
		`{}`,
		`{ "id": "ses_live_2", "title": "c" }`,
	}, ", ") + `]`

	stamped := stampArchivedSetStale([]byte(body))
	assert.Equal(t, []string{
		`{ "id": "ses_live_1", "title": "Fix a & b <script>", "unknownField": { "nested": [1, 2] } ,"archived_set_stale":true}`,
		`{"archived_set_stale":true}`,
		`{ "id": "ses_live_2", "title": "c" ,"archived_set_stale":true}`,
	}, splitJSONArray(t, stamped),
		"the marker is spliced in before the closing brace — every original byte (internal whitespace, & <> unescaped, unknown fields) survives")
	assert.NotContains(t, stamped, `\u0026`)
	assert.NotContains(t, stamped, `\u003c`)
}

func TestStampArchivedSetStale_Malformed_PassesThrough(t *testing.T) {
	for _, body := range []string{
		`{"not":"an array"}`,
		`[`,
		`not even json`,
	} {
		assert.Equal(t, body, stampArchivedSetStale([]byte(body)),
			"a non-array body must pass through byte-identical — the stamp must never break the raw list contract")
	}
	assert.Equal(t, `[]`, stampArchivedSetStale([]byte(`[]`)),
		"an empty list stays empty — nothing to stamp")
}

// --- 2. session_metadata top-level marker ----------------------------------

type metadataStaleResult struct {
	Sessions []struct {
		SessionID string `json:"session_id"`
	} `json:"sessions"`
	ArchivedSetStale bool `json:"archived_set_stale"`
}

func TestSessionMetadata_StaleSet_TopLevelMarker(t *testing.T) {
	staleEnv(t, "one", "two")

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true, false)
	require.NoError(t, err)
	var res metadataStaleResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Sessions, 2, "the aggregate degrade is still the unfiltered set")
	assert.True(t, res.ArchivedSetStale,
		"a metadata result built on an unprovable archived set carries the top-level marker (#1879)")
}

func TestSessionMetadata_FreshSet_NoMarker(t *testing.T) {
	setupArchivedFilterEnv(t)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true, false)
	require.NoError(t, err)
	var res metadataStaleResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.False(t, res.ArchivedSetStale, "absent marker = provably-fresh archived annotations")
}

// --- 3. session_archive post-write verification -----------------------------

// archivePlatformFake answers the archive write AND the confirmation
// read. archivedSet controls the GET /internal/v1/session-archived body;
// confirmFail flips that endpoint to a 500 (the unreachable-platform
// confirmation shape).
func archivePlatformFake(t *testing.T, archivedSet []string, confirmFail bool) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	set := archivedSet
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			if confirmFail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			body, _ := json.Marshal(map[string]any{"archived": set})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func archiveCacheFreshness() (map[string]bool, bool) {
	archivedSetCacheMu.Lock()
	defer archivedSetCacheMu.Unlock()
	fresh := !archivedSetCacheFetchedAt.IsZero() && time.Since(archivedSetCacheFetchedAt) < archivedSetCacheTTL
	return archivedSetCacheVal, fresh
}

func TestMCPSessionArchive_VerifiedRefreshesCache(t *testing.T) {
	setupPodSessionEnv(t, archivePlatformFake(t, []string{"ses-1"}, false))

	out, err := mcpSessionArchive(context.Background(), mcpTestPassword, "ses-1", true)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "archived", res["status"])
	assert.Equal(t, "verified", res["archived_visibility"],
		"a confirmed write reports verified — the fresh archived set agrees (#1879)")

	set, fresh := archiveCacheFreshness()
	assert.True(t, fresh, "the confirmation read refreshes the pod's shared cache — same-pod listings converge immediately, not after the ~15s TTL")
	assert.True(t, set["ses-1"])
}

func TestMCPSessionArchive_UnarchiveVerifiedWhenSetLacksID(t *testing.T) {
	setupPodSessionEnv(t, archivePlatformFake(t, nil, false))

	out, err := mcpSessionArchive(context.Background(), mcpTestPassword, "ses-1", false)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "unarchived", res["status"])
	assert.Equal(t, "verified", res["archived_visibility"],
		"unarchive verifies against the id's ABSENCE from the fresh set")
}

func TestMCPSessionArchive_ConfirmationUnreachable_UnverifiedNotFailed(t *testing.T) {
	setupPodSessionEnv(t, archivePlatformFake(t, nil, true))

	out, err := mcpSessionArchive(context.Background(), mcpTestPassword, "ses-1", true)
	require.NoError(t, err, "a failed confirmation read never fails the tool — the platform write is the state truth")
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "archived", res["status"])
	assert.Equal(t, "unverified", res["archived_visibility"])

	_, fresh := archiveCacheFreshness()
	assert.False(t, fresh, "a failed confirmation read must NOT fabricate cache freshness (#1879)")
}

func TestMCPSessionArchive_ConfirmationMismatch_Unverified(t *testing.T) {
	// The write "succeeded" but the fresh set does not carry the id —
	// the exact "archives not sticking" shape from the incident.
	setupPodSessionEnv(t, archivePlatformFake(t, []string{"ses-other"}, false))

	out, err := mcpSessionArchive(context.Background(), mcpTestPassword, "ses-1", true)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "unverified", res["archived_visibility"],
		"a fresh set that disagrees with the write is reported, not masked")
}
