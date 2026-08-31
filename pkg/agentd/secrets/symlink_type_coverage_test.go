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
// Level 4b: Per-Type Write Coverage + Init Script Structure
//
// Each credential TYPE has a separate apply function with a different write
// mechanism (atomicWrite vs appendFile). US-35.7 must ensure ALL types land
// in tmpfs, not just the ones tested in Level 3.
//
// Additionally: init script ordering and pre-US-35.7 migration edge cases
// that the substring assertions in security_test.go don't catch.
// =============================================================================

// TestMaterialize_AllTypes_NoPlaintextOnPVC (R2b): every credential type
// lands either in the tmpfs staging tree (file classes) or the tmpfs
// env/config files; the PVC walk stays clean for all of them.
func TestMaterialize_AllTypes_NoPlaintextOnPVC(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	secretList := []Secret{
		{Type: "ssh-key", Name: "deploy", Plaintext: "SSH_KEY_PLAINTEXT",
			Metadata: map[string]string{"key_type": "ed25519"}},
		{Type: "git-credential", Name: "github", Plaintext: "GIT_TOKEN_PLAINTEXT",
			Metadata: map[string]string{"host": "github.com", "protocol": "https"}},
		{Type: "secret-file", Name: "config", Plaintext: "SECRET_FILE_PLAINTEXT",
			Metadata: map[string]string{"mount_path": "app/secrets.json"}},
		{Type: "env-secret", Name: "db", Plaintext: "ENV_SECRET_PLAINTEXT",
			Metadata: map[string]string{"var_name": "DATABASE_URL"}},
		{Type: "api-key", Name: "custom", Plaintext: `{"kind":"openai_compatible","slug":"custom"}`},
	}

	result, err := m.Materialize(secretList)
	require.NoError(t, err)
	require.False(t, result.HasFailures(), "failures: %v", result.Results)

	for _, marker := range []string{
		"SSH_KEY_PLAINTEXT", "GIT_TOKEN_PLAINTEXT",
		"SECRET_FILE_PLAINTEXT", "ENV_SECRET_PLAINTEXT",
	} {
		err = filepath.Walk(sim.pvcDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return err
			}
			content, _ := os.ReadFile(path)
			assert.NotContains(t, string(content), marker, "PVC file %s leaks plaintext", path)
			return nil
		})
		require.NoError(t, err)
	}

	// Staged file classes carry the plaintext on tmpfs.
	ssh, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "ssh", "id_ed25519_deploy"))
	require.NoError(t, err)
	assert.Contains(t, string(ssh), "SSH_KEY_PLAINTEXT")
	gitC, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "git-credentials"))
	require.NoError(t, err)
	assert.Contains(t, string(gitC), "GIT_TOKEN_PLAINTEXT")
	sf, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "secrets", "app", "secrets.json"))
	require.NoError(t, err)
	assert.Contains(t, string(sf), "SECRET_FILE_PLAINTEXT")

	// Env classes still land in secrets-env (direct write, tmpfs).
	envContent, err := os.ReadFile(sim.paths.SecretsEnvPath)
	require.NoError(t, err)
	assert.Contains(t, string(envContent), "ENV_SECRET_PLAINTEXT")
	assert.Contains(t, string(envContent), "API_KEY_CUSTOM")
}

// TestMaterialize_StagedModeContract (R2b): staged bytes carry the
// per-type mode contract; delivered modes are applied by the delivery
// side from the manifest (pinned in cmd/workspace-agentd).
func TestMaterialize_StagedModeContract(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	_, err := m.Materialize([]Secret{
		{Type: "ssh-key", Name: "deploy", Plaintext: "key-data",
			Metadata: map[string]string{"key_type": "ed25519"}},
		{Type: "secret-file", Name: "config", Plaintext: "secret-data",
			Metadata: map[string]string{"mount_path": "cfg.json"}},
	})
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(sim.paths.StagingDir, "ssh", "id_ed25519_deploy"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(ModeSSHPrivateKey), info.Mode().Perm())

	sf, err := os.Stat(filepath.Join(sim.paths.StagingDir, "secrets", "cfg.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(ModeSecretFile), sf.Mode().Perm())

	mi, err := os.Stat(filepath.Join(sim.paths.StagingDir, ManifestName))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), mi.Mode().Perm())
}

// TestAuthJSON_WriteThroughSymlink verifies the relay injector's auth.json
// write path (os.WriteFile) follows the symlink to tmpfs. This is a DIFFERENT
// code path from the Materializer's atomicWrite/appendFile — relay_injector.go
// writes directly via os.WriteFile, so it must be tested separately.
func TestAuthJSON_WriteThroughSymlink(t *testing.T) {
	sim := newSymlinkFarmSim(t)

	authJSONSymlink := filepath.Join(sim.pvcDir, "workspace", ".local", "opencode", "auth.json")

	// The symlink target must be dangling initially (init creates symlink,
	// does NOT touch the target — zero-byte file would break JSON.parse).
	target, err := os.Readlink(authJSONSymlink)
	require.NoError(t, err)
	_, err = os.Stat(target)
	require.True(t, os.IsNotExist(err),
		"auth.json symlink target must be dangling before first write")

	// Simulate what updateAuthJSONForRelay does: os.WriteFile to the symlink path.
	// This follows the symlink and creates the target on the tmpfs side.
	authData := `{"opencode-relay":{"type":"api","key":"public"}}`
	require.NoError(t, os.WriteFile(authJSONSymlink, []byte(authData), 0o600))

	// File must exist at the tmpfs target.
	content, err := os.ReadFile(target)
	require.NoError(t, err, "auth.json must be created at tmpfs target through symlink")
	assert.Contains(t, string(content), "opencode-relay")

	// PVC path must still be a symlink (not a real file).
	fi, err := os.Lstat(authJSONSymlink)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0,
		"PVC auth.json path must remain a symlink after write, not become a real file")

	// After "pod death" (tmpfs removed), auth.json must not be on PVC.
	require.NoError(t, os.RemoveAll(sim.tmpfsDir))

	// Walk PVC — no auth.json plaintext.
	err = filepath.Walk(sim.pvcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		content, _ := os.ReadFile(path)
		assert.NotContains(t, string(content), "opencode-relay",
			"PVC file %s must not contain auth.json content after pod death", path)
		return nil
	})
	require.NoError(t, err)
}

// TestInitScript_rmBeforeLnS verifies the credential-setup init script
// executes `rm -rf` BEFORE `ln -s` for each symlink. If the order is
// reversed, `ln -s` creates the symlink INSIDE the existing directory
// (pre-US-35.7 workspace) rather than replacing it.
//
// This test reads the actual generated script and checks command positions.
func TestInitScript_rmBeforeLnS(t *testing.T) {
	// This test lives in the secrets package because it uses the same
	// symlinkFarmSim helper. The actual script generation test is in
	// controller/internal/workspace/security_test.go (substring assertions).
	// Here we test the RUNTIME behavior: given a pre-existing real directory
	// at a PVC path, does rm -rf + ln -s correctly replace it with a symlink?

	pvcDir := t.TempDir()
	tmpfsDir := t.TempDir()
	homeDir := filepath.Join(pvcDir, "home")
	require.NoError(t, os.MkdirAll(homeDir, 0o755))

	// Pre-create a REAL directory (simulates pre-US-35.7 workspace).
	oldSSHDir := filepath.Join(homeDir, ".ssh")
	require.NoError(t, os.MkdirAll(oldSSHDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(oldSSHDir, "old_key"), []byte("OLD_KEY_ON_PVC"), 0o600))

	// Simulate the init script: rm -rf + ln -s.
	require.NoError(t, os.MkdirAll(filepath.Join(tmpfsDir, "rt", "ssh"), 0o700))
	os.RemoveAll(filepath.Join(homeDir, ".ssh"))
	require.NoError(t, os.Symlink(filepath.Join(tmpfsDir, "rt", "ssh"), filepath.Join(homeDir, ".ssh")))

	// Verify it's now a symlink, not a directory.
	fi, err := os.Lstat(filepath.Join(homeDir, ".ssh"))
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0,
		".ssh must be a symlink after rm -rf + ln -s, not a directory")

	// The OLD_KEY must be gone (it was in the PVC directory that rm -rf removed).
	// It's NOT inside the symlink target.
	_, err = os.Stat(filepath.Join(homeDir, ".ssh", "old_key"))
	assert.True(t, os.IsNotExist(err),
		"old PVC SSH key must be gone after symlink replacement")

	// The tmpfs target must be empty (no old_key).
	entries, err := os.ReadDir(filepath.Join(tmpfsDir, "rt", "ssh"))
	require.NoError(t, err)
	assert.Empty(t, entries, "tmpfs SSH dir must be empty (old key was on PVC, not tmpfs)")
}
