// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_boot.go — the sidecar's credential boot phase (design 0051
// sidecar migration, step 1: bootstrap+materialize absorbed from the
// runtime-image credential-setup init into the platform sidecar).
//
// Ordering: runs FIRST in runSidecarCommand, before ensureBootAgentConfig
// and the muxes. The kubelet gates the main container on this sidecar's
// startup probe (/v1/healthz, unserved until boot completes), so
// opencode's first config read observes the completed credential state —
// the #857 stamp-before-read guarantee now rides the probe instead of
// init-container exit ordering.
//
// Paths: bootstrap output relocates to the pod-scoped tmpfs
// (/sandbox-runtime/rt/secrets.json) because the sidecar's /sandbox-cfg
// mount is ReadOnly. Same lifetime semantics as the emptyDir it
// replaces: wiped on pod death, survives CONTAINER restart.
//
// US-70.2 conditional semantics (superseding the old restart guard,
// which skipped the API entirely once any batch existed): every boot
// attempts the CONDITIONAL pull. Unchanged manifest → the API answers
// 304 without decrypting and the prior batch stays byte-identical;
// changed → a fresh envelope lands. A failed pull (API down, expired
// projected SA token) keeps the prior batch as last-good — never-block-
// boot holds either way, and a secrets change now reaches a restarted
// sidecar within the same pod instead of waiting for the push path.
//
// Failure semantics (lens: a native sidecar RESTARTS, unlike an init
// container): bootstrap never blocks boot on API failure (degrades to an
// empty batch on first boot, keeps last-good afterwards); materialize
// failures PROPAGATE non-zero so the sidecar exits and kubelet surfaces
// CrashLoopBackOff with a restart reason — never a never-Ready zombie
// (2026-08-25 incident class).

import (
	"io"
)

// sidecarBootOpts parameterizes the boot phase. Production values come
// from the sidecar's container env (set by the controller); tests pass
// a temp tree explicitly.
type sidecarBootOpts struct {
	WorkspaceID string
	APIURL      string
	TokenFile   string
	// SecretsOut is both bootstrap's --out and materialize's --from:
	// the tmpfs batch path for this pod. Production resolves it via
	// LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT (controller wires the pod-scoped
	// /sandbox-runtime/rt/secrets.json — shared with the resync endpoint
	// so boot pulls and in-process re-pulls read one coordinate).
	SecretsOut string
	Stderr     io.Writer
}

// runSidecarBootSecrets performs the boot phase and returns the process
// exit code the sidecar should propagate (0 to continue serving).
func runSidecarBootSecrets(opts sidecarBootOpts) int {
	// The conditional pull runs on every boot: runBootstrapCommand
	// presents the prior envelope's manifest hash (304 = keep file) and
	// keeps the last-good batch on failure. It never fails boot on API
	// errors — flag-level failure is the only non-zero it can return.
	if code := runBootstrapCommand([]string{
		"--workspace-id", opts.WorkspaceID,
		"--api-url", opts.APIURL,
		"--token-file", opts.TokenFile,
		"--out", opts.SecretsOut,
	}, io.Discard, opts.Stderr); code != 0 {
		return code
	}

	// Materialize is unconditional (idempotent replay on restart).
	// Path set comes from the LLMSAFESPACES_* env overrides — the same
	// seam the subcommand corpus uses; the controller points them at
	// /sandbox-runtime for sidecar pods.
	return runMaterializeCommand([]string{"--from", opts.SecretsOut}, io.Discard, opts.Stderr)
}
