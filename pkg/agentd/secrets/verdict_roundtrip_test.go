// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review N1 on #871: the reload-secrets cache round-trip used to lose
// the metadata-invalid verdict (flag was json:"-"; nil metadata
// marshaled as null), so a secret REJECTED at first parse materialized
// with defaults after a container restart — a T5 violation on the
// restart path. The verdict now persists through the reserved
// "metadata_invalid" wire key: cache replay must skip identically to
// the first parse.

func TestSecret_CacheRoundTrip_PreservesInvalidVerdict(t *testing.T) {
	original := Secret{
		Type:            "ssh-key",
		Name:            "deploy",
		Metadata:        nil, // normalized from garbage
		Plaintext:       "pk",
		MetadataInvalid: "metadata is not a JSON object: json: cannot unmarshal string into Go value of type map[string]json.RawMessage",
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var restored Secret
	require.NoError(t, json.Unmarshal(data, &restored))

	assert.Equal(t, original.MetadataInvalid, restored.MetadataInvalid,
		"the skip verdict must survive the cache round-trip")
	assert.Equal(t, "ssh-key", restored.Type)
	assert.Equal(t, "deploy", restored.Name)
	assert.Equal(t, "pk", restored.Plaintext)
}

// The wire form itself: verdict key present, metadata null (not
// dropped), no phantom fields.
func TestSecret_MarshalWireForm_InvalidVerdict(t *testing.T) {
	s := Secret{Type: "ssh-key", Name: "deploy", MetadataInvalid: "bad metadata"}
	data, err := json.Marshal(s)
	require.NoError(t, err)

	var probe map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &probe))
	require.Contains(t, probe, "metadata_invalid")
	require.Contains(t, string(probe["metadata_invalid"]), "bad metadata")
	require.Contains(t, probe, "metadata", "metadata key must be present even when nil (null), not omitted")
	assert.Equal(t, "null", string(probe["metadata"]))
}

// Clean entries marshal without the reserved key (byte-shape stable for
// the cache's other consumers).
func TestSecret_MarshalWireForm_CleanEntryNoVerdictKey(t *testing.T) {
	s := Secret{Type: "env-secret", Name: "x", Metadata: map[string]string{"var_name": "X"}, Plaintext: "v"}
	data, err := json.Marshal(s)
	require.NoError(t, err)

	var probe map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &probe))
	assert.NotContains(t, probe, "metadata_invalid")
}

// End-to-end replay: parse a file with a garbage-metadata ssh-key,
// simulate the reload cache write, then re-parse from the cache — the
// replayed entry must still be Skipped by Materialize (this is the
// exact two-run sequence the reviewer reproduced against the real
// binary, where run 2 used to write id_ed25519_deploy).
func TestSecret_ReplayFromCache_SkipsIdentically(t *testing.T) {
	dir := t.TempDir()
	original := `[
		{"type":"ssh-key","name":"deploy","metadata":"garbage","plaintext":"pk"},
		{"type":"env-secret","name":"ok","metadata":{"var_name":"OK"},"plaintext":"v"}
	]`
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	run1, err := LoadSecretsFile(path)
	require.NoError(t, err)

	// What the reload handler caches: the parsed batch.
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	cacheData, err := json.Marshal(run1)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cachePath, cacheData, 0o600))

	// Restart: parse from the cache.
	run2, err := LoadSecretsFile(cachePath)
	require.NoError(t, err)

	m := &Materializer{FS: RealFS(), Paths: Paths{
		Home:            "/home/sandbox",
		SecretsBaseDir:  filepath.Join(dir, "rt", "secrets"),
		SSHDir:          filepath.Join(dir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}}
	res, err := m.Materialize(run2)
	require.NoError(t, err)

	byName := map[string]SecretResult{}
	for _, r := range res.Results {
		byName[r.Name] = r
	}
	assert.Equal(t, OutcomeSkipped, byName["deploy"].Outcome,
		"replayed garbage-metadata ssh-key must STILL be skipped — not materialized with defaults (N1)")
	assert.Contains(t, byName["deploy"].Reason, "not a JSON object")
	assert.Equal(t, OutcomeMaterialized, byName["ok"].Outcome)
}

// The assumption-5 strike (review round 5): the persisted verdict skips
// replay for EVERY type, including metadata-ignoring ones. An api-key
// with garbage metadata is Skipped on first boot AND after a cache
// replay — the "replay flip" the worklog once claimed does not exist
// at HEAD.
func TestSecret_ReplayFromCache_MetadataIgnoringTypeStillSkips(t *testing.T) {
	dir := t.TempDir()
	original := `[
		{"type":"api-key","name":"legacy-key","metadata":"garbage","plaintext":"{\"kind\":\"custom\",\"slug\":\"x\"}"},
		{"type":"env-secret","name":"ok","metadata":{"var_name":"OK"},"plaintext":"v"}
	]`
	path := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	run1, err := LoadSecretsFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, run1[0].MetadataInvalid, "first parse records the verdict")

	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	cacheData, err := json.Marshal(run1)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cachePath, cacheData, 0o600))

	run2, err := LoadSecretsFile(cachePath)
	require.NoError(t, err)
	require.Equal(t, run1[0].MetadataInvalid, run2[0].MetadataInvalid,
		"api-key verdict must survive the cache round-trip")

	m := &Materializer{FS: RealFS(), Paths: Paths{
		Home:            "/home/sandbox",
		SecretsBaseDir:  filepath.Join(dir, "rt", "secrets"),
		SSHDir:          filepath.Join(dir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "git-credentials"),
	}}
	res, err := m.Materialize(run2)
	require.NoError(t, err)
	byName := map[string]SecretResult{}
	for _, r := range res.Results {
		byName[r.Name] = r
	}
	assert.Equal(t, OutcomeSkipped, byName["legacy-key"].Outcome,
		"metadata-ignoring type must STILL skip on replay — no flip at HEAD")
	assert.Equal(t, OutcomeMaterialized, byName["ok"].Outcome)
}
