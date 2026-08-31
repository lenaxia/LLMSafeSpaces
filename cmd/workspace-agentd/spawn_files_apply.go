// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_files_apply.go — design 0057 R2b (#1165): the uid-1000 delivery
// side of the file-class pull.
//
// The supervisor is the ONLY writer of the consumed credential paths
// (~/.ssh/*, ~/.git-credentials, the secrets base): it applies the pulled
// manifest with the per-type mode contracts, so every delivered file is
// owned by the consuming uid — the property OpenSSH's config ownership
// check (and every strict consumer's secure-file semantics) requires and
// that a uid-2000 writer could never confer.
//
// Reset is LEDGER-scoped, never directory-scoped: the previous apply's
// ledger minus the current manifest is exactly the stale set; deleting
// only those entries is what makes uid-1000-owned user state in the same
// directories (known_hosts, user keys) survive every reload — the
// blast-radius half of #1165.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	fileDeliveryRootsEnvOverride  = "LLMSAFESPACES_FILE_DELIVERY_ROOTS"
	fileDeliveryLedgerEnvOverride = "LLMSAFESPACES_SPAWN_FILES_LEDGER"
	fileDeliveryLedgerPath        = "/sandbox-runtime/spawn-files-ledger.json"
	sshConfigDFrag                = "config.d"
)

// errBadDeliveryPath marks a manifest entry outside the delivery roots or
// with a deliverable-weakening mode — the whole manifest is refused (a
// manifest the platform did not build would surface as spawn_files_bad_path).
var errBadDeliveryPath = errors.New("delivery path/mode outside contract")

// fileDelivery is the supervisor-side applier. All fields are settable so
// tests can point roots/ledger at temp dirs; production uses
// fileDeliveryFromEnv().
type fileDelivery struct {
	roots      []string // cleaned absolute prefixes a target must live under
	ledgerPath string
	sshDir     string // for the config.d user-fragment home
}

// fileDeliveryFromEnv builds the production delivery config: the rt/*
// credential roots, the tmpfs ledger, and the ssh dir. The ssh dir (the
// config.d user-fragment home) is derived from the roots so test
// overrides stay consistent. The env overrides are the exec-level test
// seams.
func fileDeliveryFromEnv() fileDelivery {
	roots := []string{
		"/sandbox-runtime/rt/ssh",
		"/sandbox-runtime/rt/secrets",
		filepath.Dir("/sandbox-runtime/rt/git-credentials"),
	}
	if v := os.Getenv(fileDeliveryRootsEnvOverride); v != "" {
		roots = strings.Split(v, ":")
	}
	ledger := fileDeliveryLedgerPath
	if v := os.Getenv(fileDeliveryLedgerEnvOverride); v != "" {
		ledger = v
	}
	sshDir := "/sandbox-runtime/rt/ssh"
	for _, r := range roots {
		if filepath.Base(filepath.Clean(r)) == "ssh" {
			sshDir = filepath.Clean(r)
			break
		}
	}
	return fileDelivery{roots: cleanAll(roots), ledgerPath: ledger, sshDir: sshDir}
}

func cleanAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Clean(p))
	}
	return out
}

// deliveryLedger is the durable record of the previously applied path
// set — the level-triggered truth that makes reset exact: delete
// ledger−manifest, nothing else. tmpfs per pod; a missing ledger means
// "nothing delivered yet".
type deliveryLedger struct {
	Paths []string `json:"paths"`
	Rev   string   `json:"rev"`
}

func (d fileDelivery) readLedger() deliveryLedger {
	var l deliveryLedger
	data, err := os.ReadFile(d.ledgerPath)
	if err != nil {
		return deliveryLedger{Paths: []string{}}
	}
	if json.Unmarshal(data, &l) != nil || l.Paths == nil {
		return deliveryLedger{Paths: []string{}}
	}
	return l
}

func (d fileDelivery) writeLedger(l deliveryLedger) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return atomicWriteAny(d.ledgerPath, data, 0o600)
}

// validate refuses a manifest whose entries escape the roots or carry a
// group/world-writable mode — the manifest is a platform-built contract;
// anything else is corruption, not a delivery instruction.
func (d fileDelivery) validate(files []spawnFileEntry) error {
	for _, f := range files {
		clean := filepath.Clean(f.Path)
		inside := false
		for _, root := range d.roots {
			if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
				inside = true
				break
			}
		}
		if !inside {
			return fmt.Errorf("%w: %s", errBadDeliveryPath, f.Path)
		}
		if f.Mode&0o022 != 0 || f.Mode <= 0 {
			return fmt.Errorf("%w: mode 0%o for %s", errBadDeliveryPath, f.Mode, f.Path)
		}
	}
	return nil
}

// apply delivers the manifest and returns the terminal revision over what
// was actually written (I4: computed here, never taken from the response
// Rev). Stale entries (previous ledger − manifest) are removed first —
// revocation is absence; user-owned files in the same dirs are never
// touched because they are not in the ledger.
func (d fileDelivery) apply(files []spawnFileEntry) (string, error) {
	if err := d.validate(files); err != nil {
		return "", err
	}

	old := d.readLedger()
	next := map[string]bool{}
	for _, f := range files {
		next[filepath.Clean(f.Path)] = true
	}
	for _, p := range old.Paths {
		if !next[p] {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("revoke %s: %w", p, err)
			}
		}
	}

	applied := make([]spawnFileEntry, 0, len(files))
	for _, f := range files {
		target := filepath.Clean(f.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		// G115: masked to 9 bits and validated >0 above — cannot overflow.
		if err := atomicWriteAny(target, f.Content, os.FileMode(f.Mode&0o777)); err != nil { //nolint:gosec
			return "", fmt.Errorf("write %s: %w", target, err)
		}
		applied = append(applied, spawnFileEntry{Path: target, Mode: f.Mode, Content: f.Content})
	}

	// The user-fragment home: never a manifest target (the materializer
	// only stages ssh/config), always present so the Include line
	// resolves from the first delivery on.
	if d.sshDir != "" {
		if err := os.MkdirAll(filepath.Join(d.sshDir, sshConfigDFrag), 0o700); err != nil {
			return "", fmt.Errorf("mkdir config.d: %w", err)
		}
	}

	rev := spawnFilesRev(applied)
	paths := make([]string, 0, len(applied))
	for _, f := range applied {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	if err := d.writeLedger(deliveryLedger{Paths: paths, Rev: rev}); err != nil {
		return "", fmt.Errorf("ledger: %w", err)
	}
	return rev, nil
}

// atomicWriteAny writes bytes to path with the final mode atomically:
// temp file in the same directory (same filesystem ⇒ rename is atomic),
// created with the FINAL mode so no window exists with looser bits.
func atomicWriteAny(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".deliver-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// deliverStaged applies the currently published staging manifest in the
// consuming-uid context (single-container boot and reload). Sidecar
// contexts (crossUID) never call this — their delivery is the uid-1000
// supervisor's pull. Absent staging is the quiet empty state and revokes
// via the ledger, exactly like the pull side.
func deliverStaged(cfg materializeConfig) error {
	if cfg.crossUID {
		return nil
	}
	if cfg.secretsEnvPath == "" {
		// An empty config is not a delivery context (handler-level tests
		// construct bare configs; the staging root would be relative to
		// the CWD). Skip rather than write to an unintended location.
		return nil
	}
	staging := cfg.stagingDir
	if staging == "" {
		staging = stagingDirFor(cfg.secretsEnvPath, cfg.crossUID)
	}
	resp, ok := loadStagedFiles(staging)
	if !ok {
		return fmt.Errorf("staged files unreadable: %s", staging)
	}
	_, err := cfg.deliveryFromCfg().apply(resp.Files)
	return err
}
