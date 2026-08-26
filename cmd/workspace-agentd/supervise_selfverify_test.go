// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervise_selfverify_test.go — design 0051 sidecar migration, step 2
// (TDD: authored before the implementation).
//
// With the main container's Command: set to the overlay supervisor
// binary (bypassing the baked entrypoint), the #863 verify-before-exec
// that entrypoint-common.sh performed in bash must move INTO the
// supervisor: it hashes its own binary (/proc/self/exe) against the
// pod-spec pin env (LLMSAFESPACES_AGENTD_SHA256_<ARCH>, immutable
// post-create, workspace-unwritable — the only integrity anchor).
//
// Exit-code and message contracts are pinned to the bash version so the
// controller's detectAgentdVerificationFailure keeps working unchanged:
//
//   - AGENTD_IMAGE_VOLUME unset → skip (legacy baked binary).
//   - pin mismatch → exit 81, stderr carries
//     "AgentdVerificationFailed: expected=<hex> got=<hex>".
//   - no pin for the arch → exit 81.
//   - pin match → proceed (no exit from the verify step).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSelfVerifyDecision pins the pure decision logic for every pin
// combination.
func TestSelfVerifyDecision(t *testing.T) {
	const good = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const bad = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cases := []struct {
		name    string
		volume  string
		amd64   string
		arm64   string
		arch    string
		actual  string
		wantErr string // "" = ok
	}{
		{"legacy: no volume flag", "", good, good, "x86_64", good, ""},
		{"match amd64", "1", good, "bad", "x86_64", good, ""},
		{"match arm64", "1", "bad", good, "aarch64", good, ""},
		{"mismatch", "1", good, "bad", "x86_64", bad, "AgentdVerificationFailed"},
		{"no pin for arch", "1", "", "", "x86_64", good, "no sha256 pin"},
		{"unknown arch", "1", good, good, "riscv64", good, "no sha256 pin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := selfVerifyDecision(selfVerifyEnv{
				volumeFlag: tc.volume,
				amd64Pin:   tc.amd64,
				arm64Pin:   tc.arm64,
				arch:       tc.arch,
				actualSHA:  tc.actual,
			})
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSelfVerifyMessageShape: the mismatch error carries the exact
// expected=/got= key=value form — detectAgentdVerificationFailure's
// event message parses it (agentd_overlay.go), same as the bash
// entrypoint's log_fail line.
func TestSelfVerifyMessageShape(t *testing.T) {
	err := selfVerifyDecision(selfVerifyEnv{
		volumeFlag: "1",
		amd64Pin:   "aaaa",
		arch:       "x86_64",
		actualSHA:  "cccc",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected=aaaa")
	assert.Contains(t, err.Error(), "got=cccc")
}

// TestSuperviseOpencode_SelfVerifyMismatch_Exit81: the REAL subcommand,
// as a subprocess, with a wrong pin → exits 81 BEFORE binding the
// control socket (no hang), stderr carries the detection marker.
func TestSuperviseOpencode_SelfVerifyMismatch_Exit81(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/exe self-hash is linux-only")
	}

	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	// A pin that cannot match: hash of empty input — wrong for both
	// arches, so the runner's arch is irrelevant.
	emptyHash := sha256Hex(nil)

	cmd := exec.Command(bin, "supervise-opencode")
	cmd.Env = append(os.Environ(),
		"AGENTD_IMAGE_VOLUME=1",
		"LLMSAFESPACES_AGENTD_SHA256_AMD64="+emptyHash,
		"LLMSAFESPACES_AGENTD_SHA256_ARM64="+emptyHash,
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=127.0.0.1:0",
	)
	stderr := filepath.Join(dir, "stderr")
	f, err := os.Create(stderr)
	require.NoError(t, err)
	cmd.Stderr = f
	cmd.Stdout = f

	runErr := cmd.Run()
	require.Error(t, runErr, "verify failure must exit non-zero")
	exitErr, ok := runErr.(*exec.ExitError)
	require.True(t, ok, "got %v", runErr)
	require.Equal(t, 81, exitErr.ExitCode(), "mismatch keeps the bash 81 contract")
	_ = f.Close()

	out, err := os.ReadFile(stderr)
	require.NoError(t, err)
	require.Contains(t, string(out), "AgentdVerificationFailed: expected=")
	require.Contains(t, string(out), "got=")
}
