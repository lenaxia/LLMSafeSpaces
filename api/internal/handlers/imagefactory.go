// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// imageFactoryStore is the data-access surface for image-factory catalog
// and config data. Satisfied by *database.Service via duck-typing.
type imageFactoryStore interface {
	// Catalog reads.
	GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error)
	ListBases(ctx context.Context) ([]imagefactory.Base, error)
	GetBase(ctx context.Context, name, version string) (imagefactory.Base, error)
	ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error)
	ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error)
	GetKnownFailure(ctx context.Context, selectionHash, baseName string) (imagefactory.KnownFailure, error)

	// Config reads + writes.
	GetConfig(ctx context.Context, id string) (imagefactory.Config, error)
	GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error)
	ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error)
	CreateConfig(ctx context.Context, c *imagefactory.Config) error
	CreateConfigAndBuild(ctx context.Context, c *imagefactory.Config, b *imagefactory.Build) error
	DeleteConfig(ctx context.Context, id string) error
	RenameConfig(ctx context.Context, id, newName string) error

	// Build reads + writes.
	GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error)
	CreateBuild(ctx context.Context, b *imagefactory.Build) error
}

// orgResolver resolves the current user's org membership. Separate interface
// (ISP) because GetUserOrgID lives on *database.PgOrgStore, not
// *database.Service — the handler depends on both, injected separately.
type orgResolver interface {
	GetUserOrgID(ctx context.Context, userID string) (string, error)
	IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error)
}

// ImageFactoryHandler serves the consumer-facing image-factory endpoints
// (design/0046, design/0047). Read-only in S3; POST /configs in S4;
// callback + status derivation in S5; admin endpoints in S7.
type ImageFactoryHandler struct {
	store      imageFactoryStore
	orgs       orgResolver
	dispatcher buildDispatcher
	logger     pkginterfaces.LoggerInterface

	// S5: callback + failure handling.
	buildStore  buildStore
	imageRepo   string
	callbackURL string
	explainer   failureExplainer
	adminStore  extensionReviewer
}

// NewImageFactoryHandler constructs the handler.
func NewImageFactoryHandler(store imageFactoryStore, orgs orgResolver) *ImageFactoryHandler {
	return &ImageFactoryHandler{store: store, orgs: orgs}
}

// SetDispatcher wires the GH Actions build dispatcher.
func (h *ImageFactoryHandler) SetDispatcher(d buildDispatcher) {
	h.dispatcher = d
}

// SetLogger installs a structured logger so the dispatch-failure path can
// emit the underlying error (e.g. "gh dispatch: unexpected status 403: …").
// Without this, POST /configs returns a generic 503 "failed to dispatch
// build" and the root cause (a GitHub App permission gap, a token-mint
// failure, a network error) is silently dropped — exactly the
// observability gap that made the 2026-08-03 outage look like a
// wiring/version problem instead of a missing Actions:Write permission.
//
// The logger is optional: when nil, the handler falls back to a silent
// no-op so unit tests that don't care about log emission can omit it.
// Production wiring (api/internal/app/app.go) MUST install one — see
// TestImageFactoryHandler_LoggerWired in api/internal/app/ for the
// regression guard that enforces this.
func (h *ImageFactoryHandler) SetLogger(l pkginterfaces.LoggerInterface) {
	h.logger = l
}

// HasLogger reports whether a logger has been wired. Used by the app-level
// wiring test to enforce that production constructs the handler with
// SetLogger called — without it, the underlying error in the 503 path is
// silently dropped (the very gap this fix closes).
func (h *ImageFactoryHandler) HasLogger() bool {
	return h.logger != nil
}

// SetFailureExplainer wires the LLM failure explainer (S6). Optional —
// when nil, a fallback string is used.
func (h *ImageFactoryHandler) SetFailureExplainer(e failureExplainer) {
	h.explainer = e
}

// SetExtensionReviewer wires the extension review flagger (S6 attribution).
// Optional — when nil, attribution is recorded but no flag is set.
func (h *ImageFactoryHandler) SetExtensionReviewer(r extensionReviewer) {
	h.adminStore = r
}

// SetBuildStore wires the build-scoped store (callback transitions).
func (h *ImageFactoryHandler) SetBuildStore(bs buildStore, imageRepo, callbackURL string) {
	h.buildStore = bs
	h.imageRepo = imageRepo
	h.callbackURL = callbackURL
}

// CatalogResponse is the body of GET /v1/image-factory/catalog. Drives the
// settings-page extension/base picker and the known-failures greying-out.
// retired extensions are excluded — members select from live catalog entries.
type CatalogResponse struct {
	Architectures []string                    `json:"architectures"`
	Bases         []imagefactory.Base         `json:"bases"`
	Extensions    []imagefactory.Extension    `json:"extensions"`
	KnownFailures []imagefactory.KnownFailure `json:"knownFailures"`
}

// Catalog handles GET /v1/image-factory/catalog.
// Any authenticated user may read the catalog (so they can request additions
// via the issue tracker even under published_only policy). Non-retired
// extensions only; the default base is marked isDefault in the bases list.
func (h *ImageFactoryHandler) Catalog(c *gin.Context) {
	ctx := c.Request.Context()

	pc, err := h.store.GetPlatformConfig(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load platform config"})
		return
	}
	bases, err := h.store.ListBases(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bases"})
		return
	}
	exts, err := h.store.ListExtensions(ctx, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load extensions"})
		return
	}
	failures, err := h.store.ListKnownFailures(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load known failures"})
		return
	}

	if failures == nil {
		failures = []imagefactory.KnownFailure{}
	}
	for i := range failures {
		failures[i].FailureReason = ""
	}

	c.JSON(http.StatusOK, CatalogResponse{
		Architectures: pc.Architectures,
		Bases:         bases,
		Extensions:    exts,
		KnownFailures: failures,
	})
}

// ListConfigsResponse is the body of GET /v1/image-factory/configs.
type ListConfigsResponse struct {
	Configs []imagefactory.Config `json:"configs"`
}

// ListConfigs handles GET /v1/image-factory/configs.
// Returns the configs visible to the current user: their member-scoped
// configs, their org's org-scoped configs (if any), and platform-scoped
// configs. The launch picker renders these with friendly names + status pills.
func (h *ImageFactoryHandler) ListConfigs(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	ctx := c.Request.Context()

	ownerID := &userID
	var orgID *string
	if oid, err := h.orgs.GetUserOrgID(ctx, userID); err == nil && oid != "" {
		o := oid
		orgID = &o
	}

	cfgs, err := h.store.ListVisibleConfigs(ctx, ownerID, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load configs"})
		return
	}
	if cfgs == nil {
		cfgs = []imagefactory.Config{}
	}
	h.enrichWithBaseUpdates(ctx, cfgs)
	c.JSON(http.StatusOK, ListConfigsResponse{Configs: cfgs})
}

// enrichWithBaseUpdates stamps Config.UpdatesAvailable on each config
// (#928) from ONE catalog read regardless of config count. Failures are
// logged-and-skipped: the pill is advisory, never worth failing the
// read path over (a nil field simply renders no pill).
func (h *ImageFactoryHandler) enrichWithBaseUpdates(ctx context.Context, cfgs []imagefactory.Config) {
	if len(cfgs) == 0 {
		return
	}
	bases, err := h.store.ListBases(ctx)
	if err != nil {
		if h.logger != nil { // logger is optional (SetLogger doc); every other call site guards
			h.logger.Warn("image-factory: base-update enrichment skipped (catalog read failed)", "error", err.Error())
		}
		return
	}
	for i := range cfgs {
		// Issue #928 scope: the pill is for READY configs — building
		// ones haven't finished their first build, rejected ones can't
		// launch, and neither should suggest a re-save.
		if cfgs[i].Status != imagefactory.StatusReady {
			cfgs[i].UpdatesAvailable = nil
			continue
		}
		cfgs[i].UpdatesAvailable = imagefactory.ComputeBaseUpdates(cfgs[i], bases)
	}
}

// GetConfig handles GET /configs/:hash. Uses resolveConfigByHash (shared
// with DeleteConfig/RenameConfig) to search scopes in resolution order.
func (h *ImageFactoryHandler) GetConfig(c *gin.Context) {
	cfg, _, ok := h.resolveConfigByHash(c)
	if !ok {
		return
	}
	// Box-and-read-back: the slice element is a copy of cfg; enrich
	// mutates slice elements, so take the enriched one back out.
	boxed := []imagefactory.Config{cfg}
	h.enrichWithBaseUpdates(c.Request.Context(), boxed)
	c.JSON(http.StatusOK, boxed[0])
}
