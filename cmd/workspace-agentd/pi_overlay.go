// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_overlay.go — the second-harness overlay's supervisor half (design
// 0053 §5.4, scoping report §2.3 story S1): verify-before-spawn for the
// pi tree, WITHOUT spawning it yet (the harness driver is the S2 story;
// harness selection itself is S5).
//
// The controller mounts a digest-pinned image volume at /pi on the
// workspace container and stamps the pod-spec env pins (immutable
// post-create, workspace-unwritable — the only integrity anchor the
// container itself cannot touch). When PI_IMAGE_VOLUME=1, the supervisor
// verifies the pi BINARY's sha256 against the arch pin once per process
// at boot — the same seam and ordering as the opencode overlay verify —
// and exits on failure so the controller attributes it. Nothing spawns
// pi in this story; the verify exists because the mount does: a
// delivered-but-unverified tree would make the pins decorative and the
// PiVerified condition vacuous.
//
// Trust ruling (recorded in runtimes/pi/Dockerfile): hash-the-binary +
// digest-the-tree — the binary carries the per-arch sha256 contract;
// the rest of the tree is non-executable data covered by the image
// digest (the #1416 CA-bundle / #1469 plugin precedent).
//
// Contracts (agreed with the controller-side delegation — do not deviate):
//
//	PI_IMAGE_VOLUME=1                    → overlay verify enabled
//	LLMSAFESPACES_PI_BINARY              → binary path (default /pi/pi)
//	LLMSAFESPACES_PI_SHA256_AMD64/_ARM64 → 64-hex pins, arch selected
//	  by runtime.GOARCH; mismatch/missing-pin/unhashable → exit 85
//	overlay binary absent                → exit 86
//
// Exit codes 81/82 are agentd self-verification (supervise_selfverify)
// and 83/84 the opencode overlay's; 85/86 stay disjoint from both so a
// tri-overlay pod's failure attribution never crosses artifacts. (The
// CONTROLLER process's own exit 85 — relay staging not armed,
// controller/controller.go — is a different process on a different pod.)
//
// The verify runs at most once per process (sync.Once): called from BOTH
// supervisor boot paths — managedProcess.start()'s lazy factory default
// (single-container --supervise) and newSupervisorProcess (supervise-
// opencode) — whichever is reached first.

import (
	"fmt"
	"os"
	"runtime"
	"sync"
)

const (
	envPiImageVolume = "PI_IMAGE_VOLUME"
	envPiBinary      = "LLMSAFESPACES_PI_BINARY"
	envPiSHA256AMD64 = "LLMSAFESPACES_PI_SHA256_AMD64"
	envPiSHA256ARM64 = "LLMSAFESPACES_PI_SHA256_ARM64"

	// piOverlayBinaryDefault is the in-pod mount point of the
	// controller's image volume; the binary sits at the TREE ROOT
	// (/pi/pi), preserving the upstream release layout — bun's
	// --compile-autoload-package-json resolves a package.json NEXT TO
	// the executable and native prebuilds sit at fixed relative paths,
	// so relocating the binary into e.g. /pi/bin would silently break
	// runtime resolution (the ruling lives in runtimes/pi/Dockerfile).
	piOverlayBinaryDefault = "/pi/pi"

	// piExitVerifyFailed mirrors the agentd/opencode 81/83 contract for
	// the pi child: missing arch pin, hash mismatch, or an
	// unreadable/unhashable binary.
	piExitVerifyFailed = 85
	// piExitOverlayMissing: the marker demands an overlay but the binary
	// file is absent (config/rollout error, not tampering).
	piExitOverlayMissing = 86
)

// piOverlayExitProceed is the zero exit code returned by
// piOverlayDecision when the overlay contract allows boot.
const piOverlayExitProceed = 0

// piOverlayEnv is the resolved pin environment for the verify decision.
// Separated from os.Getenv and the filesystem so the decision logic is
// unit-testable (same shape as opencodeOverlayEnv).
type piOverlayEnv struct {
	volumeFlag    string
	binaryPath    string
	binaryPresent bool
	amd64Pin      string
	arm64Pin      string
	arch          string
	actualSHA     string
}

// pinForArch maps runtime.GOARCH to its pin.
func (e piOverlayEnv) pinForArch() string {
	switch e.arch {
	case "amd64":
		return e.amd64Pin
	case "arm64":
		return e.arm64Pin
	default:
		return ""
	}
}

// piOverlayDecision evaluates the pin contract. (0, nil) = proceed;
// otherwise the controller-contract exit code (85/86) and the
// controller-parsable failure reason. Binary absence is checked first
// (the agentd entrypoint's verify order); an unreadable binary surfaces
// as an empty actual hash — a guaranteed mismatch, fail-closed.
func piOverlayDecision(e piOverlayEnv) (int, error) {
	if e.volumeFlag != "1" {
		return piOverlayExitProceed, nil
	}
	if !e.binaryPresent {
		return piExitOverlayMissing, fmt.Errorf(
			"PiOverlayMissing: binary=%s node_arch=%s", e.binaryPath, e.arch)
	}
	expected := e.pinForArch()
	if expected == "" {
		return piExitVerifyFailed, fmt.Errorf(
			"PiVerificationFailed: no sha256 pin for arch %s", e.arch)
	}
	if e.actualSHA != expected {
		return piExitVerifyFailed, fmt.Errorf(
			"PiVerificationFailed: expected=%s got=%s binary=%s node_arch=%s",
			expected, e.actualSHA, e.binaryPath, e.arch)
	}
	return piOverlayExitProceed, nil
}

// piOverlayBinaryPath resolves the binary path: explicit env wins, unset
// falls back to the in-pod overlay mount layout.
func piOverlayBinaryPath(envValue string) string {
	if envValue != "" {
		return envValue
	}
	return piOverlayBinaryDefault
}

// piBinaryPresent reports whether path names a readable-as-file binary
// target. A directory at the path counts as absent — the mount did not
// land.
func piBinaryPresent(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // fixed overlay-mount path from the pod spec
	return err == nil && !info.IsDir()
}

var piVerifyOnce sync.Once

// verifyPiOverlayAtBoot resolves the pi overlay contract once per
// process and exits 85/86 on failure (stderr + termination log
// best-effort, same as opencodeSpawnBaseFactory). Marker unset/other →
// a no-op proceed (pods from a pre-pi controller never carried the
// overlay). Idempotent: both supervisor boot paths call it; the sync.
// Once makes the second call a cheap pass-through.
func verifyPiOverlayAtBoot() {
	piVerifyOnce.Do(func() {
		if os.Getenv(envPiImageVolume) != "1" {
			return
		}
		path := piOverlayBinaryPath(os.Getenv(envPiBinary))
		env := piOverlayEnv{
			volumeFlag:    "1",
			binaryPath:    path,
			binaryPresent: piBinaryPresent(path),
			amd64Pin:      os.Getenv(envPiSHA256AMD64),
			arm64Pin:      os.Getenv(envPiSHA256ARM64),
			arch:          runtime.GOARCH,
		}
		if env.binaryPresent {
			if actual, err := sha256Path(path); err == nil {
				env.actualSHA = actual
			}
		}
		if code, err := piOverlayDecision(env); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = writeTerminationLog(err.Error())
			os.Exit(code)
		}
	})
}
