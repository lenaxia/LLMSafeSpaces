// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_rpc_test.go — the JSONL command client pinned against the
// scripted RPC fixture subprocess (pi_fake_test.go): id correlation,
// the four record families' routing, framing past the 64 KiB Scanner
// cap, the stdin-wedge classification, and the rpc.md shutdown ladder
// (EOF → SIGTERM → SIGKILL).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakePiChild is one launched fake child plus a race-safe event
// snapshot.
type fakePiChild struct {
	child  *piChild
	events func() []string
}

// startFakePiChild launches one fake child with the fixture env and an
// event collector. The returned stop drains via the ladder.
func startFakePiChild(t *testing.T, extraEnv ...string) *fakePiChild {
	t.Helper()
	withTestLogger(t)
	fixture := t.TempDir()
	cfg := piFakeBaseConfig(t, fixture, extraEnv...)
	var mu sync.Mutex
	var events []string
	child := newPiChild(cfg, zap.NewNop(), func(rec *piRecord, raw []byte) {
		mu.Lock()
		events = append(events, rec.Type)
		mu.Unlock()
	})
	require.NoError(t, child.start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = child.Shutdown(ctx, time.Second, time.Second)
	})
	return &fakePiChild{
		child:  child,
		events: func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), events...) },
	}
}

func TestPiChildCallIdCorrelation(t *testing.T) {
	fake := startFakePiChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data, err := fake.child.Call(ctx, map[string]any{"type": "get_state"})
	require.NoError(t, err)
	var st struct {
		IsStreaming bool   `json:"isStreaming"`
		SessionID   string `json:"sessionId"`
	}
	require.NoError(t, json.Unmarshal(data, &st))
	require.False(t, st.IsStreaming)
	require.NotEmpty(t, st.SessionID)

	data, err = fake.child.Call(ctx, map[string]any{"type": "get_session_stats"})
	require.NoError(t, err)
	var stats struct {
		ContextUsage *struct {
			Tokens int64 `json:"tokens"`
		} `json:"contextUsage"`
	}
	require.NoError(t, json.Unmarshal(data, &stats))
	require.NotNil(t, stats.ContextUsage)
	require.Equal(t, int64(100), stats.ContextUsage.Tokens)
}

// errMalformedResponse marks an undecodable success payload in the
// concurrent-correlation test.
var errMalformedResponse = errors.New("malformed response payload")

func TestPiChildConcurrentCallsCorrelate(t *testing.T) {
	fake := startFakePiChild(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			data, err := fake.child.Call(ctx, map[string]any{"type": "get_state"})
			if err != nil {
				errs <- err
				return
			}
			var st struct {
				IsStreaming bool `json:"isStreaming"`
			}
			if json.Unmarshal(data, &st) != nil {
				errs <- errMalformedResponse
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestPiChildUnknownCommandFailsWithMessage(t *testing.T) {
	fake := startFakePiChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := fake.child.Call(ctx, map[string]any{"type": "definitely_not_a_command"})
	require.ErrorContains(t, err, "unknown command")
}

func TestPiChildHugeRecordPastScannerCap(t *testing.T) {
	// A real agent_end carries the full message array and exceeds
	// bufio.Scanner's 64 KiB cap; the reader must be cap-less
	// (bufio.Reader.ReadString). The fake emits a 96 KiB record on
	// boot; the event observer must receive it whole.
	fake := startFakePiChild(t, "PI_FAKE_HUGE_ON_BOOT=1")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(fake.events()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	evs := fake.events()
	require.GreaterOrEqual(t, len(evs), 1, "huge agent_end must arrive")
	require.Equal(t, "agent_end", evs[0])
	// The child stays usable after the big record.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := fake.child.Call(ctx, map[string]any{"type": "get_state"})
	require.NoError(t, err)
}

func TestPiChildStdinWedgeClassification(t *testing.T) {
	// Tighten the production 15s window so the test is fast.
	orig := piStdinWriteTimeout
	piStdinWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { piStdinWriteTimeout = orig })

	// The fake stops reading stdin after the first command.
	fake := startFakePiChild(t, "PI_FAKE_STOP_READ_AFTER=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First command lands and is answered.
	_, err := fake.child.Call(ctx, map[string]any{"type": "get_state"})
	require.NoError(t, err)

	// Fill the pipe with large commands until a write cannot drain.
	// Unanswered-but-written calls fail on their own short deadlines
	// (the child stopped answering); only the BLOCKED write produces
	// the wedge classification, and that is what must break the loop.
	junk := strings.Repeat("j", 8*1024)
	wedged := false
	for i := 0; i < 32 && !wedged; i++ {
		callCtx, callCancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		_, err := fake.child.Call(callCtx, map[string]any{"type": "prompt", "padding": junk})
		callCancel()
		if err == nil {
			continue // pre-wedge commands may still be answered
		}
		if strings.Contains(err.Error(), "wedged") {
			wedged = true
		}
	}
	require.True(t, wedged, "a full pipe that does not drain must classify as wedged")
	require.True(t, fake.child.Wedged())

	// Later calls fail fast without touching the pipe.
	fastCtx, fastCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer fastCancel()
	start := time.Now()
	_, err = fake.child.Call(fastCtx, map[string]any{"type": "get_state"})
	require.ErrorContains(t, err, "wedged")
	require.Less(t, time.Since(start), 50*time.Millisecond, "wedge fail-fast")
}

func TestPiChildShutdownLadderEOFClean(t *testing.T) {
	fake := startFakePiChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// rpc.md: stdin EOF disposes the runtime and exits — the fake's
	// default disposition. No signals needed.
	start := time.Now()
	err := fake.child.Shutdown(ctx, 5*time.Second, time.Second)
	require.NoError(t, err)
	require.False(t, fake.child.Alive())
	require.Less(t, time.Since(start), 5*time.Second, "clean EOF exit is prompt")
}

func TestPiChildShutdownLadderSIGTERM(t *testing.T) {
	// EOF ignored: the child survives stdin close and only SIGTERM
	// ends it — the "waits for pending work" posture's backstop.
	fake := startFakePiChild(t, "PI_FAKE_IGNORE_EOF=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := fake.child.Shutdown(ctx, 2*time.Second, 2*time.Second)
	require.NoError(t, err)
	require.False(t, fake.child.Alive())
}

func TestPiChildShutdownLadderSIGKILL(t *testing.T) {
	// EOF ignored AND SIGTERM trapped: only SIGKILL ends it. The wait
	// error is the signal death ("signal: killed") — the ladder SUCCEEDED;
	// only process liveness is being asserted.
	fake := startFakePiChild(t, "PI_FAKE_IGNORE_EOF=1", "PI_FAKE_TRAP_TERM=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_ = fake.child.Shutdown(ctx, 300*time.Millisecond, 300*time.Millisecond)
	require.False(t, fake.child.Alive(), "SIGKILL must reap the trapper")
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestPiChildCmdArgv(t *testing.T) {
	// The spawn contract (scoping §2.3 item 4): --mode rpc, optional
	// --no-session/--offline, --session-dir, --session-id, and
	// PI_CODING_AGENT_DIR in the environment.
	fixture := t.TempDir()
	cfg := piFakeBaseConfig(t, fixture)
	cfg.SessionID = "sess-42"
	cfg.Ephemeral = true
	cfg.Offline = true
	cmd := piChildCmd(cfg)
	require.Equal(t, cfg.Binary, cmd.Args[0])
	argv := strings.Join(cmd.Args, " ")
	require.Contains(t, argv, "--mode rpc")
	require.Contains(t, argv, "--no-session")
	require.Contains(t, argv, "--offline")
	require.Contains(t, argv, "--session-dir "+cfg.SessionDir)
	require.Contains(t, argv, "--session-id sess-42")
	found := false
	for _, e := range cmd.Env {
		if e == "PI_CODING_AGENT_DIR="+cfg.AgentDir {
			found = true
		}
	}
	require.True(t, found, "PI_CODING_AGENT_DIR must be set")
}

func TestPiScanSessionDirUnderscoreIdsAndNewestWins(t *testing.T) {
	dir := t.TempDir()
	// Timestamps never contain '_'; session ids may.
	names := []string{
		"20261113T100000_ts_with_underscores.jsonl",
		"20261113T090000_plain.jsonl",
		"20261113T080000_plain.jsonl", // older duplicate of plain
		"notjsonl.txt",
		"malformed.jsonl",
	}
	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644))
	}
	ids, err := piScanSessionDir(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"plain", "ts_with_underscores"}, ids)

	// Missing dir is an empty list, never an error (boot before first
	// session).
	none, err := piScanSessionDir(filepath.Join(dir, "absent"))
	require.NoError(t, err)
	require.Empty(t, none)
}
