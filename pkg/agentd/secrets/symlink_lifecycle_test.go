// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Level 3: Symlink Lifecycle with Real Filesystem
//
// These tests use RealFS() (real os.* calls) with t.TempDir() to simulate
// the PVC/tmpfs split. The fake filesystem used in secrets_test.go is an
// in-memory map — it CANNOT exercise symlink behavior because symlinks are
// kernel-level inodes, not map entries.
//
// Test setup simulates the production volume layout:
//
//	pvc/             ← simulates the PVC (persists across "pod death")
//	├── home/
//	│   ├── .ssh → tmpfs/rt/ssh          (symlink)
//	│   ├── .secrets → tmpfs/rt/secrets  (symlink)
//	│   └── .git-credentials → tmpfs/rt/git-credentials  (symlink)
//	└── workspace/.local/opencode/
//	    └── auth.json → tmpfs/rt/auth.json  (symlink)
//	tmpfs/           ← simulates sandbox-runtime (RAM, wiped on pod death)
//	├── agent-config.json
//	├── secrets-env
//	└── rt/
//	    ├── ssh/
//	    ├── secrets/
//	    ├── git-credentials
//	    └── auth.json
// =============================================================================

// symlinkFarmSim sets up the PVC/tmpfs directory structure that the init
// container would create. Returns pvcDir (persists) and tmpfsDir (ephemeral).
type symlinkFarmSim struct {
	pvcDir   string
	tmpfsDir string
	paths    Paths
}

func newSymlinkFarmSim(t *testing.T) symlinkFarmSim {
	t.Helper()
	pvcDir := t.TempDir()
	tmpfsDir := t.TempDir()

	// Create the tmpfs target structure (what the init container creates).
	rtDir := filepath.Join(tmpfsDir, "rt")
	require.NoError(t, os.MkdirAll(filepath.Join(rtDir, "ssh"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(rtDir, "secrets"), 0o700))

	// Create PVC-side directory structure.
	homeDir := filepath.Join(pvcDir, "home")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))
	opencodeDir := filepath.Join(pvcDir, "workspace", ".local", "opencode")
	require.NoError(t, os.MkdirAll(opencodeDir, 0o755))

	// Create symlinks (what the init container's ln -s does).
	require.NoError(t, os.Symlink(filepath.Join(rtDir, "ssh"), filepath.Join(homeDir, ".ssh")))
	require.NoError(t, os.Symlink(filepath.Join(rtDir, "secrets"), filepath.Join(homeDir, ".secrets")))
	require.NoError(t, os.Symlink(filepath.Join(rtDir, "git-credentials"), filepath.Join(homeDir, ".git-credentials")))
	require.NoError(t, os.Symlink(filepath.Join(rtDir, "auth.json"), filepath.Join(opencodeDir, "auth.json")))

	paths := Paths{
		Home:            homeDir,
		SecretsBaseDir:  filepath.Join(rtDir, "secrets"),
		SSHDir:          filepath.Join(rtDir, "ssh"),
		AgentConfigPath: filepath.Join(tmpfsDir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(tmpfsDir, "secrets-env"),
		GitCredsPath:    filepath.Join(rtDir, "git-credentials"),
		StagingDir:      filepath.Join(tmpfsDir, "staged-secret-files"),
	}

	return symlinkFarmSim{pvcDir: pvcDir, tmpfsDir: tmpfsDir, paths: paths}
}

// assertPVCPathsAreSymlinks verifies the PVC-side paths are symlinks (not
// real files/directories). This is the core US-35.7 invariant: the PVC must
// contain only symlink inodes, never plaintext credential bytes.
func assertPVCPathsAreSymlinks(t *testing.T, sim symlinkFarmSim) {
	t.Helper()
	homeDir := sim.paths.Home
	pvcLinks := []string{
		filepath.Join(homeDir, ".ssh"),
		filepath.Join(homeDir, ".secrets"),
		filepath.Join(homeDir, ".git-credentials"),
		filepath.Join(sim.pvcDir, "workspace", ".local", "opencode", "auth.json"),
	}
	for _, link := range pvcLinks {
		fi, err := os.Lstat(link)
		require.NoError(t, err, "PVC path %s must exist", link)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0,
			"PVC path %s must be a symlink, got mode %v", link, fi.Mode())
	}
}

// --- Tests ------------------------------------------------------------------

// TestReset_PreservesPVCSymlinks is the #1 regression guard for US-35.7.
// reset() must operate on tmpfs targets only — if it resolves Paths to PVC
// symlink paths, RemoveAll destroys the symlink, then MkdirAll creates a
// real directory on the PVC. The next Materialize writes plaintext there.
func TestReset_PreservesPVCSymlinks(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	// Pre-seed tmpfs targets with some content (simulates prior materialize).
	require.NoError(t, os.WriteFile(filepath.Join(sim.paths.SSHDir, "id_rsa"), []byte("plaintext-key"), 0o600))

	// Run reset — this is the operation that must NOT touch PVC symlinks.
	require.NoError(t, m.reset())

	// PVC-side paths must STILL be symlinks (not real dirs/files).
	assertPVCPathsAreSymlinks(t, sim)
}

// TestReset_NeverTouchesConsumedTmpfs (R2b, #1165): reset() must NOT
// remove consumed-dir content — uid-1000-owned user state (known_hosts,
// user keys) and the delivered credential files live there, and their
// lifecycle is owned by the delivery side's ledger. reset() clears only
// the staging scratch trees and the env/config files.
func TestReset_NeverTouchesConsumedTmpfs(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	userKey := filepath.Join(sim.paths.SSHDir, "known_hosts")
	require.NoError(t, os.WriteFile(userKey, []byte("github.com ssh-ed25519 AAAA"), 0o600))
	require.NoError(t, os.WriteFile(sim.paths.GitCredsPath, []byte("stale"), 0o600))

	require.NoError(t, m.reset())

	data, err := os.ReadFile(userKey)
	require.NoError(t, err, "user-owned known_hosts must survive reset")
	assert.Contains(t, string(data), "ssh-ed25519")
	_, err = os.Stat(sim.paths.GitCredsPath)
	require.NoError(t, err, "delivered files are delivery-side state; reset never removes them")

	// Staging scratch IS reset-scope.
	// (scratch lifecycle covered by TestR2B_ReloadReplacesStagingAtomically)
}

// TestMaterialize_StagesFileClass_NeverWritesConsumed (R2b): Materialize
// stages file-class bytes in the tmpfs staging tree; the consumed tmpfs
// targets are NOT written (the uid-1000 delivery side owns them), and the
// PVC keeps only symlink inodes.
func TestMaterialize_StagesFileClass_NeverWritesConsumed(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	secretList := []Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "fake-key",
			Metadata: map[string]string{"key_type": "ed25519"}},
		{Type: "git-credential", Name: "github", Plaintext: "ghp_test_token_12345",
			Metadata: map[string]string{"host": "github.com", "protocol": "https"}},
		{Type: "llm-provider", Name: "anthropic",
			Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-test"}`},
	}

	result, err := m.Materialize(secretList)
	require.NoError(t, err)
	require.False(t, result.HasFailures(), "all secrets should materialize")

	// Staged bytes exist under tmpfs staging.
	key, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "ssh", "id_ed25519_github"))
	require.NoError(t, err)
	assert.Contains(t, string(key), "fake-key")
	git, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "git-credentials"))
	require.NoError(t, err)
	assert.Contains(t, string(git), "ghp_test_token_12345")

	// Consumed targets untouched by the materializer.
	for _, p := range []string{
		filepath.Join(sim.paths.SSHDir, "id_ed25519_github"),
		sim.paths.GitCredsPath,
	} {
		_, err := os.Lstat(p)
		assert.True(t, os.IsNotExist(err), "%s must be delivery-side state only", p)
	}

	assertPVCPathsAreSymlinks(t, sim)
}

// TestSimulatedPodDeath_NoPlaintextOnPVC is the CORE SECURITY PROPERTY
// test. Staged and delivered plaintext live on tmpfs; after "pod death"
// the PVC contains only dangling symlinks — no credential bytes anywhere.
func TestSimulatedPodDeath_NoPlaintextOnPVC(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	_, err := m.Materialize([]Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "SECRET_SSH_KEY_BYTES",
			Metadata: map[string]string{"key_type": "ed25519"}},
		{Type: "git-credential", Name: "github", Plaintext: "SECRET_GHP_TOKEN_abc123",
			Metadata: map[string]string{"host": "github.com", "protocol": "https"}},
	})
	require.NoError(t, err)

	// Plaintext exists while the pod is alive — in the tmpfs staging tree.
	staged, _ := os.ReadFile(filepath.Join(sim.paths.StagingDir, "ssh", "id_ed25519_github"))
	require.Contains(t, string(staged), "SECRET_SSH_KEY_BYTES")

	require.NoError(t, os.RemoveAll(sim.tmpfsDir), "failed to simulate pod death")

	err = filepath.Walk(sim.pvcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		s := string(content)
		assert.NotContains(t, s, "SECRET_SSH_KEY_BYTES", "PVC file %s leaks ssh key", path)
		assert.NotContains(t, s, "SECRET_GHP_TOKEN", "PVC file %s leaks git token", path)
		return nil
	})
	require.NoError(t, err)

	pvcSymlink := filepath.Join(sim.paths.Home, ".ssh")
	target, err := os.Readlink(pvcSymlink)
	require.NoError(t, err)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr), "PVC symlink must dangle after pod death")
}
