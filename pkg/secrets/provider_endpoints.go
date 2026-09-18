// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// provider_endpoints.go — US-72.3 (design 0058 §4.4): the kind→default-
// upstream table behind the controller staging's token minting. A scoped
// token names exactly one upstream (SSRF-proof by construction), so the
// controller must resolve each provider's effective endpoint at seal/mint
// time even when the credential carries no explicit BaseURL (first-party
// SDK defaults). Single table next to LLMProviderData so US-72.4's builder
// swap reuses it verbatim — the token's baseURL and the pod-facing routing
// decision can never drift apart.
//
// Stageability: the BYO router is a Bearer-injecting reverse proxy. Kinds
// whose SDK performs its own auth handshake (bedrock SigV4, vertex GCP
// tokens, azure_openai resource-key exchange) cannot be fronted by it by
// construction, and `opencode` (zen) rides the auth-store merge path
// (shouldSkipRelay's concern, US-72.4). Those kinds are not staged.

// providerDefaultBaseURLs maps first-party SDK classes to their default
// upstream endpoints (the values the corresponding ai-sdk/opencode adapters
// embed when the credential's BaseURL is empty). US-72.4/US-72.5 must
// cross-check this table against opencode's models.dev catalog during the
// flip-gate review.
var providerDefaultBaseURLs = map[string]string{
	"openai":            "https://api.openai.com/v1",
	"anthropic":         "https://api.anthropic.com/v1",
	"google":            "https://generativelanguage.googleapis.com/v1",
	"cohere":            "https://api.cohere.com/v1",
	"mistral":           "https://api.mistral.ai/v1",
	"perplexity":        "https://api.perplexity.ai",
	"groq":              "https://api.groq.com/openai/v1",
	"xai":               "https://api.x.ai/v1",
	"openrouter":        "https://openrouter.ai/api/v1",
	"together":          "https://api.together.xyz/v1",
	"openai_compatible": "", // custom endpoints always carry an explicit BaseURL
}

// ProviderDefaultBaseURL returns the default upstream endpoint for a kind,
// or "" when the kind has none (openai_compatible without an explicit
// BaseURL, and every non-stageable kind).
func ProviderDefaultBaseURL(kind string) string {
	return providerDefaultBaseURLs[kind]
}

// ProviderRelayStageable reports whether a credential of this kind can be
// fronted by the BYO resolve router (plain HTTP + Bearer upstream auth).
func ProviderRelayStageable(kind string) bool {
	_, ok := providerDefaultBaseURLs[kind]
	return ok
}
