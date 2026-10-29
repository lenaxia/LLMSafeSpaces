// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// gate_docs.go — the #1812 gate-script worked-patterns doc (2026-10-29
// owner ruling, tsk-arctic-wardrobe-verifies).
//
// The canonical patterns live at docs/automation/gate-script-patterns.md
// — the source of truth. This file embeds a byte-identical copy
// (gate_script_patterns.md, drift-pinned equal by gate_docs_test.go —
// `go:embed` cannot reach outside the package dir) so the agentd BINARY —
// one release unit, already in every pod, carrying the MCP tools whose
// descriptions point at the doc — ships it without image-layout
// coupling. The init-fs boot phase materializes the embedded bytes to
// /sandbox-runtime/docs/gate-script-patterns.md (idempotent copy), the
// stable pod path trigger_create's description names. Boot-time copy on
// purpose: init-fs runs before any app container, so the doc is present
// before the first agent session — no sync mechanism racing pod start.

import (
	"bytes"
	// The //go:embed directive below requires the embed package even
	// though only the directive consumes it (blank import, per the
	// embed package contract for non-embed.FS targets).
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed gate_script_patterns.md
var gateScriptPatternsDoc []byte

// gateScriptPatternsDocName is the stable pod-side filename (the path
// the trigger_create descriptions point at:
// /sandbox-runtime/docs/gate-script-patterns.md).
const gateScriptPatternsDocName = "gate-script-patterns.md"

// ensureGateScriptPatternsDoc materializes the embedded gate-script
// patterns doc into dstDir/gate-script-patterns.md. Idempotent: an
// identical existing file is left untouched (no rewrite, stable mtime);
// a stale or absent file is (re)written atomically (temp + rename), so
// an agentd upgrade replaces an older doc on the next boot. Advisory
// content — the caller owns the failure posture (init-fs logs loudly
// and never blocks boot on it).
func ensureGateScriptPatternsDoc(dstDir string) error {
	dst := filepath.Join(dstDir, gateScriptPatternsDocName)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, gateScriptPatternsDoc) {
		return nil // already current — idempotent no-op
	}
	// 0750: dir traversable by the pod's group (gid 1000), no world
	// write — the same posture the runtime dir's other children carry.
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("gate-docs dir %s: %w", dstDir, err)
	}
	tmp, err := os.CreateTemp(dstDir, ".gate-docs-*")
	if err != nil {
		return fmt.Errorf("gate-docs temp in %s: %w", dstDir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(gateScriptPatternsDoc); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("gate-docs write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gate-docs close: %w", err)
	}
	//nolint:gosec // G306: 0644 — public documentation, zero secrets (CreateTemp starts 0600)
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("gate-docs chmod: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("gate-docs rename to %s: %w", dst, err)
	}
	return nil
}
