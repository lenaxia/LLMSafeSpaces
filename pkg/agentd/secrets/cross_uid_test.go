// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// cross_uid_test.go — design 0051 US-4b (owner ruling 2026-08-21):
// in sidecar mode the RELOAD-path materializer runs as uid 2000 while
// rt/* stays tool-consumed by uid-1000 processes (US-35.7 class C —
// git/ssh/user tools read ~/.secrets/*, ~/.ssh/*, ~/.git-credentials
// via the shared gid 1000). CrossUID arms the group bits on exactly
// those stores:
//
//   - dirs  0770 (group rwx — traversal + the NEXT reset()'s unlink)
//   - files 0640 (group r — tool reads; writes stay owner-only)
//
// Everything else keeps its mode: secrets-env (0600 — sidecar-only
// volume, no uid-1000 reader exists) and the agent config (already
// AgentConfigWriteMode). Single-container mode (CrossUID=false) must
// stay byte-identical: 0700/0600, the pre-US-4b shapes.
//
// These tests run against the REAL filesystem (not fakeFS) so the mode
// assertions stat actual inodes.

import (
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
	}
	return &Materializer{FS: RealFS(), Paths: paths, CrossUID: true}, paths
}

func requirePerm(t *testing.T, path string, want os.FileMode, msg string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, path)
	require.Equal(t, want, info.Mode().Perm(), "%s: %s", path, msg)
}

// TestCrossUID_ToolConsumedFilesGroupReadable: after a cross-uid
// materialization, every rt/* store a uid-1000 tool consumes is
// group-readable (gid 1000), and its directories are group-traversable.
func TestCrossUID_ToolConsumedFilesGroupReadable(t *testing.T) {
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

	requirePerm(t, paths.SecretsBaseDir, 0o770, "secret-file root must be group-traversable (nested binds)")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "app"), 0o770, "nested mount dirs must be group-traversable")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "app", "config.env"), 0o640, "secret-file content must be tool-readable via gid 1000")
	requirePerm(t, paths.SSHDir, 0o770, "ssh dir must be group-traversable")
	requirePerm(t, filepath.Join(paths.SSHDir, "id_ed25519_deploy"), 0o640, "ssh keys must be tool-readable via gid 1000")
	requirePerm(t, filepath.Join(paths.SSHDir, "config"), 0o640, "ssh config must be tool-readable via gid 1000")
	requirePerm(t, paths.GitCredsPath, 0o640, "git-credentials must be tool-readable via gid 1000")
}

// TestCrossUID_SecretsEnvGroupReadableForBootHandoff: secrets-env is
// written by the INIT container (uid 1000) and read by the SIDECAR's
// boot push (uid 2000) — the cross-uid READ bridge is the shared gid.
// The uid-1000 EXCLUSION is the mount topology (the agentd-secrets
// volume is never mounted in the workspace container), not the mode.
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

// TestCrossUID_ReMaterializeSucceedsAcrossOwnership: the ruling's core
// scenario — dirs/files first owned by "uid 1000" (the init container's
// boot materialization), then re-materialized by the "uid 2000" sidecar.
// Same-os-user in tests, but the MODES are what carry the property in
// production: group bits on every rt/* store the rewrite touches.
func TestCrossUID_ReMaterializeSucceedsAcrossOwnership(t *testing.T) {
	m, paths := crossUIDFixture(t)

	// First generation (init container, CrossUID=false: uid-1000-only).
	boot := &Materializer{FS: m.FS, Paths: m.Paths}
	_, err := boot.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "gen1",
			Metadata: map[string]string{"mount_path": "config.env"}},
	})
	require.NoError(t, err)

	// Second generation (sidecar reload, cross-uid modes): reset() must
	// unlink the uid-1000-owned tree and rebuild it group-readable.
	_, err = m.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "gen2",
			Metadata: map[string]string{"mount_path": "config.env"}},
	})
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(paths.SecretsBaseDir, "config.env"))
	require.NoError(t, err)
	require.Equal(t, "gen2", string(data), "reload must fully replace boot materialization")
}

// TestCrossUID_DisabledKeepsOwnerOnlyModes: the single-container
// regression pin — CrossUID=false keeps the exact pre-US-4b modes.
func TestCrossUID_DisabledKeepsOwnerOnlyModes(t *testing.T) {
	m, paths := crossUIDFixture(t)
	m.CrossUID = false

	_, err := m.Materialize([]Secret{
		{Type: "secret-file", Name: "cfg", Plaintext: "x",
			Metadata: map[string]string{"mount_path": "config.env"}},
		{Type: "ssh-key", Name: "k", Plaintext: "x"},
		{Type: "git-credential", Name: "g", Plaintext: "tok"},
	})
	require.NoError(t, err)

	requirePerm(t, paths.SecretsBaseDir, 0o700, "single-container: owner-only dirs")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "config.env"), 0o600, "single-container: owner-only files")
	requirePerm(t, filepath.Join(paths.SSHDir, "id_ed25519_k"), 0o600, "single-container: owner-only ssh keys")
	requirePerm(t, paths.GitCredsPath, 0o600, "single-container: owner-only git-credentials")
}
