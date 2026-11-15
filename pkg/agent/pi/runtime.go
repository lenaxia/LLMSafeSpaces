// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// runtime.go — the agent.AgentRuntime seam (credentials) for pi. The
// adapter delegates FormatProviderConfig/ValidateCredentials here so
// the credential surface has one source of truth per harness (the
// opencode delegation posture, PR #714 R3).
//
// The config SHAPE is pi's models.json providers object (the surface
// the ConfigWriter seam renders files from; models-configuration.md):
//
//	{"providers": {"<id>": {"baseUrl": ..., "api": ..., "apiKey": ...,
//	                   "models": [{"id": ..., "contextWindow": ...,
//	                               "maxTokens": ...}]}}}
//
// ValidateCredentials accepts that object (or the bare inner provider
// map): structurally valid = Present, empty/{} = Missing, undecodable
// JSON = Invalid. First-party-key-only credentials (what the platform
// binds when a user pastes an Anthropic/OpenAI key with no baseURL)
// render as provider entries WITHOUT baseUrl — pi's built-in catalog
// already knows the endpoint; only the credential is missing.

import (
	"encoding/json"
	"fmt"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// piAgentRuntime implements agent.AgentRuntime for pi.
type piAgentRuntime struct{}

var _ agent.AgentRuntime = (*piAgentRuntime)(nil)

func (r *piAgentRuntime) Type() agent.AgentType { return agent.AgentTypePi }

// providerConfigEntry is one models.json provider entry (the wire shape
// FormatProviderConfig emits and ValidateCredentials accepts).
type providerConfigEntry struct {
	BaseURL string          `json:"baseUrl,omitempty"`
	API     string          `json:"api,omitempty"`
	APIKey  string          `json:"apiKey,omitempty"`
	Models  []providerModel `json:"models,omitempty"`
}

type providerModel struct {
	ID            string `json:"id"`
	Label         string `json:"label,omitempty"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
}

// FormatProviderConfig renders LLMProviderData in pi's native
// models.json providers shape. Entries with a baseURL become custom
// endpoints (openai-compatible completions API — the relay shape);
// first-party keys (no baseURL) render credential-only entries.
func (r *piAgentRuntime) FormatProviderConfig(providers []agent.LLMProviderData) ([]byte, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	out := map[string]providerConfigEntry{}
	for _, p := range providers {
		if p.Slug == "" {
			return nil, fmt.Errorf("pi provider config: provider %q has no slug", p.Kind)
		}
		e := providerConfigEntry{}
		if p.BaseURL != "" {
			e.BaseURL = p.BaseURL
			e.API = "openai-completions"
		}
		if p.APIKey != "" {
			e.APIKey = p.APIKey
		}
		for _, m := range p.Models {
			e.Models = append(e.Models, providerModel{
				ID:            m.ID,
				Label:         m.Label,
				ContextWindow: int64(m.ContextLimit),
				MaxTokens:     int64(m.OutputLimit),
			})
		}
		out[p.Slug] = e
	}
	return json.MarshalIndent(map[string]any{"providers": out}, "", "  ")
}

// ValidateCredentials checks structural validity of a pi-formatted
// provider config (whole providers object, bare provider map, or a
// single provider entry).
func (r *piAgentRuntime) ValidateCredentials(rawConfig []byte) (*agent.CredentialCheckResult, error) {
	fail := func(state agent.CredentialState, msg string) (*agent.CredentialCheckResult, error) {
		return &agent.CredentialCheckResult{State: state, Agent: agent.AgentTypePi, Message: msg}, nil
	}
	if len(rawConfig) == 0 {
		return fail(agent.CredentialStateMissing, "no pi provider config")
	}
	var doc struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rawConfig, &doc); err != nil {
		return fail(agent.CredentialStateInvalid, "not valid JSON: "+err.Error())
	}
	entries := doc.Providers
	if entries == nil {
		// Bare provider map ({"<id>": {...}}) — one level shallower.
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rawConfig, &m); err != nil {
			return fail(agent.CredentialStateInvalid, "not a providers object: "+err.Error())
		}
		if len(m) == 0 {
			return fail(agent.CredentialStateInvalid, "empty or non-object pi provider config")
		}
		entries = m
	}
	if len(entries) == 0 {
		return fail(agent.CredentialStateMissing, "no providers in pi config")
	}
	for id, raw := range entries {
		var e providerConfigEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fail(agent.CredentialStateInvalid, fmt.Sprintf("provider %q: %v", id, err))
		}
	}
	return fail(agent.CredentialStatePresent, "")
}
