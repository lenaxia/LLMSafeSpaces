// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/common"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// Design 0058 §4.1 hops 1–2 + §4.2 controller legs + §4.6 conditions +
// §4.9 lifecycle (US-72.3). The staging reconcile: seal each stageable bound
// BYO llm-provider credential into a per-workspace/per-provider envelope
// Secret in llm-relay, mint a scoped token per provider from the router's
// internal API, and write the token handoff Secret in the workspace
// namespace (the batch builder's future token source — US-72.4). Revocation
// is envelope Secret deletion (D2) with same-pass redaction-group
// unregister; the DR/residual-window terminator and seal-time
// pub-generation validation live here too (§4.2's controller-owned legs).
//
// Flag OFF (RelayStaging == nil) is byte-identical legacy behavior: the
// pass is a no-op (pinned by TestStaging_FlagOff_ZeroBehaviorChange).

const (
	// relaySealedGenerationAnnotation persists the pub generation the
	// workspace's envelopes were last sealed under (§4.2 reconcile
	// predicate input). Written only on change.
	relaySealedGenerationAnnotation = "llmsafespaces.dev/relay-sealed-generation"
	// relayStagedRevisionAnnotation persists the staged revision (the
	// content hash the handoff carries) — the lineage conjunct compares
	// the terminal spawned_rev against THIS.
	relayStagedRevisionAnnotation = "llmsafespaces.dev/relay-staged-revision"
	// relayStagedProvidersAnnotation persists the per-provider staging
	// bookkeeping (key digest, keyID, models digest, redaction-group ID).
	// The controller holds NO read access to the llm-relay envelope
	// Secrets beyond the name-scoped pub/mint-key reads (design 0058 §4.3
	// deviation note, US-72.3): envelopes are written BLIND
	// (delete+create) and every diffing decision — re-seal, revocation,
	// §4.9 unregister — reads THIS state instead. Digests are 12-hex
	// sha256 prefixes: non-reversible, not brute-forceable against
	// high-entropy provider keys.
	relayStagedProvidersAnnotation = "llmsafespaces.dev/relay-staged-providers"
	// relayLastRotateEscalationAnnotation lives on the llm-relay-mint-key
	// Secret (cluster-global: rotations are cluster-wide) and spaces
	// controller-initiated rotations ≥ relayRotateMinInterval (anti-storm).
	relayLastRotateEscalationAnnotation = "llmsafespaces.dev/last-rotate-escalation"
	// relayRotateMinInterval is the anti-storm floor — sized to the
	// prior-key retention default (10m, design §4.2).
	relayRotateMinInterval = 10 * time.Minute
	relayHandoffDataKey    = "handoff"
	// relayProviderCacheTTL bounds controller→API credential-source calls
	// (one fetch per workspace per window at the Active requeue cadence).
	relayProviderCacheTTL = 60 * time.Second
	relayDefaultTokenTTL  = 24 * time.Hour
	relayDefaultRouterURL = "http://llm-relay-router.llm-relay.svc.cluster.local"
)

// LLMProviderSource is the credential-source seam (worklog D1): the
// controller→API internal llm-providers endpoint. Interface for test
// injection.
type LLMProviderSource interface {
	LLMProviders(ctx context.Context, ownerUserID, workspaceID string) ([]secrets.LLMProviderData, error)
}

// RelayMintRequest is the POST /internal/v1/tokens body (US-72.2 contract).
type RelayMintRequest struct {
	WorkspaceID    string   `json:"workspaceID"`
	ProviderSlug   string   `json:"providerSlug"`
	BaseURL        string   `json:"baseURL"`
	ModelAllowlist []string `json:"modelAllowlist"`
	TTLSec         int64    `json:"ttlSeconds"`
	KeyID          string   `json:"keyID"`
}

// RelayRotateReceipt is the POST /internal/v1/keys/rotate response.
type RelayRotateReceipt struct {
	KeyID      string `json:"keyID"`
	Generation int64  `json:"generation"`
	PublicKey  []byte `json:"publicKey"`
}

// RelayRouterClient is the router internal-API surface the staging pass
// needs (mint + rotate). Interface for test injection.
type RelayRouterClient interface {
	MintToken(ctx context.Context, req RelayMintRequest) (string, error)
	RotateKeys(ctx context.Context) (RelayRotateReceipt, error)
}

// RelayStagingConfig wires the deployment flag. A nil *RelayStagingConfig on
// the reconciler disables staging entirely (flag off). Construction is
// NewRelayStagingConfig, which pins a NON-NIL redactor (design §4.9
// amendment: fail-open is test-adoption only).
type RelayStagingConfig struct {
	RouterURL      string
	Namespace      string
	TokenTTL       time.Duration
	ProviderSource LLMProviderSource
	RouterClient   RelayRouterClient
	redaction      secrets.StagedKeyRedactor
	Now            func() time.Time

	// APIReader is the DIRECT (non-cached) reader every llm-relay Secret
	// read goes through (pub, mint-key). The reconciler's cached client
	// cannot serve these: a namespace outside the cache scope fails
	// without an API call, and an informer inside it needs LIST+WATCH —
	// verbs the llm-relay Role deliberately withholds (§4.3 deviation
	// note). Writes stay on the reconciler client (controller-runtime
	// writes bypass the cache). Wired to mgr.GetAPIReader() in
	// SetupRelayStaging; nil falls back to the reconciler client (unit
	// tests that don't exercise the topology).
	APIReader client.Reader
}

// readerFor returns the reader llm-relay reads must use.
func (r *WorkspaceReconciler) relayReader() client.Reader {
	if r.RelayStaging.APIReader != nil {
		return r.RelayStaging.APIReader
	}
	return r.Client
}

// NewRelayStagingConfig constructs the production staging config. The
// redactor is mandatory (US-72.1 §4.9 amendment binding on US-72.3's
// construction path) — a nil redactor means an unregistered staged key could
// echo through diagnostics, which the design forbids.
func NewRelayStagingConfig(routerURL, namespace string, tokenTTL time.Duration, source LLMProviderSource, routerClient RelayRouterClient, redactor secrets.StagedKeyRedactor) (*RelayStagingConfig, error) {
	if redactor == nil {
		return nil, fmt.Errorf("relay staging: a nil redactor on the production path violates design 0058 §4.9 (fail-open is test-adoption only)")
	}
	if routerURL == "" {
		return nil, fmt.Errorf("relay staging: router URL is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("relay staging: namespace is required")
	}
	if source == nil || routerClient == nil {
		return nil, fmt.Errorf("relay staging: provider source and router client are required")
	}
	if tokenTTL <= 0 {
		tokenTTL = relayDefaultTokenTTL
	}
	return &RelayStagingConfig{
		RouterURL:      routerURL,
		Namespace:      namespace,
		TokenTTL:       tokenTTL,
		ProviderSource: source,
		RouterClient:   routerClient,
		redaction:      redactor,
		Now:            time.Now,
	}, nil
}

func (c *RelayStagingConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// handoff shapes (worklog D2) — the workspace-namespace Secret US-72.4's
// batch builder reads tokens from.
type relayHandoff struct {
	Revision  string              `json:"revision"`
	RouterURL string              `json:"routerURL"`
	Providers []relayHandoffEntry `json:"providers"`
}

type relayHandoffEntry struct {
	ProviderSlug   string   `json:"providerSlug"`
	Kind           string   `json:"kind"`
	Token          string   `json:"token"`
	RouterPath     string   `json:"routerPath"`
	BaseURL        string   `json:"baseURL"`
	ModelAllowlist []string `json:"modelAllowlist"`
	KeyID          string   `json:"keyID"`
	ExpiresAt      string   `json:"expiresAt"`
}

// relayDesiredProvider is a stageable bound provider with its resolved
// upstream endpoint.
type relayDesiredProvider struct {
	pd      secrets.LLMProviderData
	baseURL string
	models  []string
}

// relayStagedProviderState is the per-provider bookkeeping persisted in
// the relay-staged-providers annotation (see its const doc).
type relayStagedProviderState struct {
	KeyDigest    string `json:"keyDigest"`
	KeyID        string `json:"keyID"`
	ModelsDigest string `json:"modelsDigest"`
	RedactionID  string `json:"redactionID"`
}

// relayStaleClass is the §4.6 cause classification; only
// relayStaleCorruption is escalation-eligible (§4.2: rotating on a
// non-repairable cause is cluster-wide churn for nothing).
type relayStaleClass int

const (
	relayStaleNone relayStaleClass = iota
	relayStaleTokenExpiry
	relayStaleRevocation
	relayStaleDelivery
	relayStaleCorruption
)

func envelopeSecretName(workspaceName, slug string) string {
	return "llm-relay-env-" + workspaceName + "-" + slug
}

func handoffSecretName(workspaceName string) string {
	return "workspace-relay-" + workspaceName
}

// relayReadStagedState parses the annotation-persisted staging
// bookkeeping. Unparseible/absent = empty (fresh stage; the safe direction
// is re-sealing, never skipping).
func relayReadStagedState(ws *v1.Workspace) map[string]relayStagedProviderState {
	out := map[string]relayStagedProviderState{}
	if ws.Annotations == nil {
		return out
	}
	raw := ws.Annotations[relayStagedProvidersAnnotation]
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// relayDigest is the staging fingerprint: a 12-hex sha256 prefix.
// Non-reversible and not brute-forceable against high-entropy provider
// keys; persisted only in the workspace annotation for change detection.
func relayDigest(material []byte) string {
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:6])
}

// reconcileRelayStaging is the staging reconcile step. Errors are infra
// failures worth a requeue; business failures (source down, pub unreadable,
// mint failure) surface as conditions and return nil — pre-flip the raw-key
// path still delivers and must not be blocked by relay trouble (worklog D7).
func (r *WorkspaceReconciler) reconcileRelayStaging(ctx context.Context, ws *v1.Workspace) error {
	cfg := r.RelayStaging
	if cfg == nil {
		return nil
	}
	logger := log.FromContext(ctx)
	before := snapshotRelayState(ws)
	r.relayPendingRevocations = nil

	providers, err := cfg.ProviderSource.LLMProviders(ctx, ws.Spec.Owner.UserID, ws.Name)
	if err != nil {
		r.relayMarkStale(ws, v1.ReasonStageFailed, fmt.Sprintf("credential source unavailable: %v", err))
		return r.relayPersist(ctx, ws, before)
	}

	if _, err := r.ensureRelayMintKey(ctx); err != nil {
		logger.Error(err, "relay staging: mint key unavailable")
		r.relayMarkStale(ws, v1.ReasonStageFailed, fmt.Sprintf("mint key unavailable: %v", err))
		return r.relayPersist(ctx, ws, before)
	}

	pub, pubErr := r.relayReadPub(ctx)
	if pubErr != nil {
		// Shape-invalid pub bytes fail seal-time parsing loudly (§4.2);
		// shape-valid wrong-pub residual is undetectable here by
		// construction and arrives as the agentd credential_stale degrade.
		r.relayMarkStale(ws, v1.ReasonStalePubUnreadable, pubErr.Error())
		return r.relayPersist(ctx, ws, before)
	}
	keyID := secrets.HPKEKeyID(pub.Generation)

	desired, skipped := relayDesiredSet(providers)

	// No llm-relay List/Get on the envelope Secrets (§4.3): every diffing
	// decision reads the annotation-persisted staged state instead.
	staged := relayReadStagedState(ws)
	lastSealed := relayAnnotationInt(ws, relaySealedGenerationAnnotation)
	forceReseal := pub.Generation != lastSealed

	revoked, revokeErr := r.relayRevokeUndesired(ctx, ws, desired, staged)
	if revokeErr != nil {
		// Persist what DID complete + the pending-revocation condition,
		// then requeue — the retained envelope keeps resolving (D2
		// incomplete) until the delete lands.
		r.relayEvaluateStale(ws, revoked, false, nil)
		if err := r.relayPersist(ctx, ws, before); err != nil {
			return err
		}
		return revokeErr
	}

	// §4.2 reconcile predicate — the DR/residual-window terminator.
	// Generation unchanged while corruption-class stale persists under an
	// intact spawn-layer lineage → anti-storm-bounded rotate escalation.
	// Never for revocation-, delivery- (incl. #852 deferral), or
	// token-expiry-class causes.
	escalated := false
	if !forceReseal && lastSealed > 0 && len(staged) > 0 {
		class := r.relayStaleClass(ws, revoked)
		if class == relayStaleCorruption && r.relayLineageIntact(ws, desired, staged, keyID) {
			receipt, ok, err := r.relayEscalateRotate(ctx, ws)
			if err != nil {
				logger.Error(err, "relay staging: rotate escalation failed")
				if r.Recorder != nil {
					// The 10m anti-storm budget was consumed by the attempt
					// (deliberate — a failing rotate must not storm); make
					// that visible to the operator.
					r.Recorder.Eventf(ws, corev1.EventTypeWarning, "RelayRotateEscalationFailed",
						"rotate escalation failed (anti-storm 10m budget consumed): %v", err)
				}
			}
			if ok {
				// Seal-time generation validation (§4.2): the re-got pub
				// must match the rotate response before ANY envelope is
				// sealed against it — a pub read whose generation ≠ the
				// rotate receipt is rejected, never sealed against (a
				// mid-update read can never confirm a torn rotation).
				pub2, err2 := r.relayReadPub(ctx)
				if err2 != nil {
					r.relayMarkStale(ws, v1.ReasonStaleWrongPubLineage, fmt.Sprintf("post-rotate pub read failed: %v", err2))
				} else if verr := validateRotateReceipt(pub2, receipt); verr != nil {
					r.relayMarkStale(ws, v1.ReasonStaleWrongPubLineage, verr.Error())
				} else {
					pub, keyID, escalated = pub2, secrets.HPKEKeyID(pub2.Generation), true
					forceReseal = true
				}
			}
		}
	}

	sealer, err := secrets.NewHPKEStagingSealer(pub.PublicKey, keyID, cfg.redaction)
	if err != nil {
		r.relayMarkStale(ws, v1.ReasonStageFailed, fmt.Sprintf("sealer construction failed: %v", err))
		return r.relayPersist(ctx, ws, before)
	}

	// Seal loop. A provider (re-)seals when it was never staged, when the
	// pub generation moved, or when its keyID / model catalog / KEY VALUE
	// changed — the key digest closes the rotation gap where a credential
	// value changed under a stable slug and would otherwise stay sealed
	// forever. Envelopes are written BLIND (delete + create): the
	// controller holds no read on them, and confirmation is its own
	// write-acks — no decrypt, no resolve probe, no read-back (§4.2).
	for i := range desired {
		dp := &desired[i]
		wantModels, _ := json.Marshal(dp.models)
		wantKey := relayDigest([]byte(dp.pd.APIKey))
		wantModelsDigest := relayDigest(wantModels)
		old, had := staged[dp.pd.Slug]
		if !had || forceReseal || old.KeyID != keyID || old.ModelsDigest != wantModelsDigest || old.KeyDigest != wantKey {
			envelope, serr := sealer.Seal(ctx, []byte(dp.pd.APIKey))
			if serr != nil {
				// §4.6 "staged Secret absent": the envelope for a bound
				// provider is missing after the seal attempt.
				r.relayMarkStale(ws, v1.ReasonStaleEnvelopeMissing, fmt.Sprintf("envelope for provider %q absent: seal failed: %v", dp.pd.Slug, serr))
				return r.relayPersist(ctx, ws, before)
			}
			if err := r.writeRelayEnvelope(ctx, ws, dp, envelope, wantModels); err != nil {
				return err
			}
			// §4.9 lifecycle: the re-seal pass unregisters the superseded
			// envelope's rule group in the SAME pass — the annotation
			// carries the superseded group's ID (the envelope itself is
			// unreadable by design).
			if had {
				cfg.redaction.UnregisterStagedKey(old.RedactionID)
			}
			staged[dp.pd.Slug] = relayStagedProviderState{
				KeyDigest:    wantKey,
				KeyID:        keyID,
				ModelsDigest: wantModelsDigest,
				RedactionID:  secrets.StagedKeyRedactionID(envelope),
			}
		}
	}

	handoff, tokensFailed, err := r.relayMintTokens(ctx, ws, desired, keyID)
	if err != nil {
		return err
	}

	revision := relayStagedRevision(handoff, staged)
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[relaySealedGenerationAnnotation] = strconv.FormatInt(pub.Generation, 10)
	ws.Annotations[relayStagedRevisionAnnotation] = revision
	stagedJSON, err := json.Marshal(staged)
	if err != nil {
		return err
	}
	ws.Annotations[relayStagedProvidersAnnotation] = string(stagedJSON)

	if err := r.upsertRelayHandoff(ctx, ws, handoff); err != nil {
		return err
	}

	// Conditions (§4.6): staged carries the revision; stale carries the
	// classified cause; rejected mirrors router rejection telemetry.
	stagedMsg := fmt.Sprintf("%d provider(s) staged at revision %s (keyID %s)", len(desired), revision, keyID)
	if len(skipped) > 0 {
		stagedMsg += fmt.Sprintf("; %d skipped (not relay-frontable): %s", len(skipped), joinQuoted(skipped))
	}
	r.setCondition(ws, v1.WorkspaceConditionCredentialsStaged, "True",
		v1.ReasonCredentialsStaged, stagedMsg)
	r.relayEvaluateStale(ws, revoked, tokensFailed, handoff)
	if escalated {
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeNormal, "RelayRotateEscalated",
				"corruption/wrong-pub-class staleness under intact lineage escalated to key rotation (generation %d)", pub.Generation)
		}
	}
	return r.relayPersist(ctx, ws, before)
}

// relayDesiredSet splits the resolved providers into stageable (with their
// effective upstream endpoint) and skipped (kinds the BYO router cannot
// front, or custom credentials without a BaseURL — worklog D5).
func relayDesiredSet(providers []secrets.LLMProviderData) (desired []relayDesiredProvider, skipped []string) {
	for _, pd := range providers {
		if !secrets.ProviderRelayStageable(pd.Kind) {
			skipped = append(skipped, pd.Slug+" (kind "+pd.Kind+": SDK-shaped auth, not relay-frontable)")
			continue
		}
		base := pd.BaseURL
		if base == "" {
			base = secrets.ProviderDefaultBaseURL(pd.Kind)
		}
		if base == "" {
			skipped = append(skipped, pd.Slug+" (kind "+pd.Kind+": no baseURL and no kind default)")
			continue
		}
		models := make([]string, 0, len(pd.Models))
		for _, m := range pd.Models {
			models = append(models, m.ID)
		}
		desired = append(desired, relayDesiredProvider{pd: pd, baseURL: base, models: models})
	}
	return desired, skipped
}

// relayRevokeUndesired implements revocation = Secret deletion (D2): an
// envelope whose provider is no longer bound (unbind/credential delete) is
// deleted, its redaction group unregistered in the same pass, and a
// one-pass CredentialStale(revocation-class) is raised (worklog D7).
func (r *WorkspaceReconciler) relayRevokeUndesired(ctx context.Context, ws *v1.Workspace, desired []relayDesiredProvider, staged map[string]relayStagedProviderState) ([]string, error) {
	want := make(map[string]bool, len(desired))
	for i := range desired {
		want[desired[i].pd.Slug] = true
	}
	var revoked []string
	var pending []string
	for slug, old := range staged {
		if want[slug] {
			continue
		}
		// Blind delete (name derived; no read on the envelope Secrets).
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: envelopeSecretName(ws.Name, slug), Namespace: r.RelayStaging.Namespace}}
		if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
			// Fail-closed on the D2 contract: the envelope is RETAINED and
			// stays in the staged map so the next pass retries; the caller
			// requeues on the returned error. Reported distinctly — a
			// pending revocation must not read as a completed one.
			log.FromContext(ctx).Error(err, "relay staging: revocation delete failed — envelope retained, retrying", "secret", sec.Name)
			pending = append(pending, slug)
			continue
		}
		r.RelayStaging.redaction.UnregisterStagedKey(old.RedactionID)
		delete(staged, slug)
		revoked = append(revoked, slug)
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeNormal, "CredentialRevoked",
				"provider %q unbound: llm-relay envelope deleted (revocation = Secret deletion, D2); in-flight tokens fail closed until the batch applies the removal", slug)
		}
	}
	r.relayPendingRevocations = pending
	if len(pending) > 0 {
		return revoked, fmt.Errorf("revocation incomplete: envelope delete failed for %s — retained in llm-relay, retrying", joinQuoted(pending))
	}
	return revoked, nil
}

func (r *WorkspaceReconciler) relayMintTokens(ctx context.Context, ws *v1.Workspace, desired []relayDesiredProvider, keyID string) (*relayHandoff, bool, error) {
	cfg := r.RelayStaging
	cached := r.relayReadHandoff(ctx, ws)
	now := cfg.now()
	out := &relayHandoff{RouterURL: cfg.RouterURL}
	tokensFailed := false
	for i := range desired {
		dp := &desired[i]
		var entry relayHandoffEntry
		if cached != nil {
			for _, c := range cached.Providers {
				if c.ProviderSlug == dp.pd.Slug {
					entry = c
					break
				}
			}
		}
		fresh := entry.Token != "" && entry.KeyID == keyID && entry.ExpiresAt != ""
		if fresh {
			if exp, err := time.Parse(time.RFC3339, entry.ExpiresAt); err != nil || now.After(exp.Add(-cfg.TokenTTL/2)) {
				fresh = false
			}
		}
		if !fresh {
			token, err := cfg.RouterClient.MintToken(ctx, RelayMintRequest{
				WorkspaceID:    ws.Name,
				ProviderSlug:   dp.pd.Slug,
				BaseURL:        dp.baseURL,
				ModelAllowlist: dp.models,
				TTLSec:         int64(cfg.TokenTTL / time.Second),
				KeyID:          keyID,
			})
			if err != nil {
				log.FromContext(ctx).Error(err, "relay staging: token mint failed", "provider", dp.pd.Slug)
				// The handoff Secret is the ONLY token store: a transient
				// mint failure at the renewal boundary must retain the
				// still-valid cached token rather than persisting a
				// reduced handoff. Only a fully expired (or keyID-stale)
				// token is dropped, with the loud tokensFailed signal.
				if entry.Token != "" && entry.KeyID == keyID && entry.ExpiresAt != "" {
					if exp, perr := time.Parse(time.RFC3339, entry.ExpiresAt); perr == nil && now.Before(exp) {
						out.Providers = append(out.Providers, entry)
						continue
					}
				}
				tokensFailed = true
				continue
			}
			entry = relayHandoffEntry{
				ProviderSlug:   dp.pd.Slug,
				Kind:           dp.pd.Kind,
				Token:          token,
				RouterPath:     fmt.Sprintf("/w/%s/%s/v1", ws.Name, dp.pd.Slug),
				BaseURL:        dp.baseURL,
				ModelAllowlist: dp.models,
				KeyID:          keyID,
				ExpiresAt:      now.Add(cfg.TokenTTL).UTC().Format(time.RFC3339),
			}
		}
		out.Providers = append(out.Providers, entry)
	}
	return out, tokensFailed, nil
}

// relayStaleClass classifies the CURRENT stale causes (§4.6 → §4.2
// escalation classes). The corruption/wrong-pub class is the agentd
// `credential_stale` degrade — the router's resolve-failure code for a
// wrong or corrupt keypair lineage (live emission lands with US-72.4's
// degrade codes; the classification and predicate land here, per the story).
func (r *WorkspaceReconciler) relayStaleClass(ws *v1.Workspace, revoked []string) relayStaleClass {
	degraded := ""
	if ws.Status.SecretsDelivery != nil {
		degraded = ws.Status.SecretsDelivery.DegradedReason
	}
	// A revocation performed this pass wins over a co-present corruption
	// degrade: the degrade is most plausibly ABOUT the revoked provider
	// (its token now fails closed by design), and escalation is explicitly
	// forbidden for revocation-class (§4.2) — anti-churn conservatism.
	switch {
	case len(revoked) > 0 || len(r.relayPendingRevocations) > 0:
		return relayStaleRevocation
	case degraded == "credential_stale":
		return relayStaleCorruption
	case degraded == "token_expired":
		return relayStaleTokenExpiry
	case degraded != "":
		return relayStaleDelivery
	default:
		return relayStaleNone
	}
}

// relayEvaluateStale sets/clears CredentialStale and CredentialRejected from
// the recomputed causes (worklog D7: revocation-class is one-pass; the
// durable post-72.4 signal is the degrade feed).
func (r *WorkspaceReconciler) relayEvaluateStale(ws *v1.Workspace, revoked []string, tokensFailed bool, handoff *relayHandoff) {
	class := r.relayStaleClass(ws, revoked)
	degraded := ""
	if ws.Status.SecretsDelivery != nil {
		degraded = ws.Status.SecretsDelivery.DegradedReason
	}

	switch rejected := relayRouterRejection(degraded); {
	case rejected != "":
		r.setCondition(ws, v1.WorkspaceConditionCredentialRejected, "True",
			v1.ReasonCredentialRouterRejected, fmt.Sprintf("router rejected a token: %s", rejected))
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, string(v1.ReasonCredentialRouterRejected),
				"relay router rejected a token (%s) — an operator-visible anomaly, never silent (§4.7)", rejected)
		}
	default:
		removeCondition(ws, v1.WorkspaceConditionCredentialRejected)
	}

	switch {
	case tokensFailed:
		r.setCondition(ws, v1.WorkspaceConditionCredentialStale, "True",
			v1.ReasonStageFailed, "token mint failed; staged tokens incomplete")
	case class == relayStaleCorruption:
		r.setCondition(ws, v1.WorkspaceConditionCredentialStale, "True",
			v1.ReasonStaleWrongPubLineage, "corruption/wrong-pub-class resolve failure (§4.2's one escalating cause)")
	case class == relayStaleTokenExpiry:
		r.setCondition(ws, v1.WorkspaceConditionCredentialStale, "True",
			v1.ReasonStaleTokenExpired, "token expired; renewal owns this cause")
	case class == relayStaleRevocation:
		msg := fmt.Sprintf("credential revoked: %s — envelope deleted (D2); resolves fail closed until the batch applies the removal", joinQuoted(revoked))
		if len(r.relayPendingRevocations) > 0 {
			msg += fmt.Sprintf("; revocation INCOMPLETE (delete failed, retained + retrying): %s", joinQuoted(r.relayPendingRevocations))
		}
		r.setCondition(ws, v1.WorkspaceConditionCredentialStale, "True", v1.ReasonStaleRevoked, msg)
	case class == relayStaleDelivery:
		r.setCondition(ws, v1.WorkspaceConditionCredentialStale, "True",
			v1.ReasonStaleDeliveryDeferred, fmt.Sprintf("staged material not applied by the running pod: %s (a wait or fault rotation cannot repair)", degraded))
	default:
		removeCondition(ws, v1.WorkspaceConditionCredentialStale)
	}
}

// relayRouterRejection maps agentd-forwarded router reject codes (§4.7) to
// the CredentialRejected cause; "" when the degrade is not rejection-class.
func relayRouterRejection(degraded string) string {
	switch degraded {
	case "scope_violation", "sanitization_refused", "quota_exceeded", "credential_rejected":
		return degraded
	default:
		return ""
	}
}

// relayLineageIntact is §4.2's spawn-layer conjunct: every desired
// provider is staged under the CURRENT keyID AND the child spawned with
// the staged revision — the `spawned_rev`-class TERMINAL signal, not the
// batch-apply anchor, so the #852 deferral window (fresh token applied,
// restart waiting behind busy sessions) reads as pending-delivery and
// never escalates.
func (r *WorkspaceReconciler) relayLineageIntact(ws *v1.Workspace, desired []relayDesiredProvider, staged map[string]relayStagedProviderState, keyID string) bool {
	for i := range desired {
		old, ok := staged[desired[i].pd.Slug]
		if !ok || old.KeyID != keyID {
			return false
		}
	}
	stagedRev := ws.Annotations[relayStagedRevisionAnnotation]
	if stagedRev == "" {
		return false
	}
	sd := ws.Status.SecretsDelivery
	return sd != nil && sd.SpawnedRev != "" && sd.SpawnedRev == stagedRev
}

// relayEscalateRotate fires the bounded rotate escalation. The anti-storm
// timestamp is recorded on the mint-key Secret (cluster-global, survives
// controller restart) BEFORE the call, so a failing rotate cannot storm.
func (r *WorkspaceReconciler) relayEscalateRotate(ctx context.Context, ws *v1.Workspace) (RelayRotateReceipt, bool, error) {
	cfg := r.RelayStaging
	key := &corev1.Secret{}
	if err := r.relayReader().Get(ctx, types.NamespacedName{Name: secrets.RelayMintKeyName, Namespace: cfg.Namespace}, key); err != nil {
		return RelayRotateReceipt{}, false, err
	}
	if last := key.Annotations[relayLastRotateEscalationAnnotation]; last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			if cfg.now().Sub(t) < relayRotateMinInterval {
				return RelayRotateReceipt{}, false, nil
			}
		}
	}
	if key.Annotations == nil {
		key.Annotations = map[string]string{}
	}
	key.Annotations[relayLastRotateEscalationAnnotation] = cfg.now().UTC().Format(time.RFC3339)
	if err := r.Update(ctx, key); err != nil {
		return RelayRotateReceipt{}, false, err
	}
	receipt, err := cfg.RouterClient.RotateKeys(ctx)
	if err != nil {
		return RelayRotateReceipt{}, false, err
	}
	return receipt, true, nil
}

// validateRotateReceipt is the seal-time generation validation (§4.2): the
// pub Secret the controller just read must carry exactly the generation and
// public key the rotate response named, or NOTHING is sealed against it.
func validateRotateReceipt(pub *secrets.HPKEPubPayload, receipt RelayRotateReceipt) error {
	if pub.Generation != receipt.Generation {
		return fmt.Errorf("pub generation %d != rotate receipt %d — torn rotation refused; nothing sealed", pub.Generation, receipt.Generation)
	}
	if !bytes.Equal(pub.PublicKey, receipt.PublicKey) {
		return fmt.Errorf("pub public key does not match rotate receipt (generation %d) — torn rotation refused; nothing sealed", pub.Generation)
	}
	return nil
}

func (r *WorkspaceReconciler) relayReadPub(ctx context.Context) (*secrets.HPKEPubPayload, error) {
	sec := &corev1.Secret{}
	// Deliberately NO llm-relay-kek probe: the Role's read carve-out is
	// name-scoped to pub + mint-key (§4.3 deviation note), so a KEK GET is
	// Forbidden in-cluster and the KMS-vs-HPKE distinction cannot be made
	// by the controller without widening the grant — deferred to US-72.5
	// with the rest of the KMS-mode wiring (worklog D4).
	if err := r.relayReader().Get(ctx, types.NamespacedName{Name: secrets.RelayPubSecretName, Namespace: r.RelayStaging.Namespace}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s not found in namespace %s — router not bootstrapped, or a KMS-mode (llm-relay-kek) deployment where controller staging is not yet wired (US-72.5)", secrets.RelayPubSecretName, r.RelayStaging.Namespace)
		}
		return nil, fmt.Errorf("reading %s: %w", secrets.RelayPubSecretName, err)
	}
	pub, err := secrets.ParseHPKEPubPayload(sec.Data[secrets.RelayPubDataKey])
	if err != nil {
		return nil, fmt.Errorf("shape-invalid %s payload (seal-time parse fails loudly): %w", secrets.RelayPubSecretName, err)
	}
	return pub, nil
}

// ensureRelayMintKey create-or-adopts the controller-held mint key
// (byo_run.go documents this Secret as controller-created; the router reads
// it lazily so boot ordering never wedges).
func (r *WorkspaceReconciler) ensureRelayMintKey(ctx context.Context) (string, error) {
	sec := &corev1.Secret{}
	err := r.relayReader().Get(ctx, types.NamespacedName{Name: secrets.RelayMintKeyName, Namespace: r.RelayStaging.Namespace}, sec)
	if err == nil {
		if key := string(sec.Data[secrets.RelayMintKeyDataKey]); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("%s exists with an empty %q", secrets.RelayMintKeyName, secrets.RelayMintKeyDataKey)
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayMintKeyName, Namespace: r.RelayStaging.Namespace},
		Data:       map[string][]byte{secrets.RelayMintKeyDataKey: []byte(common.GenerateRandomString(43))},
	}
	if err := r.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.ensureRelayMintKey(ctx)
		}
		return "", err
	}
	return string(sec.Data[secrets.RelayMintKeyDataKey]), nil
}

// writeRelayEnvelope writes an envelope Secret BLIND: Delete by derived
// name (NotFound = fresh stage), then Create. The controller holds no read
// on the envelope Secrets (design 0058 §4.3 deviation note) — the
// annotation bookkeeping upstream decides whether a write is needed at
// all, so the steady-state pass performs zero calls here.
func (r *WorkspaceReconciler) writeRelayEnvelope(ctx context.Context, ws *v1.Workspace, dp *relayDesiredProvider, envelope string, modelsJSON []byte) error {
	name := envelopeSecretName(ws.Name, dp.pd.Slug)
	stub := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.RelayStaging.Namespace}}
	if err := r.Delete(ctx, stub); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Cross-namespace owner references are forbidden — envelope lifetime is
	// managed explicitly (relayDeleteEnvelopes on termination).
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.RelayStaging.Namespace,
			Labels: map[string]string{
				secrets.RelayEnvWorkspaceLabel: ws.Name,
				secrets.RelayEnvProviderLabel:  dp.pd.Slug,
			},
		},
		Data: map[string][]byte{
			secrets.RelayEnvDataKey:   []byte(envelope),
			secrets.RelayEnvModelsKey: modelsJSON,
		},
	}
	return r.Create(ctx, sec)
}

func (r *WorkspaceReconciler) upsertRelayHandoff(ctx context.Context, ws *v1.Workspace, handoff *relayHandoff) error {
	handoff.Revision = relayStagedRevision(handoff, relayReadStagedState(ws))
	data, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	name := handoffSecretName(ws.Name)
	sec := &corev1.Secret{}
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ws.Namespace}, sec)
	if getErr == nil {
		if bytes.Equal(sec.Data[relayHandoffDataKey], data) {
			return nil // steady state: nothing staged changed
		}
		sec.Data = map[string][]byte{relayHandoffDataKey: data}
		return r.Update(ctx, sec)
	}
	if !apierrors.IsNotFound(getErr) {
		return getErr
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ws.Namespace},
		Data:       map[string][]byte{relayHandoffDataKey: data},
	}
	if err := controllerutil.SetControllerReference(ws, sec, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, sec)
}

func (r *WorkspaceReconciler) relayReadHandoff(ctx context.Context, ws *v1.Workspace) *relayHandoff {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: handoffSecretName(ws.Name), Namespace: ws.Namespace}, sec); err != nil {
		return nil
	}
	var h relayHandoff
	if json.Unmarshal(sec.Data[relayHandoffDataKey], &h) != nil {
		return nil
	}
	return &h
}

// relayDeleteEnvelopes removes every envelope Secret for a workspace
// (termination path — cross-namespace Secrets get no owner-ref GC) and
// unregisters each redaction group, by the annotation bookkeeping (no
// llm-relay reads; §4.3).
func (r *WorkspaceReconciler) relayDeleteEnvelopes(ctx context.Context, ws *v1.Workspace) {
	if r.RelayStaging == nil {
		return
	}
	staged := relayReadStagedState(ws)
	for slug, old := range staged {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: envelopeSecretName(ws.Name, slug), Namespace: r.RelayStaging.Namespace}}
		if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "relay staging: termination envelope delete failed", "secret", sec.Name)
			continue
		}
		r.RelayStaging.redaction.UnregisterStagedKey(old.RedactionID)
	}
}

func (r *WorkspaceReconciler) relayMarkStale(ws *v1.Workspace, reason, message string) {
	r.setCondition(ws, v1.WorkspaceConditionCredentialsStaged, "False", reason, message)
	// CredentialStale is deliberately NOT cleared here: a staging-infra
	// outage (pub unreadable, source down) must not erase a durable
	// corruption-class signal the operator was already shown.
}

// relayPersist commits staging annotations + conditions. Both subresource
// writes refresh the object from the store (the suspend-request single-
// writer precedent), so the metadata write re-applies the annotations the
// status write's copy-back dropped. Change-gated: the steady-state pass
// (same revision, same conditions) performs zero writes.
func (r *WorkspaceReconciler) relayPersist(ctx context.Context, ws *v1.Workspace, before relayStateSnapshot) error {
	desired := map[string]string{}
	for k, v := range ws.Annotations {
		desired[k] = v
	}
	statusDirty := relayConditionsChanged(before, ws)
	annotationsDirty := desired[relaySealedGenerationAnnotation] != before.sealedGeneration ||
		desired[relayStagedRevisionAnnotation] != before.stagedRevision ||
		desired[relayStagedProvidersAnnotation] != before.stagedProviders
	if !statusDirty && !annotationsDirty {
		return nil
	}
	if statusDirty {
		if err := r.Status().Update(ctx, ws); err != nil {
			return err
		}
	}
	if !annotationsDirty {
		return nil
	}
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	for k, v := range desired {
		if ws.Annotations[k] != v {
			ws.Annotations[k] = v
		}
	}
	return r.Update(ctx, ws)
}

// relayStateSnapshot is the change-detection input for relayPersist.
type relayStateSnapshot struct {
	sealedGeneration string
	stagedRevision   string
	stagedProviders  string
	conditions       []relayCondSig
}

type relayCondSig struct {
	typ    v1.WorkspaceConditionType
	status string
	reason string
}

func snapshotRelayState(ws *v1.Workspace) relayStateSnapshot {
	s := relayStateSnapshot{}
	if ws.Annotations != nil {
		s.sealedGeneration = ws.Annotations[relaySealedGenerationAnnotation]
		s.stagedRevision = ws.Annotations[relayStagedRevisionAnnotation]
		s.stagedProviders = ws.Annotations[relayStagedProvidersAnnotation]
	}
	for _, c := range ws.Status.Conditions {
		switch c.Type {
		case v1.WorkspaceConditionCredentialsStaged, v1.WorkspaceConditionCredentialStale, v1.WorkspaceConditionCredentialRejected:
			s.conditions = append(s.conditions, relayCondSig{typ: c.Type, status: c.Status, reason: c.Reason})
		}
	}
	return s
}

func relayConditionsChanged(before relayStateSnapshot, ws *v1.Workspace) bool {
	now := snapshotRelayState(ws)
	if len(now.conditions) != len(before.conditions) {
		return true
	}
	for i := range now.conditions {
		if now.conditions[i] != before.conditions[i] {
			return true
		}
	}
	return false
}

// relayStagedRevision derives the staged revision: a deterministic content
// hash over the staged provider set (slug, envelope-independent token expiry,
// keyID-bearing token identity via entry fields). It changes exactly when
// any envelope or token changes, so US-72.4 can compare it against
// spawned_rev and CredentialsStaged carries it as the revision.
func relayStagedRevision(h *relayHandoff, staged map[string]relayStagedProviderState) string {
	if h == nil {
		return ""
	}
	parts := make([]string, 0, len(h.Providers))
	for _, p := range h.Providers {
		keyDigest := ""
		if old, ok := staged[p.ProviderSlug]; ok {
			keyDigest = old.KeyDigest + "/" + old.ModelsDigest
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", p.ProviderSlug, p.KeyID, p.ExpiresAt, p.BaseURL, keyDigest))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s;%s", h.RouterURL, joinWith(parts, ";"))))
	return "r" + hex.EncodeToString(sum[:6])
}

func relayAnnotationInt(ws *v1.Workspace, key string) int64 {
	if ws.Annotations == nil {
		return 0
	}
	v, err := strconv.ParseInt(ws.Annotations[key], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func joinQuoted(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += "\"" + s + "\""
	}
	return out
}

func joinWith(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}
