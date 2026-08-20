// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// VerifyDelivery tests (#987): the outbox ambiguity resolver for
// opencode. opencode persists the user message BEFORE the turn starts
// (prompt.ts createUserMessage, v1.18.10), so "text present in the
// transcript at/after the send-window start" proves delivery even while
// the turn is still running — and cursor-paged coverage past the window
// start proves absence definitively.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyPage is one canned page of history for the verify test server.
type verifyPage struct {
	items      string // JSON array body
	nextCursor string // X-Next-Cursor; "" = history exhausted
}

// newVerifyServer serves GET /session/:id/message?limit[&before=] from a
// page map keyed by cursor ("" = first page), asserting the limit param.
func newVerifyServer(t *testing.T, pages map[string]verifyPage, hits *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/message") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, pw, _ := r.BasicAuth(); pw != testPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("limit") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cursor := r.URL.Query().Get("before")
		*hits = append(*hits, cursor)
		page, ok := pages[cursor]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if page.nextCursor != "" {
			w.Header().Set("X-Next-Cursor", page.nextCursor)
		}
		_, _ = w.Write([]byte(page.items))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// userMsg builds a history-array entry with one text part.
func userMsg(role, text string, created time.Time) string {
	return fmt.Sprintf(`{"info":{"role":%q,"id":"msg_%d","time":{"created":%d}},"parts":[{"type":"text","text":%q}]}`,
		role, created.UnixMilli(), created.UnixMilli(), text)
}

func TestVerifyDelivery_PresentAfterWindowStart(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	srv := newVerifyServer(t, map[string]verifyPage{
		"": {items: "[" + userMsg("user", "the long turn text", since.Add(2*time.Minute)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "the long turn text", since)
	require.NoError(t, err)
	assert.True(t, delivered, "text persisted after the window start proves delivery")
	assert.True(t, definitive)
}

func TestVerifyDelivery_OlderOnlyIsAbsentDefinitive(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	srv := newVerifyServer(t, map[string]verifyPage{
		"": {items: "[" + userMsg("user", "unrelated", since.Add(-30*time.Minute)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "the long turn text", since)
	require.NoError(t, err)
	assert.False(t, delivered)
	assert.True(t, definitive, "oldest page entry predates the window: absence is proven, safe to re-send")
}

func TestVerifyDelivery_PagesThroughCursor(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	srv := newVerifyServer(t, map[string]verifyPage{
		"":      {items: "[" + userMsg("user", "newer noise", since.Add(10*time.Minute)) + "]", nextCursor: "cur-1"},
		"cur-1": {items: "[" + userMsg("user", "paged match", since.Add(1*time.Minute)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "paged match", since)
	require.NoError(t, err)
	assert.True(t, delivered, "match found on the second page (before=cur-1)")
	assert.True(t, definitive)
	require.Len(t, hits, 2, "exactly two pages fetched")
	assert.Equal(t, "cur-1", hits[1], "second page carries the cursor")
}

func TestVerifyDelivery_CoverageIncompleteIsInconclusive(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	// Endless newer-than-window pages with cursors: coverage never
	// reaches the window start within the page budget.
	pages := map[string]verifyPage{}
	for i := 0; i < verifyPageBudget+5; i++ {
		pages[fmt.Sprintf("cur-%d", i)] = verifyPage{
			items:      "[" + userMsg("user", "noise", since.Add(time.Duration(i+1)*time.Minute)) + "]",
			nextCursor: fmt.Sprintf("cur-%d", i+1),
		}
	}
	pages[""] = verifyPage{items: "[" + userMsg("user", "newest", since.Add(2*time.Hour)) + "]", nextCursor: "cur-0"}
	var hits []string
	srv := newVerifyServer(t, pages, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "never present", since)
	require.NoError(t, err)
	assert.False(t, delivered)
	assert.False(t, definitive, "budget exhausted before covering the window: NOT proven absent — recheck, never re-send")
}

func TestVerifyDelivery_IdenticalTextBeforeWindowNotMatched(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	// The exact text exists — but from BEFORE this entry's send window
	// (beyond skew): an earlier, already-completed delivery of the same
	// words must not satisfy THIS entry's verification.
	srv := newVerifyServer(t, map[string]verifyPage{
		"": {items: "[" + userMsg("user", "same words", since.Add(-30*time.Minute)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "same words", since)
	require.NoError(t, err)
	assert.False(t, delivered, "pre-window identical text does not count")
	assert.True(t, definitive)
}

func TestVerifyDelivery_ClockSkewMargin(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	// Agent-pod clock trails the API by 90s (< the 2m skew margin): the
	// persisted message timestamps just before our window start.
	srv := newVerifyServer(t, map[string]verifyPage{
		"": {items: "[" + userMsg("user", "skewed clock", since.Add(-90*time.Second)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, _, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "skewed clock", since)
	require.NoError(t, err)
	assert.True(t, delivered, "within the skew margin the match counts")
}

func TestVerifyDelivery_AssistantTextDoesNotCount(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var hits []string
	srv := newVerifyServer(t, map[string]verifyPage{
		"": {items: "[" + userMsg("assistant", "echo of user text", since.Add(1*time.Minute)) + "]"},
	}, &hits)
	a := newTestAdapter(t, srv)

	delivered, definitive, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "echo of user text", since)
	require.NoError(t, err)
	assert.False(t, delivered, "only USER messages prove delivery")
	assert.True(t, definitive)
}

func TestVerifyDelivery_TransportErrorIsInconclusive(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	a := newTestAdapter(t, srv)

	_, _, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "anything", since)
	require.Error(t, err, "transport/5xx failures surface as errors — the caller treats them as inconclusive")
}

func TestVerifyDelivery_LimitParamSent(t *testing.T) {
	since := time.Now().Add(-1 * time.Hour)
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	a := newTestAdapter(t, srv)

	_, _, err := a.VerifyDelivery(context.Background(), "u-1", "ws-1", "ses_1", "x", since)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(verifyPageSize), gotLimit, "the verifier pages with the tuned page size")
}
