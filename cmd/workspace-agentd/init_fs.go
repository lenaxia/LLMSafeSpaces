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

// ensureExchangeDir creates/repairs the exchange/ PVC subPath — the
// board payload exchange dir (design 0070 §2.4a), the ONE PVC subtree
// both uid spaces write.
//
// Mode decision (0770 shared group, NOT 1777 sticky — recorded in the
// design-0070 amendment): deletion permission for an unlink comes from
// the PARENT directory's write bit, not file ownership — a sticky bit
// would block the sidecar (uid 2000) unlinking the runtime's (uid
// 1000) payload files, which is the exact operation the dir exists
// for. Group rwx under the pod's shared gid 1000 (pod RunAsGroup +
// FSGroup; the sidecar runs RunAsGroup 1000, the runtime container
// carries gid 1000 supplemental) is the US-4b rt/ cross-uid
// precedent. Explicit Chmod after creation because MkdirAll applies
// the process umask (0770 & ~022 = 0750 loses the group-write bit the
// contract hangs on).
//
// Threat model: the dir holds ONLY short-lived board payload files;
// the sidecar deletes only files it CONSUMED (the anchored path its
// read discipline validated — regular file, caps, UTF-8), never a
// sweep; nothing else in the user's tree is reachable through it.
//
// Cross-uid tolerance (a mode flip on an existing PVC re-creates the
// pod under the OTHER uid's init): when the dir already exists at
// EXACTLY the contract mode (0770, no sticky), chmod is SKIPPED — the
// other uid owns the dir and a chmod would EPERM. Anything else —
// missing group bits, OR a planted 0777/1777 (world/sticky: a sticky
// bit blocks exactly the cross-uid unlink this dir exists for, review
// r1 finding) — is repaired when the running uid owns the dir and
// fails loudly into Init:Error when it cannot (a root-owned
// kubelet-fallback dir needs operator repair; the failure is bounded
// and named). Lstat, never Stat: a planted SYMLINK at exchange/ is
// refused as not-a-directory (the replaceSymlink discipline — checks
// must describe the LINK, not its target).
func ensureExchangeDir(pvcRoot string) error {
	dir := filepath.Join(pvcRoot, "exchange")
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		//nolint:gosec // G301: 0770 is the dual-uid contract (see above); parent is the PVC root this command owns
		if err := os.Mkdir(dir, 0o770); err != nil {
			return err
		}
		//nolint:gosec // G302/G306: directory (not file) — exact-mode; umask-proofing the group-write bit
		return os.Chmod(dir, 0o770)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s exists and is not a directory (planted symlinks are refused, never followed)", dir)
	}
	if mode := info.Mode(); mode.Perm() != 0o770 || mode&os.ModeSticky != 0 {
		// Off-contract mode (missing group bits, world-writable, or
		// sticky): repair. We may not own the dir (the other mode's
		// init — or the kubelet fallback — created it); a failure
		// here is loud by design.
		//nolint:gosec // G302: directory (not file) — the dual-uid contract mode
		if err := os.Chmod(dir, 0o770); err != nil {
			return fmt.Errorf("repair mode (currently %v): %w", mode.Perm(), err)
		}
	}
	return nil
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
	// Epic 69 US-69.2 — the platform/ PVC subPath (sessionstate seq cursor
	// + future ledger). Ownership follows the RUNNING uid: single-container
	// agentd is uid 1000, so the default platform-init (uid 1000) creates
	// it; in sidecar mode platform-init skips it and a uid-2000 init
	// (init-fs --platform-subpath=only) creates it owned by the sidecar.
	// Mode 0750, files inside 0640: a directory needs the x bit to
	// traverse — design 0055's "0640" applies to the payload files.
	platformSubpath := fs.String("platform-subpath", "create", "platform/ subPath handling: create | skip | only")
	// Design 0070 §2.4a — the exchange/ PVC subPath (the board payload
	// exchange dir). Same create/skip discipline as platform/: the
	// RUNNING uid creates it (single-container platform-init uid 1000;
	// sidecar-mode platform-dirs uid 2000 — ownership follows the
	// writer), except the dir is DUAL-RW: mode 0770 group rwx under the
	// pod's shared gid 1000, so the runtime container's write tool (uid
	// 1000) and the sidecar's cleanup hook (uid 2000, gid 1000) BOTH
	// write it. See ensureExchangeDir for the mode rationale.
	exchangeSubpath := fs.String("exchange-subpath", "create", "exchange/ subPath handling: create | skip | only")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Review r1 (flag-value asymmetry): an unknown --exchange-subpath
	// value must be a LOUD flag error (exit 2), never a silent skip —
	// a typo'd value deferring creation falls straight into the
	// root-owned-0755 kubelet-fallback failure class. (The older
	// --platform-subpath keeps its historical behavior — pre-existing
	// surface, out of scope here.)
	switch *exchangeSubpath {
	case "create", "skip", "only":
	default:
		_, _ = fmt.Fprintf(stderr, "init-fs: --exchange-subpath must be create|skip|only (got %q)\n", *exchangeSubpath)
		return 2
	}

	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "init-fs: "+format+"\n", a...)
		return initFSExitOperational
	}

	if *platformSubpath == "only" {
		if err := os.MkdirAll(filepath.Join(*pvcRoot, "platform"), 0o750); err != nil {
			return fail("platform subPath: %v", err)
		}
		if *exchangeSubpath != "skip" {
			if err := ensureExchangeDir(*pvcRoot); err != nil {
				return fail("exchange subPath: %v", err)
			}
		}
		_, _ = fmt.Fprintln(stderr, "init-fs: platform-only ok")
		return 0
	}

	// 1. PVC subPath roots (absorbs workspace-dirs).
	for _, d := range []string{"workspace", "home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(*pvcRoot, d), 0o750); err != nil {
			return fail("subPath root %s: %v", d, err)
		}
	}
	if *platformSubpath == "create" {
		if err := os.MkdirAll(filepath.Join(*pvcRoot, "platform"), 0o750); err != nil {
			return fail("platform subPath: %v", err)
		}
	}
	if *exchangeSubpath == "create" || *exchangeSubpath == "only" {
		if err := ensureExchangeDir(*pvcRoot); err != nil {
			return fail("exchange subPath: %v", err)
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

	// 7. Gate-script patterns doc (2026-10-29 owner ruling): the
	// canonical #1812 worked patterns, embedded in this binary,
	// materialized to the pod-shared tmpfs at
	// $runtimeDir/docs/gate-script-patterns.md — the stable pod path
	// trigger_create's description points agents at. Best-effort by
	// design (advisory content, the bytes live in the binary): a failed
	// copy logs loudly but never blocks boot — unlike the credentials
	// above, a missing doc degrades to the tool description alone.
	if err := ensureGateScriptPatternsDoc(filepath.Join(*runtimeDir, "docs")); err != nil {
		_, _ = fmt.Fprintf(stderr, "init-fs: gate-docs copy failed (non-fatal): %v\n", err)
	}

	_, _ = fmt.Fprintln(stderr, "init-fs: ok")
	return 0
}
