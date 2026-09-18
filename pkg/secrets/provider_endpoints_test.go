// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// provider_endpoints_test.go — US-72.3: the kind→default-upstream table the
// controller staging uses to pin each token's baseURL (design 0058 §4.4:
// "the token names exactly one upstream"), and the stageability set (which
// kinds a Bearer-injecting reverse proxy can front at all).

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProviderDefaultBaseURL(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want string
	}{
		{"openai", "https://api.openai.com/v1"},
		{"anthropic", "https://api.anthropic.com/v1"},
		{"google", "https://generativelanguage.googleapis.com/v1"},
		{"cohere", "https://api.cohere.com/v1"},
		{"mistral", "https://api.mistral.ai/v1"},
		{"perplexity", "https://api.perplexity.ai"},
		{"groq", "https://api.groq.com/openai/v1"},
		{"xai", "https://api.x.ai/v1"},
		{"openrouter", "https://openrouter.ai/api/v1"},
		{"together", "https://api.together.xyz/v1"},
		{"openai_compatible", ""}, // requires an explicit BaseURL by definition
		{"bedrock", ""},           // SDK-shaped auth — not frontable
		{"vertex", ""},
		{"azure_openai", ""},
		{"opencode", ""}, // zen: auth-store path, not relay-routed
		{"", ""},
		{"unknown-kind", ""},
	} {
		assert.Equal(t, tc.want, ProviderDefaultBaseURL(tc.kind), "kind %q", tc.kind)
	}
}

func TestProviderRelayStageable(t *testing.T) {
	stageable := []string{
		"openai", "anthropic", "google", "cohere", "mistral", "perplexity",
		"groq", "xai", "openrouter", "together", "openai_compatible",
	}
	for _, k := range stageable {
		assert.True(t, ProviderRelayStageable(k), "kind %q must be stageable", k)
	}
	for _, k := range []string{"bedrock", "vertex", "azure_openai", "opencode", "", "unknown"} {
		assert.False(t, ProviderRelayStageable(k), "kind %q must NOT be stageable", k)
	}
}

// TestProviderRelayStageableCoversValidKinds pins that the stageability set
// stays aligned with the credential-kind enum: every ValidKinds entry is
// either stageable or explicitly excluded (a NEW kind added to ValidKinds
// without a staging decision must fail here, not silently skip staging).
func TestProviderRelayStageableCoversValidKinds(t *testing.T) {
	explicitlyExcluded := map[string]bool{
		"bedrock": true, "vertex": true, "azure_openai": true, "opencode": true,
	}
	for _, k := range ValidKinds {
		if !explicitlyExcluded[k] {
			assert.True(t, ProviderRelayStageable(k), "kind %q is in ValidKinds but has no staging decision", k)
		}
	}
}
