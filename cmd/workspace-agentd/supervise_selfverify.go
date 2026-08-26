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
	"fmt"
	"io"
	"os"
	"runtime"
)

// supervisorExitVerifyFailed mirrors the entrypoint's exit 81 — the
// controller's agentd-verification detection keys on it.
const supervisorExitVerifyFailed = 81

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
		return fmt.Errorf("AgentdVerificationFailed: no sha256 pin for arch %s (self-verify)", e.arch)
	}
	if e.actualSHA != expected {
		return fmt.Errorf("AgentdVerificationFailed: expected=%s got=%s binary=/proc/self/exe node_arch=%s",
			expected, e.actualSHA, e.arch)
	}
	return nil
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
