// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// scrub_supervisor_exec_test.go — US-72.6 r1: the exec-level chain proof.
// The #1570 review's deletion test showed the new WIRING (the supervisor
// path's tracker + boot call + seam) was 0%-covered: removing every line
// kept the suite green. This runs the REAL supervise-opencode subcommand
// (the same TestSupervisorHelperProcess re-exec pattern as
// supervisor_subprocess_test.go) with a planted Surface-2 residue under
// an env-pointed scrub root, then reads the REAL control socket's status:
// the boot scrub must have fired IN THE REAL PROCESS and the report must
// cross the wire — LegacyKeysScrubbed's sidecar mirror has its complete
// red mode (delete the boot call, the seam, or the decode: this fails).

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupervisorSubprocess_BootScrubChainExec: the residue-bearing boot.
func TestSupervisorSubprocess_BootScrubChainExec(t *testing.T) {
	if testing.Short() {
		t.Skip("exec-level: the real subcommand; skipped under -short")
	}
	withTestLogger(t)

	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "opencode")
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 3600\n"), 0o755))

	// The PVC-shaped scrub root with the Surface-2 residue the R3 row
	// plants (the file init-fs never touches — the only plant that
	// survives to the boot scrub).
	root := t.TempDir()
	residue := filepath.Join(root, ".local", "config", "opencode", "agent-config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(residue), 0o755))
	require.NoError(t, os.WriteFile(residue,
		[]byte(`{"provider": {"legacy": {"options": {"apiKey": "sk-EXEC-RESIDUE"}}}}`), 0o644))

	port := freeTCPPort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorHelperProcess", "-test.v") //nolint:gosec // trusted test binary
	cmd.Env = append([]string{
		"GO_TEST_SUPERVISOR=1",
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=" + addr,
		"LLMSAFESPACES_LEGACY_SCRUB_ROOT=" + root,
		"PATH=" + stubDir + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"WORKSPACE_ID=supervisor-scrub-exec-test",
	}, "GO_TEST_FAKE_OPENCODE=1", "FAKE_PORT="+strconv.Itoa(port), "FAKE_BIND_DELAY_MS=1000")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Belt and braces (the supervisor_subprocess_test pattern): the
	// supervisor is a subreaper whose `sleep 3600` child inherits this
	// pipe — without WaitDelay the lingering child I/O fails the whole
	// test binary after exit ("Test I/O incomplete").
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())
	// SIGTERM (never SIGKILL): the supervisor's serial shutdown takes
	// its `sleep` child down with it (TestSupervisorSubprocess_Shutdown-
	// ReapsChild's contract) — a killed supervisor orphans the child,
	// which holds the inherited stdout pipe and fails the whole test
	// binary on incomplete I/O after exit.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	// Wait for the socket.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cc := newControlClient(addr)
	var scrub *struct {
		ConfigKeysRemoved int
		AuthKeysRemoved   int
	}
	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		if err != nil || st == nil || st.LegacyScrub == nil {
			return false
		}
		scrub = &struct {
			ConfigKeysRemoved int
			AuthKeysRemoved   int
		}{st.LegacyScrub.ConfigKeysRemoved, st.LegacyScrub.AuthKeysRemoved}
		return true
	}, 10*time.Second, 200*time.Millisecond,
		"the REAL supervisor's status must carry the boot-scrub report (the whole sidecar mirror chain starts here)")

	assert.Equal(t, 1, scrub.ConfigKeysRemoved,
		"the real boot scrub must remove the Surface-2 residue (configKeysRemoved=1 — the R3 evidence)")
	after, err := os.ReadFile(residue)
	require.NoError(t, err)
	assert.NotContains(t, string(after), "sk-EXEC-RESIDUE",
		"the residue key material must be gone from the file the real process scrubbed")
}
