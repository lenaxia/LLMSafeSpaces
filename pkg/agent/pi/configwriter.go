// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pi owns the pi coding-agent (earendil-works/pi) config surface
// behind the agent.AgentConfigWriter seam.
//
// Containment (Epic 65 / Rule 12, mirroring pkg/agent/opencode): every
// byte of pi config-shape knowledge — models.json's providers object,
// mcp.json's mcpServers shape, auth.json's api_key entries,
// APPEND_SYSTEM.md, settings.json's defaultProvider/defaultModel — lives
// in this package. Platform code constructs a ConfigWriter via
// NewConfigWriter and holds it as an agent.AgentConfigWriter; it never
// learns what the rendered files look like.
//
// pi's whole config surface relocates with one env var — "Set its
// location with the PI_CODING_AGENT_DIR environment variable"
// (configuration.md) — and every write target of this writer sits under
// that single directory, satisfying readOnlyRootFilesystem exactly as the
// opencode writer's single agent-config.json does. The writer takes the
// directory (not individual paths) as its construction parameter.
//
// Source mapping (AgentConfigInput → pi files):
//
//   - Providers  → models.json custom-endpoint provider entries
//     (baseUrl + api "openai-completions" + apiKey + models[]) for
//     entries the formatter rendered with a baseURL, and auth.json
//     {"type":"api_key"} entries for first-party keys (baseURL empty —
//     pi's built-in catalog already knows the provider; only the
//     credential is missing).
//   - Relay      → a models.json provider entry under the same
//     "opencode-relay" id the opencode writer uses, with the free model
//     list as models[].
//   - Model      → settings.json defaultProvider/defaultModel (the
//     "providerID/modelID" selection split on the first "/"), with
//     ModelResolutionWarning parity (below).
//   - MCPServers → mcp.json mcpServers entries (stdio: command/args/env;
//     http/sse: url/headers; timeoutMs converted to pi's integer
//     SECONDS timeout).
//   - AdminPrompt → APPEND_SYSTEM.md. The append form is deliberate:
//     SYSTEM.md REPLACES pi's default system prompt, which carries the
//     harness's own operational instructions; the platform prompt is
//     additive control-surface content, so APPEND_SYSTEM.md ("Adds
//     instructions to Pi's system prompt", configuration.md) is its
//     natural home. SYSTEM.md is never written.
//   - AllowedDirs → accepted and DROPPED. pi has no permission system
//     ("Pi does not include a built-in permission system for restricting
//     filesystem, process, network, or credential access", README) —
//     the §1.5 scoping ruling (pi-integration report): the sandbox is
//     the boundary. Apply still succeeds so existing callers (secrets
//     pipeline) work unmodified.
//
// restartRequired matrix (the seam contract, agentconfig.go:60-63: "A
// future agent that hot-reloads returns false and the platform's restart
// machinery no-ops"):
//
//   - Providers  → true. Credentials land in auth.json, whose !command
//     keys "run the command when the key is first needed and cache its
//     standard output for the process lifetime" (providers.md) —
//     restart-bounded, exactly like opencode's auth store.
//   - Relay      → false when the relay credential is delivered as a
//     dynamic models.json apiKey ("!command", "$VAR" or "${VAR}"):
//     "Commands in models.json run at request time and are not cached by
//     Pi" (models.md), so credential rotation needs no restart — the
//     pi hot-reload seam the agentconfig.go comment anticipated. A
//     literal apiKey (the "public" default, parity with the opencode
//     relay entry) or a relay clear returns true.
//   - Model / MCPServers / AdminPrompt → true: settings.json, mcp.json
//     and system-prompt resources are read at process/session start
//     (pi re-reads them on /reload or /model, neither of which exists
//     in the RPC mode the platform drives).
//
// ModelResolutionWarning parity (agentd.ModelResolutionWarningPath,
// pkg/agentd/types.go:75-86): pi's models.md Troubleshooting — custom
// models "remain unavailable ... until Pi can resolve credentials" — is
// the same failure class the 2026-08-16 incident fixed for opencode. The
// writer verifies the Model source against the provider set it renders
// (custom endpoints + first-party keys + relay; allowlisted providers
// must CLAIM the model): an unresolvable id is omitted from settings.json
// and recorded in a model-resolution-warning.json marker placed beside
// the agent directory (same base name as the agentd constant), instead of
// poisoning pi's startup model. The marker self-clears when a later
// render resolves the selection (e.g. the relay injector succeeded).

package pi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// relayProviderID is the models.json provider id the relay renders under.
// Deliberately identical to the opencode writer's provider map key so
// cross-harness code (relay injector, statusz, HasRelay probing) sees one
// vocabulary.
const relayProviderID = "opencode-relay"

// defaultRelayAPIKey mirrors the opencode relay injector's dummy
// credential: the self-hosted relay fleet routes free-tier requests
// anonymously and ignores the bearer value, so "public" (opencode parity)
// is the neutral default. Clusters that front the relay with a real
// credential override it via WithRelayAPIKey — preferably with a
// "!command" or "${VAR}" form, which additionally makes relay
// re-application restart-free (see the restartRequired matrix above).
const defaultRelayAPIKey = "public"

// modelsSchemaURL is pi's published models.json schema (editor
// validation; models-configuration.md).
const modelsSchemaURL = "https://pi.dev/schemas/models.schema.json"

// authTypeAPIKey is auth.json's API-key credential discriminator
// (providers.md: {"<provider>": {"type": "api_key", "key": "..."}}).
const authTypeAPIKey = "api_key"

// openaiCompletionsAPI is pi's api discriminator for OpenAI-compatible
// /v1/chat/completions endpoints (models-configuration.md) — what every
// custom-endpoint provider the platform stages renders as, the same
// endpoint class the opencode formatter's "@ai-sdk/openai-compatible"
// npm marker selects.
const openaiCompletionsAPI = "openai-completions"

// modelResolutionWarningFile matches the base name of
// agentd.ModelResolutionWarningPath ("/sandbox-runtime/
// model-resolution-warning.json"). Duplicated as a literal so this
// package does not import pkg/agentd (import direction: pkg/agent must
// stay below the agentd packages that consume it). The marker is written
// BESIDE the agent directory — with the standard agent dir
// /sandbox-runtime/pi that is exactly agentd.ModelResolutionWarningPath.
const modelResolutionWarningFile = "model-resolution-warning.json"

// ConfigWriterOption configures a ConfigWriter at construction.
type ConfigWriterOption func(*ConfigWriter)

// WithAdminPromptPath sets the path to the admin-configured system prompt
// file (written by the agentd bootstrap subcommand). The writer reads it
// once at construction and renders it as APPEND_SYSTEM.md. Empty (the
// default) means no admin-prompt source.
func WithAdminPromptPath(p string) ConfigWriterOption {
	return func(w *ConfigWriter) { w.adminPromptPath = p }
}

// WithRelayAPIKey sets the credential string rendered into the relay
// provider entry's models.json apiKey. A leading "!" (command) or "$"
// (environment interpolation) makes the credential request-time-resolved
// by pi, which makes relay re-application restart-free; any other value
// is written literally. Default: "public" (opencode relay parity).
func WithRelayAPIKey(key string) ConfigWriterOption {
	return func(w *ConfigWriter) { w.relayAPIKey = key }
}

// WithModelResolutionWarningPath overrides where the unresolvable-model
// marker is written. Default: <parent-of-agent-dir>/model-resolution-
// warning.json (the "beside the config" convention; with agent dir
// /sandbox-runtime/pi this is agentd.ModelResolutionWarningPath exactly).
func WithModelResolutionWarningPath(p string) ConfigWriterOption {
	return func(w *ConfigWriter) { w.warnPathOverride = p }
}

// providerSourceModel is one model of a provider source, at the writer's
// agent-neutral granularity (pi field names).
type providerSourceModel struct {
	ID      string
	Name    string
	Context int // pi contextWindow; 0 = undeclared
	Output  int // pi maxTokens; 0 = undeclared
	// ImageInput mirrors the credential-declared attachment capability:
	// true renders pi's "input": ["text","image"]; false/undeclared
	// stays omitted (pi's default is text-only input).
	ImageInput bool
}

// providerEntry is one provider from the Providers source, after parsing
// the platform-formatted bytes.
type providerEntry struct {
	ID string
	// BaseURL is the custom endpoint. Empty = first-party provider —
	// the credential lands in auth.json and no endpoint override is
	// rendered (pi's catalog already defines the provider).
	BaseURL string
	APIKey  string
	// API is the pi api discriminator; non-empty only for custom
	// endpoints parsed from a pi-native providers shape.
	API string
	// Models is the declared model list. nil = no allowlist declared
	// (first-party catalog-sourced); non-nil = the provider claims
	// exactly these ids (model resolution requires a claim).
	Models []providerSourceModel
}

// ConfigWriter is the single writer of pi's agent-dir config files within
// the agentd process, implementing agent.AgentConfigWriter. It holds the
// independent AgentConfigInput sources and renders every file from them
// on each Apply (full rewrite via temp-file + rename; each file is
// atomic, sources are merged under one lock).
//
// Thread-safe: Apply guards the whole merge+render+write cycle with mu,
// mirroring the opencode writer's Rule 11 F1 atomicity contract.
type ConfigWriter struct {
	mu  sync.Mutex
	dir string // pi agent dir (PI_CODING_AGENT_DIR target)

	// sources
	providers   []providerEntry   // sorted by ID; nil = none
	model       string            // "providerID/modelID"; "" = none
	relay       *agent.RelayState // nil = no relay; URL "" = block present but unparseable (HasRelay sentinel)
	mcpServers  []agent.MCPServerEntry
	adminPrompt string

	// relayAPIKey is the credential rendered into the relay entry
	// (WithRelayAPIKey; default "public").
	relayAPIKey string

	// captured state (construction-time disk reads, preserved across
	// renders unless a source supersedes them)
	authExtra    map[string]json.RawMessage // auth.json entries not owned by this writer (pi OAuth creds, unknown types)
	authExtrasOK bool                       // auth.json was absent or valid JSON at construction
	mcpRaw       map[string]json.RawMessage // on-disk mcpServers when no staged source has ever superseded it
	mcpOK        bool                       // mcp.json was absent or parseable at construction
	settingsRaw  map[string]json.RawMessage // settings.json minus the writer-owned model keys
	settingsOK   bool                       // settings.json was absent or valid JSON at construction

	// paths
	adminPromptPath  string
	warnPathOverride string
}

// NewConfigWriter creates the writer over a pi agent directory and seeds
// its sources from the files already on disk (written by the materialize
// subcommand / a prior writer lifetime / pre-boot relay injection), plus
// the admin-prompt side-car file. Absent or corrupt files mean the
// corresponding source starts empty — correct for zero-credential users;
// for auth.json/settings.json/mcp.json an unparseable file is LEFT ALONE
// (pi or the user owns it; the writer refuses to clobber what it cannot
// parse, and refuses to delete it too).
func NewConfigWriter(dir string, opts ...ConfigWriterOption) *ConfigWriter {
	w := &ConfigWriter{dir: dir, relayAPIKey: defaultRelayAPIKey}
	for _, opt := range opts {
		opt(w)
	}
	w.loadExisting()
	w.loadAdminPrompt()
	return w
}

// HasRelay reports whether the writer's current state includes a relay
// block (applied via Apply, or discovered on disk at construction — the
// pre-boot-injection short-circuit the relay injector gates on).
func (w *ConfigWriter) HasRelay() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.relay != nil
}

// Apply implements agent.AgentConfigWriter. Each non-nil input field
// updates one source; nil leaves it unchanged; a non-nil pointer to a
// zero value clears it (see AgentConfigInput). After merging, every
// config file is re-rendered from the full source set.
//
// restartRequired is the per-source OR documented on the package: true
// for Providers (auth.json credentials are process-lifetime-cached),
// Model, MCPServers and AdminPrompt (startup-read files); false for
// Relay when its rendered apiKey is a dynamic credential reference pi
// re-resolves at request time. AllowedDirs is dropped (no pi-native
// permission surface) and contributes false.
//
// On a render error the writer's in-memory state rolls back so a retry
// re-renders the pre-Apply sources; files already written stay on disk
// (each individual file write is atomic; there is no cross-file
// transaction — the same class as the opencode writer, whose atomicity
// unit is the merge+write under the lock, not a filesystem transaction).
func (w *ConfigWriter) Apply(in agent.AgentConfigInput) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	prev := w.snapshotLocked()

	restart := false

	if in.Providers != nil {
		if len(in.Providers.Formatted) > 0 {
			entries, err := parseProvidersFormatted(in.Providers.Formatted)
			if err != nil {
				return false, fmt.Errorf("pi config writer Apply: providers: %w", err)
			}
			w.providers = entries
		} else {
			// Clear: non-nil pointer with empty bytes drops the source.
			w.providers = nil
		}
		restart = true
	}

	if in.Model != nil {
		w.model = string(*in.Model)
		restart = true
	}

	if in.Relay != nil {
		if in.Relay.URL != "" {
			rs := *in.Relay // copy; callers must not observe writer state by mutation
			rs.Models = append([]agent.RelayModel(nil), in.Relay.Models...)
			w.relay = &rs
		} else {
			// Clear: non-nil pointer with empty URL drops the source.
			w.relay = nil
		}
		restart = restart || relayRequiresRestart(w.relay, w.relayAPIKey)
	}

	if in.MCPServers != nil {
		// A staged input (even an empty clear) supersedes the captured
		// on-disk section — staged state is authoritative (opencode
		// writer parity). An explicitly staged source also takes
		// ownership of the file from an unparseable on-disk one (the
		// leave-alone rule guards against DELETING what the writer
		// cannot understand, not against explicit platform direction).
		w.mcpRaw = nil
		w.mcpOK = true
		if len(in.MCPServers.Servers) > 0 {
			w.mcpServers = append([]agent.MCPServerEntry(nil), in.MCPServers.Servers...)
		} else {
			w.mcpServers = nil
		}
		restart = true
	}

	if in.AdminPrompt != nil {
		w.adminPrompt = in.AdminPrompt.Text
		restart = true
	}

	// in.AllowedDirs: deliberately dropped — no pi-native equivalent
	// (permission-less harness, §1.5 ruling). Accepted so existing
	// callers work unmodified; contributes no restart.

	if err := w.renderLocked(); err != nil {
		w.restoreLocked(prev)
		return false, fmt.Errorf("pi config writer Apply: render: %w", err)
	}
	return restart, nil
}

// writerSnapshot captures the mutable writer state for Apply rollback.
type writerSnapshot struct {
	providers   []providerEntry
	model       string
	relay       *agent.RelayState
	mcpServers  []agent.MCPServerEntry
	mcpRaw      map[string]json.RawMessage
	mcpOK       bool
	adminPrompt string
}

func (w *ConfigWriter) snapshotLocked() writerSnapshot {
	return writerSnapshot{
		providers:   w.providers,
		model:       w.model,
		relay:       w.relay,
		mcpServers:  w.mcpServers,
		mcpRaw:      w.mcpRaw,
		mcpOK:       w.mcpOK,
		adminPrompt: w.adminPrompt,
	}
}

func (w *ConfigWriter) restoreLocked(s writerSnapshot) {
	w.providers = s.providers
	w.model = s.model
	w.relay = s.relay
	w.mcpServers = s.mcpServers
	w.mcpRaw = s.mcpRaw
	w.mcpOK = s.mcpOK
	w.adminPrompt = s.adminPrompt
}

// relayRequiresRestart decides the relay source's restartRequired
// contribution: a cleared relay leaves models.json (true); a literal
// apiKey is file-carried (true); a dynamic "!command" or "$VAR"/"${VAR}"
// key is re-resolved by pi at request time (models.md) — false.
func relayRequiresRestart(relay *agent.RelayState, apiKey string) bool {
	if relay == nil {
		return true
	}
	return !isDynamicCredential(apiKey)
}

// isDynamicCredential reports whether key is a pi request-time credential
// reference: a leading "!" (command) or "$" ($VAR / ${VAR}
// interpolation) per models.md "apiKey and header values can use $NAME
// or ${NAME} environment interpolation, a literal value, or a leading
// !command".
func isDynamicCredential(key string) bool {
	return strings.HasPrefix(key, "!") || strings.HasPrefix(key, "$")
}

// --- construction-time loading -----------------------------------------

// loadExisting seeds the writer's sources from the files in the agent
// directory. Silent on per-file errors: a missing file means an empty
// source; for auth.json/settings.json an UNPARSEABLE file sets the OK
// flag false so renders leave it alone instead of clobbering pi-owned
// state the writer does not understand.
func (w *ConfigWriter) loadExisting() {
	w.loadExistingModels()
	w.loadExistingAuth()
	w.loadExistingMCP()
	w.loadExistingSettings()
	w.loadExistingPrompt()
}

// loadExistingPrompt seeds the AdminPrompt source from a previously
// rendered APPEND_SYSTEM.md so the prompt survives writer
// re-construction (writer restart round-trip). The bootstrap side-car
// file (WithAdminPromptPath) is loaded after and takes precedence.
func (w *ConfigWriter) loadExistingPrompt() {
	if w.adminPrompt != "" {
		return
	}
	data, err := os.ReadFile(w.appendSystemPath())
	if err != nil || len(data) == 0 {
		return
	}
	w.adminPrompt = string(data)
}

// loadExistingModels seeds providers + relay from models.json. A relay
// entry discovered on disk sets the relay source even when partially
// unparseable (non-nil sentinel — HasRelay() must be true so the relay
// injector's pre-boot short-circuit works, opencode Phase D parity); an
// empty-URL sentinel renders nothing (renderLocked skips empty URLs).
func (w *ConfigWriter) loadExistingModels() {
	data, err := os.ReadFile(w.modelsPath())
	if err != nil || len(data) == 0 {
		return
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				ContextWindow int      `json:"contextWindow"`
				MaxTokens     int      `json:"maxTokens"`
				Input         []string `json:"input"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return
	}

	entries := make([]providerEntry, 0, len(doc.Providers))
	for id, p := range doc.Providers {
		if id == relayProviderID {
			relay := &agent.RelayState{URL: p.BaseURL}
			for _, m := range p.Models {
				relay.Models = append(relay.Models, agent.RelayModel{
					ID:           m.ID,
					Name:         m.Name,
					ContextLimit: m.ContextWindow,
					OutputLimit:  m.MaxTokens,
				})
			}
			w.relay = relay
			continue
		}
		entry := providerEntry{ID: id, BaseURL: p.BaseURL, API: p.API, APIKey: p.APIKey}
		for _, m := range p.Models {
			pm := providerSourceModel{ID: m.ID, Name: m.Name, Context: m.ContextWindow, Output: m.MaxTokens}
			for _, in := range m.Input {
				if in == "image" {
					pm.ImageInput = true
				}
			}
			entry.Models = append(entry.Models, pm)
		}
		entries = append(entries, entry)
	}
	sortProviders(entries)
	w.providers = entries
}

// loadExistingAuth classifies auth.json entries and re-hydrates the
// provider set. Ownership rule (round-2 review, C1/C4): an entry the
// writer itself could have rendered — exactly {"type":"api_key"} with a
// non-empty key and no other fields (isWriterOwnedAPIKeyEntry) — is
// writer-owned regardless of whether the provider appears in
// models.json:
//
//   - a seeded provider with an empty APIKey (first-party providers
//     render models-only entries; their credential lives only here)
//     gets its key re-hydrated, completing the restart round-trip;
//   - an id absent from models.json (a first-party provider with no
//     declared model list renders nothing there) becomes a seeded
//     first-party providerEntry — so a later Providers clear or
//     key rotation can actually remove/replace the entry instead of
//     preserving it as an undeletable foreign extra forever.
//
// Everything else (pi-written OAuth credentials, degenerate or
// annotated api_key shapes the writer did not produce) is a foreign
// extra preserved verbatim. The relay id never hydrates (its
// credential rides models.json apiKey, never auth.json). In the
// platform context provider keys are platform-managed (headless RPC
// pi, materialize re-applies the full set each boot) — adopting
// type-api_key entries as writer-owned is the fail-closed direction
// for a platform-managed file. An unparseable file sets authExtrasOK
// false so renders leave it alone.
func (w *ConfigWriter) loadExistingAuth() {
	data, err := os.ReadFile(w.authPath())
	if err != nil {
		w.authExtrasOK = true // absent file: nothing to preserve, writer may create
		return
	}
	var auth map[string]json.RawMessage
	if json.Unmarshal(data, &auth) != nil {
		// Corrupt: pi owns this file; never write it.
		w.authExtrasOK = false
		return
	}
	w.authExtrasOK = true
	extras := make(map[string]json.RawMessage, len(auth))
	for k, v := range auth {
		if !isRelayID(k) && isWriterOwnedAPIKeyEntry(v) {
			var entry authEntryJSON
			_ = json.Unmarshal(v, &entry) // shape verified by the predicate
			if p := w.seededProvider(k); p != nil {
				if p.APIKey == "" {
					p.APIKey = entry.Key
				}
			} else {
				w.providers = append(w.providers, providerEntry{ID: k, APIKey: entry.Key})
			}
			continue
		}
		extras[k] = v
	}
	sortProviders(w.providers)
	w.authExtra = extras
}

// isWriterOwnedAPIKeyEntry reports whether raw is EXACTLY an entry this
// writer could have rendered: {"type":"api_key","key":"<non-empty>"} and
// nothing else. Anything else — an empty key (degenerate), extra fields
// (pi/user-authored annotations on an api_key shape) — is foreign and
// preserved verbatim as an extra, so the restart round-trip never
// silently rewrites or drops bytes the writer does not own (round-2
// review O2/O3).
func isWriterOwnedAPIKeyEntry(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return false
	}
	if _, ok := fields["type"]; !ok {
		return false
	}
	if _, ok := fields["key"]; !ok {
		return false
	}
	var entry authEntryJSON
	if json.Unmarshal(raw, &entry) != nil || entry.Type != authTypeAPIKey {
		return false
	}
	return entry.Key != ""
}

// seededProvider returns the seeded provider entry with id, or nil.
func (w *ConfigWriter) seededProvider(id string) *providerEntry {
	for i := range w.providers {
		if w.providers[i].ID == id {
			return &w.providers[i]
		}
	}
	return nil
}

// loadExistingMCP captures the on-disk mcpServers section (user-staged
// servers); re-emitted until a staged MCPServers source supersedes it.
// An unparseable file (or a non-object mcpServers value) sets mcpOK
// false — the same fail-closed leave-alone contract as corrupt
// auth.json/settings.json (round-2 review C5): a boot normalize must
// not delete user-staged MCP state the writer could not understand.
func (w *ConfigWriter) loadExistingMCP() {
	w.mcpOK = true
	data, err := os.ReadFile(w.mcpPath())
	if err != nil || len(data) == 0 {
		return
	}
	var doc struct {
		MCPServers json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil || len(doc.MCPServers) == 0 {
		w.mcpOK = false
		return
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(doc.MCPServers, &servers) != nil {
		w.mcpOK = false
		return
	}
	w.mcpRaw = servers // nil (a JSON null) counts as parseable-empty
}

// loadExistingSettings captures settings.json minus the writer-owned
// defaultProvider/defaultModel keys, seeding the Model source from them
// when it is empty (opencode loadExisting parity: the boot model
// survives writer re-construction and later partial Applies). Corrupt
// file → settingsOK=false → renders leave the file alone (pi owns it).
func (w *ConfigWriter) loadExistingSettings() {
	data, err := os.ReadFile(w.settingsPath())
	if err != nil {
		w.settingsOK = true
		return
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(data, &settings) != nil {
		w.settingsOK = false
		return
	}
	w.settingsOK = true
	if w.model == "" {
		var provider, model string
		if raw, ok := settings["defaultProvider"]; ok {
			_ = json.Unmarshal(raw, &provider)
		}
		if raw, ok := settings["defaultModel"]; ok {
			_ = json.Unmarshal(raw, &model)
		}
		if provider != "" && model != "" {
			w.model = provider + "/" + model
		}
	}
	delete(settings, "defaultProvider")
	delete(settings, "defaultModel")
	w.settingsRaw = settings
}

// loadAdminPrompt reads the admin-configured system prompt file (path
// set via WithAdminPromptPath). Loaded once at construction; runtime
// updates ride the AdminPrompt Apply source. Silent on error — a missing
// file means no platform prompt, which is a valid state.
func (w *ConfigWriter) loadAdminPrompt() {
	if w.adminPromptPath == "" {
		return
	}
	data, err := os.ReadFile(w.adminPromptPath) //nolint:gosec // path from operator-controlled option
	if err != nil || len(data) == 0 {
		return
	}
	w.adminPrompt = string(data)
}

// --- rendering ----------------------------------------------------------

// modelsDoc is the models.json shape (models-configuration.md).
type modelsDoc struct {
	Schema    string                  `json:"$schema"`
	Providers map[string]providerJSON `json:"providers"`
}

type providerJSON struct {
	Name    string      `json:"name,omitempty"`
	BaseURL string      `json:"baseUrl,omitempty"`
	API     string      `json:"api,omitempty"`
	APIKey  string      `json:"apiKey,omitempty"`
	Models  []modelJSON `json:"models,omitempty"`
}

type modelJSON struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Input         []string `json:"input,omitempty"`
}

// authEntryJSON is auth.json's api-key credential shape (providers.md).
type authEntryJSON struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// mcpDoc is the mcp.json shape (mcp.md). Entry field order follows the
// documented examples: stdio command/args/env, then url/headers, shared
// timeout last.
type mcpDoc struct {
	MCPServers map[string]mcpEntryJSON `json:"mcpServers"`
}

type mcpEntryJSON struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

// fileAction is what renderLocked should do with one config file.
type fileAction int

const (
	// fileLeave: pi owns the file and the writer does not understand it
	// (unparseable at construction) — never written, never removed.
	fileLeave fileAction = iota
	// fileRemove: the source renders nothing; an absent file is pi's
	// default state for every optional source.
	fileRemove
	// fileWrite: replace the file with the rendered bytes.
	fileWrite
)

// renderLocked re-renders every config file from the current sources.
// All bytes are built in memory first; writes happen afterwards so a
// build error touches nothing on disk. Each file is written atomically
// (temp + rename); see Apply for the failure window this leaves.
func (w *ConfigWriter) renderLocked() error {
	providerSet := w.renderedProvidersLocked()

	modelJSONBytes, modelAct := w.buildModelsJSON(providerSet)
	authJSONBytes, authAct := w.buildAuthJSON(providerSet)
	mcpJSONBytes, mcpAct, err := w.buildMCPJSON()
	if err != nil {
		return err
	}
	settingsJSONBytes, settingsAct, err := w.buildSettingsJSON()
	if err != nil {
		return err
	}

	// #nosec G301 -- 0755 matches pi's own agent-dir default (the
	// runtime uid must traverse it); config files inside carry 0640/0660.
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", w.dir, err)
	}

	// auth.json: 0o660 — the #1296 uid-split mode (the consuming uid must
	// be able to write credential updates, shared gid carries both).
	if err := w.applyFile(w.modelsPath(), modelJSONBytes, modelAct, 0o640); err != nil {
		return err
	}
	if err := w.applyFile(w.authPath(), authJSONBytes, authAct, 0o660); err != nil {
		return err
	}
	if err := w.applyFile(w.mcpPath(), mcpJSONBytes, mcpAct, 0o640); err != nil {
		return err
	}
	if err := w.applyFile(w.settingsPath(), settingsJSONBytes, settingsAct, 0o640); err != nil {
		return err
	}
	if err := w.writePromptFile(); err != nil {
		return err
	}
	return w.writeModelResolutionMarker(providerSet)
}

// renderedProvidersLocked is the ordered provider list models.json (and
// model resolution) render from: source providers plus the relay entry
// (when applied with a URL). First-party source entries (no BaseURL)
// render an allowlist-only models.json entry when they declare models;
// custom endpoints render full entries.
func (w *ConfigWriter) renderedProvidersLocked() []providerEntry {
	out := make([]providerEntry, 0, len(w.providers)+1)
	out = append(out, w.providers...)
	if w.relay != nil && w.relay.URL != "" {
		models := make([]providerSourceModel, 0, len(w.relay.Models))
		for _, m := range w.relay.Models {
			models = append(models, providerSourceModel{
				ID: m.ID, Name: m.Name, Context: m.ContextLimit, Output: m.OutputLimit,
			})
		}
		out = append(out, providerEntry{
			ID:      relayProviderID,
			BaseURL: w.relay.URL,
			APIKey:  w.relayAPIKey,
			API:     openaiCompletionsAPI,
			Models:  models, // non-nil even when empty: the relay claims exactly its model list
		})
	}
	sortProviders(out)
	return out
}

// buildModelsJSON renders models.json. Returns fileRemove when no
// provider would be rendered — an absent models.json is pi's default
// state, so the file is removed instead of emptied.
func (w *ConfigWriter) buildModelsJSON(providerSet []providerEntry) ([]byte, fileAction) {
	providers := make(map[string]providerJSON, len(providerSet))
	for _, p := range providerSet {
		entry := providerJSON{API: p.API, APIKey: p.APIKey, BaseURL: p.BaseURL}
		if entry.API == "" && p.BaseURL != "" {
			// Custom endpoint without a declared protocol: the platform's
			// formatter shapes all custom endpoints OpenAI-compatible (the
			// opencode npm "@ai-sdk/openai-compatible" ⇔ baseURL pairing).
			// A pi-native declared api (e.g. anthropic-messages) wins.
			entry.API = openaiCompletionsAPI
		}
		for _, m := range p.Models {
			mj := modelJSON{ID: m.ID, Name: m.Name, ContextWindow: m.Context, MaxTokens: m.Output}
			if m.ImageInput {
				mj.Input = []string{"text", "image"}
			}
			entry.Models = append(entry.Models, mj)
		}
		if p.BaseURL == "" {
			// First-party provider: the credential rides auth.json (the
			// auth.json-keyed restart contract), so models.json carries
			// at most the declared model additions — and nothing at all
			// when no list is declared (pi's catalog supplies those).
			entry.APIKey = ""
			if len(entry.Models) == 0 {
				continue
			}
		}
		providers[p.ID] = entry
	}
	if len(providers) == 0 {
		return nil, fileRemove
	}
	doc := modelsDoc{Schema: modelsSchemaURL, Providers: providers}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fileLeave // unreachable: the shape is marshal-safe
	}
	return b, fileWrite
}

// buildAuthJSON renders auth.json: first-party provider credentials
// (type api_key) over the preserved non-writer-owned extras. fileLeave
// when the on-disk file is pi-owned garbage; fileRemove when the render
// is empty (the file could only contain writer-rendered output —
// construction captured everything else into extras).
func (w *ConfigWriter) buildAuthJSON(providerSet []providerEntry) ([]byte, fileAction) {
	if !w.authExtrasOK {
		return nil, fileLeave // pi-owned unparseable file: leave alone
	}
	// extras are raw JSON captured at construction — re-emit verbatim
	// rather than re-typing them (OAuth entries carry fields the api_key
	// struct does not model).
	out := make(map[string]json.RawMessage, len(w.authExtra)+len(providerSet))
	for k, v := range w.authExtra {
		out[k] = v
	}
	for _, p := range providerSet {
		if p.BaseURL != "" {
			continue // custom endpoint: credential rides models.json apiKey
		}
		if p.APIKey == "" {
			continue
		}
		entry, err := json.Marshal(authEntryJSON{Type: authTypeAPIKey, Key: p.APIKey})
		if err != nil {
			continue
		}
		out[p.ID] = entry
	}
	if len(out) == 0 {
		return nil, fileRemove
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fileLeave // unreachable: raw-message map is marshal-safe
	}
	return b, fileWrite
}

// buildMCPJSON renders mcp.json: staged servers are authoritative; with
// no staged source the section captured at construction is re-emitted.
// fileLeave when the on-disk file was unparseable (pi/user-owned —
// never clobbered); fileRemove when nothing is left to declare.
func (w *ConfigWriter) buildMCPJSON() ([]byte, fileAction, error) {
	if !w.mcpOK {
		return nil, fileLeave, nil
	}
	servers := make(map[string]mcpEntryJSON)
	if len(w.mcpServers) > 0 {
		for _, srv := range w.mcpServers {
			servers[srv.Name] = renderMCPServerEntry(srv)
		}
	} else if len(w.mcpRaw) > 0 {
		b, err := json.MarshalIndent(mcpDocRaw{MCPServers: w.mcpRaw}, "", "  ")
		if err != nil {
			return nil, fileLeave, fmt.Errorf("marshal mcp.json: %w", err)
		}
		return b, fileWrite, nil
	}
	if len(servers) == 0 {
		return nil, fileRemove, nil
	}
	b, err := json.MarshalIndent(mcpDoc{MCPServers: servers}, "", "  ")
	if err != nil {
		return nil, fileLeave, fmt.Errorf("marshal mcp.json: %w", err)
	}
	return b, fileWrite, nil
}

// mcpDocRaw re-emits the captured on-disk section verbatim.
type mcpDocRaw struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// renderMCPServerEntry converts one agent.MCPServerEntry to pi's
// mcpServers shape. Remote transports (http, sse) render as URL entries
// — pi has no SSE transport ("The legacy SSE transport is not
// supported", mcp.md) and skips invalid entries itself, so an sse URL is
// rendered as-is and reported by `pi mcp list` rather than silently
// dropped here. stdio renders as command/args/env. TimeoutMs converts
// to pi's integer-seconds timeout (ceil; a sub-second value keeps 1s).
func renderMCPServerEntry(srv agent.MCPServerEntry) mcpEntryJSON {
	entry := mcpEntryJSON{}
	if srv.Transport == "http" || srv.Transport == "sse" {
		entry.URL = srv.URL
		if len(srv.Headers) > 0 {
			entry.Headers = srv.Headers
		}
	} else {
		entry.Command = srv.Command
		if len(srv.Args) > 0 {
			entry.Args = srv.Args
		}
		if len(srv.Env) > 0 {
			entry.Env = srv.Env
		}
	}
	if srv.TimeoutMs > 0 {
		entry.Timeout = (srv.TimeoutMs + 999) / 1000
	}
	return entry
}

// buildSettingsJSON renders settings.json: the captured settings (minus
// writer-owned keys) plus the resolved defaultProvider/defaultModel.
// Model resolution parity: an unresolvable selection is OMITTED (a bare
// or stale id poisons pi's startup model exactly as it poisoned
// opencode's — incident 2026-08-16) and lands in the warning marker
// instead; pi then applies its own default and the selection self-heals
// on a render where the provider exists again.
func (w *ConfigWriter) buildSettingsJSON() ([]byte, fileAction, error) {
	if !w.settingsOK {
		return nil, fileLeave, nil // pi-owned unparseable file: leave alone
	}
	settings := make(map[string]json.RawMessage, len(w.settingsRaw)+2)
	for k, v := range w.settingsRaw {
		settings[k] = v
	}
	delete(settings, "defaultProvider")
	delete(settings, "defaultModel")
	providerSet := w.renderedProvidersLocked()
	if provider, modelID, ok := w.resolveModelLocked(providerSet); ok {
		pJSON, err := json.Marshal(provider)
		if err != nil {
			return nil, fileLeave, fmt.Errorf("marshal defaultProvider: %w", err)
		}
		mJSON, err := json.Marshal(modelID)
		if err != nil {
			return nil, fileLeave, fmt.Errorf("marshal defaultModel: %w", err)
		}
		settings["defaultProvider"] = pJSON
		settings["defaultModel"] = mJSON
	}
	if len(settings) == 0 {
		// Nothing writer-owned and nothing captured: an absent file is
		// pi's default state.
		return nil, fileRemove, nil
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fileLeave, fmt.Errorf("marshal settings.json: %w", err)
	}
	return b, fileWrite, nil
}

// resolveModelLocked qualifies and verifies the model source against the
// rendered provider set — the pi equivalent of the opencode pipeline's
// resolveModelWithProvider (cmd/workspace-agentd/secrets.go), run inside
// the writer because pi has no external boot resolver.
//
//	qualified "prov/model": prov must exist; an allowlisted provider must
//	  CLAIM the model (a first-party provider without a declared list
//	  cannot be verified config-side — existence stands).
//	flat "model": the first provider (sorted by id, deterministic)
//	  whose allowlist claims it qualifies it.
//
// Split on the FIRST "/" (a bare id parses as provider + empty tail and
// is rejected — the incident's own parse shape).
func (w *ConfigWriter) resolveModelLocked(providerSet []providerEntry) (provider, modelID string, ok bool) {
	if w.model == "" {
		return "", "", false
	}
	if idx := strings.Index(w.model, "/"); idx > 0 && idx < len(w.model)-1 {
		wantProv, wantModel := w.model[:idx], w.model[idx+1:]
		for _, p := range providerSet {
			if p.ID != wantProv {
				continue
			}
			if len(p.Models) == 0 {
				return wantProv, wantModel, true
			}
			for _, m := range p.Models {
				if m.ID == wantModel {
					return wantProv, wantModel, true
				}
			}
		}
		return "", "", false
	}
	// Flat: scan the allowlisted providers (sorted) for a claim.
	for _, p := range providerSet {
		for _, m := range p.Models {
			if m.ID == w.model {
				return p.ID, m.ID, true
			}
		}
	}
	return "", "", false
}

// writePromptFile renders APPEND_SYSTEM.md (never SYSTEM.md — see the
// package comment). Empty source removes the file: absence is pi's
// no-append default, and a stale file would keep injecting a cleared
// prompt.
func (w *ConfigWriter) writePromptFile() error {
	path := w.appendSystemPath()
	if w.adminPrompt == "" {
		return removeIfExists(path)
	}
	return atomicWrite(path, []byte(w.adminPrompt), 0o640)
}

// writeModelResolutionMarker maintains the unresolvable-model marker
// beside the agent dir: written when the model source is set but
// unresolvable against this render's provider set; removed when the
// selection resolves or no selection exists (stale-marker hygiene,
// applyWorkspaceConfig parity).
func (w *ConfigWriter) writeModelResolutionMarker(providerSet []providerEntry) error {
	path := w.warnPath()
	if path == "" {
		return nil
	}
	if w.model == "" {
		return removeIfExists(path)
	}
	if _, _, ok := w.resolveModelLocked(providerSet); ok {
		return removeIfExists(path)
	}
	payload, err := json.Marshal(struct {
		DefaultModel string `json:"defaultModel"`
	}{DefaultModel: w.model})
	if err != nil {
		return nil // non-fatal: only the user-visible warning is lost
	}
	return atomicWrite(path, payload, 0o640)
}

// --- path helpers -------------------------------------------------------

func (w *ConfigWriter) modelsPath() string       { return filepath.Join(w.dir, "models.json") }
func (w *ConfigWriter) authPath() string         { return filepath.Join(w.dir, "auth.json") }
func (w *ConfigWriter) mcpPath() string          { return filepath.Join(w.dir, "mcp.json") }
func (w *ConfigWriter) settingsPath() string     { return filepath.Join(w.dir, "settings.json") }
func (w *ConfigWriter) appendSystemPath() string { return filepath.Join(w.dir, "APPEND_SYSTEM.md") }
func (w *ConfigWriter) warnPath() string {
	if w.warnPathOverride != "" {
		return w.warnPathOverride
	}
	if w.dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(w.dir), modelResolutionWarningFile)
}

// applyFile enacts one fileAction: fileWrite atomically replaces the
// file with bytes at perm; fileRemove removes it; fileLeave touches
// nothing.
func (w *ConfigWriter) applyFile(path string, data []byte, action fileAction, perm os.FileMode) error {
	switch action {
	case fileLeave:
		return nil
	case fileRemove:
		return removeIfExists(path)
	default:
		return atomicWrite(path, data, perm)
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// atomicWrite writes data to a temp file in path's directory, then
// renames it into place (readers never observe a partial file; rename
// is atomic on POSIX, same-mount via same-dir temp).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pi-config-*.tmp")
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

// --- providers parsing --------------------------------------------------

// parseProvidersFormatted parses the Providers source bytes the platform
// produces. Two shapes are accepted:
//
//   - the opencode formatter's output (FormatOpenCodeConfig:
//     {"$schema", "provider": {"<id>": {"npm", "options":
//     {"apiKey","baseURL"}, "models": {...}}}, "model"}) — the shape
//     every existing Apply caller (secrets pipeline, relay injector
//     paths) produces today, which is why pi accepts it verbatim: the
//     existing callers work unmodified. The embedded "model" key is
//     IGNORED — the Model source is owned by its own Apply field
//     (opencode SetProviders parity).
//   - pi-native {"providers": {...models.json shape...}} — the shape a
//     future pi AgentRuntime formatter will emit.
func parseProvidersFormatted(formatted []byte) ([]providerEntry, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(formatted, &top); err != nil {
		return nil, fmt.Errorf("parse provider payload: %w", err)
	}
	if raw, ok := top["provider"]; ok {
		return parseOpenCodeProviders(raw)
	}
	if raw, ok := top["providers"]; ok {
		return parsePiProviders(raw)
	}
	return nil, fmt.Errorf("provider payload has neither \"provider\" (opencode shape) nor \"providers\" (pi shape) key")
}

// isRelayID reports whether id is the relay provider id — owned
// exclusively by the Relay source. A formatted payload carrying it (e.g.
// one rendered by a writer that merged the relay into its provider map,
// as the opencode writer does) would otherwise collide with the relay
// entry rendered from the Relay source.
func isRelayID(id string) bool { return id == relayProviderID }

// parseOpenCodeProviders converts the opencode provider map into the
// writer's provider entries. npm=="@ai-sdk/openai-compatible" ⇔ the
// formatter set it for baseURL-carrying providers; the baseURL itself
// is the discriminator here.
func parseOpenCodeProviders(raw json.RawMessage) ([]providerEntry, error) {
	var providers map[string]struct {
		Options struct {
			APIKey  string `json:"apiKey"`
			BaseURL string `json:"baseURL"`
		} `json:"options"`
		Models map[string]struct {
			Name  string `json:"name"`
			Limit *struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Attachment *bool `json:"attachment"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, fmt.Errorf("parse opencode provider map: %w", err)
	}
	entries := make([]providerEntry, 0, len(providers))
	for id, p := range providers {
		if isRelayID(id) {
			continue
		}
		entry := providerEntry{ID: id, BaseURL: p.Options.BaseURL, APIKey: p.Options.APIKey}
		// Deterministic model order (map iteration is random; pi's
		// models[] is an array and golden renders must be stable).
		ids := make([]string, 0, len(p.Models))
		for mID := range p.Models {
			ids = append(ids, mID)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			entry.Models = make([]providerSourceModel, 0, len(ids))
		}
		for _, mID := range ids {
			m := p.Models[mID]
			pm := providerSourceModel{ID: mID, Name: m.Name}
			if m.Limit != nil {
				pm.Context = m.Limit.Context
				pm.Output = m.Limit.Output
			}
			if m.Attachment != nil && *m.Attachment {
				pm.ImageInput = true
			}
			entry.Models = append(entry.Models, pm)
		}
		entries = append(entries, entry)
	}
	sortProviders(entries)
	return entries, nil
}

// parsePiProviders converts a pi-native providers map (the shape this
// writer itself renders) into provider entries.
func parsePiProviders(raw json.RawMessage) ([]providerEntry, error) {
	var providers map[string]struct {
		BaseURL string `json:"baseUrl"`
		API     string `json:"api"`
		APIKey  string `json:"apiKey"`
		Models  []struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			ContextWindow int      `json:"contextWindow"`
			MaxTokens     int      `json:"maxTokens"`
			Input         []string `json:"input"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, fmt.Errorf("parse pi provider map: %w", err)
	}
	entries := make([]providerEntry, 0, len(providers))
	for id, p := range providers {
		if isRelayID(id) {
			continue
		}
		entry := providerEntry{ID: id, BaseURL: p.BaseURL, API: p.API, APIKey: p.APIKey}
		for _, m := range p.Models {
			pm := providerSourceModel{ID: m.ID, Name: m.Name, Context: m.ContextWindow, Output: m.MaxTokens}
			for _, in := range m.Input {
				if in == "image" {
					pm.ImageInput = true
				}
			}
			entry.Models = append(entry.Models, pm)
		}
		sortModels(entry.Models)
		entries = append(entries, entry)
	}
	sortProviders(entries)
	return entries, nil
}

func sortProviders(entries []providerEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for _, e := range entries {
		sortModels(e.Models)
	}
}

func sortModels(models []providerSourceModel) {
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
}
