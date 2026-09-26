// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// busy_overlay_test.go — #1574 ask 2 (TDD: authored before the wiring).
//
// The #1573 incident caught the two views holding OPPOSITE busy
// answers (tracker busy, projection idle) with no reconciliation. The
// fix shape per the issue: ONE definition, computed in the projection,
// served to every view. This pins the read path statusz consumes: the
// authority's derived busy truth OVERLAYS the SSE tracker's statuses —
// both directions — with the tracker as fallback for sessions the
// projection does not know.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// newBusyTruthTestClient serves one session per ID handed to it.
func newBusyTruthTestClient(t *testing.T, ids ...string) *OpenCodeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider":
			_ = json.NewEncoder(w).Encode(map[string][]string{"connected": {"opencode"}})
		case "/config/providers":
			_ = json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			resp := make([]struct {
				ID string `json:"id"`
			}, 0, len(ids))
			for _, id := range ids {
				resp = append(resp, struct {
					ID string `json:"id"`
				}{ID: id})
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(server.Close)
	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(server.URL)
	return &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
}

// TestCachedState_AuthorityBusyTruthOverlaysTracker: the authority's
// derived busy wins in BOTH directions for sessions it knows (the #1574
// leg: tracker idle + tool in flight → busy; the #1573 leg: tracker
// busy + authority not-busy → not busy); the tracker stays the
// fallback for sessions the projection has no record of.
func TestCachedState_AuthorityBusyTruthOverlaysTracker(t *testing.T) {
	client := newBusyTruthTestClient(t, "s1", "s2", "s3")
	tracker := newSessionStatusTracker()
	tracker.set("s1", "idle") // the #1574 shape: harness says idle…
	tracker.set("s2", "busy") // …the #1573 shape: tracker stuck busy…
	tracker.set("s3", "busy") // …and a session the projection never saw

	busyTruth := func(id string) (busy, known bool) {
		switch id {
		case "s1":
			return true, true // a tool part is in flight (derived)
		case "s2":
			return false, true // a permission wait — the carve-out
		default:
			return false, false // not the authority's session
		}
	}

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, busyTruth)
	byID := map[string]string{}
	for _, s := range sessions {
		byID[s.ID] = s.Status
	}
	assert.Equal(t, "busy", byID["s1"], "authority busy must overlay tracker idle (the #1574 incident shape)")
	assert.Equal(t, "idle", byID["s2"], "authority not-busy must overlay tracker busy (the #1573 divergence shape)")
	assert.Equal(t, "busy", byID["s3"], "tracker remains the fallback for sessions the projection does not know")
}

// TestCachedState_NilBusyTruthKeepsTrackerBehavior: nil overlay — the
// pre-#1574 read path (bare tracker statuses) is preserved for
// constructions without an authority.
func TestCachedState_NilBusyTruthKeepsTrackerBehavior(t *testing.T) {
	client := newBusyTruthTestClient(t, "s1")
	tracker := newSessionStatusTracker()
	tracker.set("s1", "idle")

	cache := &providerCache{}
	_, _, sessions := cachedState(context.Background(), client, cache, tracker, nil)
	assert.Equal(t, "idle", sessions[0].Status)
}
