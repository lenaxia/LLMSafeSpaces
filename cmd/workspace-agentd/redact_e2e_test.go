// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRedactWrapper_ExecLevelE2E exercises the FULL S2 wiring with zero
// fakes: it builds the real workspace-agentd binary, installs the real
// PATH wrapper via the production writeRedactWrapper, and pipes a secret
// through a real /bin/sh invocation of `redact` — the exact
// `some-command | redact` UX the wrapper preserves (docs/reference/cli.md).
//
// Cluster-level e2e (wrapper present on a real pod before opencode spawn,
// unwritable /sandbox-runtime legs) is S5 scope per design 0053 §4.3/§7
// (platform-contract assertions move to the artifact's own CI; the kind
// suite extension lands with S5) — the same deferral S1 (#1126) shipped
// under.
func TestRedactWrapper_ExecLevelE2E(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available; skipping exec-level e2e")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "workspace-agentd")
	out, err := exec.Command(goBin, "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, "build workspace-agentd: %s", out)

	wrapperDir := filepath.Join(dir, "sandbox-runtime", "bin")
	require.NoError(t, writeRedactWrapper(wrapperDir, bin))

	config := filepath.Join(dir, "patterns.json")
	require.NoError(t, os.WriteFile(config, []byte(`[{"regex":"s3cr3t-token","replacement":"[REDACTED]"}]`), 0o600))

	// The documented UX: pipe through `redact` found on PATH.
	wrapperSh := `PATH="` + wrapperDir + `:$PATH"; echo 'token=s3cr3t-token Authorization: Bearer sk-ant-api03-abcdef123456' | redact -config ` + config
	out, err = exec.Command("/bin/sh", "-c", wrapperSh).CombinedOutput()
	require.NoError(t, err, "pipe through wrapper failed: %s", out)

	assert.NotContains(t, string(out), "s3cr3t-token",
		"custom-pattern secret leaked through the wrapper exec chain")
	assert.NotContains(t, string(out), "sk-ant-api03-abcdef123456",
		"built-in-rule secret leaked through the wrapper exec chain")
	assert.Contains(t, string(out), "[REDACTED]",
		"custom pattern applied through the wrapper exec chain")
	assert.True(t, strings.Contains(string(out), "\n") || strings.Contains(string(out), "token="),
		"stdout survived the pipe")

	// Subcommand invoked DIRECTLY (no wrapper) must behave identically.
	direct := `echo 'key=s3cr3t-token' | "` + bin + `" redact -config ` + config
	out, err = exec.Command("/bin/sh", "-c", direct).CombinedOutput()
	require.NoError(t, err, "direct subcommand failed: %s", out)
	assert.NotContains(t, string(out), "s3cr3t-token")
	assert.Contains(t, string(out), "[REDACTED]")

	// Unhappy leg: malformed config fails closed through the wrapper
	// (exit 1, nothing unredacted emitted on stdout).
	badConfig := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(badConfig, []byte("not json"), 0o600))
	failSh := `PATH="` + wrapperDir + `:$PATH"; echo 'token=s3cr3t-token' | redact -config ` + badConfig + ` > ` + filepath.Join(dir, "stdout.txt") + ` 2>/dev/null; echo "rc=$?"`
	out, err = exec.Command("/bin/sh", "-c", failSh).CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "rc=1", "malformed config must exit 1 through the wrapper")
	stdoutBytes, err := os.ReadFile(filepath.Join(dir, "stdout.txt"))
	require.NoError(t, err)
	assert.Empty(t, string(stdoutBytes), "no partial/unredacted output on failure")
}
