// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// #1481: CountMessages is the reconcile pass's count source — a bounded
// paged walk of the V1 message route (?limit&before= + X-Next-Cursor),
// mirroring the loopback client's SessionMessageCount contract so the
// cursor knowledge stays entirely inside the seam. The pinned 1.18.15
// session list carries no count field (verified live, #1452 lane), so
// the walk IS the cheapest source; the 40-page ceiling keeps a
// pathological session bounded.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// countWalkServer serves the V1 message route with scripted pages. In
// scripted mode the i-th request returns pages[i] (a full page carries
// X-Next-Cursor when more scripted pages remain); in alwaysCursor mode
// every response is a full 500-page with a fresh cursor — the
// pathological never-ending history the ceiling must bound.
func countWalkServer(t *testing.T, pages []int, alwaysCursor bool) (*httptest.Server, *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := len(got)
		got = append(got, r.URL.RequestURI())
		if alwaysCursor {
			writeCountPage(w, countWalkPageSize, fmt.Sprintf("cur-%d", i+1), true)
			return
		}
		n := pages[i]
		writeCountPage(w, n, fmt.Sprintf("cur-%d", i+1), i < len(pages)-1)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func writeCountPage(w http.ResponseWriter, n int, cursor string, withCursor bool) {
	body := make([]string, n)
	for i := range body {
		body[i] = `{"id":"m","info":{"id":"m"},"parts":[]}`
	}
	if cursor != "" && withCursor {
		w.Header().Set("X-Next-Cursor", cursor)
	}
	_, _ = w.Write([]byte("[" + strings.Join(body, ",") + "]"))
}

func TestAdapter_CountMessages_PagesThroughCursor(t *testing.T) {
	srv, got := countWalkServer(t, []int{500, 7}, false)
	a := newTestAdapter(t, srv)

	count, err := a.CountMessages(context.Background(), "", "ws-1", "ses_c1")
	require.NoError(t, err)
	require.Equal(t, 507, count)
	require.Len(t, *got, 2, "exactly two page GETs")
	require.Contains(t, (*got)[1], "before=cur-1", "the continuation page carries the cursor query param")
}

func TestAdapter_CountMessages_ShortPageStops(t *testing.T) {
	srv, got := countWalkServer(t, []int{12}, false)
	a := newTestAdapter(t, srv)

	count, err := a.CountMessages(context.Background(), "", "ws-1", "ses_c2")
	require.NoError(t, err)
	require.Equal(t, 12, count)
	require.Len(t, *got, 1, "a short page with no cursor ends the walk")
}

func TestAdapter_CountMessages_CeilingBoundsPathologicalSessions(t *testing.T) {
	srv, got := countWalkServer(t, []int{500}, true) // always full + always a cursor
	a := newTestAdapter(t, srv)

	count, err := a.CountMessages(context.Background(), "", "ws-1", "ses_c3")
	require.ErrorIs(t, err, agent.ErrMessageCountTruncated,
		"a ceiling-exhausted walk is a floor, not ground truth — the rebuild must skip the row, never persist an undercount")
	require.Equal(t, 500*40, count, "the floor is still returned for display-only callers")
	require.Len(t, *got, 40, "no infinite pagination walk")
}

func TestAdapter_CountMessages_SessionGoneIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	a := newTestAdapter(t, srv)

	_, err := a.CountMessages(context.Background(), "", "ws-1", "ses_gone1")
	require.ErrorIs(t, err, agent.ErrSessionNotFound, "a vanished session surfaces the typed gone error the #1479 machinery classifies")
}
