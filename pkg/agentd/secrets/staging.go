// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// staging.go — design 0057 R2b (#1165): file-class credential delivery is
// STAGED, never written to the consumed paths by the materializer.
//
// The live defect: in sidecar mode the materializer runs as uid 2000 and
// wrote ~/.ssh/* and ~/.git-credentials directly — OpenSSH rejects
// uid-2000-owned configs on ownership ("Bad owner or permissions"), and
// reset()'s RemoveAll of the shared ~/.ssh destroyed uid-1000-owned user
// state (known_hosts, user keys) on every reload.
//
// The invariant that fixes the class: credential files are written by the
// uid that consumes them. The materializer (whichever process runs it)
// stages canonical bytes + a typed manifest into a staging tree it owns;
// the uid-1000 supervisor pulls the manifest over the §D1 seam and writes
// the delivered files itself. Ownership and modes are correct by
// construction, and reset becomes manifest-scoped on the consumer side
// (the supervisor deletes exactly the entries of its previous ledger —
// user-owned files in the same directories are never touched).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ManifestName is the manifest file inside the staging tree. The pull
// endpoint reads it fresh at request time; its absence means "no file-class
// secrets" (the quiet empty state, design 0057 law 5).
const ManifestName = "manifest.json"

// StagedEntry is one manifest row: the delivered target (absolute,
// confined to the delivery roots), the final mode per the consumer
// contract, and the staged byte file (relative to the staging dir).
type StagedEntry struct {
	Target string `json:"target"`
	Mode   int    `json:"mode"`
	File   string `json:"file"`
}

// File-mode contracts (design 0057 R2b): one table, the single place that
// knows what each file class's consumer requires. Owner-only modes are
// correct because the DELIVERED files are written by the uid-1000
// consumer itself — the CROSS_UID 0640 group-read machinery is obsolete
// for these classes (it solved readability; the consumers' parsers need
// ownership, which only the writer's uid can confer).
const (
	ModeSSHPrivateKey = 0o600 // ssh: refuses group/private keys only on drift; 0600 is the contract
	ModeSSHConfig     = 0o600 // ssh: owner-must-be-invoking-user, never group/world-writable
	ModeGitCredential = 0o600 // git store helper: no checks today; 0600 by secret semantics
	ModeSecretFile    = 0o600 // arbitrary consumer; user may override via metadata "mode"
)

// secretFileModeMetadata is the secret-file class's user-facing override
// key: {"mode": "0640"}. Validated octal; group/world-write rejected.
const secretFileModeMetadata = "mode"

// resolveSecretFileMode applies the mode contract for a secret-file entry,
// honoring the user's "mode" metadata override. Anything that would make
// the delivered credential writable beyond its owner is rejected — the
// manifest is the delivery contract and must never weaken it.
func resolveSecretFileMode(metadata map[string]string) (int, error) {
	raw, ok := metadata[secretFileModeMetadata]
	if !ok || raw == "" {
		return ModeSecretFile, nil
	}
	if len(raw) < 3 || len(raw) > 4 {
		return 0, fmt.Errorf("metadata %q must be 3-4 octal digits", secretFileModeMetadata)
	}
	if strings.HasPrefix(raw, "0") {
		raw = strings.TrimPrefix(raw, "0")
		if raw == "" {
			return ModeSecretFile, nil
		}
	}
	v, err := strconv.ParseInt(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("metadata %q: %v", secretFileModeMetadata, err)
	}
	mode := int(v)
	if mode&0o022 != 0 {
		return 0, fmt.Errorf("metadata %q 0%o: group/world-write is not a deliverable mode", secretFileModeMetadata, mode)
	}
	return mode, nil
}

// stageBuilder accumulates staged bytes + manifest rows for one
// Materialize pass and publishes them atomically (tmp tree → rename
// swap), so the pull endpoint never observes a half-built staging state.
type stageBuilder struct {
	dir     string
	entries []StagedEntry
}

func newStageBuilder(stagingDir string) *stageBuilder {
	return &stageBuilder{dir: stagingDir}
}

// add stages content under rel (a clean, slash-separated relative path
// with no ".." elements) and records the delivery contract.
func (b *stageBuilder) add(rel, target string, mode int, content []byte) error {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return fmt.Errorf("staged path %q escapes staging dir", rel)
	}
	full := filepath.Join(b.dir+".tmp", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	// G115: masked to 9 bits; modes come from the contract constants or
	// the validated metadata override — cannot overflow.
	if err := os.WriteFile(full, content, os.FileMode(mode&0o777)); err != nil { //nolint:gosec
		return err
	}
	b.entries = append(b.entries, StagedEntry{Target: target, Mode: mode, File: rel})
	return nil
}

// publish swaps the tmp tree into place atomically (rename is atomic on
// one filesystem): staging → .old, tmp → staging, then .old is removed.
// A reader either sees the previous complete tree or the new complete
// tree — never a mix.
func (b *stageBuilder) publish() error {
	if err := os.MkdirAll(b.dir+".tmp", 0o700); err != nil {
		return err
	}
	entries := b.entries
	if entries == nil {
		// An empty batch publishes `[]` (the quiet empty state), never
		// `null` — the endpoint's absent-vs-empty distinction must not
		// depend on JSON null handling.
		entries = []StagedEntry{}
	}
	manifest, err := json.Marshal(sortedEntries(entries))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(b.dir+".tmp", ManifestName), manifest, 0o600); err != nil {
		return err
	}
	_ = os.RemoveAll(b.dir + ".old")
	if _, err := os.Stat(b.dir); err == nil {
		if err := os.Rename(b.dir, b.dir+".old"); err != nil {
			return err
		}
	}
	if err := os.Rename(b.dir+".tmp", b.dir); err != nil {
		return err
	}
	return os.RemoveAll(b.dir + ".old")
}

// stageCleanup removes scratch trees left by a crash between builds. The
// published staging tree itself is NOT removed — it is the level-triggered
// source of truth the endpoint serves until the next publish replaces it.
func stageCleanup(stagingDir string) {
	_ = os.RemoveAll(stagingDir + ".tmp")
	_ = os.RemoveAll(stagingDir + ".old")
}

// sortedEntries returns the manifest rows deterministically ordered —
// the staging tree's manifest and the pull's rev derivation both depend
// on a stable order.
func sortedEntries(entries []StagedEntry) []StagedEntry {
	out := make([]StagedEntry, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}
