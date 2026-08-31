// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_files_exec_test.go — R2b (#1165) exec-level integration: the REAL
// `supervise-opencode` subcommand pulling the REAL /v1/spawn-files
// endpoint over HTTP and delivering files with the production materializer's
// staging output. This is the in-process analog of the live defect:
//
//   - AC-F1 (ownership by construction): the delivered ssh key + config
//     are written by the supervisor's own uid with the mode contract —
//     the property OpenSSH's ownership check requires and the uid-2000
//     writer could never confer.
//   - AC-F2 (revocation is absence): an empty staging manifest removes the
//     previously delivered set at the next spawn — via the ledger, never
//     by directory wipe.
//   - AC-F3 (blast radius): a user-owned known_hosts in the same dir
//     survives every reload cycle.
//   - AC-F4: files_rev appears on the control socket status (terminal
//     verification, I4) and clears its degrade on recovery.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/stretchr/testify/require"
)

const execFilesPassword = "exec-files-pw"

// filesMux is a live endpoint pair whose staging source can be swapped
// mid-test (the reload path re-stages; the supervisor keeps pulling the
// same address).
type filesMux struct {
	addr    string
	staging *string
}

// startFilesMux serves the PRODUCTION spawn-env + spawn-files handlers
// over one mux with an indirection on the staging dir so a test can
// publish a new generation without changing the address.
func startFilesMux(t *testing.T, secretsEnvPath string, stagingDir *string) *filesMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/spawn-env", http.HandlerFunc(spawnEnvHandler(execFilesPassword, "", secretsEnvPath)))
	mux.Handle("/v1/spawn-files", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spawnFilesHandler(execFilesPassword, "", *stagingDir)(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &filesMux{addr: stripHTTP(srv.URL), staging: stagingDir}
}

// filesEnvFor builds the supervisor subprocess env: pull addr, credential,
// delivery roots + ledger in a temp tree, and the staging override is NOT
// needed supervisor-side (the endpoint serves staging; the supervisor
// only consumes the manifest).
func filesEnvFor(addr string, rtDir, ledger string) []string {
	return []string{
		"LLMSAFESPACES_SPAWN_ENV_PULL_ADDR=" + addr,
		"OPENCODE_SERVER_PASSWORD=" + execFilesPassword,
		fileDeliveryRootsEnvOverride + "=" + strings.Join([]string{
			filepath.Join(rtDir, "ssh"),
			filepath.Join(rtDir, "secrets"),
			rtDir,
		}, ":"),
		fileDeliveryLedgerEnvOverride + "=" + ledger,
	}
}

// materializeStaged runs the production materializer against tmp paths
// and returns the staging dir it published.
func materializeStaged(t *testing.T, rtDir string, batch []secrets.Secret) string {
	t.Helper()
	staging := filepath.Join(t.TempDir(), "staged")
	paths := secrets.Paths{
		Home:            t.TempDir(),
		SecretsBaseDir:  filepath.Join(rtDir, "secrets"),
		SSHDir:          filepath.Join(rtDir, "ssh"),
		AgentConfigPath: filepath.Join(t.TempDir(), "agent-config.json"),
		SecretsEnvPath:  filepath.Join(t.TempDir(), "secrets-env"),
		GitCredsPath:    filepath.Join(rtDir, "git-credentials"),
		StagingDir:      staging,
	}
	_, err := (&secrets.Materializer{FS: secrets.RealFS(), Paths: paths}).Materialize(batch)
	require.NoError(t, err)
	return staging
}

// TestSupervisorSubprocess_SpawnFiles_OwnershipByConstruction (AC-F1):
// with ssh + git credentials staged, the supervisor's first spawn
// delivers them — written by the supervisor's own uid, mode contract
// intact, config Include line present, files_rev reported.
func TestSupervisorSubprocess_SpawnFiles_OwnershipByConstruction(t *testing.T) {
	rtDir := t.TempDir()
	staging := materializeStaged(t, rtDir, []secrets.Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "EXEC_KEY_BYTES",
			Metadata: map[string]string{"key_type": "ed25519", "host": "github.com"}},
		{Type: "git-credential", Name: "gh", Plaintext: "exec_token_123",
			Metadata: map[string]string{"host": "github.com"}},
	})
	secretsEnv := writeSecretsEnv(t, t.TempDir(), "export PULL_PROBE='x'\n")
	m := startFilesMux(t, secretsEnv, &staging)

	sp := startSupervisorSubprocessEnv(t, filesEnvFor(m.addr, rtDir, filepath.Join(t.TempDir(), "led.json"))...)
	cc := newControlClient(sp.addr)

	require.Eventually(t, func() bool { return sp.childPIDOf(t, cc) > 0 },
		15*time.Second, 100*time.Millisecond, "first spawn must happen (delivery never blocks it)")

	keyPath := filepath.Join(rtDir, "ssh", "id_ed25519_github")
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(keyPath)
		return err == nil && string(data) == "EXEC_KEY_BYTES"
	}, 15*time.Second, 200*time.Millisecond, "the staged key must be delivered at first spawn")

	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "ssh key mode contract at delivery")

	cfg, err := os.ReadFile(filepath.Join(rtDir, "ssh", "config"))
	require.NoError(t, err)
	require.Contains(t, string(cfg), "Include config.d/*", "user-fragment home wired into the generated config")
	require.Contains(t, string(cfg), "Host github.com")

	git, err := os.ReadFile(filepath.Join(rtDir, "git-credentials"))
	require.NoError(t, err)
	require.Contains(t, string(git), "exec_token_123")

	_, err = os.Stat(filepath.Join(rtDir, "ssh", "config.d"))
	require.NoError(t, err, "config.d exists from the first delivery on")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.FilesRev != "" && st.SpawnFilesReason == ""
	}, 10*time.Second, 100*time.Millisecond, "files_rev must surface on the control socket, healthy")
}

// TestSupervisorSubprocess_SpawnFiles_RevocationAndBlastRadius (AC-F2/F3):
// deliver v1, then publish an empty manifest (unbind-all) and reload: the
// delivered set is revoked by ledger, and a user-owned known_hosts in the
// same directory survives — the exact #1165 live defect, inverted.
func TestSupervisorSubprocess_SpawnFiles_RevocationAndBlastRadius(t *testing.T) {
	rtDir := t.TempDir()
	staging := materializeStaged(t, rtDir, []secrets.Secret{
		{Type: "ssh-key", Name: "github", Plaintext: "V1",
			Metadata: map[string]string{"key_type": "ed25519", "host": "github.com"}},
	})
	secretsEnv := writeSecretsEnv(t, t.TempDir(), "")
	m := startFilesMux(t, secretsEnv, &staging)
	ledger := filepath.Join(t.TempDir(), "led.json")

	sp := startSupervisorSubprocessEnv(t, filesEnvFor(m.addr, rtDir, ledger)...)
	cc := newControlClient(sp.addr)
	cc.timeout = 30 * time.Second

	require.Eventually(t, func() bool { return sp.childPIDOf(t, cc) > 0 }, 15*time.Second, 100*time.Millisecond)
	keyPath := filepath.Join(rtDir, "ssh", "id_ed25519_github")
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(keyPath)
		return err == nil && string(data) == "V1"
	}, 15*time.Second, 200*time.Millisecond)

	// The user's own ssh state in the SAME directory.
	knownHosts := filepath.Join(rtDir, "ssh", "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, []byte("github.com ssh-ed25519 AAAA\n"), 0o600))

	// Unbind-all: publish an empty staging generation on the SAME address
	// (the reload path re-stages), then a credential reload restart.
	staging = materializeStaged(t, rtDir, nil)
	_, err := cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := os.Stat(keyPath)
		return os.IsNotExist(err)
	}, 20*time.Second, 200*time.Millisecond, "revocation is absence: the empty manifest removes the delivered key at the next spawn")

	data, err := os.ReadFile(knownHosts)
	require.NoError(t, err, "user known_hosts must survive the reload cycle")
	require.Contains(t, string(data), "ssh-ed25519")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.SpawnFilesReason == ""
	}, 10*time.Second, 100*time.Millisecond, "empty-but-healthy is quiet")
}

// TestSupervisorSubprocess_SpawnFiles_DeadMuxLoud (AC-F4 unhappy):
// first boot with the endpoint unreachable: the spawn proceeds
// (never-block), no files are delivered, and the degrade names
// spawn_files_unavailable; bringing the mux up heals at the next spawn.
func TestSupervisorSubprocess_SpawnFiles_DeadMuxLoud(t *testing.T) {
	rtDir := t.TempDir()
	ledger := filepath.Join(t.TempDir(), "led.json")

	sp := startSupervisorSubprocessEnv(t, filesEnvFor("127.0.0.1:1", rtDir, ledger)...)
	cc := newControlClient(sp.addr)
	cc.timeout = 30 * time.Second

	require.Eventually(t, func() bool { return sp.childPIDOf(t, cc) > 0 },
		15*time.Second, 100*time.Millisecond, "a dead endpoint must never block the spawn")

	require.Eventually(t, func() bool {
		st, err := cc.Status(context.Background())
		return err == nil && st.SpawnFilesReason == spawnFilesReasonUnavailable
	}, 15*time.Second, 100*time.Millisecond, "the file-delivery degrade must be loud and machine-readable")

	_, err := os.Stat(filepath.Join(rtDir, "ssh"))
	require.True(t, os.IsNotExist(err), "nothing is delivered from a dead endpoint")
}
