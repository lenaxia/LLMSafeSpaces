// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func TestMain(m *testing.M) {
	log = zap.NewNop()
	os.Exit(m.Run())
}

// TestE2E_SSEToStatusz exercises the full flow:
// opencode SSE stream → sessionStatusTracker → cachedState → /v1/statusz response
func TestE2E_SSEToStatusz(t *testing.T) {
	// Channel to control when SSE events are sent
	sseEvents := make(chan string, 10)
	var sseConnected sync.WaitGroup
	sseConnected.Add(1)

	// Mock opencode server with SSE endpoint
	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			json.NewEncoder(w).Encode(map[string]interface{}{"healthy": true, "version": "2.0.0"})
		case "/provider":
			json.NewEncoder(w).Encode(map[string][]string{"connected": {"anthropic"}})
		case "/config/providers":
			json.NewEncoder(w).Encode(map[string][]struct{}{"providers": {{}}})
		case "/session":
			json.NewEncoder(w).Encode([]struct {
				ID string `json:"id"`
			}{
				{ID: "ses_1"}, {ID: "ses_2"}, {ID: "ses_3"},
			})
		case "/session/ses_1":
			json.NewEncoder(w).Encode(map[string]string{"title": "Auth refactor"})
		case "/session/ses_2":
			json.NewEncoder(w).Encode(map[string]string{"title": "Fix proxy"})
		case "/session/ses_3":
			json.NewEncoder(w).Encode(map[string]string{"title": ""})
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("server doesn't support flushing")
			}
			sseConnected.Done()
			for evt := range sseEvents {
				fmt.Fprintf(w, "data: %s\n\n", evt)
				flusher.Flush()
			}
		}
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { agentAddrAtomic.Store(origAddr) }()
	agentAddrAtomic.Store(opencodeSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	cache := &providerCache{}
	tracker := newSessionStatusTracker()
	go tracker.subscribe(ctx, client)

	// Wait for SSE connection to be established
	sseConnected.Wait()

	// --- Phase 1: All sessions idle (no SSE events yet) ---
	_, _, sessions := cachedState(context.Background(), client, cache, tracker)
	require.Len(t, sessions, 3)
	assert.Equal(t, "idle", sessions[0].Status)
	assert.Equal(t, "idle", sessions[1].Status)
	assert.Equal(t, "idle", sessions[2].Status)
	assert.Equal(t, "Auth refactor", sessions[0].Title)
	assert.Equal(t, "Fix proxy", sessions[1].Title)

	// --- Phase 2: SSE marks ses_1 as busy ---
	sseEvents <- `{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"busy"}}}`
	time.Sleep(50 * time.Millisecond) // let tracker process

	// Invalidate cache to force re-read with merged statuses
	cache.mu.Lock()
	cache.lastFetchedAt = time.Time{}
	cache.mu.Unlock()

	_, _, sessions = cachedState(context.Background(), client, cache, tracker)
	assert.Equal(t, "busy", sessions[0].Status, "ses_1 should be busy after SSE event")
	assert.Equal(t, "idle", sessions[1].Status, "ses_2 should still be idle")
	assert.Equal(t, "idle", sessions[2].Status, "ses_3 should still be idle")

	// --- Phase 3: SSE marks ses_2 as busy, ses_1 back to idle ---
	sseEvents <- `{"type":"session.status","properties":{"sessionID":"ses_2","status":{"type":"busy"}}}`
	sseEvents <- `{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"idle"}}}`
	time.Sleep(50 * time.Millisecond)

	cache.mu.Lock()
	cache.lastFetchedAt = time.Time{}
	cache.mu.Unlock()

	_, _, sessions = cachedState(context.Background(), client, cache, tracker)
	assert.Equal(t, "idle", sessions[0].Status, "ses_1 should be idle again")
	assert.Equal(t, "busy", sessions[1].Status, "ses_2 should be busy")

	// --- Phase 4: Verify statusz response has correct active count ---
	healthy, version, _ := client.IsHealthy(context.Background())
	connected, configured, sessions := cachedState(context.Background(), client, cache, tracker)
	ready := healthy && len(connected) > 0

	activeCnt := 0
	for _, s := range sessions {
		if s.Status == "busy" {
			activeCnt++
		}
	}

	resp := agentd.StatuszResponse{
		Healthy:             healthy,
		Ready:               ready,
		Connected:           connected,
		ProvidersConfigured: configured,
		Sessions:            sessions,
		SessionsActive:      activeCnt,
		AgentType:           "opencode",
		AgentVersion:        version,
		UptimeSeconds:       1,
	}

	assert.True(t, resp.Healthy)
	assert.True(t, resp.Ready)
	assert.Equal(t, 1, resp.SessionsActive, "only ses_2 is busy")
	assert.Len(t, resp.Sessions, 3)
	assert.Equal(t, "2.0.0", resp.AgentVersion)

	close(sseEvents)
}

// TestE2E_SSEReconnectsOnDrop verifies the tracker reconnects after stream drops.
func TestE2E_SSEReconnectsOnDrop(t *testing.T) {
	var mu sync.Mutex
	connectionCount := 0

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			mu.Lock()
			connectionCount++
			count := connectionCount
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)

			if count == 1 {
				// First connection: send busy then close (simulates drop)
				fmt.Fprintf(w, "data: %s\n\n", `{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"busy"}}}`)
				flusher.Flush()
				return
			}
			// Second connection: send idle then hold open
			fmt.Fprintf(w, "data: %s\n\n", `{"type":"session.status","properties":{"sessionID":"ses_1","status":{"type":"idle"}}}`)
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(struct{}{})
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { agentAddrAtomic.Store(origAddr) }()
	agentAddrAtomic.Store(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	tracker := newSessionStatusTracker()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.subscribe(ctx, client)

	// First: tracker should get "busy" from first connection
	require.Eventually(t, func() bool {
		return tracker.get("ses_1") == "busy"
	}, 5*time.Second, 20*time.Millisecond, "should get busy from first connection")

	// Then: after reconnect, tracker should get "idle" from second connection
	require.Eventually(t, func() bool {
		return tracker.get("ses_1") == "idle"
	}, 10*time.Second, 50*time.Millisecond, "should get idle after reconnect")

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, connectionCount, 2, "should have reconnected")
}

// TestE2E_SSENestedFormat verifies the tracker handles the legacy nested event format.
func TestE2E_SSENestedFormat(t *testing.T) {
	sseEvents := make(chan string, 5)
	var connected sync.WaitGroup
	connected.Add(1)

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			connected.Done()
			for evt := range sseEvents {
				fmt.Fprintf(w, "data: %s\n\n", evt)
				flusher.Flush()
			}
			return
		}
		json.NewEncoder(w).Encode(struct{}{})
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { agentAddrAtomic.Store(origAddr) }()
	agentAddrAtomic.Store(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	tracker := newSessionStatusTracker()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.subscribe(ctx, client)
	connected.Wait()

	// Send nested format event
	sseEvents <- `{"payload":{"type":"session.status","properties":{"sessionID":"ses_nested","status":{"type":"busy"}}}}`
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, "busy", tracker.get("ses_nested"))

	close(sseEvents)
}

// TestE2E_SSELargeEventDoesNotDropConnection is the regression test for
// the stuck-busy root cause. The SSE scanner had a 64KB per-line buffer.
// opencode 1.18.10 emits message.part.updated events that exceed 300KB
// (patch parts listing thousands of files). When the scanner hit such a
// line, it failed silently and dropped the SSE connection, causing the
// tracker to miss the subsequent session.status:idle event.
//
// This test sends:
//  1. A session.status:busy event
//  2. A very large (>64KB) message.part.updated event
//  3. A session.status:idle event
//
// With the old 64KB buffer, step 2 kills the scanner and step 3 is never
// received → session stays "busy". With the fix (16MB buffer), all three
// events are processed and the session transitions to idle.
func TestE2E_SSELargeEventDoesNotDropConnection(t *testing.T) {
	sseEvents := make(chan string, 10)
	var connected sync.WaitGroup
	connected.Add(1)

	opencodeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			connected.Done()
			for evt := range sseEvents {
				fmt.Fprintf(w, "data: %s\n\n", evt)
				flusher.Flush()
			}
			return
		}
		json.NewEncoder(w).Encode(struct{}{})
	}))
	defer opencodeSrv.Close()

	origAddr := getAgentAddr()
	defer func() { agentAddrAtomic.Store(origAddr) }()
	agentAddrAtomic.Store(opencodeSrv.URL)

	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 5 * time.Second}}
	tracker := newSessionStatusTracker()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tracker.subscribe(ctx, client)
	connected.Wait()

	// Step 1: session goes busy
	sseEvents <- `{"type":"session.status","properties":{"sessionID":"ses_big","status":{"type":"busy"}}}`
	require.Eventually(t, func() bool {
		return tracker.get("ses_big") == "busy"
	}, 3*time.Second, 20*time.Millisecond, "session must be busy before large event")

	// Step 2: send a large event (>64KB, simulating a patch part with
	// thousands of files). This must NOT kill the SSE connection.
	largeFiles := strings.Repeat(`"file.go",`, 8000) // ~72KB of file paths
	largeEvent := `{"type":"message.part.updated","properties":{"sessionID":"ses_big","part":{"type":"patch","files":[` +
		strings.TrimSuffix(largeFiles, ",") + `]},"time":1234567890}}`
	require.Greater(t, len(largeEvent), 64*1024,
		"large event must exceed the old 64KB scanner buffer (got %d bytes)", len(largeEvent))
	sseEvents <- largeEvent

	// Step 3: session goes idle — this is the event that was missed when
	// the scanner died on the large event.
	sseEvents <- `{"type":"session.status","properties":{"sessionID":"ses_big","status":{"type":"idle"}}}`

	// The tracker must receive the idle transition.
	require.Eventually(t, func() bool {
		return tracker.get("ses_big") == "idle"
	}, 5*time.Second, 50*time.Millisecond,
		"session must transition to idle AFTER the large event (stuck-busy regression)")

	close(sseEvents)
}
