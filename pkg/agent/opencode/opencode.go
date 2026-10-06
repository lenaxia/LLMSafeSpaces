// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"encoding/json"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

type OpenCodeAgent struct{}

func (a *OpenCodeAgent) Type() agent.AgentType { return agent.AgentTypeOpenCode }

func (a *OpenCodeAgent) ValidateCredentials(rawConfig []byte) (*agent.CredentialCheckResult, error) {
	if len(rawConfig) == 0 || string(rawConfig) == "{}" {
		return &agent.CredentialCheckResult{
			State:   agent.CredentialStateMissing,
			Agent:   agent.AgentTypeOpenCode,
			Message: "empty config",
		}, nil
	}
	var config map[string]interface{}
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		return &agent.CredentialCheckResult{
			State:   agent.CredentialStateInvalid,
			Agent:   agent.AgentTypeOpenCode,
			Message: "invalid JSON",
		}, nil
	}
	if len(config) == 0 {
		return &agent.CredentialCheckResult{
			State:   agent.CredentialStateMissing,
			Agent:   agent.AgentTypeOpenCode,
			Message: "empty config object",
		}, nil
	}
	return &agent.CredentialCheckResult{
		State: agent.CredentialStatePresent,
		Agent: agent.AgentTypeOpenCode,
	}, nil
}

func (a *OpenCodeAgent) FormatProviderConfig(providers []agent.LLMProviderData) ([]byte, error) {
	// Convert agent.LLMProviderData to secrets.LLMProviderData for the formatter.
	secProviders := make([]secrets.LLMProviderData, len(providers))
	for i, p := range providers {
		models := make([]secrets.LLMModelConfig, len(p.Models))
		for j, m := range p.Models {
			models[j] = secrets.LLMModelConfig{
				ID:           m.ID,
				Label:        m.Label,
				ContextLimit: m.ContextLimit,
				OutputLimit:  m.OutputLimit,
				Attachment:   m.Attachment,
			}
		}
		secProviders[i] = secrets.LLMProviderData{
			Kind:       p.Kind,
			Slug:       p.Slug,
			APIKey:     p.APIKey,
			BaseURL:    p.BaseURL,
			Models:     models,
			Default:    p.Default,
			SmallModel: p.SmallModel,
		}
	}
	return FormatOpenCodeConfig(secProviders)
}
