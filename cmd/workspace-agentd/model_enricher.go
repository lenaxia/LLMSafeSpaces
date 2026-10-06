// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// model_enricher.go — fetches and caches model lists from custom OpenAI-compatible
// provider endpoints (e.g. ai.thekao.cloud/v1/models) at workspace startup.
//
// Problem: platform credentials (e.g. epic30-openai-1780700420) are stored without
// an explicit model list. When FlushProviders writes the opencode config, the openai
// provider block has no models entry, so opencode falls back to its internal hardcoded
// list of openai.com model IDs — which are wrong for a custom endpoint.
//
// Fix: before FlushProviders, for each staged LLM provider that has a BaseURL but no
// Models, fetch GET {BaseURL}/models (OpenAI-compatible list endpoint), parse the
// response, and populate LLMModelConfig entries. The result is cached to
// {secretsBaseDir}/provider-models-cache-{provider}.json (TTL: providerModelCacheTTL)
// so pod restarts don't repeat the round-trip on every boot.
//
// The fetch is best-effort: on failure (network, auth, non-200), the provider is
// left with an empty model list. OpenCode will still register the provider and use
// its own internal model list as fallback — degraded but not broken.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const providerModelCacheTTL = 24 * time.Hour

// modelListResponse is the OpenAI-compatible GET /models response shape.
type modelListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// enrichProviderModels returns an EnrichProviders transform function that,
// for each staged provider with a non-empty BaseURL and empty Models list,
// fetches the model list from the provider's /models endpoint and populates
// the Models field. Results are cached under cacheDir. When the provider
// carries a delivered model allowlist (#1575 delivery fields), the FETCHED
// list is filtered against it — the fetch is the voucher that decides
// whether a model literally named "default" (or any allowlist ID) is real.
func enrichProviderModels(ctx context.Context, cacheDir string, client *http.Client) func([]sec.LLMProviderData) []sec.LLMProviderData {
	return func(providers []sec.LLMProviderData) []sec.LLMProviderData {
		out := make([]sec.LLMProviderData, len(providers))
		copy(out, providers)
		for i, p := range out {
			if p.BaseURL == "" || len(p.Models) > 0 {
				continue
			}
			// Cache key is per-slug because Slug is the unique identity
			// (Epic 55). Two credentials of the same Kind with different
			// BaseURLs would otherwise share a cache file.
			models, err := fetchOrCacheModels(ctx, p.Slug, p.BaseURL, p.APIKey, cacheDir, client)
			if err != nil {
				log.Warn("model enricher: failed to fetch model list; provider will use opencode default list",
					zap.String("slug", p.Slug),
					zap.String("kind", p.Kind),
					zap.String("baseURL", p.BaseURL),
					zap.Error(err))
				continue
			}
			// #1575: filter AFTER the fetch (and after any cache read —
			// the cache stores the raw list, so an allowlist change
			// re-filters without a refetch). No synthesis here: a live
			// catalog that matches nothing allowed is correctly empty;
			// a fetch failure keeps the degrade above.
			filtered := applyDeliveredAllowlist(models, p.ModelAllowlist, p.ModelContextLimits, p.ModelOutputLimits)
			if len(filtered) < len(models) {
				log.Info("model enricher: applied delivered model allowlist",
					zap.String("slug", p.Slug),
					zap.Int("fetched", len(models)),
					zap.Int("allowed", len(filtered)))
			}
			out[i].Models = filtered
			log.Info("model enricher: populated model list",
				zap.String("slug", p.Slug),
				zap.String("kind", p.Kind),
				zap.Int("count", len(filtered)))
		}
		return out
	}
}

// applyDeliveredAllowlist intersects a fetched catalog with the
// credential's delivered allowlist and merges the per-model limit
// overrides (fetched entries carry none; an explicit entry value would
// win, matching the server-side precedence). An empty allowlist leaves
// the fetched list untouched.
func applyDeliveredAllowlist(models []sec.LLMModelConfig, allowlist []string, contextLimits, outputLimits map[string]int) []sec.LLMModelConfig {
	if len(allowlist) == 0 {
		return models
	}
	allowed := make(map[string]bool, len(allowlist))
	for _, id := range allowlist {
		allowed[id] = true
	}
	filtered := make([]sec.LLMModelConfig, 0, len(models))
	for _, m := range models {
		if !allowed[m.ID] {
			continue
		}
		if m.ContextLimit == 0 {
			m.ContextLimit = contextLimits[m.ID]
		}
		if m.OutputLimit == 0 {
			m.OutputLimit = outputLimits[m.ID]
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// fetchOrCacheModels returns the model list for the given provider, reading
// from cache if present and fresh, otherwise fetching from baseURL/models.
func fetchOrCacheModels(ctx context.Context, provider, baseURL, apiKey, cacheDir string, client *http.Client) ([]sec.LLMModelConfig, error) {
	// Sanitize provider name before using it in a filename: keep only
	// alphanumerics and hyphens. Dots are intentionally excluded because
	// a sequence like ".." in a provider name could survive naive sanitization
	// and produce a directory-traversal filename component. Provider names
	// in practice are simple identifiers (e.g. "openai", "anthropic",
	// "epic30-openai") that never need dots in the cache filename.
	safeName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '_'
	}, provider)

	// Ensure the cache directory exists. It may not on first boot or after a
	// node recycle. MkdirAll is a no-op when the directory already exists.
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		// Non-fatal: proceed without caching.
		log.Warn("model enricher: failed to create cache dir; will skip cache",
			zap.String("cacheDir", cacheDir), zap.Error(err))
		models, fetchErr := fetchModels(ctx, baseURL, apiKey, client)
		return models, fetchErr
	}

	cacheFile := filepath.Join(cacheDir, "provider-models-cache-"+safeName+".json")

	// Use cache if it exists and is within TTL.
	if info, err := os.Stat(cacheFile); err == nil {
		if time.Since(info.ModTime()) < providerModelCacheTTL {
			if cached, err := loadModelCache(cacheFile); err == nil {
				log.Info("model enricher: using cached model list",
					zap.String("provider", provider),
					zap.Int("count", len(cached)))
				return cached, nil
			}
			// Cache file corrupt — fall through to fetch.
		}
	}

	models, err := fetchModels(ctx, baseURL, apiKey, client)
	if err != nil {
		return nil, err
	}

	// Write cache best-effort. Failure is non-fatal — the pod still gets
	// the fresh model list; next restart will re-fetch.
	if data, err := json.Marshal(models); err == nil {
		_ = os.WriteFile(cacheFile, data, 0o600)
	}

	return models, nil
}

// fetchModels calls GET {baseURL}/models and returns parsed LLMModelConfig entries.
// It expects an OpenAI-compatible response: {"data": [{"id": "model-name"}, ...]}.
func fetchModels(ctx context.Context, baseURL, apiKey string, client *http.Client) ([]sec.LLMModelConfig, error) {
	url := baseURL
	if len(url) > 0 && url[len(url)-1] == '/' {
		url = url[:len(url)-1]
	}
	url += "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, string(body))
	}

	var mlr modelListResponse
	if err := json.NewDecoder(resp.Body).Decode(&mlr); err != nil {
		return nil, fmt.Errorf("decode model list from %s: %w", url, err)
	}

	models := make([]sec.LLMModelConfig, 0, len(mlr.Data))
	for _, m := range mlr.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, sec.LLMModelConfig{ID: m.ID})
	}
	return models, nil
}

// loadModelCache reads and parses a cached model list from disk.
func loadModelCache(path string) ([]sec.LLMModelConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var models []sec.LLMModelConfig
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("parse model cache %s: %w", path, err)
	}
	return models, nil
}
