// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Tests for the reload-secrets cache (issue #443).
//
// When a workspace's main container restarts (OOM, panic, kubelet restart —
// anything short of full pod recreation), the boot-time `materialize`
// subcommand runs again and its reset() wipes the user-DEK credentials that
// were live-pushed via /v1/reload-secrets. To survive a container restart we
// persist the last reload batch to /sandbox-runtime/last-reload-secrets.json
// (tmpfs — survives container restart, wiped on pod death) and replay it on
// the next boot, merged on top of the base /sandbox-cfg/secrets.json batch.
//
// These tests are written TDD-style before the implementation:
//
//   - mergeSecretBatches: base + cache, cache wins on duplicate Type+Name.
//   - writeReloadSecretsCache: atomic write, mode 0600, temp+rename.
//   - loadReloadSecretsCache: absent → empty; corrupt → warn + empty; valid.
//   - reloadSecretsHandler: persists after success; never on failure.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// mergeSecretBatches
// =============================================================================

func TestMergeSecretBatches_CacheWinsOnDuplicate(t *testing.T) {
	base := []secrets.Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "base-value"},
		{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "base-ssh"},
	}
	cache := []secrets.Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "cache-value"},
	}

	merged := mergeSecretBatches(base, cache)

	// Exactly one env-secret "gh" (cache wins), plus the base ssh-key.
	require.Len(t, merged, 2)
	gh := findSecret(t, merged, "env-secret", "gh")
	assert.Equal(t, "cache-value", gh.Plaintext, "cache must win for duplicate Type+Name")
	assert.Contains(t, merged, secrets.Secret{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "base-ssh"})
}

func TestMergeSecretBatches_NoDuplicate_AllPresent(t *testing.T) {
	base := []secrets.Secret{
		{Type: "llm-provider", Name: "anthropic", Plaintext: `{"kind":"anthropic","slug":"anthropic"}`},
	}
	cache := []secrets.Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "tok"},
		{Type: "git-credential", Name: "g", Metadata: map[string]string{"protocol": "https", "host": "github.com"}, Plaintext: "user:pass"},
	}

	merged := mergeSecretBatches(base, cache)

	require.Len(t, merged, 3, "all distinct entries from both batches must be present")
}

func TestMergeSecretBatches_BothEmpty(t *testing.T) {
	assert.Empty(t, mergeSecretBatches(nil, nil))
	assert.Empty(t, mergeSecretBatches([]secrets.Secret{}, []secrets.Secret{}))
}

func TestMergeSecretBatches_BaseOnly(t *testing.T) {
	base := []secrets.Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}}
	assert.Equal(t, base, mergeSecretBatches(base, nil))
}

func TestMergeSecretBatches_CacheOnly(t *testing.T) {
	cache := []secrets.Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}}
	assert.Equal(t, cache, mergeSecretBatches(nil, cache))
}

func TestMergeSecretBatches_SameTypeDifferentName_BothKept(t *testing.T) {
	base := []secrets.Secret{
		{Type: "env-secret", Name: "a", Metadata: map[string]string{"var_name": "A"}, Plaintext: "1"},
	}
	cache := []secrets.Secret{
		{Type: "env-secret", Name: "b", Metadata: map[string]string{"var_name": "B"}, Plaintext: "2"},
	}
	merged := mergeSecretBatches(base, cache)
	require.Len(t, merged, 2, "different Name under the same Type is NOT a duplicate")
}

// =============================================================================
// writeReloadSecretsCache
// =============================================================================

func TestWriteReloadSecretsCache_WritesValidJSON0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-reload-secrets.json")
	batch := []secrets.Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "tok"},
	}

	require.NoError(t, writeReloadSecretsCache(path, batch))

	st, err := os.Stat(path)
	require.NoError(t, err, "cache file must exist")
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(),
		"cache file must be 0600 — it contains plaintext credentials")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got []secrets.Secret
	require.NoError(t, json.Unmarshal(data, &got))
	require.Len(t, got, 1)
	assert.Equal(t, "gh", got[0].Name)
}

func TestWriteReloadSecretsCache_OverwritesExistingAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-reload-secrets.json")
	require.NoError(t, writeReloadSecretsCache(path, []secrets.Secret{
		{Type: "env-secret", Name: "old", Plaintext: "old"},
	}))
	require.NoError(t, writeReloadSecretsCache(path, []secrets.Secret{
		{Type: "env-secret", Name: "new", Plaintext: "new"},
	}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got []secrets.Secret
	require.NoError(t, json.Unmarshal(data, &got))
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Name, "second write must replace the first (reload = full replace)")
}

func TestWriteReloadSecretsCache_EmptyBatchStillWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-reload-secrets.json")
	// An empty batch means "clear all live materialisations" (unbind). The
	// cache must record this so a subsequent container restart also clears
	// rather than reverting to the base secrets.json only.
	require.NoError(t, writeReloadSecretsCache(path, []secrets.Secret{}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(string(data)),
		"empty batch must persist as a JSON empty array so replay knows the user cleared all creds")
}

func TestWriteReloadSecretsCache_FailsOnUnwritableDir(t *testing.T) {
	roDir := filepath.Join(t.TempDir(), "ro")
	require.NoError(t, os.Mkdir(roDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })

	err := writeReloadSecretsCache(filepath.Join(roDir, "cache.json"), []secrets.Secret{
		{Type: "env-secret", Name: "x", Plaintext: "v"},
	})
	require.Error(t, err, "unwritable target must surface an error")
}

func TestWriteReloadSecretsCache_NoTempFileLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-reload-secrets.json")
	require.NoError(t, writeReloadSecretsCache(path, []secrets.Secret{
		{Type: "env-secret", Name: "x", Plaintext: "v"},
	}))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.Contains(e.Name(), ".tmp"),
			"no leftover temp file after atomic rename; found %q", e.Name())
	}
}

// =============================================================================
// loadReloadSecretsCache
// =============================================================================

func TestLoadReloadSecretsCache_Absent_ReturnsEmptyNoWarn(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	got := loadReloadSecretsCache(filepath.Join(dir, "absent.json"), &stderr)
	assert.Empty(t, got, "absent cache is the first-boot / never-reloaded case — empty")
	assert.Empty(t, stderr.String(), "absent cache must not warn (it is the normal first-boot state)")
}

func TestLoadReloadSecretsCache_Valid_ReturnsBatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, writeReloadSecretsCache(path, []secrets.Secret{
		{Type: "env-secret", Name: "gh", Plaintext: "tok"},
		{Type: "ssh-key", Name: "k", Plaintext: "key"},
	}))
	var stderr bytes.Buffer
	got := loadReloadSecretsCache(path, &stderr)
	require.Len(t, got, 2)
	assert.Empty(t, stderr.String())
}

func TestLoadReloadSecretsCache_Corrupt_WarnsAndReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	var stderr bytes.Buffer
	got := loadReloadSecretsCache(path, &stderr)
	assert.Empty(t, got, "corrupt cache must degrade to base-only materialization")
	assert.Contains(t, stderr.String(), "last-reload-secrets",
		"corrupt cache must warn so operators can diagnose missing creds after restart")
}

func TestLoadReloadSecretsCache_EmptyArray_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	require.NoError(t, os.WriteFile(path, []byte("[]"), 0o600))
	var stderr bytes.Buffer
	got := loadReloadSecretsCache(path, &stderr)
	assert.Empty(t, got, "empty array cache is a valid 'cleared' state — empty, no warn")
	assert.Empty(t, stderr.String())
}

// =============================================================================
// reloadSecretsHandler persistence
// =============================================================================

func newPersistTestCfg(t *testing.T) (materializeConfig, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
		reloadCachePath: filepath.Join(dir, "last-reload-secrets.json"),
	}
	return cfg, dir
}

// TestReloadSecretsHandler_PersistsCacheAfterMaterialize is the core regression
// test for #443: after a successful reload, the batch must be persisted so a
// container restart can replay it. Without this write, the next boot's
// materialize wipes user-DEK creds with no way to restore them.
func TestReloadSecretsHandler_PersistsCacheAfterMaterialize(t *testing.T) {
	cfg, dir := newPersistTestCfg(t)

	body := `[{"type":"env-secret","name":"gh","metadata":{"var_name":"GH_TOKEN"},"plaintext":"tok"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "rec=%s", rec.Body.String())

	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	data, err := os.ReadFile(cachePath)
	require.NoError(t, err, "cache must be written after successful reload")
	var persisted []secrets.Secret
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.Len(t, persisted, 1)
	assert.Equal(t, "GH_TOKEN", persisted[0].Metadata["var_name"])

	st, err := os.Stat(cachePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// TestReloadSecretsHandler_DoesNotPersistOnMaterializeFailure verifies the
// cache is NEVER written when materialization fails — otherwise a failed
// reload could persist an incomplete/wrong batch and clobber the last known
// good state on the next restart.
func TestReloadSecretsHandler_DoesNotPersistOnFailure(t *testing.T) {
	cfg, dir := newPersistTestCfg(t)

	// R2b (#1165): the platform-owned materialize failure surface is now
	// the STAGING tree (the consumed file-class dirs are delivery-side
	// state). An unwritable staging parent fails reset()'s MkdirAll → 500.
	roDir := filepath.Join(dir, "ro")
	require.NoError(t, os.Mkdir(roDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	cfg.stagingDir = filepath.Join(roDir, "staged")

	body := `[{"type":"env-secret","name":"gh","metadata":{"var_name":"GH_TOKEN"},"plaintext":"tok"}]`
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(cfg, reloadSecretsDeps{OpencodePassword: "test-pw"})(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	_, err := os.Stat(filepath.Join(dir, "last-reload-secrets.json"))
	assert.True(t, os.IsNotExist(err),
		"cache must NOT be written on failure — last known good state must survive")
}

// findSecret locates an entry by Type+Name in a merged batch (test helper).
func findSecret(t *testing.T, batch []secrets.Secret, typ, name string) secrets.Secret {
	t.Helper()
	for _, s := range batch {
		if s.Type == typ && s.Name == name {
			return s
		}
	}
	t.Fatalf("secret %s/%s not found in batch", typ, name)
	return secrets.Secret{}
}
