// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// agent_config_writer.go implements the SINGLE writer of agent-config.json.
//
// Before US-46.10, four independent code paths wrote agent-config.json:
//   1. FlushProviders (boot + reload) — provider credentials only
//   2. applyWorkspaceConfig (boot subcommand) — adds model key
//   3. startRelayInjector (~T+7s) — merges relay provider + disabled_providers
//   4. reloadSecretsHandler re-merge — restores relay after FlushProviders clobbers it
//
// None coordinated atomically. The design relied on strict boot ordering,
// reloadMu serialization, and opencode not hot-reloading the file. This was
// fragile — a future change that reorders the boot sequence or adds a new
// write path could reintroduce relay clobbering.
//
// The AgentConfigWriter eliminates this fragility by being the sole writer.
// It holds three sources — providers, model, relay — and Rebuild() merges
// them into a complete config written atomically via temp-file + os.Rename.
//
// Boot initialization: NewAgentConfigWriter reads the existing file (written
// by the materialize subcommand) and captures the provider map and model as
// initial sources. This lets the relay injector merge into them without
// re-deriving provider credentials.
//
// The materialize subcommand still writes the file directly (it is a separate
// process before agentd starts). But once agentd is running, ALL writes go
// through the writer. See README-LLM.md "Relay Config Subsystem" for the
// full write-sequence documentation.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	dsecrets "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// relaySource holds the relay URL and free model list that the relay
// injector discovered from opencode's /provider endpoint.
type relaySource struct {
	url    string
	models []relayModel
}

// AgentConfigWriter is the single writer of agent-config.json within the
// agentd process. All config changes (provider credentials, model
// selection, relay injection) go through SetProviders/SetModel/SetRelay
// followed by Rebuild.
//
// Thread-safe: all methods acquire mu. Rebuild serializes the
// read-merge-write cycle so concurrent reloads and relay injection
// cannot interleave.
type AgentConfigWriter struct {
	mu              sync.Mutex
	path            string
	providerRaw     json.RawMessage  // raw "provider" map JSON from FormatOpenCodeConfig; nil = no providers
	model           string           // fully-qualified "providerID/modelID" form; "" = no model
	relay           *relaySource     // nil = relay not yet injected / skipped
	mcpServers      []mcpServerEntry // staged MCP servers from applyMCPServer; nil = none
	adminPrompt     string           // admin-configured system prompt from agentd.AdminPromptPath; "" = none
	agentRaw        json.RawMessage  // existing "agent" config from loadExisting, preserved across rebuilds
	modeRaw         json.RawMessage  // existing "mode" config from loadExisting, preserved across rebuilds
	allowedDirs     []string         // glob patterns from AllowedDirsPath, merged as external_directory allow-rules
	allowedDirsPath string           // path to the allowed-dirs JSON file; defaults to agentd.AllowedDirsPath
}

// newAgentConfigWriter creates the writer and initializes its sources
// from the existing agent-config.json file (written by the materialize
// subcommand at boot). If the file is absent or corrupt, sources start
// empty and the first Rebuild() creates a fresh file.
func newAgentConfigWriter(path string) *AgentConfigWriter {
	w := &AgentConfigWriter{path: path, allowedDirsPath: agentd.AllowedDirsPath}
	w.loadExisting()
	w.loadAdminPrompt()
	w.loadAllowedDirs()
	return w
}

// loadExisting reads the current agent-config.json and captures the
// provider map and model as sources. Called once at construction.
// Silent on error — a missing or corrupt file means the writer starts
// empty, which is correct for zero-credential users.
func (w *AgentConfigWriter) loadExisting() {
	data, err := os.ReadFile(w.path)
	if err != nil || len(data) == 0 {
		return
	}
	var cfg struct {
		Provider json.RawMessage `json:"provider"`
		Model    string          `json:"model,omitempty"`
		Agent    json.RawMessage `json:"agent,omitempty"`
		Mode     json.RawMessage `json:"mode,omitempty"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	w.providerRaw = cfg.Provider
	w.model = cfg.Model
	w.agentRaw = cfg.Agent
	w.modeRaw = cfg.Mode

	// 2026-06-23 cold-start optimization (item #1a, Phase D): detect
	// a pre-boot-injected relay block and set the writer's relay
	// source so hasRelay() returns true. Without this, the legacy
	// in-pod startRelayInjector goroutine would think no relay is
	// configured (writer.relay == nil) and run its full
	// fetch+kill+restart cycle redundantly, defeating the entire
	// point of Phase C.
	//
	// We extract just enough info to satisfy hasRelay() — the actual
	// relay config is already on disk, so we don't need to round-trip
	// the full URL or model list back into the writer's source. A
	// sentinel non-nil relaySource is sufficient, but we populate
	// fields where we can so any future caller that introspects the
	// writer sees consistent state.
	if len(cfg.Provider) > 0 {
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(cfg.Provider, &providers); err == nil {
			if relayRaw, ok := providers["opencode-relay"]; ok {
				w.relay = parseRelayFromExisting(relayRaw)
			}
		}
	}
}

// loadAdminPrompt reads the admin-configured system prompt written by the
// bootstrap subcommand to agentd.AdminPromptPath. Loaded once at init;
// persists across all rebuilds. Changes take effect on next pod boot
// (design decision: no hot-reload).
func (w *AgentConfigWriter) loadAdminPrompt() {
	data, err := os.ReadFile(agentd.AdminPromptPath)
	if err != nil || len(data) == 0 {
		return
	}
	w.adminPrompt = string(data)
}

// setAllowedDirsPath overrides the allowed-dirs file path. Test seam so
// tests can point at a t.TempDir() file instead of the production tmpfs
// path. The constructor already calls loadAllowedDirs() with the default
// path (agentd.AllowedDirsPath); calling this after construction + calling
// loadAllowedDirs() again re-reads from the new path. The second call
// appends to the slice (not replaces) — tests rely on the first call being
// a no-op (the production path doesn't exist in a test environment).
func (w *AgentConfigWriter) setAllowedDirsPath(p string) {
	w.allowedDirsPath = p
}

// loadAllowedDirs reads the instance's allowedExternalDirectories setting
// (written by the bootstrap subcommand as a JSON array of glob patterns)
// and stores the patterns as the external_directory allow-rule source.
// Called once at construction. Silent on error — a missing or corrupt file
// means no allow-rules are injected, which is correct (agents prompt as
// before). Patterns are de-duplicated to keep the rendered config clean.
func (w *AgentConfigWriter) loadAllowedDirs() {
	if w.allowedDirsPath == "" {
		return
	}
	data, err := os.ReadFile(w.allowedDirsPath)
	if err != nil || len(data) == 0 {
		return
	}
	var patterns []string
	if json.Unmarshal(data, &patterns) != nil {
		return
	}
	seen := make(map[string]struct{}, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		w.allowedDirs = append(w.allowedDirs, p)
	}
}

// parseRelayFromExisting extracts URL + models from a pre-injected
// opencode-relay provider block. Used by loadExisting to make the
// writer aware of a relay block written by the materialize subcommand
// (Phase C) before agentd started.
//
// Returns a populated *relaySource on success, or a sentinel
// non-nil source with empty fields if extraction fails — the
// non-nil-ness is what matters for hasRelay().
func parseRelayFromExisting(relayRaw json.RawMessage) *relaySource {
	var entry struct {
		Options struct {
			BaseURL string `json:"baseURL"`
		} `json:"options"`
		Models map[string]struct {
			Name  string `json:"name"`
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	src := &relaySource{}
	if err := json.Unmarshal(relayRaw, &entry); err != nil {
		// Block exists but isn't parseable — still set non-nil
		// sentinel so hasRelay() reports true. Rebuild() will
		// regenerate the block from defaults if anyone calls it.
		return src
	}
	src.url = entry.Options.BaseURL
	for id, m := range entry.Models {
		src.models = append(src.models, relayModel{
			ID:           id,
			Name:         m.Name,
			ContextLimit: m.Limit.Context,
			OutputLimit:  m.Limit.Output,
		})
	}
	return src
}

// setProviders updates the provider source from a FormatOpenCodeConfig
// result. The formatted bytes contain the complete opencode config shape
// ({ $schema, provider: {...} }); this method extracts just the provider
// map. The model from the formatter is NOT captured — the model source
// is owned by applyWorkspaceConfig (set at boot via loadExisting) and
// must survive credential reloads.
func (w *AgentConfigWriter) setProviders(formattedConfig []byte) error {
	var cfg struct {
		Provider json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(formattedConfig, &cfg); err != nil {
		return fmt.Errorf("parse formatted providers: %w", err)
	}
	w.mu.Lock()
	w.providerRaw = cfg.Provider
	w.mu.Unlock()
	return nil
}

// setModel updates the model source. Called by applyWorkspaceConfig
// at boot (via the materialize subcommand) to set the default model
// from workspace-config.json.
func (w *AgentConfigWriter) setModel(model string) {
	w.mu.Lock()
	w.model = model
	w.mu.Unlock()
}

// setRelay updates the relay source after the relay injector successfully
// discovers the free model list. The writer stores the URL and models;
// Rebuild() merges them into the provider map.
func (w *AgentConfigWriter) setRelay(url string, models []relayModel) {
	w.mu.Lock()
	w.relay = &relaySource{url: url, models: models}
	w.mu.Unlock()
}

// mcpServerEntry is one staged MCP server, carrying the fields needed to
// render its opencode config entry (local or remote shape per the contract).
type mcpServerEntry struct {
	Name      string
	Transport string // http, sse, or stdio
	URL       string
	Command   string
	Args      []string
	TimeoutMs int
	Env       map[string]string
	Headers   map[string]string
}

// SetMCPServers replaces the MCP server source and rebuilds. Called after
// materialize stages MCP server entries from secrets.json. Each server
// renders as one entry in the opencode "mcp" top-level config section.
func (w *AgentConfigWriter) SetMCPServers(servers []mcpServerEntry) {
	w.mu.Lock()
	w.mcpServers = servers
	w.mu.Unlock()
}

// hasRelay returns true if the relay injector has successfully injected
// relay config. Used by the readyz handler for the RelayInjected signal
// (replaces the old getActiveRelayModels() != nil check).
func (w *AgentConfigWriter) hasRelay() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.relay != nil
}

// rebuild merges all sources (providers, model, relay) and writes the
// complete agent-config.json atomically via temp-file + os.Rename.
//
// Merge semantics:
//   - $schema is always set to "https://opencode.ai/config.json"
//   - provider map = existing providers (from setProviders or loadExisting)
//   - opencode-relay (if relay is set). No existing provider is removed.
//   - model = the model source (from setModel or loadExisting)
//   - disabled_providers = ["opencode"] (only if relay is set)
//
// The temp-file + rename pattern ensures readers never see a partially
// written file. os.Rename is atomic on POSIX filesystems (same mount).
func (w *AgentConfigWriter) rebuild() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg := make(map[string]json.RawMessage)

	schema, _ := json.Marshal("https://opencode.ai/config.json")
	cfg["$schema"] = schema

	// Build provider map from the provider source.
	providers := make(map[string]json.RawMessage)
	if len(w.providerRaw) > 0 {
		if err := json.Unmarshal(w.providerRaw, &providers); err != nil {
			return fmt.Errorf("agent-config writer: parse provider source: %w", err)
		}
	}

	// Merge relay provider if relay is set.
	if w.relay != nil {
		relayEntry, err := buildRelayProviderEntry(w.relay.url, w.relay.models)
		if err != nil {
			return fmt.Errorf("agent-config writer: build relay provider: %w", err)
		}
		providers["opencode-relay"] = relayEntry

		disabled, _ := json.Marshal([]string{"opencode"})
		cfg["disabled_providers"] = disabled
	}

	if len(providers) > 0 {
		providerJSON, err := json.Marshal(providers)
		if err != nil {
			return fmt.Errorf("agent-config writer: marshal provider map: %w", err)
		}
		cfg["provider"] = providerJSON
	}

	if w.model != "" {
		modelJSON, _ := json.Marshal(w.model)
		cfg["model"] = modelJSON
	}

	// Merge admin prompt into opencode's `agent` config block. Sets
	// agent.build.prompt so opencode injects it as the build agent's
	// system prompt (see AgentConfig at https://opencode.ai/config.json).
	// Existing agent config from loadExisting is preserved; the admin
	// prompt overrides only agent.build.prompt.
	//
	// LLMSafeSpaces#486: this block previously emitted `agents.build.system`,
	// which does not exist in opencode's config schema — the top-level key
	// is `agent` (singular) and the AgentConfig field is `prompt`, not
	// `system`. Every non-empty adminPrompt made opencode reject the
	// config with ConfigInvalidError → all session endpoints 500'd.
	// Enforced by TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema
	// (validates rebuild output against opencode's actual JSON schema).
	if w.adminPrompt != "" || len(w.agentRaw) > 0 {
		agent := make(map[string]json.RawMessage)
		if len(w.agentRaw) > 0 {
			_ = json.Unmarshal(w.agentRaw, &agent)
		}
		if w.adminPrompt != "" {
			// Deep-merge into any existing build agent config so we only
			// override "prompt" and preserve sibling fields (tools, model,
			// mode, temperature, etc.) rather than wholesale-replacing.
			var existingBuild map[string]json.RawMessage
			if raw, ok := agent["build"]; ok {
				_ = json.Unmarshal(raw, &existingBuild)
			}
			if existingBuild == nil {
				existingBuild = map[string]json.RawMessage{}
			}
			promptJSON, _ := json.Marshal(w.adminPrompt)
			existingBuild["prompt"] = promptJSON
			buildJSON, err := json.Marshal(existingBuild)
			if err != nil {
				return fmt.Errorf("agent-config writer: marshal build agent: %w", err)
			}
			agent["build"] = buildJSON
		}
		agentJSON, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("agent-config writer: marshal agent: %w", err)
		}
		cfg["agent"] = agentJSON
	}

	// Merge allowed-external-directories into mode.permissions.external_directory.
	// The instance's allowedExternalDirectories setting (e.g. ["/tmp/*"]) is
	// injected as "allow" rules so opencode stops prompting for paths outside
	// the /workspace project root. The existing mode block is preserved —
	// only the external_directory sub-object gains entries — so sibling
	// permission rules (bash, edit, etc.) and other mode fields survive.
	//
	// The mode block is re-emitted when EITHER we have allowed-dirs to inject
	// OR an existing mode block needs preserving (loadExisting captured it).
	// When allowedDirs is empty and no mode exists, no mode block is emitted
	// (true no-op, no empty-object noise).
	if len(w.allowedDirs) > 0 || len(w.modeRaw) > 0 {
		mode := make(map[string]json.RawMessage)
		if len(w.modeRaw) > 0 {
			_ = json.Unmarshal(w.modeRaw, &mode)
		}

		// Only touch external_directory when we have patterns to inject.
		// Without this guard, an existing mode block with no external_directory
		// key would gain an empty "external_directory": {} — functional but
		// unnecessary noise in the rendered config.
		if len(w.allowedDirs) > 0 {
			var perms map[string]json.RawMessage
			if raw, ok := mode["permissions"]; ok {
				_ = json.Unmarshal(raw, &perms)
			}
			if perms == nil {
				perms = map[string]json.RawMessage{}
			}
			// external_directory may be a bare action string ("ask"/"allow"/
			// "deny") or an object map of {pattern: action} (both valid per
			// opencode's PermissionRuleConfig schema). If it's a bare string,
			// we PRESERVE it as-is — converting to a map would silently narrow
			// a global policy (e.g. "allow" for all dirs → "allow" only for
			// /tmp/*). Only merge our patterns when the value is absent or is
			// already in the map form.
			if raw, ok := perms["external_directory"]; ok {
				var existing map[string]string
				if json.Unmarshal(raw, &existing) == nil {
					for _, p := range w.allowedDirs {
						existing[p] = "allow"
					}
					extDirJSON, err := json.Marshal(existing)
					if err != nil {
						return fmt.Errorf("agent-config writer: marshal external_directory: %w", err)
					}
					perms["external_directory"] = extDirJSON
				}
				// Bare-string branch: preserved as-is, no injection.
			} else {
				extDir := make(map[string]string, len(w.allowedDirs))
				for _, p := range w.allowedDirs {
					extDir[p] = "allow"
				}
				extDirJSON, err := json.Marshal(extDir)
				if err != nil {
					return fmt.Errorf("agent-config writer: marshal external_directory: %w", err)
				}
				perms["external_directory"] = extDirJSON
			}
			permsJSON, err := json.Marshal(perms)
			if err != nil {
				return fmt.Errorf("agent-config writer: marshal permissions: %w", err)
			}
			mode["permissions"] = permsJSON
		}

		modeJSON, err := json.Marshal(mode)
		if err != nil {
			return fmt.Errorf("agent-config writer: marshal mode: %w", err)
		}
		cfg["mode"] = modeJSON
	}

	// Merge MCP servers into the top-level "mcp" section. Each server
	// becomes one named entry. Remote transports (http/sse) render as
	// opencode "remote"; stdio renders as "local". The section is only
	// emitted when at least one server is staged — a no-MCP workspace
	// produces byte-equivalent output to the pre-Epic-53 writer.
	if len(w.mcpServers) > 0 {
		mcp := make(map[string]json.RawMessage, len(w.mcpServers))
		for _, srv := range w.mcpServers {
			// Convert mcpServerEntry → StagedMCPServer → shared renderer.
			// This is the single render path shared with the materialize subcommand.
			staged := dsecrets.StagedMCPServer{
				Name: srv.Name, Transport: srv.Transport, URL: srv.URL,
				Command: srv.Command, Args: srv.Args, TimeoutMs: srv.TimeoutMs,
				Env: srv.Env, Headers: srv.Headers,
			}
			entry := dsecrets.RenderOpencodeMCPServerEntry(staged)
			entryJSON, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("agent-config writer: marshal mcp server %q: %w", srv.Name, err)
			}
			mcp[srv.Name] = entryJSON
		}
		mcpJSON, err := json.Marshal(mcp)
		if err != nil {
			return fmt.Errorf("agent-config writer: marshal mcp section: %w", err)
		}
		cfg["mcp"] = mcpJSON
	}

	output, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("agent-config writer: marshal config: %w", err)
	}

	return atomicRenameWrite(w.path, output, 0o600)
}

// atomicRenameWrite writes data to a temp file in the same directory as
// path, then atomically renames it to path. This ensures readers never
// observe a partially-written file (os.Rename is atomic on POSIX).
//
// The temp file is created in the same directory as the target so the
// rename is guaranteed to be on the same filesystem (rename across
// filesystems fails with EXDEV).
func atomicRenameWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agent-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp to target: %w", err)
	}
	return nil
}

// buildRelayProviderEntry builds the JSON for the opencode-relay provider
// entry that gets merged into the provider map. This is the same logic
// that buildRelayConfig used inline — extracted so the writer can call it
// during Rebuild without reading the existing file.
//
// The relay entry shape:
//
//	{
//	  "name": "OpenCode Zen (Free)",
//	  "npm": "@ai-sdk/openai-compatible",
//	  "options": {"baseURL": "<relayURL>", "apiKey": "public"},
//	  "models": {"<id>": {"name": "...", "limit": {"context": ..., "output": ...}}}
//	}
func buildRelayProviderEntry(relayURL string, models []relayModel) (json.RawMessage, error) {
	type modelLimit struct {
		Context int `json:"context,omitempty"`
		Output  int `json:"output,omitempty"`
	}
	type modelEntry struct {
		Name  string     `json:"name"`
		Limit modelLimit `json:"limit,omitempty"`
	}
	modelMap := make(map[string]modelEntry, len(models))
	for _, m := range models {
		modelMap[m.ID] = modelEntry{
			Name:  m.Name,
			Limit: modelLimit{Context: m.ContextLimit, Output: m.OutputLimit},
		}
	}

	type options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	type provider struct {
		Name    string                `json:"name"`
		NPM     string                `json:"npm"`
		Options options               `json:"options"`
		Models  map[string]modelEntry `json:"models"`
	}

	entry := provider{
		Name: "OpenCode Zen (Free)",
		NPM:  "@ai-sdk/openai-compatible",
		Options: options{
			BaseURL: relayURL,
			APIKey:  "public",
		},
		Models: modelMap,
	}
	return json.Marshal(entry)
}
