// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SecretType defines the type of secret.
type SecretType string

const (
	// APIKeySunsetDate is the fixed date on which new api-key secrets
	// become uncreatable (US-44.9). Six months after the Epic 44 ship
	// date. Existing api-key secrets remain functional after this date;
	// only creation is blocked by the CreateSecret gate.
	APIKeySunsetDate = "2026-12-19"

	// SecretTypeAPIKey is for generic API-key secrets (legacy).
	// New code should use SecretTypeLLMProvider for structured provider
	// credentials. Kept for backward compatibility with existing secrets.
	SecretTypeAPIKey SecretType = "api-key"
	// SecretTypeLLMProvider is for structured LLM provider credentials
	// (Anthropic, OpenAI, etc.). Each secret holds one provider with
	// its API key, optional base URL, model visibility allowlist, and
	// default model selection. Multiple llm-provider secrets bound to
	// the same workspace are merged by the agent's FormatProviderConfig.
	SecretTypeLLMProvider   SecretType = "llm-provider"
	SecretTypeSSHKey        SecretType = "ssh-key"
	SecretTypeGitCredential SecretType = "git-credential"
	SecretTypeSecretFile    SecretType = "secret-file"
	SecretTypeEnvSecret     SecretType = "env-secret"
	// SecretTypeMcpServer (Epic 53) carries an external MCP server config
	// through the injection pipeline. Metadata holds transport/url/command/args;
	// Plaintext is the JSON-encoded env+headers map (decrypted at injection).
	SecretTypeMcpServer SecretType = "mcp-server"
)

// isAPIKeySunset reports whether the fixed api-key sunset date has
// passed. It is a function variable so tests can force the post-sunset
// branch without waiting for the real date; production callers always
// use the default value, which compares now against 2026-12-19 (the
// value of APIKeySunsetDate, expressed as a time.Time literal).
var isAPIKeySunset = func() bool {
	return time.Now().After(time.Date(2026, 12, 19, 0, 0, 0, 0, time.UTC))
}

// ValidSecretTypes is the set of allowed secret types.
var ValidSecretTypes = map[SecretType]bool{
	SecretTypeAPIKey:        true,
	SecretTypeLLMProvider:   true,
	SecretTypeSSHKey:        true,
	SecretTypeGitCredential: true,
	SecretTypeSecretFile:    true,
	SecretTypeEnvSecret:     true,
	SecretTypeMcpServer:     true,
}

// ValidSecretTypesList returns the canonical list of valid secret types,
// in stable order. Used to format the error message returned when a
// caller submits an invalid type, so the response is self-documenting.
func ValidSecretTypesList() []SecretType {
	return []SecretType{
		SecretTypeAPIKey,
		SecretTypeLLMProvider,
		SecretTypeSSHKey,
		SecretTypeGitCredential,
		SecretTypeSecretFile,
		SecretTypeEnvSecret,
		SecretTypeMcpServer,
	}
}

// MetadataRequirementsBySecretType is a self-documenting map of which
// metadata keys each secret type requires. Surfaced in error responses
// (Bug 7 in worklog 0085) so callers don't have to reverse-engineer the
// schema from 400s.
var MetadataRequirementsBySecretType = map[SecretType][]string{
	SecretTypeAPIKey:        {}, // optional: provider, model
	SecretTypeLLMProvider:   {}, // all config in value JSON, metadata optional
	SecretTypeSSHKey:        {"key_type"},
	SecretTypeGitCredential: {}, // optional: host
	SecretTypeSecretFile:    {"mount_path"},
	SecretTypeEnvSecret:     {"var_name"},
}

// formatSecretTypes joins SecretType values with commas for use in
// error messages (e.g. "api-key, ssh-key, git-credential, secret-file,
// env-secret"). Stable order.
func formatSecretTypes(types []SecretType) string {
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, string(t))
	}
	return strings.Join(parts, ", ")
}

// UserSecret represents an encrypted secret record.
type UserSecret struct {
	ID            string          `json:"id"`
	UserID        string          `json:"userId"`
	Name          string          `json:"name"`
	Type          SecretType      `json:"type"`
	Ciphertext    []byte          `json:"-"` // never exposed via API
	KeyVersion    int             `json:"keyVersion"`
	Metadata      json.RawMessage `json:"metadata"`
	GlobalDefault bool            `json:"globalDefault"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// SecretBinding represents a secret-to-workspace binding.
type SecretBinding struct {
	SecretID    string    `json:"secretId"`
	WorkspaceID string    `json:"workspaceId"`
	CreatedAt   time.Time `json:"createdAt"`
}

// AuditEntry represents a secret audit log entry.
type AuditEntry struct {
	ID          int64           `json:"id"`
	UserID      string          `json:"userId"`
	Action      string          `json:"action"`
	SecretID    *string         `json:"secretId,omitempty"`
	WorkspaceID *string         `json:"workspaceId,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Timestamp   time.Time       `json:"timestamp"`
}

// CreateSecretRequest is the API request for creating a secret.
type CreateSecretRequest struct {
	Name          string          `json:"name" binding:"required,min=1,max=255"`
	Type          SecretType      `json:"type" binding:"required"`
	Value         string          `json:"value" binding:"required"` // plaintext, encrypted before storage
	Metadata      json.RawMessage `json:"metadata"`
	GlobalDefault bool            `json:"globalDefault,omitempty"`
}

// UpdateSecretRequest is the API request for updating a secret value.
type UpdateSecretRequest struct {
	Value    string          `json:"value" binding:"required"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// GlobalDefault, when non-nil, updates whether this secret is automatically
	// bound to newly-created workspaces. Nil means "leave unchanged".
	GlobalDefault *bool `json:"globalDefault,omitempty"`
}

// SecretResponse is the API response for a secret (never includes value).
type SecretResponse struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Type          SecretType      `json:"type"`
	Metadata      json.RawMessage `json:"metadata"`
	GlobalDefault bool            `json:"globalDefault"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

// SetBindingsRequest is the API request for setting workspace bindings.
type SetBindingsRequest struct {
	SecretIDs []string `json:"secretIds" binding:"required"`
}

// BindingsResponse is the API response for workspace bindings.
type BindingsResponse struct {
	Bindings []BoundSecret `json:"bindings"`
}

// BoundSecret is a secret reference in a binding response.
type BoundSecret struct {
	SecretID string     `json:"secretId"`
	Name     string     `json:"name"`
	Type     SecretType `json:"type"`
}

// AuditQuery defines filters for querying the audit log.
type AuditQuery struct {
	Action      string
	SecretID    string
	WorkspaceID string
	Since       *time.Time
	Until       *time.Time
	Limit       int
	Offset      int
}

// LLMModelConfig declares an allowlisted model and its display/limit metadata.
// At minimum, ID is required. Label is shown in pickers when set.
//
// ContextLimit and OutputLimit drive the opencode `limit` block in
// agent-config.json. opencode's published JSON Schema
// (https://opencode.ai/config.json) declares the model `limit` object with
// `"required": ["context", "output"]` and `"additionalProperties": false`.
// Therefore FormatOpenCodeConfig emits a `limit` block ONLY when BOTH are
// non-zero — emitting a partial block (only context, or only output) makes
// opencode reject the entire config with
// SchemaError: Missing key, which causes every endpoint that calls
// Config.state() (including POST /session) to return 500.
//
// ContextLimit is the total context window size in tokens. When set together
// with OutputLimit it is written into agent-config.json as limit.context,
// which makes opencode's /config/providers return ctx=N, which feeds
// ModelContextLimit() in agentd → context.total_tokens in /v1/statusz →
// CRD status.contextTotal → the frontend's "used / total" context bar.
//
// OutputLimit is the maximum response tokens for the model. opencode uses
// this for compaction sizing. Like ContextLimit it cannot be auto-discovered
// from a provider's /v1/models endpoint and must be configured explicitly
// by the workspace/credential owner.
type LLMModelConfig struct {
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	ContextLimit int    `json:"contextLimit,omitempty"`
	OutputLimit  int    `json:"outputLimit,omitempty"`
}

// LLMProviderData holds structured credentials for one LLM provider.
// The Plaintext value of an "llm-provider" secret is the JSON encoding of this struct.
//
// Epic 55 identity model:
//   - Kind is the SDK-class enum (openai, anthropic, openai_compatible, ...).
//     Required. Determines which adapter opencode loads.
//   - Slug is the per-owner unique identity AND the literal key used in
//     agent-config.json's provider map. opencode persists this as
//     `providerID` on sessions.
//
// APIKey is required.
// BaseURL is optional; when empty the provider's default endpoint is used.
// Models is an optional allowlist. When empty or nil all models from the
// provider are visible (no filtering). When non-empty only the listed
// models are shown.
// Default is the model ID to use when no per-session model is specified.
// SmallModel is the model ID used for lightweight/cheap operations
// (e.g. summarization).
type LLMProviderData struct {
	Kind       string           `json:"kind"`
	Slug       string           `json:"slug"`
	APIKey     string           `json:"apiKey"`
	BaseURL    string           `json:"baseURL,omitempty"`
	Models     []LLMModelConfig `json:"models,omitempty"`
	Default    string           `json:"default,omitempty"`
	SmallModel string           `json:"smallModel,omitempty"`
}

// Validate checks that required fields are set in LLMProviderData.
func (d LLMProviderData) Validate() error {
	if d.Kind == "" {
		return fmt.Errorf("%w: kind is required", ErrInvalidLLMProvider)
	}
	if d.Slug == "" {
		return fmt.Errorf("%w: slug is required", ErrInvalidLLMProvider)
	}
	if d.APIKey == "" {
		return fmt.Errorf("%w: apiKey is required", ErrInvalidLLMProvider)
	}
	return nil
}
