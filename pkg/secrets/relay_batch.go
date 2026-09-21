// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// relay_batch.go — US-72.4 (design 0058 §4.4/§4.5): relay-only token
// emission in the one builder. When a RelayTokenSource is installed
// (the deployment's relayOnlyKeyDelivery flag reaching the API server),
// the batch's llm-provider entries are rebuilt from the
// controller-staged handoff Secret (US-72.3 worklog D2): apiKey becomes
// the scoped relay token, baseURL the router (RouterURL + RouterPath).
//
// Invariants:
//   - A nil source (flag off) is byte-identical legacy behavior — the
//     relay paths are unreachable by construction.
//   - A provider present in the handoff is emitted with the token and
//     NEVER with its raw key (the token path decrypts server-side for
//     the model list only; the raw key value is dropped before the
//     entry is marshaled). The canary leg of the rogue-agent sweep
//     greps the emitted batch for raw-key bytes.
//   - A provider absent from the handoff because its KIND is not
//     relay-frontable (bedrock/vertex/azure_openai/opencode — US-72.3
//     worklog D5) keeps the raw-key mixed-fleet path until US-72.5
//     decides coverage; the controller names these in
//     CredentialsStaged's message.
//   - A MISSING handoff under flag-on is staging-not-ready: NO
//     llm-provider entries at all (never a raw-key fallback — the
//     design's central property), a machine-readable BuildDegrade
//     (relay_staging_not_ready) and an audit row. Non-provider classes
//     (env-secrets, MCP) still deliver.
//   - The handoff revision participates in the manifest tier
//     (ManifestHashWithRelayRevision): a token renewal at ~TTL/2
//     changes the staged revision, which changes the manifest hash,
//     which mints a new seq — the US-70.2/70.3 conditional-pull/resync
//     machinery then delivers the fresh token with no pod restart.

import (
	"context"
	"encoding/json"
	"strings"
)

// DegradeRelayStagingNotReady is the loud-degrade reason emitted when
// relay-only is enabled but the controller-staged handoff Secret is
// absent or unreadable (staging not ready: the controller has not
// completed a pass, or the Secret was deleted out-of-band).
const DegradeRelayStagingNotReady = "relay_staging_not_ready"

// Relay batch-entry metadata keys (string-valued — the materializer's
// wire shape flattens metadata to map[string]string). agentd's relay
// liveness registry reads these off the durable batch file.
const (
	// RelayMetadataKey marks a token-emitted llm-provider entry.
	RelayMetadataKey = "relay"
	// RelayMetadataExpiresAtKey carries the token's RFC3339 expiry
	// (honored pod-side for the token_expired degrade code).
	RelayMetadataExpiresAtKey = "relayExpiresAt"
	// RelayMetadataRevisionKey carries the staged revision the token
	// came from (the controller's lineage conjunct compares it against
	// the workspace's staged-revision annotation).
	RelayMetadataRevisionKey = "relayRevision"
)

// Handoff Secret contract (US-72.3 worklog D2 — written by the
// controller's staging pass, read here): name
// `workspace-relay-<workspaceName>` in the workspace namespace, single
// data key `handoff` carrying the RelayHandoff JSON.
const (
	// RelayHandoffSecretPrefix builds the Secret name from the K8s
	// Workspace's name.
	RelayHandoffSecretPrefix = "workspace-relay-"
	// RelayHandoffDataKey is the Secret's single data key.
	RelayHandoffDataKey = "handoff"
)

// RelayHandoffSecretName derives the handoff Secret's name.
func RelayHandoffSecretName(workspaceName string) string {
	return RelayHandoffSecretPrefix + workspaceName
}

// RelayHandoff is the handoff Secret's single `handoff` JSON document
// (US-72.3 worklog D2 — the JSON tags are the wire contract with the
// controller's staging pass; parse-only here).
type RelayHandoff struct {
	Revision  string                 `json:"revision"`
	RouterURL string                 `json:"routerURL"`
	Providers []RelayHandoffProvider `json:"providers"`
}

// RelayHandoffProvider is one staged provider's token entry.
type RelayHandoffProvider struct {
	ProviderSlug   string   `json:"providerSlug"`
	Kind           string   `json:"kind"`
	Token          string   `json:"token"`
	RouterPath     string   `json:"routerPath"`
	BaseURL        string   `json:"baseURL"`
	ModelAllowlist []string `json:"modelAllowlist"`
	KeyID          string   `json:"keyID"`
	ExpiresAt      string   `json:"expiresAt"`
}

// RouterBase returns the router origin (no trailing slash) every
// provider baseURL is built on.
func (h *RelayHandoff) RouterBase() string {
	return strings.TrimSuffix(h.RouterURL, "/")
}

// Find returns the staged entry for a provider slug (the DECRYPTED
// provider-data slug — the same key the controller staged by).
func (h *RelayHandoff) Find(slug string) (RelayHandoffProvider, bool) {
	for _, p := range h.Providers {
		if p.ProviderSlug == slug {
			return p, true
		}
	}
	return RelayHandoffProvider{}, false
}

// RelayTokenSource resolves the controller-staged handoff for a
// workspace (production: the k8s `workspace-relay-<wsName>` Secret in
// the workspace namespace, data key `handoff`). A nil handoff with a
// nil error means "staged Secret absent" — staging not ready, never a
// raw fallback. A nil RelayTokenSource on the service means the
// deployment flag is OFF: legacy behavior, byte-identical.
type RelayTokenSource interface {
	RelayHandoff(ctx context.Context, workspaceID string) (*RelayHandoff, error)
}

// SetRelayTokenSource installs (or with nil, removes) the relay-only
// token source. Called once at API construction from the
// relayOnlyKeyDelivery config flag.
func (s *SecretService) SetRelayTokenSource(src RelayTokenSource) {
	s.relayTokens = src
}

// relayHandoffOpt reads the handoff under the flag; every nil-source
// call short-circuits (flag-off paths never touch the seam).
func (s *SecretService) relayHandoffOpt(ctx context.Context, workspaceID string) (*RelayHandoff, error) {
	if s.relayTokens == nil {
		return nil, nil
	}
	return s.relayTokens.RelayHandoff(ctx, workspaceID)
}

// relayBatchDegrade resolves the relay tier's contribution to a build:
// nil when the handoff is staged (or the flag is off); the loud
// relay_staging_not_ready degrade + audit row when it is not.
func (s *SecretService) relayBatchDegrade(ctx context.Context, ownerUserID, workspaceID string, handoff *RelayHandoff, handoffErr error) *BuildDegrade {
	if s.relayTokens == nil {
		return nil
	}
	if handoffErr == nil && handoff != nil {
		return nil
	}
	detail := "handoff Secret absent"
	if handoffErr != nil {
		detail = handoffErr.Error()
	}
	s.audit(ctx, ownerUserID, DegradeRelayStagingNotReady, nil, &workspaceID,
		map[string]string{"detail": detail})
	return &BuildDegrade{Reason: DegradeRelayStagingNotReady}
}

// relayMetadataFor builds the token-entry metadata document.
func relayMetadataFor(h *RelayHandoff, p RelayHandoffProvider) json.RawMessage {
	meta, err := json.Marshal(map[string]string{
		RelayMetadataKey:          "true",
		RelayMetadataExpiresAtKey: p.ExpiresAt,
		RelayMetadataRevisionKey:  h.Revision,
	})
	if err != nil {
		return nil
	}
	return meta
}

// relayEmitOutcome classifies one provider against the staged handoff.
type relayEmitOutcome int

const (
	// relayNotStaged: the provider's slug has no handoff entry (the
	// non-frontable kinds — US-72.3 D5 mixed-fleet raw path continues).
	relayNotStaged relayEmitOutcome = iota
	// relayEmitted: rewritten onto the token path (meta carries the
	// relay metadata).
	relayEmitted
	// relayEmptyToken: a handoff entry exists with an empty token —
	// corruption the controller never writes; skip loudly, never a
	// keyless entry and never a raw fallback under flag-on.
	relayEmptyToken
)

// applyRelayHandoff rewrites one decrypted provider entry onto the
// token path: apiKey = staged token, baseURL = RouterURL + RouterPath.
// Models and everything else are untouched (flag-on/flag-off behavior
// parity for the formatter).
func applyRelayHandoff(pd *LLMProviderData, h *RelayHandoff) (json.RawMessage, relayEmitOutcome) {
	staged, ok := h.Find(pd.Slug)
	if !ok {
		return nil, relayNotStaged
	}
	if staged.Token == "" {
		return nil, relayEmptyToken
	}
	pd.APIKey = staged.Token
	pd.BaseURL = h.RouterBase() + staged.RouterPath
	return relayMetadataFor(h, staged), relayEmitted
}
