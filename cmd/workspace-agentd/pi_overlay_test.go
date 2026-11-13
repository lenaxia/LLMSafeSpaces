// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_overlay_test.go — the second-harness overlay's supervisor half
// (design 0053 §5.4; scoping report §2.3 story S1).
//
// The controller mounts a digest-pinned image volume at /pi and stamps
// PI_IMAGE_VOLUME=1 + LLMSAFESPACES_PI_SHA256_<ARCH> pins. The
// supervisor must, at boot (no pi spawn exists yet — the harness driver
// is S2; the verify exists because the mount does):
//
//   - marker unset/other  → no-op proceed (pods from a pre-pi
//     controller never carried the overlay);
//   - marker set, match   → proceed;
//   - marker set, mismatch / no pin for arch / unhashable binary
//                        → exit 85 (PiVerificationFailed);
//   - marker set, binary absent → exit 86 (PiOverlayMissing).
//
// Exit codes 81/82 (agentd self-verify) and 83/84 (opencode overlay)
// stay disjoint — a tri-overlay pod's failure attribution must never
// cross artifacts. The subprocess tests run the REAL supervise-opencode
// binary, proving the verify fires at the construction seam before any
// child spawn.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- pure decision logic -----------------------------------------------------

// TestPiOverlayBinaryPath pins the binary-path contract: explicit env
// wins, unset/empty falls back to the tree-root mount layout (/pi/pi —
// the upstream release tree, package.json adjacency; never a /pi/bin
// subpath — the ruling lives in runtimes/pi/Dockerfile).
func TestPiOverlayBinaryPath(t *testing.T) {
	require.Equal(t, "/pi/pi", piOverlayBinaryDefault)
	require.Equal(t, "/pi/pi", piOverlayBinaryPath(""))
	require.Equal(t, "/custom/pi", piOverlayBinaryPath("/custom/pi"))
}

// TestPiOverlayDecision pins the pure verify decision for every
// contract combination. wantCode 0 = proceed.
func TestPiOverlayDecision(t *testing.T) {
	const good = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const bad = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cases := []struct {
		name     string
		volume   string
		present  bool
		amd64    string
		arm64    string
		arch     string
		actual   string
		wantCode int
		wantMsg  string
	}{
		{"legacy: marker unset", "", true, good, good, "amd64", good, 0, ""},
		{"legacy: marker unset even with mismatch", "", true, bad, bad, "amd64", good, 0, ""},
		{"legacy: marker other value skips verify", "true", true, bad, bad, "amd64", bad, 0, ""},
		{"match amd64", "1", true, good, bad, "amd64", good, 0, ""},
		{"match arm64", "1", true, bad, good, "arm64", good, 0, ""},
		{"mismatch amd64", "1", true, good, bad, "amd64", bad, 85, "PiVerificationFailed"},
		{"mismatch arm64", "1", true, bad, good, "arm64", bad, 85, "PiVerificationFailed"},
		{"no pin for arch amd64", "1", true, "", "", "amd64", good, 85, "no sha256 pin for arch amd64"},
		{"no pin for arch arm64", "1", true, good, "", "arm64", good, 85, "no sha256 pin for arch arm64"},
		{"unknown arch has no pin", "1", true, good, good, "riscv64", good, 85, "no sha256 pin"},
		{"overlay missing", "1", false, good, good, "amd64", "", 86, "PiOverlayMissing"},
		{"overlay missing precedes pin check", "1", false, "", "", "amd64", "", 86, "PiOverlayMissing"},
		{"unreadable binary hashes empty: fail closed", "1", true, good, bad, "amd64", "", 85, "PiVerificationFailed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := piOverlayDecision(piOverlayEnv{
				volumeFlag:    tc.volume,
				binaryPath:    "/pi/pi",
				binaryPresent: tc.present,
				amd64Pin:      tc.amd64,
				arm64Pin:      tc.arm64,
				arch:          tc.arch,
				actualSHA:     tc.actual,
			})
			if tc.wantCode == 0 {
				require.NoError(t, err)
				require.Equal(t, piOverlayExitProceed, code)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg)
			require.Equal(t, tc.wantCode, code)
		})
	}
}

// TestPiOverlayDecision_CodesDisjointFromSiblings pins the ladder: 85/86
// for pi, never 81/82 (agentd) or 83/84 (opencode) — and the opencode
// decision never emits 85/86 either.
func TestPiOverlayDecision_CodesDisjointFromSiblings(t *testing.T) {
	const good = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const bad = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	code, err := piOverlayDecision(piOverlayEnv{
		volumeFlag: "1", binaryPresent: true, amd64Pin: good, arm64Pin: bad,
		arch: "amd64", actualSHA: bad, binaryPath: "/pi/pi",
	})
	require.Equal(t, 85, code, "pi verify failure is 85")
	require.Contains(t, err.Error(), "PiVerificationFailed")

	// The opencode decision must keep emitting 83 for the same shape.
	ocCode, ocErr := opencodeOverlayDecision(opencodeOverlayEnv{
		volumeFlag: "1", binaryPresent: true, amd64Pin: good, arm64Pin: bad,
		arch: "amd64", actualSHA: bad, binaryPath: "/opencode/usr/local/bin/opencode",
	})
	require.Equal(t, 83, ocCode)
	require.Contains(t, ocErr.Error(), "OpencodeVerificationFailed")
}

// --- the boot seam (real binary) ---------------------------------------------

// writePiStub writes a fake pi binary and returns its path and sha256
// (via the production hasher).
func writePiStub(t *testing.T, dir string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "pi")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	sha, err := sha256Path(path)
	require.NoError(t, err)
	return path, sha
}

// TestSuperviseOpencode_PiVerifyFailureExits85 runs the REAL
// supervise-opencode subcommand with a WRONG pi pin: the boot-seam verify
// must exit 85 (reason on stderr + termination log) BEFORE any opencode
// child spawn — the tri-overlay attribution contract made executable.
// The opencode overlay marker stays UNSET, proving the pi verify fires
// on its own contract, not as a side effect of opencode wiring.
func TestSuperviseOpencode_PiVerifyFailureExits85(t *testing.T) {
	dir := t.TempDir()
	path, _ := writePiStub(t, dir)
	const wrong = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	code, stderr := runSuperviseSubprocess(t,
		"PI_IMAGE_VOLUME=1",
		"LLMSAFESPACES_PI_BINARY="+path,
		"LLMSAFESPACES_PI_SHA256_AMD64="+wrong,
		"LLMSAFESPACES_PI_SHA256_ARM64="+wrong,
	)
	require.Equal(t, 85, code, "pi verify failure must exit 85 from the supervise-opencode boot seam; stderr: %s", stderr)
	require.Contains(t, stderr, "PiVerificationFailed")
	require.Contains(t, stderr, path, "the failure must name the binary that failed verification")
}

// TestSuperviseOpencode_PiOverlayMissingExits86: the marker demands an
// overlay but the binary path does not resolve → exit 86.
func TestSuperviseOpencode_PiOverlayMissingExits86(t *testing.T) {
	const good = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	code, stderr := runSuperviseSubprocess(t,
		"PI_IMAGE_VOLUME=1",
		"LLMSAFESPACES_PI_BINARY=/nonexistent/pi-tree/pi",
		"LLMSAFESPACES_PI_SHA256_AMD64="+good,
		"LLMSAFESPACES_PI_SHA256_ARM64="+good,
	)
	require.Equal(t, 86, code, "pi overlay missing must exit 86; stderr: %s", stderr)
	require.Contains(t, stderr, "PiOverlayMissing")
}

// TestSuperviseOpencode_BothOverlaysVerified_SupervisorServes: the happy
// path through the SAME real seam — opencode overlay verified AND the pi
// pin matching → the supervisor proceeds to normal operation (control
// socket serving). Mirrors
// TestSuperviseOpencode_OpenCodeOverlayMatch_SpawnsOverlayNotPath with
// the pi contract added: the pi verify must be a pass-through, not a
// boot blocker.
func TestSuperviseOpencode_BothOverlaysVerified_SupervisorServes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	spawnMarker := filepath.Join(dir, "spawn-argv0")
	oc, ocSHA := writeOverlayStub(t, dir,
		`echo "$0" >> "$SPAWN_ARGV0_MARKER"; exec sleep 3600`)
	piBin, piSHA := writePiStub(t, dir)

	cleanPathDir := t.TempDir()
	if _, err := exec.LookPath("opencode"); err == nil {
		t.Skip("an `opencode` binary is resolvable on the sanitized PATH; cannot prove non-PATH spawn")
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)

	//nolint:gosec // buildAgentdBinary output is the trusted test artifact
	cmd := exec.Command(bin, "supervise-opencode")
	cmd.Env = append(filteredEnviron(overlayEnvKeys()...),
		"PATH="+cleanPathDir+":/usr/bin:/bin",
		"HOME="+t.TempDir(),
		"WORKSPACE_ID=pi-overlay-test",
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+ocSHA,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+ocSHA,
		"PI_IMAGE_VOLUME=1",
		"LLMSAFESPACES_PI_BINARY="+piBin,
		"LLMSAFESPACES_PI_SHA256_AMD64="+piSHA,
		"LLMSAFESPACES_PI_SHA256_ARM64="+piSHA,
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR="+addr,
		"SPAWN_ARGV0_MARKER="+spawnMarker,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	serving := false
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			serving = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, serving, "control socket must serve with BOTH overlays verified — the pi verify passed through")
}
