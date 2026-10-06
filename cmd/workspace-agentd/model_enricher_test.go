// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeModelsServer returns an httptest.Server that serves a static
// OpenAI-compatible /models response.
func fakeModelsServer(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		type entry struct {
			ID string `json:"id"`
		}
		type resp struct {
			Data []entry `json:"data"`
		}
		entries := make([]entry, len(models))
		for i, m := range models {
			entries[i] = entry{ID: m}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp{Data: entries})
	}))
}

// --- fetchModels ---

func TestFetchModels_ParsesIDsFromData(t *testing.T) {
	srv := fakeModelsServer(t, []string{"gpt-5.4", "deepseek-v3-chat", "glm-5.1"})
	defer srv.Close()

	models, err := fetchModels(context.Background(), srv.URL, "test-key", srv.Client())
	require.NoError(t, err)
	require.Len(t, models, 3)
	assert.Equal(t, "gpt-5.4", models[0].ID)
	assert.Equal(t, "deepseek-v3-chat", models[1].ID)
	assert.Equal(t, "glm-5.1", models[2].ID)
}

func TestFetchModels_SkipsEmptyIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4"},{"id":""},{"id":"deepseek-v3-chat"}]}`))
	}))
	defer srv.Close()

	models, err := fetchModels(context.Background(), srv.URL, "any-key", srv.Client())
	require.NoError(t, err)
	assert.Len(t, models, 2, "empty IDs must be skipped")
	assert.Equal(t, "gpt-5.4", models[0].ID)
	assert.Equal(t, "deepseek-v3-chat", models[1].ID)
}

func TestFetchModels_ReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	_, err := fetchModels(context.Background(), srv.URL, "bad-key", srv.Client())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestFetchModels_TrailingSlashInBaseURL(t *testing.T) {
	srv := fakeModelsServer(t, []string{"model-a"})
	defer srv.Close()

	// BaseURL with trailing slash should still produce correct /models path.
	models, err := fetchModels(context.Background(), srv.URL+"/", "test-key", srv.Client())
	require.NoError(t, err)
	assert.Len(t, models, 1)
}

// --- fetchOrCacheModels ---

func TestFetchOrCacheModels_WritesCache(t *testing.T) {
	srv := fakeModelsServer(t, []string{"gpt-5.4", "deepseek-v3-chat"})
	defer srv.Close()

	dir := t.TempDir()
	models, err := fetchOrCacheModels(context.Background(), "openai", srv.URL, "test-key", dir, srv.Client())
	require.NoError(t, err)
	assert.Len(t, models, 2)

	// Cache file must exist.
	cacheFile := filepath.Join(dir, "provider-models-cache-openai.json")
	assert.FileExists(t, cacheFile)
}

func TestFetchOrCacheModels_ReadsCacheThatIsFresh(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "provider-models-cache-openai.json")
	cached := []sec.LLMModelConfig{{ID: "cached-model"}}
	data, _ := json.Marshal(cached)
	require.NoError(t, os.WriteFile(cacheFile, data, 0o600))

	// Server must NOT be called — cache is fresh.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	models, err := fetchOrCacheModels(context.Background(), "openai", srv.URL, "test-key", dir, srv.Client())
	require.NoError(t, err)
	assert.Equal(t, 0, callCount, "server must not be called for a fresh cache")
	require.Len(t, models, 1)
	assert.Equal(t, "cached-model", models[0].ID)
}

func TestFetchOrCacheModels_RefetchesExpiredCache(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "provider-models-cache-openai.json")
	cached := []sec.LLMModelConfig{{ID: "stale-model"}}
	data, _ := json.Marshal(cached)
	require.NoError(t, os.WriteFile(cacheFile, data, 0o600))

	// Back-date the cache file beyond the TTL.
	staleTime := time.Now().Add(-(providerModelCacheTTL + time.Minute))
	require.NoError(t, os.Chtimes(cacheFile, staleTime, staleTime))

	srv := fakeModelsServer(t, []string{"fresh-model"})
	defer srv.Close()

	models, err := fetchOrCacheModels(context.Background(), "openai", srv.URL, "test-key", dir, srv.Client())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "fresh-model", models[0].ID, "stale cache must be refreshed")
}

func TestFetchOrCacheModels_CorruptCacheForcesRefetch(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "provider-models-cache-openai.json")
	require.NoError(t, os.WriteFile(cacheFile, []byte("{not valid json"), 0o600))

	srv := fakeModelsServer(t, []string{"fresh-model"})
	defer srv.Close()

	models, err := fetchOrCacheModels(context.Background(), "openai", srv.URL, "test-key", dir, srv.Client())
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "fresh-model", models[0].ID)
}

func TestFetchOrCacheModels_SanitizesProviderNameInCacheFilepath(t *testing.T) {
	// A provider name with path-traversal characters must produce a safe cache
	// filename — the sanitized name must stay within cacheDir.
	dir := t.TempDir()

	srv := fakeModelsServer(t, []string{"model-x"})
	defer srv.Close()

	// Provider name with ../ path traversal attempt
	_, err := fetchOrCacheModels(context.Background(), "../../evil", srv.URL, "test-key", dir, srv.Client())
	require.NoError(t, err)

	// The cache file must be inside dir, not escaped via traversal.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, strings.HasPrefix(entries[0].Name(), "provider-models-cache-"),
		"cache filename must start with provider-models-cache-")
	assert.NotContains(t, entries[0].Name(), "..", "sanitized filename must not contain ..")
}

// --- enrichProviderModels ---

func TestEnrichProviderModels_PopulatesModelsFromEndpoint(t *testing.T) {
	srv := fakeModelsServer(t, []string{"gpt-5.4", "deepseek-v3-chat", "glm-5.1"})
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "test-key", BaseURL: srv.URL, Models: nil},
	}

	fn := enrichProviderModels(context.Background(), dir, srv.Client())
	out := fn(providers)

	require.Len(t, out, 1)
	assert.Len(t, out[0].Models, 3)
	assert.Equal(t, "gpt-5.4", out[0].Models[0].ID)
}

func TestEnrichProviderModels_SkipsProviderWithNoBaseURL(t *testing.T) {
	dir := t.TempDir()
	providers := []sec.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "test-key", BaseURL: "", Models: nil},
	}

	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		callCount++
		return nil, nil
	})}

	fn := enrichProviderModels(context.Background(), dir, client)
	out := fn(providers)

	assert.Equal(t, 0, callCount, "no HTTP call for provider without BaseURL")
	assert.Empty(t, out[0].Models)
}

func TestEnrichProviderModels_SkipsProviderWithExistingModels(t *testing.T) {
	dir := t.TempDir()
	existingModels := []sec.LLMModelConfig{{ID: "allow-listed-model"}}
	providers := []sec.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "test-key", BaseURL: "https://example.com/v1", Models: existingModels},
	}

	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		callCount++
		return nil, nil
	})}

	fn := enrichProviderModels(context.Background(), dir, client)
	out := fn(providers)

	assert.Equal(t, 0, callCount, "no HTTP call when models are already populated")
	assert.Equal(t, existingModels, out[0].Models, "existing models must be preserved")
}

func TestEnrichProviderModels_FetchFailIsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "test-key", BaseURL: srv.URL, Models: nil},
	}

	fn := enrichProviderModels(context.Background(), dir, srv.Client())
	out := fn(providers)

	// Fetch failure must NOT be fatal — provider is returned with empty Models.
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Models, "failed fetch must leave Models empty (best-effort)")
}

func TestEnrichProviderModels_MultipleProviders(t *testing.T) {
	// Each provider has its own server to avoid conflating auth headers.
	type entry struct {
		ID string `json:"id"`
	}
	type resp struct {
		Data []entry `json:"data"`
	}
	makeSrv := func(models []string, expectedKey string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer "+expectedKey, r.Header.Get("Authorization"))
			entries := make([]entry, len(models))
			for i, m := range models {
				entries[i] = entry{ID: m}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp{Data: entries})
		}))
	}
	srv1 := makeSrv([]string{"model-a", "model-b"}, "key1")
	defer srv1.Close()
	srv2 := makeSrv([]string{"model-c"}, "key2")
	defer srv2.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{
		{Kind: "provider1", Slug: "provider1", APIKey: "key1", BaseURL: srv1.URL},
		{Kind: "provider2", Slug: "provider2", APIKey: "key2", BaseURL: srv2.URL},
		{Kind: "provider3", Slug: "provider3", APIKey: "key3", BaseURL: ""}, // no BaseURL — skipped
	}

	// Use srv1's client (both are plain httptest servers — same transport).
	fn := enrichProviderModels(context.Background(), dir, srv1.Client())
	out := fn(providers)

	assert.Len(t, out[0].Models, 2)
	assert.Len(t, out[1].Models, 1)
	assert.Empty(t, out[2].Models)
}

// roundTripFunc is a helper to build a custom http.RoundTripper from a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestEnrichProviderModels_CacheWrittenToEnricherCacheDir verifies that the
// enricher cache file lands in the configured enricherCacheDir and NOT in
// secretsBaseDir. This is the regression test for Bug 2: the cache was
// previously written to SecretsBasePath which reset() deletes on every reload.
//
// If someone changes enrichProviderModels callers back to cfg.secretsBaseDir,
// this test will catch it: secretsBaseDir is separate from enricherCacheDir
// in the test and we assert the cache is absent from secretsBaseDir.
func TestEnrichProviderModels_CacheWrittenToEnricherCacheDir(t *testing.T) {
	srv := fakeModelsServer(t, []string{"glm-5.1", "deepseek-v4-flash"})
	defer srv.Close()

	dir := t.TempDir()
	secretsBaseDir := filepath.Join(dir, "secrets-base")     // simulates /home/sandbox/.secrets
	enricherCacheDir := filepath.Join(dir, "enricher-cache") // simulates /home/sandbox/.local/state/llmsafespaces
	require.NoError(t, os.MkdirAll(secretsBaseDir, 0o700))
	// enricherCacheDir intentionally NOT pre-created — MkdirAll inside fetchOrCacheModels must create it.

	providers := []sec.LLMProviderData{
		{Kind: "thekao", Slug: "thekao", APIKey: "test-key", BaseURL: srv.URL},
	}

	fn := enrichProviderModels(context.Background(), enricherCacheDir, srv.Client())
	out := fn(providers)

	require.Len(t, out[0].Models, 2, "enricher must populate model list from endpoint")
	assert.Equal(t, "glm-5.1", out[0].Models[0].ID)

	// Cache file must exist in enricherCacheDir.
	entries, err := os.ReadDir(enricherCacheDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one cache file must be written to enricherCacheDir")
	assert.Contains(t, entries[0].Name(), "provider-models-cache-thekao")

	// Cache file must NOT exist in secretsBaseDir.
	// If this fails, enricherCacheDir was not used — the fix regressed.
	secretsEntries, err := os.ReadDir(secretsBaseDir)
	require.NoError(t, err)
	for _, e := range secretsEntries {
		assert.NotContains(t, e.Name(), "provider-models-cache",
			"cache file must not be written to secretsBaseDir (reset() deletes it on every reload)")
	}
}

// TestEnrichProviderModels_PreservesContextLimitOnExistingModels verifies that
// when a provider's Models list is already populated (e.g. from the user-configured
// credential secret with explicit ContextLimit values), the enricher leaves it
// untouched — no HTTP call, ContextLimit values preserved.
//
// This is the normal path for personal-key providers where the workspace owner
// explicitly sets ContextLimit in their credential to fix the "Unknown" denominator
// in the context usage bar. The enricher must not overwrite those values with a
// fresh fetch (which would return ContextLimit=0 since /v1/models doesn't include it).
func TestEnrichProviderModels_PreservesContextLimitOnExistingModels(t *testing.T) {
	dir := t.TempDir()

	existingModels := []sec.LLMModelConfig{
		{ID: "glm-5.1", ContextLimit: 200000},
		{ID: "glm-5.2", Label: "GLM 5.2", ContextLimit: 200000},
	}
	providers := []sec.LLMProviderData{
		{
			Kind:    "thekao cloud",
			Slug:    "thekao cloud",
			APIKey:  "sk-test",
			BaseURL: "https://ai.thekao.cloud/v1",
			Models:  existingModels,
		},
	}

	callCount := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		callCount++
		return nil, nil
	})}

	fn := enrichProviderModels(context.Background(), dir, client)
	out := fn(providers)

	assert.Equal(t, 0, callCount, "enricher must not call /v1/models when Models is already set")

	require.Len(t, out[0].Models, 2)
	assert.Equal(t, "glm-5.1", out[0].Models[0].ID)
	assert.Equal(t, 200000, out[0].Models[0].ContextLimit,
		"ContextLimit must be preserved — enricher must not zero it out")
	assert.Equal(t, "glm-5.2", out[0].Models[1].ID)
	assert.Equal(t, 200000, out[0].Models[1].ContextLimit)
	assert.Equal(t, "GLM 5.2", out[0].Models[1].Label)
}

// TestEnrichProviderModels_FetchedModels_HaveZeroContextLimit documents that
// models discovered by the enricher from /v1/models always have ContextLimit=0
// because the OpenAI-compatible /v1/models endpoint does not return context
// window data. This is expected and correct — such models will show "Unknown"
// as the denominator in the context usage bar unless the user later configures
// an explicit ContextLimit in their credential secret.
func TestEnrichProviderModels_FetchedModels_HaveZeroContextLimit(t *testing.T) {
	srv := fakeModelsServer(t, []string{"glm-5.1", "glm-5.2", "deepseek-v4-flash"})
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{
		{Kind: "thekao cloud", Slug: "thekao cloud", APIKey: "test-key", BaseURL: srv.URL},
	}

	fn := enrichProviderModels(context.Background(), dir, srv.Client())
	out := fn(providers)

	require.Len(t, out[0].Models, 3)
	for _, m := range out[0].Models {
		assert.Equal(t, 0, m.ContextLimit,
			"enricher-discovered models must have ContextLimit=0 — /v1/models does not return context window sizes. "+
				"Users must configure ContextLimit explicitly in their credential secret to fix the 'Unknown' denominator.")
	}
}

// --- #1575: the delivered allowlist filters the FETCHED catalog ---

// The production delivery shape for custom endpoints: the batch carries
// the allowlist (delivery fields), the enricher fetches the live catalog
// and must intersect — "default" is delivered iff the provider actually
// serves it, with the per-model limits merged (fetched entries carry
// none).
func TestEnrichProviderModels_AppliesDeliveredAllowlistPostFetch(t *testing.T) {
	srv := fakeModelsServer(t, []string{"default", "glm-5.1", "bge-m3"})
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{{
		Kind: "openai_compatible", Slug: "thekaocloud", APIKey: "test-key", BaseURL: srv.URL,
		ModelAllowlist:     []string{"default", "glm-5.1"},
		ModelContextLimits: map[string]int{"default": 200000},
		ModelOutputLimits:  map[string]int{"default": 8192},
	}}

	out := enrichProviderModels(context.Background(), dir, srv.Client())(providers)

	require.Len(t, out[0].Models, 2, "fetched [default glm-5.1 bge-m3] filtered to the allowlist")
	ids := []string{out[0].Models[0].ID, out[0].Models[1].ID}
	assert.ElementsMatch(t, []string{"default", "glm-5.1"}, ids,
		"a live-served model named default must survive the filter — it is a real catalog entry")
	for _, m := range out[0].Models {
		if m.ID == "default" {
			assert.Equal(t, 200000, m.ContextLimit, "per-model context limit merged from the delivered map")
			assert.Equal(t, 8192, m.OutputLimit, "per-model output limit merged from the delivered map")
		}
	}
}

// The adjacent bug (#1575 review): a default-only allowlist previously
// synthesized an empty model list server-side, the empty list let the
// fetch fire, and NOTHING filtered the result — the full unfiltered
// catalog bypassed the allowlist. Post-fetch filtering restricts the
// fetched list to exactly the allowlist.
func TestEnrichProviderModels_AllowlistRestrictsFetchedCatalog(t *testing.T) {
	srv := fakeModelsServer(t, []string{"default", "glm-5.1", "bge-m3"})
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{{
		Kind: "openai_compatible", Slug: "router", APIKey: "test-key", BaseURL: srv.URL,
		ModelAllowlist: []string{"default"},
	}}

	out := enrichProviderModels(context.Background(), dir, srv.Client())(providers)

	require.Len(t, out[0].Models, 1, "the fetched catalog must be restricted to the allowlist")
	assert.Equal(t, "default", out[0].Models[0].ID)
}

// An allowlist naming models the provider does not serve yields an empty
// list against a live catalog (correct: stale ids must not be
// synthesized — that was the original 0-providers bug).
func TestEnrichProviderModels_AllowlistWithStaleIDsFetchedListWins(t *testing.T) {
	srv := fakeModelsServer(t, []string{"glm-5.1"})
	defer srv.Close()

	dir := t.TempDir()
	providers := []sec.LLMProviderData{{
		Kind: "openai_compatible", Slug: "router", APIKey: "test-key", BaseURL: srv.URL,
		ModelAllowlist: []string{"gone-model", "also-gone"},
	}}

	out := enrichProviderModels(context.Background(), dir, srv.Client())(providers)

	assert.Empty(t, out[0].Models,
		"no pod-side synthesis: a live catalog that matches nothing allowed is correctly empty")
}

// The cache stores the RAW fetched list; the filter applies after the
// cache read — an allowlist change between calls re-filters without a
// refetch.
func TestEnrichProviderModels_CacheServesRawFilterAppliesAfter(t *testing.T) {
	srv := fakeModelsServer(t, []string{"default", "glm-5.1", "bge-m3"})
	defer srv.Close()

	dir := t.TempDir()
	first := []sec.LLMProviderData{{
		Kind: "openai_compatible", Slug: "thekaocloud", APIKey: "test-key", BaseURL: srv.URL,
		ModelAllowlist: []string{"default"},
	}}
	out1 := enrichProviderModels(context.Background(), dir, srv.Client())(first)
	require.Len(t, out1[0].Models, 1)

	// Same provider (cache hit), a different allowlist.
	second := []sec.LLMProviderData{{
		Kind: "openai_compatible", Slug: "thekaocloud", APIKey: "test-key", BaseURL: srv.URL,
		ModelAllowlist: []string{"glm-5.1", "bge-m3"},
	}}
	out2 := enrichProviderModels(context.Background(), dir, srv.Client())(second)
	require.Len(t, out2[0].Models, 2)
	ids := []string{out2[0].Models[0].ID, out2[0].Models[1].ID}
	assert.ElementsMatch(t, []string{"glm-5.1", "bge-m3"}, ids,
		"the cached raw list must re-filter under the new allowlist without a refetch")
}
