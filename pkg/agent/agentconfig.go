// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agent — AgentConfigWriter seam (US-65.1, design 0049 §5).
//
// This file defines the seam between platform code (cmd/workspace-agentd)
// and the agent-specific config writer. Platform code holds an
// AgentConfigWriter (interface) and calls Apply, branching on the
// returned restartRequired. The concrete implementation owns every
// detail of the underlying agent's config-merge semantics, file
// format, and restart policy.
//
// The opencode implementation lives in pkg/agent/opencode/. Platform
// code constructs the concrete type at boot (analogous to registering
// a database driver) and then treats it as the interface for every
// subsequent call.
//
// Design note — why partial-update input (Rule 12 override):
//
// design/0049 §5 specifies Apply(AgentConfigInput) and lists three
// sources (Providers, Model, Relay). A literal reading implies a
// full-replace input where the caller supplies all sources on every
// call. Code-study of the existing call sites
// (cmd/workspace-agentd/{secrets,relay_injector,pre_boot_relay}.go)
// shows that model is wrong: every existing caller updates ONE source
// per Apply call:
//
//   - the secrets apply pipeline: providers + MCP servers (model and relay
//     must survive from boot and injection respectively)
//   - relay_injector: relay only (providers came from init container's
//     FlushProviders, model came from workspace-config.json)
//   - pre_boot_relay: relay only (same source-of-truth split)
//
// Forcing each caller to read the writer's existing state to preserve
// sources it does not own would invert the writer's role as
// state-holder and re-introduce exactly the multiple-writer race
// fragility US-46.10 eliminated. The input is therefore a
// partial-update: pointer fields where nil = "leave this source
// unchanged".
//
// This file was deferred in worklog 0700 (Rule 12: speculative
// abstraction against a single consumer). User direction overrides
// that call: the seam is the foundation US-65.3 (opencode adapter)
// builds on, and shipping the adapter on top of an unsealed seam
// would propagate the leakage the design doc exists to fix.

package agent

// AgentConfigWriter is the seam between platform code and the
// agent-specific config writer. The interface is the ONLY surface
// platform code holds after construction; it does not import or
// reference the concrete type.
//
// Methods:
//
//   - Apply merges the supplied input onto the writer's existing
//     state and writes the result atomically. Returns
//     restartRequired=true when the running agent process must be
//     restarted to pick up the change (the opencode implementation
//     always returns true — opencode does not hot-reload its config
//     file). A future agent that hot-reloads returns false and the
//     platform's restart machinery no-ops.
//
//   - HasRelay reports whether the writer's current state includes a
//     relay block. Used by the readyz handler and the relay injector
//     short-circuit. Read-only — does not write.
//
// Thread-safety: implementations must be safe for concurrent use.
// The opencode implementation guards Apply with a mutex.
type AgentConfigWriter interface {
	Apply(in AgentConfigInput) (restartRequired bool, err error)
	HasRelay() bool
}

// AgentConfigInput is the partial-update payload for Apply. Each
// pointer field is one source; nil means "leave the writer's existing
// state for this source unchanged". A non-nil pointer to a zero-value
// struct means "this source is now empty/disabled" — e.g.
// `Relay: &RelayState{}` clears the relay source.
//
// Fields are pointers (not values + a "set" bool) because:
//   - Pointer-omission composes cleanly with JSON encoding semantics
//     callers already understand.
//   - The zero value of the input struct is a valid no-op Apply.
type AgentConfigInput struct {
	// Providers updates the LLM provider map. Formatted is the
	// agent-rendered provider JSON — the agent's formatter is the
	// only thing that knows the shape (e.g. opencode's
	// {"provider": {...}} struct). The writer passes the bytes
	// through verbatim.
	//
	// A non-nil pointer with nil/empty Formatted bytes clears the
	// provider source.
	Providers *AgentProvidersChange

	// Model updates the default model selection. Empty string
	// (ModelSelection("")) clears the model source.
	Model *ModelSelection

	// Relay updates the relay state. A non-nil pointer with empty URL
	// clears the relay source (distinct from nil = leave unchanged).
	Relay *RelayState

	// MCPServers updates the MCP server list. A non-nil pointer with
	// an empty Servers slice clears the MCP source.
	MCPServers *MCPServerChange

	// AdminPrompt updates the platform-level system prompt rendered into
	// the agent's build prompt (replace semantics: the new Text fully
	// supersedes the prior source, side-car-loaded or rendered). A
	// non-nil pointer with empty Text clears the prompt source
	// (distinct from nil = leave unchanged).
	//
	// At bootstrap the writer loads this source from the side-car file
	// (WithAdminPromptPath — the materialize staging contract); this
	// field is the runtime update path so a caller can revise the
	// prompt without recreating the writer. It does NOT watch files.
	AdminPrompt *AdminPromptChange

	// AllowedDirs updates the glob patterns auto-approved as the
	// agent's external-directory allow rules. A non-nil pointer with an
	// empty Dirs slice clears the source. Same bootstrap/runtime split
	// as AdminPrompt: construction may load from the side-car file
	// (WithAllowedDirsPath); this field updates thereafter.
	AllowedDirs *AllowedDirsChange
}

// AdminPromptChange is the admin-prompt-source update payload. See
// AgentConfigInput.AdminPrompt for semantics.
type AdminPromptChange struct {
	Text string
}

// AllowedDirsChange is the allowed-dirs-source update payload. See
// AgentConfigInput.AllowedDirs for semantics.
type AllowedDirsChange struct {
	Dirs []string
}

// AgentProvidersChange is the provider-source update payload. See
// AgentConfigInput.Providers for semantics.
type AgentProvidersChange struct {
	// Formatted is the agent-rendered provider JSON. For opencode,
	// this is the output of FormatOpenCodeConfig — a struct
	// {"provider": {...}} containing the agent's exact provider map.
	Formatted []byte
}

// ModelSelection is the fully-qualified "providerID/modelID" string
// the agent expects in its config. Platform code resolves the
// providerID before calling Apply; the writer does not look it up.
//
// Empty string = clear the model source.
type ModelSelection string

// RelayModel is one free-tier model discovered by the relay
// injector. Mirrors opencode.RelayModel (and the controller's
// freemodels.Model wire format) — the type is agent-neutral at this
// layer; the opencode adapter converts.
type RelayModel struct {
	ID           string
	Name         string
	ContextLimit int
	OutputLimit  int
}

// RelayState describes what the relay injector discovered. A
// pointer-to-zero-value means "relay disabled" (empty URL); nil means
// "leave the existing relay source unchanged". The two are distinct
// and load-bearing — see AgentConfigInput.Relay.
type RelayState struct {
	URL    string
	Models []RelayModel
}

// MCPServerEntry is one MCP server to render into the agent's config.
// Agent-neutral: the opencode adapter converts to opencode's
// remote/local shape via pkg/agentd/secrets.RenderOpencodeMCPServerEntry.
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

// MCPServerChange is the MCP-source update payload. See
// AgentConfigInput.MCPServers for semantics.
type MCPServerChange struct {
	Servers []MCPServerEntry
}
