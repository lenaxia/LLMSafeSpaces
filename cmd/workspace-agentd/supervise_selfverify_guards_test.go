// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervise_selfverify_guards_test.go — #1573 Part 1 (TDD: authored
// before the implementation).
//
// The incident: a recovery-path re-exec of the STALE BAKED agentd
// (pre-#1021, bash-entrypoint era) verified with expected=sha256("")
// — it hashed the pin VALUE — then its failure path group-SIGKILLed
// the process group: exit 137, PID 1 dead, two live turns halted. The
// owner's split: an empty pin is an OPERATOR/CONFIG signal, not a
// tamper verdict, and must never take live turns down; and the baked
// binary must never execute at all under the overlay contract.
//
// Guards pinned here:
//
//   - config class: volume marker set + no pin for arch (or unknown
//     arch) → *verifyConfigError ("AgentdVerificationConfigError: …"),
//     exit 87 — loud, distinct, never the tamper verdict.
//   - tamper class UNCHANGED: pin present + mismatch → exit 81 with
//     the exact "AgentdVerificationFailed: expected=…" shape (the
//     controller's detection contract).
//   - baked self-denial: marker set + running exe outside the overlay
//     mount → *bakedRefusalError ("AgentdBakedRefused: …"), exit 88 —
//     the baked path is never the recovery fallback. Legacy pods
//     (marker unset) are exempt: the baked binary is the contract
//     there.
//   - no verify path ever signals the process GROUP — exit codes only.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSelfVerifyConfigClassIsNotTamper pins the class split for the
// missing/unknown-pin configurations. Pre-#1573 these shared the
// tamper verdict's exit 81 — an operator misconfiguration was
// indistinguishable from a tampered binary, and the incident's
// group-kill was the worst reading of that ambiguity.
func TestSelfVerifyConfigClassIsNotTamper(t *testing.T) {
	const good = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cases := []struct {
		name       string
		volume     string
		amd64      string
		arm64      string
		arch       string
		actual     string
		overlayBin string
	}{
		{"no pin for arch", "1", "", "", "x86_64", good, ""},
		{"unknown arch", "1", good, good, "riscv64", good, ""},
		{"malformed pin (not 64 hex)", "1", "notahash", "", "x86_64", good, ""},
		{"sanitized env: marker lost, overlay coordinate kept", "", good, good, "x86_64", good, "/agentd/usr/local/bin/workspace-agentd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := selfVerifyDecision(selfVerifyEnv{
				volumeFlag: tc.volume,
				amd64Pin:   tc.amd64,
				arm64Pin:   tc.arm64,
				arch:       tc.arch,
				actualSHA:  tc.actual,
				overlayBin: tc.overlayBin,
			})
			if err == nil {
				t.Fatal("expected an error for the config class")
			}
			var cfg *verifyConfigError
			if !errors.As(err, &cfg) {
				t.Fatalf("expected *verifyConfigError, got %T: %v", err, err)
			}
			if want := "AgentdVerificationConfigError"; !errors.Is(err, errVerifyConfig) || cfg.Error()[:len(want)] != want {
				t.Fatalf("expected the %q sentinel + prefix, got %q", want, err)
			}
			if got := verifyExitCode(err); got != supervisorExitVerifyConfig {
				t.Fatalf("config error must exit %d, got %d", supervisorExitVerifyConfig, got)
			}
		})
	}
}

// TestSelfVerifyTamperContractUnchanged pins the tamper verdict's
// shape — the controller's detectAgentdVerificationFailure keys on the
// message and exit 81 (the #863 contract; #1573 must not loosen it).
func TestSelfVerifyTamperContractUnchanged(t *testing.T) {
	const good = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const bad = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	err := selfVerifyDecision(selfVerifyEnv{
		volumeFlag: "1",
		amd64Pin:   good,
		arm64Pin:   bad,
		arch:       "x86_64",
		actualSHA:  bad,
	})
	if err == nil {
		t.Fatal("mismatch must fail")
	}
	var cfg *verifyConfigError
	if errors.As(err, &cfg) {
		t.Fatalf("mismatch is TAMPER, not config: %v", err)
	}
	if got, want := verifyExitCode(err), supervisorExitVerifyFailed; got != want {
		t.Fatalf("tamper exits %d, got %d", want, got)
	}
}

// TestSelfVerifyBakedPathRefusal pins the baked self-denial: under the
// overlay marker the binary refuses to run from anywhere outside the
// overlay delivery — the controller-set LLMSAFESPACES_AGENTD_BINARY is
// authoritative, the canonical mount prefix the fallback. The
// incident's artifact group-killed on this path; any binary carrying
// this guard refuses instead — the baked path can never be the
// recovery fallback.
func TestSelfVerifyBakedPathRefusal(t *testing.T) {
	const overlayBin = "/agentd/usr/local/bin/workspace-agentd"
	cases := []struct {
		name        string
		volume      string
		selfExe     string
		overlayBin  string
		wantErr     bool
		wantMessage string
	}{
		{"baked path under marker, env set", "1", "/usr/local/bin/workspace-agentd", overlayBin, true, "is not the overlay binary"},
		{"overlay binary under marker, env set", "1", overlayBin, overlayBin, false, ""},
		{"overlay sibling dir under marker, env set", "1", "/agentd/usr/local/bin/workspace-agentd.new", overlayBin, false, ""},
		{"legacy pod, baked path, no marker", "", "/usr/local/bin/workspace-agentd", overlayBin, false, ""},
		{"other path under marker, env set", "1", "/opt/somewhere/workspace-agentd", overlayBin, true, "is not the overlay binary"},
		{"sanitized env: env absent, mount prefix fallback refuses", "1", "/usr/local/bin/workspace-agentd", "", true, "is outside the overlay mount"},
		{"sanitized env: env absent, overlay prefix proceeds", "1", "/agentd/usr/local/bin/workspace-agentd", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := bakedPathRefusal(tc.volume, tc.selfExe, tc.overlayBin)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("expected no refusal, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected refusal")
			}
			var refusal *bakedRefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("expected *bakedRefusalError, got %T: %v", err, err)
			}
			if want := "AgentdBakedRefused"; refusal.Error()[:len(want)] != want {
				t.Fatalf("prefix %q expected, got %q", want, err)
			}
			if !strings.Contains(refusal.Error(), tc.wantMessage) {
				t.Fatalf("message %q expected to contain %q", refusal.Error(), tc.wantMessage)
			}
			if got := verifyExitCode(err); got != supervisorExitBakedRefused {
				t.Fatalf("refusal must exit %d, got %d", supervisorExitBakedRefused, got)
			}
		})
	}
}

// TestVerifyExitCodeClassification pins the full classification — the
// incident's ask (b): every class exits ITS OWN code; no class ever
// escalates to a signal, and nil means proceed.
func TestVerifyExitCodeClassification(t *testing.T) {
	if got := verifyExitCode(nil); got != 0 {
		t.Fatalf("nil error must mean proceed (0), got %d", got)
	}
	if got := verifyExitCode(errors.New("AgentdVerificationFailed: expected=x got=y")); got != supervisorExitVerifyFailed {
		t.Fatalf("tamper 81 expected, got %d", got)
	}
	if got := verifyExitCode(&verifyConfigError{msg: "AgentdVerificationConfigError: no pin"}); got != supervisorExitVerifyConfig {
		t.Fatalf("config 87 expected, got %d", got)
	}
	if got := verifyExitCode(&bakedRefusalError{msg: "AgentdBakedRefused: baked path"}); got != supervisorExitBakedRefused {
		t.Fatalf("refusal 88 expected, got %d", got)
	}
}

// TestSupervisorSelfVerify_ClassExits pins the REAL binary's exits for
// the two new classes, subprocess-grade: the config error (marker set,
// pins absent, exe IS the overlay binary) exits 87 with its marker —
// NOT 81, and the process dies alone (no group signal); the baked
// refusal (marker set, exe is NOT the declared overlay binary) exits
// 88 with its marker before any hashing.
func TestSupervisorSelfVerify_ClassExits(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/exe self-hash is linux-only")
	}

	cases := []struct {
		name     string
		overlays []string // extra env on top of the filtered base
		wantExit int
		wantMsg  string
	}{
		{
			name: "config error exits 87, not 81",
			overlays: []string{
				"AGENTD_IMAGE_VOLUME=1",
				"LLMSAFESPACES_AGENTD_BINARY=", // set below to bin
			},
			wantExit: supervisorExitVerifyConfig,
			wantMsg:  "AgentdVerificationConfigError",
		},
		{
			name: "sanitized env: marker absent, coordinate kept → exit 87 at the ENTRY POINT",
			overlays: []string{
				"LLMSAFESPACES_AGENTD_BINARY=/agentd/usr/local/bin/workspace-agentd",
			},
			wantExit: supervisorExitVerifyConfig,
			wantMsg:  "AgentdVerificationConfigError",
		},
		{
			name: "baked refusal exits 88 before hashing",
			overlays: []string{
				"AGENTD_IMAGE_VOLUME=1",
				"LLMSAFESPACES_AGENTD_BINARY=/agentd/usr/local/bin/workspace-agentd",
				"LLMSAFESPACES_AGENTD_SHA256_AMD64=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			wantExit: supervisorExitBakedRefused,
			wantMsg:  "AgentdBakedRefused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildAgentdBinary(t)
			dir := t.TempDir()

			env := filteredEnviron(overlayEnvKeys()...)
			// The subprocess IS the declared overlay binary for every
			// case except the refusal one (its overlay env deliberately
			// points elsewhere).
			for _, kv := range tc.overlays {
				if kv == "LLMSAFESPACES_AGENTD_BINARY=" {
					env = append(env, "LLMSAFESPACES_AGENTD_BINARY="+bin)
				} else {
					env = append(env, kv)
				}
			}

			cmd := exec.Command(bin, "supervise-opencode")
			cmd.Env = env
			stderr := filepath.Join(dir, "stderr")
			f, err := os.Create(stderr)
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = f
			cmd.Stdout = f

			runErr := cmd.Run()
			if runErr == nil {
				_ = f.Close()
				t.Fatal("verify failure must exit non-zero")
			}
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok {
				_ = f.Close()
				t.Fatalf("got %v", runErr)
			}
			_ = f.Close()
			if exitErr.ExitCode() != tc.wantExit {
				t.Fatalf("exit %d expected, got %d (stderr in %s)", tc.wantExit, exitErr.ExitCode(), stderr)
			}
			out, err := os.ReadFile(stderr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), tc.wantMsg) {
				t.Fatalf("stderr must carry %q, got: %s", tc.wantMsg, string(out))
			}
		})
	}
}
