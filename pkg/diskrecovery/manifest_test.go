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

// PIN (#1744): the production opencode manifest must declare the
// regenerable classes that actually fill workspace PVCs — playwright
// browsers, the session-set /tmp/opencode caches, HOME-side Go scratch
// (gotmp + stale gotmp-w* workers), golangci-lint's cache, npm's
// _cacache/_npx payload, and the envtest assets under /workspace/.local.
// Evidence on the incident pod (2026-10-18/19): three ENOSPC episodes
// from exactly these trees; 97%→60% swings needed manual sweeps because
// none of them was a deletion candidate.
func TestIssue1744RegenerableClassesPresent(t *testing.T) {
	m := ManifestFor("opencode")
	want := []Entry{
		{Class: "playwright-browsers", Path: "/home/sandbox/.cache/ms-playwright", Kind: EntryDir},
		{Class: "tmp-opencode-gocache", Path: "/tmp/opencode/gocache", Kind: EntryDir},
		{Class: "tmp-opencode-goocache", Path: "/tmp/opencode/goocache", Kind: EntryDir},
		{Class: "tmp-opencode-npm-cache", Path: "/tmp/opencode/npm-cache", Kind: EntryDir},
		{Class: "gotmp-residue", Path: "/home/sandbox/.cache/gotmp", Kind: EntryPrefixStale},
		{Class: "golangci-lint-cache", Path: "/home/sandbox/.cache/golangci-lint", Kind: EntryDir},
		{Class: "npm-npx", Path: "/home/sandbox/.npm/_npx", Kind: EntryDir},
		{Class: "kubebuilder-envtest", Path: "/workspace/.local/kubebuilder-envtest", Kind: EntryDir},
	}
	for _, w := range want {
		found := false
		for _, e := range m.Entries {
			if e.Class == w.Class {
				found = true
				if e.Path != w.Path || e.Kind != w.Kind {
					t.Errorf("class %s: want path=%s kind=%s, got path=%s kind=%s", w.Class, w.Path, w.Kind, e.Path, e.Kind)
				}
			}
		}
		if !found {
			t.Errorf("#1744: production manifest must declare class %q (%s)", w.Class, w.Path)
		}
	}
	// #1744 item 5: npm-cache is NARROWED from the whole ~/.npm root to
	// npm's actual cache payload (_cacache is `npm cache clean`'s target).
	npmSeen := false
	for _, e := range m.Entries {
		if e.Class == "npm-cache" {
			npmSeen = true
			if e.Path != "/home/sandbox/.npm/_cacache" {
				t.Errorf("npm-cache must target ~/.npm/_cacache (#1744 item 5), got %s", e.Path)
			}
		}
	}
	if !npmSeen {
		t.Error("#1744: npm-cache class missing entirely")
	}
}

// PIN (#1744 protectedPath discipline): kubebuilder-envtest assets are
// regenerable (setup-envtest re-downloads) but live under the
// /workspace/.local package-home tree — deletable ONLY via the explicit
// root+entry allow, never by weakening the protected list: the package
// homes and every non-allowlisted sibling stay refused.
func TestKubebuilderEnvtestExplicitAllow(t *testing.T) {
	m := ManifestFor("opencode")
	if err := validateEntry(m, Entry{Class: "kubebuilder-envtest", Path: "/workspace/.local/kubebuilder-envtest", Kind: EntryDir}); err != nil {
		t.Fatalf("the explicit #1744 allow must validate, got %v", err)
	}
	for _, p := range []string{
		"/workspace/.local/other-tool",          // sibling: no declared root
		"/workspace/.local",                     // the package-home tree
		"/workspace/.local/share",               // the share tree
		"/workspace/.local/share/mise/installs", // the user's toolchains
	} {
		err := validateEntry(m, Entry{Class: "probe", Path: p, Kind: EntryDir})
		if !errors.Is(err, ErrProtectedPath) && !errors.Is(err, ErrNotAllowlisted) {
			t.Errorf("%s must stay refused, got %v", p, err)
		}
	}
}

// PIN (#1744 /tmp/opencode discipline): the session cache home under
// the PVC "tmp" subpath gets a PARENT root — the root itself (it also
// holds live session scratch and GOBIN tools) is never deletable, and
// the entries under it are EXACTLY the three named cache dirs.
func TestTmpOpencodeCacheRootsDeclared(t *testing.T) {
	m := ManifestFor("opencode")
	rootSeen := false
	for _, r := range m.Roots {
		if r.Path == "/tmp/opencode" {
			rootSeen = true
			if r.Kind != RootParent {
				t.Errorf("/tmp/opencode root must be RootParent (session scratch home), got %s", r.Kind)
			}
		}
	}
	if !rootSeen {
		t.Fatal("#1744: no /tmp/opencode cache root declared")
	}
	if err := validateEntry(m, Entry{Class: "probe", Path: "/tmp/opencode", Kind: EntryDir}); !errors.Is(err, ErrRootItselfProtected) {
		t.Errorf("/tmp/opencode itself must stay root-protected, got %v", err)
	}
	want := map[string]bool{
		"/tmp/opencode/gocache":   true,
		"/tmp/opencode/goocache":  true,
		"/tmp/opencode/npm-cache": true,
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if strings.HasPrefix(e.Path, "/tmp/opencode/") {
			seen[e.Path] = true
		}
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("#1744: cache entry %s must be declared", p)
		}
	}
	for p := range seen {
		if !want[p] {
			t.Errorf("#1744: unexpected entry under /tmp/opencode widens the boundary: %s", p)
		}
	}
}

// PIN (#1744 gate discipline): every REGENERABLE-SET class carries the
// safety gates it was specified with — a per-class size floor and
// staleness on the dir classes. The residue classes (tmp-build-residue,
// gotmp-residue) are floor-0 ON PURPOSE (review r2: the observed
// corpses are empty dirs — inode hygiene is their job; the age gate +
// open-handle guard carry their safety). A class added without its
// gates reopens the reap-under-a-live-process hole the issue documents.
func TestIssue1744EntriesCarryGates(t *testing.T) {
	m := ManifestFor("opencode")
	dirs := map[string]bool{
		"playwright-browsers":    true,
		"tmp-opencode-gocache":   true,
		"tmp-opencode-goocache":  true,
		"tmp-opencode-npm-cache": true,
		"golangci-lint-cache":    true,
		"npm-cache":              true, // #1744 item 5 narrowing
		"npm-npx":                true,
		"kubebuilder-envtest":    true,
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if !dirs[e.Class] && e.Class != "gotmp-residue" {
			continue
		}
		seen[e.Class] = true
		if e.Class == "gotmp-residue" {
			if e.Kind != EntryPrefixStale || e.MinAgeSecs < 120 {
				t.Errorf("gotmp-residue must be prefix_stale with the >=120s corpse rule, got kind=%s minAge=%d", e.Kind, e.MinAgeSecs)
			}
			if e.MinBytes != 0 {
				t.Errorf("gotmp-residue must stay floor 0 (empty corpses are its evidence — review r2), got floor=%d", e.MinBytes)
			}
		} else if e.MinBytes <= 0 {
			t.Errorf("class %s: MinBytes floor missing (got %d)", e.Class, e.MinBytes)
		}
		if e.Class != "gotmp-residue" && e.MinAgeSecs <= 0 {
			t.Errorf("dir class %s: staleness gate missing (MinAgeSecs=%d)", e.Class, e.MinAgeSecs)
		}
	}
	for c := range dirs {
		if !seen[c] {
			t.Errorf("class %s missing from the production manifest", c)
		}
	}
	if !seen["gotmp-residue"] {
		t.Error("class gotmp-residue missing from the production manifest")
	}
	// Negative gates are authoring bugs and must not validate.
	bad := Entry{Class: "neg", Path: "/home/sandbox/.cache/go-build", Kind: EntryDir, MinAgeSecs: -1}
	if err := validateEntry(m, bad); err == nil {
		t.Error("negative MinAgeSecs must be refused at validation")
	}
	badBytes := Entry{Class: "negb", Path: "/home/sandbox/.cache/go-build", Kind: EntryDir, MinBytes: -1}
	if err := validateEntry(m, badBytes); err == nil {
		t.Error("negative MinBytes must be refused at validation")
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
