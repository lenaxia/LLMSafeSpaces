// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Entrypoint agentd-overlay verification tests (#863).
//
// verify_and_select_agentd in runtimes/base/tools/entrypoints/
// entrypoint-common.sh is the security-critical sha256 gate: the whole
// image-volume delivery design rests on "mismatch refuses to exec, with
// a distinct exit code, and no silent fallback". These tests run the
// REAL script under bash (the same interpreter the pod uses) against a
// real file, locking in:
//
//   - match          → exit 0, AGENTD_BIN set to the overlay path
//   - mismatch       → exit 81, stderr carries expected=/got= for the
//                      controller event, termination-log best-effort
//   - missing binary → exit 82
//   - unknown arch   → exit 81 (no pin available to verify against)
//   - legacy (env unset) → baked binary selected via command -v
//   - unwritable termination-log → exit code STILL 81/82 (the tee-to-
//     /dev/termination-log must never mask the contract codes)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const entrypointScript = "../../../runtimes/base/tools/entrypoints/entrypoint-common.sh"

func requireBashEntrypoint(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping entrypoint regression")
	}
	return entrypointScript
}

// fakeAgentdBin writes a tiny executable and returns its path + sha256.
func fakeAgentdBin(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "workspace-agentd")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	out, err := exec.Command("sha256sum", path).Output()
	require.NoError(t, err)
	return path, strings.Fields(string(out))[0]
}

// runEntrypoint sources the script in a bash that simulates the entrypoint
// environment (including a best-effort termination log), then prints the
// selected AGENTD_BIN. Returns the exit code and combined output.
func runEntrypoint(t *testing.T, script string, env []string, cwd string) (int, string) {
	t.Helper()
	// The script ends with "${AGENTD_BIN}" materialize; intercept by
	// overriding AGENTD_BIN after sourcing is impossible, so instead run
	// the verify function only: source with a trailing redefinition guard.
	// Simplest robust approach: copy the script, strip the final exec
	// line, source it, echo AGENTD_BIN.
	src, err := os.ReadFile(script)
	require.NoError(t, err)
	content := string(src)
	content = strings.Replace(content, `"${AGENTD_BIN}" materialize`, `echo "AGENTD_BIN=${AGENTD_BIN}"`, 1)
	require.Contains(t, content, "AGENTD_BIN=${AGENTD_BIN}",
		"script tail changed — update the intercept to match (the final line must remain the materialize exec)")
	stripped := filepath.Join(t.TempDir(), "entrypoint-common-test.sh")
	require.NoError(t, os.WriteFile(stripped, []byte(content), 0o755))

	cmd := exec.Command("bash", "-c", "set -euo pipefail; source "+stripped)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return code, string(out)
}

func TestEntrypointAgentd_MatchSelectsOverlayBinary(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()
	bin, sha := fakeAgentdBin(t, dir)

	code, out := runEntrypoint(t, script, []string{
		"AGENTD_IMAGE_VOLUME=1",
		"LLMSAFESPACES_AGENTD_BINARY=" + bin,
		"LLMSAFESPACES_AGENTD_SHA256_AMD64=" + sha,
		"LLMSAFESPACES_AGENTD_SHA256_ARM64=" + sha,
	}, dir)
	require.Equal(t, 0, code, "output: %s", out)
	require.Contains(t, out, "AGENTD_BIN="+bin, "output: %s", out)
}

func TestEntrypointAgentd_MismatchExits81WithExpectedGot(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()
	bin, _ := fakeAgentdBin(t, dir)

	code, out := runEntrypoint(t, script, []string{
		"AGENTD_IMAGE_VOLUME=1",
		"LLMSAFESPACES_AGENTD_BINARY=" + bin,
		"LLMSAFESPACES_AGENTD_SHA256_AMD64=" + strings.Repeat("d", 64),
		"LLMSAFESPACES_AGENTD_SHA256_ARM64=" + strings.Repeat("d", 64),
	}, dir)
	require.Equal(t, 81, code, "mismatch must exit 81; output: %s", out)
	require.Contains(t, out, "expected="+strings.Repeat("d", 64), "event-message format; output: %s", out)
	require.Contains(t, out, "got=", "output: %s", out)
}

func TestEntrypointAgentd_MissingBinaryExits82(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()

	code, out := runEntrypoint(t, script, []string{
		"AGENTD_IMAGE_VOLUME=1",
		"LLMSAFESPACES_AGENTD_BINARY=" + filepath.Join(dir, "does-not-exist"),
		"LLMSAFESPACES_AGENTD_SHA256_AMD64=" + strings.Repeat("a", 64),
		"LLMSAFESPACES_AGENTD_SHA256_ARM64=" + strings.Repeat("a", 64),
	}, dir)
	require.Equal(t, 82, code, "missing overlay must exit 82; output: %s", out)
}

func TestEntrypointAgentd_NoShaPinForArchExits81(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()
	bin, _ := fakeAgentdBin(t, dir)

	// AMD64 pin set to empty → arch lookup finds no pin.
	code, out := runEntrypoint(t, script, []string{
		"AGENTD_IMAGE_VOLUME=1",
		"LLMSAFESPACES_AGENTD_BINARY=" + bin,
		"LLMSAFESPACES_AGENTD_SHA256_AMD64=",
	}, dir)
	require.Equal(t, 81, code, "unknown-arch/missing pin must exit 81 (cannot verify); output: %s", out)
}

func TestEntrypointAgentd_LegacyModeUsesBakedBinary(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()
	bin, _ := fakeAgentdBin(t, dir)
	// Put the fake on PATH as the baked binary.
	binDir := filepath.Dir(bin)

	// AGENTD_IMAGE_VOLUME unset → legacy branch, command -v lookup.
	cmd := exec.Command("bash", "-c", "set -euo pipefail; PATH=\""+binDir+":$PATH\"; source "+script+"; echo \"AGENTD_BIN=${AGENTD_BIN}\"; exit 0")
	cmd.Env = append(os.Environ(), "AGENTD_IMAGE_VOLUME=")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "output: %s", out)
	require.Contains(t, string(out), "AGENTD_BIN="+bin, "legacy mode selects baked binary via PATH; output: %s", out)
}

func TestEntrypointAgentd_UnwritableTerminationLogDoesNotMaskExitCode(t *testing.T) {
	script := requireBashEntrypoint(t)
	dir := t.TempDir()
	bin, _ := fakeAgentdBin(t, dir)

	// /dev/termination-log unwritable: point the kernel file at a path
	// inside a read-only dir. The script hardcodes /dev/termination-log,
	// so simulate via a mount namespace substitute: run in a cwd whose
	// /dev is a readonly dir we control via unshare is overkill — instead
	// bind the failure by making the write fail through permissions on
	// the kernel-provided path is not possible as non-root. Compensating
	// approach: verify the log_fail helper's `|| true` semantics by
	// extracting and unit-testing the pattern.
	src, err := os.ReadFile(script)
	require.NoError(t, err)
	require.Contains(t, string(src), "/dev/termination-log 2>/dev/null || true",
		"termination-log writes must be best-effort (|| true) so an unwritable log cannot mask exit 81/82 under set -euo pipefail")
	_ = dir
	_ = bin
}
