// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// cross_uid_boot_matrix_test.go — US-70.1 R3 (design 0057): the standing
// cross-uid credential matrix for the AGENTD-side of the boot path —
// the CROSS_UID_FILES machinery (materializer modes) plus the US-70.1
// spawn-pull crossings.
//
// Rule (epic #1158, normative): every credential/file crossing uids in
// the boot path is enumerated — writer uid, reader uid, mode, expected
// outcome. A new crossing is ADDED to the matrix, never fixed ad hoc.
// The pod-spec side (container credential wiring, volume mounts) lives
// in controller/internal/workspace/cross_uid_boot_matrix_test.go.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	secretpkg "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// crossUIDBootMatrixRow enumerates one boot-path crossing. Writer/reader
// uids refer to design 0051's split: init/sidecar materializes at uid
// 2000 (or platform-init uid 1000), the agent's tools read at uid 1000
// via the shared gid 1000, and uid-1000 code must never reach the
// sidecar-only stores (I9).
type crossUIDBootMatrixRow struct {
	artifact  string
	writerUID string
	readerUID string
	mode      string
	outcome   string
}

// The enumeration itself — the standing registry. File-mode rows are
// validated by the executable checks below (via the real materializer,
// the same CROSS_UID_FILES machinery production uses).
var crossUIDBootMatrix = []crossUIDBootMatrixRow{
	{artifact: "/agentd-secrets/secrets-env (staged delta)", writerUID: "1000 init / 2000 sidecar", readerUID: "2000 sidecar (pull endpoint) — volume NEVER in uid-1000 space", mode: "0640 (CROSS_UID) / 0600", outcome: "sidecar reads; uid-1000 consumes only via the child env the supervisor composes"},
	{artifact: "/sandbox-runtime/rt/secrets/* (secret-files)", writerUID: "2000", readerUID: "1000 tools (gid 1000)", mode: "0640 files / 0770 dirs (CROSS_UID)", outcome: "group-readable"},
	{artifact: "/sandbox-runtime/rt/ssh/* (ssh keys)", writerUID: "2000", readerUID: "1000 tools (gid 1000)", mode: "0640 (CROSS_UID)", outcome: "group-readable"},
	{artifact: "/sandbox-runtime/rt/git-credentials", writerUID: "2000", readerUID: "1000 tools (gid 1000)", mode: "0640 (CROSS_UID)", outcome: "group-readable"},
	{artifact: "agent-config.json", writerUID: "2000", readerUID: "1000 opencode", mode: "0640 always (T2 exception)", outcome: "group-readable; integrity by RO mount (US-4b)"},
	{artifact: "last-reload-secrets.json (reload cache)", writerUID: "1000 init / 2000 sidecar", readerUID: "2000", mode: "0640 (CROSS_UID)", outcome: "sidecar-only volume (US-4b)"},
	{artifact: "OPENCODE_SERVER_PASSWORD (pull credential)", writerUID: "controller (Secret keyRef)", readerUID: "1000 supervisor", mode: "env-only", outcome: "readable at spawn by construction — A2 evidence; §D1 carve-out class"},
	{artifact: "AGENTD_CONTROL_PLANE_PASSWORD", writerUID: "controller (Secret keyRef)", readerUID: "2000 sidecar ONLY", mode: "env-only", outcome: "must never exist in uid-1000 space (pinned pod-spec side)"},
	{artifact: "/v1/spawn-env (pull endpoint)", writerUID: "—", readerUID: "credentialed callers only", mode: "Basic (§D1 pair)", outcome: "401 without credential — uncredentialed uid-1000 code gets nothing"},
}

func TestCrossUIDBootMatrix_RegistryExecutableRows(t *testing.T) {
	// The registry must never be empty and every row must name its
	// crossing completely — a row with a blank field is a matrix decay.
	require.NotEmpty(t, crossUIDBootMatrix)
	for _, row := range crossUIDBootMatrix {
		require.NotEmpty(t, row.artifact)
		require.NotEmpty(t, row.writerUID)
		require.NotEmpty(t, row.readerUID)
		require.NotEmpty(t, row.mode)
		require.NotEmpty(t, row.outcome)
	}
}

func matrixPaths(t *testing.T) secretpkg.Paths {
	t.Helper()
	dir := t.TempDir()
	return secretpkg.Paths{
		Home:            dir,
		SecretsBaseDir:  filepath.Join(dir, "secrets"),
		SSHDir:          filepath.Join(dir, "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}
}

// TestCrossUIDBootMatrix_MaterializerModes: the CROSS_UID machinery
// writes every tool-consumed artifact group-readable (0640/0770) so the
// uid-2000 writer and uid-1000 readers coexist via gid 1000 — one
// materialize run validates the whole file-mode family of the matrix.
func TestCrossUIDBootMatrix_MaterializerModes(t *testing.T) {
	paths := matrixPaths(t)
	m := secretpkg.NewMaterializer()
	m.Paths = paths
	m.CrossUID = true

	_, err := m.Materialize([]secretpkg.Secret{
		{Type: "env-secret", Name: "e", Metadata: map[string]string{"var_name": "MY_VAR"}, Plaintext: "v"},
		{Type: "ssh-key", Name: "deploy", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "ssh-ed25519 AAAA"},
		{Type: "git-credential", Name: "gh", Metadata: map[string]string{"host": "github.com"}, Plaintext: "ghp_tok123"},
		{Type: "secret-file", Name: "cert", Metadata: map[string]string{"mount_path": "app/cert.pem"}, Plaintext: "CERTDATA"},
	})
	require.NoError(t, err)

	requirePerm(t, paths.SecretsEnvPath, 0o640, "secrets-env (staged delta source) 0640 under CROSS_UID")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "app"), 0o770, "secret-file dir 0770")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "app", "cert.pem"), 0o640, "secret-file 0640")

	sshKey, err := filepath.Glob(filepath.Join(paths.SSHDir, "*"))
	require.NoError(t, err)
	require.NotEmpty(t, sshKey)
	requirePerm(t, sshKey[0], 0o640, "ssh key 0640")

	requirePerm(t, paths.GitCredsPath, 0o640, "git-credentials 0640")
}

func requirePerm(t *testing.T, path string, want os.FileMode, msg string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err, "%s: stat %s", msg, path)
	require.Equal(t, want, info.Mode().Perm(), "%s: %s", msg, path)
}

// TestCrossUIDBootMatrix_AgentConfigAlwaysGroupReadable: agent-config
// is the T2 exception — 0640 in BOTH mode branches (uid-2000 writer,
// uid-1000 opencode reader), never owner-only.
func TestCrossUIDBootMatrix_AgentConfigAlwaysGroupReadable(t *testing.T) {
	require.Equal(t, os.FileMode(0o640), secretpkg.AgentConfigWriteMode)
}

// TestCrossUIDBootMatrix_PullEndpointCredentialGate: the spawn-pull
// endpoint serves nothing without a §D1 credential — uncredentialed
// uid-1000 code (which by definition holds no agentdPassword) gets a
// bare 401, and even a wrong credential gets nothing.
func TestCrossUIDBootMatrix_PullEndpointCredentialGate(t *testing.T) {
	h := spawnEnvHandler("pw", "cp", writeSecretsEnv(t, t.TempDir(), "export GATED='secret'\n"))

	for _, tc := range []struct{ name, user, pass string }{
		{"no credential", "", ""},
		{"wrong credential", "opencode", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/spawn-env", nil)
			if tc.pass != "" {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.NotContains(t, rec.Body.String(), "GATED",
				"no delta values may leak on an auth failure")
		})
	}
}

// TestCrossUIDBootMatrix_SupervisorCredentialEnvReadable: A2 runtime
// half — the pull credential is the supervisor's OWN container env
// (OPENCODE_SERVER_PASSWORD, wired by the controller; the pod-spec half
// is pinned controller-side). Readable at spawn by construction.
func TestCrossUIDBootMatrix_SupervisorCredentialEnvReadable(t *testing.T) {
	t.Setenv(supervisorCredentialEnv, "a2-evidence")
	require.Equal(t, "a2-evidence", supervisorSpawnCredential())
}
