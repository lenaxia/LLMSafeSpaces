// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// control_socket_concurrency_test.go — design 0051 A.3 amendment pins.
//
// The amendment (US-2) replaces "single-threaded request handling" with
// handler-per-connection semantics. These tests validate the three claims
// the amendment makes against the real server:
//
//  1. reads (hello/status/metrics) are served WHILE a restart is in
//     flight — the head-of-line-blocking shape the original wording
//     would have produced under the same load;
//  2. a second restart during a slow restart reports in_progress
//     (already pinned by TestControlSocket_RestartIdempotency — the
//     slow-restart fixture here reuses its mechanics);
//  3. spawn_env stores under concurrent access are mutex-protected
//     last-write-wins assignments — no torn state, no lost store, and
//     the env visible to the NEXT spawn is exactly one written value.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestControlSocket_ReadsNotBlockedBySlowRestart: stage a restart that
// blocks in the proc (200ms is far beyond a poll's service time) and
// verify hello/status/metrics round-trips complete concurrently. Under
// single-threaded handling every read would queue behind the restart —
// 3 reads × 200ms+ each. The assert boundary (each read < 150ms) sits
// well below the blocked regime.
func TestControlSocket_ReadsNotBlockedBySlowRestart(t *testing.T) {
	proc := &fakeRestartProc{block: make(chan struct{})}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	// Kick off a slow restart, wait until the proc is inside Restart…
	restartDone := make(chan struct{})
	go func() {
		defer close(restartDone)
		resp := mustDial(t, srv.addr(), `{"v":1,"id":1,"method":"restart","params":{"reason":"manual","grace_seconds":5}}`)
		res := resp["result"].(map[string]any)
		require.Equal(t, true, res["restarted"])
	}()
	require.Eventually(t, func() bool { return proc.blocked.Load() },
		2*time.Second, 5*time.Millisecond, "slow restart should be in flight")

	// …then serve reads concurrently. All must answer well within the
	// restart's remaining hold time.
	for _, method := range []string{"hello", "status", "metrics"} {
		m := method
		begin := time.Now()
		resp := mustDial(t, srv.addr(),
			`{"v":1,"id":2,"method":"`+m+`","params":{}}`)
		elapsed := time.Since(begin)
		require.NotNil(t, resp["result"], "%s must answer while a restart is in flight", m)
		require.Less(t, elapsed, 150*time.Millisecond,
			"%s took %v while a restart was in flight — reads must not head-of-line-block behind restarts (A.3 amendment)", m, elapsed)
	}

	close(proc.block) // release the restart
	select {
	case <-restartDone:
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not complete after release")
	}
}

// TestControlSocket_ConcurrentSpawnEnvLastWriteWins: many concurrent
// spawn_env requests against a slow-restart-holding server (worst-case
// interleaving) must leave the supervisor's stored env as exactly ONE of
// the written maps — a mutex-protected whole-map assignment, never a
// torn merge and never a lost store window observable by the next
// restart's spawn.
func TestControlSocket_ConcurrentSpawnEnvLastWriteWins(t *testing.T) {
	proc := &fakeRestartProc{block: make(chan struct{})}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	// Hold a restart open so the server is provably serving other
	// connections mid-restart.
	go func() {
		mustDial(t, srv.addr(), `{"v":1,"id":1,"method":"restart","params":{"reason":"manual"}}`)
	}()
	require.Eventually(t, func() bool { return proc.blocked.Load() },
		2*time.Second, 5*time.Millisecond)

	const writers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			payload, _ := json.Marshal(map[string]any{
				"v": 1, "id": i + 100, "method": "spawn_env",
				"params": map[string]any{"env": map[string]string{"WRITER": string(rune('a' + i))}},
			})
			resp, err := dialReq(t, srv.addr(), string(payload))
			require.NoError(t, err)
			require.Nil(t, resp["error"], "spawn_env writer %d: %v", i, resp["error"])
		}(i)
	}
	close(start)
	wg.Wait()

	// The stored env must be exactly one writer's whole map.
	env := *proc.lastEnv.Load()
	require.Len(t, env, 1, "whole-map assignment: exactly one key, got %v", env)
	require.Contains(t, env, "WRITER")
	require.Regexp(t, `^[a-p]$`, env["WRITER"], "value must be one written writer tag")

	close(proc.block)
}

// TestControlSocket_SpawnEnvVisibleToNextSpawn: after spawn_env, the
// stored env reaches the NEXT restart's proc call — the store→use seam
// the A.2 contract ("used for the NEXT spawn") requires, exercised
// through the socket (the sidecar's only path).
func TestControlSocket_SpawnEnvVisibleToNextSpawn(t *testing.T) {
	proc := &fakeRestartProc{}
	srv := newControlSocketServerWithProc(t, "127.0.0.1:0", proc)
	go srv.serve()

	_ = mustDial(t, srv.addr(),
		`{"v":1,"id":1,"method":"spawn_env","params":{"env":{"A":"1","B":"2"}}}`)
	resp := mustDial(t, srv.addr(), `{"v":1,"id":2,"method":"restart","params":{"reason":"credential_reload"}}`)
	require.Equal(t, true, resp["result"].(map[string]any)["restarted"])

	env := *proc.lastEnv.Load()
	require.Equal(t, map[string]string{"A": "1", "B": "2"}, env,
		"the env stored before restart must be the one the supervisor uses for the next spawn")
}
