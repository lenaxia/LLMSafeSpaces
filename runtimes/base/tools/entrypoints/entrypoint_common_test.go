// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Regression tests for the mise-shims boot fix in entrypoint-common.sh.
//
// The bug (2026-08-27): build-time `mise reshim` wrote shims to
// MISE_DATA_DIR=/workspace/.local/share/mise/shims, but /workspace is a
// PVC mount that shadows the image layer — fresh PVC ⇒ both shims dirs
// empty ⇒ go/python3/node unresolvable in every non-interactive shell
// (harness tool shells never see `mise activate`). The fix: reshim at
// every boot, non-fatal on error.
//
// Two layers:
//
//   - Contract tests (mock mise + mock agentd; run everywhere CI runs):
//     the entrypoint must CALL reshim, must survive reshim failure, and
//     must still reach `materialize` in both cases.
//   - Reproduction test (real mise; skips where mise is absent — e.g.
//     plain GH runners — and runs where the runtime exists: dev boxes,
//     the kind integration, workspaces): a fresh MISE_DATA_DIR starts
//     with no resolvable shims; after the entrypoint's reshim step, the
//     shims dir resolves the toolchain. This is the test that FAILS on
//     the pre-fix script.
package entrypoints

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath locates entrypoint-common.sh relative to this test file.
func scriptPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(".", "entrypoint-common.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("entrypoint-common.sh not found next to test: %v", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	return abs
}

// runEntrypoint executes entrypoint-common.sh with a controlled PATH and
// env. mockBin must provide `mise` and `workspace-agentd` stand-ins.
func runEntrypoint(t *testing.T, mockBin string, env map[string]string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command("bash", scriptPath(t))
	cmd.Env = []string{
		"PATH=" + mockBin + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"AGENTD_IMAGE_VOLUME=",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errB strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errB
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run entrypoint: %v", err)
	}
	return out.String(), errB.String(), code
}

// writeMockTool creates an executable shell script.
func writeMockTool(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write mock %s: %v", name, err)
	}
}

// TestEntrypoint_CallsReshim: the contract — boot must invoke `mise
// reshim`. The mock mise records its args; absence of the call is the
// pre-fix behavior.
func TestEntrypoint_CallsReshim(t *testing.T) {
	bin := t.TempDir()
	writeMockTool(t, bin, "mise", `echo "reshim $@" >> "`+filepath.Join(bin, "mise.log")+`"; exit 0`)
	writeMockTool(t, bin, "workspace-agentd", `exit 0`)

	_, _, code := runEntrypoint(t, bin, nil)
	if code != 0 {
		t.Fatalf("entrypoint exited %d, want 0", code)
	}

	log, err := os.ReadFile(filepath.Join(bin, "mise.log"))
	if err != nil {
		t.Fatalf("mise was never invoked — the reshim boot step is missing (pre-fix regression): %v", err)
	}
	if !strings.Contains(string(log), "reshim reshim") {
		t.Fatalf("mise invoked but not with 'reshim': %q", string(log))
	}
}

// TestEntrypoint_ReshimFailureIsNonFatal: a broken mise must degrade to
// the documented `mise which` fallback, never block boot — agentd
// materialize still runs and the entrypoint exits 0.
func TestEntrypoint_ReshimFailureIsNonFatal(t *testing.T) {
	bin := t.TempDir()
	writeMockTool(t, bin, "mise", `echo "mise: reshim exploded" >&2; exit 1`)
	matFile := filepath.Join(bin, "materialized")
	writeMockTool(t, bin, "workspace-agentd", `echo "$1" >> "`+matFile+`"; exit 0`)

	stdout, stderr, code := runEntrypoint(t, bin, nil)
	if code != 0 {
		t.Fatalf("reshim failure must be non-fatal: entrypoint exited %d (stderr: %s)", code, stderr)
	}
	mat, err := os.ReadFile(matFile)
	if err != nil || !strings.Contains(string(mat), "materialize") {
		t.Fatalf("materialize did not run after reshim failure (err=%v)", err)
	}
	if !strings.Contains(stderr, "non-fatal") {
		t.Fatalf("reshim failure should log the non-fatal notice, got stderr: %q", stderr)
	}
	_ = stdout
}

// TestEntrypoint_ReshimReproducesAndFixes: the true regression test —
// requires REAL mise (skipped where absent). A fresh MISE_DATA_DIR has
// no shims (the PVC-shadowed state); the entrypoint's reshim must
// create them such that the toolchain resolves THROUGH the shims dir
// when it is prepended to PATH (the Dockerfile's job).
func TestEntrypoint_ReshimReproducesAndFixes(t *testing.T) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("real mise not on PATH (plain CI runner); reproduction runs where the runtime exists")
	}

	// Guard: this environment must manage go via mise for the
	// reproduction to be meaningful. If it doesn't (some other layout),
	// skip rather than false-fail.
	if out, err := exec.Command(mise, "which", "go").Output(); err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Skipf("mise does not manage `go` here; reproduction not applicable (err=%v)", err)
	}

	// Fresh data dir — the fresh-PVC state.
	dataDir := t.TempDir()
	shimsDir := filepath.Join(dataDir, "shims")

	// BEFORE (reproduces the bug): no shims exist, so a PATH with the
	// shims dir prepended cannot resolve go THROUGH it.
	if entries, _ := os.ReadDir(shimsDir); len(entries) > 0 {
		t.Fatalf("precondition: fresh data dir must have no shims, found %d", len(entries))
	}

	bin := t.TempDir()
	writeMockTool(t, bin, "workspace-agentd", `exit 0`)
	// Real mise for the entrypoint run: expose via PATH with the mocks.
	// (mise reshim needs the real binary; the mock dir only adds agentd.)

	cmd := exec.Command("bash", scriptPath(t))
	cmd.Env = []string{
		"PATH=" + bin + ":" + filepath.Dir(mise) + ":/usr/bin:/bin",
		"HOME=" + t.TempDir(),
		"AGENTD_IMAGE_VOLUME=",
		"MISE_DATA_DIR=" + dataDir,
		"MISE_YES=1",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint failed with real mise: %v\n%s", err, out)
	}

	// AFTER (the fix): shims exist and go resolves through them.
	entries, err := os.ReadDir(shimsDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("reshim did not populate %s (err=%v) — pre-fix behavior", shimsDir, err)
	}

	resolve := exec.Command("sh", "-c", "command -v go")
	resolve.Env = []string{"PATH=" + shimsDir + ":/usr/bin:/bin"}
	out, err := resolve.Output()
	if err != nil {
		t.Fatalf("go does not resolve through the shims dir after reshim: %v", err)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, shimsDir) {
		t.Fatalf("go resolved outside the shims dir (%q) — shim resolution broken", got)
	}
}
