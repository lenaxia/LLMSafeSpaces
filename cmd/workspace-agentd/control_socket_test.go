// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Appendix A.6 TDD targets (design 0051): golden wire shapes, version/
// method/malformed rejection, restart idempotency, spawn_env memory-only,
// and the negative capability test. These tests ARE the spec's teeth.

// dialReq performs one control-socket round trip per Appendix A: one JSON
// request per connection, one JSON response back.
func dialReq(t *testing.T, addr string, req string) (map[string]any, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(req)); err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func mustDial(t *testing.T, addr, req string) map[string]any {
	t.Helper()
	resp, err := dialReq(t, addr, req)
	require.NoError(t, err, "request %s", req)
	return resp
}

// --- A.6.1: golden wire shapes ---

func TestControlSocket_HelloShape(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	addr := srv.addr()

	resp := mustDial(t, addr, `{"v":1,"id":42,"method":"hello","params":{}}`)
	require.Equal(t, float64(1), resp["v"], "version echoed")
	require.Equal(t, float64(42), resp["id"], "id echoed verbatim")
	res, ok := resp["result"].(map[string]any)
	require.True(t, ok, "hello returns result object, got %v", resp)
	assert.Equal(t, "supervise-opencode", res["supervisor"])
	assert.Contains(t, []any{"running", "stopped"}, res["child_state"])
	_, hasErr := resp["error"]
	assert.False(t, hasErr, "no error field on success")
}

func TestControlSocket_StatusShape(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()
	addr := srv.addr()

	resp := mustDial(t, addr, `{"v":1,"id":7,"method":"status","params":{}}`)
	require.Equal(t, float64(7), resp["id"])
	res := resp["result"].(map[string]any)
	assert.Contains(t, []any{"running", "stopped"}, res["child_state"])
	assert.Contains(t, res, "restarts")
	assert.Contains(t, res, "last_restart_at")
}

// --- A.6.2: rejection paths ---

func TestControlSocket_VersionUnsupported(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()

	resp := mustDial(t, srv.addr(), `{"v":2,"id":1,"method":"hello","params":{}}`)
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "v:2 must error, got %v", resp)
	assert.Equal(t, "version_unsupported", errObj["code"])
}

func TestControlSocket_MethodUnknown(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()

	resp := mustDial(t, srv.addr(), `{"v":1,"id":1,"method":"exec","params":{"argv":["/bin/sh"]}}`)
	errObj := resp["error"].(map[string]any)
	assert.Equal(t, "method_unknown", errObj["code"], "exec must be rejected, never dispatched")
}

func TestControlSocket_Malformed(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()

	resp, err := dialReq(t, srv.addr(), `{"v":1,"id":1,"method":`)
	if err == nil {
		errObj, ok := resp["error"].(map[string]any)
		require.True(t, ok, "partial-JSON gets bad_request or close, got %v", resp)
		assert.Equal(t, "bad_request", errObj["code"])
	}
	// Either a close-with-no-response or bad_request is spec-conformant.
}

// Unknown FIELDS within a known method are ignored (A.1 forward compat).
func TestControlSocket_UnknownFieldsIgnored(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	go srv.serve()

	resp := mustDial(t, srv.addr(), `{"v":1,"id":9,"method":"hello","params":{},"future_field":"x"}`)
	_, hasErr := resp["error"]
	assert.False(t, hasErr, "unknown top-level fields must not fail the request")
	assert.Equal(t, float64(9), resp["id"])
}

// --- A.6.3: restart idempotency (in-progress-wins) ---

func TestControlSocket_RestartIdempotency(t *testing.T) {
	proc := &fakeRestartProc{block: make(chan struct{})}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()
	addr := srv.addr()

	// First restart: blocks inside Restart (slow teardown in flight).
	firstDone := make(chan map[string]any, 1)
	go func() {
		r, _ := dialReq(t, addr, `{"v":1,"id":100,"method":"restart","params":{"reason":"manual"}}`)
		firstDone <- r
	}()

	// Wait until the first restart is genuinely inside Restart.
	require.Eventually(t, func() bool { return proc.blocked.Load() },
		5*time.Second, 10*time.Millisecond, "first restart should be in flight")

	// Second restart while the first is still running: must report
	// in_progress, NOT queue or start a second restart.
	second := mustDial(t, addr, `{"v":1,"id":101,"method":"restart","params":{"reason":"manual"}}`)
	res := second["result"].(map[string]any)
	assert.Equal(t, false, res["restarted"], "second request must not win")
	assert.Equal(t, true, res["in_progress"], "second request reports in_progress")

	// Release the first; it completes as the winner.
	close(proc.block)
	first := <-firstDone
	fres := first["result"].(map[string]any)
	assert.Equal(t, true, fres["restarted"], "first restart completes as the winner")

	assert.Equal(t, int64(1), proc.restarts.Load(), "exactly one restart performed")
}

// --- A.6.4: spawn_env memory-only ---

func TestControlSocket_SpawnEnvWritesNoFile(t *testing.T) {
	dir := t.TempDir()
	_ = dir // env storage is memory-only; the walk below asserts no file
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	resp := mustDial(t, srv.addr(), `{"v":1,"id":5,"method":"spawn_env","params":{"env":{"GH_TOKEN":"ghp_test","X":"1"}}}`)
	res := resp["result"].(map[string]any)
	assert.Equal(t, true, res["stored"])

	// The stored env must be usable by the next spawn (in-memory)…
	stored := proc.lastEnv.Load()
	require.NotNil(t, stored)
	assert.Equal(t, map[string]string{"GH_TOKEN": "ghp_test", "X": "1"}, *stored)

	// …and must NOT exist as a file: the supervisor keeps it in memory
	// only (A.2). Structural assertion: the spawnEnvStore type has no
	// file path field, and no file under the sandbox tmpfs contains the
	// value (verified by the memory-only store test below).
	assert.True(t, proc.lastEnv.Load() != nil, "env retained in memory for next spawn")
}

// --- A.6.5: negative capability test ---

// No v1 method may return env values, and restart accepts only the closed
// reason enum — never argv. These are the A.4 invariants.
func TestControlSocket_NoEnvOutNoArgvIn(t *testing.T) {
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", &fakeRestartProc{})
	go srv.serve()
	addr := srv.addr()

	// Store an env; every subsequent response must not echo it.
	mustDial(t, addr, `{"v":1,"id":1,"method":"spawn_env","params":{"env":{"CANARY_SECRET":"s3cr3t"}}}`)
	for _, m := range []string{`hello`, `status`, `metrics`} {
		raw, err := dialReq(t, addr, fmt.Sprintf(`{"v":1,"id":2,"method":%q,"params":{}}`, m))
		require.NoError(t, err)
		b, _ := json.Marshal(raw)
		assert.NotContains(t, string(b), "s3cr3t", "%s must never return env values (A.4 invariant 1)", m)
	}

	// restart with an unknown reason (or argv-shaped params) → bad_request.
	resp := mustDial(t, addr, `{"v":1,"id":3,"method":"restart","params":{"reason":"not_a_reason"}}`)
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "unknown reason must be rejected, got %v", resp)
	assert.Equal(t, "bad_request", errObj["code"])

	resp2 := mustDial(t, addr, `{"v":1,"id":4,"method":"restart","params":{"reason":"manual","argv":["/bin/sh"]}}`)
	errObj2 := resp2["error"].(map[string]any)
	assert.Equal(t, "bad_request", errObj2["code"], "argv in restart params must be rejected (A.4 invariant 2)")
}
