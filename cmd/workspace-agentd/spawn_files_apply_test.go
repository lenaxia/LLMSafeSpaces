// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// spawn_files_apply_test.go — R2b (#1165) unit coverage for the uid-1000
// delivery side: mode-faithful writes, ledger-scoped revocation (the
// reset blast-radius fix), path/mode confinement, the config.d user
// fragment home, and ledger continuity across supervisor restarts.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func deliveryFixture(t *testing.T) (fileDelivery, string) {
	t.Helper()
	dir := t.TempDir()
	sshDir := filepath.Join(dir, "rt", "ssh")
	secretDir := filepath.Join(dir, "rt", "secrets")
	d := fileDelivery{
		roots:      []string{sshDir, secretDir, filepath.Join(dir, "rt")},
		ledgerPath: filepath.Join(dir, "spawn-files-ledger.json"),
		sshDir:     sshDir,
	}
	return d, dir
}

func entry(path string, mode int, content string) spawnFileEntry {
	return spawnFileEntry{Path: path, Mode: mode, Content: []byte(content)}
}

func TestFileDelivery_WritesWithContractModes(t *testing.T) {
	d, dir := deliveryFixture(t)
	key := filepath.Join(dir, "rt", "ssh", "id_ed25519_x")
	cfg := filepath.Join(dir, "rt", "ssh", "config")

	rev, err := d.apply([]spawnFileEntry{
		entry(key, 0o600, "KEY"),
		entry(cfg, 0o600, "Include config.d/*\n"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, rev)

	data, err := os.ReadFile(key)
	require.NoError(t, err)
	require.Equal(t, "KEY", string(data))
	info, err := os.Stat(key)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "ssh key mode contract")

	// The user-fragment home exists from the first delivery on.
	fi, err := os.Stat(filepath.Join(dir, "rt", "ssh", "config.d"))
	require.NoError(t, err)
	require.True(t, fi.IsDir())

	// The ledger records the applied set + terminal rev.
	var led deliveryLedger
	raw, err := os.ReadFile(d.ledgerPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &led))
	require.Equal(t, rev, led.Rev)
	require.Contains(t, led.Paths, key)
}

func TestFileDelivery_LedgerScopedRevocation(t *testing.T) {
	d, dir := deliveryFixture(t)
	oldKey := filepath.Join(dir, "rt", "ssh", "id_ed25519_old")
	newKey := filepath.Join(dir, "rt", "ssh", "id_ed25519_new")

	_, err := d.apply([]spawnFileEntry{entry(oldKey, 0o600, "OLD")})
	require.NoError(t, err)

	// User-owned state in the SAME directory — never in any manifest.
	knownHosts := filepath.Join(dir, "rt", "ssh", "known_hosts")
	require.NoError(t, os.WriteFile(knownHosts, []byte("github.com ssh-ed25519 AAAA\n"), 0o600))
	userFrag := filepath.Join(dir, "rt", "ssh", "config.d", "local-host")
	require.NoError(t, os.MkdirAll(filepath.Dir(userFrag), 0o700))
	require.NoError(t, os.WriteFile(userFrag, []byte("Host internal\n"), 0o600))

	_, err = d.apply([]spawnFileEntry{entry(newKey, 0o600, "NEW")})
	require.NoError(t, err)

	_, err = os.Stat(oldKey)
	require.True(t, os.IsNotExist(err), "revoked entry is removed (absence is the delete)")

	data, err := os.ReadFile(knownHosts)
	require.NoError(t, err, "THE #1165 BLAST-RADIUS PIN: user known_hosts must survive every reload")
	require.Contains(t, string(data), "ssh-ed25519")
	frag, err := os.ReadFile(userFrag)
	require.NoError(t, err, "user config.d fragments are never manifest targets, never touched")
	require.Contains(t, string(frag), "internal")

	_, err = os.Stat(newKey)
	require.NoError(t, err)
}

func TestFileDelivery_LedgerSurvivesApplierRestart(t *testing.T) {
	d, dir := deliveryFixture(t)
	key := filepath.Join(dir, "rt", "ssh", "id_ed25519_x")
	_, err := d.apply([]spawnFileEntry{entry(key, 0o600, "v1")})
	require.NoError(t, err)

	// A NEW applier instance (supervisor restart) reads the same ledger:
	// revocation must still delete exactly the stale set.
	d2 := fileDelivery{roots: d.roots, ledgerPath: d.ledgerPath, sshDir: d.sshDir}
	_, err = d2.apply(nil)
	require.NoError(t, err)
	_, err = os.Stat(key)
	require.True(t, os.IsNotExist(err), "unbind-all after a restart still revokes via the persisted ledger")
}

func TestFileDelivery_EmptyManifestRevokesAll(t *testing.T) {
	d, dir := deliveryFixture(t)
	key := filepath.Join(dir, "rt", "git-credentials")
	_, err := d.apply([]spawnFileEntry{entry(key, 0o600, "https://oauth2:t@github.com\n")})
	require.NoError(t, err)

	rev, err := d.apply(nil)
	require.NoError(t, err)
	require.Equal(t, spawnFilesRev(nil), rev, "empty manifest → empty-delta rev")
	_, err = os.Stat(key)
	require.True(t, os.IsNotExist(err))
}

func TestFileDelivery_RejectsPathsOutsideRoots(t *testing.T) {
	d, dir := deliveryFixture(t)
	_, err := d.apply([]spawnFileEntry{entry(filepath.Join(dir, "escape", "x"), 0o600, "x")})
	require.ErrorIs(t, err, errBadDeliveryPath)

	// Traversal via .. resolves outside the roots after Clean.
	_, err = d.apply([]spawnFileEntry{entry(filepath.Join(dir, "rt", "ssh", "..", "..", "etc-passwd"), 0o600, "x")})
	require.ErrorIs(t, err, errBadDeliveryPath)
}

func TestFileDelivery_RejectsWeakModes(t *testing.T) {
	d, dir := deliveryFixture(t)
	key := filepath.Join(dir, "rt", "ssh", "config")
	for _, mode := range []int{0o640 + 0o020, 0o602, 0, -1} {
		_, err := d.apply([]spawnFileEntry{entry(key, mode, "x")})
		require.ErrorIs(t, err, errBadDeliveryPath, "mode 0%o must never be deliverable", mode)
	}
}

func TestFileDelivery_IdempotentReapply(t *testing.T) {
	d, dir := deliveryFixture(t)
	key := filepath.Join(dir, "rt", "ssh", "config")
	files := []spawnFileEntry{entry(key, 0o600, "Include config.d/*\n")}
	r1, err := d.apply(files)
	require.NoError(t, err)
	r2, err := d.apply(files)
	require.NoError(t, err)
	require.Equal(t, r1, r2, "re-applying the identical manifest is a no-op with a stable rev")
}

func TestFileDeliveryFromEnv_DefaultsAndOverrides(t *testing.T) {
	t.Setenv(fileDeliveryRootsEnvOverride, "")
	t.Setenv(fileDeliveryLedgerEnvOverride, "")
	d := fileDeliveryFromEnv()
	require.Equal(t, fileDeliveryLedgerPath, d.ledgerPath)
	require.Equal(t, "/sandbox-runtime/rt/ssh", d.sshDir)
	require.Contains(t, d.roots, "/sandbox-runtime/rt/ssh")

	t.Setenv(fileDeliveryRootsEnvOverride, "/a:/b")
	t.Setenv(fileDeliveryLedgerEnvOverride, "/tmp/led.json")
	d = fileDeliveryFromEnv()
	require.Equal(t, []string{"/a", "/b"}, d.roots)
	require.Equal(t, "/tmp/led.json", d.ledgerPath)
}
