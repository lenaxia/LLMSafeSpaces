// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervise_selfverify.go — design 0051 sidecar migration, step 2: the
// supervisor's binary-integrity self-check.
//
// With the main container Command: pointed at the overlay supervisor
// binary (bypassing the baked entrypoint), the #863 verify-before-exec
// that entrypoint-common.sh performed in bash moves here. The
// supervisor hashes its own executable (/proc/self/exe — the running
// bytes, not the argv[0] path) against the pod-spec pin env
// (LLMSAFESPACES_AGENTD_SHA256_<ARCH>), the only integrity anchor the
// container itself cannot touch (immutable post-create,
// workspace-unwritable).
//
// Contracts pinned to the bash version so the controller's
// detectAgentdVerificationFailure (exit 81 + expected=/got= message
// shape) keeps working unchanged:
//
//	AGENTD_IMAGE_VOLUME unset → skip (legacy baked binary).
//	pin mismatch / no pin for arch → exit 81 before ANY work (socket,
//	  children, markers) — fail closed, never a silent fallback.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// supervisorExitVerifyFailed mirrors the entrypoint's exit 81 — the
// controller's agentd-verification detection keys on it.
const supervisorExitVerifyFailed = 81

// #1573 Part 1: the config and refusal classes. The incident's worst
// reading came from one verdict covering two very different truths —
// a tampered binary and a missing pin config both exited 81, and the
// (pre-#1021 stale baked) artifact's failure path escalated to a
// process-group SIGKILL that halted two live turns. Every class now
// exits its OWN code; no class ever signals the group.
const (
	// supervisorExitVerifyConfig: empty/malformed pin, unknown arch —
	// an operator signal, loud but never the tamper verdict. 87/88,
	// NOT 83/84: those are the opencode-overlay verify's codes in the
	// same container (opencode_overlay.go) — a dual-overlay pod's
	// failure attribution must never cross overlays.
	supervisorExitVerifyConfig = 87
	// supervisorExitBakedRefused: the baked path executed under the
	// overlay contract — refuse; the baked binary is never the
	// recovery fallback.
	supervisorExitBakedRefused = 88
)

// errVerifyConfig is the sentinel for the #1573 config class.
var errVerifyConfig = errors.New("agentd verification config")

// verifyConfigError is the #1573 config class: the pin contract is
// unfulfilled (empty/malformed pin, unknown arch). This is an
// operator/config signal — loud (own exit code, termination log,
// controller condition) — never a tamper verdict and never a
// process-group signal. The incident's expected=sha256("") came from a
// stale pre-#1021 artifact hashing the pin VALUE; current code never
// hashes the pin, and a missing pin is classified here instead.
type verifyConfigError struct{ msg string }

func (e *verifyConfigError) Error() string        { return e.msg }
func (e *verifyConfigError) Is(target error) bool { return target == errVerifyConfig }

// bakedRefusalError is the #1573 self-denial: under the overlay
// contract (AGENTD_IMAGE_VOLUME=1) the binary refuses to run from
// anywhere outside the overlay mount — the baked path (the 80683260
// incident class) can never be the recovery fallback.
type bakedRefusalError struct{ msg string }

func (e *bakedRefusalError) Error() string { return e.msg }

// bakedPathRefusal refuses execution when the overlay marker is set
// but the running executable resolves outside the overlay delivery.
// The authoritative comparison is the controller-set
// LLMSAFESPACES_AGENTD_BINARY (immutable in the pod, set in the same
// branch as the marker); the canonical mount prefix is the fallback
// when the env is absent (the #1573 incident shape: a sanitized
// environment must not disable the guard). Legacy pods (marker unset)
// are exempt: the baked binary IS the contract there.
func bakedPathRefusal(volumeFlag, selfExe, overlayBinEnv string) error {
	if volumeFlag != "1" {
		return nil
	}
	if overlayBinEnv != "" {
		if selfExe == overlayBinEnv || strings.HasPrefix(selfExe, filepath.Dir(overlayBinEnv)+string(os.PathSeparator)) {
			return nil
		}
		return &bakedRefusalError{msg: fmt.Sprintf(
			"AgentdBakedRefused: exe %s is not the overlay binary %s — the baked binary never executes under the overlay contract (#1573)",
			selfExe, overlayBinEnv)}
	}
	if strings.HasPrefix(selfExe, agentd.OverlayMountPath+"/") {
		return nil
	}
	return &bakedRefusalError{msg: fmt.Sprintf(
		"AgentdBakedRefused: exe %s is outside the overlay mount %s — the baked binary never executes under the overlay contract (#1573)",
		selfExe, agentd.OverlayMountPath)}
}

// verifyExitCode classifies a self-verify error to its exit code.
// nil → 0 (proceed); config → 83; refusal → 84; anything else is the
// tamper verdict → 81 (the #863 contract, unchanged).
func verifyExitCode(err error) int {
	var cfg *verifyConfigError
	if errors.As(err, &cfg) {
		return supervisorExitVerifyConfig
	}
	var refusal *bakedRefusalError
	if errors.As(err, &refusal) {
		return supervisorExitBakedRefused
	}
	if err != nil {
		return supervisorExitVerifyFailed
	}
	return 0
}

// selfVerifyEnv is the resolved pin environment for the self-check.
// Separated from os.Getenv so the decision logic is unit-testable.
type selfVerifyEnv struct {
	volumeFlag string
	amd64Pin   string
	arm64Pin   string
	arch       string
	actualSHA  string
}

// pinForArch maps the uname-style arch to its pin, mirroring
// entrypoint-common.sh's case statement.
func (e selfVerifyEnv) pinForArch() string {
	switch e.arch {
	case "x86_64":
		return e.amd64Pin
	case "aarch64":
		return e.arm64Pin
	default:
		return ""
	}
}

// selfVerifyDecision evaluates the pin contract. Nil error = proceed.
func selfVerifyDecision(e selfVerifyEnv) error {
	if e.volumeFlag != "1" {
		return nil // legacy: baked binary, no overlay pin contract
	}
	expected := e.pinForArch()
	if expected == "" {
		return &verifyConfigError{msg: fmt.Sprintf(
			"AgentdVerificationConfigError: no sha256 pin for arch %s (self-verify) — operator signal, not a tamper verdict (#1573)", e.arch)}
	}
	if !isHex64(expected) {
		return &verifyConfigError{msg: fmt.Sprintf(
			"AgentdVerificationConfigError: malformed pin for arch %s (%d chars, want 64 hex) — operator signal, not a tamper verdict (#1573)", e.arch, len(expected))}
	}
	if e.actualSHA != expected {
		return fmt.Errorf("AgentdVerificationFailed: expected=%s got=%s binary=/proc/self/exe node_arch=%s",
			expected, e.actualSHA, e.arch)
	}
	return nil
}

// isHex64 reports whether s is exactly 64 lowercase hex characters —
// the pin shape the controller validates at startup; a pod carrying
// anything else is a config break, not tamper evidence.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// unameArch maps Go arch to the uname -m vocabulary the pins use.
func unameArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// sha256Hex hashes bytes — split out for test use.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sha256Path hashes a file's contents streaming (the binary is tens of
// MB; never buffered whole).
func sha256Path(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // fixed self-referential paths only
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runSupervisorSelfVerify is the production entry: resolve env, hash
// /proc/self/exe, decide. Non-nil error → the supervisor exits 81.
// The volume flag short-circuits BEFORE any hashing — legacy pods skip
// the multi-hundred-ms binary hash entirely (the unconditional form
// shifted supervisor startup latency enough to miss the spawn-env
// delta window in TestSupervisorSubprocess_LifecycleAndContract, CI
// 2026-08-26).
func runSupervisorSelfVerify(exePath string) error {
	if os.Getenv("AGENTD_IMAGE_VOLUME") != "1" {
		return nil // legacy: baked binary, no overlay pin contract
	}
	// #1573 baked self-denial: judge the RESOLVED path (a symlinked
	// /proc/self/exe still resolves to the real file on disk).
	resolved := exePath
	if r, err := filepath.EvalSymlinks(exePath); err == nil {
		resolved = r
	}
	if err := bakedPathRefusal(os.Getenv("AGENTD_IMAGE_VOLUME"), resolved, os.Getenv("LLMSAFESPACES_AGENTD_BINARY")); err != nil {
		return err
	}
	actual, err := sha256Path(exePath)
	if err != nil {
		// Unreadable self → empty hash → guaranteed pin mismatch → 81.
		// Fail closed without inventing a hash.
		actual = ""
	}
	return selfVerifyDecision(selfVerifyEnv{
		volumeFlag: os.Getenv("AGENTD_IMAGE_VOLUME"),
		amd64Pin:   os.Getenv("LLMSAFESPACES_AGENTD_SHA256_AMD64"),
		arm64Pin:   os.Getenv("LLMSAFESPACES_AGENTD_SHA256_ARM64"),
		arch:       unameArch(runtime.GOARCH),
		actualSHA:  actual,
	})
}
