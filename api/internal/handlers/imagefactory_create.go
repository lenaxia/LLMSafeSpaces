// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// buildDispatcher fires a GitHub Actions workflow_dispatch for an image
// factory build. The handler calls Dispatch BEFORE committing the config row
// (design/0046 #17 — dispatch-before-commit: rollback on failure, no config
// stranded at 'building' with null gh_run_id).
//
// Returns the GH Actions run ID. A fake implementation in tests scripts the
// outcome; the production implementation calls the GH Actions API.
type buildDispatcher interface {
	Dispatch(ctx context.Context, req dispatchRequest) (ghRunID int64, err error)
	// Cancel aborts a dispatched workflow run (#936): when a config save
	// fails AFTER dispatch (scoped-name conflict), the already-fired run
	// must not churn as an orphan. Best-effort — cancellation failure is
	// logged by the caller, never fatal.
	Cancel(ctx context.Context, ghRunID int64) error
}

// dispatchRequest is the input to the GH Actions workflow_dispatch. Mirrors
// the dispatch contract in design/0046 "Build dispatch contract".
type dispatchRequest struct {
	BuildID       string
	CallbackURL   string
	CallbackToken string
	Hash          string
	BaseName      string
	BaseVersion   string
	Architectures []string
	Dockerfile    string
}

// createConfigRequest is the body of POST /v1/image-factory/configs.
type createConfigRequest struct {
	Name        string   `json:"name"        binding:"required"`
	Selection   []string `json:"selection"   binding:"required"`
	BaseName    string   `json:"baseName"    binding:"required"`
	BaseVersion string   `json:"baseVersion"`
}

// CreateConfig handles POST /v1/image-factory/configs (member scope).
func (h *ImageFactoryHandler) CreateConfig(c *gin.Context) {
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	uid := userID
	h.createConfigAtScope(c, imagefactory.ScopeMember, &uid, nil)
}

// CreateOrgConfig handles POST /v1/orgs/:id/image-factory/configs (org scope).
// Behind OrgAdminGuard — the caller is verified as an admin of :id.
func (h *ImageFactoryHandler) CreateOrgConfig(c *gin.Context) {
	orgID := c.Param("id")
	if orgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "org ID is required"})
		return
	}
	oid := orgID
	h.createConfigAtScope(c, imagefactory.ScopeOrg, nil, &oid)
}

// CreatePlatformConfig handles POST /v1/admin/image-factory/configs (platform scope).
// Behind AdminGuard — the caller is a platform admin.
func (h *ImageFactoryHandler) CreatePlatformConfig(c *gin.Context) {
	h.createConfigAtScope(c, imagefactory.ScopePlatform, nil, nil)
}

// createConfigAtScope is the shared create logic for all three scopes.
// The scope determines which owner/org IDs are set on the Config struct;
// all validation, coalescing, known-failure checking, and dispatch logic
// is identical regardless of scope — the build produces the same image.
//
// Cross-scope coalescing (design/0047 Q2): GetInFlightOrSuccessfulBuild is
// scope-agnostic, so an org/platform config will coalesce onto a build
// initiated by any user/org for the same hash. This is intentional —
// images are platform-wide artifacts.
func (h *ImageFactoryHandler) createConfigAtScope(
	c *gin.Context,
	scope imagefactory.ConfigScope,
	ownerID, orgID *string,
) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image builds are not configured"})
		return
	}
	ctx := c.Request.Context()

	var req createConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "name is required"})
		return
	}

	// Resolve base + version.
	baseName := req.BaseName
	baseVersion := req.BaseVersion
	if baseVersion == "" {
		bases, err := h.store.ListBases(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve base"})
			return
		}
		found := false
		for _, b := range bases {
			if b.Name == baseName && b.IsDefault {
				baseVersion = b.Version
				found = true
				break
			}
		}
		if !found {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no default version for base " + baseName})
			return
		}
	}
	base, err := h.store.GetBase(ctx, baseName, baseVersion)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "unknown base/version"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load base"})
		}
		return
	}

	// Load extensions and resolve the selection.
	exts, err := h.store.ListExtensions(ctx, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load extensions"})
		return
	}
	extMap := make(map[string]imagefactory.Extension, len(exts))
	for _, e := range exts {
		extMap[e.ID] = e
	}
	sel := imagefactory.Selection(req.Selection)
	if err := imagefactory.ValidateSelection(sel); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	resolved, err := imagefactory.ResolveSelection(sel, extMap, baseName)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	if err := imagefactory.ValidateResolved(resolved); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}

	// Hash the selection + base name.
	hash, err := imagefactory.HashSelection(sel, baseName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to compute hash"})
		return
	}

	// Check known_failures: permanently blocked → 422.
	if kf, err := h.store.GetKnownFailure(ctx, hash, baseName); err == nil && !kf.Retriable {
		expl := kf.Explanation
		if expl == "" {
			expl = "this combination is known not to build"
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": expl})
		return
	} else if err != nil && !errors.Is(err, database.ErrNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check known failures"})
		return
	}

	// Coalescing: check for existing in-flight or successful build (scope-agnostic).
	existing, err := h.store.GetInFlightOrSuccessfulBuild(ctx, hash, baseVersion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing builds"})
		return
	}
	if existing != nil {
		status := imagefactory.StatusBuilding
		if existing.Status == imagefactory.BuildSucceeded {
			status = imagefactory.StatusReady
		}
		cfg := imagefactory.Config{
			Hash:           hash,
			Name:           req.Name,
			Selection:      sel,
			ResolvedValues: resolved,
			BaseName:       baseName,
			BaseVersion:    baseVersion,
			Scope:          scope,
			OwnerID:        ownerID,
			OrgID:          orgID,
			Status:         status,
		}
		if err := h.store.CreateConfig(ctx, &cfg); err != nil {
			if errors.Is(err, database.ErrConflict) {
				// #936: scoped-name collision — 409 with the shape named.
				c.JSON(http.StatusConflict, gin.H{
					"error": fmt.Sprintf("a config named %q already exists in this scope — pick another name", cfg.Name),
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
			return
		}
		c.JSON(http.StatusCreated, cfg)
		return
	}

	// No existing build → dispatch before commit (design #17).
	pc, err := h.store.GetPlatformConfig(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load platform config"})
		return
	}
	callbackToken, err := generateCallbackToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate callback token"})
		return
	}

	buildID := newUUID()
	dockerfile, err := imagefactory.RenderDockerfile(resolved, base)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to render Dockerfile"})
		return
	}
	ghRunID, err := h.dispatcher.Dispatch(ctx, dispatchRequest{
		BuildID:       buildID,
		CallbackURL:   h.callbackURL,
		CallbackToken: callbackToken,
		Hash:          hash,
		BaseName:      baseName,
		BaseVersion:   baseVersion,
		Architectures: pc.Architectures,
		Dockerfile:    dockerfile,
	})
	if err != nil {
		if h.logger != nil {
			h.logger.Error("image-factory: build dispatch failed", err,
				"hash", hash, "baseName", baseName, "baseVersion", baseVersion)
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to dispatch build"})
		return
	}

	// Dispatch succeeded — commit both rows atomically.
	cfg := imagefactory.Config{
		Hash:           hash,
		Name:           req.Name,
		Selection:      sel,
		ResolvedValues: resolved,
		BaseName:       baseName,
		BaseVersion:    baseVersion,
		Scope:          scope,
		OwnerID:        ownerID,
		OrgID:          orgID,
		Status:         imagefactory.StatusBuilding,
	}
	build := imagefactory.Build{
		ID:             buildID,
		Hash:           hash,
		BaseName:       baseName,
		BaseVersion:    baseVersion,
		ResolvedValues: resolved,
		Architectures:  pc.Architectures,
		Status:         imagefactory.BuildDispatched,
		GHRunID:        &ghRunID,
		CallbackToken:  callbackToken,
		TriggeredBy:    ownerID,
		Scope:          scope,
		OrgID:          orgID,
	}
	if err := h.store.CreateConfigAndBuild(ctx, &cfg, &build); err != nil {
		// #936: the dispatch already fired (dispatch-before-commit, by
		// design) — a colliding name must cancel the run so the conflict
		// doesn't leave an orphaned GH workflow churning, then 409.
		if errors.Is(err, database.ErrConflict) {
			// Dispatch returns the GH run ID only when the dispatch API
			// provides one; the workflow_dispatch endpoint does NOT (the
			// ID arrives via the callback), so this is 0 today and Cancel
			// no-ops with a log. The orphan is bounded: the run finishes,
			// its callback 404s (no build row), nothing retries.
			if build.GHRunID != nil {
				if cerr := h.dispatcher.Cancel(ctx, *build.GHRunID); cerr != nil {
					h.logger.Warn("image-factory: conflict cleanup — cancel dispatched run failed", "error", cerr.Error())
				}
			}
			c.JSON(http.StatusConflict, gin.H{
				"error": fmt.Sprintf("a config named %q already exists in this scope — pick another name", cfg.Name),
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config and build"})
		return
	}

	c.JSON(http.StatusCreated, cfg)
}

// generateCallbackToken produces a 32-byte hex token for the build callback
// (design/0046 #18). ConstantTimeCompare on the callback endpoint.
func generateCallbackToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newUUID generates a UUID v4 string. Kept local to avoid importing google/uuid
// just for one call site; the handler doesn't need UUID validation.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
