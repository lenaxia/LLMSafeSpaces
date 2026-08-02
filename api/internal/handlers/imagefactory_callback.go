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
	MarkBuildSucceeded(ctx context.Context, id, imageRef, digest string) error
	MarkBuildFailed(ctx context.Context, id, failureReason, explanation string) error
	SetConfigStatus(ctx context.Context, id string, status imagefactory.ConfigStatus) error
	RecordKnownFailure(ctx context.Context, kf imagefactory.KnownFailure) error
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid callback body: " + err.Error()})
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
	if err := h.buildStore.MarkBuildSucceeded(ctx, build.ID, imageRef, req.Digest); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark build succeeded"})
		return
	}
	if err := h.buildStore.SetConfigStatus(ctx, build.ConfigID, imagefactory.StatusReady); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update config status"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ImageFactoryHandler) handleCallbackFailed(c *gin.Context, ctx context.Context, build *imagefactory.Build, req *callbackRequest) {
	// Mark the build failed with the raw log tail.
	if err := h.buildStore.MarkBuildFailed(ctx, build.ID, req.FailureReason, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mark build failed"})
		return
	}

	// Invoke the failure seam (S6: LLM explanation). For now, use a
	// placeholder explanation so the failure path is functional end-to-end.
	explanation := h.explainFailure(ctx, req.FailureReason, build.ResolvedValues)

	// Record the known failure so future saves of this combo are blocked.
	hash, _ := imagefactory.HashSelection(build.ResolvedValues.Selection(), build.BaseName)
	if hash != "" {
		_ = h.buildStore.RecordKnownFailure(ctx, imagefactory.KnownFailure{
			SelectionHash: hash,
			Selection:     build.ResolvedValues.Selection(),
			BaseName:      build.BaseName,
			Explanation:   explanation,
			FailureReason: req.FailureReason,
			Retriable:     true,
		})
	}

	// Flip the config to rejected.
	if err := h.buildStore.SetConfigStatus(ctx, build.ConfigID, imagefactory.StatusRejected); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update config status"})
		return
	}
	c.Status(http.StatusNoContent)
}

// explainFailure is the failure-seam placeholder (S6 wires the real LLM).
// Returns a human-readable explanation of the failure. Degradation mode:
// if the LLM is unavailable, returns a fallback string.
func (h *ImageFactoryHandler) explainFailure(_ context.Context, logTail string, _ imagefactory.ResolvedValues) string {
	if h.explainer != nil {
		explanation, _, err := h.explainer.Explain(context.Background(), logTail, nil)
		if err == nil && explanation != "" {
			return explanation
		}
	}
	return "this combination failed to build; contact your administrator for details"
}

// failureExplainer is the LLM seam interface (S6 provides the real impl).
type failureExplainer interface {
	Explain(ctx context.Context, logTail string, rv imagefactory.ResolvedValues) (explanation string, attributedExtensionID string, err error)
}
