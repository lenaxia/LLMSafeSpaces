// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// imageFactoryStore is the data-access surface the image-factory handlers
// depend on. Local interface (the repo DI convention — see auditStore,
// CredentialStore) so handlers are unit-testable with a fake; *database.Service
// satisfies it via duck-typing. Grown per story as handlers need more.
type imageFactoryStore interface {
	// Catalog reads.
	GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error)
	ListBases(ctx context.Context) ([]imagefactory.Base, error)
	ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error)
	ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error)

	// Config reads.
	GetConfig(ctx context.Context, id string) (imagefactory.Config, error)
	GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error)
	ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error)

	// Org resolution for the current user (single-org model; "" if none).
	GetUserOrgID(ctx context.Context, userID string) (string, error)
}

// ImageFactoryHandler serves the consumer-facing image-factory endpoints
// (design/0046, design/0047). Read-only in S3; POST /configs lands in S4,
// admin endpoints in S7, callback in S5.
type ImageFactoryHandler struct {
	store imageFactoryStore
}

// NewImageFactoryHandler constructs the handler.
func NewImageFactoryHandler(store imageFactoryStore) *ImageFactoryHandler {
	return &ImageFactoryHandler{store: store}
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

	// Strip admin-only fields from known failures before returning to members.
	// FailureReason is json:"-" already (never serialized), but be explicit:
	// the consumer only needs the explanation + retriable flag for greying.
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
	if oid, err := h.store.GetUserOrgID(ctx, userID); err == nil && oid != "" {
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
	c.JSON(http.StatusOK, ListConfigsResponse{Configs: cfgs})
}

// GetConfig handles GET /v1/image-factory/configs/:hash.
// Decodes a schematic: returns the config's frozen resolved_values (the
// cached projection of the immutable extension values at save time) plus its
// status. Used by "what's in this image?" inspection. Scopes searched in
// friendly-name resolution order: member, then org, then platform.
func (h *ImageFactoryHandler) GetConfig(c *gin.Context) {
	hash := c.Param("hash")
	if hash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "hash is required"})
		return
	}
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	ctx := c.Request.Context()

	var orgID *string
	if oid, err := h.store.GetUserOrgID(ctx, userID); err == nil && oid != "" {
		o := oid
		orgID = &o
	}
	uid := userID

	// Search scopes in resolution order: member, org, platform.
	for _, scope := range []imagefactory.ConfigScope{
		imagefactory.ScopeMember,
		imagefactory.ScopeOrg,
		imagefactory.ScopePlatform,
	} {
		var oidArg, ownerArg *string
		switch scope {
		case imagefactory.ScopeMember:
			ownerArg = &uid
		case imagefactory.ScopeOrg:
			oidArg = orgID
			if oidArg == nil {
				continue
			}
		}
		cfg, err := h.store.GetConfigByHash(ctx, hash, scope, ownerArg, oidArg)
		if err == nil {
			c.JSON(http.StatusOK, cfg)
			return
		}
		// miss → try next scope; other errors → 500.
		if !isNotFound(err) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load config"})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "config not found"})
}

// isNotFound reports whether err is the store's not-found sentinel. Kept
// loose (string match) rather than importing the database package's
// errNotFound, to preserve the duck-typed store boundary.
func isNotFound(err error) bool {
	return err != nil && (err.Error() == "not found")
}
