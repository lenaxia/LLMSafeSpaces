// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ModelClient is the caller-shaped interface ModelsHandler needs from an
// agent client: fetch the catalog and push config changes. Worklog 0377
// H2-a: split from the fat AgentClient (5 methods) so the handler depends
// on exactly the 2 methods it uses, and fakes only need to stub 2.
type ModelClient interface {
	ListModels(ctx context.Context, userID, workspaceID string) ([]byte, error)
	PatchConfig(ctx context.Context, userID, workspaceID string, config map[string]any) error
}

// RelayStateChecker returns whether the relay injector has completed
// for the given workspace. Production implementation resolves podIP +
// password and calls /v1/readyz on the agentd admin port (4098).
// This is separate from AgentClient (which targets opencode port 4096
// with Basic auth) because the relay check uses the admin port with
// Bearer auth.
type RelayStateChecker func(ctx context.Context, userID, workspaceID string) bool

// ModelsHandler handles GET /workspaces/:id/models and
// PUT /workspaces/:id/model (US-29.5). Extracted from SecretsHandler
// to enforce single responsibility. Consumes ModelClient (H2-a) for
// opencode HTTP communication and ModelCatalogParser (H1-a′) to decode
// the catalog response into a typed Catalog.
type ModelsHandler struct {
	agentClient     ModelClient
	catalogParser   ModelCatalogParser
	wsUpdater       ModelStore
	policyChecker   OrgPolicyChecker
	metricsRecorder ModelSelectionRecorder
	modelCache      ModelCache
	relayActive     bool
	relayChecker    RelayStateChecker
	logger          pkginterfaces.LoggerInterface
}

// NewModelsHandler creates a ModelsHandler with the required ModelClient.
// The parser defaults to opencodeProviderParser; override via SetCatalogParser
// for tests or a future agent variant. Optional deps via the Set methods.
func NewModelsHandler(agentClient ModelClient) *ModelsHandler {
	return &ModelsHandler{
		agentClient:   agentClient,
		catalogParser: NewOpencodeProviderParser(),
		modelCache:    newInMemoryModelCache(),
	}
}

func (h *ModelsHandler) SetAgentClient(ac ModelClient)         { h.agentClient = ac }
func (h *ModelsHandler) SetCatalogParser(p ModelCatalogParser) { h.catalogParser = p }
func (h *ModelsHandler) SetModelStore(s ModelStore)            { h.wsUpdater = s }
func (h *ModelsHandler) SetPolicyChecker(p OrgPolicyChecker)   { h.policyChecker = p }
func (h *ModelsHandler) SetMetricsRecorder(r ModelSelectionRecorder) {
	h.metricsRecorder = r
}
func (h *ModelsHandler) SetModelCache(c ModelCache)                { h.modelCache = c }
func (h *ModelsHandler) SetRelayActive(active bool)                { h.relayActive = active }
func (h *ModelsHandler) SetRelayChecker(rc RelayStateChecker)      { h.relayChecker = rc }
func (h *ModelsHandler) SetLogger(l pkginterfaces.LoggerInterface) { h.logger = l }

func (h *ModelsHandler) warn(msg string, fields ...interface{}) {
	if h.logger != nil {
		h.logger.Warn(msg, fields...)
	}
}

// ListModels handles GET /api/v1/workspaces/:id/models.
func (h *ModelsHandler) ListModels(c *gin.Context) {
	workspaceID := c.Param("id")
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if h.agentClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model discovery unavailable"})
		return
	}

	// Check cache first (5s TTL).
	var annotated []annotatedModel
	var relayInjected bool
	var cacheHit bool
	if cached := h.modelCache.Get(workspaceID); cached != nil {
		var payload modelCachePayload
		if json.Unmarshal(cached, &payload) == nil {
			annotated = payload.Models
			relayInjected = payload.RelayInjected
			cacheHit = true
		}
	}

	// Issue #467: when serving from cache, re-check the live relayInjected
	// state via relayChecker. If it differs from the cached payload's
	// RelayInjected (typical case: false→true transition right after the
	// relay injector completes mid-cache-window), evict the cache and
	// re-fetch. Without this guard, the cached pre-injection snapshot is
	// served for up to the cache TTL (5s) plus agentd's providerCache TTL
	// (15s) — together a ~20s window where free-tier models appear under
	// providerID="opencode" while the workspace's agent-config.json /
	// auth.json have already been swapped to "opencode-relay". The
	// frontend faithfully forwards the stale providerID in /prompt
	// requests, opencode rejects them (the source provider is now in
	// disabled_providers), and the user sees a silent send failure on
	// the first message after workspace creation.
	//
	// relayChecker hits agentd's admin port — in steady state that hits
	// agentd's own 15s cache, so the round-trip is ~1ms in-cluster. We
	// only gate this on h.relayActive being true (relay disabled =
	// nothing to detect) and h.relayChecker being non-nil (defensive —
	// the field is optional in the handler API).
	//
	// On a transition we evict + force a refetch below. To avoid two
	// relayChecker round-trips on a transition (the fetch path at :148
	// repeats the call), we carry the result via livenessChecked +
	// liveRelayInjected so the fetch path can reuse it.
	var livenessChecked bool
	var liveRelayInjected bool
	if cacheHit && h.relayActive && h.relayChecker != nil {
		liveRelayInjected = h.relayChecker(c.Request.Context(), userID, workspaceID)
		livenessChecked = true
		if liveRelayInjected != relayInjected {
			h.modelCache.Evict(workspaceID)
			annotated = nil // force the fresh-fetch path below
		}
	}

	if annotated == nil {
		// Fetch model catalog via ModelClient (resolves podIP + password internally).
		body, err := h.agentClient.ListModels(c.Request.Context(), userID, workspaceID)
		if err != nil {
			if errors.Is(err, agent.ErrNoRunningPod) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workspace pod not running"})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to reach agent"})
			return
		}

		// Check relay injection state via the admin-port relay checker.
		// Reuse the value from the cache-eviction guard if we already
		// checked above (transitions evict + refall through here).
		if livenessChecked {
			relayInjected = liveRelayInjected
		} else if h.relayActive && h.relayChecker != nil {
			relayInjected = h.relayChecker(c.Request.Context(), userID, workspaceID)
		}

		// Parse the catalog via the injected parser (H1-a′).
		catalog, parseErr := h.catalogParser.Parse(body)
		if parseErr != nil {
			annotated = []annotatedModel{}
		} else {
			annotated = annotateModels(catalog, h.relayActive, relayInjected)
		}
		if serialized, serErr := json.Marshal(modelCachePayload{Models: annotated, RelayInjected: relayInjected}); serErr == nil {
			h.modelCache.Set(workspaceID, serialized)
		}
	}

	// Filter unavailable models.
	usable := make([]annotatedModel, 0, len(annotated))
	for _, m := range annotated {
		if m.Availability != ModelUnavailable {
			usable = append(usable, m)
		}
	}
	annotated = usable

	// US-43.8: Filter by org policy.
	if h.policyChecker != nil && h.wsUpdater != nil {
		annotated = h.filterByOrgPolicy(c.Request.Context(), workspaceID, annotated)
	}

	// Include current model selection + resolve providerID. The DB may hold
	// the qualified form ("providerID/modelID", what SetModel persists) or
	// the legacy flat form; the response is always flat for client
	// compatibility — qualified is a storage detail.
	currentModel := ""
	currentModelProviderID := ""
	if h.wsUpdater != nil {
		var err error
		currentModel, err = h.wsUpdater.GetDefaultModel(c.Request.Context(), workspaceID)
		if err != nil && h.logger != nil {
			h.logger.Warn("Failed to get default model", "error", err, "workspaceID", workspaceID)
		}
	}
	if idx := strings.LastIndex(currentModel, "/"); idx >= 0 {
		currentModel, currentModelProviderID = currentModel[idx+1:], currentModel[:idx]
	}
	if currentModelProviderID == "" {
		currentModelProviderID = resolveProviderID(annotated, currentModel)
	}
	markSelected(annotated, currentModel)

	c.JSON(http.StatusOK, gin.H{
		"models":                 annotated,
		"currentModel":           currentModel,
		"currentModelProviderID": currentModelProviderID,
	})
}

// SetModel handles PUT /api/v1/workspaces/:id/model.
func (h *ModelsHandler) SetModel(c *gin.Context) {
	workspaceID := c.Param("id")
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var req ModelSelectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model field is required"})
		return
	}

	// Accept both the flat catalog form ("gpt-5.5") and the qualified form
	// ("openai/gpt-5.5") that SetModel itself persists — clients echoing a
	// previously persisted selection must round-trip. Split on the LAST "/"
	// — agentd's resolveModelWithProvider uses LastIndex too, so a slashed
	// model ID ("a/b/c" = provider "a/b", model "c") parses identically on
	// both sides.
	//
	// Catalog model IDs may themselves contain slashes (OpenRouter-style
	// "vendor/model" — the EnrichProviders path). Such an ID advertised by
	// ListModels is ALSO accepted verbatim as the request value; the flat
	// form is only used for axis-independent checks, and catalog matching
	// tries the full ID first (modelExists(flat) fails for slashed IDs).
	flatModel := req.Model
	if idx := strings.LastIndex(req.Model, "/"); idx >= 0 {
		flatModel = req.Model[idx+1:]
	}

	if h.wsUpdater == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model selection unavailable"})
		return
	}

	if h.agentClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model selection unavailable (no agent client)"})
		return
	}

	// Fetch and parse the catalog (if pod is running) to validate + resolve.
	var catalog *Catalog
	catalogBytes, catErr := h.agentClient.ListModels(c.Request.Context(), userID, workspaceID)
	if catErr == nil && len(catalogBytes) > 0 {
		if parsed, parseErr := h.catalogParser.Parse(catalogBytes); parseErr == nil {
			catalog = parsed
		}
	}
	if catalog != nil && !catalog.modelExists(req.Model) && !catalog.modelExists(flatModel) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model not found in workspace catalog"})
		return
	}
	// If pod not running, skip validation (store optimistically).

	// Resolve the routing target once: the qualified "providerID/modelID"
	// the catalog maps this selection to (includes the relay remap).
	// Lookup key: the FULL request value when the catalog lists it as a
	// model ID (slashed catalog IDs — OpenRouter-style "vendor/model" —
	// match on their whole ID); otherwise the flat form. resolveModel
	// passes unfindable values through unchanged, so keying on
	// modelExists keeps resolution unambiguous.
	var resolved string
	if catalog != nil {
		relayInjected := false
		if h.relayActive && h.relayChecker != nil {
			relayInjected = h.relayChecker(c.Request.Context(), userID, workspaceID)
		}
		lookup := flatModel
		if catalog.modelExists(req.Model) {
			lookup = req.Model
		}
		resolved = catalog.resolveModel(lookup, h.relayActive, relayInjected)
	}

	// Org policy enforcement (PR #912). Runs on EVERY path — not only when
	// the catalog resolved (review round 2: pod-down or catalog-parse
	// failure previously bypassed the check entirely while still
	// persisting the selection).
	//   - Model axis: always enforceable — the flat ID is known regardless
	//     of catalog state.
	//   - Provider axis: enforced against every provider the selection can
	//     route through — the catalog-resolved target (what live routing
	//     uses) AND, on the degraded path, the request's own prefix (what
	//     gets persisted verbatim). Checking only the resolved provider
	//     while persisting the request prefix left an unchecked provider
	//     in the DB (review round 2, finding 3).
	if !h.modelSelectionAllowedByOrgPolicy(c.Request.Context(), workspaceID, req.Model, flatModel, resolved) {
		c.JSON(http.StatusForbidden, gin.H{"error": "model not allowed by organization policy"})
		return
	}

	// Persist to workspace metadata. Catalog-resolvable selections persist
	// the CATALOG-RESOLVED qualified form ("providerID/modelID") — the same
	// value policy checked and the live push routes, so pod-restart
	// routing matches live routing (review round 2 finding 3: a qualified
	// request whose prefix differs from the resolution previously
	// persisted its unchecked prefix verbatim). Agentd checks the provider
	// entry at boot and omits+warns when absent (incident 2026-08-16
	// follow-up). Catalog unavailable + qualified request persists the
	// request verbatim (prefix was policy-checked above); catalog
	// unavailable + flat request degrades to flat persistence, as before.
	persisted := flatModel
	if strings.Contains(resolved, "/") {
		persisted = resolved
	} else if strings.Contains(req.Model, "/") {
		persisted = req.Model
	}
	if err := h.wsUpdater.UpdateWorkspace(c.Request.Context(), workspaceID, types.WorkspaceUpdates{
		DefaultModel: &persisted,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workspace"})
		return
	}

	h.modelCache.Evict(workspaceID)

	// Epic 35: default model is persisted to PostgreSQL (UpdateWorkspace above).
	// The bootstrap endpoint reads it from Postgres at pod boot — no K8s Secret
	// is needed for pod-boot durability.

	// Push model selection to running agent. Only attempt when the catalog
	// fetch succeeded (M8-a: a down pod can't receive the patch anyway, and
	// the model is already persisted to the CRD for next boot).
	applied := false
	var resolvedModel string
	if catalog != nil {
		if resolved != "" && strings.Contains(resolved, "/") {
			config := map[string]any{"model": resolved}
			if patchErr := h.agentClient.PatchConfig(c.Request.Context(), userID, workspaceID, config); patchErr != nil {
				h.warn("PATCH model to agent failed", "error", patchErr.Error())
			} else {
				applied = true
				resolvedModel = resolved
			}
		}
	}

	// Metering.
	if h.metricsRecorder != nil {
		providerID := "unknown"
		if idx := strings.Index(resolvedModel, "/"); idx >= 0 {
			providerID = resolvedModel[:idx]
		}
		h.metricsRecorder.RecordModelSelection(flatModel, providerID)
	}
	c.JSON(http.StatusOK, gin.H{"model": req.Model, "persistedModel": persisted, "applied": applied})
}

// modelSelectionAllowedByOrgPolicy checks an explicit SetModel selection
// against the org's allowed-models/allowed-providers policy on every path.
//
// The model axis is always enforceable — it is known regardless of catalog
// state. Both the full request value and its flat tail are checked: the
// full value is the catalog-advertised ID for slashed catalog models
// (OpenRouter-style "vendor/model"), the tail is the historical flat form.
// The provider axis is enforced against every provider the selection can
// route through (FIRST-segment split — opencode's own routing convention,
// proven by the incident: bare "deepseek-v4-flash-free" parsed as provider
// + empty modelID; so "a/b/c" routes via provider "a"):
//   - the catalog-resolved target (resolved, "provider/model") — what
//     live routing and (now) persistence use;
//   - the request's own embedded prefix — what would be persisted
//     verbatim on the degraded catalog-unavailable path.
//
// A provider is skipped when it cannot be determined (flat request,
// unresolved catalog) — matching the axis-availability rule, not a
// fail-open for a known-denied provider. Fails open on policy-infra
// errors and for personal workspaces — matching ListModels'
// filterByOrgPolicy semantics; the policy is an org governance filter,
// and an infra hiccup must not brick model selection.
func (h *ModelsHandler) modelSelectionAllowedByOrgPolicy(ctx context.Context, workspaceID, rawReq, flatModel, resolved string) bool {
	if h.policyChecker == nil || h.wsUpdater == nil {
		return true
	}
	meta, err := h.wsUpdater.GetWorkspace(ctx, workspaceID)
	if err != nil || meta == nil || meta.OrgID == nil || *meta.OrgID == "" {
		return true // personal workspace — no org policy
	}
	pol, polErr := h.policyChecker.GetEffectivePolicy(ctx, *meta.OrgID)
	if polErr != nil || pol == nil {
		h.warn("SetModel: policy check unavailable, failing open", "workspaceID", workspaceID, "error", fmt.Sprint(polErr))
		return true
	}
	if !pol.IsModelAllowed(flatModel) && !pol.IsModelAllowed(rawReq) {
		return false
	}
	providerAllowed := func(providerID string) bool {
		if providerID == "" {
			return true // axis undeterminable on this path
		}
		return pol.IsProviderAllowed(providerID)
	}
	if idx := strings.Index(resolved, "/"); idx >= 0 {
		if !providerAllowed(resolved[:idx]) {
			return false
		}
	}
	if idx := strings.Index(rawReq, "/"); idx >= 0 {
		if !providerAllowed(rawReq[:idx]) {
			return false
		}
	}
	return true
}

// filterByOrgPolicy applies org allowed_models / allowed_providers.
func (h *ModelsHandler) filterByOrgPolicy(ctx context.Context, workspaceID string, models []annotatedModel) []annotatedModel {
	meta, err := h.wsUpdater.GetWorkspace(ctx, workspaceID)
	if err != nil || meta == nil || meta.OrgID == nil || *meta.OrgID == "" {
		return models
	}
	pol, polErr := h.policyChecker.GetEffectivePolicy(ctx, *meta.OrgID)
	if polErr != nil || pol == nil {
		return models
	}
	filtered := make([]annotatedModel, 0, len(models))
	for _, m := range models {
		if pol.IsModelAllowed(m.ID) && pol.IsProviderAllowed(m.ProviderID) {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

// resolveProviderID finds the providerID for the selected model.
// Returns "" if ambiguous (multiple providers have the same model ID).
func resolveProviderID(models []annotatedModel, modelID string) string {
	if modelID == "" {
		return ""
	}
	var providerID string
	for _, m := range models {
		if m.ID == modelID {
			if providerID == "" {
				providerID = m.ProviderID
			} else if providerID != m.ProviderID {
				return "" // collision
			}
		}
	}
	return providerID
}

func markSelected(models []annotatedModel, modelID string) {
	if modelID == "" {
		return
	}
	for i := range models {
		if models[i].ID == modelID {
			models[i].Selected = true
		}
	}
}
