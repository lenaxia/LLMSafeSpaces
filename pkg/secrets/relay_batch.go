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
//   - A MISSING handoff under flag-on is staging-not-ready: in STRICT
//     mode NO llm-provider entries at all (the fail-closed posture —
//     the design's central property, keyed relay_staging_not_ready); in
//     MIGRATION mode (design 0061 §4, the chart default) the pre-flip
//     RAW-key entries deliver — counted by
//     relay_fallback_deliveries_total, audited, and surfaced as the
//     relay_fallback_delivery degrade. Non-provider classes
//     (env-secrets, MCP) still deliver in both modes.
//   - The handoff revision participates in the manifest tier
//     (ManifestHashWithRelayRevision): a token renewal at ~TTL/2
//     changes the staged revision, which changes the manifest hash,
//     which mints a new seq — the US-70.2/70.3 conditional-pull/resync
//     machinery then delivers the fresh token with no pod restart.

import (
	"context"
	"encoding/json"
	"strings"

	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DegradeRelayStagingNotReady is the loud-degrade reason emitted when
// relay-only is enabled but the controller-staged handoff Secret is
// absent or unreadable (staging not ready: the controller has not
// completed a pass, or the Secret was deleted out-of-band).
const DegradeRelayStagingNotReady = "relay_staging_not_ready"

// DegradeRelayFallbackDelivery (design 0061 §4, M2): the migration-mode
// fallback outcome — staging was not ready at batch time and the batch
// delivered PRE-FLIP RAW-KEY entries instead (counted per provider by
// relay_fallback_deliveries_total, audited relay_fallback_delivery).
// NON-NIL by contract: the M4 CredentialsStaged hook surfaces it as
// False/relay_fallback_delivery so the migration is per-workspace
// visible (the counter alerts; the condition explains).
const DegradeRelayFallbackDelivery = "relay_fallback_delivery"

// IsRelayDegrade reports whether a BuildDegrade belongs to the relay
// tier (design 0061 §6/M4): the CredentialsStaged condition mirrors
// the RELAY outcome only — a DEK-tier degrade (dek_unwrap_failed,
// owner_no_keys) says nothing about relay staging (admin/org providers
// were still relay-rewritten and delivered) and must NOT surface as a
// relay-staging condition. The vocabulary lives HERE behind the
// builder seam: M2's relay_fallback_delivery joined this set (one
// place), and consumers never enumerate reasons themselves.
func IsRelayDegrade(d *BuildDegrade) bool {
	if d == nil {
		return false
	}
	switch d.Reason {
	case DegradeRelayStagingNotReady, DegradeRelayFallbackDelivery:
		return true
	default:
		return false
	}
}

// The M2 counters (design 0061 §4). Deliberately NOT promauto: agentd
// links this package too, and the series belong to the API process
// that builds batches — the API registers them at relay-install time
// (api/internal/app/relay_handoff.go, the registerRelayMetricsOnce
// precedent). Unregistered collectors still count in-process (the
// tests read them directly).
var (
	relayFallbackDeliveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_fallback_deliveries_total",
		Help: "Raw-key (pre-flip) llm-provider deliveries under relay-only because no usable staged token existed at batch time (migration mode; design 0061 §4). The stall detector: fires at the exact moment harm would begin.",
	}, []string{"workspace", "provider_slug"})

	relayDegradedBatches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_degraded_batches_total",
		Help: "Fail-closed relay batches (strict mode's not-ready mute, or any zero-entry relay degrade). Mode-independent detection: survives the strict flip (design 0061 §4's r3 addition).",
	}, []string{"workspace", "reason"})
)

// RelayFallbackMetrics exposes the M2 counters for the API's
// registration (relay_handoff.go MustRegisters them once at
// relay-install).
func RelayFallbackMetrics() (fallback, degraded *prometheus.CounterVec) {
	return relayFallbackDeliveries, relayDegradedBatches
}

// RelayFallbackForTest exposes the installed fallback mode (the
// RelayTokensForTest precedent — cross-package wiring tests assert the
// app seam installed MIGRATION, the builder's non-default).
func (s *SecretService) RelayFallbackForTest() bool { return s.relayFallback }

// SetRelayDeliveryFallback installs the fallback mode (design 0061 §4):
// true = MIGRATION (not-ready delivers pre-flip raw keys + the fallback
// counter); false/zero-value = STRICT (the fail-closed class-mute —
// the steady-state posture; the safe default until the API installs
// migration explicitly).
func (s *SecretService) SetRelayDeliveryFallback(enabled bool) {
	s.relayFallback = enabled
}

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
// nil error means "staged Secret absent" — staging not ready (strict
// mutes; migration falls back to raw keys, counted — M2). A nil
// RelayTokenSource on the service means the
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

// RelayTokensForTest exposes the installed relay source for cross-package
// wiring tests (the app.New seam test cannot reach the unexported field;
// the GetOutboxForTest precedent). Nil means the flag is off.
func (s *SecretService) RelayTokensForTest() RelayTokenSource { return s.relayTokens }

// RelayOnlyEnabled reports whether the relay-only token source is
// installed (the deployment's relayOnlyKeyDelivery flag reached the API
// server). Design 0061 §6/M4: the pod-bootstrap condition hook gates on
// this before writing CredentialsStaged — under flag-off the condition
// must stay absent (the W15 contract), so the relay paths' callers
// assert the flag rather than duplicate it.
func (s *SecretService) RelayOnlyEnabled() bool { return s.relayTokens != nil }

// relayHandoffOpt reads the handoff under the flag; every nil-source
// call short-circuits (flag-off paths never touch the seam).
func (s *SecretService) relayHandoffOpt(ctx context.Context, workspaceID string) (*RelayHandoff, error) {
	if s.relayTokens == nil {
		return nil, nil
	}
	return s.relayTokens.RelayHandoff(ctx, workspaceID)
}

// relayBatchDegrade resolves the relay tier's contribution to a build
// (design 0061 §4, M2 semantics):
//   - flag off / handoff staged: nil (unchanged);
//   - not ready (absent/err) + MIGRATION: the NON-NIL fallback degrade
//     (the M4 seam contract — CredentialsStaged=False/relay_fallback_delivery)
//     while the batch DELIVERS raw keys (buildCredentialEntries' mute is
//     lifted); the per-provider fallback counting lives at the entry
//     site, where the slug is known;
//   - not ready + STRICT: the fail-closed not-ready degrade + audit row
//   - relay_degraded_batches_total (the mode-independent detector).
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
	if s.relayFallback {
		// MIGRATION: deliver raw (the mute lifts), surface the fallback
		// outcome loudly. The batch is NOT fail-closed — the degraded
		// counter stays silent; relay_fallback_deliveries_total (fired
		// per provider at the entry site) is the alert surface.
		s.audit(ctx, ownerUserID, DegradeRelayFallbackDelivery, nil, &workspaceID,
			map[string]string{"detail": detail})
		return &BuildDegrade{Reason: DegradeRelayFallbackDelivery}
	}
	s.audit(ctx, ownerUserID, DegradeRelayStagingNotReady, nil, &workspaceID,
		map[string]string{"detail": detail})
	relayDegradedBatches.WithLabelValues(workspaceID, DegradeRelayStagingNotReady).Inc()
	return &BuildDegrade{Reason: DegradeRelayStagingNotReady}
}

// relayMetadataFor builds the token-entry metadata document. The
// marshal error is ignored BECAUSE it is unconstructible: every key and
// value is a Go string (json.Marshal of map[string]string cannot fail),
// so a nil return — which would strip the relay metadata the liveness
// registry keys on — is impossible by construction.
func relayMetadataFor(h *RelayHandoff, p RelayHandoffProvider) json.RawMessage {
	meta, _ := json.Marshal(map[string]string{
		RelayMetadataKey:          "true",
		RelayMetadataExpiresAtKey: p.ExpiresAt,
		RelayMetadataRevisionKey:  h.Revision,
	})
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
	// keyless entry (and never a fallback: an empty token is corrupt,
	// not not-ready — the design's fallback class is absent/expired).
	relayEmptyToken
	// relayExpired (M2): the staged token's expiry is past at batch
	// time — not-ready for THIS provider (per-provider granularity).
	relayExpired
)

// applyRelayHandoff rewrites one decrypted provider entry onto the
// token path: apiKey = staged token, baseURL = RouterURL + RouterPath.
// Models and everything else are untouched (flag-on/flag-off behavior
// parity for the formatter).
func applyRelayHandoff(pd *LLMProviderData, h *RelayHandoff, fallbackAllowed bool) (json.RawMessage, relayEmitOutcome) {
	staged, ok := h.Find(pd.Slug)
	if !ok {
		return nil, relayNotStaged
	}
	if staged.Token == "" {
		return nil, relayEmptyToken
	}
	// M2 (design 0061 §4): an EXPIRED token is not-ready at batch time.
	// Migration (fallbackAllowed): the relayExpired outcome carries to
	// the caller, which delivers the raw key + counts. Strict: the
	// rewrite proceeds below — the token DELIVERS and the existing
	// renewal path owns expiry (unchanged behavior).
	if exp, err := time.Parse(time.RFC3339, staged.ExpiresAt); err == nil && time.Now().After(exp) && fallbackAllowed {
		return nil, relayExpired
	}
	pd.APIKey = staged.Token
	pd.BaseURL = h.RouterBase() + staged.RouterPath
	return relayMetadataFor(h, staged), relayEmitted
}
