// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// cross_uid_test.go — design 0057 R2b (#1165) supersedes the US-4b
// group-bit regime for file-class credentials: the tool-consumed rt/*
// stores are no longer written by the materializer at all (neither sidecar
// uid 2000 nor single-container), because no mode/ownership the writer
// confers survives the uid split for consumers with ownership-sensitive
// parsers (OpenSSH). File-class secrets are STAGED owner-only; the
// uid-1000 delivery side writes the consumed paths itself (tested in
// cmd/workspace-agentd).
//
// CrossUID's remaining scope: secrets-env — written by the init container
// (uid 1000), read by the sidecar's boot handoff (uid 2000) — where the
// reader is a platform process with no parser constraints and the shared
// gid remains the read bridge.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func crossUIDFixture(t *testing.T) (*Materializer, Paths) {
	t.Helper()
	dir := t.TempDir()
	paths := Paths{
		Home:            filepath.Join(dir, "home"),
		SecretsBaseDir:  filepath.Join(dir, "rt", "secrets"),
		SSHDir:          filepath.Join(dir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "rt", "git-credentials"),
		StagingDir:      filepath.Join(dir, "staged"),
	}
	return &Materializer{FS: RealFS(), Paths: paths, CrossUID: true}, paths
}

func requirePerm(t *testing.T, path string, want os.FileMode, msg string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, path)
	require.Equal(t, want, info.Mode().Perm(), "%s: %s", path, msg)
}

// readManifest loads the published staging manifest, sorted by target.
func readManifest(t *testing.T, stagingDir string) []StagedEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stagingDir, ManifestName))
	require.NoError(t, err)
	var entries []StagedEntry
	require.NoError(t, json.Unmarshal(data, &entries))
	return sortedEntries(entries)
}

// TestR2B_FileClassStagedNotWritten: after materialization the consumed
// paths DO NOT EXIST (no uid-2000-owned ~/.ssh, ~/.git-credentials,
// secrets base) — the files are staged owner-only, awaiting the uid-1000
// pull-side delivery. This is the ownership fix's construction half.
func TestR2B_FileClassStagedNotWritten(t *testing.T) {
	m, paths := crossUIDFixture(t)

	_, err := m.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "file-bytes",
			Metadata: map[string]string{"mount_path": "app/config.env"}},
		{Type: "ssh-key", Name: "deploy", Plaintext: "key-bytes",
			Metadata: map[string]string{"key_type": "ed25519", "host": "github.com"}},
		{Type: "git-credential", Name: "gh", Plaintext: "tok123",
			Metadata: map[string]string{"host": "github.com"}},
	})
	require.NoError(t, err)

	for _, p := range []string{
		paths.SSHDir,
		paths.GitCredsPath,
		paths.SecretsBaseDir,
	} {
		_, err := os.Lstat(p)
		require.True(t, os.IsNotExist(err),
			"%s must not be created by the materializer (R2b: the consumer's uid writes it)", p)
	}

	entries := readManifest(t, paths.StagingDir)
	require.Len(t, entries, 4, "key + assembled config + git creds + secret-file → 4 staged entries")
	targets := map[string]StagedEntry{}
	for _, e := range entries {
		targets[e.Target] = e
	}
	require.Contains(t, targets, filepath.Join(paths.SSHDir, "id_ed25519_deploy"))
	require.Contains(t, targets, filepath.Join(paths.SSHDir, "config"))
	require.Contains(t, targets, paths.GitCredsPath)
	require.Contains(t, targets, filepath.Join(paths.SecretsBaseDir, "app", "config.env"))
	require.Equal(t, ModeSSHPrivateKey, targets[filepath.Join(paths.SSHDir, "id_ed25519_deploy")].Mode)
	require.Equal(t, ModeSSHConfig, targets[filepath.Join(paths.SSHDir, "config")].Mode)
	require.Equal(t, ModeGitCredential, targets[paths.GitCredsPath].Mode)

	// Staged bytes carry the mode contract and the owner-only shape.
	stagedKey := filepath.Join(paths.StagingDir, "ssh", "id_ed25519_deploy")
	requirePerm(t, stagedKey, os.FileMode(ModeSSHPrivateKey), "staged key is owner-only — the delivery side owns the final mode")

	cfg, err := os.ReadFile(filepath.Join(paths.StagingDir, "ssh", "config"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), "Include config.d/*\n",
		"user fragments get a durable home reloads never touch")
	require.Contains(t, string(cfg), "Host github.com")

	// CrossUID must NOT widen staged or delivered file-class modes: the
	// 0640 group-read bridge is dead for these classes.
	requirePerm(t, filepath.Join(paths.StagingDir, "ssh", "config"), os.FileMode(ModeSSHConfig), "staged config owner-only regardless of CrossUID")
}

// TestR2B_SecretFileModeContract: the mode table + user override, and the
// rejection of deliverable-weakening overrides.
func TestR2B_SecretFileModeContract(t *testing.T) {
	mode, err := resolveSecretFileMode(nil)
	require.NoError(t, err)
	require.Equal(t, ModeSecretFile, mode)

	mode, err = resolveSecretFileMode(map[string]string{"mode": "0640"})
	require.NoError(t, err)
	require.Equal(t, 0o640, mode, "a user-nominated group-read mode is honored")

	_, err = resolveSecretFileMode(map[string]string{"mode": "0660"})
	require.Error(t, err, "group-write is never deliverable")
	_, err = resolveSecretFileMode(map[string]string{"mode": "0666"})
	require.Error(t, err, "world-write is never deliverable")
	_, err = resolveSecretFileMode(map[string]string{"mode": "99"})
	require.Error(t, err, "non-octal rejected")
}

// TestCrossUID_SecretsEnvGroupReadableForBootHandoff: secrets-env is the
// one remaining cross-uid FILE crossing the materializer writes (init
// uid 1000 writes, sidecar uid 2000 reads at boot) — the shared gid is
// the read bridge, and the reader is a platform process with no parser
// constraints.
func TestCrossUID_SecretsEnvGroupReadableForBootHandoff(t *testing.T) {
	m, paths := crossUIDFixture(t)

	_, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "token", Plaintext: "v",
			Metadata: map[string]string{"var_name": "GH_TOKEN"}},
	})
	require.NoError(t, err)
	requirePerm(t, paths.SecretsEnvPath, 0o640,
		"secrets-env must be sidecar-readable across the uid split (boot handoff)")
}

// TestDefault_SecretsEnvStaysOwnerOnly: single-container mode has no
// cross-uid reader — secrets-env stays 0600.
func TestDefault_SecretsEnvStaysOwnerOnly(t *testing.T) {
	m, paths := crossUIDFixture(t)
	m.CrossUID = false

	_, err := m.Materialize([]Secret{
		{Type: "env-secret", Name: "token", Plaintext: "v",
			Metadata: map[string]string{"var_name": "GH_TOKEN"}},
	})
	require.NoError(t, err)
	requirePerm(t, paths.SecretsEnvPath, 0o600, "single-container: owner-only secrets-env")
}

// TestR2B_ReloadReplacesStagingAtomically: two generations — the second
// publish fully replaces the first (revocation is absence), and no
// scratch trees survive a completed pass.
func TestR2B_ReloadReplacesStagingAtomically(t *testing.T) {
	m, paths := crossUIDFixture(t)

	_, err := m.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "gen1",
			Metadata: map[string]string{"mount_path": "config.env"}},
		{Type: "secret-file", Name: "gone", Plaintext: "temp",
			Metadata: map[string]string{"mount_path": "gone.env"}},
	})
	require.NoError(t, err)
	require.Len(t, readManifest(t, paths.StagingDir), 2)

	_, err = m.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "gen2",
			Metadata: map[string]string{"mount_path": "config.env"}},
	})
	require.NoError(t, err)

	entries := readManifest(t, paths.StagingDir)
	require.Len(t, entries, 1, "the revoked secret is absent from the new staging (absence is the delete)")

	data, err := os.ReadFile(filepath.Join(paths.StagingDir, "secrets", "config.env"))
	require.NoError(t, err)
	require.Equal(t, "gen2", string(data), "reload must fully replace boot materialization")

	for _, scratch := range []string{paths.StagingDir + ".tmp", paths.StagingDir + ".old"} {
		_, err := os.Lstat(scratch)
		require.True(t, os.IsNotExist(err), "no scratch tree survives a completed pass: %s", scratch)
	}
}

// TestR2B_UserStateSurvivesMaterializePasses: the #1165 reset-blast-radius
// regression pin — uid-1000-owned user files in the consumed dirs (e.g.
// ssh's own known_hosts) are NEVER touched by materialize/reload passes,
// because the materializer never writes or resets those dirs.
func TestR2B_UserStateSurvivesMaterializePasses(t *testing.T) {
	m, paths := crossUIDFixture(t)

	require.NoError(t, os.MkdirAll(paths.SSHDir, 0o700))
	knownHosts := filepath.Join(paths.SSHDir, "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, []byte("github.com ssh-ed25519 AAAA\n"), 0o600))

	_, err := m.Materialize([]Secret{
		{Type: "ssh-key", Name: "deploy", Plaintext: "key",
			Metadata: map[string]string{"host": "github.com"}},
	})
	require.NoError(t, err)
	_, err = m.Materialize([]Secret{
		{Type: "ssh-key", Name: "deploy", Plaintext: "key2",
			Metadata: map[string]string{"host": "github.com"}},
	})
	require.NoError(t, err, "second pass (reload) must succeed")

	data, err := os.ReadFile(knownHosts)
	require.NoError(t, err, "the user's known_hosts must survive every materialize pass")
	require.Equal(t, "github.com ssh-ed25519 AAAA\n", string(data))
}
