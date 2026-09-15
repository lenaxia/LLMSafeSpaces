// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_interrupter_test.go — #1342 item 1: the force path's
// interrupt seam. newSessionInterrupter must issue the Act interrupt
// (V1 abort route) through the actor seam — Basic §D1 credential, JSON
// POST — without the restart path learning any opencode wire shape.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withTestAgentAddr(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	orig := agentAddrAtomic.Load().(string)
	srv := httptest.NewServer(handler)
	agentAddrAtomic.Store(srv.URL)
	t.Cleanup(func() {
		agentAddrAtomic.Store(orig)
		srv.Close()
	})
}

func TestNewSessionInterrupter_PostsV1Abort(t *testing.T) {
	var calls atomic.Int32
	var gotPath, gotUser, gotPass string
	withTestAgentAddr(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		gotUser, gotPass, _ = r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		var gotBody map[string]any
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	})

	intr := newSessionInterrupter("pw-1")
	require.NoError(t, intr(context.Background(), "ses_x"))

	require.Equal(t, int32(1), calls.Load(), "exactly one interrupt POST")
	assert.Equal(t, "/session/ses_x/abort", gotPath, "the interrupt must ride the V1 abort route")
	assert.Equal(t, "opencode", gotUser)
	assert.Equal(t, "pw-1", gotPass)
}

func TestNewSessionInterrupter_SurfacesHarnessError(t *testing.T) {
	withTestAgentAddr(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	intr := newSessionInterrupter("pw-1")
	err := intr(context.Background(), "ses_gone")
	require.Error(t, err, "a harness 404 must surface to the force path (logged, restart still proceeds)")
}

func TestNewSessionInterrupter_RespectsContextDeadline(t *testing.T) {
	// The handler hangs until released — a wedged harness. (It must not
	// block on r.Context().Done(): Go's server does not cancel a
	// handler's context on client disconnect until the handler reads the
	// request body, so the test releases it explicitly instead.)
	release := make(chan struct{})
	withTestAgentAddr(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
	})
	defer close(release)

	intr := newSessionInterrupter("pw-1")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err := intr(ctx, "ses_slow")
	require.Error(t, err, "a wedged harness must surface as a context error, never block the force path unbounded")
	assert.Less(t, time.Since(begin).Milliseconds(), int64(2000),
		"the interrupt must return at the ctx deadline, not wait on the harness")
}
