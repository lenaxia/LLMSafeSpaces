// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// gate_docs.go — the pod-side worked-patterns docs (the #1812 gate-script
// doc, 2026-10-29 owner ruling, tsk-arctic-wardrobe-verifies; plus the
// 2026-10-30 owner ruling's two example DIRECTORIES — Sprint Board story
// patterns and workflow DAG patterns, tsk-rested-shanty-feels).
//
// The canonical sources live under docs/automation/ — gate-script-patterns.md,
// sprint-board-patterns/, workflow-patterns/ — the source of truth. This
// file embeds byte-identical copies (gate_script_patterns.md,
// sprint_board_patterns/, workflow_patterns/ — drift-pinned equal by
// gate_docs_test.go and pattern_docs_test.go — `go:embed` cannot reach
// outside the package dir) so the agentd BINARY — one release unit, already
// in every pod, carrying the MCP tools whose descriptions point at the
// docs — ships them without image-layout coupling. The init-fs boot phase
// materializes the embedded bytes under $runtimeDir/docs/ (idempotent
// copies), the stable pod paths the tool descriptions name. Boot-time copy
// on purpose: init-fs runs before any app container, so the docs are
// present before the first agent session — no sync mechanism racing pod
// start.

import (
	"bytes"
	// embed: the //go:embed directives below consume it — embed.FS for
	// the pattern directories, and the byte-slice target keeps the
	// directive itself legal per the embed package contract.
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed gate_script_patterns.md
var gateScriptPatternsDoc []byte

// The two pattern directories (2026-10-30 owner ruling): one file per
// example, each self-indexing with the canonical greppable header. The
// directory embed keeps adding an example doc-only — no Go change, no
// new release ceremony beyond the image rebuild every release already is.
//
//go:embed sprint_board_patterns/*.md
var sprintBoardPatternsFS embed.FS

//go:embed workflow_patterns/*.md
var workflowPatternsFS embed.FS

// gateScriptPatternsDocName is the stable pod-side filename (the path
// the trigger_create descriptions point at:
// /sandbox-runtime/docs/gate-script-patterns.md).
const gateScriptPatternsDocName = "gate-script-patterns.md"

// writeDocAtomic writes data to dst (inside dstDir) via temp + rename,
// 0644 — public documentation, zero secrets. label names the step family
// in wrapped errors (gate-docs / pattern-docs) so the single-file and
// directory paths share ONE write mechanism with distinguishable errors.
func writeDocAtomic(dstDir, dst, label string, data []byte) error {
	tmp, err := os.CreateTemp(dstDir, "."+label+"-*")
	if err != nil {
		return fmt.Errorf("%s temp in %s: %w", label, dstDir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%s write: %w", label, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%s close: %w", label, err)
	}
	//nolint:gosec // G306: 0644 — public documentation, zero secrets (CreateTemp starts 0600)
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("%s chmod: %w", label, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("%s rename to %s: %w", label, dst, err)
	}
	return nil
}

// ensureGateScriptPatternsDoc materializes the embedded gate-script
// patterns doc into dstDir/gate-script-patterns.md. Idempotent: an
// identical existing file is left untouched (no rewrite, stable mtime);
// a stale or absent file is (re)written atomically (temp + rename), so
// an agentd upgrade replaces an older doc on the next boot. Advisory
// content — the caller owns the failure posture (init-fs logs loudly
// and never blocks boot on it).
func ensureGateScriptPatternsDoc(dstDir string) error {
	// 0750: dir traversable by the pod's group (gid 1000), no world
	// write — the same posture the runtime dir's other children carry.
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("gate-docs dir %s: %w", dstDir, err)
	}
	dst := filepath.Join(dstDir, gateScriptPatternsDocName)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, gateScriptPatternsDoc) {
		return nil // already current — idempotent no-op
	}
	return writeDocAtomic(dstDir, dst, "gate-docs", gateScriptPatternsDoc)
}

// ensureEmbeddedPatternDir materializes an embedded pattern directory
// (one file per example) into dstDir so the pod path MIRRORS the embedded
// set exactly: identical files untouched (stable mtimes), stale or absent
// files (re)written atomically, and files no longer in the embedded set
// REMOVED — an agentd upgrade that drops an example replaces the
// directory on the next boot, leaving no orphan a tool description's
// grep catalog could still find. srcDir is the directory's path inside
// fsys (e.g. "sprint_board_patterns"). Advisory content — the caller
// owns the failure posture (init-fs logs loudly, never blocks boot).
func ensureEmbeddedPatternDir(fsys embed.FS, srcDir, dstDir string) error {
	entries, err := fsys.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("pattern-docs embed read %s: %w", srcDir, err)
	}
	want := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, err := fsys.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return fmt.Errorf("pattern-docs embed read %s/%s: %w", srcDir, e.Name(), err)
		}
		want[e.Name()] = b
	}
	if len(want) == 0 {
		return fmt.Errorf("pattern-docs embed %s: no .md files matched (the //go:embed pattern must match at least one example)", srcDir)
	}
	// 0750: dir traversable by the pod's group (gid 1000), no world
	// write — the same posture the runtime dir's other children carry.
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("pattern-docs dir %s: %w", dstDir, err)
	}
	for name, data := range want {
		dst := filepath.Join(dstDir, name)
		if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
			continue // already current — idempotent no-op
		}
		if err := writeDocAtomic(dstDir, dst, "pattern-docs", data); err != nil {
			return err
		}
	}
	existing, err := os.ReadDir(dstDir)
	if err != nil {
		return fmt.Errorf("pattern-docs dir read %s: %w", dstDir, err)
	}
	for _, e := range existing {
		if e.IsDir() {
			continue // never touch subdirectories an operator may own
		}
		if _, ok := want[e.Name()]; !ok {
			if err := os.Remove(filepath.Join(dstDir, e.Name())); err != nil {
				return fmt.Errorf("pattern-docs remove stale %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}
