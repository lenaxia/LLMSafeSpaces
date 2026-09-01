// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// maxLoginDiscoveryBodySize limits the request body on the public lookup
// endpoint (1 MiB), matching password_reset.go's maxPasswordResetBodySize.
// Prevents memory-exhaustion DoS on an unauthenticated endpoint.
const maxLoginDiscoveryBodySize = 1 << 20 // 1 MiB

// loginDiscoveryUserLookup resolves a user by email. Subset of the
// interfaces.UserLookup interface — only the method this handler needs.
type loginDiscoveryUserLookup interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
}

// loginDiscoveryOrgLookup resolves a user's org membership and the org record.
// Subset of the OrgStore interface — only the methods this handler needs.
type loginDiscoveryOrgLookup interface {
	GetUserOrgID(ctx context.Context, userID string) (string, error)
	GetOrg(ctx context.Context, orgID string) (*types.Organization, error)
}

// loginDiscoveryLogger captures errors without surfacing them to the caller
// (the enumeration-safe contract: every error branch returns the same 200 +
// not-found redirect). Matches the passwordResetLogger shape.
type loginDiscoveryLogger interface {
	Warn(msg string, args ...any)
	Error(msg string, err error, args ...any)
}

// LoginDiscoveryHandler implements POST /api/v1/auth/lookup — the email-led
// login discovery endpoint (Epic 54, US-54.1).
//
// The endpoint resolves an email to a single redirect URL pointing at the
// user's org (subdomain if routing is enabled, direct SSO start URL otherwise).
// The response shape is uniform across all non-validation branches:
//
//	200 OK
//	{ "redirectUrl": "<url>" }
//
// Found users with an org get a real redirect; everyone else (not found, no
// org, suspended, DB error) gets the same 200 with a not-found redirect URL.
// This matches the password_reset.go:119 enumeration-safe precedent exactly:
// uniform status, uniform body shape, no timing pad.
type LoginDiscoveryHandler struct {
	users      loginDiscoveryUserLookup
	orgs       loginDiscoveryOrgLookup
	baseDomain string // auth.orgSubdomainRouting.baseDomain; "" = fallback to direct SSO URL
	log        loginDiscoveryLogger
}

// NewLoginDiscoveryHandler constructs the handler. baseDomain is the subdomain
// base (e.g. "app.example.com"); when empty, the handler falls back to the
// direct SSO start URL (/api/v1/auth/sso/<slug>/start) which works regardless
// of chart config.
func NewLoginDiscoveryHandler(
	users loginDiscoveryUserLookup,
	orgs loginDiscoveryOrgLookup,
	baseDomain string,
	log loginDiscoveryLogger,
) *LoginDiscoveryHandler {
	return &LoginDiscoveryHandler{
		users:      users,
		orgs:       orgs,
		baseDomain: strings.TrimPrefix(baseDomain, "."), // normalize ".app.example.com" -> "app.example.com"
		log:        log,
	}
}

// notFoundRedirectURL is the URL returned for every non-found branch.
// It uses a relative path so it works regardless of where the API is hosted.
const notFoundRedirectPath = "/?lookup=not_found"

// Lookup handles POST /api/v1/auth/lookup.
//
// Enumeration-safe: always returns 200 with { redirectUrl } on valid input.
// DB errors are logged and masked — never surfaced as 5xx.
func (h *LoginDiscoveryHandler) Lookup(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxLoginDiscoveryBodySize)
	var req struct {
		Email string `json:"email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid email is required"})
		return
	}

	ctx := c.Request.Context()
	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))

	user, err := h.users.GetUserByEmail(ctx, emailAddr)
	if err != nil {
		// DB error — mask. Log + return not-found redirect (never 500).
		if h.log != nil {
			h.log.Error("login-discovery: user lookup error", err, "email", emailAddr)
		}
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}
	if user == nil {
		// User does not exist.
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}

	orgID, err := h.orgs.GetUserOrgID(ctx, user.ID)
	if err != nil {
		if h.log != nil {
			h.log.Error("login-discovery: org lookup error", err, "user_id", user.ID)
		}
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}
	if orgID == "" {
		// User exists but has no org membership (personal-only user).
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}

	org, err := h.orgs.GetOrg(ctx, orgID)
	if err != nil {
		if h.log != nil {
			h.log.Error("login-discovery: org fetch error", err, "org_id", orgID)
		}
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}
	if org == nil || org.Slug == "" {
		// Org was soft-deleted or has no slug — cannot construct a redirect.
		c.JSON(http.StatusOK, gin.H{"redirectUrl": notFoundRedirectPath})
		return
	}

	c.JSON(http.StatusOK, gin.H{"redirectUrl": subdomainFor(org.Slug, h.baseDomain)})
}

// subdomainFor constructs the redirect URL for a found user's org.
//
// When base is non-empty (subdomain routing enabled), returns the org's
// subdomain URL: https://<slug>.<base>.
//
// When base is empty (subdomain routing disabled), falls back to the direct
// SSO start URL (/api/v1/auth/sso/<slug>/start) — which works today regardless
// of chart config, so the lookup endpoint is fully functional even before an
// operator enables subdomain routing. A found user is never told "not found".
func subdomainFor(slug, base string) string {
	if base = strings.TrimPrefix(base, "."); base == "" {
		return fmt.Sprintf("/api/v1/auth/sso/%s/start", slug)
	}
	return fmt.Sprintf("https://%s.%s", slug, base)
}
