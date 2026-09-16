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

// --- r2 extended sweep: the remaining OpenCodeClient wire parses ---

// leg10DriftErrPins: one-decode sites where a corrupted 200 must
// error loudly. Each body's trailing_garbage variant carries a
// type-satisfying prefix — the phantom-success shape that matters.
func leg10DriftErrPins(t *testing.T) map[string]struct {
	body string
	call func(c *OpenCodeClient) error
} {
	t.Helper()
	ctx := context.Background()
	return map[string]struct {
		body string
		call func(c *OpenCodeClient) error
	}{
		"IsHealthy": {
			body: `{"healthy":true,"version":"1.18.15"}garbage`,
			call: func(c *OpenCodeClient) error {
				_, _, err := c.IsHealthy(ctx)
				return err
			},
		},
		"ConnectedProviders": {
			body: `{"connected":["opencode"]}garbage`,
			call: func(c *OpenCodeClient) error {
				_, err := c.ConnectedProviders(ctx)
				return err
			},
		},
		"ConfiguredProviderCount": {
			body: `{"providers":[{},{}]}garbage`,
			call: func(c *OpenCodeClient) error {
				_, err := c.ConfiguredProviderCount(ctx)
				return err
			},
		},
	}
}

func TestOpenCodeClient_WireDriftCorruption_r2(t *testing.T) {
	for name, site := range leg10DriftErrPins(t) {
		for _, mode := range leg10DriftBodies {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				body := mode.body
				if mode.name == "trailing_garbage" {
					body = site.body
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(body))
				}))
				t.Cleanup(srv.Close)
				setAgentAddr(srv.URL)
				client := &OpenCodeClient{password: "pw", client: srv.Client()}

				err := site.call(client)
				require.Error(t, err, "a corrupted 200 must never parse as success (a phantom healthy/provider verdict must never escape)")
			})
		}
	}
}

// The fail-open display values (context limit, prompt tokens) degrade
// to 0 on drift — by contract — but the decode failure must be LOGGED,
// never silently swallowed.
func TestOpenCodeClient_FailOpenDisplayValues_LogDrift(t *testing.T) {
	observed, logs := observer.New(zapcore.DebugLevel)
	catalog := `{"providers":[{"id":"p1","models":{"m1":{"id":"m1","limit":{"context":1000}}}}]}garbage`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(catalog))
	}))
	t.Cleanup(srv.Close)
	setAgentAddr(srv.URL)
	client := &OpenCodeClient{password: "pw", client: srv.Client()}

	prev := log
	log = zap.New(observed)
	defer func() { log = prev }()

	assert.Equal(t, int64(0), client.ModelContextLimit(context.Background(), "m1", "p1"),
		"drifted catalog degrades to 0 (fail-open display contract)")
	assert.Equal(t, int64(0), client.fetchSessionPromptTokens(context.Background(), "ses_1"),
		"drifted history degrades to 0 (fail-open display contract)")

	driftLogged := 0
	for _, e := range logs.All() {
		if e.Message == "ModelContextLimit: decode failed" || e.Message == "fetchSessionPromptTokens: decode failed" {
			driftLogged++
		}
	}
	assert.Equal(t, 2, driftLogged, "both fail-open drift decodes must surface in the log (Rule 3)")
}
