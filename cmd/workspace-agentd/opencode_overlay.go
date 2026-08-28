// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// opencode_overlay.go — design 0053 S1 "opencodeDelivery", supervisor half:
// verify-before-spawn + overlay binary resolution for the opencode child.
//
// The controller mounts a digest-pinned image volume at /opencode on the
// workspace container and stamps the pod-spec env pins (immutable
// post-create, workspace-unwritable — the only integrity anchor the
// container itself cannot touch). When OPENCODE_IMAGE_VOLUME=1, the
// supervisor verifies the opencode binary's sha256 against the arch pin
// BEFORE the first spawn and execs the overlay path directly instead of
// the PATH lookup. Any other marker value (or unset) is the legacy baked
// binary: PATH lookup, no verify, byte-identical to pre-overlay behavior.
//
// Contracts (agreed with the controller-side delegation — do not deviate):
//
//	OPENCODE_IMAGE_VOLUME=1                    → overlay path enabled
//	LLMSAFESPACES_OPENCODE_BINARY              → binary path
//	  (default /opencode/usr/local/bin/opencode)
//	LLMSAFESPACES_OPENCODE_SHA256_AMD64/_ARM64 → 64-hex pins, arch selected
//	  by runtime.GOARCH; mismatch/missing-pin/unhashable → exit 83
//	overlay binary absent                      → exit 84
//
// Exit codes 81/82 are reserved for agentd self-verification
// (supervise_selfverify.go) and are never reused here. The failure reason
// goes to stderr AND best-effort /dev/termination-log so the controller
// can attribute the failure; an unwritable termination log never masks
// the exit code.
//
// The verify runs exactly once per process at the single seam both
// supervisor modes share: managedProcess.start()'s lazy factory default
// (single-container --supervise) and newSupervisorProcess (supervise-
// opencode, which also hands the resolved base to the control-socket
// adapter so spawn-env pushes wrap the verified factory).

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

const (
	envOpencodeImageVolume = "OPENCODE_IMAGE_VOLUME"
	envOpencodeBinary      = "LLMSAFESPACES_OPENCODE_BINARY"
	envOpencodeSHA256AMD64 = "LLMSAFESPACES_OPENCODE_SHA256_AMD64"
	envOpencodeSHA256ARM64 = "LLMSAFESPACES_OPENCODE_SHA256_ARM64"

	// opencodeOverlayBinaryDefault is the in-pod mount point of the
	// controller's image volume; the binary inside the volume keeps the
	// runtime image's /usr/local/bin/opencode layout.
	opencodeOverlayBinaryDefault = "/opencode/usr/local/bin/opencode"

	// opencodeExitVerifyFailed mirrors the agentd 81 contract for the
	// opencode child: missing arch pin, hash mismatch, or an
	// unreadable/unhashable binary.
	opencodeExitVerifyFailed = 83
	// opencodeExitOverlayMissing: the marker demands an overlay but the
	// binary file is absent (config/rollout error, not tampering).
	opencodeExitOverlayMissing = 84

	// envTerminationLogPath overrides the termination-log location. The
	// kernel always provides /dev/termination-log in a pod; the override
	// exists so the exit-code subprocess tests can point it at an
	// unwritable path (same env-override seam pattern as
	// LLMSAFESPACES_CONTROL_SOCKET_ADDR).
	envTerminationLogPath = "LLMSAFESPACES_TERMINATION_LOG_PATH"
	// terminationLogDefaultPath is kubelet's message file for pod
	// failure attribution.
	terminationLogDefaultPath = "/dev/termination-log"
)

// opencodeOverlayExitProceed is the zero exit code returned by
// opencodeOverlayDecision when the overlay contract allows the spawn.
const opencodeOverlayExitProceed = 0

// opencodeOverlayEnv is the resolved pin environment for the verify
// decision. Separated from os.Getenv and the filesystem so the decision
// logic is unit-testable.
type opencodeOverlayEnv struct {
	volumeFlag    string
	binaryPath    string
	binaryPresent bool
	amd64Pin      string
	arm64Pin      string
	arch          string
	actualSHA     string
}

// pinForArch maps runtime.GOARCH to its pin. The pin env names use the
// GOARCH vocabulary; any other arch has no pin and cannot be verified.
func (e opencodeOverlayEnv) pinForArch() string {
	switch e.arch {
	case "amd64":
		return e.amd64Pin
	case "arm64":
		return e.arm64Pin
	default:
		return ""
	}
}

// opencodeOverlayDecision evaluates the pin contract. (0, nil) = proceed;
// otherwise the controller-contract exit code (83/84) and the
// controller-parsable failure reason. Binary absence is checked first
// (mirroring the agentd entrypoint's verify order): a missing overlay is
// a rollout error regardless of pin state. An unreadable binary surfaces
// as an empty actual hash — a guaranteed mismatch, mirroring
// supervise_selfverify's fail-closed handling rather than inventing a
// hash.
func opencodeOverlayDecision(e opencodeOverlayEnv) (int, error) {
	if e.volumeFlag != "1" {
		return opencodeOverlayExitProceed, nil
	}
	if !e.binaryPresent {
		return opencodeExitOverlayMissing, fmt.Errorf(
			"OpencodeOverlayMissing: binary=%s node_arch=%s", e.binaryPath, e.arch)
	}
	expected := e.pinForArch()
	if expected == "" {
		return opencodeExitVerifyFailed, fmt.Errorf(
			"OpencodeVerificationFailed: no sha256 pin for arch %s", e.arch)
	}
	if e.actualSHA != expected {
		return opencodeExitVerifyFailed, fmt.Errorf(
			"OpencodeVerificationFailed: expected=%s got=%s binary=%s node_arch=%s",
			expected, e.actualSHA, e.binaryPath, e.arch)
	}
	return opencodeOverlayExitProceed, nil
}

// opencodeOverlayBinaryPath resolves the binary path: explicit env wins,
// unset/empty falls back to the in-pod overlay mount layout.
func opencodeOverlayBinaryPath(envValue string) string {
	if envValue != "" {
		return envValue
	}
	return opencodeOverlayBinaryDefault
}

// opencodeBinaryPresent reports whether path names a readable-as-file
// binary target. A directory at the path counts as absent — the mount
// did not land.
func opencodeBinaryPresent(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // fixed overlay-mount path from the pod spec
	return err == nil && !info.IsDir()
}

// opencodeOverlayCmdFactory builds the overlay spawn factory: identical
// argv/stdout/stderr/env construction to the legacy factory, with argv[0]
// pinned to the verified overlay path (direct exec, no PATH lookup).
func opencodeOverlayCmdFactory(path string) func() *exec.Cmd {
	return func() *exec.Cmd {
		return opencodeServeCmd(path)
	}
}

// resolveOpencodeSpawnFactory resolves this boot's base opencode child
// factory from the environment. Marker unset/other → the legacy
// PATH-lookup factory, byte-identical to pre-overlay behavior. Marker set
// → verify the overlay binary (existence, arch pin, sha256) and return a
// factory that execs it directly. Verify failures are returned, not
// exited, so in-process tests can inspect them; opencodeSpawnBaseFactory
// turns them into the contract exit.
func resolveOpencodeSpawnFactory() (func() *exec.Cmd, int, error) {
	if os.Getenv(envOpencodeImageVolume) != "1" {
		return defaultOpencodeCmdFactory, opencodeOverlayExitProceed, nil
	}

	path := opencodeOverlayBinaryPath(os.Getenv(envOpencodeBinary))
	env := opencodeOverlayEnv{
		volumeFlag:    "1",
		binaryPath:    path,
		binaryPresent: opencodeBinaryPresent(path),
		amd64Pin:      os.Getenv(envOpencodeSHA256AMD64),
		arm64Pin:      os.Getenv(envOpencodeSHA256ARM64),
		arch:          runtime.GOARCH,
	}
	if env.binaryPresent {
		if actual, err := sha256Path(path); err == nil {
			env.actualSHA = actual
		}
	}
	if code, err := opencodeOverlayDecision(env); err != nil {
		return nil, code, err
	}
	return opencodeOverlayCmdFactory(path), opencodeOverlayExitProceed, nil
}

// opencodeSpawnBaseFactory is the single supervisor-side seam: it returns
// the base factory every opencode spawn composes on. On overlay verify
// failure it reports the reason to stderr + termination log (best-effort)
// and exits 83/84 — before any child spawn, so opencode itself never
// crash-loops; the supervisor exit is the controller's signal.
//
// Callers resolve exactly once per process: managedProcess.start()'s lazy
// factory default (single-container --supervise) or newSupervisorProcess
// (supervise-opencode mode, which also hands the resolved base to the
// control-socket adapter so spawn-env pushes wrap the same factory
// instead of re-resolving).
func opencodeSpawnBaseFactory() func() *exec.Cmd {
	factory, code, err := resolveOpencodeSpawnFactory()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = writeTerminationLog(err.Error())
		os.Exit(code)
	}
	return factory
}

// writeTerminationLog writes msg to the termination log best-effort.
// Failures are returned (and ignored by the fatal paths) so an unwritable
// log can never mask a contract exit code.
func writeTerminationLog(msg string) error {
	return os.WriteFile(terminationLogPathOrDefault(), []byte(msg), 0o644) //nolint:gosec // G304: kubelet-provided path, or the test-seam override of it
}

// terminationLogPathOrDefault resolves the termination-log location:
// /dev/termination-log in a pod, the env override under test.
func terminationLogPathOrDefault() string {
	if p := os.Getenv(envTerminationLogPath); p != "" {
		return p
	}
	return terminationLogDefaultPath
}
