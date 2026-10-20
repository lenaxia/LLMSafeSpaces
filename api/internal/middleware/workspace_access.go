// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// workspaceAccessService is the narrow, caller-shaped interface required by
// WorkspaceAccessMiddleware (ISP). The concrete workspace service satisfies it
// via ResolveWorkspace + CheckOwnership.
type workspaceAccessService interface {
	ResolveWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error)
	CheckOwnership(ctx context.Context, userID string, meta *types.WorkspaceMetadata) error
}

// WorkspaceMetaFromContext returns the metadata stored by
// WorkspaceAccessMiddleware. The ok flag is false when the middleware did not
// run (e.g. the route is mounted outside an idGroup) — callers must handle
// that case explicitly rather than relying on a non-nil meta.
//
// The canonical store is c.Request.Context() (set under
// types.ContextKeyWorkspaceMeta) so the same value is visible to
// service-layer code reading a plain context.Context. This accessor is kept
// for handler ergonomics and delegates to types.WorkspaceMetaFromCtx.
func WorkspaceMetaFromContext(c *gin.Context) (meta *types.WorkspaceMetadata, ok bool) {
	if c == nil || c.Request == nil {
		return nil, false
	}
	return types.WorkspaceMetaFromCtx(c.Request.Context())
}

// WorkspaceAccessMiddleware is the single ownership gate for /:id workspace
// routes (design 0041 D1). It resolves the workspace once, runs the
// CheckOwnership authorisation (D5 creator-membership + D6 org-admin), and on
// success stores the metadata in the request context so downstream handlers
// and service methods can reuse it without a second DB hit.
//
// Error mapping follows verifyOwner semantics exactly: NotFound → 404,
// Forbidden → 403, Internal/bare errors → 500. The middleware never rewrites
// an infrastructure failure as 403 — fail-closed here means "deny", not
// "pretend the user is unauthorized".
func WorkspaceAccessMiddleware(svc workspaceAccessService) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, _ := c.Get("userID")
		uid, _ := userID.(string)
		if uid == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		workspaceID := c.Param("id")
		if workspaceID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
			return
		}

		meta, err := svc.ResolveWorkspace(c.Request.Context(), workspaceID)
		if err != nil {
			respondWithAPIError(c, err)
			return
		}

		if err := svc.CheckOwnership(c.Request.Context(), uid, meta); err != nil {
			respondWithAPIError(c, err)
			return
		}

		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), types.ContextKeyWorkspaceMeta, meta))
		c.Next()
	}
}

// apiErrorMatcher is the minimal anonymous interface used to map APIError-like
// failures to their declared HTTP status without importing the errors package
// (keeps the middleware decoupled, mirroring server.respondWithError).
type apiErrorMatcher interface {
	StatusCode() int
	Error() string
}

// respondWithAPIError maps a service error to the gin response. APIError-shaped
// failures use their StatusCode; everything else is a 500. This intentionally
// duplicates server.respondWithError's logic so the middleware can stay
// decoupled from the server package (avoiding an import cycle).
func respondWithAPIError(c *gin.Context, err error) {
	if ae, ok := err.(apiErrorMatcher); ok {
		c.AbortWithStatusJSON(ae.StatusCode(), gin.H{"error": ae.Error()})
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
