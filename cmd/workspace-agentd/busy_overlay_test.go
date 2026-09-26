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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
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

	busyTruth := func() map[string]bool {
		return map[string]bool{
			"s1": true,  // a tool part is in flight (derived)
			"s2": false, // a permission wait — the carve-out
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

// TestStatuszHandler_BusyTruthThroughTheRealHandler: the r1 ask — the
// production wiring (buildStatuszHandler with a NON-nil busyTruth) is
// exercised end-to-end: authority-busy overlays tracker-idle (session
// renders busy), authority-idle overlays tracker-busy (session renders
// idle AND its fictional age stops contributing to busy_ages /
// oldest_busy_seconds — status and age from ONE view).
func TestStatuszHandler_BusyTruthThroughTheRealHandler(t *testing.T) {
	client := newBusyTruthTestClient(t, "s1", "s2")
	tracker := newSessionStatusTracker()
	tracker.set("s1", "idle")
	tracker.set("s2", "busy")
	// Give s2 a long fictional busy age (the #1573 Part 2 shape:
	// tracker stamps that predate the truth).
	tracker.mu.Lock()
	if tracker.busySince == nil {
		tracker.busySince = map[string]time.Time{}
	}
	tracker.busySince["s2"] = time.Now().Add(-7 * time.Hour)
	tracker.mu.Unlock()

	busyTruth := func() map[string]bool {
		return map[string]bool{"s1": true, "s2": false}
	}

	handler := buildStatuszHandler(client, &providerCache{}, tracker, busyTruth, newMemoryPressureMonitor(),
		time.Now(), "", defaultSysMetrics(), nil, nil, nil)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/statusz", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Sessions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"sessions"`
		BusyAges          map[string]int `json:"busy_ages"`
		OldestBusySeconds int            `json:"oldest_busy_seconds"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))

	byStatus := map[string]string{}
	for _, s := range body.Sessions {
		byStatus[s.ID] = s.Status
	}
	assert.Equal(t, "busy", byStatus["s1"], "authority busy overlays tracker idle")
	assert.Equal(t, "idle", byStatus["s2"], "authority idle overlays tracker busy")
	assert.NotContains(t, body.BusyAges, "s2",
		"a session corrected to idle must not keep contributing fictional age")
	assert.Equal(t, 0, body.OldestBusySeconds,
		"s1 is derived-busy without a tracker stamp (no fictional clock); s2's fiction is dropped")
}

// TestBusyTruthFrom_RealAuthority: the adapter serves the projection's
// DERIVED busy from a real Authority (one snapshot, sessions it knows
// only) — the queue leg (an admitted-then-unpromoted delivery → busy)
// and the at-rest leg (seeded idle, nothing in flight → not busy) in
// one map.
func TestBusyTruthFrom_RealAuthority(t *testing.T) {
	store := wiringEvidenceStore{
		states: map[string]sessionstate.SessionSeed{
			"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
			"s2": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
		},
	}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      noopParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		Admitter:    instantAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })

	// Seed the projection's records from store truth (s1, s2).
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	// s1: an admitted-then-unpromoted delivery — real queued turn
	// machinery through the real wire op.
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/llmsafespaces.abi.v1.HarnessABIService/Deliver",
		strings.NewReader(`{"sessionId":"s1","entryId":"e-1","attempt":1,"parts":[{"text":"hi"}]}`))
	req.SetBasicAuth("opencode", "pw")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)
	_ = res.Body.Close()

	truth := busyTruthFrom(a)
	require.NotNil(t, truth)
	require.Eventually(t, func() bool {
		return truth()["s1"]
	}, 5*time.Second, 20*time.Millisecond, "queued delivery must derive busy (the queue leg)")
	require.Never(t, func() bool {
		return truth()["s2"]
	}, 200*time.Millisecond, 20*time.Millisecond, "at-rest session derives not-busy")
	assert.NotContains(t, truth(), "s3", "projection-unknown sessions stay absent (tracker fallback)")
	assert.Nil(t, busyTruthFrom(nil), "nil authority yields nil (bare tracker behavior)")
}
