// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Integration tests for the fail-closed boot gates (#934 review gates;
// design 0051 D5.2/D5.3, V10). These run the REAL binary as a subprocess
// via the existing buildAgentdBinary helper — a deleted gate must fail
// them, not just unit tests.
//
// agentd.PasswordPath is a hardcoded production constant
// (pkg/agentd/types.go:25 → /sandbox-cfg/password), so the password file
// must be staged at that exact path. In sandboxed test environments
// /sandbox-cfg is read-only or absent; the helper below probes writability
// once and skips the family cleanly (with the reason) where staging is
// impossible — CI runners and dev containers can write it.

// stagePasswordAt writes pw at the hardcoded password path, or skips.
func stagePasswordAt(t *testing.T, pw string) {
	t.Helper()
	dir := filepath.Dir(agentd.PasswordPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create %s in this environment: %v (integration boot tests need the hardcoded path)", dir, err)
	}
	if err := os.WriteFile(agentd.PasswordPath, []byte(pw), 0o600); err != nil {
		t.Skipf("cannot stage password at %s: %v", agentd.PasswordPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(agentd.PasswordPath) })
}

func clearPasswordAt(t *testing.T) {
	t.Helper()
	if err := os.Remove(agentd.PasswordPath); err != nil && !os.IsNotExist(err) {
		t.Skipf("cannot remove %s in this environment: %v (live read-only /sandbox-cfg)", agentd.PasswordPath, err)
	}
	t.Cleanup(func() { _ = os.Remove(agentd.PasswordPath) })
}

// runAgentdBoot runs the built binary with env and captures combined
// output + exit status. The gates fire before any listener binds, so the
// process must exit promptly; the timeout guards a hang (a gate that
// never fires would block on server startup → timeout fails the test).
func runAgentdBoot(t *testing.T, bin string, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	require.NoError(t, err, "unexpected non-exit error: %v", err)
	return string(out), 0
}

// Gate order + fatal path: valid password but no admin token → exit 1
// with the "admin token required" fatal log. Pinning that the gate fires
// AFTER the password read (G46) — a missing password must fail with the
// G46 message instead.
func TestBootGate_NoAdminToken_FatalAfterPassword(t *testing.T) {
	bin := buildAgentdBinary(t)
	stagePasswordAt(t, "valid-password-32-chars-aaaaaaaaaa")

	out, code := runAgentdBoot(t, bin, []string{
		"AGENTD_ADMIN_TOKEN=", "AGENTD_ADMIN_TOKEN_FILE=", "AGENTD_ALLOW_NO_ADMIN_TOKEN=",
	})
	require.Equal(t, 1, code, "boot must be fatal; output:\n%s", out)
	require.Contains(t, out, "admin token required",
		"the D5.2 fatal must fire (not G46, not a server start); output:\n%s", out)
	require.NotContains(t, out, "failed to read password file",
		"password was valid — G46 must NOT fire first; output:\n%s", out)
}

// G46 ordering: missing password fails with the G46 fatal even when the
// admin token is present — pins gate ordering (password read first).
func TestBootGate_MissingPassword_G46FiresFirst(t *testing.T) {
	bin := buildAgentdBinary(t)
	clearPasswordAt(t)

	out, code := runAgentdBoot(t, bin, []string{
		"AGENTD_ADMIN_TOKEN=some-token", "AGENTD_ALLOW_NO_ADMIN_TOKEN=",
	})
	require.Equal(t, 1, code, "G46 must be fatal; output:\n%s", out)
	require.Contains(t, out, "failed to read password file", "output:\n%s", out)
}

// D5.3: readable-but-EMPTY password is fatal (the guessable-credential
// guard), even with a valid admin token and the escape hatch set — the
// escape hatch must not bypass the password gates.
func TestBootGate_EmptyPassword_FatalEvenWithEscapeHatch(t *testing.T) {
	bin := buildAgentdBinary(t)
	stagePasswordAt(t, "   \n")

	out, code := runAgentdBoot(t, bin, []string{
		"AGENTD_ADMIN_TOKEN=tok", "AGENTD_ALLOW_NO_ADMIN_TOKEN=1",
	})
	require.Equal(t, 1, code, "empty password must be fatal; output:\n%s", out)
	require.Contains(t, out, "password file", "output:\n%s", out)
}

// File-delivered token passes the gate: with a valid password AND a
// token file, boot proceeds past both gates into server startup —
// observed as NOT the fatal messages (the process starts serving; we
// kill it after a probe window).
func TestBootGate_FileToken_PassesGate(t *testing.T) {
	bin := buildAgentdBinary(t)
	stagePasswordAt(t, "valid-password-32-chars-aaaaaaaaaa")

	tokFile := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(tokFile, []byte("distinct-token\n"), 0o400))

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"AGENTD_ADMIN_TOKEN=", "AGENTD_ADMIN_TOKEN_FILE="+tokFile,
	)
	outCh := make(chan []byte, 1)
	cmd.Stdout = nil
	pipe, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	go func() { buf := make([]byte, 4096); n, _ := pipe.Read(buf); outCh <- buf[:n] }()

	// The gates are synchronous pre-listener checks; a pass means the
	// process stays alive past the window (no fatal exit).
	time.Sleep(1200 * time.Millisecond)
	select {
	case out := <-outCh:
		t.Fatalf("boot gate fired despite file token; output:\n%s", string(out))
	default:
	}
	require.NoError(t, cmd.Process.Kill())
	_, _ = cmd.Process.Wait()

}
