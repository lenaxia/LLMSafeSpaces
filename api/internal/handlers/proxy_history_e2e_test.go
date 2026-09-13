// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_MessageHistory_PaginationFullWalk reproduces the production
// failure mode observed against session ses_0f01dd6f1ffe8awjS68zzWTjI5:
//
//   - opencode stores 84 messages for the session
//   - frontend issues GET /sessions/{id}/message?limit=50
//   - BEFORE FIX: server returns all 84 messages, no X-Next-Cursor header,
//     so the frontend never renders 'Load earlier messages'. Users with
//     long sessions are unable to scroll past whatever the auto-scroll
//     window happens to render.
//   - AFTER FIX: server returns the newest 50 (oldest-first within page)
//     with X-Next-Cursor=msg_0034. A second call with ?before=msg_0034
//     returns the remaining 34 with no cursor.
//
// This drives the FULL gin router and the FULL handler stack — only the
// opencode upstream and Kubernetes API are faked. If this passes, the
// browser UX works.
func TestE2E_MessageHistory_PaginationFullWalk(t *testing.T) {
	const total = 84
	const pageSize = 50

	// Build the upstream fixture once; reuse across the two requests
	// the test will make.
	upstream := buildHistoryFixture(total)

	// Ported to the adapter harness (#828 batch 2): GetHistory is
	// adapter-only; the fixture serves opencode-shaped JSON the adapter
	// parses into contract messages. The cursor contract is unchanged.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The API owns pagination semantics: `before` is NEVER forwarded
		// to the agent. `limit` IS forwarded on the first-page native
		// fetch (#971 — GetHistoryPage's agent-side pagination); the
		// cursor itself stays API-owned.
		assert.NotContains(t, r.URL.RawQuery, "before=")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstream))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	// ---- Page 1: GET .../message?limit=50 ----
	w1 := env.do("GET",
		fmt.Sprintf("/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=%d", pageSize), nil)
	require.Equal(t, http.StatusOK, w1.Code)

	page1 := extractIDs(t, w1.Body.Bytes())
	require.Len(t, page1, pageSize, "page 1 should be exactly the newest %d messages", pageSize)
	// Oldest-first within the page; newest message is the last element.
	assert.Equal(t, "msg_0034", page1[0], "page 1 starts at index 34 (84 - 50)")
	assert.Equal(t, "msg_0083", page1[pageSize-1], "page 1 ends at the newest message")

	cursor := w1.Header().Get("X-Next-Cursor")
	require.NotEmpty(t, cursor, "X-Next-Cursor required for the next page")
	require.Equal(t, "msg_0034", cursor)

	// ---- Page 2: GET .../message?limit=50&before=<cursor> ----
	w2 := env.do("GET",
		fmt.Sprintf("/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=%d&before=%s", pageSize, cursor), nil)
	require.Equal(t, http.StatusOK, w2.Code)

	page2 := extractIDs(t, w2.Body.Bytes())
	require.Len(t, page2, total-pageSize, "page 2 should contain the remaining %d messages", total-pageSize)
	assert.Equal(t, "msg_0000", page2[0], "page 2 starts at the oldest message")
	assert.Equal(t, "msg_0033", page2[len(page2)-1], "page 2 ends just before the cursor")
	assert.Empty(t, w2.Header().Get("X-Next-Cursor"),
		"no cursor at the beginning of history")

	// ---- Union check: pages cover the entire history with no gaps/duplicates ----
	combined := append(append([]string{}, page2...), page1...)
	require.Len(t, combined, total)
	for i := 0; i < total; i++ {
		assert.Equal(t, fmt.Sprintf("msg_%04d", i), combined[i],
			"chronological reconstruction must equal the original upstream order")
	}
}

// TestE2E_MessageHistory_FirstMessagePresent specifically guards the
// user-reported symptom: the FIRST user message of a long session must
// be reachable. Before the fix, the frontend stalled on page 1 (the
// last 50 messages) and msg_0000 was effectively orphaned.
func TestE2E_MessageHistory_FirstMessagePresent(t *testing.T) {
	upstream := buildHistoryFixture(84)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(upstream))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	// Walk all the way back to the start.
	var seen []string
	cursor := ""
	for hop := 0; hop < 10; hop++ {
		path := "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50"
		if cursor != "" {
			path += "&before=" + cursor
		}
		w := env.do("GET", path, nil)
		require.Equal(t, http.StatusOK, w.Code)

		page := extractIDs(t, w.Body.Bytes())
		require.NotEmpty(t, page, "no empty pages while paginating; hop=%d", hop)
		// Prepend the page so `seen` ends up oldest-first.
		seen = append(page, seen...)

		cursor = w.Header().Get("X-Next-Cursor")
		if cursor == "" {
			break
		}
	}
	require.Empty(t, cursor, "pagination must terminate")
	require.Equal(t, 84, len(seen), "all messages must be reachable")

	// THE bug-replication assertion: the FIRST user message is the oldest
	// (index 0) — it must be the very first element after a full back-walk.
	require.NoError(t, json.Unmarshal([]byte(upstream), &[]json.RawMessage{}))
	assert.Equal(t, "msg_0000", seen[0], "the original first user message must be reachable")
}
