// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// scrub_legacy_keys.go — US-72.6 (design 0058 §8, Epic 72 close-out):
// the migration scrub that removes legacy provider-key plaintext from
// PVC residue. NARROW BY DESIGN (the design 0051 §3 boundary): the scrub
// touches exactly two platform-shaped surfaces under the workspace root
// and NOTHING else —
//
//   - <root>/.local/opencode/auth.json when it is a REGULAR FILE: the
//     pre-US-35.7 legacy shape (since #1296 the platform maintains a
//     SYMLINK there; a regular file is old-PVC residue). The scrub
//     strips the `key` fields from the entries, preserving structure —
//     the file survives, the key material does not.
//   - regular files NAMED agent-config.json under <root>/.local (bounded
//     depth): stale copies of the platform-rendered config; the scrub
//     strips provider `options.apiKey` fields, preserving siblings.
//
// NEVER touched: the live symlink (the platform owns the resolved store
// through its own machinery), any user-authored file (any other name,
// any location outside the two shapes — even with key-shaped fields),
// anything outside <root>/.local. Idempotent: a second run removes
// nothing (AlreadyClean).

// ScrubReport is the scrub outcome — the subcommand's JSON output and
// the tracker's healthz mirror both carry it.
type ScrubReport struct {
	// AuthKeysRemoved is the number of `key` fields stripped from the
	// legacy regular-file auth.json.
	AuthKeysRemoved int `json:"authKeysRemoved"`
	// ConfigKeysRemoved is the number of provider apiKey fields stripped
	// from agent-config.json copies under .local.
	ConfigKeysRemoved int `json:"configKeysRemoved"`
	// AlreadyClean is true when this run removed nothing (the
	// idempotent steady state).
	AlreadyClean bool `json:"alreadyClean"`
	// AuthPathSkipped is true when the auth path held a symlink or was
	// absent — the healthy live shape.
	AuthPathSkipped bool `json:"authPathSkipped"`
}

// scrubLegacyKeys performs the narrow scrub under root and reports the
// outcome. It never fails on absent paths (a clean workspace is the
// steady state); it fails only on I/O or unmarshal errors of shapes it
// elected to scrub (a malformed legacy file is surfaced loudly, never
// silently skipped).
func scrubLegacyKeys(root string) (ScrubReport, error) {
	var report ScrubReport

	// Surface 1: the legacy regular-file auth.json.
	authPath := filepath.Join(root, ".local", "opencode", "auth.json")
	fi, err := os.Lstat(authPath)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		report.AuthPathSkipped = true // the live #1296 shape
	case err == nil && fi.Mode().IsRegular():
		removed, err := stripKeysFromFile(authPath, stripAuthEntryKeys)
		if err != nil {
			return report, fmt.Errorf("scrub legacy auth.json: %w", err)
		}
		report.AuthKeysRemoved = removed
	case err == nil:
		report.AuthPathSkipped = true // neither regular nor symlink — not ours
	default:
		report.AuthPathSkipped = true // absent — the clean steady state
	}

	// Surface 2: agent-config.json copies under .local (bounded walk).
	walkRoot := filepath.Join(root, ".local")
	_ = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == walkRoot {
			return nil // walk errors on unrelated subtrees never abort the scrub
		}
		if d.IsDir() {
			// Bounded depth: .local/<one-or-two>/agent-config.json covers
			// the known copy shapes (share/, opencode/, config/opencode/).
			// strings.Count on the separator — filepath.SplitList splits
			// PATH LISTS on ':' and never bounds anything (r1 finding 1;
			// .local hosts the user's whole toolchain tree: MISE_DATA_DIR,
			// GOPATH, CARGO_HOME... — an unbounded walk would scrub
			// user-authored copies at any depth).
			rel, _ := filepath.Rel(walkRoot, path)
			if strings.Count(rel, string(filepath.Separator)) > 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "agent-config.json" || !d.Type().IsRegular() {
			return nil
		}
		// The depth bound applies to FILES too (r2 finding 1): the
		// directory SkipDir at >2 separators never fires for a file
		// DIRECTLY at 3 separators (its parent dir has only 2) — a user
		// copy at .local/a/b/c/agent-config.json was still scrubbed.
		if rel, rerr := filepath.Rel(walkRoot, path); rerr == nil && strings.Count(rel, string(filepath.Separator)) > 2 {
			return nil
		}
		removed, serr := stripKeysFromFile(path, stripConfigProviderKeys)
		if serr != nil {
			return nil // a malformed COPY never aborts the scrub; it is residue
		}
		report.ConfigKeysRemoved += removed
		return nil
	})

	report.AlreadyClean = report.AuthKeysRemoved == 0 && report.ConfigKeysRemoved == 0
	return report, nil
}

// stripKeysFromFile loads the JSON object at path, applies the stripper,
// and rewrites the file IN PLACE when the stripper removed anything.
func stripKeysFromFile(path string, strip func(map[string]any) int) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	removed := strip(doc)
	if removed == 0 {
		return 0, nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode()
	}
	if err := os.WriteFile(path, out, mode); err != nil {
		return 0, err
	}
	return removed, nil
}

// stripAuthEntryKeys removes every top-level entry's "key" field (the
// opencode auth-store shape: {"provider": {"type": ..., "key": ...}}).
func stripAuthEntryKeys(doc map[string]any) int {
	removed := 0
	for _, v := range doc {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, has := entry["key"]; has {
			delete(entry, "key")
			removed++
		}
	}
	return removed
}

// stripConfigProviderKeys removes every provider entry's
// options.apiKey (the platform-rendered agent-config shape).
func stripConfigProviderKeys(doc map[string]any) int {
	removed := 0
	providers, ok := doc["provider"].(map[string]any)
	if !ok {
		return 0
	}
	for _, v := range providers {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		opts, ok := entry["options"].(map[string]any)
		if !ok {
			continue
		}
		if _, has := opts["apiKey"]; has {
			delete(opts, "apiKey")
			removed++
		}
	}
	return removed
}

// runScrubLegacyKeysCommand is the `scrub-legacy-keys` subcommand: runs
// the scrub and prints the report as JSON (the in-process boot wiring
// consumes the same ScrubReport type through the tracker).
func runScrubLegacyKeysCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scrub-legacy-keys", flag.ContinueOnError)
	root := fs.String("workspace-root", "/workspace", "workspace root (the PVC mount)")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stderr, "scrub-legacy-keys: %v\n", err)
		return 2
	}
	report, err := scrubLegacyKeys(*root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scrub-legacy-keys: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(report); err != nil {
		_, _ = fmt.Fprintf(stderr, "scrub-legacy-keys: encode report: %v\n", err)
		return 1
	}
	return 0
}
