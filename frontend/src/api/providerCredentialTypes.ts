// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Shared type definitions for provider credentials across the admin, user, and
// org API clients. The three clients keep their own method implementations
// (different base paths and auth contexts) but import these types so a field
// added to the credential shape only needs updating in one place.

// ProviderCredential is the base API response shape for any provider credential.
// Never includes apiKey. The owner scope (orgId for org credentials) and a
// possible bindWarning are carried by the per-client response types below.
//
// Epic 55 identity model:
//   - kind: SDK-class enum (openai, anthropic, openai_compatible, ...).
//     Determines which opencode adapter loads.
//   - slug: per-owner unique identity AND the literal key in
//     agent-config.json's provider map. opencode persists this as
//     `providerID` on session records. Slug-safe regex enforced by the
//     server: ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$.
//   - name: free-form UX display label.
//
// modelContextLimits and modelOutputLimits MUST be set together for a given
// model to take effect: opencode's published JSON Schema requires both
// `limit.context` and `limit.output` whenever the `limit` block is present.
// If only one is set, the formatter omits the entire `limit` block and
// opencode falls back to built-in defaults.
export interface ProviderCredential {
  id: string;
  name: string;
  kind: string;
  slug: string;
  baseURL?: string;
  modelAllowlist?: string[];
  modelContextLimits?: Record<string, number>;
  modelOutputLimits?: Record<string, number>;
  createdAt: string;
  updatedAt: string;
}

// CreateCredentialRequest is the shared create body for admin and user
// credentials (identical request shape). The org client uses the same fields
// plus its own validation; it reuses this type directly.
export interface CreateCredentialRequest {
  name: string;
  kind: string;
  slug: string;
  apiKey: string;
  baseURL?: string;
  modelAllowlist?: string[];
  modelContextLimits?: Record<string, number>;
  modelOutputLimits?: Record<string, number>;
}

// UpdateCredentialRequest is the shared partial-update body. All fields are
// optional — only the fields present in the request are changed server-side.
export interface UpdateCredentialRequest {
  name?: string;
  kind?: string;
  slug?: string;
  apiKey?: string;
  baseURL?: string;
  modelAllowlist?: string[];
  modelContextLimits?: Record<string, number>;
  modelOutputLimits?: Record<string, number>;
}

// CreateCredentialResponse is the create response carrying an optional
// bindWarning (set when auto-bind to existing workspaces failed — non-fatal,
// the credential is still stored). Admin/user credentials reuse
// ProviderCredential directly; org credentials extend it with orgId.
export interface CreateCredentialResponse extends ProviderCredential {
  bindWarning?: string;
}

// ProbeModelEntry is one entry from GET /:id/models.
export interface ProbeModelEntry {
  id: string;
  contextLimit: number; // 0 = unknown / not yet configured
  outputLimit: number; // 0 = unknown / not yet configured
}

// ProbeModelsResponse is the response from GET /:id/models for any owner type.
export interface ProbeModelsResponse {
  models: ProbeModelEntry[];
  baseURL?: string;
  warning?: string;
}

// SDK_KINDS is the canonical list of allowed `kind` values. Keep in sync
// with pkg/secrets/credential_identity.go ValidKinds and the DB CHECK in
// api/migrations/000001_initial_schema.up.sql. The order here matches the
// rendering order in the UI dropdown — most common kinds first.
export const SDK_KINDS = [
  "openai",
  "anthropic",
  "google",
  "openai_compatible",
  "bedrock",
  "azure_openai",
  "vertex",
  "cohere",
  "mistral",
  "perplexity",
  "groq",
  "xai",
  "openrouter",
  "together",
  "opencode",
] as const;
export type SdkKind = (typeof SDK_KINDS)[number];

// SLUG_REGEX is the canonical slug regex, mirrored from
// pkg/secrets/credential_identity.go SlugRegex and the DB CHECK. Used by
// client-side validation to mirror the server-side check; the server also
// validates so this is just for early UX feedback.
export const SLUG_REGEX = /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$/;

// slugFromName converts a free-form display name into a slug-safe value.
// Mirrors the SQL backfill expression in the schema migration so the
// auto-suggestion the UI offers matches what the DB would compute if a
// user left the slug field empty.
export function slugFromName(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64)
    .replace(/-+$/g, ""); // trim trailing hyphens after truncation
}
