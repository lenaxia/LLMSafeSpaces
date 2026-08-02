// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// buildStore is the data-access surface for build lifecycle operations
// (callback transitions). Satisfied by *database.Service. Separate from
// imageFactoryStore (catalog/config reads) via ISP — the callback handler
// needs a different method subset.
type buildStore interface {
	GetBuild(ctx context.Context, id string) (imagefactory.Build, error)
	TransitionBuildSucceeded(ctx context.Context, buildID, configID, imageRef, digest string) error
	TransitionBuildFailed(ctx context.Context, buildID, configID string, kf imagefactory.KnownFailure) error
}

// extensionReviewer flags an extension for admin review when the LLM
// attributes a build failure to it (design/0046 attribution). Optional —
// when nil, attribution is recorded but no flag is set.
type extensionReviewer interface {
	SetExtensionReviewRequested(ctx context.Context, id string, v bool) error
}

// callbackRequest is the POST body the GH Actions workflow sends on build
// completion (design/0046 "Build dispatch contract").
type callbackRequest struct {
	Status        string `json:"status"         binding:"required"` // "succeeded" | "failed"
	Digest        string `json:"digest,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
}

// Callback handles POST /internal/image-factory/builds/:id/callback.
//
// This is the authenticated path by which an external GH Actions runner
// reports build results back to the API (design/0046 #18). The per-build
// callback_token (generated at dispatch time, stored on the build row) is
// compared with subtle.ConstantTimeCompare against the Authorization
// Bearer header — the only path an external runner can mutate build state.
//
// On success: build → succeeded, config → ready, image_ref/digest set.
// On failure: build → failed, config → rejected; the failure-seam is
// invoked (LLM explanation — S6; for now a placeholder).
//
// The callback is idempotent: a second POST for an already-terminal build
// returns 204 without re-transitioning.
func (h *ImageFactoryHandler) Callback(c *gin.Context) {
	buildID := c.Param("id")
	if buildID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "build id is required"})
		return
	}
	ctx := c.Request.Context()

	// Load the build row. We need the stored callback_token for comparison.
	build, err := h.buildStore.GetBuild(ctx, buildID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load build"})
		}
		return
	}

	// Idempotent: if the build is already terminal, return 204 without
	// re-transitioning. Prevents double-write of known_failures on a
	// replayed or concurrent callback.
	if build.Status == imagefactory.BuildSucceeded || build.Status == imagefactory.BuildFailed {
		c.Status(http.StatusNoContent)
		return
	}

	// Authenticate: compare the presented token against the stored one.
	// subtle.ConstantTimeCompare prevents timing side-channels.
	presented := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if presented == "" || build.CallbackToken == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "missing callback token"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(build.CallbackToken)) != 1 {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid callback token"})
		return
	}

	// Bind the result payload.
	var req callbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid callback body"})
		return
	}

	switch req.Status {
	case "succeeded":
		h.handleCallbackSucceeded(c, ctx, &build, &req)
	case "failed":
		h.handleCallbackFailed(c, ctx, &build, &req)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'succeeded' or 'failed'"})
	}
}

func (h *ImageFactoryHandler) handleCallbackSucceeded(c *gin.Context, ctx context.Context, build *imagefactory.Build, req *callbackRequest) {
	imageRef := h.imageRepo + ":" + build.Hash + "-" + build.BaseVersion
	if err := h.buildStore.TransitionBuildSucceeded(ctx, build.ID, build.ConfigID, imageRef, req.Digest); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to transition build to succeeded"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ImageFactoryHandler) handleCallbackFailed(c *gin.Context, ctx context.Context, build *imagefactory.Build, req *callbackRequest) {
	explanation, attributedExtension := h.explainFailure(ctx, req.FailureReason, build.ResolvedValues)
	kf := imagefactory.KnownFailure{
		SelectionHash: build.Hash,
		Selection:     build.ResolvedValues.Selection(),
		BaseName:      build.BaseName,
		Explanation:   explanation,
		FailureReason: req.FailureReason,
		Retriable:     true,
	}
	if err := h.buildStore.TransitionBuildFailed(ctx, build.ID, build.ConfigID, kf); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to transition build to failed"})
		return
	}
	// If the LLM attributed the failure to a single extension, flag it for
	// admin review (design/0046 attribution). The admin can then investigate
	// and retire the extension if needed.
	if attributedExtension != "" && h.adminStore != nil {
		_ = h.adminStore.SetExtensionReviewRequested(ctx, attributedExtension, true)
	}
	c.Status(http.StatusNoContent)
}

// explainFailure calls the LLM (if configured) for a plain-language
// explanation + optional extension attribution. Degradation: if the LLM
// is unavailable, returns a fallback string with no attribution.
func (h *ImageFactoryHandler) explainFailure(ctx context.Context, logTail string, rv imagefactory.ResolvedValues) (string, string) {
	if h.explainer != nil {
		explanation, attributedExtension, err := h.explainer.Explain(ctx, logTail, rv)
		if err == nil && explanation != "" {
			return explanation, attributedExtension
		}
	}
	return fallbackExplanation, ""
}

// failureExplainer is the LLM seam interface (S6 provides the real impl).
type failureExplainer interface {
	Explain(ctx context.Context, logTail string, rv imagefactory.ResolvedValues) (explanation string, attributedExtensionID string, err error)
}
