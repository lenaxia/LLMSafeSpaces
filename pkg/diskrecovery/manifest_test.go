// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// productionManifest is the compiled-in opencode base manifest. Most
// boundary tests exercise IT directly: the production entries are the
// artifact review probes, not synthetic ones.
func TestProductionManifestValidates(t *testing.T) {
	if err := ValidateProductionManifests(); err != nil {
		t.Fatalf("compiled-in manifests must self-validate: %v", err)
	}
}

func TestManifestForUnknownBaseIsEmptyFailSafe(t *testing.T) {
	m := ManifestFor("does-not-exist")
	if len(m.Entries) != 0 || len(m.Roots) != 0 {
		t.Fatalf("unknown base must yield an empty manifest (deny-by-default fail-safe), got %d entries", len(m.Entries))
	}
}

// PIN (never-user-data): an allowlist entry naming a protected path —
// the PVC root, $HOME, /tmp, a package home — must be REFUSED. This is
// the entry that would delete the user's workspace.
func TestEntryNamingWorkspaceRootRefused(t *testing.T) {
	m := ManifestFor("opencode")
	refused := []string{
		"/workspace",
		"/home/sandbox",
		"/tmp",
		"/workspace/.local",
		"/workspace/.local/share",
		"/workspace/.local/share/go",
		"/workspace/.local/share/cargo",
		"/workspace/.local/share/mise",
		"/home/sandbox/.cache", // parent root itself
	}
	for _, p := range refused {
		e := Entry{Class: "probe", Path: p, Kind: EntryDir}
		if err := validateEntry(m, e); !errors.Is(err, ErrProtectedPath) && !errors.Is(err, ErrRootItselfProtected) {
			t.Errorf("entry %s must be refused as protected/root, got %v", p, err)
		}
	}
}

// PIN (deny-by-default): a path outside every declared cache root is
// refused even though the manifest exists — user source trees, home
// files, anything not allowlisted.
func TestEntryOutsideCacheRootsRefused(t *testing.T) {
	m := ManifestFor("opencode")
	for _, p := range []string{
		"/workspace/src",                        // user source
		"/workspace/tmp",                        // /tmp's PVC sibling
		"/home/sandbox/project",                 // user project in home
		"/home/sandbox/.ssh",                    // sensitive
		"/var/cache",                            // system cache outside roots
		"/workspace/.local/share/go/bin",        // user-installed binaries
		"/workspace/.local/share/cargo/bin",     // user-installed binaries
		"/workspace/.local/share/mise/installs", // the user's toolchains
	} {
		e := Entry{Class: "probe", Path: p, Kind: EntryDir}
		if err := validateEntry(m, e); !errors.Is(err, ErrNotAllowlisted) {
			t.Errorf("entry %s must be refused (not allowlisted), got %v", p, err)
		}
	}
}

// PIN (bad path shapes): relative, unclean, and dot-dot paths never
// reach the containment logic.
func TestMalformedEntriesRefused(t *testing.T) {
	m := ManifestFor("opencode")
	for _, p := range []string{"relative/path", "/home/sandbox/.cache/", "/home/sandbox/.cache/../.npm", ""} {
		e := Entry{Class: "probe", Path: p, Kind: EntryDir}
		if err := validateEntry(m, e); !errors.Is(err, ErrBadPath) && !errors.Is(err, ErrNotAllowlisted) {
			t.Errorf("entry %q must be refused as malformed/outside, got %v", p, err)
		}
	}
}

// PIN (symlink escape): an allowlisted cache path that is a symlink
// into user data must be refused at execution-time validation.
func TestSymlinkedCacheEscapeRefused(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home", "sandbox")
	userData := filepath.Join(dir, "workspace", "src")
	if err := os.MkdirAll(filepath.Join(home, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userData, 0o755); err != nil {
		t.Fatal(err)
	}
	// ~/.cache/go-build -> /workspace/src (escape into user source)
	if err := os.Symlink(userData, filepath.Join(home, ".cache", "go-build")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Base: "test",
		Roots: []CacheRoot{
			{Path: filepath.Join(home, ".cache"), Kind: RootParent},
		},
		Entries: []Entry{
			{Class: "go-build-cache", Path: filepath.Join(home, ".cache", "go-build"), Kind: EntryDir},
		},
	}
	err := ValidateManifest(m)
	if err == nil || !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("symlinked cache escaping into user data must be refused, got %v", err)
	}
}

// PIN (prefix class discipline): prefix entries must anchor exactly at
// their declared root, need a positive age filter, and reject relative
// shapes.
func TestPrefixEntryValidation(t *testing.T) {
	parent := "/tmp"
	m := Manifest{
		Base:  "test",
		Roots: []CacheRoot{{Path: "/tmp/go-build", Kind: RootParent}},
	}
	if err := validateEntry(m, Entry{Class: "ok", Path: "/tmp/go-build", Kind: EntryPrefixStale, MinAgeSecs: 120}); err != nil {
		t.Fatalf("well-formed prefix entry must validate, got %v", err)
	}
	if err := validateEntry(m, Entry{Class: "noage", Path: "/tmp/go-build", Kind: EntryPrefixStale}); err == nil {
		t.Fatal("prefix entry without an age filter must be refused (would reap live builds)")
	}
	if err := validateEntry(m, Entry{Class: "nested", Path: "/tmp/go-build/deeper", Kind: EntryPrefixStale, MinAgeSecs: 1}); err == nil {
		t.Fatal("prefix entry not anchored at its root must be refused")
	}
	if err := validateEntry(m, Entry{Class: "wrongroot", Path: parent + "/other", Kind: EntryPrefixStale, MinAgeSecs: 1}); err == nil {
		t.Fatal("prefix entry under an undeclared root must be refused")
	}
}

// PIN (longest-root match): when roots nest, the most specific
// declaration governs — a RootSelf root nested inside a RootParent
// root is deletable itself (the parent rule would refuse it), and the
// production manifest has NO GOPATH-family root: everything under
// GOPATH outside the module cache is refused (bin/ and pkg/ hold user
// state).
func TestLongestRootWins(t *testing.T) {
	m := Manifest{
		Base: "test",
		// Overlapping roots where classification actually differs for
		// the entry: inner declaration says PARENT (the entry root
		// itself is NOT deletable), outer says SELF (descendants and
		// the root are). Longest-match must win → the entry equals the
		// inner PARENT root → refused. A first/last-match selector that
		// ignores specificity would consult the outer SELF root and
		// allow it.
		Roots: []CacheRoot{
			{Path: "/data/mod", Kind: RootParent},
			{Path: "/data", Kind: RootSelf},
		},
	}
	if err := validateEntry(m, Entry{Class: "innerroot", Path: "/data/mod", Kind: EntryDir}); !errors.Is(err, ErrRootItselfProtected) {
		t.Fatalf("longest (inner, parent-kind) root must govern its own path, got %v", err)
	}
	if err := validateEntry(m, Entry{Class: "innerdeep", Path: "/data/mod/sub", Kind: EntryDir}); err != nil {
		t.Fatalf("strict descendant of the inner parent root must validate, got %v", err)
	}
	if err := validateEntry(m, Entry{Class: "outerroot", Path: "/data", Kind: EntryDir}); err != nil {
		t.Fatalf("outer self root is deletable itself, got %v", err)
	}
	if err := validateEntry(m, Entry{Class: "outersibling", Path: "/data/other", Kind: EntryDir}); err != nil {
		t.Fatalf("descendant of the outer self root must validate, got %v", err)
	}
	// Production pin: no GOPATH root exists — user state under GOPATH
	// outside the module cache is not allowlisted.
	prod := ManifestFor("opencode")
	for _, p := range []string{
		"/workspace/.local/share/go/pkg",
		"/workspace/.local/share/go/pkg/mod/cache", // INSIDE modcache — must PASS
	} {
		if strings.HasPrefix(p, "/workspace/.local/share/go/pkg/mod") {
			if err := validateEntry(prod, Entry{Class: "probe", Path: p, Kind: EntryDir}); err != nil {
				t.Errorf("modcache descendant %s must validate, got %v", p, err)
			}
		} else if err := validateEntry(prod, Entry{Class: "probe", Path: p, Kind: EntryDir}); !errors.Is(err, ErrNotAllowlisted) {
			t.Errorf("%s must be refused (no GOPATH root in production), got %v", p, err)
		}
	}
}
