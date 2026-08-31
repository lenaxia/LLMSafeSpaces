// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// cross_uid_write_test.go — design 0051 sidecar migration step 1 (TDD:
// authored before the implementation).
//
// In sidecar mode the materializer runs as uid 2000, but the consumers
// of its credential outputs live in uid-1000 space:
//
//   - secrets-env → the supervisor's buildEnvFrom (managed_process.go)
//     and the entrypoint's `source` — reads at opencode spawn. A 0600
//     uid-2000 file EACCESes there and buildEnvFrom SILENTLY degrades
//     to the parent env: env-secrets vanish with no signal.
//   - rt/ssh/*, rt/secrets/*, git-credentials → the user's ssh / file
//     reads / git credential helper (all uid 1000).
//
// agent-config.json already solved this exact split with
// AgentConfigWriteMode=0640 + the pod's shared gid 1000 (the documented
// T2 exception). This profile extends the same mechanism to every
// credential output with a uid-1000 reader: CredentialWriteMode.
//
// Invariants pinned:
//
//   - Legacy default (zero value): every credential output 0600 —
//     byte-identical to pre-migration pods.
//   - CrossUID profile (CredentialWriteMode=0640): env/ssh/secret-file/
//     git-credentials outputs 0640; agent-config.json 0640 as today.
//   - The reload cache stays 0600 (sidecar-internal: written and read
//     by the same uid-2000 process).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func crossUIDBatch() []Secret {
	return []Secret{
		{Type: "env-secret", Name: "e", Metadata: map[string]string{"var_name": "MY_VAR"}, Plaintext: "v"},
		{Type: "api-key", Name: "k", Plaintext: `{"kind":"x","slug":"x"}`},
		{Type: "ssh-key", Name: "deploy", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "ssh-ed25519 AAAA"},
		{Type: "git-credential", Name: "gh", Metadata: map[string]string{"host": "github.com"}, Plaintext: "ghp_tok123"},
		{Type: "secret-file", Name: "cert", Metadata: map[string]string{"mount_path": "app/cert.pem"}, Plaintext: "CERTDATA"},
	}
}

func crossUIDPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Home:            dir,
		SecretsBaseDir:  filepath.Join(dir, "secrets"),
		SSHDir:          filepath.Join(dir, "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
		StagingDir:      filepath.Join(dir, "staged"),
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "expected output at %s", path)
	return info.Mode().Perm()
}

// TestCredentialWriteMode_LegacyDefault_0600: zero-value Paths must keep
// every credential output 0600 — legacy pods are byte-identical.
// R2b (#1165): file-class modes moved to the mode-contract table; the
// only materializer-written credential file left is secrets-env, and the
// only direct-write mode assertion left is its default owner-only shape.
func TestCredentialWriteMode_LegacyDefault_0600(t *testing.T) {
	p := crossUIDPaths(t)
	m := &Materializer{FS: RealFS(), Paths: p}
	_, err := m.Materialize(crossUIDBatch())
	require.NoError(t, err)

	assert.Equal(t, os.FileMode(0o600), modeOf(t, p.SecretsEnvPath), "secrets-env")
	for _, path := range []string{
		p.GitCredsPath,
		filepath.Join(p.SSHDir, "id_ed25519_deploy"),
		filepath.Join(p.SecretsBaseDir, "app", "cert.pem"),
	} {
		_, err := os.Lstat(path)
		assert.True(t, os.IsNotExist(err),
			"%s must not exist — file-class delivery is staged, not direct-written", path)
	}
}

// R2b (#1165): the 0640 group-read bridge is retired for file-class —
// CrossUID widens ONLY secrets-env now; staged file-class bytes stay
// owner-only regardless.
func TestCredentialWriteMode_CrossUID_0640(t *testing.T) {
	p := crossUIDPaths(t)
	m := &Materializer{FS: RealFS(), Paths: p, CrossUID: true}
	_, err := m.Materialize(crossUIDBatch())
	require.NoError(t, err)

	assert.Equal(t, os.FileMode(0o640), modeOf(t, p.SecretsEnvPath),
		"secrets-env must be 0640 for the sidecar boot handoff via gid 1000")
	for _, path := range []string{
		p.GitCredsPath,
		filepath.Join(p.SSHDir, "id_ed25519_deploy"),
		filepath.Join(p.SecretsBaseDir, "app", "cert.pem"),
	} {
		_, err := os.Lstat(path)
		assert.True(t, os.IsNotExist(err), "%s must not exist (staged delivery)", path)
	}
	manifest := filepath.Join(p.StagingDir, ManifestName)
	assert.Equal(t, os.FileMode(0o600), modeOf(t, manifest), "staged manifest owner-only")
}
