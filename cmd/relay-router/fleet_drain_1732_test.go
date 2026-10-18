// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// fleet_drain_1732_test.go — #1732 leg A1 pins: the FLEET router's
// deploy-roll drain. The incident's halt leg: the v0.38.0 roll replaced
// the single replica; SIGTERM ran Shutdown on a 10s window, hard-cutting
// every in-flight generation older than 10s (cmd/relay-router main.go,
// pre-#1732). The BYO plane already carries the correct semantics
// (#1078: 630s TGS + 600s drain grace sized to minute-long SSE
// generations, a cap not a delay); these pins hold the fleet plane to
// the same contract.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test1732_FleetDrainGraceDefaultsToBYOParity pins the parity contract:
// the default drain grace matches the BYO plane's sizing rationale
// (#1078) — minute-long generations must fit inside the window. The
// historical 10s window is the #1732 halt mechanism and must never
// return.
func Test1732_FleetDrainGraceDefaultsToBYOParity(t *testing.T) {
	cfg := loadRouterConfig()
	assert.Equal(t, 10*time.Minute, cfg.drainGrace,
		"fleet drain grace must default to the #1078 BYO parity value — 10s hard-cuts minute-long generations on every deploy")
}

// Test1732_FleetDrainGraceEnvOverride pins the operator knob: DRAIN_GRACE
// overrides the default (the chart sets it alongside
// terminationGracePeriodSeconds; the TGS must exceed the drain bound).
func Test1732_FleetDrainGraceEnvOverride(t *testing.T) {
	t.Setenv("DRAIN_GRACE", "90s")
	cfg := loadRouterConfig()
	assert.Equal(t, 90*time.Second, cfg.drainGrace)
}

// Test1732_FleetDrainWiringThroughServeFleetOn pins the SIGNAL→DRAIN
// WIRING — the fleet twin of the BYO plane's
// TestDrainWiringThroughServeBYO: serveFleetOn's ctx-cancel path runs
// http.Server.Shutdown under the grace bound exactly as SIGTERM does at
// runtime. In-flight streams COMPLETE through the drain (the incident's
// severed generations), and the listener closes (an abandoned, undrained
// server leaves it open and fails the dial-refusal assertion).
func Test1732_FleetDrainWiringThroughServeFleetOn(t *testing.T) {
	chunks := 8
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	// A generation-shaped stream: SSE chunks over ~160ms — longer than
	// the shutdown trigger's scheduling, short enough for a fast test.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			_, _ = io.WriteString(w, "data: k\n\n")
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	server := &http.Server{Handler: mux} //nolint:gosec // test rig

	ctx, cancel := context.WithCancel(context.Background())
	cfg := loadRouterConfig()
	cfg.drainGrace = 5 * time.Second
	serveErr := make(chan error, 1)
	go func() {
		// The PRODUCTION drain path.
		serveErr <- serveFleetOn(ctx, cfg, server, ln)
	}()

	streamDone := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/v1/chat/completions") //nolint:gosec // test rig, fixed addr
		if err != nil {
			streamDone <- "err:" + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		streamDone <- string(body)
	}()

	time.Sleep(50 * time.Millisecond) // mid-stream: SIGTERM-equivalent cancel
	cancel()

	select {
	case got := <-streamDone:
		require.NotContains(t, got, "err:")
		assert.Equal(t, chunks, strings.Count(got, "data: k"), "in-flight generation completed through the drain")
	case <-time.After(10 * time.Second):
		t.Fatal("stream never completed")
	}
	select {
	case err := <-serveErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("serveFleetOn never returned after drain")
	}

	// The distinguishing assertion: Shutdown CLOSES the listener. A
	// deleted drain block returns while the abandoned server keeps
	// serving — dialing would succeed; after a real drain it must not.
	_, dialErr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	require.Error(t, dialErr, "listener must be closed after drain")
}

// Test1732_FleetDrainCutsAtGraceBound pins the CAP semantics: a stream
// that outlives the grace bound is cut when the bound expires (drain is
// bounded, never infinite — the pod still terminates inside its
// terminationGracePeriodSeconds).
func Test1732_FleetDrainCutsAtGraceBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: start\n\n")
		flusher.Flush()
		<-release // stream never ends on its own
	})
	server := &http.Server{Handler: mux} //nolint:gosec // test rig

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := loadRouterConfig()
	cfg.drainGrace = 300 * time.Millisecond
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveFleetOn(ctx, cfg, server, ln)
	}()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/") //nolint:gosec // test rig, fixed addr
		if err != nil {
			body <- "err:" + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // SIGTERM-equivalent; the stream hangs past the 300ms bound

	// The bound is enforced by RETURN (main exits; the pod's SIGKILL at
	// terminationGracePeriodSeconds is the final backstop) — in-process,
	// Shutdown does not force-close active connections when its ctx
	// expires, so the pinned contract is: serveFleetOn returns within
	// the grace bound instead of hanging forever.
	select {
	case err := <-serveErr:
		require.Error(t, err, "drain that hits the bound reports the context deadline")
	case <-time.After(2 * time.Second):
		t.Fatal("serveFleetOn never returned after the bounded drain — the cap is not enforced")
	}
	close(release)
	<-body // drain the client goroutine
}
