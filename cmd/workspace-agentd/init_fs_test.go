// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for the `init-fs` subcommand (design 0051 sidecar migration,
// step 1: platform boot logic leaves the runtime image).
//
// TDD: authored before the implementation. The subcommand absorbs the
// credential-setup heredoc in controller/internal/workspace/pod_builder.go
// and the workspace-dirs init, replacing bash with Go so it ships in the
// digest-pinned agentd image instead of the stale-able runtime base
// (incident 2026-08-25: factory-built bases carried a pre-#871 agentd).
//
// Contract under test:
//
//   - Creates the PVC subPath roots (workspace/, home/, tmp/) — absorbs
//     workspace-dirs.
//   - Builds the US-35.7 symlink farm (PVC paths → tmpfs targets),
//     HARDENED: a pre-planted symlink at a managed path is replaced
//     without following it; the victim target is never touched. A
//     pre-planted directory is removed (bash `rm -rf` parity — these
//     paths are platform-owned, reset() wipes them every reload).
//   - Creates /sandbox-runtime/rt/{ssh,secrets} mode 0700.
//   - Installs the workspace password 0600 (G21: never briefly
//     world-readable; missing source is FATAL — G46 class).
//   - Installs the admin token 0400 when present (#887 D5.1).
//   - Copies the free-models catalog when present (optional).
//   - Idempotent: a second run is a no-op that still exits 0.
//
// Exit codes: 0 success; 2 flag errors; 1 operational failure
// (missing/unreadable password source, unwritable filesystem).

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runInitFSSubcommand runs `workspace-agentd init-fs` against a
// fully-overridden path tree and returns (exit, stderr).
func runInitFSSubcommand(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"init-fs"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("subprocess failed: %v", err)
	}
	return exit, stderr.String()
}

// initFSTree builds the override flag set for a temp tree: pvc/, cfg/,
// rt/, pw/, fm/.
type initFSTree struct {
	pvc, cfg, rt, pw, fm string
}

func newInitFSTree(t *testing.T) initFSTree {
	t.Helper()
	dir := t.TempDir()
	tr := initFSTree{
		pvc: filepath.Join(dir, "pvc"),
		cfg: filepath.Join(dir, "cfg"),
		rt:  filepath.Join(dir, "rt"),
		pw:  filepath.Join(dir, "pw"),
		fm:  filepath.Join(dir, "fm"),
	}
	for _, d := range []string{tr.pvc, tr.cfg, tr.rt, tr.pw + "/password-src", tr.fm} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	return tr
}

func (tr initFSTree) args() []string {
	return []string{
		"--pvc-root", tr.pvc,
		"--cfg-dir", tr.cfg,
		"--runtime-dir", tr.rt,
		"--pw-source", tr.pw + "/password-src",
		"--freemodels", tr.fm + "/models.json",
	}
}

// managedSymlinks returns the four managed PVC paths and the tmpfs
// targets (derived from --runtime-dir) they must point at.
func (tr initFSTree) managedSymlinks() map[string]string {
	return map[string]string{
		filepath.Join(tr.pvc, "home", ".ssh"):                                 filepath.Join(tr.rt, "rt", "ssh"),
		filepath.Join(tr.pvc, "home", ".secrets"):                             filepath.Join(tr.rt, "rt", "secrets"),
		filepath.Join(tr.pvc, "home", ".git-credentials"):                     filepath.Join(tr.rt, "rt", "git-credentials"),
		filepath.Join(tr.pvc, "workspace", ".local", "opencode", "auth.json"): filepath.Join(tr.rt, "rt", "auth.json"),
	}
}

func (tr initFSTree) writePasswordSource(t *testing.T, password, adminToken string) {
	t.Helper()
	require.NoError(t, os.WriteFile(tr.pw+"/password-src/password", []byte(password), 0o644))
	if adminToken != "" {
		require.NoError(t, os.WriteFile(tr.pw+"/password-src/admin-token", []byte(adminToken), 0o644))
	}
}

// TestInitFS_HappyPath_FreshPVC pins the full first-boot layout: dirs,
// symlink farm, tmpfs dirs, password/admin-token installs, free-models
// copy — byte-for-byte what the bash heredoc produced.
func TestInitFS_HappyPath_FreshPVC(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "adm-tok\n")
	require.NoError(t, os.WriteFile(tr.fm+"/models.json", []byte(`{"models":[]}`), 0o644))

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	// PVC subPath roots (absorbs workspace-dirs).
	for _, d := range []string{"workspace", "home", "tmp"} {
		info, err := os.Stat(filepath.Join(tr.pvc, d))
		require.NoError(t, err, "subPath root %s must exist", d)
		require.True(t, info.IsDir())
	}

	// Symlink farm.
	for path, target := range tr.managedSymlinks() {
		got, err := os.Readlink(path)
		require.NoError(t, err, "managed path %s must be a symlink", path)
		assert.Equal(t, target, got)
	}

	// Tmpfs credential dirs, 0770 (US-4b cross-uid contract: the uid-2000
	// sidecar's boot phase/reset traverses and re-materializes here via
	// the pod's shared gid 1000 — kind L3 08:42 UTC 2026-08-26 caught
	// 0700 producing sidecar materialize EACCES).
	for _, d := range []string{filepath.Join(tr.rt, "rt"), filepath.Join(tr.rt, "rt", "ssh"), filepath.Join(tr.rt, "rt", "secrets")} {
		info, err := os.Stat(d)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o770), info.Mode().Perm(), "%s mode", d)
	}

	// Password: exact bytes, 0600, no group/other bits.
	pwBytes, err := os.ReadFile(tr.cfg + "/password")
	require.NoError(t, err)
	assert.Equal(t, "s3cret-pw\n", string(pwBytes))
	pwInfo, err := os.Stat(tr.cfg + "/password")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), pwInfo.Mode().Perm())

	// Admin token: 0400.
	tokInfo, err := os.Stat(tr.cfg + "/admin-token")
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o400), tokInfo.Mode().Perm())

	// Free-models catalog copied verbatim.
	fmBytes, err := os.ReadFile(tr.cfg + "/free-models.json")
	require.NoError(t, err)
	assert.Equal(t, `{"models":[]}`, string(fmBytes))
}

// TestInitFS_PrePlantedSymlink_ReplacedWithoutFollowing is the attack
// corpus entry the bash version left open: a user-planted symlink at a
// managed path (e.g. .ssh → somewhere interesting) must be REPLACED, and
// the victim target must survive untouched — removal operates on the
// link inode, never through it.
func TestInitFS_PrePlantedSymlink_ReplacedWithoutFollowing(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	victim := filepath.Join(tr.pvc, "victim") // outside the managed set
	require.NoError(t, os.MkdirAll(victim, 0o755))
	sentinel := filepath.Join(victim, "keep-me")
	require.NoError(t, os.WriteFile(sentinel, []byte("sentinel"), 0o644))

	sshPath := filepath.Join(tr.pvc, "home", ".ssh")
	require.NoError(t, os.MkdirAll(filepath.Dir(sshPath), 0o755))
	require.NoError(t, os.Symlink(victim, sshPath))

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)

	got, err := os.Readlink(sshPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(tr.rt, "rt", "ssh"), got, "planted link replaced with platform link")

	_, err = os.Stat(sentinel)
	require.NoError(t, err, "victim target content must survive the replacement")
}

// TestInitFS_PrePlantedDir_Replaced pins bash `rm -rf` parity: a real
// directory at a managed path (with content) is removed and replaced by
// the platform symlink. These paths are platform-owned; reset() wipes
// them on every credential reload — no user data is lost by contract.
func TestInitFS_PrePlantedDir_Replaced(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	secretsPath := filepath.Join(tr.pvc, "home", ".secrets")
	require.NoError(t, os.MkdirAll(filepath.Join(secretsPath, "junk"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secretsPath, "junk", "f"), []byte("x"), 0o644))

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)

	got, err := os.Readlink(secretsPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(tr.rt, "rt", "secrets"), got)
	_, err = os.Stat(filepath.Join(secretsPath, "junk"))
	require.True(t, os.IsNotExist(err), "pre-planted content must be gone")
}

// TestInitFS_DanglingSymlink_Replaced: a dangling link (target deleted)
// is still replaced cleanly.
func TestInitFS_DanglingSymlink_Replaced(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	gcPath := filepath.Join(tr.pvc, "home", ".git-credentials")
	require.NoError(t, os.MkdirAll(filepath.Dir(gcPath), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(tr.pvc, "gone"), gcPath))

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)
	got, err := os.Readlink(gcPath)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(tr.rt, "rt", "git-credentials"), got)
}

// TestInitFS_Idempotent: second run over an already-initialized tree
// exits 0 and leaves the layout stable (link targets unchanged,
// password re-installed). Sidecar-restart safety.
func TestInitFS_Idempotent(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw-one\n", "")

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)

	tr.writePasswordSource(t, "pw-two\n", "")
	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "second run must succeed; stderr=%q", stderr)

	for path, want := range tr.managedSymlinks() {
		got, err := os.Readlink(path)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	pw, err := os.ReadFile(tr.cfg + "/password")
	require.NoError(t, err)
	require.Equal(t, "pw-two\n", string(pw), "password re-installed on re-run")
}

// TestInitFS_MissingPasswordSource_Fails: G46 class — a workspace
// without its password file is silently non-functional; the init must
// fail loudly (Init:Error), not boot into a broken state.
func TestInitFS_MissingPasswordSource_Fails(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t) // no password written

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.NotZero(t, exit, "missing password source must fail the init")
	assert.Contains(t, stderr, "password")

	_, err := os.Stat(tr.cfg + "/password")
	require.True(t, os.IsNotExist(err), "no partial password file")
}

// TestInitFS_RTDirs_CrossUIDContract pins the exact access bit the
// 2026-08-26 kind incident was about: the uid-2000 sidecar's
// materialize must CREATE/UNLINK inside rt/secrets via the pod's shared
// gid 1000 — which requires the directory's GROUP-WRITE bit (0o020).
// Mode equality alone is satisfiable by shapes that still EACCES the
// sidecar (0700: no group bits; 0750 — what a bare MkdirAll(0770)
// yields under umask 022: group bits without write). Red against both
// the original bug and the umask variant; green only with the exact
// chmod-after-mkdir 0770.
func TestInitFS_RTDirs_CrossUIDContract(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)

	for _, d := range []string{filepath.Join(tr.rt, "rt"), filepath.Join(tr.rt, "rt", "ssh"), filepath.Join(tr.rt, "rt", "secrets")} {
		info, err := os.Stat(d)
		require.NoError(t, err)
		require.NotZero(t, info.Mode().Perm()&0o020,
			"%s must carry the group-WRITE bit (0o020) — without it the uid-2000 sidecar cannot create/unlink inside via shared gid 1000 (kind incident 2026-08-26)", d)
	}
}

// TestInitFS_ThenSidecarWrite_CrossUID_RealReproduction is the full
// incident reproduction when the test runs as root (local, kind nodes,
// privileged CI): init-fs runs as uid 1000/gid 1000, then a probe as
// uid 2000/gid 1000 (the sidecar's identity) opens
// rt/secrets/secrets.json for write through the directory path. With
// 0700/0750 the probe gets EACCES; with the 0770 contract it succeeds.
// Unprivileged runs skip — TestInitFS_RTDirs_CrossUIDContract is the
// always-on pin (PR #1025 review).
func TestInitFS_ThenSidecarWrite_CrossUID_RealReproduction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess test in -short mode")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to setuid subprocesses (the always-on contract pin covers unprivileged runs)")
	}
	if runtime.GOOS != "linux" {
		t.Skip("linux credentials are linux-only")
	}

	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	// 1. init-fs AS uid 1000/gid 1000 (the platform-init identity).
	cmd := exec.Command(bin, append([]string{"init-fs"}, tr.args()...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "init-fs as uid1000: %s", out)

	// 2. Write probe AS uid 2000/gid 1000 (the sidecar identity), via
	// this test binary re-invoked as the helper (standard TestMain-less
	// re-exec pattern — no production surface added).
	probe := filepath.Join(tr.rt, "rt", "secrets", "secrets.json")
	pcmd := exec.Command(os.Args[0], "-test.run=TestHelperSidecarWriteProbe", "--", probe)
	pcmd.Env = append(os.Environ(), "LSS_WRITE_PROBE=1")
	pcmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 2000, Gid: 1000}}
	pout, perr := pcmd.CombinedOutput()
	require.NoError(t, perr,
		"uid-2000 sidecar write probe failed (this is the 2026-08-26 kind incident): %s", pout)
}

// TestHelperSidecarWriteProbe is never run directly: it is re-exec'd by
// TestInitFS_ThenSidecarWrite_CrossUID_RealReproduction under the
// sidecar's credentials and writes the probe file (exit non-zero on
// EACCES — which is the incident's failure mode).
func TestHelperSidecarWriteProbe(t *testing.T) {
	if os.Getenv("LSS_WRITE_PROBE") != "1" {
		t.Skip("helper: only runs via re-exec")
	}
	probe := os.Args[len(os.Args)-1]
	if err := os.WriteFile(probe, []byte(`[]`), 0o640); err != nil {
		fmt.Fprintf(os.Stderr, "probe: %v\n", err)
		os.Exit(3)
	}
}

// TestInitFS_OptionalSourcesAbsent: no admin-token, no free-models
// catalog → exit 0, neither file created (relay disabled / legacy
// Secret during upsert convergence is normal).
func TestInitFS_OptionalSourcesAbsent(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit)

	for _, f := range []string{tr.cfg + "/admin-token", tr.cfg + "/free-models.json"} {
		_, err := os.Stat(f)
		assert.True(t, os.IsNotExist(err), "%s must not exist", f)
	}
}

// TestInitFS_UnwritablePVCRoot_Fails: operational failures surface as
// non-zero exit (visible Init:Error), never as a silent partial layout.
func TestInitFS_UnwritablePVCRoot_Fails(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "pw\n", "")

	blocker := filepath.Join(tr.pvc, "home")
	require.NoError(t, os.WriteFile(blocker, []byte("i am a file where a dir belongs"), 0o644))

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.NotZero(t, exit)
}
