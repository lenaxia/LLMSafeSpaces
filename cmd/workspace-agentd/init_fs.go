// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// init_fs.go — the `init-fs` subcommand (design 0051 sidecar migration,
// step 1: platform boot logic leaves the runtime image).
//
// This absorbs BOTH init containers' bash into the digest-pinned agentd
// artifact:
//
//   - workspace-dirs (`mkdir -p /pvc/{workspace,home,tmp}`)
//   - credential-setup's filesystem half (the US-35.7 symlink farm, the
//     G21 password install, the #887 admin-token install, the
//     free-models catalog copy)
//
// so that no platform code executes from the stale-able runtime base
// (incident 2026-08-25: factory-built bases carried a pre-#871 agentd
// and crash-looped Init:Error on contract-shape MCP metadata). The
// bootstrap+materialize half of credential-setup moves to the SIDECAR's
// boot phase (sidecar_boot.go) in sidecar mode and stays a chained
// subcommand in legacy mode — both run this same agentd image.
//
// Runs as uid 1000 (the PVC home owner) with RW mounts on the PVC root,
// /sandbox-cfg, /sandbox-runtime, and RO mounts on the password Secret
// and free-models ConfigMap.
//
// Hardening vs the bash heredoc (threat model: /home/sandbox and
// /workspace are PVC — USER-WRITABLE across suspend/resume):
//
//   - Managed paths are replaced via lstat semantics — a pre-planted
//     symlink is removed as a LINK (never followed); the planted target
//     survives untouched. Bash `rm -rf` matched this for the final
//     component but offered no such guarantee compositionally.
//   - Installs create the destination with its final mode from the
//     first byte (temp file created 0600/0400, then renamed) — the
//     credential is never briefly world-readable, preserving G21's
//     `install -m` guarantee without the external binary.
//
// Exit codes: 0 success; 2 flag errors; 1 operational failure (missing
// password source is G46-class fatal — a workspace without its password
// is silently non-functional, so fail loudly into Init:Error).

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const initFSExitOperational = 1

// initFSManagedLinks describes the US-35.7 symlink farm: PVC-side paths
// (user-visible credential locations) → tmpfs targets (pod-scoped, wiped
// on pod death — the PVC keeps only dangling link inodes, never
// plaintext bytes).
func initFSManagedLinks(pvcRoot, runtimeDir string) [][2]string {
	return [][2]string{
		{filepath.Join(pvcRoot, "home", ".ssh"), filepath.Join(runtimeDir, "rt", "ssh")},
		{filepath.Join(pvcRoot, "home", ".secrets"), filepath.Join(runtimeDir, "rt", "secrets")},
		{filepath.Join(pvcRoot, "home", ".git-credentials"), filepath.Join(runtimeDir, "rt", "git-credentials")},
		{filepath.Join(pvcRoot, "workspace", ".local", "opencode", "auth.json"), filepath.Join(runtimeDir, "rt", "auth.json")},
	}
}

// replaceSymlink atomically points path at target, tolerating (and
// replacing) whatever a previous lifecycle or a user left there. It
// NEVER follows a pre-existing symlink at path: os.Remove/os.RemoveAll
// operate on the link inode itself. Idempotent: an already-correct link
// is left alone (stable inode across sidecar-style re-runs).
func replaceSymlink(path, target string) error {
	//nolint:gosec // G703: path derives from package constants (symlinkPairs); MkdirAll only creates the parent
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("parent dir for %s: %w", path, err)
	}
	//nolint:gosec // G703: path derives from package constants; Lstat/Readlink never traverse a planted link
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if cur, rErr := os.Readlink(path); rErr == nil && cur == target {
				return nil // already correct — idempotent no-op
			}
			//nolint:gosec // G703: path derives from package constants; Remove acts on the LINK inode (never follows it)
			if err := os.Remove(path); err != nil { // removes the LINK, not the target
				return fmt.Errorf("remove planted symlink %s: %w", path, err)
			}
		} else {
			// Pre-planted file or directory: platform-owned path,
			// bash `rm -rf` parity (reset() wipes these every reload).
			//nolint:gosec // G703: path derives from package constants (platform-owned runtime paths)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove planted path %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	return os.Symlink(target, path)
}

// installCredential copies src to dst atomically with mode perm,
// creating the temp file with perm from the first byte so the
// credential is never briefly more permissive (G21 `install -m`
// semantics without the external binary).
func installCredential(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	dir := filepath.Dir(dst)
	tmp, err := os.OpenFile(filepath.Join(dir, ".init-fs-*"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	return os.Rename(tmpName, dst)
}

// runInitFSCommand implements `workspace-agentd init-fs`.
func runInitFSCommand(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("init-fs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pvcRoot := fs.String("pvc-root", "/pvc", "PVC root mount (subPath source)")
	cfgDir := fs.String("cfg-dir", "/sandbox-cfg", "pod-scoped config dir (password, free-models)")
	runtimeDir := fs.String("runtime-dir", "/sandbox-runtime", "pod-scoped tmpfs runtime dir")
	pwSource := fs.String("pw-source", "/mnt/secrets/password", "projected password Secret dir")
	freemodels := fs.String("freemodels", "/mnt/freemodels/models.json", "free-models catalog (optional)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "init-fs: "+format+"\n", a...)
		return initFSExitOperational
	}

	// 1. PVC subPath roots (absorbs workspace-dirs).
	for _, d := range []string{"workspace", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(*pvcRoot, d), 0o750); err != nil {
			return fail("subPath root %s: %v", d, err)
		}
	}

	// 2. Tmpfs credential dirs (reset() operates here every reload).
	// 0770 exact (US-4b cross-uid contract): the uid-2000 sidecar's boot
	// phase and reload reset() must traverse and re-materialize here via
	// the pod's shared gid 1000 — 0700 produced sidecar materialize
	// EACCES (kind L3, 2026-08-26). Chmod AFTER MkdirAll: MkdirAll
	// applies the process umask, and 0770 & ~umask(022) = 0750 loses
	// the group-write bit the bridge depends on.
	//
	// rt/ ITSELF is in the managed set (kind run 3, 12:16 UTC): the
	// sidecar's bootstrap writes rt/secrets.json DIRECTLY in rt/, so a
	// 0750 parent silently defeats the batch write (writeEmptySecrets
	// ignores its error) — the restart guard then never holds (K11) and
	// rt/ stat shows 2750 (K3; setgid from the pod fsGroup).
	rtDir := filepath.Join(*runtimeDir, "rt")
	for _, d := range []string{rtDir, filepath.Join(rtDir, "ssh"), filepath.Join(rtDir, "secrets")} {
		if err := os.MkdirAll(d, 0o770); err != nil { //nolint:gosec // G301: 0770 is the US-4b cross-uid contract (see comment above)
			return fail("tmpfs dir %s: %v", d, err)
		}
		if err := os.Chmod(d, 0o770); err != nil { //nolint:gosec // G302: directory (not file) — exact-mode, see comment above
			return fail("tmpfs dir %s mode: %v", d, err)
		}
	}

	// 3. Symlink farm (hardened — see replaceSymlink).
	for _, link := range initFSManagedLinks(*pvcRoot, *runtimeDir) {
		if err := replaceSymlink(link[0], link[1]); err != nil {
			return fail("symlink %s: %v", link[0], err)
		}
	}

	// 4. Password install (G21/G46: required, 0600, never briefly wider).
	if err := installCredential(filepath.Join(*pwSource, "password"), filepath.Join(*cfgDir, "password"), 0o600); err != nil {
		return fail("password source: %v", err)
	}

	// 5. Admin token (#887 D5.1: optional, 0400).
	if _, err := os.Stat(filepath.Join(*pwSource, "admin-token")); err == nil {
		if err := installCredential(filepath.Join(*pwSource, "admin-token"), filepath.Join(*cfgDir, "admin-token"), 0o400); err != nil {
			return fail("admin-token: %v", err)
		}
	}

	// 6. Free-models catalog (optional: relay disabled / not yet fetched).
	// 0640, not 0600: written uid 1000, read by the uid-2000 sidecar's
	// boot phase — the pod's shared gid 1000 is the read bridge (same
	// exception class as the design-0051 boot pair).
	if data, err := os.ReadFile(*freemodels); err == nil {
		if wErr := os.WriteFile(filepath.Join(*cfgDir, "free-models.json"), data, 0o640); wErr != nil { //nolint:gosec // G306: cross-uid read via shared gid 1000 (see comment)
			return fail("free-models copy: %v", wErr)
		}
	}

	_, _ = fmt.Fprintln(stderr, "init-fs: ok")
	return 0
}
