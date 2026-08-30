// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// Design 0053 S2: redact folds into the agentd binary as a subcommand.
// The supervisor writes a PATH wrapper at /sandbox-runtime/bin/redact so
// the documented `some-command | redact` UX (docs/reference/cli.md)
// survives with zero bytes of a second executable in the trusted
// one-file-one-hash artifact.
const (
	defaultRedactWrapperPath = "/sandbox-runtime/bin/redact"
	redactWrapperPathEnv     = "LLMSAFESPACES_REDACT_WRAPPER_PATH"
)

// redactWrapperPath returns the wrapper's install path, honoring the
// env override (same LLMSAFESPACES_*_PATH pattern as the store
// coordinates in pkg/agentd/types.go; the override exists for tests and
// non-standard layouts, not for production).
func redactWrapperPath() string {
	if p := os.Getenv(redactWrapperPathEnv); p != "" {
		return p
	}
	return defaultRedactWrapperPath
}

// shellSingleQuote quotes s for /bin/sh as a single argument. Single
// quotes are literal in sh — only the quote character itself needs
// handling — so this is interpolation-proof by construction (a path
// containing $, spaces, or quotes cannot split argv or substitute).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeRedactWrapper installs dir/redact as an executable /bin/sh script
// that execs the agentd binary with the redact subcommand. Atomic
// (temp-file + rename) and idempotent — a rewrite over an existing
// wrapper replaces it whole, never truncating the live file mid-read.
func writeRedactWrapper(dir, agentdPath string) error {
	// G301: 0750 — the wrapper dir is uid-1000 tmpfs; group (the pod's
	// shared gid) may traverse, others may not.
	//nolint:gosec // G301: deliberate mode for a credential-adjacent dir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	script := "#!/bin/sh\nexec " + shellSingleQuote(agentdPath) + ` redact "$@"` + "\n"
	target := filepath.Join(dir, "redact")

	// G703 (taint via dir): dir is not user input — it derives from
	// redactWrapperPath(), a package constant or the operator-set
	// LLMSAFESPACES_REDACT_WRAPPER_PATH env (same trust domain as every
	// other LLMSAFESPACES_*_PATH store coordinate). Temp+rename stay
	// inside that same directory by construction.
	tmp, err := os.CreateTemp(dir, ".redact-wrapper-*") //nolint:gosec // G703: see above
	if err != nil {
		return fmt.Errorf("temp file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after successful rename

	if _, err := tmp.WriteString(script); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write wrapper script: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod wrapper script: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close wrapper script: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil { //nolint:gosec // G703: same dir by construction
		return fmt.Errorf("rename into %s: %w", target, err)
	}
	return nil
}

// ensureRedactWrapper installs the redact PATH wrapper before the first
// opencode spawn. Best-effort by design (the ensureMiseShims precedent,
// design 0051 D1: the supervisor is plumbing): a failed install logs a
// Warn and boot continues — the wrapper preserves UX, it is not load-
// bearing for the pod contract. Post-S3 (base strip) a failed install
// means `redact` is absent from shells; pre-S3 the baked /usr/local/bin
// wrapper in the base image still serves.
func ensureRedactWrapper(logger *zap.Logger) {
	self, err := os.Executable()
	if err != nil {
		logger.Warn("redact wrapper not installed: cannot resolve agentd binary path", zap.Error(err))
		return
	}
	wrapper := redactWrapperPath()
	if err := writeRedactWrapper(filepath.Dir(wrapper), self); err != nil {
		logger.Warn("redact wrapper not installed (redact stays available via the PATH fallback)",
			zap.String("path", wrapper), zap.Error(err))
	}
}

// prependPathEnv returns env with dir as the first PATH entry. The
// input slice is never mutated; when dir already leads PATH the input is
// returned unchanged (the factory composes a fresh env per spawn, but
// idempotence keeps an accidental double prepend from stacking
// duplicate entries).
func prependPathEnv(env []string, dir string) []string {
	const prefix = "PATH="
	sep := string(os.PathListSeparator)
	for i, e := range env {
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		existing := e[len(prefix):]
		if existing == dir || strings.HasPrefix(existing, dir+sep) {
			return env
		}
		out := make([]string, len(env))
		copy(out, env)
		out[i] = prefix + dir + sep + existing
		return out
	}
	out := make([]string, len(env)+1)
	copy(out, env)
	out[len(env)] = prefix + dir
	return out
}
