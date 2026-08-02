// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// imageFactoryAdminStore is the data-access surface for admin CRUD over the image-
// factory catalog. Satisfied by *database.Service via duck-typing.
// Separate from imageFactoryStore (ISP) — admin writes are a different
// concern from consumer reads.
type imageFactoryAdminStore interface {
	GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error)
	SetPlatformConfig(ctx context.Context, pc imagefactory.PlatformConfig) error

	ListBases(ctx context.Context) ([]imagefactory.Base, error)
	UpsertBase(ctx context.Context, b imagefactory.Base) error
	DeleteBase(ctx context.Context, name, version string) error

	ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error)
	PublishExtension(ctx context.Context, e imagefactory.Extension) error
	RetireExtension(ctx context.Context, id string) error

	ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error)
	SetKnownFailureRetriable(ctx context.Context, selectionHash, baseName string, retriable bool) error
	DeleteKnownFailure(ctx context.Context, selectionHash, baseName string) error
}

// ImageFactoryAdminHandler serves the platform-owner admin endpoints for
// the image factory (design/0046 admin portal). All endpoints are behind
// AdminGuard (platform owner only).
type ImageFactoryAdminHandler struct {
	store imageFactoryAdminStore
}

// NewImageFactoryAdminHandler constructs the handler.
func NewImageFactoryAdminHandler(store imageFactoryAdminStore) *ImageFactoryAdminHandler {
	return &ImageFactoryAdminHandler{store: store}
}

// ── Platform config ─────────────────────────────────────────────────────

func (h *ImageFactoryAdminHandler) GetPlatformConfig(c *gin.Context) {
	pc, err := h.store.GetPlatformConfig(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load platform config"})
		return
	}
	c.JSON(http.StatusOK, pc)
}

type setPlatformConfigRequest struct {
	Architectures []string `json:"architectures" binding:"required"`
}

func (h *ImageFactoryAdminHandler) SetPlatformConfig(c *gin.Context) {
	var req setPlatformConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.store.SetPlatformConfig(c.Request.Context(), imagefactory.PlatformConfig{Architectures: req.Architectures}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save platform config"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── Bases ───────────────────────────────────────────────────────────────

func (h *ImageFactoryAdminHandler) ListBases(c *gin.Context) {
	bases, err := h.store.ListBases(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bases"})
		return
	}
	if bases == nil {
		bases = []imagefactory.Base{}
	}
	c.JSON(http.StatusOK, gin.H{"bases": bases})
}

type upsertBaseRequest struct {
	Name      string `json:"name" binding:"required"`
	Version   string `json:"version" binding:"required"`
	Image     string `json:"image" binding:"required"`
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	IsDefault bool   `json:"isDefault"`
}

func (h *ImageFactoryAdminHandler) UpsertBase(c *gin.Context) {
	var req upsertBaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.store.UpsertBase(c.Request.Context(), imagefactory.Base{
		Name: req.Name, Version: req.Version, Image: req.Image,
		Tag: req.Tag, Digest: req.Digest, IsDefault: req.IsDefault,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save base"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

func (h *ImageFactoryAdminHandler) DeleteBase(c *gin.Context) {
	name := c.Param("name")
	version := c.Param("version")
	if err := h.store.DeleteBase(c.Request.Context(), name, version); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "base not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete base"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── Extensions ──────────────────────────────────────────────────────────

func (h *ImageFactoryAdminHandler) ListExtensions(c *gin.Context) {
	exts, err := h.store.ListExtensions(c.Request.Context(), true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load extensions"})
		return
	}
	if exts == nil {
		exts = []imagefactory.Extension{}
	}
	c.JSON(http.StatusOK, gin.H{"extensions": exts})
}

type publishExtensionRequest struct {
	ID             string                     `json:"id" binding:"required"`
	Type           imagefactory.ExtensionType `json:"type" binding:"required"`
	Value          string                     `json:"value" binding:"required"`
	FileSpec       *imagefactory.FileSpec     `json:"fileSpec"`
	SupportedBases []string                   `json:"supportedBases" binding:"required"`
	Description    string                     `json:"description"`
}

func (h *ImageFactoryAdminHandler) PublishExtension(c *gin.Context) {
	var req publishExtensionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	// Validate type is one of the allowed kinds (design/0046: no run/env).
	switch req.Type {
	case imagefactory.ExtensionTypeApt, imagefactory.ExtensionTypeMise, imagefactory.ExtensionTypeFile:
	default:
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "type must be apt, mise, or file"})
		return
	}
	// File extensions must have a valid FileSpec (absolute path, valid mode).
	if req.Type == imagefactory.ExtensionTypeFile {
		if req.FileSpec == nil || req.FileSpec.Path == "" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "file extensions require fileSpec with a path"})
			return
		}
		// Reuse the pure-logic validator from the imagefactory package.
		dummy := imagefactory.ResolvedValues{req.ID: {
			Type: req.Type, Value: req.Value, FileSpec: req.FileSpec,
		}}
		if err := imagefactory.ValidateResolved(dummy); err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
	}
	if err := h.store.PublishExtension(c.Request.Context(), imagefactory.Extension{
		ID: req.ID, Type: req.Type, Value: req.Value, FileSpec: req.FileSpec,
		SupportedBases: req.SupportedBases, Description: req.Description,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to publish extension"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

func (h *ImageFactoryAdminHandler) RetireExtension(c *gin.Context) {
	id := c.Param("id")
	if err := h.store.RetireExtension(c.Request.Context(), id); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "extension not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retire extension"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── Known failures ──────────────────────────────────────────────────────

func (h *ImageFactoryAdminHandler) ListKnownFailures(c *gin.Context) {
	failures, err := h.store.ListKnownFailures(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load known failures"})
		return
	}
	if failures == nil {
		failures = []imagefactory.KnownFailure{}
	}
	c.JSON(http.StatusOK, gin.H{"knownFailures": failures})
}

type setRetriableRequest struct {
	Retriable bool `json:"retriable"`
}

func (h *ImageFactoryAdminHandler) SetKnownFailureRetriable(c *gin.Context) {
	hash := c.Param("hash")
	baseName := c.Param("baseName")
	var req setRetriableRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := h.store.SetKnownFailureRetriable(c.Request.Context(), hash, baseName, req.Retriable); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "known failure not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update known failure"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *ImageFactoryAdminHandler) DeleteKnownFailure(c *gin.Context) {
	hash := c.Param("hash")
	baseName := c.Param("baseName")
	if err := h.store.DeleteKnownFailure(c.Request.Context(), hash, baseName); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "known failure not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete known failure"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
