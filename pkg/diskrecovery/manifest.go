// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package diskrecovery

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// EntryKind distinguishes whole-directory cache entries from the
// age-filtered /tmp corpse class.
type EntryKind string

const (
	// EntryDir: the entry path is a cache directory. Missing = not
	// present (normal); present = deletable wholesale.
	EntryDir EntryKind = "dir"
	// EntryPrefixStale: the entry path is a NAME PREFIX under its
	// parent (e.g. /tmp/go-build matches /tmp/go-build12345). Only
	// children whose mtime is older than MinAgeSecs are candidates —
	// live builds must never be reaped out from under a compiler.
	EntryPrefixStale EntryKind = "prefix_stale"
)

// Entry is one deletion candidate class in the manifest.
type Entry struct {
	// Class is the report-facing name (stable contract with the
	// frontend render).
	Class string
	// Path is an absolute literal. For EntryDir: the cache directory
	// itself. For EntryPrefixStale: the fixed child-name prefix.
	Path string
	Kind EntryKind
	// MinAgeSecs gates age-filtered candidacy. For EntryPrefixStale it
	// is MANDATORY (validatePrefixEntry refuses <= 0): only children
	// whose mtime is older than the gate are candidates — live builds
	// must never be reaped out from under a compiler. For EntryDir it
	// is OPTIONAL (0 = no gate): a positive value holds the whole class
	// while the directory's own mtime is younger than the gate — a
	// freshly-written cache dir is likely mid-population (#1744).
	MinAgeSecs int64
	// MinBytes is the per-class size floor (#1744): a measured class
	// below the floor is not a deletion candidate (not_present, with
	// the floor in the reason) — a small tree is a partial download or
	// debris, not the full regenerable set the floor was calibrated
	// against. 0 = no floor (the pre-#1744 classes keep 0).
	MinBytes int64
}

// RootKind marks whether a cache root may itself be deleted.
type RootKind string

const (
	// RootSelf: the root IS the cache (npm's ~/.npm, the go module
	// cache) — an entry may target the root itself.
	RootSelf RootKind = "self"
	// RootParent: the root is a shared home for caches (XDG ~/.cache) —
	// only strict descendants are ever deletable, never the root.
	RootParent RootKind = "parent"
)

// CacheRoot declares one allowlisted cache root. Roots are part of the
// compiled-in manifest (deny-by-default: anything outside a root is
// untouchable by construction).
type CacheRoot struct {
	Path string
	Kind RootKind
}

// Manifest is the per-runtime-base declarative allowlist. The platform
// owns the runtime images; adding a runtime base means adding a
// manifest in this package, versioned and reviewed with the executor.
type Manifest struct {
	Base    string
	Roots   []CacheRoot
	Entries []Entry
}

// Refusal reasons (the wire vocabulary for ClassReport.Reason).
var (
	// ErrNotAllowlisted: the entry path is not inside any declared
	// cache root — deny-by-default tripped.
	ErrNotAllowlisted = errors.New("path is not inside any declared cache root")
	// ErrRootItselfProtected: the entry targets a RootParent root (e.g.
	// all of ~/.cache) — only strict descendants are deletable.
	ErrRootItselfProtected = errors.New("parent cache root itself is not deletable")
	// ErrProtectedPath: the entry is (or contains) a protected path —
	// the never-user-data boundary.
	ErrProtectedPath = errors.New("entry is or contains a protected path")
	// ErrSymlinkEscape: the entry resolves (via symlink) outside the
	// cache root that contains its literal path.
	ErrSymlinkEscape = errors.New("symlink resolution escapes the cache root")
	// ErrBadPath: the entry path is not an absolute clean path.
	ErrBadPath = errors.New("path must be absolute and clean")
)

// protectedPaths is the manifest-independent anchor list: the PVC
// volume roots, $HOME and its share roots, the platform package homes,
// and the filesystem root. NOT overridable by any manifest — this is
// the fixed half of the never-user-data boundary. An entry that is
// equal to OR an ancestor of any of these is refused.
var protectedPaths = []string{
	"/",
	"/workspace",
	"/home",
	"/home/sandbox",
	"/tmp",
	// /workspace/.local tree: package homes hold user-installed tools
	// (mise installs, go bin, cargo bin, npm prefix) — only their
	// cache-by-construction subpaths are ever allowlisted.
	"/workspace/.local",
	"/workspace/.local/share",
	"/workspace/.local/share/go",
	"/workspace/.local/share/cargo",
	"/workspace/.local/share/mise",
	"/workspace/.local/share/pnpm",
	"/workspace/.local/share/gem",
	// $HOME tree outside the cache roots.
	"/home/sandbox/.local",
	"/home/sandbox/.local/share",
	"/home/sandbox/.cache",
}

// manifests is the compiled-in registry. LLMSAFESPACE_RUNTIME_BASE may
// only SELECT a key here; unknown → empty manifest (fail-safe: every
// class reports not_present/refused, nothing is ever deleted).
var manifests = map[string]Manifest{
	// The opencode runtime base (runtimes/base → runtimes/opencode).
	// Path literals mirror the controller-injected platform env
	// (controller/internal/workspace/platform_env.go) and the PVC mount
	// topology (pod_builder.go): GOPATH=/workspace/.local/share/go,
	// CARGO_HOME=/workspace/.local/share/cargo,
	// MISE_DATA_DIR=/workspace/.local/share/mise, $HOME=/home/sandbox,
	// /tmp = PVC "tmp" subpath. GOCACHE/GOMODCACHE are unset in the pod
	// env, so Go's defaults land under $HOME/.cache/go-build and
	// $GOPATH/pkg/mod.
	"opencode": {
		Base: "opencode",
		Roots: []CacheRoot{
			// XDG-ish user cache tree: go-build, pip, mise's XDG cache.
			// Parent root — the whole tree is NOT deletable wholesale.
			{Path: "/home/sandbox/.cache", Kind: RootParent},
			// npm's cache IS ~/.npm (npm cache clean target).
			{Path: "/home/sandbox/.npm", Kind: RootSelf},
			// pnpm's content-addressed store (re-linked, re-downloadable).
			{Path: "/home/sandbox/.local/share/pnpm/store", Kind: RootSelf},
			// Go module cache — exactly `go clean -modcache`'s target.
			{Path: "/workspace/.local/share/go/pkg/mod", Kind: RootSelf},
			// Cargo's downloaded .crate archives only. CARGO_HOME root,
			// bin/ (user tools) and registry/src stay untouched.
			{Path: "/workspace/.local/share/cargo/registry/cache", Kind: RootSelf},
			// mise's raw download cache. MISE_DATA_DIR root and installs/
			// (the user's toolchains) stay untouched.
			{Path: "/workspace/.local/share/mise/downloads", Kind: RootSelf},
			// #1744 item 6: envtest assets (etcd+kube-apiserver binaries).
			// Regenerable — setup-envtest re-downloads on demand — but they
			// live under the protected /workspace/.local package-home tree
			// (see protectedPaths): the explicit allow is EXACTLY this
			// directory-sized root, never the package homes around it.
			{Path: "/workspace/.local/kubebuilder-envtest", Kind: RootSelf},
			// Stale go build corpses under /tmp (pod PVC "tmp" subpath).
			{Path: "/tmp/go-build", Kind: RootParent},
			// #1744: the session cache home on the PVC "tmp" subpath
			// (agents set GOCACHE=/tmp/opencode/gocache and friends).
			// RootParent on purpose: /tmp/opencode also hosts LIVE
			// session scratch (session dirs, logs) and GOBIN-installed
			// tools — only the three declared cache entries under it
			// are ever deletion candidates.
			{Path: "/tmp/opencode", Kind: RootParent},
			// #1744: anchor for the stale GOTMP worker-scratch prefix
			// class. Workers with HOME=/home/sandbox get Go's defaults
			// under ~/.cache — gotmp, gotmp-*, gotmp-w* scratch dirs
			// that outlive their sessions (observed: gotmp-1735).
			{Path: "/home/sandbox/.cache/gotmp", Kind: RootParent},
		},
		Entries: []Entry{
			{Class: "go-build-cache", Path: "/home/sandbox/.cache/go-build", Kind: EntryDir},
			{Class: "pip-cache", Path: "/home/sandbox/.cache/pip", Kind: EntryDir},
			{Class: "mise-xdg-cache", Path: "/home/sandbox/.cache/mise", Kind: EntryDir},
			// #1744 item 5: NARROWED from the whole ~/.npm root to
			// npm's actual cache payload — _cacache is exactly `npm
			// cache clean`'s target; _logs and friends stay untouched
			// (_npx is its own class below).
			{Class: "npm-cache", Path: "/home/sandbox/.npm/_cacache", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 32 << 20},
			{Class: "pnpm-store", Path: "/home/sandbox/.local/share/pnpm/store", Kind: EntryDir},
			{Class: "go-module-cache", Path: "/workspace/.local/share/go/pkg/mod", Kind: EntryDir},
			{Class: "cargo-download-cache", Path: "/workspace/.local/share/cargo/registry/cache", Kind: EntryDir},
			{Class: "mise-download-cache", Path: "/workspace/.local/share/mise/downloads", Kind: EntryDir},
			// /tmp/go-build<N> corpse dirs untouched for >120s. The
			// prefix is fixed here; /tmp as a whole is never a target.
			{Class: "tmp-build-residue", Path: "/tmp/go-build", Kind: EntryPrefixStale, MinAgeSecs: 120},
			// ---- #1744: the regenerable classes that actually fill
			// workspace PVCs. Three safety gates apply: the per-class
			// staleness gate below (dir mtime), the engine's open-handle
			// scan (/proc/*/fd — never reap under a live process), and
			// the MinBytes floor (calibrated to the observed FULL-set
			// sizes on the incident pod: playwright ~641M, session
			// gocache ~2.4G, npm _cacache+_npx ~260M, golangci ~103M,
			// envtest ~163M — below the floor a tree is a partial
			// download or debris, left alone).
			// Item 1: playwright browser binaries — re-downloadable via
			// `playwright install`; the single biggest miss pre-#1744.
			{Class: "playwright-browsers", Path: "/home/sandbox/.cache/ms-playwright", Kind: EntryDir, MinAgeSecs: 600, MinBytes: 128 << 20},
			// Item 2: the session-set cache targets under /tmp/opencode
			// (goocache is carried for lanes that split GOCACHE/GOO —
			// absent trees simply report not_present).
			{Class: "tmp-opencode-gocache", Path: "/tmp/opencode/gocache", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 64 << 20},
			{Class: "tmp-opencode-goocache", Path: "/tmp/opencode/goocache", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 64 << 20},
			{Class: "tmp-opencode-npm-cache", Path: "/tmp/opencode/npm-cache", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 32 << 20},
			// Item 3's novel half: gotmp + gotmp-w* worker scratch under
			// ~/.cache (go-build itself is the existing class above).
			// The "gotmp" prefix matches the whole GOTMP-family naming
			// convention; 120s matches the /tmp corpse rule. Floor 0 on
			// purpose (review r2): the observed corpses are EMPTY dirs
			// (gotmp-1735) — inode hygiene is exactly this class's job,
			// and the age gate + open-handle guard carry its safety.
			{Class: "gotmp-residue", Path: "/home/sandbox/.cache/gotmp", Kind: EntryPrefixStale, MinAgeSecs: 120},
			// Item 4: golangci-lint's build cache.
			{Class: "golangci-lint-cache", Path: "/home/sandbox/.cache/golangci-lint", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 16 << 20},
			// Item 5, second half: npx-installed packages.
			{Class: "npm-npx", Path: "/home/sandbox/.npm/_npx", Kind: EntryDir, MinAgeSecs: 300, MinBytes: 16 << 20},
			// Item 6: envtest assets — regenerable (setup-envtest
			// re-downloads on demand) but under the protected
			// /workspace/.local package-home tree: deletable ONLY via
			// this explicit entry + the exact-size RootSelf root above.
			{Class: "kubebuilder-envtest", Path: "/workspace/.local/kubebuilder-envtest", Kind: EntryDir, MinAgeSecs: 600, MinBytes: 64 << 20},
		},
	},
}

// ManifestFor resolves the compiled-in manifest for a runtime base.
//
// Contract (pinned): an unknown base yields an EMPTY manifest —
// fail-safe, deliberately not an error. The env selects an optional
// feature; a typo must never kill a workspace pod (the D5.2/D5.3
// fail-closed doctrine covers UNGATED muxes, not feature narrowing —
// and an empty manifest can delete nothing). Contrast
// ValidateProductionManifests/NewDiskRecoverEngine: a KNOWN base whose
// entries fail boundary validation IS boot-fatal — that is a manifest
// bug, not an operator typo.
func ManifestFor(base string) Manifest {
	m, ok := manifests[strings.ToLower(strings.TrimSpace(base))]
	if !ok {
		return Manifest{Base: base}
	}
	return m
}

// DefaultRuntimeBase is used when the selection env is unset.
const DefaultRuntimeBase = "opencode"

// ValidateManifest checks every entry against the boundary rules.
// Returns the first refusal (wrapped with the class name) so callers
// can fail loudly at wiring time; Recover re-validates per entry at
// execution time regardless (defense against post-wiring changes to
// the filesystem, e.g. symlinked caches).
func ValidateManifest(m Manifest) error {
	for _, e := range m.Entries {
		if err := validateEntry(m, e); err != nil {
			return fmt.Errorf("entry %s (%s): %w", e.Class, e.Path, err)
		}
	}
	return nil
}

// validateEntry enforces the boundary for one entry against the
// manifest's roots and the fixed protected list. The filesystem is
// consulted only for symlink resolution of the deepest EXISTING
// ancestor (read-only).
func validateEntry(m Manifest, e Entry) error {
	p := filepath.Clean(e.Path)
	if !filepath.IsAbs(p) || p != e.Path || strings.Contains(e.Path, "..") {
		return ErrBadPath
	}
	// Gate sanity (#1744): negative gate values are authoring bugs —
	// they would silently invert the staleness/floor comparisons.
	if e.MinAgeSecs < 0 || e.MinBytes < 0 {
		return fmt.Errorf("gates must be >= 0: minAgeSecs=%d minBytes=%d", e.MinAgeSecs, e.MinBytes)
	}
	// Protected FIRST: naming a protected path — the PVC root, $HOME,
	// /tmp, a package home, or any ancestor of them — is the
	// never-user-data tripwire, and must classify as such regardless
	// of root containment.
	if err := checkProtected(p); err != nil {
		return err
	}
	if e.Kind == EntryPrefixStale {
		return validatePrefixEntry(m, e)
	}
	root := containingRoot(m, p)
	if root == nil {
		return ErrNotAllowlisted
	}
	// A parent root never surrenders itself.
	if root.Kind == RootParent && p == filepath.Clean(root.Path) {
		return ErrRootItselfProtected
	}
	// Symlink containment: resolve the deepest existing ancestor of the
	// entry and require the resolution to stay inside the SAME root.
	return checkSymlinkContainment(p, root)
}

// validatePrefixEntry: the entry path must BE a declared root — the
// anchor — so the reaped children are exactly <root><suffix>* one
// level under the root's parent. The age filter is mandatory (a
// filterless prefix class would reap live builds).
func validatePrefixEntry(m Manifest, e Entry) error {
	if e.MinAgeSecs <= 0 {
		return fmt.Errorf("prefix_stale entry %s: MinAgeSecs must be > 0", e.Class)
	}
	for i := range m.Roots {
		if filepath.Clean(m.Roots[i].Path) == filepath.Clean(e.Path) {
			return nil
		}
	}
	return ErrNotAllowlisted
}

// containingRoot returns the root whose path contains p at any depth
// (p == root allowed only for RootSelf, enforced by callers).
func containingRoot(m Manifest, p string) *CacheRoot {
	best := ""
	var found *CacheRoot
	for i := range m.Roots {
		r := filepath.Clean(m.Roots[i].Path)
		if r == best {
			continue
		}
		if p == r || strings.HasPrefix(p, r+"/") {
			if best == "" || len(r) > len(best) {
				best = r
				found = &m.Roots[i]
			}
		}
	}
	return found
}

// checkProtected refuses p when p is a protected path or an ancestor of
// one (an ancestor "contains" a protected path — deleting it would
// delete the protected path with it).
func checkProtected(p string) error {
	for _, prot := range protectedPaths {
		q := filepath.Clean(prot)
		if p == q {
			return fmt.Errorf("%w: %s", ErrProtectedPath, p)
		}
		if strings.HasPrefix(q, p+"/") {
			return fmt.Errorf("%w: %s contains %s", ErrProtectedPath, p, q)
		}
	}
	return nil
}

// checkSymlinkContainment resolves p (or its deepest existing ancestor
// when the tail is missing) via EvalSymlinks and requires the
// resolution to remain inside the same root. A cache dir replaced by a
// symlink into user data — or a root whose ancestors relocate it off
// the allowlisted tree — is refused.
func checkSymlinkContainment(p string, root *CacheRoot) error {
	rp := filepath.Clean(root.Path)
	full, ok := resolveExisting(p)
	if !ok {
		// Nothing on the path exists at all: no symlink can be in
		// play; the literal path already passed the root check, and
		// measurement treats it as not_present.
		return nil
	}
	if full != rp && !strings.HasPrefix(full, rp+"/") {
		return fmt.Errorf("%w: %s resolves to %s", ErrSymlinkEscape, p, full)
	}
	return checkProtected(full)
}

// resolveExisting EvalSymlinks the deepest existing ancestor of p and
// returns the fully resolved path (resolved ancestor + missing tail).
// ok=false when no ancestor of p (not even /) exists — impossible for
// absolute paths, handled defensively.
func resolveExisting(p string) (full string, ok bool) {
	cur := filepath.Clean(p)
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			out := resolved
			for _, s := range suffix {
				out = filepath.Join(out, s)
			}
			return out, true
		}
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
}
