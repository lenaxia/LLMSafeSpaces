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
}

// dispatchRequest is the input to the GH Actions workflow_dispatch. Mirrors
// the dispatch contract in design/0046 "Build dispatch contract".
type dispatchRequest struct {
	BuildID       string
	Hash          string
	BaseName      string
	BaseVersion   string
	BaseImageRef  string
	Architectures []string
}

// createConfigRequest is the body of POST /v1/image-factory/configs.
type createConfigRequest struct {
	Name        string   `json:"name"        binding:"required"`
	Selection   []string `json:"selection"   binding:"required"`
	BaseName    string   `json:"baseName"    binding:"required"`
	BaseVersion string   `json:"baseVersion"`
}

// CreateConfig handles POST /v1/image-factory/configs.
//
// The load-bearing handler (design/0046 #12, #15, #16, #17). Exact sequence:
//
//  1. Bind + validate body.
//  2. Resolve base (default version when BaseVersion is empty).
//  3. Load extensions; ResolveSelection → ResolvedValues; ValidateResolved.
//  4. HashSelection → hash.
//  5. Check known_failures for (hash, baseName) with retriable=false → 422.
//  6. GetInFlightOrSuccessfulBuild(hash, baseVersion):
//     a. Succeeded exists → create config at status=ready, return 201. No dispatch.
//     b. In-flight exists → create config at status=building, return 201. No dispatch.
//  7. No existing build → generate callback_token; Dispatch to GH Actions.
//     On error → return 503, do NOT commit config row.
//  8. On dispatch success → CreateBuild + CreateConfig in one tx; return 201.
func (h *ImageFactoryHandler) CreateConfig(c *gin.Context) {
	if h.dispatcher == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "image builds are not configured"})
		return
	}
	userID := c.GetString("userID")
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
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
		// Default version: find the base row marked is_default for this name.
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

	// Coalescing: check for existing in-flight or successful build.
	existing, err := h.store.GetInFlightOrSuccessfulBuild(ctx, hash, baseVersion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check existing builds"})
		return
	}
	if existing != nil {
		// Link, don't dispatch.
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
			Scope:          imagefactory.ScopeMember,
			OwnerID:        &userID,
			Status:         status,
		}
		if err := h.store.CreateConfig(ctx, cfg); err != nil {
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

	// We need a build ID to pass to dispatch. Generate a UUID first, then
	// use it as the build row's ID.
	buildID := newUUID()
	ghRunID, err := h.dispatcher.Dispatch(ctx, dispatchRequest{
		BuildID:       buildID,
		Hash:          hash,
		BaseName:      baseName,
		BaseVersion:   baseVersion,
		BaseImageRef:  base.Ref(),
		Architectures: pc.Architectures,
	})
	if err != nil {
		// Dispatch failed — do NOT commit. No orphaned config row.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to dispatch build: " + err.Error()})
		return
	}

	// Dispatch succeeded — commit both rows.
	ghRun := ghRunID
	cfg := imagefactory.Config{
		Hash:           hash,
		Name:           req.Name,
		Selection:      sel,
		ResolvedValues: resolved,
		BaseName:       baseName,
		BaseVersion:    baseVersion,
		Scope:          imagefactory.ScopeMember,
		OwnerID:        &userID,
		Status:         imagefactory.StatusBuilding,
	}
	if err := h.store.CreateConfig(ctx, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
		return
	}

	build := imagefactory.Build{
		ID:             buildID,
		ConfigID:       cfg.ID,
		Hash:           hash,
		BaseName:       baseName,
		BaseVersion:    baseVersion,
		ResolvedValues: resolved,
		Architectures:  pc.Architectures,
		Status:         imagefactory.BuildDispatched,
		GHRunID:        &ghRun,
		CallbackToken:  callbackToken,
		TriggeredBy:    &userID,
	}
	if err := h.store.CreateBuild(ctx, build); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save build record"})
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
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
