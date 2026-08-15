// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package opencode owns the opencode agent runtime integration. This file
// implements the SINGLE writer of opencode's agent-config.json.
//
// Containment (Epic 65 / Rule 12): every byte of opencode config-shape
// knowledge — the $schema URL, the "provider" map, the "opencode-relay"
// relay block, "disabled_providers", the "agent.build.prompt" deep-merge,
// the "mode.permissions.external_directory" merge, the "mcp" section —
// lives here, behind the pkg/agent/opencode/ seam. Platform code
// (cmd/workspace-agentd) constructs a ConfigWriter via NewConfigWriter
// and calls the exported setters; it does not know what the rendered
// JSON looks like.
//
// Before US-46.10, four independent code paths wrote agent-config.json
// with no coordination. AgentConfigWriter (now ConfigWriter) eliminated
// that fragility by being the sole writer: it holds independent sources
// and Rebuild merges them into a complete config written atomically via
// temp-file + os.Rename. This file was relocated from
// cmd/workspace-agentd/agent_config_writer.go (package main) to here so
// the opencode config knowledge is importable, independently testable,
// and behind the existing boundary. LLMSafeSpaces#486's schema-validation
// harness travels with it (configwriter_schema_test.go).

package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	dsecrets "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// RelayModel is one free-tier model discovered from opencode's /provider
// endpoint by the relay injector. The writer renders it into the
// opencode-relay provider block's "models" map.
type RelayModel struct {
	ID           string
	Name         string
	ContextLimit int
	OutputLimit  int
}

// MCPServerEntry is one staged MCP server, carrying the fields needed to
// render its opencode config entry (local or remote shape per the contract).
type MCPServerEntry struct {
	Name      string
	Transport string // http, sse, or stdio
	URL       string
	Command   string
	Args      []string
	TimeoutMs int
	Env       map[string]string
	Headers   map[string]string
}

// relaySource holds the relay URL and free model list that the relay
// injector discovered from opencode's /provider endpoint.
type relaySource struct {
	url    string
	models []RelayModel
}

// ConfigWriterOption configures a ConfigWriter at construction.
type ConfigWriterOption func(*ConfigWriter)

// WithAdminPromptPath sets the path to the admin-configured system prompt
// file (written by the agentd bootstrap subcommand). The writer reads it
// once at construction. Empty (the default) means no admin-prompt source.
func WithAdminPromptPath(p string) ConfigWriterOption {
	return func(w *ConfigWriter) { w.adminPromptPath = p }
}

// WithAllowedDirsPath sets the path to the instance's
// allowedExternalDirectories JSON array (written by the bootstrap
// subcommand). The writer reads it once at construction. Empty (the
// default) means no external-directory allow-rules are injected.
func WithAllowedDirsPath(p string) ConfigWriterOption {
	return func(w *ConfigWriter) { w.allowedDirsPath = p }
}

// WithPreMarshalHook registers a function invoked on the rendered config
// map immediately before final marshal. agentd uses this to inject its
// built-in admin MCP server (the "llmsafespaces" entry pointing at
// agentd's own admin port) without this package needing to know that
// port. nil (the default) means no hook.
func WithPreMarshalHook(fn func(map[string]json.RawMessage)) ConfigWriterOption {
	return func(w *ConfigWriter) { w.preMarshalHook = fn }
}

// ConfigWriter is the single writer of agent-config.json within the
// agentd process. All config changes (provider credentials, model
// selection, relay injection, MCP servers) go through SetProviders /
// SetModel / SetRelay / SetMCPServers followed by Rebuild.
//
// Thread-safe: all mutating methods acquire mu. Rebuild serializes the
// read-merge-write cycle so concurrent reloads and relay injection
// cannot interleave.
type ConfigWriter struct {
	mu              sync.Mutex
	path            string
	providerRaw     json.RawMessage  // raw "provider" map JSON from FormatOpenCodeConfig; nil = no providers
	model           string           // fully-qualified "providerID/modelID" form; "" = no model
	relay           *relaySource     // nil = relay not yet injected / skipped
	mcpServers      []MCPServerEntry // staged MCP servers from SetMCPServers; nil = none
	adminPrompt     string           // admin-configured system prompt; "" = none
	agentRaw        json.RawMessage  // existing "agent" config from loadExisting, preserved across rebuilds
	modeRaw         json.RawMessage  // existing "mode" config from loadExisting, preserved across rebuilds
	mcpRaw          json.RawMessage  // existing "mcp" object from loadExisting (e.g. user-staged servers written by materialize, Epic 53); re-emitted when no staged source. Non-object or null sections are NOT captured (dropped, not round-tripped)
	allowedDirs     []string         // glob patterns, merged as external_directory allow-rules
	injectedDirs    []string         // external_directory keys the writer last injected (or recovered from a prior render); stripped from modeRaw when the AllowedDirs source changes so replace/clear are authoritative over prior renders
	adminPromptPath string           // path to admin-prompt file; "" = skip
	allowedDirsPath string           // path to allowed-dirs JSON; "" = skip
	preMarshalHook  func(map[string]json.RawMessage)
}

// NewConfigWriter creates the writer and initializes its sources from
// the existing agent-config.json file (written by the materialize
// subcommand at boot), the admin-prompt file, and the allowed-dirs file.
// Options set the paths; absent options mean the corresponding source is
// skipped. If agent-config.json is absent or corrupt, sources start
// empty and the first Rebuild creates a fresh file.
func NewConfigWriter(path string, opts ...ConfigWriterOption) *ConfigWriter {
	w := &ConfigWriter{path: path}
	for _, opt := range opts {
		opt(w)
	}
	w.loadExisting()
	w.loadAdminPrompt()
	w.loadAllowedDirs()
	return w
}

// loadExisting reads the current agent-config.json and captures the
// provider map and model as sources. Called once at construction.
// Silent on error — a missing or corrupt file means the writer starts
// empty, which is correct for zero-credential users.
func (w *ConfigWriter) loadExisting() {
	data, err := os.ReadFile(w.path)
	if err != nil || len(data) == 0 {
		return
	}
	var cfg struct {
		Provider json.RawMessage `json:"provider"`
		Model    string          `json:"model,omitempty"`
		Agent    json.RawMessage `json:"agent,omitempty"`
		Mode     json.RawMessage `json:"mode,omitempty"`
		MCP      json.RawMessage `json:"mcp,omitempty"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return
	}
	w.providerRaw = cfg.Provider
	w.model = cfg.Model
	w.agentRaw = cfg.Agent
	w.modeRaw = cfg.Mode

	// Preserve the on-disk "mcp" section (user-staged servers written by
	// the materialize subcommand, Epic 53). Without this, any rebuild
	// from a writer whose staged MCP source is nil (boot normalize,
	// pre-boot relay, relay injector) would silently delete the user's
	// servers: rebuildLocked emitted "mcp" only from staged state.
	// Staged sources (SetMCPServers / Apply MCPServers) remain
	// authoritative and supersede the captured section entirely.
	// Only a JSON object is captured — null / arrays / scalars are
	// dropped rather than round-tripped into the output.
	if len(cfg.MCP) > 0 {
		var mcpMap map[string]json.RawMessage
		if json.Unmarshal(cfg.MCP, &mcpMap) == nil && mcpMap != nil {
			w.mcpRaw = cfg.MCP
		}
	}

	// Recover the writer-injected external_directory keys from a
	// previously rendered mode block (the post-boot-normalize pod
	// state). Fail-closed heuristic: every map-form entry valued
	// "allow" is treated as writer-injected — a user-authored allow is
	// indistinguishable from a writer-rendered one in the artifact, so
	// a later AllowedDirs replace/clear sweeps it too. Deny/ask rules
	// and bare-string policies are distinguishable and survive. The
	// ambiguity dissolves in increment 3 (#860, pure render: injected
	// keys tracked in writer state, not re-derived from the artifact).
	if len(cfg.Mode) > 0 {
		var mode struct {
			Permissions struct {
				ExternalDirectory map[string]string `json:"external_directory"`
			} `json:"permissions"`
		}
		if json.Unmarshal(cfg.Mode, &mode) == nil {
			for k, v := range mode.Permissions.ExternalDirectory {
				if v == "allow" {
					w.injectedDirs = append(w.injectedDirs, k)
				}
			}
		}
	}

	// 2026-06-23 cold-start optimization (item #1a, Phase D): detect
	// a pre-boot-injected relay block and set the writer's relay
	// source so hasRelay() returns true. Without this, the legacy
	// in-pod startRelayInjector goroutine would think no relay is
	// configured (writer.relay == nil) and run its full
	// fetch+kill+restart cycle redundantly, defeating the entire
	// point of Phase C.
	//
	// We extract just enough info to satisfy HasRelay() — the actual
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

// loadAdminPrompt reads the admin-configured system prompt file (path
// set via WithAdminPromptPath). Loaded once at init; persists across all
// rebuilds. Changes take effect on next pod boot (design decision: no
// hot-reload).
func (w *ConfigWriter) loadAdminPrompt() {
	if w.adminPromptPath == "" {
		return
	}
	data, err := os.ReadFile(w.adminPromptPath)
	if err != nil || len(data) == 0 {
		return
	}
	w.adminPrompt = string(data)
}

// setAllowedDirsPath overrides the allowed-dirs file path. Test seam so
// tests can point at a t.TempDir() file instead of a production path.
// The constructor already called loadAllowedDirs() with the option path
// (or no path); calling this after construction + calling loadAllowedDirs()
// again re-reads from the new path.
func (w *ConfigWriter) setAllowedDirsPath(p string) {
	w.allowedDirsPath = p
}

// loadAllowedDirs reads the instance's allowedExternalDirectories setting
// (written by the bootstrap subcommand as a JSON array of glob patterns)
// and stores the patterns as the external_directory allow-rule source.
// Silent on error — a missing or corrupt file means no allow-rules are
// injected, which is correct (agents prompt as before). Patterns are
// de-duplicated to keep the rendered config clean.
func (w *ConfigWriter) loadAllowedDirs() {
	if w.allowedDirsPath == "" {
		return
	}
	w.allowedDirs = nil
	data, err := os.ReadFile(w.allowedDirsPath)
	if err != nil || len(data) == 0 {
		return
	}
	var patterns []string
	if json.Unmarshal(data, &patterns) != nil {
		return
	}
	w.allowedDirs = sanitizeAllowedDirs(patterns)
	// The side-car patterns are what this writer will inject; UNION them
	// with the injected-dirs set recovered from a previously rendered
	// mode block (loadExisting ran first) — a restart must retain
	// authority over entries a prior writer lifetime rendered (e.g. a
	// runtime AllowedDirs Apply before the restart), or a later clear
	// would resurrect them (round-2 review: the side-car load used to
	// overwrite the recovered set wholesale).
	seen := make(map[string]struct{}, len(w.injectedDirs)+len(w.allowedDirs))
	union := make([]string, 0, len(w.injectedDirs)+len(w.allowedDirs))
	for _, set := range [][]string{w.injectedDirs, w.allowedDirs} {
		for _, p := range set {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			union = append(union, p)
		}
	}
	w.injectedDirs = union
}

// parseRelayFromExisting extracts URL + models from a pre-injected
// opencode-relay provider block. Used by loadExisting to make the
// writer aware of a relay block written by the materialize subcommand
// (Phase C) before agentd started.
//
// Returns a populated *relaySource on success, or a sentinel
// non-nil source with empty fields if extraction fails — the
// non-nil-ness is what matters for HasRelay().
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
		// sentinel so HasRelay() reports true. Rebuild() will
		// regenerate the block from defaults if anyone calls it.
		return src
	}
	src.url = entry.Options.BaseURL
	for id, m := range entry.Models {
		src.models = append(src.models, RelayModel{
			ID:           id,
			Name:         m.Name,
			ContextLimit: m.Limit.Context,
			OutputLimit:  m.Limit.Output,
		})
	}
	return src
}

// SetProviders updates the provider source from a FormatOpenCodeConfig
// result. The formatted bytes contain the complete opencode config shape
// ({ $schema, provider: {...} }); this method extracts just the provider
// map. The model from the formatter is NOT captured — the model source
// is owned by SetModel (set at boot via loadExisting) and must survive
// credential reloads.
func (w *ConfigWriter) SetProviders(formattedConfig []byte) error {
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

// SetModel updates the model source. Called by applyWorkspaceConfig
// at boot (via the materialize subcommand) to set the default model
// from workspace-config.json.
func (w *ConfigWriter) SetModel(model string) {
	w.mu.Lock()
	w.model = model
	w.mu.Unlock()
}

// SetRelay updates the relay source after the relay injector successfully
// discovers the free model list. The writer stores the URL and models;
// Rebuild merges them into the provider map.
func (w *ConfigWriter) SetRelay(url string, models []RelayModel) {
	w.mu.Lock()
	w.relay = &relaySource{url: url, models: models}
	w.mu.Unlock()
}

// SetMCPServers replaces the MCP server source. Called after materialize
// stages MCP server entries from secrets.json. Each server renders as one
// entry in the opencode "mcp" top-level config section.
func (w *ConfigWriter) SetMCPServers(servers []MCPServerEntry) {
	w.mu.Lock()
	w.mcpServers = servers
	w.mu.Unlock()
}

// HasRelay returns true if the relay injector has successfully injected
// relay config. Used by the readyz handler for the RelayInjected signal.
func (w *ConfigWriter) HasRelay() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.relay != nil
}

// Rebuild merges all sources (providers, model, relay, MCP, admin prompt,
// allowed dirs) and writes the complete agent-config.json atomically via
// temp-file + os.Rename.
//
// Merge semantics:
//   - $schema is always set to "https://opencode.ai/config.json"
//   - provider map = existing providers (from SetProviders or loadExisting)
//   - opencode-relay (if relay is set). No existing provider is removed.
//   - model = the model source (from SetModel or loadExisting)
//   - disabled_providers = ["opencode"] (only if relay is set)
//   - agent.build.prompt = admin prompt (deep-merged into existing build agent)
//   - mode.permissions.external_directory = allowed-dirs glob allow-rules
//   - mcp = staged MCP servers + pre-marshal hook additions
//
// The temp-file + rename pattern ensures readers never see a partially
// written file. os.Rename is atomic on POSIX filesystems (same mount).
//
// Thread-safety: Rebuild acquires w.mu for the whole read-merge-write
// cycle so concurrent Rebuild/SetX calls cannot interleave.
func (w *ConfigWriter) Rebuild() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rebuildLocked()
}

// rebuildLocked is Rebuild without the lock acquire. Caller must hold w.mu.
// Used by Apply to merge multiple source updates into a single atomic
// read-merge-write cycle (Rule 11 F1: Apply atomicity contract).
func (w *ConfigWriter) rebuildLocked() error {

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
			// JSON `null` decodes into a nil map WITHOUT error — writing
			// into it would panic the whole agentd process (round-3
			// review; reachable via agent self-tampering of
			// /sandbox-runtime, RW in the main container). Null is
			// treated as absent: a fresh map of the injected patterns
			// replaces it.
			if raw, ok := perms["external_directory"]; ok {
				var existing map[string]string
				unmarshalErr := json.Unmarshal(raw, &existing)
				switch {
				case unmarshalErr == nil && existing != nil:
					for _, p := range w.allowedDirs {
						existing[p] = "allow"
					}
					extDirJSON, err := json.Marshal(existing)
					if err != nil {
						return fmt.Errorf("agent-config writer: marshal external_directory: %w", err)
					}
					perms["external_directory"] = extDirJSON
				case unmarshalErr == nil && existing == nil:
					extDir := make(map[string]string, len(w.allowedDirs))
					for _, p := range w.allowedDirs {
						extDir[p] = "allow"
					}
					extDirJSON, err := json.Marshal(extDir)
					if err != nil {
						return fmt.Errorf("agent-config writer: marshal external_directory: %w", err)
					}
					perms["external_directory"] = extDirJSON
				default:
					// Bare-string branch: preserved as-is, no injection.
				}
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
	// opencode "remote"; stdio renders as "local". The staged source is
	// authoritative: when at least one server is staged, the section is
	// exactly the staged list (a credential reload re-stages the full
	// workspace set). With no staged source, the section captured from
	// the existing file in loadExisting is re-emitted verbatim — a
	// no-MCP workspace with no on-disk section produces byte-equivalent
	// output to the pre-Epic-53 writer.
	if len(w.mcpServers) > 0 {
		mcp := make(map[string]json.RawMessage, len(w.mcpServers))
		for _, srv := range w.mcpServers {
			// Convert MCPServerEntry → StagedMCPServer → shared renderer.
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
	} else if len(w.mcpRaw) > 0 {
		cfg["mcp"] = w.mcpRaw
	}

	// preMarshalHook lets the caller (agentd) inject entries that this
	// package must not know about — e.g. the built-in admin MCP server
	// pointing at agentd's own port. The hook runs after all internal
	// sources are merged and before final marshal.
	if w.preMarshalHook != nil {
		w.preMarshalHook(cfg)
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

// Apply implements agent.AgentConfigWriter. It is the seam platform code
// uses after construction; it never calls the SetX methods directly.
//
// Each non-nil field on in updates one source on the writer. A nil field
// leaves the writer's existing state for that source unchanged. A non-nil
// pointer to a zero-value struct clears the source (where meaningful).
//
// After merging the input, Apply calls rebuildLocked to render and write
// the full config atomically. Returns (true, nil) on success — opencode
// does not hot-reload its config file, so every successful Apply requires
// the process to be restarted for the change to take effect. A future
// agent that hot-reloads returns false from its Apply; platform code
// branches on the bool and skips the restart.
//
// Thread-safety: Apply holds w.mu across the entire merge + write cycle
// so concurrent Apply / Rebuild / SetX calls serialize. Two concurrent
// Apply calls cannot interleave such that one caller's update is lost
// (Rule 11 F1: atomicity is part of the Apply contract, not just the
// write step).
//
// The opencode-specific rendering (deep-merge semantics, $schema URL,
// disabled_providers, the opencode-relay provider block, the agent.build
// prompt merge, the mode.permissions.external_directory merge, the mcp
// section) is owned by this method and rebuildLocked — none of it leaks
// through the agent.AgentConfigInput type. Platform code calls Apply and
// reacts to restartRequired; it does not know WHY a restart is needed.
func (w *ConfigWriter) Apply(in agent.AgentConfigInput) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Capture prior source state so we can roll back if rebuildLocked
	// fails. A failed Apply must leave the writer's in-memory state
	// unchanged — otherwise HasRelay() would return true after a
	// failed relay Apply, breaking the readyz + relay-injector
	// short-circuits that gate on it. The on-disk file is unchanged
	// (rebuildLocked's atomic rename either fully succeeds or never
	// replaces the target); only in-memory state needs the rollback.
	prevProviderRaw := w.providerRaw
	prevModel := w.model
	prevRelay := w.relay
	prevMCPServers := w.mcpServers
	prevMCPRaw := w.mcpRaw
	prevAdminPrompt := w.adminPrompt
	prevAllowedDirs := w.allowedDirs
	prevAgentRaw := w.agentRaw
	prevModeRaw := w.modeRaw
	prevInjectedDirs := w.injectedDirs
	rollback := func() {
		w.providerRaw = prevProviderRaw
		w.model = prevModel
		w.relay = prevRelay
		w.mcpServers = prevMCPServers
		w.mcpRaw = prevMCPRaw
		w.adminPrompt = prevAdminPrompt
		w.allowedDirs = prevAllowedDirs
		w.agentRaw = prevAgentRaw
		w.modeRaw = prevModeRaw
		w.injectedDirs = prevInjectedDirs
	}

	if in.Providers != nil {
		if len(in.Providers.Formatted) > 0 {
			if err := w.setProvidersLocked(in.Providers.Formatted); err != nil {
				rollback()
				return false, fmt.Errorf("agent-config writer Apply: set providers: %w", err)
			}
		} else {
			// Clear: a non-nil pointer with empty Formatted bytes
			// drops the provider source entirely.
			w.providerRaw = nil
		}
	}

	if in.Model != nil {
		// Empty string = clear (matches Apply semantics elsewhere).
		w.model = string(*in.Model)
	}

	if in.Relay != nil {
		if in.Relay.URL != "" {
			w.relay = &relaySource{
				url:    in.Relay.URL,
				models: relayModelsFromAgent(in.Relay.Models),
			}
		} else {
			// Clear: a non-nil pointer with empty URL drops the relay
			// source. Distinct from nil = leave unchanged.
			w.relay = nil
		}
	}

	if in.MCPServers != nil {
		// A staged MCP input (even an empty clear) supersedes the
		// section captured from disk — staged state is authoritative.
		w.mcpRaw = nil
		if len(in.MCPServers.Servers) > 0 {
			w.mcpServers = mcpServersFromAgent(in.MCPServers.Servers)
		} else {
			// Clear: a non-nil pointer with empty Servers drops the
			// MCP source entirely.
			w.mcpServers = nil
		}
	}

	// AdminPrompt / AllowedDirs are first-class sources (US-65.9
	// increment 2): construction may seed them from the bootstrap
	// side-car files; Apply updates them thereafter with the same
	// pointer semantics as every other source. Non-nil input makes the
	// source authoritative: keys the writer previously rendered are
	// stripped from the captured raw sections first, so replace/clear
	// take effect on the output even when this writer was constructed
	// over an already-rendered file (the post-boot-normalize pod state).
	if in.AdminPrompt != nil {
		w.adminPrompt = in.AdminPrompt.Text
		w.agentRaw = stripBuildPrompt(w.agentRaw)
	}
	if in.AllowedDirs != nil {
		dirs := sanitizeAllowedDirs(in.AllowedDirs.Dirs)
		w.modeRaw = stripInjectedExternalDirs(w.modeRaw, w.injectedDirs)
		w.allowedDirs = dirs
		w.injectedDirs = dirs
	}

	if err := w.rebuildLocked(); err != nil {
		rollback()
		return false, fmt.Errorf("agent-config writer Apply: rebuild: %w", err)
	}

	return true, nil
}

// setProvidersLocked is SetProviders without the lock acquire. Caller
// must hold w.mu. Used by Apply to keep the merge+write atomic.
func (w *ConfigWriter) setProvidersLocked(formattedConfig []byte) error {
	var cfg struct {
		Provider json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(formattedConfig, &cfg); err != nil {
		return fmt.Errorf("parse formatted providers: %w", err)
	}
	w.providerRaw = cfg.Provider
	return nil
}

// relayModelsFromAgent converts the agent-layer RelayModel slice to the
// opencode-layer type. The two are structurally identical; the conversion
// exists so the agent.AgentConfigWriter seam does not export the
// opencode-specific type.
func relayModelsFromAgent(in []agent.RelayModel) []RelayModel {
	if len(in) == 0 {
		return nil
	}
	out := make([]RelayModel, len(in))
	for i, m := range in {
		out[i] = RelayModel{
			ID:           m.ID,
			Name:         m.Name,
			ContextLimit: m.ContextLimit,
			OutputLimit:  m.OutputLimit,
		}
	}
	return out
}

// mcpServersFromAgent converts the agent-layer MCPServerEntry slice to
// the opencode-layer type. Same rationale as relayModelsFromAgent.
func mcpServersFromAgent(in []agent.MCPServerEntry) []MCPServerEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]MCPServerEntry, len(in))
	for i, s := range in {
		out[i] = MCPServerEntry{
			Name:      s.Name,
			Transport: s.Transport,
			URL:       s.URL,
			Command:   s.Command,
			Args:      s.Args,
			TimeoutMs: s.TimeoutMs,
			Env:       s.Env,
			Headers:   s.Headers,
		}
	}
	return out
}

// buildRelayProviderEntry builds the JSON for the opencode-relay provider
// entry that gets merged into the provider map.
//
// The relay entry shape:
//
//	{
//	  "name": "OpenCode Zen (Free)",
//	  "npm": "@ai-sdk/openai-compatible",
//	  "options": {"baseURL": "<relayURL>", "apiKey": "public"},
//	  "models": {"<id>": {"name": "...", "limit": {"context": ..., "output": ...}}}
//	}
func buildRelayProviderEntry(relayURL string, models []RelayModel) (json.RawMessage, error) {
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
