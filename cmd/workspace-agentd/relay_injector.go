// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_injector.go implements the two-phase relay config injection for the
// self-hosted InferenceRelay fleet (Epic 42).
//
// After opencode boots with its default config (Phase 1), this module:
//   1. Checks whether the user has a personal opencode API key — if yes, skips
//      the relay entirely and lets opencode call opencode.ai/zen/v1 directly.
//   2. Calls GET /provider on the running opencode server to get the live
//      free model list (providerID in connected[], cost.input == 0).
//   3. MERGES a new provider block into the existing agent-config.json:
//        - disabled_providers: ["opencode"] — removes the built-in provider
//        - provider.opencode-relay — custom OpenAI-compatible provider pointing
//          at the relay fleet router with the free model list. Any other
//          providers already in the file (e.g. openai written by the init
//          container via the platform credential) are preserved unchanged.
//   4. Writes the opencode-relay auth entry to auth.json (preserving existing
//      paid provider entries from llm-provider secrets).
//   5. Kills the opencode process — the agentd supervisor restarts it and
//      opencode reads the merged config on boot.
//
// The injection is gated by a one-shot flag so it runs exactly once per pod
// lifetime. On subsequent opencode restarts (crash recovery), agentd does NOT
// overwrite the config.
//
// Bypass condition:
//   If auth.json contains an "opencode" entry with key != "public", the user
//   has a personal opencode Zen API key. In that case the relay is bypassed
//   and opencode routes to opencode.ai/zen/v1 using the personal key directly.
//   This is the correct behavior for paying Zen subscribers.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// relayURLHost returns only the scheme+host of a relay URL so it can be
// safely logged without exposing any path-segment token
// (e.g. "https://relay.example.test/path" → "https://relay.example.test").
func relayURLHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid-url]"
	}
	return u.Scheme + "://" + u.Host
}

var relayInjectorOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llmsafespaces_relay_injector_total",
	Help: "Phase-2 relay injector outcomes per agentd pod boot.",
}, []string{"outcome"})

// relayModel is the minimal model info needed to build the custom provider config.
type relayModel struct {
	ID           string
	Name         string
	ContextLimit int
	OutputLimit  int
}

// shouldSkipRelay reads auth.json at authPath and returns (true, reason) if
// relay injection should be skipped because the user has a personal opencode
// API key. Returns (false, "") if relay should proceed.
//
// The check: auth.json["opencode"]["key"] exists and is not "public".
// "public" is the default anonymous key used for free-tier access. Any other
// value indicates a personal paid key — in that case opencode routes directly.
func shouldSkipRelay(authJSONPath string) (bool, string) {
	data, err := os.ReadFile(authJSONPath)
	if err != nil {
		return false, "" // absent = fresh pod, proceed with relay
	}

	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		return false, ""
	}

	ocRaw, ok := auth["opencode"]
	if !ok {
		return false, ""
	}

	var entry struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(ocRaw, &entry); err != nil {
		return false, ""
	}

	if entry.Key != "" && entry.Key != "public" {
		return true, "personal opencode API key configured — relay bypassed, using key directly"
	}
	return false, ""
}

// fetchFreeModels calls GET /provider on the opencode server at baseURL,
// authenticating with the given password, and returns models that are:
//   - providerID == "opencode" (the built-in opencode free-tier provider)
//   - "opencode" is in the connected[] list (credentials are live)
//   - cost.input == 0  (free tier)
//
// The /provider response has shape {all:[{id, models:{id:{cost:{input,output},...}}},...], connected:[]}.
// all[] contains every provider from models.dev regardless of auth;
// connected[] is the subset we actually have credentials for.
// We must use connected[] to distinguish accessible models from catalog noise.
func fetchFreeModels(ctx context.Context, baseURL, password string) ([]relayModel, error) {
	url := baseURL + "/provider"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G107: internal pod URL
	if err != nil {
		return nil, fmt.Errorf("build GET /provider request: %w", err)
	}
	req.SetBasicAuth("opencode", password)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /provider: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /provider returned %d: %s", resp.StatusCode, body)
	}

	var providerResp struct {
		Connected []string `json:"connected"`
		All       []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Cost struct {
					Input  float64 `json:"input"`
					Output float64 `json:"output"`
				} `json:"cost"`
				Limit struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"all"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&providerResp); err != nil {
		return nil, fmt.Errorf("decode /provider: %w", err)
	}

	// Build a set of connected provider IDs for O(1) lookup.
	connectedSet := make(map[string]bool, len(providerResp.Connected))
	for _, id := range providerResp.Connected {
		connectedSet[id] = true
	}

	// Only relay opencode free-tier models. If opencode is not connected
	// (no public key yet), return empty — the caller will retry.
	if !connectedSet["opencode"] {
		return nil, nil
	}

	var free []relayModel
	for _, p := range providerResp.All {
		if p.ID != "opencode" {
			continue
		}
		for modelKey, m := range p.Models {
			if m.Cost.Input != 0 {
				continue
			}
			id := m.ID
			if id == "" {
				id = modelKey // /provider uses the map key as the model ID
			}
			free = append(free, relayModel{
				ID:           id,
				Name:         m.Name,
				ContextLimit: m.Limit.Context,
				OutputLimit:  m.Limit.Output,
			})
		}
		break
	}
	return free, nil
}

// updateAuthJSONForRelay reads auth.json at authPath, adds an "opencode-relay"
// entry with key="public", and writes it back. Existing entries (including paid
// provider keys) are preserved. If the file doesn't exist, it is created.
func updateAuthJSONForRelay(authJSONPath string) error {
	var auth map[string]json.RawMessage

	data, err := os.ReadFile(authJSONPath)
	if err == nil && len(data) > 0 {
		if jsonErr := json.Unmarshal(data, &auth); jsonErr != nil {
			auth = nil
		}
	}
	if auth == nil {
		auth = make(map[string]json.RawMessage)
	}

	entry, _ := json.Marshal(map[string]string{"type": "api", "key": "public"})
	auth["opencode-relay"] = entry

	updated, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth.json: %w", err)
	}
	return os.WriteFile(authJSONPath, updated, 0o600)
}

// relayInjectorConfig holds the parameters for startRelayInjector.
type relayInjectorConfig struct {
	// RelayURL is the self-hosted relay fleet URL the controller injected
	// via INFERENCE_RELAY_BASEURL (typically the in-cluster relay-router
	// FQDN). Empty → no-op (direct-to-Zen mode).
	RelayURL string
	// OpenCodeBaseURL is the http://localhost:PORT base for opencode API calls.
	OpenCodeBaseURL string
	// OpenCodePassword is the Basic auth password for opencode.
	OpenCodePassword string
	// AgentConfigPath is the path to write agent-config.json.
	AgentConfigPath string
	// AuthJSONPath is the path to opencode's auth.json.
	AuthJSONPath string
	// AgentConfigWriter is the single writer of agent-config.json. The
	// injector calls SetRelay + Rebuild to merge the relay provider block
	// into the existing config. Required when RelayURL is set.
	AgentConfigWriter *AgentConfigWriter
	// KillOpenCode is called to trigger opencode process restart after config
	// is written. The supervisor restarts opencode, which reads the new config.
	KillOpenCode func()
	// HealthCheck returns true when opencode is healthy and ready to serve API calls.
	HealthCheck func() bool
}

// startRelayInjector starts a background goroutine that waits for opencode to
// be healthy, then applies the relay config (Phase 2 injection). It runs at
// most once per pod lifetime.
//
// If INFERENCE_RELAY_BASEURL is not set or the user has a personal opencode
// API key, the goroutine exits without making any changes.
func startRelayInjector(ctx context.Context, cfg relayInjectorConfig) {
	if cfg.RelayURL == "" {
		return
	}
	// 2026-06-23 cold-start optimization (item #1a, Phase D):
	// short-circuit if the materialize subcommand has already
	// pre-injected the relay block via the cluster-wide free-models
	// ConfigMap. AgentConfigWriter.hasRelay() is true when
	// loadExisting found a populated `provider.opencode-relay`
	// block at agentd startup (i.e. Phases A+B+C all succeeded).
	//
	// In that case the legacy fetch+kill+restart path would be a
	// pure waste — opencode is already booting (or booted) with
	// the correct config. Save the ~6-8s cycle.
	//
	// If pre-boot injection skipped (no CM, empty catalog, refresher
	// disabled, etc.), hasRelay() is false and we fall through to
	// the legacy path, preserving correctness on every cluster
	// regardless of whether the optimization is wired up.
	if cfg.AgentConfigWriter != nil && cfg.AgentConfigWriter.hasRelay() {
		log.Info("relay injector: pre-boot relay already applied; skipping in-pod injection")
		relayInjectorOutcomes.WithLabelValues("skipped_pre_boot_applied").Inc()
		return
	}
	// Capture the logger at call-site so the goroutine does not race with
	// test code that reassigns the package-level log variable.
	lg := log
	go func() {
		// Wait up to 5 minutes for opencode to be healthy.
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if cfg.HealthCheck() {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		if !cfg.HealthCheck() {
			lg.Warn("relay injector: opencode did not become healthy in time, skipping relay config")
			relayInjectorOutcomes.WithLabelValues("unhealthy_timeout").Inc()
			return
		}

		// Check whether to skip relay.
		if skip, reason := shouldSkipRelay(cfg.AuthJSONPath); skip {
			lg.Info("relay injector: skipping relay injection", zap.String("reason", reason))
			relayInjectorOutcomes.WithLabelValues("skipped_personal_key").Inc()
			return
		}
		if cfg.AgentConfigWriter == nil {
			lg.Warn("relay injector: AgentConfigWriter is nil, skipping relay injection")
			return
		}

		// Fetch the live free model list from the running opencode.
		// Retry for up to 30s if the catalog returns no free models — this
		// handles the race where the relay injector runs before opencode's
		// provider catalog is fully initialized (~16s after startup). Without
		// the retry, a 0-model response permanently skips relay injection for
		// the pod's lifetime, leaving free-tier users with no working models.
		var models []relayModel
		fetchDeadline := time.Now().Add(30 * time.Second)
		for {
			var fetchErr error
			models, fetchErr = fetchFreeModels(ctx, cfg.OpenCodeBaseURL, cfg.OpenCodePassword)
			if fetchErr != nil {
				lg.Warn("relay injector: failed to fetch free models, skipping", zap.Error(fetchErr))
				return
			}
			if len(models) > 0 {
				break
			}
			if time.Now().After(fetchDeadline) {
				lg.Warn("relay injector: no free opencode models found after 30s wait, skipping relay config")
				relayInjectorOutcomes.WithLabelValues("no_free_models").Inc()
				return
			}
			lg.Info("relay injector: no free models yet (catalog still initializing), retrying in 5s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		lg.Info("relay injector: fetched free models", zap.Int("count", len(models)))

		// Build and write the relay config via the single AgentConfigWriter.
		// The writer merges the relay provider block into the existing
		// config (providers + model) and writes atomically (temp + rename).
		cfg.AgentConfigWriter.setRelay(cfg.RelayURL, models)
		if err := cfg.AgentConfigWriter.rebuild(); err != nil {
			lg.Warn("relay injector: failed to write agent config", zap.Error(err))
			relayInjectorOutcomes.WithLabelValues("config_write_failed").Inc()
			return
		}
		lg.Info("relay injector: wrote relay config",
			zap.String("path", cfg.AgentConfigPath),
			zap.Int("models", len(models)),
			zap.String("relayHost", relayURLHost(cfg.RelayURL)))

		// Update auth.json with the opencode-relay entry.
		if err := updateAuthJSONForRelay(cfg.AuthJSONPath); err != nil {
			lg.Warn("relay injector: failed to update auth.json", zap.Error(err))
			relayInjectorOutcomes.WithLabelValues("auth_write_failed").Inc()
			return
		}
		lg.Info("relay injector: updated auth.json with opencode-relay entry")

		// Kill opencode — the supervisor restarts it and reads the new config.
		// The relay state is already stored in the AgentConfigWriter (set above
		// via SetRelay), so reloadSecretsHandler's Rebuild() will preserve it.
		cfg.KillOpenCode()
		relayInjectorOutcomes.WithLabelValues("success").Inc()
		lg.Info("relay injector: triggered opencode restart to apply relay config")
	}()
}
