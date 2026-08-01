// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// FormatOpenCodeConfig renders a slice of validated LLMProviderData into
// the JSON shape opencode accepts.
//
// **Schema** (evidence-driven; established by live cluster probe in
// worklog 0128. Do NOT change without re-validating against a running
// opencode):
//
//	{
//	  "$schema": "https://opencode.ai/config.json",
//	  "provider": {                          <-- SINGULAR (not "providers")
//	    "<id>": {
//	      "options": {                       <-- direct, NO aisdk wrapper
//	        "apiKey":  "...",                <-- the credential
//	        "baseURL": "..."                 <-- in options, NOT in a
//	      },                                     separate `endpoint` object
//	      "models": { "<id>": { "name": "..." } }
//	    }
//	  },
//	  "model": "<id>/<modelID>"
//	}
//
// What pre-fix code generated, and why opencode rejected it:
//   - top-level key was `providers` (plural) → ConfigInvalidError
//   - apiKey lived at options.aisdk.provider.apiKey → ConfigInvalidError
//   - baseURL lived at endpoint.url → silently ignored (chat requests
//     went to api.openai.com instead of the operator's endpoint)
//
// The function is pure — no side effects, no filesystem access.
//
// Returns an error if providers is empty (callers MUST check for this
// — opencode treats an empty config differently and a "no-op write of
// an empty config" is a bug).
func FormatOpenCodeConfig(providers []secrets.LLMProviderData) ([]byte, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("FormatOpenCodeConfig: no providers to render")
	}

	cfg := opencodeConfig{
		Schema:   "https://opencode.ai/config.json",
		Provider: make(map[string]*opencodeProvider, len(providers)),
	}

	for _, p := range providers {
		op := &opencodeProvider{
			Options: opencodeOptions{
				APIKey:  p.APIKey,
				BaseURL: p.BaseURL, // empty string is omitted via omitempty
			},
		}

		// Providers with a custom BaseURL are OpenAI-compatible third-party
		// endpoints (e.g. LiteLLM proxies). Set npm so opencode uses the
		// @ai-sdk/openai-compatible SDK, which calls /v1/chat/completions.
		// Without this, built-in provider IDs like "openai" trigger opencode's
		// first-party OpenAI SDK which calls /v1/responses — a path that most
		// LiteLLM proxies don't expose.
		if p.BaseURL != "" {
			op.NPM = "@ai-sdk/openai-compatible"
		}

		if len(p.Models) > 0 {
			op.Models = make(map[string]*opencodeModel, len(p.Models))
			for _, m := range p.Models {
				om := &opencodeModel{}
				if m.Label != "" {
					om.Name = m.Label
				}
				// opencode's published JSON Schema requires BOTH context and
				// output when `limit` is present (required: ["context", "output"],
				// additionalProperties: false). Emitting a partial block makes
				// opencode reject the entire config with SchemaError, so we omit
				// the whole block when either value is missing. Re-validate against
				// https://opencode.ai/config.json before relaxing this rule.
				if m.ContextLimit > 0 && m.OutputLimit > 0 {
					om.Limit = &opencodeModelLimit{
						Context: m.ContextLimit,
						Output:  m.OutputLimit,
					}
				}
				op.Models[m.ID] = om
			}
		}

		// Key the provider map by Slug — this is the literal value
		// opencode persists as `providerID` on session records. Pre-
		// Epic-55 this was keyed by Provider (the SDK kind), causing
		// identity collisions when two credentials of the same SDK
		// kind existed for the same owner.
		cfg.Provider[p.Slug] = op

		// First provider with a Default wins.
		if cfg.Model == "" && p.Default != "" {
			cfg.Model = p.Default
		}
	}

	// json.Marshal is deterministic for maps (sorted keys) since Go 1.12.
	return json.MarshalIndent(cfg, "", "  ")
}

// --- internal types (not exported) ---
//
// JSON tag ordering matters for the snapshot test. Go's json package
// emits fields in struct-declaration order (NOT alphabetical), so the
// field order below is the wire-format order.

type opencodeConfig struct {
	Schema   string                       `json:"$schema"`
	Provider map[string]*opencodeProvider `json:"provider"`
	Model    string                       `json:"model,omitempty"`
}

type opencodeProvider struct {
	// NPM specifies the AI SDK package to use for this provider.
	// When set to "@ai-sdk/openai-compatible", opencode uses the generic
	// OpenAI-compatible SDK which calls /v1/chat/completions. This MUST
	// be set for any provider with a custom BaseURL — if omitted, opencode
	// treats built-in provider IDs (like "openai") as first-party and uses
	// their native SDK (which may call /v1/responses or other non-standard
	// paths that a LiteLLM proxy won't support).
	NPM     string                    `json:"npm,omitempty"`
	Options opencodeOptions           `json:"options"`
	Models  map[string]*opencodeModel `json:"models,omitempty"`
}

type opencodeOptions struct {
	APIKey  string `json:"apiKey,omitempty"`
	BaseURL string `json:"baseURL,omitempty"`
}

type opencodeModel struct {
	Name  string              `json:"name,omitempty"`
	Limit *opencodeModelLimit `json:"limit,omitempty"`
}

// opencodeModelLimit mirrors opencode's published JSON Schema for the model
// `limit` object (https://opencode.ai/config.json). The schema declares
// `required: ["context", "output"]` with `additionalProperties: false`, so
// callers MUST set both fields. FormatOpenCodeConfig enforces this by only
// allocating opencodeModelLimit when both are non-zero. The `input` field
// in the schema is optional and intentionally not modeled — opencode treats
// `input` as a separate cap from `context` and we have no source of truth
// for it; omitting it leaves opencode's built-in default in effect.
type opencodeModelLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}
