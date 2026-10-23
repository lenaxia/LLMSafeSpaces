// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fsaudit is the #1808 corruption detector: a read-only,
// fsck-style scanner for the zero-fill corruption class (files silently
// replaced with NUL bytes at preserved length — the second
// sandbox-filesystem corruption class after #1793's SSH trailing
// spaces).
//
// The signature is exact, and deliberately narrow — a file is flagged
// only when ALL three hold:
//
//   - size > 0, and
//   - 100% of its content bytes are NUL (every chunk read to EOF), and
//   - it is NOT sparse (st_blocks*512 >= size — ftruncate/seek-created
//     all-NUL files are legitimately sparse and are reported separately,
//     never flagged: the false-positive trap a naive zeros-grep walks
//     into).
//
// Three laws, mirroring the #1601 disk-recovery rulings this engine
// rides beside:
//
//   - RESPONSE-ONLY: the scan writes nothing — no temp files, no report
//     persistence, no atime-chasing remount tricks. The report lives in
//     the HTTP response only.
//   - BOUNDED: per-file and total read budgets bound the worst case
//     (a tree of huge all-NUL files cannot turn one audit into an
//     unbounded sweep); every incomplete scan says so (Truncated /
//     ListTruncated / verdict=skipped), never a silent partial claim.
//   - NEVER-BLOCK-WORK: clean files cost one 4KiB chunk read (the scan
//     stops at the first non-NUL byte), non-regular files and symlinks
//     are skipped unread, and the whole audit is cancellable via ctx.
package fsaudit

// Request is the /v1/fs-audit body. Root is optional ("" or "." = the
// engine base); caps are optional (0 = the defaults). Values are
// clamped to at least one chunk, never rejected — the caller is tooling
// that wants an answer, not a validation error.
type Request struct {
	// Root is a path INSIDE the engine base. It is grounded: any
	// ".."-escape or absolute path collapses back under the base
	// (deny-by-default — the scan cannot be pointed at /etc).
	Root string `json:"root,omitempty"`
	// MaxFileBytes caps bytes read from one file. An all-NUL-so-far file
	// larger than the cap is verdict suspect (100%-NUL unconfirmed),
	// never nul_filled.
	MaxFileBytes int64 `json:"maxFileBytes,omitempty"`
	// MaxTotalBytes caps bytes read across the whole scan. Exhausting it
	// stops the scan with Truncated=true.
	MaxTotalBytes int64 `json:"maxTotalBytes,omitempty"`
}

// Budget defaults: the scan's worst case is bounded by construction.
// 64MiB/file covers every text/config/source shape the incident class
// touches; 256MiB total keeps a hostile tree's audit at seconds, not
// minutes. Both overridable per request.
const (
	DefaultMaxFileBytes  = 64 << 20
	DefaultMaxTotalBytes = 256 << 20
	// ChunkSize is the read granularity: the first non-NUL chunk ends a
	// file's classification (legit files cost one chunk).
	ChunkSize = 4096
	// MaxListEntries bounds each report list; counts stay exact.
	MaxListEntries = 1000
)

// Verdict is one file's classification.
type Verdict string

const (
	// VerdictClean: a non-NUL byte exists. The file is alive.
	VerdictClean Verdict = "clean"
	// VerdictSparse: 100% NUL but not allocated — ftruncate/seek-created
	// (legitimate; reported for visibility, never flagged).
	VerdictSparse Verdict = "sparse"
	// VerdictNulFilled: the corruption signature — size>0, 100% NUL,
	// allocated. The #1808 dead-file shape.
	VerdictNulFilled Verdict = "nul_filled"
	// VerdictSuspect: all-NUL within the per-file read cap with more file
	// remaining — 100%-NUL unconfirmed. Never counted as nul_filled.
	VerdictSuspect Verdict = "suspect"
	// VerdictSkipped: not a scan subject (zero-size, non-regular, or the
	// file in flight when the total budget exhausted).
	VerdictSkipped Verdict = "skipped"
	// VerdictError: unreadable (permissions, I/O). Never silently clean.
	VerdictError Verdict = "error"
)

// FileReport is one flagged file. Paths are base-relative, slash-separated.
type FileReport struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// AllocatedBytes is st_blocks*512 (0 when the platform does not
	// report it) — the sparseness evidence.
	AllocatedBytes int64   `json:"allocatedBytes"`
	Verdict        Verdict `json:"verdict"`
	// Reason carries cap/error detail (empty for clean/nul_filled).
	Reason string `json:"reason,omitempty"`
}

// Report is the full audit result: response-only by design.
type Report struct {
	Root string `json:"root"`
	// FilesScanned counts regular files examined (incl. zero-size).
	FilesScanned int `json:"filesScanned"`
	// FilesSkipped counts zero-size + non-regular files passed over.
	FilesSkipped int `json:"filesSkipped"`
	// BytesRead is the bytes actually READ — the early-exit proof (a
	// clean 1GiB tree reads kilobytes, not gigabytes).
	BytesRead int64 `json:"bytesRead"`
	// Truncated: the total read budget exhausted mid-scan — the tree is
	// NOT fully audited.
	Truncated bool `json:"truncated"`
	// ListTruncated: a report list hit MaxListEntries; counts remain
	// exact.
	ListTruncated bool `json:"listTruncated"`

	NulFilledCount int `json:"nulFilledCount"`
	SparseCount    int `json:"sparseCount"`
	SuspectCount   int `json:"suspectCount"`
	ErrorCount     int `json:"errorCount"`

	NulFilled []FileReport `json:"nulFilled"`
	Suspect   []FileReport `json:"suspect"`
	Errors    []FileReport `json:"errors"`
}
