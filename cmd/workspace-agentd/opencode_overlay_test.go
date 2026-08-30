// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// opencode_overlay_test.go — design 0053 S1 "opencodeDelivery", the
// supervisor half (TDD: authored before the implementation).
//
// The controller mounts a digest-pinned image volume at /opencode and
// stamps OPENCODE_IMAGE_VOLUME=1 + LLMSAFESPACES_OPENCODE_SHA256_<ARCH>
// pins. The supervisor must, BEFORE the first opencode spawn:
//
//   - marker unset/other  → today's PATH lookup, byte-identical, no verify;
//   - marker set, match   → exec the overlay binary path directly;
//   - marker set, mismatch / no pin for arch / unhashable binary
//                        → exit 83 (OpencodeVerificationFailed);
//   - marker set, binary absent → exit 84 (OpencodeOverlayMissing).
//
// Failure reason goes to stderr AND best-effort /dev/termination-log so
// the controller can attribute the failure. Exit codes 81/82 stay
// reserved for agentd self-verification. The verify runs exactly once
// per process, in BOTH supervisor modes (supervise-opencode sidecar
// split and --supervise single-container), at the one seam where
// defaultOpencodeCmdFactory is engaged.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// --- shared helpers ----------------------------------------------------------

// overlayEnvKeys are every environment key these tests control; subprocess
// environments filter them from os.Environ() first so inherited values
// (this dev workspace itself runs under the AGENTD overlay) cannot leak in.
func overlayEnvKeys() []string {
	return []string{
		envOpencodeImageVolume,
		envOpencodeBinary,
		envOpencodeSHA256AMD64,
		envOpencodeSHA256ARM64,
		envTerminationLogPath,
		"AGENTD_IMAGE_VOLUME",
		"LLMSAFESPACES_AGENTD_BINARY",
		"LLMSAFESPACES_AGENTD_SHA256_AMD64",
		"LLMSAFESPACES_AGENTD_SHA256_ARM64",
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR",
		"PATH",
	}
}

// filteredEnviron returns os.Environ() minus the given keys.
func filteredEnviron(skip ...string) []string {
	skipSet := make(map[string]struct{}, len(skip))
	for _, k := range skip {
		skipSet[k] = struct{}{}
	}
	out := make([]string, 0, 64)
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if _, dropped := skipSet[key]; dropped {
			continue
		}
		out = append(out, e)
	}
	return out
}

// writeOverlayStub writes a fake opencode binary and returns its path and
// sha256 (via the production hasher).
func writeOverlayStub(t *testing.T, dir, body string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "opencode-overlay-bin")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	sha, err := sha256Path(path)
	require.NoError(t, err)
	return path, sha
}

// setOverlayEnv pins the full overlay contract via t.Setenv.
func setOverlayEnv(t *testing.T, binary, amd64Pin, arm64Pin string) {
	t.Helper()
	t.Setenv(envOpencodeImageVolume, "1")
	t.Setenv(envOpencodeBinary, binary)
	t.Setenv(envOpencodeSHA256AMD64, amd64Pin)
	t.Setenv(envOpencodeSHA256ARM64, arm64Pin)
}

// runSuperviseSubprocess executes the REAL `supervise-opencode` subcommand
// with the overlay env isolated, returning (exitCode, stderr).
func runSuperviseSubprocess(t *testing.T, extraEnv ...string) (int, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildAgentdBinary(t)
	dir := t.TempDir()
	stderrPath := filepath.Join(dir, "stderr")
	f, err := os.Create(stderrPath)
	require.NoError(t, err)

	//nolint:gosec // buildAgentdBinary output is the trusted test artifact
	cmd := exec.Command(bin, "supervise-opencode")
	cmd.Env = append(filteredEnviron(overlayEnvKeys()...),
		"LLMSAFESPACES_CONTROL_SOCKET_ADDR=127.0.0.1:0",
		"HOME="+t.TempDir(),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	_ = f.Close()

	out, err := os.ReadFile(stderrPath)
	require.NoError(t, err)
	code := 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else {
		require.NoError(t, runErr, "subprocess must run to an exit code, not fail to launch")
	}
	return code, string(out)
}

// --- pure decision logic -----------------------------------------------------

// TestOpencodeOverlayBinaryPath pins the binary-path contract: explicit
// env wins, unset/empty falls back to the in-pod overlay mount point.
func TestOpencodeOverlayBinaryPath(t *testing.T) {
	require.Equal(t, "/opencode/usr/local/bin/opencode", opencodeOverlayBinaryPath(""))
	require.Equal(t, "/custom/opencode", opencodeOverlayBinaryPath("/custom/opencode"))
}

// TestOpencodeOverlayDecision pins the pure verify decision for every
// contract combination. wantCode 0 = proceed.
func TestOpencodeOverlayDecision(t *testing.T) {
	const good = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const bad = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

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
		{"mismatch amd64", "1", true, good, bad, "amd64", bad, 83, "OpencodeVerificationFailed"},
		{"mismatch arm64", "1", true, bad, good, "arm64", bad, 83, "OpencodeVerificationFailed"},
		{"no pin for arch amd64", "1", true, "", "", "amd64", good, 83, "no sha256 pin for arch amd64"},
		{"no pin for arch arm64", "1", true, good, "", "arm64", good, 83, "no sha256 pin for arch arm64"},
		{"unknown arch has no pin", "1", true, good, good, "riscv64", good, 83, "no sha256 pin"},
		{"overlay missing", "1", false, good, good, "amd64", "", 84, "OpencodeOverlayMissing"},
		{"overlay missing precedes pin check", "1", false, "", "", "amd64", "", 84, "OpencodeOverlayMissing"},
		{"unreadable binary hashes empty: fail closed", "1", true, good, bad, "amd64", "", 83, "OpencodeVerificationFailed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := opencodeOverlayDecision(opencodeOverlayEnv{
				volumeFlag:    tc.volume,
				binaryPath:    "/opencode/usr/local/bin/opencode",
				binaryPresent: tc.present,
				amd64Pin:      tc.amd64,
				arm64Pin:      tc.arm64,
				arch:          tc.arch,
				actualSHA:     tc.actual,
			})
			if tc.wantCode == 0 {
				require.NoError(t, err)
				require.Zero(t, code)
				return
			}
			require.Error(t, err)
			require.Equal(t, tc.wantCode, code, "error: %s", err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestOpencodeOverlayMessageShape pins the exact event-message forms the
// controller parses (mirrors the agentd self-verify shapes).
func TestOpencodeOverlayMessageShape(t *testing.T) {
	mismatchCode, mismatchErr := opencodeOverlayDecision(opencodeOverlayEnv{
		volumeFlag:    "1",
		binaryPath:    "/opencode/usr/local/bin/opencode",
		binaryPresent: true,
		amd64Pin:      "aaaa",
		arch:          "amd64",
		actualSHA:     "cccc",
	})
	require.Error(t, mismatchErr)
	require.Equal(t, opencodeExitVerifyFailed, mismatchCode)
	require.EqualError(t, mismatchErr,
		"OpencodeVerificationFailed: expected=aaaa got=cccc binary=/opencode/usr/local/bin/opencode node_arch=amd64")

	missingCode, missingErr := opencodeOverlayDecision(opencodeOverlayEnv{
		volumeFlag:    "1",
		binaryPath:    "/opencode/usr/local/bin/opencode",
		binaryPresent: false,
		amd64Pin:      "aaaa",
		arch:          "arm64",
	})
	require.Error(t, missingErr)
	require.Equal(t, opencodeExitOverlayMissing, missingCode)
	require.EqualError(t, missingErr,
		"OpencodeOverlayMissing: binary=/opencode/usr/local/bin/opencode node_arch=arm64")

	noPinCode, noPinErr := opencodeOverlayDecision(opencodeOverlayEnv{
		volumeFlag:    "1",
		binaryPath:    "/opencode/usr/local/bin/opencode",
		binaryPresent: true,
		arch:          "arm64",
	})
	require.Error(t, noPinErr)
	require.Equal(t, opencodeExitVerifyFailed, noPinCode)
	require.EqualError(t, noPinErr, "OpencodeVerificationFailed: no sha256 pin for arch arm64")
}

// --- factory resolution (in-process; failure returns, never exits) -----------

// TestResolveOpencodeSpawnFactory_LegacyIsPathLookupByteIdentical: with the
// marker unset the seam must resolve to EXACTLY today's factory — same
// function, same PATH-lookup argv, same stdout/stderr/env construction.
func TestResolveOpencodeSpawnFactory_LegacyIsPathLookupByteIdentical(t *testing.T) {
	t.Setenv(envOpencodeImageVolume, "")

	factory, code, ferr := resolveOpencodeSpawnFactory()
	require.Zero(t, code)
	require.NoError(t, ferr)
	require.NotNil(t, factory)
	require.Equal(t,
		reflect.ValueOf(defaultOpencodeCmdFactory).Pointer(),
		reflect.ValueOf(factory).Pointer(),
		"marker unset must resolve to the production PATH-lookup factory, unchanged")

	cmd := factory()
	// argv[0] stays the bare PATH-lookup name (exec.Command resolves it at
	// construction; cmd.Path is therefore environment-dependent).
	require.Equal(t, "opencode", cmd.Args[0], "legacy mode keeps the PATH-lookup argv")
	require.Equal(t, []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", strconv.Itoa(agentd.AgentPort)}, cmd.Args)
	require.Equal(t, os.Stdout, cmd.Stdout)
	require.Equal(t, os.Stderr, cmd.Stderr)
	require.Equal(t, defaultOpencodeCmdFactory().Env, cmd.Env)
}

// TestResolveOpencodeSpawnFactory_OverlayMatchSpawnsOverlayPathDirectly: on a
// verified match the child argv[0] is the overlay path; stdout/stderr wiring
// and env construction are identical to the legacy factory.
func TestResolveOpencodeSpawnFactory_OverlayMatchSpawnsOverlayPathDirectly(t *testing.T) {
	oc, sha := writeOverlayStub(t, t.TempDir(), "exit 0")
	setOverlayEnv(t, oc, sha, sha)

	factory, code, ferr := resolveOpencodeSpawnFactory()
	require.Zero(t, code)
	require.NoError(t, ferr)
	cmd := factory()
	require.Equal(t, oc, cmd.Path, "overlay mode must exec the overlay path, not PATH-lookup")
	require.Equal(t, []string{oc, "serve", "--hostname", "0.0.0.0", "--port", strconv.Itoa(agentd.AgentPort)}, cmd.Args)
	require.Equal(t, os.Stdout, cmd.Stdout)
	require.Equal(t, os.Stderr, cmd.Stderr)
	require.Equal(t, defaultOpencodeCmdFactory().Env, cmd.Env,
		"overlay spawn keeps env construction identical to the legacy factory")
}

// TestResolveOpencodeSpawnFactory_MismatchFailsClosed: hash mismatch returns
// the 83 error (never a factory), carrying the controller-parsable shape.
func TestResolveOpencodeSpawnFactory_MismatchFailsClosed(t *testing.T) {
	oc, _ := writeOverlayStub(t, t.TempDir(), "exit 0")
	wrong := strings.Repeat("d", 64)
	setOverlayEnv(t, oc, wrong, wrong)

	factory, code, ferr := resolveOpencodeSpawnFactory()
	require.Nil(t, factory)
	require.Error(t, ferr)
	require.Equal(t, opencodeExitVerifyFailed, code)
	assert.Contains(t, ferr.Error(), "OpencodeVerificationFailed: expected="+wrong)
	assert.Contains(t, ferr.Error(), "binary="+oc)
	assert.Contains(t, ferr.Error(), "node_arch="+runtime.GOARCH)
}

// TestResolveOpencodeSpawnFactory_MissingBinaryReturns84.
func TestResolveOpencodeSpawnFactory_MissingBinaryReturns84(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent-opencode")
	good := strings.Repeat("a", 64)
	setOverlayEnv(t, absent, good, good)

	factory, code, ferr := resolveOpencodeSpawnFactory()
	require.Nil(t, factory)
	require.Error(t, ferr)
	require.Equal(t, opencodeExitOverlayMissing, code)
	require.Contains(t, ferr.Error(), "OpencodeOverlayMissing: binary="+absent)
	require.Contains(t, ferr.Error(), "node_arch="+runtime.GOARCH)
}

// TestResolveOpencodeSpawnFactory_MissingArchPinReturns83: pins unset →
// cannot verify → 83, regardless of the binary hashing fine.
func TestResolveOpencodeSpawnFactory_MissingArchPinReturns83(t *testing.T) {
	oc, _ := writeOverlayStub(t, t.TempDir(), "exit 0")
	setOverlayEnv(t, oc, "", "")

	factory, code, ferr := resolveOpencodeSpawnFactory()
	require.Nil(t, factory)
	require.Error(t, ferr)
	require.Equal(t, opencodeExitVerifyFailed, code)
	require.Contains(t, ferr.Error(), "no sha256 pin for arch "+runtime.GOARCH)
}

// --- wiring: one seam, both modes, exactly once -------------------------------

// TestNewSupervisorProcess_SharesOverlayBaseWithAdapter: supervisor mode
// resolves the base ONCE at construction and hands the SAME factory to the
// socket adapter, so a spawn-env push wraps the verified overlay base —
// never regressing to the PATH-lookup default.
func TestNewSupervisorProcess_SharesOverlayBaseWithAdapter(t *testing.T) {
	oc, sha := writeOverlayStub(t, t.TempDir(), "exec sleep 3600")
	setOverlayEnv(t, oc, sha, sha)

	proc, adapter := newSupervisorProcess()
	require.NotNil(t, proc.cmdFactory,
		"supervisor mode resolves the opencode spawn base at construction (verify before socket, before spawn)")
	require.Equal(t, oc, proc.cmdFactory().Path)
	require.NotNil(t, adapter.baseCmdFactory,
		"the adapter must carry the SAME resolved base — SetSpawnEnv must not re-resolve or fall back to PATH lookup")
	require.Equal(t, oc, adapter.baseCmdFactory().Path)

	adapter.SetSpawnEnv(map[string]string{"X": "1"})
	cmd := proc.cmdFactory()
	require.Equal(t, oc, cmd.Path, "the spawn-env wrapper must keep the verified overlay binary as the child")
	require.Contains(t, cmd.Env, "X=1")
}

// TestManagedProcess_StartUsesOverlayFactory: the single-container seam —
// start() with no injected factory must resolve the overlay (verify) and
// spawn from it, not from PATH.
func TestManagedProcess_StartUsesOverlayFactory(t *testing.T) {
	oc, sha := writeOverlayStub(t, t.TempDir(), "exec sleep 3600")
	setOverlayEnv(t, oc, sha, sha)

	p := &managedProcess{}
	p.start()
	t.Cleanup(p.stop)

	require.Eventually(t, func() bool { return p.pid() > 0 },
		5*time.Second, 50*time.Millisecond,
		"single-container start() must spawn the verified overlay binary")
	p.mu.Lock()
	factory := p.cmdFactory
	p.mu.Unlock()
	require.NotNil(t, factory)
	require.Equal(t, oc, factory().Path,
		"the lazy default must install the overlay factory, not defaultOpencodeCmdFactory")
}

// --- supervisor mode: real subcommand, real exit codes ------------------------

// TestSuperviseOpencode_OpenCodeOverlayMismatch_Exit83: wrong pin → exit 83
// before the control socket or any child, stderr + termination-log carry the
// expected=/got= event shape.
func TestSuperviseOpencode_OpenCodeOverlayMismatch_Exit83(t *testing.T) {
	dir := t.TempDir()
	oc, actual := writeOverlayStub(t, dir, "exit 0")
	wrong := strings.Repeat("d", 64)
	termLog := filepath.Join(dir, "termination-log")

	code, stderr := runSuperviseSubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+wrong,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+wrong,
		"LLMSAFESPACES_TERMINATION_LOG_PATH="+termLog,
	)
	require.Equal(t, 83, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeVerificationFailed: expected="+wrong, "stderr: %s", stderr)
	require.Contains(t, stderr, "got="+actual, "stderr: %s", stderr)
	require.Contains(t, stderr, "binary="+oc, "stderr: %s", stderr)
	require.Contains(t, stderr, "node_arch="+runtime.GOARCH, "stderr: %s", stderr)

	logBody, err := os.ReadFile(termLog)
	require.NoError(t, err, "termination log must be written best-effort on verify failure")
	require.Contains(t, string(logBody), "OpencodeVerificationFailed: expected=")
}

// TestSuperviseOpencode_OpenCodeOverlayMissing_Exit84: marker set, binary
// absent → exit 84 with the OpencodeOverlayMissing attribution.
func TestSuperviseOpencode_OpenCodeOverlayMissing_Exit84(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "no-such-opencode")
	good := strings.Repeat("a", 64)
	termLog := filepath.Join(dir, "termination-log")

	code, stderr := runSuperviseSubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+absent,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+good,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+good,
		"LLMSAFESPACES_TERMINATION_LOG_PATH="+termLog,
	)
	require.Equal(t, 84, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeOverlayMissing: binary="+absent, "stderr: %s", stderr)

	logBody, err := os.ReadFile(termLog)
	require.NoError(t, err)
	require.Contains(t, string(logBody), "OpencodeOverlayMissing: binary="+absent)
}

// TestSuperviseOpencode_OpenCodeOverlayNoPinForArch_Exit83: binary present
// and hashable, pins unset → cannot verify → 83.
func TestSuperviseOpencode_OpenCodeOverlayNoPinForArch_Exit83(t *testing.T) {
	oc, _ := writeOverlayStub(t, t.TempDir(), "exit 0")

	code, stderr := runSuperviseSubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
	)
	require.Equal(t, 83, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeVerificationFailed: no sha256 pin for arch "+runtime.GOARCH,
		"stderr: %s", stderr)
}

// TestSuperviseOpencode_OpenCodeOverlayDefaultPath_UsedWhenEnvUnset: marker
// set but LLMSAFESPACES_OPENCODE_BINARY unset → the default in-pod overlay
// path is verified (absent here → 84 naming the default path).
func TestSuperviseOpencode_OpenCodeOverlayDefaultPath_UsedWhenEnvUnset(t *testing.T) {
	if _, err := os.Stat(opencodeOverlayBinaryDefault); err == nil {
		t.Skipf("%s exists on this machine; default-path resolution cannot be observed", opencodeOverlayBinaryDefault)
	}

	code, stderr := runSuperviseSubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_TERMINATION_LOG_PATH="+filepath.Join(t.TempDir(), "termination-log"),
	)
	require.Equal(t, 84, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeOverlayMissing: binary="+opencodeOverlayBinaryDefault,
		"stderr: %s", stderr)
}

// TestSuperviseOpencode_OpenCodeOverlayUnwritableTerminationLog_DoesNotMaskExit:
// an unwritable termination log (here: a directory — EISDIR) must never mask
// the contract exit codes. Mirrors the entrypoint's `|| true` guarantee.
func TestSuperviseOpencode_OpenCodeOverlayUnwritableTerminationLog_DoesNotMaskExit(t *testing.T) {
	dir := t.TempDir()
	oc, actual := writeOverlayStub(t, dir, "exit 0")
	wrong := strings.Repeat("e", 64)
	good := strings.Repeat("a", 64)
	dirAsLog := t.TempDir() // a directory: os.WriteFile fails with EISDIR

	t.Run("mismatch still exits 83", func(t *testing.T) {
		code, stderr := runSuperviseSubprocess(t,
			"OPENCODE_IMAGE_VOLUME=1",
			"LLMSAFESPACES_OPENCODE_BINARY="+oc,
			"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+wrong,
			"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+wrong,
			"LLMSAFESPACES_TERMINATION_LOG_PATH="+dirAsLog,
		)
		require.Equal(t, 83, code, "stderr: %s", stderr)
		require.Contains(t, stderr, "OpencodeVerificationFailed")
		require.Contains(t, stderr, "got="+actual)
	})

	t.Run("missing still exits 84", func(t *testing.T) {
		code, stderr := runSuperviseSubprocess(t,
			"OPENCODE_IMAGE_VOLUME=1",
			"LLMSAFESPACES_OPENCODE_BINARY="+filepath.Join(dir, "absent"),
			"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+good,
			"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+good,
			"LLMSAFESPACES_TERMINATION_LOG_PATH="+dirAsLog,
		)
		require.Equal(t, 84, code, "stderr: %s", stderr)
		require.Contains(t, stderr, "OpencodeOverlayMissing")
	})
}

// TestSuperviseOpencode_OpenCodeOverlayMatch_SpawnsOverlayNotPath: verified
// overlay → the real supervisor's child is exec'd FROM the overlay path with
// no opencode resolvable on PATH, and a socket spawn-env push + restart keeps
// the overlay base (US-4a wrapper composes on the verified factory).
func TestSuperviseOpencode_OpenCodeOverlayMatch_SpawnsOverlayNotPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	spawnMarker := filepath.Join(dir, "spawn-argv0")
	oc, sha := writeOverlayStub(t, dir,
		`echo "$0" >> "$SPAWN_ARGV0_MARKER"; exec sleep 3600`)

	// A PATH with NO opencode anywhere: a PATH-lookup regression fails to
	// start any child, making the regression loud instead of silent.
	cleanPathDir := t.TempDir()
	t.Setenv("PATH", cleanPathDir+":/usr/bin:/bin")
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
		"WORKSPACE_ID=opencode-overlay-test",
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+sha,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+sha,
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
		if cmd.ProcessState != nil {
			t.Fatalf("supervisor exited before serving: %v", cmd.ProcessState)
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, serving, "verified overlay must let the supervisor boot normally")

	cc := newControlClient(addr)
	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.ChildPID > 0
	}, 10*time.Second, 100*time.Millisecond, "the supervisor must spawn the overlay child")

	require.Eventually(t, func() bool {
		_, err := os.Stat(spawnMarker)
		return err == nil
	}, 5*time.Second, 50*time.Millisecond)

	// Every spawn's argv[0] is the overlay path — direct exec, no PATH
	// lookup. Polled: the marker append happens at shell start, which can
	// trail the child's /proc visibility by a few ms.
	assertSpawnArgv0IsOverlay := func(minLines int) {
		require.Eventually(t, func() bool {
			body, err := os.ReadFile(spawnMarker)
			if err != nil {
				return false
			}
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if len(lines) < minLines {
				return false
			}
			for _, line := range lines {
				if line != oc {
					return false
				}
			}
			return true
		}, 5*time.Second, 50*time.Millisecond,
			"child argv[0] must be the overlay path %s on every spawn", oc)
	}
	assertSpawnArgv0IsOverlay(1)

	// The sidecar's spawn-env handoff must wrap the SAME overlay base.
	require.NoError(t, cc.SpawnEnv(context.Background(), map[string]string{"PROBE_VAR": "overlay-kept"}))
	_, err := cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		if err != nil || st.ChildPID <= 0 {
			return false
		}
		data, err := os.ReadFile("/proc/" + strconv.Itoa(st.ChildPID) + "/environ")
		if err != nil {
			return false
		}
		return strings.Contains(string(data), "PROBE_VAR=overlay-kept\x00")
	}, 15*time.Second, 100*time.Millisecond, "the socket-handed delta must reach the next overlay child")
	assertSpawnArgv0IsOverlay(2)

	// Clean shutdown still exits 0.
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not shut down after SIGINT")
	}
	require.Equal(t, 0, cmd.ProcessState.ExitCode())
}

// --- single-container mode: the --supervise spawn path ------------------------

// TestSingleContainerOverlayHelperProcess is the re-exec entry: it drives
// startManagedProcess — the exact path main() takes for --supervise. On
// verify failure the process exits 83/84 from inside start(); on success a
// managed child runs and writes its argv[0] to the spawn marker.
func TestSingleContainerOverlayHelperProcess(t *testing.T) {
	if os.Getenv("GO_TEST_SINGLE_CONTAINER_OVERLAY") != "1" {
		return
	}
	proc := startManagedProcess(true, nil, nil)
	if proc == nil {
		fmt.Fprintln(os.Stderr, "helper: no managed process returned")
		os.Exit(9)
	}
	marker := os.Getenv("SINGLE_CONTAINER_SPAWN_MARKER")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	proc.stop()
	os.Exit(0)
}

// runSingleContainerOverlaySubprocess re-execs the helper with the given
// overlay env, returning (exitCode, stderr).
func runSingleContainerOverlaySubprocess(t *testing.T, extraEnv ...string) (int, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	//nolint:gosec // os.Args[0] is the trusted test binary path
	cmd := exec.Command(os.Args[0], "-test.run=TestSingleContainerOverlayHelperProcess", "-test.v")
	cmd.Env = append(filteredEnviron(overlayEnvKeys()...),
		"PATH=/usr/bin:/bin",
		"HOME="+t.TempDir(),
		"GO_TEST_SINGLE_CONTAINER_OVERLAY=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else {
		require.NoError(t, runErr, "helper must run to an exit code, not fail to launch")
	}
	return code, stderr.String()
}

// TestSingleContainerOverlay_MatchSpawnsOverlayBinary: verified overlay →
// the single-container supervisor spawns it (marker carries argv[0]) and
// shuts down cleanly (exit 0).
func TestSingleContainerOverlay_MatchSpawnsOverlayBinary(t *testing.T) {
	dir := t.TempDir()
	oc, sha := writeOverlayStub(t, dir,
		`echo "$0" > "$SINGLE_CONTAINER_SPAWN_MARKER"; exec sleep 3600`)
	marker := filepath.Join(dir, "spawn-argv0")

	code, stderr := runSingleContainerOverlaySubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+sha,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+sha,
		"SINGLE_CONTAINER_SPAWN_MARKER="+marker,
	)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	body, err := os.ReadFile(marker)
	require.NoError(t, err, "the single-container supervisor must have spawned the overlay child")
	require.Equal(t, oc, strings.TrimSpace(string(body)),
		"child argv[0] must be the overlay path (direct exec, no PATH lookup)")
}

// TestSingleContainerOverlay_VerifyFailureExits83BeforeSpawn: wrong pin →
// the supervisor process exits 83 and opencode NEVER starts (spawn marker
// absent) — the supervisor exit is the signal, no opencode crash-loop.
func TestSingleContainerOverlay_VerifyFailureExits83BeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	oc, _ := writeOverlayStub(t, dir,
		`echo "$0" > "$SINGLE_CONTAINER_SPAWN_MARKER"; exec sleep 3600`)
	wrong := strings.Repeat("d", 64)
	marker := filepath.Join(dir, "spawn-argv0")
	termLog := filepath.Join(dir, "termination-log")

	code, stderr := runSingleContainerOverlaySubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+oc,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+wrong,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+wrong,
		"SINGLE_CONTAINER_SPAWN_MARKER="+marker,
		"LLMSAFESPACES_TERMINATION_LOG_PATH="+termLog,
	)
	require.Equal(t, 83, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeVerificationFailed: expected="+wrong, "stderr: %s", stderr)
	require.Contains(t, stderr, "binary="+oc, "stderr: %s", stderr)

	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist,
		"opencode must never start when verify fails — supervisor exit is the signal")
	logBody, err := os.ReadFile(termLog)
	require.NoError(t, err)
	require.Contains(t, string(logBody), "OpencodeVerificationFailed: expected=")
}

// TestSingleContainerOverlay_MissingBinaryExits84BeforeSpawn: the 84 leg of
// the single-container mode, same no-spawn guarantee.
func TestSingleContainerOverlay_MissingBinaryExits84BeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent-opencode")
	good := strings.Repeat("a", 64)
	marker := filepath.Join(dir, "spawn-argv0")

	code, stderr := runSingleContainerOverlaySubprocess(t,
		"OPENCODE_IMAGE_VOLUME=1",
		"LLMSAFESPACES_OPENCODE_BINARY="+absent,
		"LLMSAFESPACES_OPENCODE_SHA256_AMD64="+good,
		"LLMSAFESPACES_OPENCODE_SHA256_ARM64="+good,
		"SINGLE_CONTAINER_SPAWN_MARKER="+marker,
	)
	require.Equal(t, 84, code, "stderr: %s", stderr)
	require.Contains(t, stderr, "OpencodeOverlayMissing: binary="+absent, "stderr: %s", stderr)

	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "no opencode spawn on overlay-missing failure")
}
