// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// buildHistoryFixture returns an opencode-shaped /session/{id}/message
// payload (oldest-first array) of N alternating user/assistant messages.
// IDs are zero-padded so lexical order matches creation order.
func buildHistoryFixture(n int) string {
	msgs := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"info": map[string]any{
				"role": role,
				"id":   fmt.Sprintf("msg_%04d", i),
				"time": map[string]any{"created": 1000 + i},
			},
			"parts": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("body %d", i)},
			},
		})
	}
	b, _ := json.Marshal(msgs)
	return string(b)
}

// Sanity: extract message IDs from a response body.
func extractIDs(t *testing.T, body []byte) []string {
	t.Helper()
	// Contract response shape (#828 batch 2): the adapter path returns
	// typed session.Message JSON — top-level id per element (the legacy
	// passthrough served opencode's nested info.id).
	var arr []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &arr))
	ids := make([]string, len(arr))
	for i, m := range arr {
		ids[i] = m.ID
	}
	return ids
}

// TestGetHistory_FirstPage_DefaultLimit50 asserts that GET .../message
// with no query params returns at most 50 messages (the newest 50,
// oldest-first within the page) and sets X-Next-Cursor when more remain.
//
// This is the contract the frontend (useMessageHistory + getHistoryPage in
// api/messages.ts) relies on. Before the fix, the handler is a dumb
// pass-through to opencode and returns ALL 84 messages with no header.
func TestGetHistory_FirstPage_DefaultLimit50(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upstream should be called WITHOUT pagination params — those are
		// API-server concerns and must not be forwarded to opencode.
		assert.NotContains(t, r.URL.RawQuery, "before=",
			"before must not be forwarded to the agent")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(84)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET", "/api/v1/workspaces/ws-1/sessions/ses_1/message", nil)
	require.Equal(t, http.StatusOK, w.Code)

	ids := extractIDs(t, w.Body.Bytes())
	require.Len(t, ids, 50, "first page should contain exactly default limit messages")

	// Oldest-first within the page; the page should be the NEWEST 50
	// (indices 34..83 of the upstream array).
	assert.Equal(t, "msg_0034", ids[0], "first id in page = oldest of newest-50")
	assert.Equal(t, "msg_0083", ids[len(ids)-1], "last id in page = newest message")

	// Cursor for the next (older) page is the OLDEST id in the current page.
	assert.Equal(t, "msg_0034", w.Header().Get("X-Next-Cursor"),
		"X-Next-Cursor must be the oldest id of the returned page")
}

// TestGetHistory_FirstPage_FewerThanLimit_NoCursor asserts no cursor header
// is emitted when the entire history fits in one page.
func TestGetHistory_FirstPage_FewerThanLimit_NoCursor(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(10)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET", "/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	require.Len(t, ids, 10)
	assert.Equal(t, "msg_0000", ids[0])
	assert.Equal(t, "msg_0009", ids[len(ids)-1])
	assert.Empty(t, w.Header().Get("X-Next-Cursor"),
		"no cursor when all history fits in one page")
}

// TestGetHistory_BeforeCursor_ReturnsOlderPage walks backwards through
// history using ?before=<oldestIdOfPreviousPage>. The contract:
//   - response contains messages strictly OLDER than the cursor
//   - response is oldest-first within the page
//   - X-Next-Cursor advances backwards (oldest id of the new page)
func TestGetHistory_BeforeCursor_ReturnsOlderPage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(84)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	// Second page: before=msg_0034 should give msg_0000..msg_0033 (34 messages).
	w := env.do("GET",
		"/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50&before=msg_0034", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	require.Len(t, ids, 34, "second page should be the remaining 34 messages")
	assert.Equal(t, "msg_0000", ids[0])
	assert.Equal(t, "msg_0033", ids[len(ids)-1])

	// No cursor — we've reached the oldest message.
	assert.Empty(t, w.Header().Get("X-Next-Cursor"),
		"no cursor at the start of history")
}

// TestGetHistory_LimitCappedAtMax verifies the server caps absurd ?limit
// values at the documented maximum (200) to bound memory.
func TestGetHistory_LimitCappedAtMax(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(500)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET",
		"/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=99999", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	assert.Len(t, ids, 200, "limit must be capped at 200")
	assert.Equal(t, "msg_0300", ids[0], "page = newest 200, oldest-first")
	assert.Equal(t, "msg_0499", ids[len(ids)-1])
	assert.Equal(t, "msg_0300", w.Header().Get("X-Next-Cursor"))
}

// TestGetHistory_InvalidLimit_Rejected covers limit=0, negative, and
// non-numeric values — all should yield 400 to surface client bugs early
// rather than silently substituting defaults.
func TestGetHistory_InvalidLimit_Rejected(t *testing.T) {
	cases := []string{"limit=0", "limit=-5", "limit=abc"}
	for _, qs := range cases {
		t.Run(qs, func(t *testing.T) {
			env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("upstream should not be called for invalid request; got %s", r.URL.String())
			})
			env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
			env.setupPasswordWithT(t, "ws-1", "test-password")
			env.setupWorkspaceWithT(t, "ws-1", 5)

			w := env.doRequestWithT(t, "GET",
				"/api/v1/workspaces/ws-1/sessions/ses_1/message?"+qs, nil)
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"invalid limit must be rejected before contacting upstream")
		})
	}
}

// TestGetHistory_BeforeCursor_NotFound_ReturnsEmpty when the supplied
// cursor doesn't appear in the upstream message list, return an empty
// array and no X-Next-Cursor (degenerate but well-defined behavior).
func TestGetHistory_BeforeCursor_NotFound_ReturnsEmpty(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(10)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET",
		"/api/v1/workspaces/ws-1/sessions/ses_1/message?before=msg_does_not_exist", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	assert.Empty(t, ids, "unknown cursor returns empty page")
	assert.Empty(t, w.Header().Get("X-Next-Cursor"))
}

// TestGetHistory_FiltersNonDisplayableBeforePaginating asserts that
// the server's page size accounts for displayable messages only —
// system-role messages and messages with no text/tool/thinking parts
// must be filtered BEFORE counting against the limit. Otherwise users
// see jumpy page sizes.
func TestGetHistory_PageIsRawSlice_TranslateAfterwards(t *testing.T) {
	// 5 displayable + 5 non-displayable, interleaved.
	// Non-displayable: role=system, or parts contain only step-start/step-finish.
	upstream := []map[string]any{
		{"info": map[string]any{"role": "user", "id": "msg_0000"},
			"parts": []map[string]any{{"type": "text", "text": "hi"}}},
		{"info": map[string]any{"role": "system", "id": "msg_0001"},
			"parts": []map[string]any{{"type": "text", "text": "sys"}}},
		{"info": map[string]any{"role": "assistant", "id": "msg_0002"},
			"parts": []map[string]any{{"type": "step-start"}, {"type": "step-finish"}}},
		{"info": map[string]any{"role": "assistant", "id": "msg_0003"},
			"parts": []map[string]any{{"type": "text", "text": "reply"}}},
		{"info": map[string]any{"role": "user", "id": "msg_0004"},
			"parts": []map[string]any{{"type": "text", "text": "next"}}},
		{"info": map[string]any{"role": "system", "id": "msg_0005"},
			"parts": []map[string]any{{"type": "text", "text": "sys2"}}},
		{"info": map[string]any{"role": "assistant", "id": "msg_0006"},
			"parts": []map[string]any{{"type": "text", "text": "again"}}},
		{"info": map[string]any{"role": "assistant", "id": "msg_0007"},
			"parts": []map[string]any{{"type": "step-start"}}},
		{"info": map[string]any{"role": "user", "id": "msg_0008"},
			"parts": []map[string]any{{"type": "text", "text": "more"}}},
		{"info": map[string]any{"role": "assistant", "id": "msg_0009"},
			"parts": []map[string]any{{"type": "text", "text": "final"}}},
	}
	body, _ := json.Marshal(upstream)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	// ADAPTER CONTRACT (#828 batch 2 port; differs from the deleted
	// legacy tail, which filtered non-displayable messages BEFORE
	// slicing): the page is the newest 3 RAW messages; the translator
	// then drops step-start/step-finish PARTS (a step-marker-only
	// message surfaces as an empty-parts contract message) and system
	// roles are contract data. The legacy filter-first behavior died
	// with the tail; surfaced in the PR for review.
	w := env.do("GET",
		"/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=3", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	require.Len(t, ids, 3, "page must contain 3 messages (raw slice)")
	assert.Equal(t, []string{"msg_0007", "msg_0008", "msg_0009"}, ids,
		"newest 3 raw; msg_0007 (step-start-only) surfaces with empty parts")
	assert.Equal(t, "msg_0007", w.Header().Get("X-Next-Cursor"))
}

// TestGetHistory_UpstreamError_DoesNotMaskAsEmptyPage was deleted with GetHistory's legacy tail (#828 batch 2): the legacy fetch-error path died with fetchUpstreamHistory; the adapter equivalent is TestGetHistory_AdapterPath_Error_Returns502.

// TestGetHistory_LimitAndBeforeStrippedFromForwardedQuery was deleted with GetHistory's legacy tail (#828 batch 2): the upstream query-forwarding contract belonged to the deleted legacy tail; the adapter owns the upstream query now (page-1 forwards a native limit, never a cursor — asserted in TestE2E_MessageHistory_PaginationFullWalk).

// TestGetHistory_EmptySession_ReturnsEmptyArrayNotNull pins the response
// shape for a brand-new session with zero messages: the body must be the
// JSON literal `[]` (not `null`, not the empty string), because the
// frontend's transformHistory calls .filter() on the parsed result. A
// `null` body would throw at the parse + filter step.
func TestGetHistory_EmptySession_ReturnsEmptyArrayNotNull(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET", "/api/v1/workspaces/ws-1/sessions/ses_1/message", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := strings.TrimSpace(w.Body.String())
	assert.Equal(t, "[]", body, "empty session body must be the JSON literal [] (not null, not empty)")
	assert.Empty(t, w.Header().Get("X-Next-Cursor"))

	// Parse-and-filter equivalent of what the frontend does — must not throw.
	ids := extractIDs(t, w.Body.Bytes())
	assert.Empty(t, ids)
}

// TestGetHistory_ExactLimitBoundary_NoCursor guards the off-by-one at the
// page edge: when the total number of displayable messages equals the
// requested limit, all messages must be returned in one page and no
// X-Next-Cursor must be emitted (there is no older page to fetch).
//
// Regression target: if the cursor-suppression condition is ever changed
// from `start > 0` to `start >= 0` (or vice versa), this test catches the
// off-by-one immediately.
func TestGetHistory_ExactLimitBoundary_EmitsOptimisticCursor(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(buildHistoryFixture(50)))
	}))
	defer backend.Close()
	env := newE2EEnv(t, backend)

	w := env.do("GET",
		"/api/v1/workspaces/ws-1/sessions/ses_1/message?limit=50", nil)
	require.Equal(t, http.StatusOK, w.Code)
	ids := extractIDs(t, w.Body.Bytes())
	require.Len(t, ids, 50, "total displayable == limit must return one full page")
	assert.Equal(t, "msg_0000", ids[0])
	assert.Equal(t, "msg_0049", ids[len(ids)-1])
	// #971 optimistic-cursor contract (supersedes the legacy boundary
	// rule): a FULL native page may hide older messages, so the handler
	// optimistically emits the page's oldest id. The spurious cursor
	// costs one back-page fetch that returns empty and no further
	// cursor — the fake upstream ignores ?limit, so this fixture's page
	// is exactly-full and the optimistic cursor fires by design.
	assert.Equal(t, "msg_0000", w.Header().Get("X-Next-Cursor"),
		"full native page emits the optimistic #971 cursor")
}

// TestGetHistory_RetriesOnStaleIP was deleted with GetHistory's legacy tail (#828 batch 2): the transport retry lived in doHistoryRequest; the generic stale-IP retry contract is pinned on the seam (TestProxy_RetriesOnStaleIP).
