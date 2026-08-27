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

// TestReset_CleansTmpfsTargets verifies reset() actually wipes the tmpfs
// content (not just leaves it alone). Old credentials from a prior reload
// must not survive into the next cycle.
func TestReset_CleansTmpfsTargets(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	// Pre-seed tmpfs with stale content.
	staleKey := filepath.Join(sim.paths.SSHDir, "id_rsa_old")
	require.NoError(t, os.WriteFile(staleKey, []byte("stale-key"), 0o600))
	staleSecret := filepath.Join(sim.paths.SecretsBaseDir, "old-secret.txt")
	require.NoError(t, os.WriteFile(staleSecret, []byte("stale-secret"), 0o600))

	require.NoError(t, m.reset())

	// Stale files must be gone.
	_, err := os.Stat(staleKey)
	assert.True(t, os.IsNotExist(err), "stale SSH key must be removed by reset()")
	_, err = os.Stat(staleSecret)
	assert.True(t, os.IsNotExist(err), "stale secret file must be removed by reset()")

	// Directories must exist (recreated by MkdirAll after RemoveAll).
	fi, err := os.Stat(sim.paths.SSHDir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "SSH dir must be recreated as directory after reset()")
	fi, err = os.Stat(sim.paths.SecretsBaseDir)
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "SecretsBaseDir must be recreated after reset()")
}

// TestMaterialize_WritesThroughSymlinkToTmpfs verifies that a Materialize
// call writes credential bytes to the tmpfs targets, not to the PVC paths.
// This is the end-to-end write-path test.
func TestMaterialize_WritesThroughSymlinkToTmpfs(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	secretList := []Secret{
		{
			Type:      "ssh-key",
			Name:      "github",
			Plaintext: "-----BEGIN OPENSSH PRIVATE KEY-----\nfake-key\n-----END OPENSSH PRIVATE KEY-----",
			Metadata: map[string]string{
				"key_type": "ed25519",
			},
		},
		{
			Type:      "git-credential",
			Name:      "github",
			Plaintext: "ghp_test_token_12345",
			Metadata:  map[string]string{"host": "github.com", "protocol": "https"},
		},
		{
			Type:      "llm-provider",
			Name:      "anthropic",
			Plaintext: `{"kind":"anthropic","slug":"anthropic","apiKey":"sk-ant-test"}`,
		},
	}

	result, err := m.Materialize(secretList)
	require.NoError(t, err)
	require.False(t, result.HasFailures(), "all secrets should materialize")

	// Verify SSH key landed in tmpfs (through the symlink).
	sshKey := filepath.Join(sim.paths.SSHDir, "id_ed25519_github")
	content, err := os.ReadFile(sshKey)
	require.NoError(t, err, "SSH key must be written to tmpfs target")
	assert.Contains(t, string(content), "fake-key")

	// Verify git credential landed in tmpfs.
	gitCreds := sim.paths.GitCredsPath
	gitContent, err := os.ReadFile(gitCreds)
	require.NoError(t, err, "git credentials must be written to tmpfs target")
	assert.Contains(t, string(gitContent), "ghp_test_token_12345")

	// PVC-side paths must still be symlinks (not real files with plaintext).
	assertPVCPathsAreSymlinks(t, sim)

	// Read through the symlink to verify it resolves to the same tmpfs content.
	pvcSSHKey := filepath.Join(sim.paths.Home, ".ssh", "id_ed25519_github")
	contentViaSymlink, err := os.ReadFile(pvcSSHKey)
	require.NoError(t, err, "reading through PVC symlink must work")
	assert.Equal(t, string(content), string(contentViaSymlink),
		"symlink must resolve to the same tmpfs content")

	// PVC-side paths must still be symlinks (not real files with plaintext).
	assertPVCPathsAreSymlinks(t, sim)
}

// TestSimulatedPodDeath_NoPlaintextOnPVC is the CORE SECURITY PROPERTY test.
// After "pod death" (tmpfs removed), the PVC must contain only dangling
// symlinks — no plaintext credential bytes recoverable.
func TestSimulatedPodDeath_NoPlaintextOnPVC(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	// Materialize credentials (writes plaintext to tmpfs).
	secretList := []Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "SECRET_SSH_KEY_BYTES",
			Metadata: map[string]string{"key_type": "ed25519"}},
		{Type: "git-credential", Name: "github", Plaintext: "SECRET_GHP_TOKEN_abc123",
			Metadata: map[string]string{"host": "github.com", "protocol": "https"}},
	}
	_, err := m.Materialize(secretList)
	require.NoError(t, err)

	// Verify plaintext exists while pod is alive (in tmpfs).
	liveContent, _ := os.ReadFile(filepath.Join(sim.paths.SSHDir, "id_ed25519_github"))
	require.Contains(t, string(liveContent), "SECRET_SSH_KEY_BYTES",
		"plaintext must exist in tmpfs while pod is alive")

	// SIMULATE POD DEATH: remove the entire tmpfs directory.
	// This is what happens when the pod's cgroup is destroyed — the kernel
	// unmounts and discards all Memory-backed emptyDir content.
	require.NoError(t, os.RemoveAll(sim.tmpfsDir),
		"failed to simulate pod death (remove tmpfs)")

	// WALK THE PVC: assert no file contains plaintext credential bytes.
	err = filepath.Walk(sim.pvcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		// Skip symlink inodes themselves (they contain only path strings, not
		// credential data). We care about regular files with plaintext content.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // unreadable, skip
		}
		s := string(content)
		assert.NotContains(t, s, "SECRET_SSH_KEY_BYTES",
			"PVC file %s must not contain plaintext SSH key after pod death", path)
		assert.NotContains(t, s, "SECRET_GHP_TOKEN",
			"PVC file %s must not contain plaintext git token after pod death", path)
		return nil
	})
	require.NoError(t, err)

	// PVC-side symlinks must now be dangling (target removed).
	pvcSymlink := filepath.Join(sim.paths.Home, ".ssh")
	target, err := os.Readlink(pvcSymlink)
	require.NoError(t, err)
	_, statErr := os.Stat(target)
	assert.True(t, os.IsNotExist(statErr),
		"PVC symlink target must be dangling (tmpfs gone) after pod death")
}
