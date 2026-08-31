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

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
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

// stagingManifest is the revisioned manifest shape (US-70.2): when the
// materialized batch carries a revision, publish writes this object
// with rev = "<seq>:<manifestHash>"; without a revision the manifest
// stays the legacy bare array (byte-compat — old readers never see a
// shape change for legacy batches).
type stagingManifest struct {
	Rev     string        `json:"rev,omitempty"`
	Entries []StagedEntry `json:"entries"`
}

// ReadStagingManifest parses manifest bytes of EITHER shape into rows
// plus the revision anchor ("" for the legacy array).
func ReadStagingManifest(data []byte) ([]StagedEntry, string, error) {
	var entries []StagedEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		return entries, "", nil
	}
	var m stagingManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", err
	}
	if m.Entries == nil {
		return nil, "", fmt.Errorf("staging manifest has no entries")
	}
	return m.Entries, m.Rev, nil
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
// All I/O goes through the Materializer's Filesystem — staging is as
// fake-injectable as every other materialize write.
type stageBuilder struct {
	fs      Filesystem
	dir     string
	entries []StagedEntry
	total   int
	// rev is the revision anchor ("<seq>:<manifestHash>") stamped into
	// the published manifest when the materialized batch carried a
	// revision; empty publishes the legacy bare array.
	rev string
}

func newStageBuilder(fs Filesystem, stagingDir string) *stageBuilder {
	if fs == nil {
		fs = RealFS()
	}
	return &stageBuilder{fs: fs, dir: stagingDir}
}

// writeFile lands bytes at path with perm via the Filesystem seam.
func (b *stageBuilder) writeFile(path string, perm os.FileMode, content []byte) error {
	if err := b.fs.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	w, err := b.fs.OpenForCreate(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// add stages content under rel (a clean, slash-separated relative path
// with no ".." elements) and records the delivery contract.
func (b *stageBuilder) add(rel, target string, mode int, content []byte) error {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return fmt.Errorf("staged path %q escapes staging dir", rel)
	}
	// W8 per-entry ceiling: a single entry larger than the whole delivery
	// budget can never be served; skip it loudly at stage time (the
	// bind-time 400 is the user-facing gate — this is the bypass guard).
	if len(content) > agentd.StagedFilesMaxBytes {
		return fmt.Errorf("size_exceeded: staged entry %s is %d bytes; the delivery budget is %d",
			rel, len(content), agentd.StagedFilesMaxBytes)
	}
	b.total += len(content)
	full := filepath.Join(b.dir+".tmp", filepath.FromSlash(rel))
	// G115: masked to 9 bits; modes come from the contract constants or
	// the validated metadata override — cannot overflow.
	if err := b.writeFile(full, os.FileMode(mode&0o777), content); err != nil { //nolint:gosec
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
	if err := b.fs.MkdirAll(b.dir+".tmp", 0o700); err != nil {
		return err
	}
	entries := b.entries
	if entries == nil {
		// An empty batch publishes `[]` (the quiet empty state), never
		// `null` — the endpoint's absent-vs-empty distinction must not
		// depend on JSON null handling.
		entries = []StagedEntry{}
	}
	deterministic := sortedEntries(entries)
	var manifest []byte
	var err error
	if b.rev == "" {
		manifest, err = json.Marshal(deterministic)
	} else {
		manifest, err = json.Marshal(stagingManifest{Rev: b.rev, Entries: deterministic})
	}
	if err != nil {
		return err
	}
	if err := b.writeFile(filepath.Join(b.dir+".tmp", ManifestName), 0o600, manifest); err != nil {
		return err
	}
	_ = b.fs.RemoveAll(b.dir + ".old")
	// Swap: current → .old (a missing source's error is the first
	// publish — ignored), tmp → current, then drop .old. The gap between
	// the two renames is microseconds and only occurs mid-restage; a
	// concurrent pull in it observes the quiet empty manifest and the
	// next pull re-delivers — level-triggered by construction.
	_ = b.fs.Rename(b.dir, b.dir+".old")
	if err := b.fs.Rename(b.dir+".tmp", b.dir); err != nil {
		return err
	}
	return b.fs.RemoveAll(b.dir + ".old")
}

// stageCleanup removes scratch trees left by a crash between builds. The
// published staging tree itself is NOT removed — it is the level-triggered
// source of truth the endpoint serves until the next publish replaces it.
func stageCleanup(fs Filesystem, stagingDir string) {
	if fs == nil {
		fs = RealFS()
	}
	_ = fs.RemoveAll(stagingDir + ".tmp")
	_ = fs.RemoveAll(stagingDir + ".old")
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
