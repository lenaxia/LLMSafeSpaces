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
// replaces: wiped on pod death, survives CONTAINER restart — which the
// idempotency guard relies on:
//
//   - secrets.json exists non-empty → bootstrap already ran for THIS
//     pod (sidecar restart: liveness kill, OOM, node pressure). Skip
//     the API fetch — it may be down and the 600s projected SA token is
//     long expired. Materialize re-runs: it is idempotent by design
//     (reset() wipes and reinstalls the tmpfs tree).
//
// Failure semantics (lens: a native sidecar RESTARTS, unlike an init
// container): bootstrap never blocks boot on API failure (degrades to an
// empty batch — the reload-secrets path recovers credentials on first
// activation); materialize failures PROPAGATE non-zero so the sidecar
// exits and kubelet surfaces CrashLoopBackOff with a restart reason —
// never a never-Ready zombie (2026-08-25 incident class).

import (
	"io"
	"os"
)

// sidecarBootOpts parameterizes the boot phase. Production values come
// from the sidecar's container env (set by the controller); tests pass
// a temp tree explicitly.
type sidecarBootOpts struct {
	WorkspaceID string
	APIURL      string
	TokenFile   string
	// SecretsOut is both bootstrap's --out and materialize's --from:
	// the tmpfs batch path for this pod.
	SecretsOut string
	Stderr     io.Writer
}

// sidecarSecretsOutPath is the production tmpfs batch path. The
// controller's init-fs container created rt/ (0700) before this runs.
const sidecarSecretsOutPath = "/sandbox-runtime/rt/secrets.json"

// sidecarBootSecretsAlreadyRan reports whether a non-empty batch
// already exists for this pod (sidecar restart guard).
func sidecarBootSecretsAlreadyRan(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// runSidecarBootSecrets performs the boot phase and returns the process
// exit code the sidecar should propagate (0 to continue serving).
func runSidecarBootSecrets(opts sidecarBootOpts) int {
	if !sidecarBootSecretsAlreadyRan(opts.SecretsOut) {
		// Fresh pod (or first successful boot): fetch the batch.
		// runBootstrapCommand never fails boot on API errors — it
		// writes an empty batch and returns 0 (never-block-boot).
		if code := runBootstrapCommand([]string{
			"--workspace-id", opts.WorkspaceID,
			"--api-url", opts.APIURL,
			"--token-file", opts.TokenFile,
			"--out", opts.SecretsOut,
		}, io.Discard, opts.Stderr); code != 0 {
			return code // flag-level failure only
		}
	}

	// Materialize is unconditional (idempotent replay on restart).
	// Path set comes from the LLMSAFESPACES_* env overrides — the same
	// seam the subcommand corpus uses; the controller points them at
	// /sandbox-runtime for sidecar pods.
	return runMaterializeCommand([]string{"--from", opts.SecretsOut}, io.Discard, opts.Stderr)
}
