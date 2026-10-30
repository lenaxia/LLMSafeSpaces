// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// gate_docs_test.go — pins for the #1812 gate-script patterns doc
// delivery (2026-10-29 owner ruling):
//
//   - DRIFT PIN: the embedded copy is byte-identical to the source of
//     truth at docs/automation/gate-script-patterns.md. go:embed cannot
//     reach outside the package dir, so the embed is a checked-in copy —
//     this pin makes silent divergence impossible (edit the source,
//     re-copy, or this test reddens).
//   - CONTENT PIN: the doc carries the canonical patterns the
//     2026-10-29 live-fire validated (the GitHub-anon contract, the
//     304-raises trap, marker-file dedup, the failure-posture choice,
//     self-contained prompts) — a rewrite cannot silently drop one.
//   - MATERIALIZATION: ensureGateScriptPatternsDoc is idempotent
//     (identical file untouched, stale file replaced) — the boot-time
//     copy contract init-fs rides.
//
// The end-to-end boot leg (init-fs → $runtimeDir/docs/) is pinned in
// init_fs_test.go's happy-path and idempotent subprocess runs.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGateScriptPatternsDoc_DriftPin_SourceOfTruth: the embedded bytes
// equal docs/automation/gate-script-patterns.md — the source of truth
// the release ships from. Tests run with CWD = this package dir, so the
// repo-relative path is ../../docs/automation/.
func TestGateScriptPatternsDoc_DriftPin_SourceOfTruth(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "automation", "gate-script-patterns.md"))
	require.NoError(t, err, "docs/automation/gate-script-patterns.md (the source of truth) must exist")
	assert.Equal(t, string(src), string(gateScriptPatternsDoc),
		"the embedded copy has drifted from the source of truth — re-copy "+
			"docs/automation/gate-script-patterns.md to cmd/workspace-agentd/gate_script_patterns.md")
}

// TestGateScriptPatternsDoc_ContentPins: the canonical pattern fragments
// the 2026-10-29 live-fire validated must survive any doc rewrite —
// each trap that burned an attempt is named in the doc.
func TestGateScriptPatternsDoc_ContentPins(t *testing.T) {
	doc := string(gateScriptPatternsDoc)
	for _, want := range []string{
		// Pattern 1 — external polling, normalized; the GitHub-anon contract.
		"review_decision", // the null-unauthenticated trap, named
		"/reviews",        // the endpoint that carries the anon truth
		".lower()",        // normalize before comparing
		"submitted_at",    // the chronology anchor of the last review
		"per-IP",          // the shared 60/h budget reality
		"calls-per-fire",  // the cadence budgeting rule
		// Pattern 2 — conditional requests.
		"e.code == 304", // 304 arrives as HTTPError — catch it as no-change
		"stale",         // the CDN-stale-body ETag trap
		"marker dedup",  // the recommended alternative to ETag diffing
		// Pattern 3 — state dedup across fires.
		"NO memory",    // fires start from scratch
		"signature",    // the marker-file signature idiom
		"at-most-once", // the mark-on-observation tradeoff, stated
		// Pattern 4 — failure posture.
		"fail CLOSED",   // the platform invariant
		"proceed=false", // the quiet choice
		"fail-open",     // the wake-someone choice
		// Pattern 5 — self-contained action prompts.
		"FRESH context",    // the LLM leg sees nothing else
		"exact relay text", // carry everything: text and destination
	} {
		assert.Contains(t, doc, want, "canonical pattern fragment %q must stay in the doc", want)
	}
}

// TestEnsureGateScriptPatternsDoc_MaterializesAndIsIdempotent: the
// boot-time copy — absent file lands, identical file is untouched
// (stable mtime), stale content is replaced with the embedded bytes.
func TestEnsureGateScriptPatternsDoc_MaterializesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, gateScriptPatternsDocName)

	// First call: materializes.
	require.NoError(t, ensureGateScriptPatternsDoc(dir))
	got, err := os.ReadFile(dst)
	require.NoError(t, err, "doc must materialize into dstDir")
	assert.Equal(t, string(gateScriptPatternsDoc), string(got))

	// Second call over identical content: no-op — the mtime is stable.
	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.NoError(t, ensureGateScriptPatternsDoc(dir))
	info2, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime(), info2.ModTime(), "identical doc must not be rewritten (idempotent no-op)")

	// Stale content (an older release's doc): replaced on the next boot.
	require.NoError(t, os.WriteFile(dst, []byte("# an older release's doc\n"), 0o644))
	require.NoError(t, ensureGateScriptPatternsDoc(dir))
	got, err = os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, string(gateScriptPatternsDoc), string(got), "stale doc must be replaced by the embedded bytes")
}

// TestEnsureGateScriptPatternsDoc_ModeIsPublicData: the materialized doc
// is 0644 — public documentation with zero secrets, readable by the
// workspace's agent session (uid 1000) and anyone else in the pod.
func TestEnsureGateScriptPatternsDoc_ModeIsPublicData(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensureGateScriptPatternsDoc(dir))
	info, err := os.Stat(filepath.Join(dir, gateScriptPatternsDocName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

// TestEnsureGateScriptPatternsDoc_ErrorPaths: the unhappy legs — both
// return wrapped errors carrying the step's identity (never panic,
// never strand a temp file); init-fs turns these into the non-fatal
// boot log pinned by TestInitFS_GateDocCopyBlocked_NeverBlocksBoot.
// These are the only error branches reachable without root.
func TestEnsureGateScriptPatternsDoc_ErrorPaths(t *testing.T) {
	t.Run("dst dir path is an existing file", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "docs")
		require.NoError(t, os.WriteFile(blocker, []byte("a file where the dir belongs"), 0o644))

		err := ensureGateScriptPatternsDoc(blocker)
		require.Error(t, err, "MkdirAll over an existing file must surface an error")
		assert.Contains(t, err.Error(), "gate-docs dir", "wrapped with the step's identity")
	})

	t.Run("dst is an existing directory", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, gateScriptPatternsDocName), 0o755))

		err := ensureGateScriptPatternsDoc(dir)
		require.Error(t, err, "rename of the temp file onto a directory must surface an error")
		assert.Contains(t, err.Error(), "gate-docs rename", "wrapped with the step's identity")

		// The write path must clean up after itself: only the blocking
		// directory remains, no .gate-docs-* temp strays.
		entries, readErr := os.ReadDir(dir)
		require.NoError(t, readErr)
		require.Len(t, entries, 1, "exactly one entry must remain (the blocker)")
		assert.Equal(t, gateScriptPatternsDocName, entries[0].Name(),
			"the blocking directory survives untouched; no temp files are stranded")
	})
}
