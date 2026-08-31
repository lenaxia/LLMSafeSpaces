// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// W8 materializer-bypass coverage: the API's 2MiB per-secret gate never
// faces agentd directly — a crafted batch (or a future producer) can
// present arbitrary bytes to /v1/reload-secrets. The materializer's own
// ceilings must refuse loudly (500 + size_exceeded), keep the previously
// published staging manifest intact, and never persist the bad batch to
// the reload cache.

type sizeTestEnv struct {
	cfg      materializeConfig
	cache    string
	staging  string
	manifest string
}

func newSizeTestEnv(t *testing.T) *sizeTestEnv {
	t.Helper()
	dir := t.TempDir()
	secretsBase := filepath.Join(dir, "rt", "secrets")
	sshDir := filepath.Join(dir, "ssh")
	agentCfg := filepath.Join(dir, "agent-config.json")
	envPath := filepath.Join(dir, "secrets-env")
	gitCreds := filepath.Join(dir, ".git-credentials")
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	cfg := materializeConfig{
		secretsBaseDir:  secretsBase,
		sshDir:          sshDir,
		agentConfigPath: agentCfg,
		secretsEnvPath:  envPath,
		gitCredsPath:    gitCreds,
		home:            dir,
		reloadCachePath: cachePath,
	}
	staging := stagingDirFor(envPath, false)
	return &sizeTestEnv{
		cfg:      cfg,
		cache:    cachePath,
		staging:  staging,
		manifest: filepath.Join(staging, secrets.ManifestName),
	}
}

func (e *sizeTestEnv) reload(t *testing.T, batch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/reload-secrets", strings.NewReader(batch))
	req.Header.Set("Authorization", "Basic "+basicAuth("size-pw"))
	rec := httptest.NewRecorder()
	reloadSecretsHandler(e.cfg, reloadSecretsDeps{OpencodePassword: "size-pw"})(rec, req)
	return rec
}

func fileClassEntryJSON(name, mount string, value string) string {
	entry := secrets.Secret{
		Type:      "secret-file",
		Name:      name,
		Metadata:  map[string]string{"mount_path": mount},
		Plaintext: value,
	}
	out, _ := json.Marshal(entry)
	return string(out)
}

func fileClassBatch(name, mount string, value string) string {
	return "[" + fileClassEntryJSON(name, mount, value) + "]"
}

func requireStagedManifestLists(t *testing.T, env *sizeTestEnv, mount string) {
	t.Helper()
	data, err := os.ReadFile(env.manifest)
	require.NoError(t, err, "the published staging manifest must survive failed reloads")
	require.Contains(t, string(data), mount, "manifest must still list %q — a failed over-budget reload may not retract the last-good delivery set", mount)
}

// T5-tolerant contract: an oversized entry is a per-input defect — the
// entry fails loudly, the REST of the batch still materializes, and the
// manifest is the honest full-replace subset. Mirrors
// TestR2B_SizeExceeded_PerEntry at the real-handler level.
func TestE2E_ReloadSecrets_PerEntryOverBudget_TolerantSubset(t *testing.T) {
	env := newSizeTestEnv(t)

	rec := env.reload(t, fileClassBatch("good", "good.bin", "GOOD_BYTES"))
	require.Equal(t, http.StatusOK, rec.Code, "baseline reload failed: %s", rec.Body.String())
	requireStagedManifestLists(t, env, "good.bin")

	mixed := "[" + fileClassEntryJSON("ok", "ok2.bin", "FINE_BYTES") + "," +
		fileClassEntryJSON("huge", "huge.bin", strings.Repeat("B", agentd.StagedFilesMaxBytes+1)) + "]"
	rec = env.reload(t, mixed)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"any failed entry makes the reload loud at the HTTP surface: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "size_exceeded",
		"the failed entry carries the machine-readable reason: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"reloaded":1`,
		"T5 tolerance is in the STATE, not the status code — the valid entry still materializes: %s", rec.Body.String())

	requireStagedManifestLists(t, env, "ok2.bin")
	manifest, err := os.ReadFile(env.manifest)
	require.NoError(t, err)
	require.NotContains(t, string(manifest), "huge.bin", "the oversized entry never stages")
}

// All-failed batch: a single oversized entry makes the pass fail loudly
// (500), and — full-replace honesty — the published manifest is the
// empty set this pass produced. The reload cache still records the batch
// (the #443 replay path re-fails the entry loudly at the next boot,
// never blocking it); the API-side per-secret cap is the actual
// prevention — this is the bypass guard.
func TestE2E_ReloadSecrets_PerEntryOverBudget_AllFailed_Loud(t *testing.T) {
	env := newSizeTestEnv(t)

	rec := env.reload(t, fileClassBatch("good", "good.bin", "GOOD_BYTES"))
	require.Equal(t, http.StatusOK, rec.Code, "baseline reload failed: %s", rec.Body.String())
	requireStagedManifestLists(t, env, "good.bin")

	bypass := fileClassBatch("huge", "huge.bin", strings.Repeat("B", agentd.StagedFilesMaxBytes+1))
	rec = env.reload(t, bypass)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"an all-failed batch is a loud failure, not a silent no-op")
	require.Contains(t, rec.Body.String(), "size_exceeded",
		"machine-readable reason (W8): %s", rec.Body.String())

	manifest, err := os.ReadFile(env.manifest)
	require.NoError(t, err)
	require.Equal(t, "[]", strings.TrimSpace(string(manifest)),
		"full-replace honesty: the pass produced nothing, the manifest says so")
}

func TestE2E_ReloadSecrets_WholeBatchOverBudget_LoudFailure(t *testing.T) {
	env := newSizeTestEnv(t)

	rec := env.reload(t, fileClassBatch("good", "good.bin", "GOOD_BYTES"))
	require.Equal(t, http.StatusOK, rec.Code, "baseline reload failed: %s", rec.Body.String())
	requireStagedManifestLists(t, env, "good.bin")

	entryBytes := agentd.StagedFilesMaxBytes / 4
	entries := make([]secrets.Secret, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, secrets.Secret{
			Type:      "secret-file",
			Name:      fmt.Sprintf("part-%d", i),
			Metadata:  map[string]string{"mount_path": fmt.Sprintf("part-%d.bin", i)},
			Plaintext: strings.Repeat("P", entryBytes),
		})
	}
	body, err := json.Marshal(entries)
	require.NoError(t, err)
	if entryBytes*5 <= agentd.StagedFilesMaxBytes {
		t.Fatalf("fixture must exceed the whole-batch ceiling: entries=%d total=%d cap=%d",
			entryBytes, entryBytes*5, agentd.StagedFilesMaxBytes)
	}

	rec = env.reload(t, string(body))
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"individually-valid entries over the ceiling are a configuration problem — loud, whole-batch")
	require.Contains(t, rec.Body.String(), "size_exceeded",
		"machine-readable reason (W8): %s", rec.Body.String())

	requireStagedManifestLists(t, env, "good.bin")
	cacheData, err := os.ReadFile(env.cache)
	require.NoError(t, err)
	require.NotContains(t, string(cacheData), "part-0.bin",
		"the over-budget batch must never reach the reload cache")
}
