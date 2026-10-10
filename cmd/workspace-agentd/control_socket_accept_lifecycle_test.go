// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// control_socket_accept_lifecycle_test.go — the #1644 regression pins.
//
// The CI job killer: serve() retried Accept() forever with a WARN per
// iteration whenever the listener was closed by any path other than
// close() (the only path that sets the `closed` flag). A test (or any
// external close) left the goroutine spinning on
// "use of closed network connection" — ~1M-line logs, exit 1, zero
// test failures (runs 37733161509, 37989366061).
//
// Pins (red-first against the unfixed loop):
//
//  1. External listener close → the loop EXITS within the bound and
//     emits at most one warning (the issue's exact fix shape).
//  2. Deliberate close() under dial traffic → same exit bound, so the
//     ErrClosed branch cannot regress the orderly shutdown path.
//  3. Transient accept errors are STILL warned and survived (scope
//     guard: the fix must not kill recovery for non-terminal errors —
//     EMFILE-class failures can self-heal once fds free up).

import (
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// acceptLoopExitBound is the deadline for "the accept loop exited after
// the listener closed". The fixed loop returns on its next iteration —
// microseconds of work; 2s tolerates race-detector scheduling on a
// loaded CI runner without ever passing the OLD spinning loop (which
// never exits: pinned red at exactly this bound).
const acceptLoopExitBound = 2 * time.Second

// syncBuffer is a mutex-guarded slog sink (the accept loop logs from
// its own goroutine; the test reads it concurrently).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog swaps the default slog for a text handler over a
// mutex-guarded buffer at debug level (the terminal exit line is debug;
// the assertion counts only WARN-shape lines). Restored on cleanup.
// These tests are sequential (no t.Parallel in this suite), so the
// global swap cannot race another test's logger expectations.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := slog.Default()
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelDebug)
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: lvl})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// warnLines returns the captured lines logged at WARN level matching
// substr.
func warnLines(t *testing.T, buf *syncBuffer, substr string) []string {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, substr) {
			hits = append(hits, line)
		}
	}
	return hits
}

// serveInBackground runs serve() until it returns on its own, closing
// done when it does. The ONLY observable for "the loop decided to
// stop" — exactly what the #1644 flood lost.
func serveInBackground(srv *controlSocketServer) (done <-chan struct{}) {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		srv.serve()
	}()
	return ch
}

// TestControlSocket_AcceptLoopExitsOnExternalListenerClose is THE #1644
// pin: the listener closed by any path other than close() (the CI
// shape — a test teardown closed the fd without the flag) must end the
// accept loop, not spin it. Red on the unfixed loop: done never closes
// and the log fills with one WARN per retry, forever.
func TestControlSocket_AcceptLoopExitsOnExternalListenerClose(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	logs := captureSlog(t)
	done := serveInBackground(srv)

	// The external close — deliberately NOT srv.close(): the `closed`
	// flag is the only path the old loop respected.
	require.NoError(t, srv.ln.Close(), "external close of the listener")

	select {
	case <-done:
	case <-time.After(acceptLoopExitBound):
		t.Fatalf("accept loop still running %s after the listener was closed — the #1644 warn-flood shape", acceptLoopExitBound)
	}

	warns := warnLines(t, logs, "control socket accept error")
	require.LessOrEqualf(t, len(warns), 1,
		"listener close must emit at most one warning, got %d: %v", len(warns), warns)
}

// TestControlSocket_AcceptLoopExitsOnDeliberateCloseUnderDialTraffic:
// the orderly close() path must keep exiting promptly while clients
// hammer the socket — the ErrClosed branch cannot turn live traffic
// into warns or delay the exit.
func TestControlSocket_AcceptLoopExitsOnDeliberateCloseUnderDialTraffic(t *testing.T) {
	srv := newControlSocketServerForTest(t, "127.0.0.1:0")
	logs := captureSlog(t)
	addr := srv.addr()
	done := serveInBackground(srv)

	stop := make(chan struct{})
	var dialers sync.WaitGroup
	for i := 0; i < 4; i++ {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
				if err != nil {
					return // listener closed — dialer's exit cue
				}
				_ = conn.Close()
			}
		}()
	}

	// Let the dial storm land, then the deliberate teardown.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, srv.close(), "deliberate close")

	select {
	case <-done:
	case <-time.After(acceptLoopExitBound):
		t.Fatalf("accept loop still running %s after deliberate close under dial traffic", acceptLoopExitBound)
	}
	close(stop)
	dialers.Wait()

	warns := warnLines(t, logs, "control socket accept error")
	require.LessOrEqualf(t, len(warns), 1,
		"deliberate close under traffic must emit at most one warning, got %d: %v", len(warns), warns)
}

// scriptedAcceptListener scripts transient Accept errors before
// delegating to the real listener — the seam for pin 3.
type scriptedAcceptListener struct {
	net.Listener
	scripted []error
}

func (l *scriptedAcceptListener) Accept() (net.Conn, error) {
	if len(l.scripted) > 0 {
		err := l.scripted[0]
		l.scripted = l.scripted[1:]
		return nil, err
	}
	return l.Listener.Accept()
}

// TestControlSocket_AcceptLoopSurvivesTransientErrorsAndExitsOnClose is
// the scope guard: a non-ErrClosed accept failure (EMFILE-class) is
// transient — it must be WARNED exactly once and SURVIVED (the loop
// still serves a request afterwards), while the later listener close
// still ends the loop. A fix that killed the loop on any error would
// break the survival half; no fix at all breaks the exit half.
func TestControlSocket_AcceptLoopSurvivesTransientErrorsAndExitsOnClose(t *testing.T) {
	real, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	synthetic := errors.New("synthetic transient accept error (EMFILE-shaped)")
	ln := &scriptedAcceptListener{Listener: real, scripted: []error{synthetic}}
	srv := &controlSocketServer{ln: ln, proc: &fakeRestartProc{}}
	t.Cleanup(func() { _ = srv.close() })
	logs := captureSlog(t)
	done := serveInBackground(srv)
	addr := real.Addr().String()

	// The transient error is surfaced, not swallowed.
	require.Eventually(t, func() bool {
		return len(warnLines(t, logs, "synthetic transient accept error")) >= 1
	}, acceptLoopExitBound, 5*time.Millisecond,
		"the transient accept error must be warned")

	// The loop SURVIVED it: a full request round trip still works.
	resp := mustDial(t, addr, `{"v":1,"id":7,"method":"hello","params":{}}`)
	require.Equal(t, float64(7), resp["id"], "the loop must keep serving after a transient accept error")

	// Terminal close still ends the loop (external shape).
	require.NoError(t, real.Close())
	select {
	case <-done:
	case <-time.After(acceptLoopExitBound):
		t.Fatalf("accept loop still running %s after close despite surviving the transient error", acceptLoopExitBound)
	}

	// Exactly one warn total: the transient error. The ErrClosed exit
	// line is debug-level by design (teardown of the suite's many
	// socket servers must not spam at the default level).
	warns := warnLines(t, logs, "control socket accept error")
	require.Lenf(t, warns, 1, "one transient warn expected, got %d: %v", len(warns), warns)
}
