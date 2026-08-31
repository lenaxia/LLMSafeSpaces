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
	// ownerUID is the OWNING uid of the delivered artifact — distinct
	// from the writer wherever a relay moved bytes across the boundary
	// (#1165: OpenSSH rejects configs not owned by the invoking user; a
	// readability-only row could not predict that failure).
	ownerUID string
	// consumerConstraint names the strictest parser requirement the
	// uid-1000 consumer imposes on the artifact (ownership, no
	// group/world-write, none, ...) — the field that makes the matrix
	// predict usability, not just readability.
	consumerConstraint string
	mode               string
	outcome            string
}

// The enumeration itself — the standing registry. File-mode rows are
// validated by the executable checks below (via the real materializer,
// the same CROSS_UID_FILES machinery production uses).
var crossUIDBootMatrix = []crossUIDBootMatrixRow{
	// ---- R2b (#1165): file-class delivery is staged by the materializer
	// and DELIVERED by the consuming uid — owner 1000 by construction. —-
	{artifact: "/sandbox-runtime/staged-secret-files/* (staging tree)", writerUID: "2000 sidecar / 1000 single-container materializer", readerUID: "2000 pull endpoint only (sidecar mode)", ownerUID: "writer itself", consumerConstraint: "none — transport artifact, never consumed by tools", mode: "0700 dirs / staged bytes final-mode", outcome: "published atomically (tmp→rename swap); absent = quiet empty manifest"},
	{artifact: "/sandbox-runtime/rt/ssh/id_* (ssh private keys)", writerUID: "1000 supervisor (pull) / 1000 materializer (single-container direct)", readerUID: "1000 ssh", ownerUID: "1000", consumerConstraint: "ssh: refusing keys beyond permission drift; 0600 contract", mode: "0600 (ModeSSHPrivateKey)", outcome: "ownership by construction — the #1165 root cause is structurally gone"},
	{artifact: "/sandbox-runtime/rt/ssh/config", writerUID: "1000 supervisor / 1000 materializer", readerUID: "1000 ssh", ownerUID: "1000", consumerConstraint: "ssh readconf: owner must be invoking user or root; never group/world-writable", mode: "0600 (ModeSSHConfig)", outcome: "Include config.d/* gives user fragments a reload-proof home"},
	{artifact: "/sandbox-runtime/rt/ssh/known_hosts + config.d/* (user state)", writerUID: "1000 user/ssh itself", readerUID: "1000 ssh", ownerUID: "1000", consumerConstraint: "n/a (user-owned)", mode: "user-chosen", outcome: "NEVER a manifest target — reset is ledger-scoped, so reloads provably never touch it (blast-radius pin)"},
	{artifact: "/sandbox-runtime/rt/git-credentials", writerUID: "1000 supervisor / 1000 materializer", readerUID: "1000 git credential store", ownerUID: "1000", consumerConstraint: "none today (store helper checks nothing) — 0600 by secret semantics", mode: "0600 (ModeGitCredential)", outcome: "the #1087 path, now ownership-correct too"},
	{artifact: "/sandbox-runtime/rt/secrets/* (secret-files)", writerUID: "1000 supervisor / 1000 materializer", readerUID: "1000 arbitrary tools", ownerUID: "1000", consumerConstraint: "ARBITRARY (kubeconfig/gnupg/TLS-class consumers may check ownership AND mode) — owner-1000 + user-settable mode ≤0640-no-group-write", mode: "0600 default; metadata mode honored within contract", outcome: "strict-consumer class closed by ownership + mode contract"},
	{artifact: "/sandbox-runtime/spawn-files-ledger.json", writerUID: "1000 supervisor / 1000 materializer (single-container)", readerUID: "same writers only", ownerUID: "1000", consumerConstraint: "none (platform-internal)", mode: "0600", outcome: "level-triggered revocation truth: reset deletes ledger−manifest, never directories"},
	// ---- unchanged crossings —-
	{artifact: "/agentd-secrets/secrets-env (staged delta)", writerUID: "1000 init / 2000 sidecar", readerUID: "2000 sidecar (pull endpoint) — volume NEVER in uid-1000 space", ownerUID: "writer itself", consumerConstraint: "none — platform parser", mode: "0640 (CROSS_UID) / 0600", outcome: "sidecar reads; uid-1000 consumes only via the child env the supervisor composes"},
	{artifact: "agent-config.json", writerUID: "2000", readerUID: "1000 opencode", ownerUID: "2000", consumerConstraint: "none (node JSON reader)", mode: "0640 always (T2 exception)", outcome: "group-readable; integrity by RO mount (US-4b)"},
	{artifact: "last-reload-secrets.json (reload cache)", writerUID: "1000 init / 2000 sidecar", readerUID: "2000", ownerUID: "writer itself", consumerConstraint: "none (platform parser)", mode: "0640 (CROSS_UID)", outcome: "sidecar-only volume (US-4b)"},
	{artifact: "OPENCODE_SERVER_PASSWORD (pull credential)", writerUID: "controller (Secret keyRef)", readerUID: "1000 supervisor", ownerUID: "n/a (env)", consumerConstraint: "none", mode: "env-only", outcome: "readable at spawn by construction — A2 evidence; §D1 carve-out class"},
	{artifact: "AGENTD_CONTROL_PLANE_PASSWORD", writerUID: "controller (Secret keyRef)", readerUID: "2000 sidecar ONLY", ownerUID: "n/a (env)", consumerConstraint: "none", mode: "env-only", outcome: "must never exist in uid-1000 space (pinned pod-spec side)"},
	{artifact: "/v1/spawn-env (pull endpoint)", writerUID: "—", readerUID: "credentialed callers only", ownerUID: "—", consumerConstraint: "§D1 Basic pair", mode: "Basic (§D1 pair)", outcome: "401 without credential — uncredentialed uid-1000 code gets nothing"},
	{artifact: "/v1/spawn-files (pull endpoint, R2b)", writerUID: "—", readerUID: "credentialed callers only", ownerUID: "—", consumerConstraint: "§D1 Basic pair", mode: "Basic (§D1 pair)", outcome: "401 without credential; corrupt staging is a loud 500, never partial"},
}

func TestCrossUIDBootMatrix_RegistryExecutableRows(t *testing.T) {
	// The registry must never be empty and every row must name its
	// crossing completely — a row with a blank field is a matrix decay.
	require.NotEmpty(t, crossUIDBootMatrix)
	for _, row := range crossUIDBootMatrix {
		require.NotEmpty(t, row.artifact)
		require.NotEmpty(t, row.writerUID)
		require.NotEmpty(t, row.readerUID)
		require.NotEmpty(t, row.ownerUID, "%s: the #1165 lesson — owner uid must be explicit", row.artifact)
		require.NotEmpty(t, row.consumerConstraint, "%s: predict usability, not just readability", row.artifact)
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
		StagingDir:      filepath.Join(dir, "staged"),
	}
}

// TestCrossUIDBootMatrix_R2BDeliveryShape (R2b, #1165): one materialize
// + one delivery validates the whole file-class family of the matrix —
// staged owner-only by the CROSS_UID materializer, delivered uid-1000-
// owned with the per-type mode contracts by the applier.
func TestCrossUIDBootMatrix_R2BDeliveryShape(t *testing.T) {
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

	// Staging side: owner-only transport artifacts + a complete manifest.
	requirePerm(t, paths.StagingDir, 0o700, "staging root owner-only")
	requirePerm(t, filepath.Join(paths.StagingDir, secretpkg.ManifestName), 0o600, "staged manifest 0600")
	requirePerm(t, filepath.Join(paths.StagingDir, "ssh", "id_ed25519_deploy"), os.FileMode(secretpkg.ModeSSHPrivateKey), "staged key carries the mode contract")

	// Consumed side: untouched by the materializer, then delivered
	// owner-only with contract modes by the uid-1000 applier.
	for _, p := range []string{paths.SSHDir, paths.GitCredsPath, paths.SecretsBaseDir} {
		_, err := os.Stat(p)
		require.True(t, os.IsNotExist(err), "%s is delivery-side state only", p)
	}

	d := fileDelivery{
		roots:      []string{paths.SSHDir, paths.SecretsBaseDir, filepath.Dir(paths.GitCredsPath)},
		ledgerPath: filepath.Join(t.TempDir(), "led.json"),
		sshDir:     paths.SSHDir,
	}
	resp, ok := loadStagedFiles(paths.StagingDir)
	require.True(t, ok)
	_, err = d.apply(resp.Files)
	require.NoError(t, err)

	requirePerm(t, filepath.Join(paths.SSHDir, "id_ed25519_deploy"), os.FileMode(secretpkg.ModeSSHPrivateKey), "delivered ssh key 0600")
	requirePerm(t, filepath.Join(paths.SSHDir, "config"), os.FileMode(secretpkg.ModeSSHConfig), "delivered ssh config 0600")
	requirePerm(t, paths.GitCredsPath, os.FileMode(secretpkg.ModeGitCredential), "delivered git-credentials 0600")
	requirePerm(t, filepath.Join(paths.SecretsBaseDir, "app", "cert.pem"), os.FileMode(secretpkg.ModeSecretFile), "delivered secret-file 0600")
	requirePerm(t, filepath.Join(paths.SSHDir, "config.d"), 0o700, "user-fragment home exists")

	// env class unchanged: secrets-env keeps the CROSS_UID boot-handoff mode.
	requirePerm(t, paths.SecretsEnvPath, 0o640, "secrets-env (staged delta source) 0640 under CROSS_UID")
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
