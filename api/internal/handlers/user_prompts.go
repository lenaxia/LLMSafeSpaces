// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1499: user-level saved prompts. Owner-scoped CRUD — userID comes
// from the auth context and never from a body; validation is
// name: trimmed 1–100 runes, no control characters; content: 1–64KiB
// (a prompt larger than that is a file — attachment-manifest scale
// argued the ceiling). Typed store errors map to 404/409.
//
// Named distinctly from prompts.go (the platform/org/workspace
// prompt-POLICY surface — a different concept that owns the bare
// "prompt" noun in this package).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	promptNameMaxRunes  = 100
	promptContentMaxLen = 64 * 1024
)

// UserPromptStore is the caller-shaped store surface these handlers
// need (satisfied by the database service).
type UserPromptStore interface {
	ListUserPrompts(ctx context.Context, userID string) ([]types.UserPrompt, error)
	CreateUserPrompt(ctx context.Context, userID, name, content string) (*types.UserPrompt, error)
	UpdateUserPrompt(ctx context.Context, userID, id string, name, content *string) (*types.UserPrompt, error)
	DeleteUserPrompt(ctx context.Context, userID, id string) error
}

type UserPromptsHandler struct {
	store UserPromptStore
}

func NewUserPromptsHandler(store UserPromptStore) *UserPromptsHandler {
	return &UserPromptsHandler{store: store}
}

func (h *UserPromptsHandler) List(c *gin.Context) {
	userID := c.GetString("userID")
	prompts, err := h.store.ListUserPrompts(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list prompts"})
		return
	}
	c.JSON(http.StatusOK, types.UserPromptListResponse{Prompts: prompts})
}

func (h *UserPromptsHandler) Create(c *gin.Context) {
	userID := c.GetString("userID")
	var req types.CreatePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name, ok := validateUserPromptName(req.Name)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": name})
		return
	}
	if !validUserPromptContent(req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content must be 1-65536 bytes"})
		return
	}
	// The TRIMMED name is what gets stored (the issue's contract):
	// persisting the raw value would break the 100-rune ceiling and
	// let " foo" and "foo" coexist under UNIQUE(user_id, name).
	p, err := h.store.CreateUserPrompt(c.Request.Context(), userID, name, req.Content)
	if errors.Is(err, database.ErrPromptNameTaken) {
		c.JSON(http.StatusConflict, gin.H{"error": "prompt_name_taken"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create prompt"})
		return
	}
	c.JSON(http.StatusCreated, types.UserPromptResponse{Prompt: *p})
}

func (h *UserPromptsHandler) Update(c *gin.Context) {
	userID := c.GetString("userID")
	id := c.Param("id")
	var req types.UpdatePromptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	var trimmedName *string
	if req.Name != nil {
		name, ok := validateUserPromptName(*req.Name)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": name})
			return
		}
		trimmedName = &name
	}
	if req.Content != nil && !validUserPromptContent(*req.Content) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content must be 1-65536 bytes"})
		return
	}
	if req.Name == nil && req.Content == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}
	p, err := h.store.UpdateUserPrompt(c.Request.Context(), userID, id, trimmedName, req.Content)
	if errors.Is(err, database.ErrPromptNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "prompt not found"})
		return
	}
	if errors.Is(err, database.ErrPromptNameTaken) {
		c.JSON(http.StatusConflict, gin.H{"error": "prompt_name_taken"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update prompt"})
		return
	}
	c.JSON(http.StatusOK, types.UserPromptResponse{Prompt: *p})
}

func (h *UserPromptsHandler) Delete(c *gin.Context) {
	userID := c.GetString("userID")
	err := h.store.DeleteUserPrompt(c.Request.Context(), userID, c.Param("id"))
	if errors.Is(err, database.ErrPromptNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "prompt not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete prompt"})
		return
	}
	c.Status(http.StatusNoContent)
}

// validateUserPromptName trims and checks the display-label rules:
// 1–100 runes, no control characters (display safety — labels cross
// the UI). On success it returns the TRIMMED name — that value is what
// callers must persist (the ceiling and the UNIQUE(user_id, name) slot
// both key on it); on failure it returns the error message.
func validateUserPromptName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "name must not be empty", false
	}
	if utf8.RuneCountInString(name) > promptNameMaxRunes {
		return "name must be at most 100 characters", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "name must not contain control characters", false
		}
	}
	return name, true
}

func validUserPromptContent(content string) bool {
	return content != "" && len(content) <= promptContentMaxLen
}
