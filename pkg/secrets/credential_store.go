// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"net/http"

	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
)

// ErrAutoBindingProtected is returned when a caller attempts to Unbind a
// credential that is bound via an auto-apply rule (source_type='auto').
// Auto-bindings are managed by SeedWorkspaceCredentials and can only be
// removed by deleting the underlying credential or the workspace.
//
// This sentinel lives in pkg/ (not api/internal/errors) because it is shared
// between the API server and the agentd daemon. It uses StatusError so the
// generic error handler maps it to HTTP 409 automatically.
var ErrAutoBindingProtected = &pkgerrors.StatusError{
	Status:  http.StatusConflict,
	Code:    "auto_binding_protected",
	Message: "auto-binding cannot be removed via unbind; delete the credential or workspace to remove it",
}

// CredentialBinding is a joined row from workspace_credential_bindings + provider_credentials.
//
// Identity model (Epic 55):
//   - Kind: SDK-class discriminator. Enum constrained by the DB CHECK.
//     Selects the adapter that opencode loads (openai, anthropic, bedrock,
//     openai_compatible, ...). Multiple credentials of the same Kind can
//     coexist (e.g. two OpenAI-compatible LiteLLM endpoints) — Kind does
//     NOT have to be unique per owner.
//   - Slug: stable per-owner identity. UNIQUE(owner_type, owner_id, slug)
//     in the DB. This is also the literal key used in agent-config.json's
//     provider map, so opencode sessions persist this value as providerID.
//     A user with two `openai_compatible` credentials picks distinct slugs
//     (e.g. "litellm-prod-us-west" and "litellm-prod-eu-central") to
//     disambiguate them on the wire.
type CredentialBinding struct {
	ID                 string
	OwnerType          string
	OwnerID            string
	Kind               string // SDK-class enum (openai, anthropic, openai_compatible, ...)
	Slug               string // slug-safe per-owner identity; the agent-config.json provider-map key
	Ciphertext         []byte
	KeyVersion         int
	ModelAllowlist     []string
	ModelContextLimits map[string]int // model_id → context window size in tokens
	ModelOutputLimits  map[string]int // model_id → max output tokens
	SourceType         string         // "explicit" or "auto"
	WithinPriority     int
}

// CredentialBindingInfo is a minimal binding row used for the ListBindings API.
// It carries the workspace ID and the source type so the UI can distinguish
// auto-seeded bindings (which cannot be manually unbound) from explicit ones.
type CredentialBindingInfo struct {
	WorkspaceID string `json:"workspaceId"`
	SourceType  string `json:"sourceType"` // "explicit" or "auto"
}

// CredentialStore abstracts database operations for provider credentials.
type CredentialStore interface {
	// GetWorkspaceCredentials returns all credential bindings for a workspace,
	// ordered by: (source_type='explicit') DESC, within_priority DESC, created_at ASC.
	GetWorkspaceCredentials(ctx context.Context, workspaceID string) ([]CredentialBinding, error)

	// UpsertFreeTierCredential atomically upserts the platform free-tier
	// credential row and its auto-apply rule in a single transaction.
	UpsertFreeTierCredential(ctx context.Context, ciphertext []byte) error

	// SeedWorkspaceCredentials inserts credential bindings for a new workspace:
	// admin auto-apply rules (all, user target types), user-owned credentials,
	// and org auto-apply rules + org credentials when orgID is non-nil.
	SeedWorkspaceCredentials(ctx context.Context, workspaceID, userID string, orgID *string) error

	// BindCredentialToAllUserWorkspaces binds a credential to every workspace
	// owned by userID. Called on credential create to maintain the invariant
	// that all credentials are bound to all of a user's workspaces.
	BindCredentialToAllUserWorkspaces(ctx context.Context, credentialID, userID string) error

	// HasUserProviderCredential returns true if the user owns a credential
	// with the given slug. The lookup is per-slug because slug is the
	// per-owner unique identity (Epic 55); kind alone is not unique per
	// owner.
	HasUserProviderCredential(ctx context.Context, userID, slug string) (bool, error)

	// GetWorkspaceMCPServers returns all bound+enabled MCP servers for a
	// workspace, carrying ciphertext for downstream decryption. Epic 53.
	GetWorkspaceMCPServers(ctx context.Context, workspaceID string) ([]MCPServerBindingRow, error)
}

// AdminKeyDeriver derives a server-side encryption key for admin credentials.
// The label parameter scopes the derived key (e.g. "provider-credentials").
// Returns nil when LLMSAFESPACES_MASTER_SECRET is not set.
//
// Deprecated: US-50.2 unifies admin/org credential crypto under RootKeyProvider.
// New code must not use this type; it is retained for one release so callers
// can fall back to the legacy path if a production issue surfaces. Removed in a
// follow-up release. See design/stories/epic-50-master-kek-hardening/README.md.
type AdminKeyDeriver func(label string) []byte
