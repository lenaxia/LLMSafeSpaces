// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// model_enricher_e2e_test.go exercises the full chain through the REAL
// workspace-agentd materialize subprocess:
//
//	secrets.json with an llm-provider that has BaseURL but no Models
//	  → workspace-agentd materialize (real binary)
//	  → enrichProviderModels (real HTTP fetch from /models endpoint)
//	  → FormatOpenCodeConfig (renders provider+models)
//	  → agent-config.json (the file opencode reads)
//
// The existing enricher tests (model_enricher_test.go) stop at the enricher
// RETURN VALUE. No test proves the fetched models actually land in
// agent-config.json through the real materialize → enrich → flush chain.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_ModelEnricher_ModelsLandInAgentConfig runs the REAL materialize
// binary against a fake /models endpoint and asserts the fetched models
// appear in agent-config.json with the correct npm attribute.
func TestE2E_ModelEnricher_ModelsLandInAgentConfig(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	// Fake /models endpoint returning three model IDs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "Bearer sk-enrich-test", r.Header.Get("Authorization"))
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "glm-5.1"},
				{"id": "glm-5.2"},
				{"id": "deepseek-v3"},
			},
		})
	}))
	defer srv.Close()

	// secrets.json with an llm-provider that has BaseURL but no Models.
	// plaintext must be a JSON STRING (double-encoded) containing the provider JSON.
	providerJSON, _ := json.Marshal(map[string]any{
		"kind":    "custom-llm",
		"slug":    "custom-llm",
		"apiKey":  "sk-enrich-test",
		"baseURL": srv.URL,
	})
	secretsBatch, err := json.Marshal([]map[string]any{{
		"type":      "llm-provider",
		"name":      "custom-llm",
		"plaintext": string(providerJSON),
	}})
	require.NoError(t, err)
	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, secretsBatch, 0o600))

	agentCfg := filepath.Join(dir, "agent-config.json")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfg,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "materialize must succeed; stderr=%s", stderr)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err, "agent-config.json must exist after materialize; stderr=%s", stderr)

	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Contains(t, cfg.Provider, "custom-llm",
		"the enriched provider must appear in agent-config.json")

	var entry struct {
		Options struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseURL"`
		} `json:"options"`
		NPM    string              `json:"npm"`
		Models map[string]struct{} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["custom-llm"], &entry))
	assert.Equal(t, "sk-enrich-test", entry.Options.APIKey)
	assert.Equal(t, srv.URL, entry.Options.BaseURL)
	assert.Equal(t, "@ai-sdk/openai-compatible", entry.NPM,
		"custom-BaseURL provider must set npm so opencode uses the OpenAI-compatible SDK")
	assert.Len(t, entry.Models, 3,
		"all three fetched models must appear in agent-config.json")
}

// TestE2E_ModelEnricher_FetchFail_StillWritesConfig is the unhappy path: when
// the /models endpoint returns 401, the provider must STILL be written to
// agent-config.json (with no models) so opencode registers it. A regression
// that drops the provider entirely on enrich failure would silently remove
// the user's credential.
func TestE2E_ModelEnricher_FetchFail_StillWritesConfig(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	providerJSON, _ := json.Marshal(map[string]any{
		"kind":    "failing-endpoint",
		"slug":    "failing-endpoint",
		"apiKey":  "sk-still-here",
		"baseURL": srv.URL,
	})
	secretsBatch, err := json.Marshal([]map[string]any{{
		"type":      "llm-provider",
		"name":      "failing-endpoint",
		"plaintext": string(providerJSON),
	}})
	require.NoError(t, err)
	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, secretsBatch, 0o600))

	agentCfg := filepath.Join(dir, "agent-config.json")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfg,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit,
		"materialize must succeed even when /models fetch fails (best-effort); stderr=%s", stderr)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err)
	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Contains(t, cfg.Provider, "failing-endpoint",
		"provider must STILL appear in agent-config.json even when /models fetch failed")

	var entry struct {
		Options struct {
			APIKey string `json:"apiKey"`
		} `json:"options"`
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["failing-endpoint"], &entry))
	assert.Equal(t, "sk-still-here", entry.Options.APIKey,
		"apiKey must survive the enrichment failure")
	assert.Empty(t, entry.Models, "no models must be present when the fetch failed")
}

// TestE2E_ModelEnricher_DeliveredAllowlistFiltersFetchedCatalog is #1575's
// production path END TO END through the REAL materialize binary: the
// batch entry carries the credential's model allowlist (delivery
// fields), the enricher fetches the live catalog, and the FILTERED
// list lands in agent-config.json — a model literally named "default"
// is delivered iff the provider serves it, with per-model limits
// merged.
func TestE2E_ModelEnricher_DeliveredAllowlistFiltersFetchedCatalog(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "default"},
				{"id": "glm-5.1"},
				{"id": "bge-m3"},
			},
		})
	}))
	defer srv.Close()

	providerJSON, _ := json.Marshal(map[string]any{
		"kind":               "thekaocloud",
		"slug":               "thekaocloud",
		"apiKey":             "sk-allowlist-test",
		"baseURL":            srv.URL,
		"modelAllowlist":     []string{"default", "glm-5.1"},
		"modelContextLimits": map[string]int{"default": 200000},
		"modelOutputLimits":  map[string]int{"default": 8192},
	})
	secretsBatch, err := json.Marshal([]map[string]any{{
		"type":      "llm-provider",
		"name":      "thekaocloud",
		"plaintext": string(providerJSON),
	}})
	require.NoError(t, err)
	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, secretsBatch, 0o600))

	agentCfg := filepath.Join(dir, "agent-config.json")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfg,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "materialize must succeed; stderr=%s", stderr)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err, "agent-config.json must exist after materialize; stderr=%s", stderr)

	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Contains(t, cfg.Provider, "thekaocloud")

	var entry struct {
		Models map[string]struct {
			Limit *struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["thekaocloud"], &entry))
	require.Len(t, entry.Models, 2, "fetched [default glm-5.1 bge-m3] filtered to the allowlist: %v", entry.Models)
	require.Contains(t, entry.Models, "default", "a live-served model named default must be delivered")
	require.Contains(t, entry.Models, "glm-5.1")
	assert.NotContains(t, entry.Models, "bge-m3", "unlisted fetched models must be filtered out")
	require.NotNil(t, entry.Models["default"].Limit, "per-model limits merged onto the filtered entry")
	assert.Equal(t, 200000, entry.Models["default"].Limit.Context)
	assert.Equal(t, 8192, entry.Models["default"].Limit.Output)
}

// TestE2E_ModelEnricher_AllowlistMatchingNothingYieldsNoModels is the
// unhappy leg: an allowlist naming models the live endpoint does not
// serve must yield a provider entry with NO models (the honest empty
// set — no pod-side synthesis of unverifiable IDs, and the unfiltered
// fetched list must NOT leak through). opencode registers the provider
// from its options block; the degrade is visible, not silent.
func TestE2E_ModelEnricher_AllowlistMatchingNothingYieldsNoModels(t *testing.T) {
	bin := buildAgentdBinary(t)
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "glm-5.1"}},
		})
	}))
	defer srv.Close()

	providerJSON, _ := json.Marshal(map[string]any{
		"kind":           "stale-allowlist",
		"slug":           "stale-allowlist",
		"apiKey":         "sk-stale-test",
		"baseURL":        srv.URL,
		"modelAllowlist": []string{"gone-model", "also-gone"},
	})
	secretsBatch, err := json.Marshal([]map[string]any{{
		"type":      "llm-provider",
		"name":      "stale-allowlist",
		"plaintext": string(providerJSON),
	}})
	require.NoError(t, err)
	secretsPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(secretsPath, secretsBatch, 0o600))

	agentCfg := filepath.Join(dir, "agent-config.json")
	exit, _, stderr := runMaterializeSubcommand(t, bin, secretsPath,
		filepath.Join(dir, "secrets"),
		filepath.Join(dir, ".ssh"),
		agentCfg,
		filepath.Join(dir, "env"),
		filepath.Join(dir, ".git-credentials"))
	require.Equal(t, 0, exit, "materialize must succeed; stderr=%s", stderr)

	raw, err := os.ReadFile(agentCfg)
	require.NoError(t, err, "agent-config.json must exist after materialize; stderr=%s", stderr)

	var cfg struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Contains(t, cfg.Provider, "stale-allowlist",
		"the provider itself must still be delivered (options block intact)")

	var entry struct {
		Options struct {
			APIKey string `json:"apiKey"`
		} `json:"options"`
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(cfg.Provider["stale-allowlist"], &entry))
	assert.Equal(t, "sk-stale-test", entry.Options.APIKey)
	assert.Empty(t, entry.Models,
		"stale allowlist against a live catalog: no models — never synthesis, never the unfiltered fetched list")
}
