// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package diskrecovery is the #1601 mechanical disk-space lever: an
// allowlist-only reclaimer of KNOWN-REPRODUCIBLE build caches and test
// residue for workspace pods too full for the agent to act.
//
// Three laws, enforced in code (see manifest.go and engine.go):
//
//   - DENY-BY-DEFAULT: nothing is ever a deletion candidate unless a
//     compiled-in, per-runtime-base manifest allowlists it. There is
//     deliberately NO runtime-loadable manifest (no env list, no file on
//     the PVC): a user-writable allowlist would let user-space code add
//     paths to its own deletion boundary.
//   - NEVER USER DATA: every candidate passes the boundary validator —
//     inside a declared cache root, never equal to or an ancestor of a
//     protected path (/, the PVC roots, $HOME, the package homes), and
//     symlink-resolved inside the same root. A manifest entry that would
//     touch a user path is REFUSED per-class and reported, never skipped
//     silently.
//   - WRITE-FREE RESCUE PATH (owner ruling on #1601): deletion is a
//     metadata operation that works at 100% full; the recover path never
//     writes anything — no temp files, no report persistence, the report
//     lives in the HTTP response only.
package diskrecovery

// Request is the /v1/disk-recover body. TargetRatio is optional; zero
// means DefaultTargetRatio. Values outside (MinTargetRatio,
// MaxTargetRatio) are clamped, never rejected — the caller is a human
// clicking a button, and clamping keeps the lever usable at 99%.
type Request struct {
	DryRun      bool    `json:"dryRun"`
	TargetRatio float64 `json:"targetRatio,omitempty"`
}

// Target-ratio bounds. Default 0.85 per #1601 ("reclaim until below a
// target (say 85%)"). Upper bound stays strictly below the 0.95
// critical tier (pkg/agent/systemnotices) so a panicked user cannot ask
// for a target that would keep the button auto-surfaced.
const (
	DefaultTargetRatio = 0.85
	MinTargetRatio     = 0.50
	MaxTargetRatio     = 0.90
)

// ClassStatus is the per-class outcome in a Report.
type ClassStatus string

const (
	// ClassFreed: entries existed and were deleted (bytesFreed set).
	ClassFreed ClassStatus = "freed"
	// ClassWouldFree: dry-run — bytes are reclaimable, nothing touched.
	ClassWouldFree ClassStatus = "would_free"
	// ClassNotPresent: the allowlisted path does not exist (nothing to
	// do — normal on fresh pods).
	ClassNotPresent ClassStatus = "not_present"
	// ClassRefused: the entry FAILED the boundary validator. The class
	// is reported with the refusal reason; other classes proceed. A
	// refusal means the manifest (or the filesystem) tried to broaden
	// the deletion boundary — loud, never silent.
	ClassRefused ClassStatus = "refused"
	// ClassError: measurement or deletion failed (I/O, permissions).
	ClassError ClassStatus = "error"
	// ClassSkippedTargetMet: measured but not deleted — the volume
	// dropped below target first ("free ENOUGH", not maximal sweep).
	ClassSkippedTargetMet ClassStatus = "skipped_target_met"
)

// ClassReport is one allowlist entry's outcome.
type ClassReport struct {
	Class string `json:"class"`
	Path  string `json:"path"`
	// Bytes is the measured reclaimable size at recover time (for
	// age-filtered classes: only the stale share counts).
	Bytes int64 `json:"bytes"`
	// BytesFreed is what this class actually contributed. Zero in
	// dry-run and for every non-freed status.
	BytesFreed int64 `json:"bytesFreed"`
	// Entries counts reclaimable deletion-candidate paths (one per
	// stale child for prefix classes; the tree for dir/symlink
	// classes) — file-level counts roll up into Bytes.
	Entries int         `json:"entries"`
	Status  ClassStatus `json:"status"`
	// Reason carries the refusal/error detail (empty for healthy
	// outcomes).
	Reason string `json:"reason,omitempty"`
}

// Report is the full recover result: response-only by design (owner
// ruling on #1601 — never persisted to the full volume).
type Report struct {
	DryRun             bool    `json:"dryRun"`
	AlreadyBelowTarget bool    `json:"alreadyBelowTarget"`
	BeforeUsedBytes    int64   `json:"beforeUsedBytes"`
	BeforeTotalBytes   int64   `json:"beforeTotalBytes"`
	AfterUsedBytes     int64   `json:"afterUsedBytes"`
	AfterTotalBytes    int64   `json:"afterTotalBytes"`
	TargetRatio        float64 `json:"targetRatio"`
	BeforeRatio        float64 `json:"beforeRatio"`
	AfterRatio         float64 `json:"afterRatio"`
	BytesFreed         int64   `json:"bytesFreed"`
	// StoppedEarly: target reached with measured classes left unvisited.
	StoppedEarly bool          `json:"stoppedEarly"`
	RuntimeBase  string        `json:"runtimeBase"`
	Classes      []ClassReport `json:"classes"`
}
