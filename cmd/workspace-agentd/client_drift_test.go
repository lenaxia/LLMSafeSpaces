// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// leg-10 wire-drift pins (epic-71 / leg10-pins, #1312 — review r1
// extended sweep) for the agentd OpenCodeClient's hand-adjacent parse
// sites: the four canonical corruption shapes (the #1308 class,
// mirroring pkg/abi/abitest.CorruptMode) riding a valid HTTP 200 must
// never parse as success. The listing decode is strict (drift fails
// the listing loudly); fetchSessionTitle stays best-effort by contract
// (a missing title must never fail the listing) but its decode is
// strict and its failures are logged, never silently swallowed.

var leg10DriftBodies = []struct {
	name string
	body string
}{
	{"invalid_json", `{"id":"ses_x","ti`},
	{"trailing_garbage", `[{"id":"ses_x"}]garbage-bytes`},
	{"empty_body", ``},
	{"html_error_page", `<html><body>502 Bad Gateway</body></html>`},
}

func TestOpenCodeClient_ListSessions_WireDriftCorruption(t *testing.T) {
	for _, mode := range leg10DriftBodies {
		t.Run(mode.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(mode.body))
			}))
			t.Cleanup(srv.Close)
			setAgentAddr(srv.URL)
			client := &OpenCodeClient{password: "pw", client: srv.Client()}

			sessions, err := client.ListSessions(context.Background())
			require.Error(t, err, "a corrupted 200 must never parse as a session list")
			assert.Empty(t, sessions, "no misparsed sessions escape the client")
		})
	}
}

// The title fetch is display enrichment: drift on /session/{id} must
// degrade to an empty title WITHOUT failing the listing — and the
// decode failure must be LOGGED (Rule 3), never silently swallowed
// (the observer makes the un-swallow load-bearing: reverting to
// `_ = json.NewDecoder(...).Decode(&s)` fails this pin).
func TestOpenCodeClient_FetchSessionTitle_DriftStaysBestEffort(t *testing.T) {
	observed, logs := observer.New(zapcore.DebugLevel)

	var titleFetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" {
			_, _ = w.Write([]byte(`[{"id":"ses_1","title":"","time":{"created":1,"updated":2}}]`))
			return
		}
		titleFetches.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body>502 Bad Gateway</body></html>`))
	}))
	t.Cleanup(srv.Close)
	setAgentAddr(srv.URL)
	client := &OpenCodeClient{password: "pw", client: srv.Client()}

	prev := log
	log = zap.New(observed)
	defer func() { log = prev }()

	sessions, err := client.ListSessions(context.Background())
	require.NoError(t, err, "a drifted title fetch must never fail the listing (best-effort contract)")
	require.Len(t, sessions, 1)
	assert.Equal(t, "ses_1", sessions[0].ID)
	assert.Equal(t, "", sessions[0].Title, "drifted title fetch degrades to empty, never phantom text")
	assert.Equal(t, int32(1), titleFetches.Load(), "the empty title did trigger the individual fetch")

	driftLogged := false
	for _, e := range logs.All() {
		if e.Message == "fetchSessionTitle: decode failed" {
			driftLogged = true
		}
	}
	assert.True(t, driftLogged, "the swallowed decode failure must surface in the log (Rule 3 — no silent swallows)")
}
