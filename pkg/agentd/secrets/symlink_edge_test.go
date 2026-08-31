// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Level 4: Adversarial Edge Cases
//
// Edge cases that are unlikely but would cause silent credential exposure
// or pod boot failures if not handled correctly.
// =============================================================================

// TestReset_Idempotent verifies that calling reset() twice doesn't error
// or corrupt state. This happens on rapid credential reloads.
func TestReset_Idempotent(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	require.NoError(t, m.reset())
	require.NoError(t, m.reset(),
		"second reset() must be a no-op, not an error")

	// Directories must still exist.
	_, err := os.Stat(sim.paths.SSHDir)
	require.NoError(t, err)
	_, err = os.Stat(sim.paths.SecretsBaseDir)
	require.NoError(t, err)

	// PVC symlinks must survive both resets.
	assertPVCPathsAreSymlinks(t, sim)
}

// TestReset_TmpfsNotYetCreated (R2b): reset() succeeds when nothing exists
// yet — it creates only its own staging scratch; the consumed dirs are
// created later by the delivery side, not by reset.
func TestReset_TmpfsNotYetCreated(t *testing.T) {
	tmpfsDir := t.TempDir()
	paths := Paths{
		Home:            filepath.Join(t.TempDir(), "home"),
		SecretsBaseDir:  filepath.Join(tmpfsDir, "rt", "secrets"),
		SSHDir:          filepath.Join(tmpfsDir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(tmpfsDir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(tmpfsDir, "secrets-env"),
		GitCredsPath:    filepath.Join(tmpfsDir, "rt", "git-credentials"),
		StagingDir:      filepath.Join(tmpfsDir, "staged-secret-files"),
	}
	m := &Materializer{FS: RealFS(), Paths: paths}
	require.NoError(t, m.reset())

	_, err := os.Stat(paths.StagingDir + ".tmp")
	require.NoError(t, err, "reset creates its own staging scratch dir")
	_, err = os.Stat(paths.SSHDir)
	assert.True(t, os.IsNotExist(err), "consumed dirs are delivery-side state — reset never creates them")
}

// TestMaterialize_EmptyBatch_LeavesEmptyManifest (R2b): the unbind-all
// path publishes an EMPTY staging manifest (absence is the delete — the
// delivery side's ledger-scoped reset removes the delivered files; that
// half is pinned in cmd/workspace-agentd). No consumed file is written or
// removed by the materializer either way.
func TestMaterialize_EmptyBatch_LeavesEmptyManifest(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	_, err := m.Materialize([]Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "secret-key-data",
			Metadata: map[string]string{"key_type": "ed25519"}},
	})
	require.NoError(t, err)

	result, err := m.Materialize([]Secret{})
	require.NoError(t, err)
	require.False(t, result.HasFailures())

	manifest := filepath.Join(sim.paths.StagingDir, "manifest.json")
	data, err := os.ReadFile(manifest)
	require.NoError(t, err, "empty batch still publishes a manifest")
	assert.Equal(t, "[]", string(data), "unbind-all → empty manifest (revocation is absence)")

	assertPVCPathsAreSymlinks(t, sim)
}

// TestGitCredentials_StagedWhileTargetDangling (R2b): the materializer
// stages git-credentials while the $HOME symlink stays dangling — the
// target file is created by the uid-1000 delivery side (pinned in
// cmd/workspace-agentd), which resolves the symlink by writing the
// target it points at.
func TestGitCredentials_StagedWhileTargetDangling(t *testing.T) {
	sim := newSymlinkFarmSim(t)

	gitCredsSymlink := filepath.Join(sim.paths.Home, ".git-credentials")
	target, err := os.Readlink(gitCredsSymlink)
	require.NoError(t, err)
	_, err = os.Stat(target)
	require.True(t, os.IsNotExist(err), "target must dangle before delivery")

	m := &Materializer{FS: RealFS(), Paths: sim.paths}
	_, err = m.Materialize([]Secret{
		{Type: "git-credential", Name: "github", Plaintext: "test_token_abc123",
			Metadata: map[string]string{"host": "github.com", "protocol": "https"}},
	})
	require.NoError(t, err)

	staged, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "git-credentials"))
	require.NoError(t, err)
	assert.Contains(t, string(staged), "test_token_abc123")

	_, err = os.Stat(target)
	assert.True(t, os.IsNotExist(err), "materializer never resolves the symlink — delivery does")
}

// TestAgentConfig_PathIsTmpfsNotPVC verifies agent-config.json is written
// to the tmpfs path and does not exist on the PVC. Unlike Group C paths,
// agent-config.json is a direct tmpfs path (not a symlink) — so this tests
// the direct-redirect mechanism (US-35.7.2).
func TestAgentConfig_PathIsTmpfsNotPVC(t *testing.T) {
	sim := newSymlinkFarmSim(t)

	// AgentConfigPath must point to tmpfs, not PVC.
	assert.True(t, strings.HasPrefix(sim.paths.AgentConfigPath, sim.tmpfsDir),
		"AgentConfigPath must be under tmpfs dir, got %s", sim.paths.AgentConfigPath)

	// Write a file directly at the tmpfs path (simulates FlushProviders).
	require.NoError(t, os.MkdirAll(sim.tmpfsDir, 0o755))
	require.NoError(t, os.WriteFile(sim.paths.AgentConfigPath, []byte(`{"apiKey":"sk-test"}`), 0o600))

	// Walk the PVC: no file must contain agent-config content.
	err := filepath.Walk(sim.pvcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		content, _ := os.ReadFile(path)
		assert.NotContains(t, string(content), "sk-test",
			"PVC file %s must not contain agent-config content", path)
		return nil
	})
	require.NoError(t, err)
}

// TestReset_DoubleMaterializeSimulatesReloadCycle (R2b): reload cycles
// replace the STAGED generation wholesale; consumed-side generation
// replacement is the delivery ledger's job (cmd tests pin it).
func TestReset_DoubleMaterializeSimulatesReloadCycle(t *testing.T) {
	sim := newSymlinkFarmSim(t)
	m := &Materializer{FS: RealFS(), Paths: sim.paths}

	_, err := m.Materialize([]Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "KEY_V1",
			Metadata: map[string]string{"key_type": "ed25519"}},
	})
	require.NoError(t, err)

	_, err = m.Materialize([]Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "KEY_V2",
			Metadata: map[string]string{"key_type": "ed25519"}},
	})
	require.NoError(t, err)

	key, err := os.ReadFile(filepath.Join(sim.paths.StagingDir, "ssh", "id_ed25519_github"))
	require.NoError(t, err)
	assert.Contains(t, string(key), "KEY_V2")
	assert.NotContains(t, string(key), "KEY_V1", "old staged generation must not survive reload")

	assertPVCPathsAreSymlinks(t, sim)
}
