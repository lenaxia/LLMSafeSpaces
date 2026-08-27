// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// git_creds_boot_test.go — the #1087 regression gate.
//
// Issue #1087 (2026-08-27): a dev session provisioned GitHub auth solely
// as a GH_TOKEN env var injected at session creation. Suspend deleted the
// pod; resume re-created it; the env var was not replayed — and `gh` +
// `git push` both died mid-conversation. The platform's DURABLE path (a
// bound git-credential secret → per-boot materialization to
// /sandbox-runtime/rt/git-credentials → $HOME/.git-credentials symlink)
// existed and worked, but nothing asserted the full boot sequence.
//
// These tests exercise the REAL subcommands (`init-fs` then `materialize`,
// the same pair the pod runs on every boot) as subprocesses against a
// temp tree that mirrors the production volume layout:
//
//	pvc/                    ← survives suspend/resume (PVC)
//	└── home/.git-credentials → rt/git-credentials (init-fs symlink)
//	rt/                     ← wiped on pod death (memory emptyDir)
//	cfg/                    ← pod-scoped bootstrap (secrets.json here)
//
// The contract under test, straight from the issue's acceptance criteria:
//
//   - Cold boot with a bound git-credential secret → the file
//     materializes and the $HOME symlink RESOLVES (not dangling).
//   - Suspend (pod death wipes rt/) → the PVC symlink dangles — the
//     exact incident state.
//   - Resume (init-fs + materialize re-run; the bound secret persists in
//     /sandbox-cfg/secrets.json) → the file is back, the symlink
//     resolves again, no manual re-injection.
//   - Degraded boot WITHOUT a bound git-credential secret → boot still
//     exits 0 (absence is a valid zero-credential state, never a crash).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitCredentialColdBoot_SurvivesSuspendResume(t *testing.T) {
	bin := buildAgentdBinary(t)

	dir := t.TempDir()
	pvc := filepath.Join(dir, "pvc")
	cfg := filepath.Join(dir, "sandbox-cfg")
	rt := filepath.Join(dir, "sandbox-runtime")
	pwSrc := filepath.Join(dir, "pw-src")
	for _, d := range []string{cfg, pwSrc} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(pwSrc, "password"), []byte("pw\n"), 0o644))

	// The bound secret set (what the harness should write per #1087's
	// primary fix): one git-credential secret for github.com over https.
	const token = "ghp_1087RegressionToken"
	secretsPath := filepath.Join(cfg, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"git-credential","name":"gh","metadata":{"host":"github.com","protocol":"https"},"plaintext":"`+token+`"}
	]`), 0o600))

	homeCreds := filepath.Join(pvc, "home", ".git-credentials")

	boot := func() {
		// init-fs: PVC roots + the US-35.7 symlink farm ($HOME paths →
		// tmpfs targets). Requires the password source (G46).
		exit, stderr := runInitFSSubcommand(t, bin,
			"--pvc-root", pvc, "--cfg-dir", cfg, "--runtime-dir", rt,
			"--pw-source", pwSrc, "--freemodels", filepath.Join(dir, "no-models.json"))
		require.Equal(t, 0, exit, "init-fs failed: %s", stderr)

		// materialize: the entrypoint's real boot step, driven through
		// the same env-var path overrides the test suite uses.
		exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath,
			filepath.Join(rt, "rt", "secrets"),
			filepath.Join(rt, "rt", "ssh"),
			filepath.Join(rt, "agent-config.json"),
			filepath.Join(rt, "secrets-env"),
			filepath.Join(rt, "rt", "git-credentials"))
		require.Equal(t, 0, exit, "materialize failed: stderr=%q stdout=%q", stderr, stdout)
	}

	// --- Boot #1 (creation): bound secret → file + resolving symlink.
	boot()
	content, err := os.ReadFile(homeCreds)
	require.NoError(t, err, "cold boot: $HOME/.git-credentials must resolve")
	assert.Contains(t, string(content), "https://oauth2:"+token+"@github.com")

	// --- Suspend: pod death wipes the memory-backed emptyDir wholesale.
	require.NoError(t, os.RemoveAll(rt))
	_, statErr := os.Stat(homeCreds)
	require.Error(t, statErr, "after pod death the PVC symlink must dangle")
	require.True(t, os.IsNotExist(statErr), "expected dangling symlink, got: %v", statErr)

	// --- Resume: pod #2 runs the same boot pair; the bound secret (and
	// only it) is what restores the credential — acceptance criterion 2.
	boot()
	content, err = os.ReadFile(homeCreds)
	require.NoError(t, err, "after resume: $HOME/.git-credentials must resolve — no dangling symlink")
	assert.Contains(t, string(content), "https://oauth2:"+token+"@github.com")

	// The PVC side must still be a symlink (US-35.7: no plaintext bytes
	// on the PVC — the credential lives on tmpfs only).
	fi, err := os.Lstat(homeCreds)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0, "PVC home path must be a symlink, got %v", fi.Mode())
}

// TestGitCredentialAbsent_BootStillSucceeds pins the degraded path: a
// workspace with NO git-credential bound (the incident workspace) must
// boot cleanly — the dangling symlink is an availability gap for git
// auth, never a boot failure.
func TestGitCredentialAbsent_BootStillSucceeds(t *testing.T) {
	bin := buildAgentdBinary(t)

	dir := t.TempDir()
	pvc := filepath.Join(dir, "pvc")
	cfg := filepath.Join(dir, "sandbox-cfg")
	rt := filepath.Join(dir, "sandbox-runtime")
	pwSrc := filepath.Join(dir, "pw-src")
	for _, d := range []string{cfg, pwSrc} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(pwSrc, "password"), []byte("pw\n"), 0o644))

	// Secrets set WITHOUT any git-credential entry (e.g. an SSH key only).
	secretsPath := filepath.Join(cfg, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(`[
		{"type":"ssh-key","name":"deploy","metadata":{"key_type":"ed25519","host":"github.com"},"plaintext":"ssh-key-data"}
	]`), 0o600))

	exit, stderr := runInitFSSubcommand(t, bin,
		"--pvc-root", pvc, "--cfg-dir", cfg, "--runtime-dir", rt,
		"--pw-source", pwSrc, "--freemodels", filepath.Join(dir, "no-models.json"))
	require.Equal(t, 0, exit, "init-fs failed: %s", stderr)

	exit, stdout, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(rt, "rt", "secrets"),
		filepath.Join(rt, "rt", "ssh"),
		filepath.Join(rt, "agent-config.json"),
		filepath.Join(rt, "secrets-env"),
		filepath.Join(rt, "rt", "git-credentials"))
	require.Equal(t, 0, exit, "boot must not fail without a git-credential secret: stderr=%q stdout=%q", stderr, stdout)

	// The SSH key DID materialize (bound), git credentials did not
	// (unbound) — the incident's observable asymmetry.
	_, sshErr := os.Stat(filepath.Join(pvc, "home", ".ssh", "id_ed25519_deploy"))
	require.NoError(t, sshErr, "bound ssh-key must materialize")
	_, statErr := os.Stat(filepath.Join(pvc, "home", ".git-credentials"))
	require.True(t, os.IsNotExist(statErr), "unbound git-credential must leave the symlink dangling")
}
