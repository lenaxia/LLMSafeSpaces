// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

// format_test.go — TDD-style tests pinned to the exact JSON shape
// opencode accepts (contract enforced against the pinned schema in
// cmd/workspace-agentd/testdata/opencode-config.schema.json).
//
// **CRITICAL**: the schema produced by FormatOpenCodeConfig is
// EVIDENCE-DRIVEN, not derived from a public spec. The shape was
// established by probing a live opencode pod (worklog 0128); pre-fix
// formatters guessed at `providers` (plural) and an `aisdk` nesting
// that opencode rejected with `ConfigInvalidError`. The valid shape is:
//
//   {
//     "$schema": "https://opencode.ai/config.json",
//     "provider": {                              <-- SINGULAR "provider"
//       "<id>": {
//         "options": {                           <-- direct, no aisdk wrapper
//           "apiKey": "...",
//           "baseURL": "..."                     <-- in options, NOT endpoint.url
//         },
//         "models": { "<id>": { "name": "..." } }
//       }
//     },
//     "model": "<id>/<modelID>"
//   }
//
// Every test in this file pins one assertion against this shape.
// If a future schema change ships and we miss it, these tests will
// fail — re-run the worklog 0128 probe to learn the new shape and
// update accordingly.

import (
	"encoding/json"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatOpenCodeConfig_SingleProvider_Minimal(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant-123"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	require.Equal(t, "https://opencode.ai/config.json", parsed["$schema"])

	// Top-level key is `provider` (singular).
	provs := parsed["provider"].(map[string]interface{})
	anth := provs["anthropic"].(map[string]interface{})

	// Options carries apiKey directly (no aisdk wrapper).
	opts := anth["options"].(map[string]interface{})
	require.Equal(t, "sk-ant-123", opts["apiKey"])
	_, hasBaseURL := opts["baseURL"]
	require.False(t, hasBaseURL, "no baseURL when BaseURL is empty")

	// No endpoint key — opencode infers from provider id.
	_, hasEndpoint := anth["endpoint"]
	require.False(t, hasEndpoint, "endpoint key must NOT be set; opencode rejects it")

	// No models when Models is empty.
	require.Nil(t, anth["models"])
	// No model when Default is empty.
	require.Nil(t, parsed["model"])
}

func TestFormatOpenCodeConfig_SingleProvider_AllFields(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "anthropic", Slug: "anthropic",
			APIKey:  "sk-ant-123",
			BaseURL: "https://custom.anthropic.com/v1",
			Default: "anthropic/claude-sonnet-4-5",
			Models: []secrets.LLMModelConfig{
				{ID: "claude-sonnet-4-5", Label: "Claude Sonnet 4.5"},
				{ID: "claude-haiku-4-5"},
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	require.Equal(t, "anthropic/claude-sonnet-4-5", parsed["model"])

	provs := parsed["provider"].(map[string]interface{})
	anth := provs["anthropic"].(map[string]interface{})

	// baseURL goes into options, NOT into a separate endpoint object.
	opts := anth["options"].(map[string]interface{})
	require.Equal(t, "sk-ant-123", opts["apiKey"])
	require.Equal(t, "https://custom.anthropic.com/v1", opts["baseURL"])

	// npm must be set for custom-baseURL providers.
	require.Equal(t, "@ai-sdk/openai-compatible", anth["npm"],
		"custom-baseURL provider must have npm=@ai-sdk/openai-compatible")

	// No endpoint key.
	_, hasEndpoint := anth["endpoint"]
	require.False(t, hasEndpoint)

	// Models.
	models := anth["models"].(map[string]interface{})
	sonnet := models["claude-sonnet-4-5"].(map[string]interface{})
	require.Equal(t, "Claude Sonnet 4.5", sonnet["name"])
	haiku := models["claude-haiku-4-5"].(map[string]interface{})
	_, hasName := haiku["name"]
	require.False(t, hasName, "model without label should omit name")
}

func TestFormatOpenCodeConfig_MultipleProviders_FirstDefaultWins(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant", Default: "anthropic/claude-sonnet-4-5"},
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai", Default: "openai/gpt-4o"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	require.Equal(t, "anthropic/claude-sonnet-4-5", parsed["model"])
	// Both providers present.
	provs := parsed["provider"].(map[string]interface{})
	require.Contains(t, provs, "anthropic")
	require.Contains(t, provs, "openai")
}

func TestFormatOpenCodeConfig_MultipleProviders_SecondHasDefault(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant"},
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai", Default: "openai/gpt-4o"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	require.Equal(t, "openai/gpt-4o", parsed["model"])
}

func TestFormatOpenCodeConfig_MultipleProviders_NoneHasDefault(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant"},
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	_, hasModel := parsed["model"]
	require.False(t, hasModel)
}

// TestFormatOpenCodeConfig_BaseURL_AppliedFromOptions is the regression
// guard for the second part of Bug 3: pre-fix the baseURL went into a
// separate `endpoint.url` object that opencode discarded, so requests
// went to api.openai.com instead of the operator's endpoint. This test
// asserts baseURL lives at provider.<id>.options.baseURL — the only
// place opencode reads it from.
func TestFormatOpenCodeConfig_BaseURL_LivesInOptions(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai", BaseURL: "https://litellm.example/v1"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	provs := parsed["provider"].(map[string]interface{})
	oai := provs["openai"].(map[string]interface{})

	// MUST be in options.
	opts := oai["options"].(map[string]interface{})
	require.Equal(t, "https://litellm.example/v1", opts["baseURL"])

	// MUST NOT be in a separate endpoint key.
	_, hasEndpoint := oai["endpoint"]
	require.False(t, hasEndpoint, "Bug 3: endpoint key must NEVER appear; baseURL belongs in options")
}

// TestFormatOpenCodeConfig_TopLevelKey_IsProviderSingular is the
// regression guard for the first part of Bug 3: pre-fix the top-level
// key was `providers` (plural) which opencode rejected with
// ConfigInvalidError, blocking opencode boot.
func TestFormatOpenCodeConfig_TopLevelKey_IsProviderSingular(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	// MUST have `provider` (singular).
	_, hasProvider := parsed["provider"]
	require.True(t, hasProvider, "Bug 3: top-level key MUST be `provider` (singular)")

	// MUST NOT have `providers` (plural).
	_, hasProviders := parsed["providers"]
	require.False(t, hasProviders, "Bug 3: opencode rejects the plural `providers` key")
}

// TestFormatOpenCodeConfig_Options_NoAisdkWrapper is the regression
// guard for the third part of Bug 3: pre-fix the apiKey lived at
// options.aisdk.provider.apiKey, which opencode rejected. The valid
// shape is options.apiKey directly.
func TestFormatOpenCodeConfig_Options_NoAisdkWrapper(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	provs := parsed["provider"].(map[string]interface{})
	oai := provs["openai"].(map[string]interface{})
	opts := oai["options"].(map[string]interface{})

	// apiKey directly under options.
	require.Equal(t, "sk-oai", opts["apiKey"])

	// MUST NOT have aisdk wrapper.
	_, hasAisdk := opts["aisdk"]
	require.False(t, hasAisdk, "Bug 3: aisdk wrapper must NEVER appear in options")
}

func TestFormatOpenCodeConfig_ModelsWithAndWithoutLabels(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "openai", Slug: "openai",
			APIKey: "sk-oai",
			Models: []secrets.LLMModelConfig{
				{ID: "gpt-4o", Label: "GPT-4o"},
				{ID: "gpt-4o-mini"},
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	provs := parsed["provider"].(map[string]interface{})
	oai := provs["openai"].(map[string]interface{})
	models := oai["models"].(map[string]interface{})

	gpt4o := models["gpt-4o"].(map[string]interface{})
	require.Equal(t, "GPT-4o", gpt4o["name"])

	mini := models["gpt-4o-mini"].(map[string]interface{})
	_, hasName := mini["name"]
	require.False(t, hasName)
}

func TestFormatOpenCodeConfig_Deterministic(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "openai", Slug: "openai", APIKey: "sk-oai"},
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant"},
		{Kind: "google", Slug: "google", APIKey: "sk-goog"},
	}

	out1, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	out2, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	require.Equal(t, string(out1), string(out2), "output must be deterministic")
}

func TestFormatOpenCodeConfig_EmptyInput_Error(t *testing.T) {
	_, err := FormatOpenCodeConfig(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no providers")

	_, err = FormatOpenCodeConfig([]secrets.LLMProviderData{})
	require.Error(t, err)
}

func TestFormatOpenCodeConfig_OutputIsValidJSON(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{Kind: "anthropic", Slug: "anthropic", APIKey: "sk-ant-with-special-chars-!@#$%"},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)
	require.True(t, json.Valid(out), "output must be valid JSON")
}

// TestFormatOpenCodeConfig_ExactSnapshot pins the exact byte output
// for a representative input. This is the most aggressive regression
// guard: any change to the formatter that affects the wire shape will
// fail this test.
//
// The snapshot below was updated in worklog 0183 to include
// "npm": "@ai-sdk/openai-compatible" for providers with a custom BaseURL.
// This shape was validated against a live opencode pod: the
// opencode-relay provider (which uses the same npm field) shows as
// connected in `/provider`. Without the npm field, opencode treats
// the built-in "openai" provider ID as first-party and calls
// /v1/responses instead of /v1/chat/completions, causing 404 on
// LiteLLM proxies that don't expose the Responses API.
func TestFormatOpenCodeConfig_ExactSnapshot(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "openai", Slug: "openai",
			APIKey:  "sk-test-key",
			BaseURL: "https://litellm.example/v1",
			Default: "openai/deepseek-v3-chat",
			Models: []secrets.LLMModelConfig{
				{ID: "deepseek-v3-chat", Label: "DeepSeek V3 Chat"},
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	expected := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "openai": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "apiKey": "sk-test-key",
        "baseURL": "https://litellm.example/v1"
      },
      "models": {
        "deepseek-v3-chat": {
          "name": "DeepSeek V3 Chat"
        }
      }
    }
  },
  "model": "openai/deepseek-v3-chat"
}`

	require.Equal(t, expected, string(out),
		"snapshot mismatch — opencode rejects shapes other than this one. "+
			"Re-validate against a live opencode pod before updating the snapshot.")
}

// TestFormatOpenCodeConfig_NoNPMWhenNoBaseURL ensures providers without a
// custom BaseURL (native built-in providers like plain "openai" with direct
// API keys) do NOT get the npm field injected — they should use opencode's
// native SDK, not the generic openai-compatible shim.
func TestFormatOpenCodeConfig_NoNPMWhenNoBaseURL(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "openai", Slug: "openai",
			APIKey:  "sk-realkey",
			BaseURL: "", // no custom endpoint
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &cfg))
	var provs map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["provider"], &provs))
	var op map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(provs["openai"], &op))
	_, hasNPM := op["npm"]
	assert.False(t, hasNPM, "npm must not be set for providers without a custom BaseURL")
}

// TestFormatOpenCodeConfig_LimitEmission_RequiresBothContextAndOutput is the
// core schema-compliance test.
//
// opencode's published JSON Schema (https://opencode.ai/config.json) declares
// the model `limit` object with `"required": ["context", "output"]` and
// `"additionalProperties": false`. Empirically verified on opencode:
// emitting a `limit` block with only `context` set returns
// `SchemaError: Missing key at [...]["limit"]["output"]` from Config.state(),
// causing every endpoint that touches config (including POST /session) to
// return 500. The reverse (only `output`) fails the same way for `context`.
//
// Therefore: emit `limit` ONLY when BOTH ContextLimit > 0 AND OutputLimit > 0.
// If either is unknown, omit the entire `limit` block — opencode then falls
// back to its built-in defaults, which is correct and harmless.
func TestFormatOpenCodeConfig_LimitEmission_RequiresBothContextAndOutput(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "thekao cloud", Slug: "thekao cloud",
			APIKey:  "sk-test",
			BaseURL: "https://ai.thekao.cloud/v1",
			Models: []secrets.LLMModelConfig{
				{ID: "glm-5.1", ContextLimit: 200000, OutputLimit: 8192},
				{ID: "glm-5.2", Label: "GLM 5.2", ContextLimit: 200000, OutputLimit: 16384},
				{ID: "classifier"},                         // neither — omit limit
				{ID: "context-only", ContextLimit: 100000}, // context only — omit limit
				{ID: "output-only", OutputLimit: 4096},     // output only — omit limit
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	provs := parsed["provider"].(map[string]interface{})
	p := provs["thekao cloud"].(map[string]interface{})
	models := p["models"].(map[string]interface{})

	// Model with BOTH ContextLimit and OutputLimit must have limit with both keys.
	glm51 := models["glm-5.1"].(map[string]interface{})
	limit51, hasLimit := glm51["limit"]
	require.True(t, hasLimit, "model with both ContextLimit and OutputLimit must have a 'limit' field")
	limitObj := limit51.(map[string]interface{})
	assert.Equal(t, float64(200000), limitObj["context"], "limit.context must match ContextLimit")
	assert.Equal(t, float64(8192), limitObj["output"], "limit.output must match OutputLimit")

	// Model with both + label.
	glm52 := models["glm-5.2"].(map[string]interface{})
	assert.Equal(t, "GLM 5.2", glm52["name"])
	limit52 := glm52["limit"].(map[string]interface{})
	assert.Equal(t, float64(200000), limit52["context"])
	assert.Equal(t, float64(16384), limit52["output"])

	// Model with NEITHER limit must NOT have a limit field.
	classifier := models["classifier"].(map[string]interface{})
	_, hasClassifierLimit := classifier["limit"]
	assert.False(t, hasClassifierLimit, "model with neither limit must NOT have a 'limit' field")

	// Model with ContextLimit but no OutputLimit must NOT have a limit field —
	// opencode schema rejects partial limit blocks (SchemaError: Missing key).
	contextOnly := models["context-only"].(map[string]interface{})
	_, hasContextOnlyLimit := contextOnly["limit"]
	assert.False(t, hasContextOnlyLimit,
		"model with ContextLimit but no OutputLimit must omit 'limit' entirely — "+
			"opencode rejects partial limit blocks with SchemaError")

	// Model with OutputLimit but no ContextLimit must NOT have a limit field.
	outputOnly := models["output-only"].(map[string]interface{})
	_, hasOutputOnlyLimit := outputOnly["limit"]
	assert.False(t, hasOutputOnlyLimit,
		"model with OutputLimit but no ContextLimit must omit 'limit' entirely — "+
			"opencode rejects partial limit blocks with SchemaError")
}

// TestFormatOpenCodeConfig_LimitFields_ZeroValues_OmitLimit verifies that
// when both ContextLimit and OutputLimit are 0 (the zero values, signaling
// "unknown"), no limit field is emitted.
func TestFormatOpenCodeConfig_LimitFields_ZeroValues_OmitLimit(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "openai", Slug: "openai",
			APIKey:  "sk-test",
			BaseURL: "https://example.com/v1",
			Models: []secrets.LLMModelConfig{
				{ID: "gpt-5", ContextLimit: 0, OutputLimit: 0},
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(out, &parsed))

	provs := parsed["provider"].(map[string]interface{})
	p := provs["openai"].(map[string]interface{})
	models := p["models"].(map[string]interface{})
	gpt5 := models["gpt-5"].(map[string]interface{})

	_, hasLimit := gpt5["limit"]
	assert.False(t, hasLimit, "both limits zero must produce no 'limit' field")
}

// TestFormatOpenCodeConfig_ExactSnapshot_WithBothLimits is the canonical
// wire-format snapshot of agent-config.json content. opencode reads this
// JSON at startup and validates against its published schema. The shape
// MUST match exactly.
func TestFormatOpenCodeConfig_ExactSnapshot_WithBothLimits(t *testing.T) {
	providers := []secrets.LLMProviderData{
		{
			Kind: "openai", Slug: "openai",
			APIKey:  "sk-test-key",
			BaseURL: "https://litellm.example/v1",
			Default: "openai/deepseek-v3-chat",
			Models: []secrets.LLMModelConfig{
				{ID: "deepseek-v3-chat", Label: "DeepSeek V3 Chat", ContextLimit: 128000, OutputLimit: 8192},
			},
		},
	}

	out, err := FormatOpenCodeConfig(providers)
	require.NoError(t, err)

	expected := `{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "openai": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "apiKey": "sk-test-key",
        "baseURL": "https://litellm.example/v1"
      },
      "models": {
        "deepseek-v3-chat": {
          "name": "DeepSeek V3 Chat",
          "limit": {
            "context": 128000,
            "output": 8192
          }
        }
      }
    }
  },
  "model": "openai/deepseek-v3-chat"
}`

	require.Equal(t, expected, string(out),
		"snapshot mismatch — update the snapshot after re-validating against a live opencode pod")
}
