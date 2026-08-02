// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sso"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ssoStore is the org-data subset used by the SSO CRUD + discovery endpoints.
type ssoStore interface {
	GetSSOConfig(ctx context.Context, orgID string) (*types.OrgSSOConfig, error)
	DeleteSSOConfig(ctx context.Context, orgID string) error
	ListSSODomains(ctx context.Context) ([]types.SSODomain, error)
	CountSSOConfigs(ctx context.Context) (int, error)
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
}

type ssoLogger interface {
	Warn(msg string, args ...any)
}

// SSOHandler exposes both the org-admin SSO config CRUD and the public OIDC
// login flow (start/callback) and domain discovery. The OIDC mechanics live in
// the sso.Service; this handler owns HTTP concerns (cookies, redirects,
// response shaping).
type SSOHandler struct {
	svc           *sso.Service
	store         ssoStore
	authSvc       orgAuthService
	sessionCookie string
	cookieDomain  string // Epic 54: set as Domain= on the session cookie when wildcard subdomain routing is enabled
	frontendURL   string
	logger        ssoLogger
}

// NewSSOHandler constructs the handler. sessionCookie is the JWT cookie name
// used elsewhere in the app (e.g. "lsp_session"); frontendURL is the post-SSO
// browser landing URL (may be empty — the handler falls back to "/").
// cookieDomain is the Domain attribute for the session cookie (empty = host-only;
// set when wildcard subdomain routing is enabled so the session survives the
// IdP→callback→subdomain redirect chain).
func NewSSOHandler(svc *sso.Service, store ssoStore, authSvc orgAuthService, sessionCookie, cookieDomain, frontendURL string, logger ssoLogger) *SSOHandler {
	return &SSOHandler{
		svc:           svc,
		store:         store,
		authSvc:       authSvc,
		sessionCookie: sessionCookie,
		cookieDomain:  cookieDomain,
		frontendURL:   frontendURL,
		logger:        logger,
	}
}

// toResponse projects the at-rest config into the API shape, omitting the
// encrypted client secret (HasSecret replaces it).
func toSSOResponse(cfg *types.OrgSSOConfig) types.OrgSSOConfigResponse {
	resp := types.OrgSSOConfigResponse{
		OrgID:             cfg.OrgID,
		DiscoveryURL:      cfg.DiscoveryURL,
		ClientID:          cfg.ClientID,
		HasSecret:         len(cfg.ClientSecret) > 0,
		ClaimedDomains:    cfg.ClaimedDomains,
		VerifiedDomains:   cfg.VerifiedDomains,
		VerificationToken: cfg.VerificationToken,
		AutoProvision:     cfg.AutoProvision,
		GroupRoleMapping:  cfg.GroupRoleMapping,
		UpdatedAt:         cfg.UpdatedAt,
	}
	if resp.ClaimedDomains == nil {
		resp.ClaimedDomains = []string{}
	}
	if resp.VerifiedDomains == nil {
		resp.VerifiedDomains = []string{}
	}
	if resp.GroupRoleMapping == nil {
		resp.GroupRoleMapping = map[string]types.OrgRole{}
	}
	return resp
}

// Get handles GET /api/v1/orgs/:id/sso (org admin).
func (h *SSOHandler) Get(c *gin.Context) {
	orgID := c.Param("id")
	cfg, err := h.store.GetSSOConfig(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load sso config"})
		return
	}
	if cfg == nil {
		c.JSON(http.StatusOK, types.OrgSSOConfigResponse{
			OrgID:            orgID,
			ClaimedDomains:   []string{},
			VerifiedDomains:  []string{},
			GroupRoleMapping: map[string]types.OrgRole{},
			AutoProvision:    true,
		})
		return
	}
	c.JSON(http.StatusOK, toSSOResponse(cfg))
}

// Put handles PUT /api/v1/orgs/:id/sso (org admin). Upserts the SSO config.
func (h *SSOHandler) Put(c *gin.Context) {
	orgID := c.Param("id")
	actorID := h.authSvc.GetUserID(c)

	var req types.UpsertSSOConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Existing config provides the current encrypted secret for the
	// "empty clientSecret = leave unchanged" partial-update path, and the
	// current verified_domains for the intersection computation (D17 Q-S2).
	existing, err := h.store.GetSSOConfig(c.Request.Context(), orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load existing sso config"})
		return
	}
	var existingSecret []byte
	var existingVerified []string
	if existing != nil {
		existingSecret = existing.ClientSecret
		existingVerified = existing.VerifiedDomains
	}

	if _, err := h.svc.ApplyConfigMutation(c.Request.Context(), orgID, req, existingSecret, existingVerified); err != nil {
		respondSSOMutationError(c, err)
		return
	}

	// Reload the stored row so the response reflects DB timestamps.
	cfg, err := h.store.GetSSOConfig(c.Request.Context(), orgID)
	if err != nil || cfg == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "sso.update", orgID, map[string]any{
		"discoveryUrl": cfg.DiscoveryURL, "autoProvision": cfg.AutoProvision,
	}); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "sso.update", "orgID", orgID, "error", err.Error())
	}
	c.JSON(http.StatusOK, toSSOResponse(cfg))
}

// Delete handles DELETE /api/v1/orgs/:id/sso (org admin).
func (h *SSOHandler) Delete(c *gin.Context) {
	orgID := c.Param("id")
	actorID := h.authSvc.GetUserID(c)
	if err := h.store.DeleteSSOConfig(c.Request.Context(), orgID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete sso config"})
		return
	}
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "sso.delete", orgID, nil); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "sso.delete", "orgID", orgID, "error", err.Error())
	}
	c.Status(http.StatusNoContent)
}

// VerifyDomain handles POST /api/v1/orgs/:id/sso/domains/:domain/verify (org
// admin). On-demand DNS verification: checks the TXT record at
// _llmsafespaces-verify.<domain> for the org's verification token and promotes
// the domain to verified on match.
func (h *SSOHandler) VerifyDomain(c *gin.Context) {
	orgID := c.Param("id")
	domain := c.Param("domain")
	actorID := h.authSvc.GetUserID(c)

	result, err := h.svc.VerifyDomain(c.Request.Context(), orgID, domain)
	if err != nil {
		respondVerifyError(c, err)
		return
	}
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "sso.domain.verify", domain, map[string]any{
		"verified": result.Verified,
	}); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "sso.domain.verify", "orgID", orgID, "error", err.Error())
	}
	c.JSON(http.StatusOK, result)
}

// RotateToken handles POST /api/v1/orgs/:id/sso/verification-token/rotate (org
// admin). Replaces the DNS verification token with a fresh random value. Used
// for both initial creation (when no token exists) and rotation.
func (h *SSOHandler) RotateToken(c *gin.Context) {
	orgID := c.Param("id")
	actorID := h.authSvc.GetUserID(c)

	token, err := h.svc.RotateToken(c.Request.Context(), orgID)
	if err != nil {
		if errors.Is(err, sso.ErrSSONotConfigured) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate verification token"})
		return
	}
	if err := h.store.LogOrgEvent(c.Request.Context(), orgID, actorID, "sso.token.rotate", orgID, nil); err != nil && h.logger != nil {
		h.logger.Warn("audit log emission failed", "action", "sso.token.rotate", "orgID", orgID, "error", err.Error())
	}
	c.JSON(http.StatusOK, gin.H{"verificationToken": token})
}

// respondVerifyError maps SSO service verification errors to HTTP responses.
func respondVerifyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sso.ErrSSONotConfigured):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case errors.Is(err, sso.ErrDomainNotClaimed):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, sso.ErrNoVerificationToken):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, sso.ErrDNSNotMatching):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "domain verification is currently unavailable"})
	}
}

// Start handles GET /api/v1/auth/sso/:orgSlug/start (public). Redirects the
// browser to the IdP authorization endpoint and sets the signed PKCE/state cookie.
// All error paths redirect to the frontend with an sso= error token so the
// user sees a clean message regardless of entry point (direct link, login-page
// button, or /sso/:orgSlug shortcut).
func (h *SSOHandler) Start(c *gin.Context) {
	orgSlug := c.Param("orgSlug")
	redirectURL, err := h.resolveCallbackURL(c, orgSlug)
	if err != nil {
		// F11: redirect base URL unset — refuse rather than trust X-Forwarded-*.
		// resolveCallbackURL already logs the detailed warning.
		c.Redirect(http.StatusFound, h.frontendRedirectWithError(err))
		return
	}

	res, err := h.svc.StartLogin(c.Request.Context(), orgSlug, redirectURL)
	if err != nil {
		if h.logger != nil && !errors.Is(err, sso.ErrSSONotConfigured) {
			h.logger.Warn("sso start failed", "orgSlug", orgSlug, "error", err.Error())
		}
		c.Redirect(http.StatusFound, h.frontendRedirectWithError(err))
		return
	}
	h.setStateCookie(c, res.Cookie)
	c.Redirect(http.StatusFound, res.AuthURL)
}

// Callback handles GET /api/v1/auth/sso/:orgSlug/callback (public). Exchanges
// the code, sets the session JWT cookie, and redirects to the frontend.
func (h *SSOHandler) Callback(c *gin.Context) {
	orgSlug := c.Param("orgSlug")
	redirectURL, err := h.resolveCallbackURL(c, orgSlug)
	if err != nil {
		// F11: redirect base URL unset. The browser is mid-flow (IdP has
		// already redirected back here), so a JSON body is not usable; route
		// the failure to the frontend with a config_error token.
		c.Redirect(http.StatusFound, h.frontendRedirectWithError(err))
		return
	}
	code := c.Query("code")
	state := c.Query("state")
	cookieValue, _ := c.Cookie(h.svc.CookieName())

	result, err := h.svc.HandleCallback(c.Request.Context(), orgSlug, redirectURL, code, state, cookieValue)
	h.clearStateCookie(c)
	if err != nil {
		c.Redirect(http.StatusFound, h.frontendRedirectWithError(err))
		return
	}

	maxAge := int(h.svc.TokenTTL().Seconds())
	if maxAge <= 0 {
		maxAge = 86400
	}
	c.SetCookie(h.sessionCookie, result.Token, maxAge, "/", h.cookieDomain, true, true)
	c.Redirect(http.StatusFound, h.frontendRedirectWithSuccess())
}

// Domains handles GET /api/v1/auth/sso/domains (public). Returns every claimed
// SSO domain for login-page discovery.
func (h *SSOHandler) Domains(c *gin.Context) {
	domains, err := h.store.ListSSODomains(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sso domains"})
		return
	}
	if domains == nil {
		domains = []types.SSODomain{}
	}
	c.JSON(http.StatusOK, gin.H{"domains": domains})
}

// OIDCEnabled reports whether any org has configured SSO, for the /auth/config
// feature flag. A DB error is treated as "disabled" so a transient failure does
// not advertise SSO when there is none to complete.
func (h *SSOHandler) OIDCEnabled(ctx context.Context) bool {
	if h.store == nil {
		return false
	}
	n, err := h.store.CountSSOConfigs(ctx)
	if err != nil {
		return false
	}
	return n > 0
}

// resolveCallbackURL builds the absolute IdP-registered callback URL for the
// given org slug. OIDC.RedirectBaseURL wins when set; when it is unset the
// function refuses to derive the URL and returns ErrRedirectBaseURLNotSet.
//
// Security (F11): the SSO callback URL is where the IdP redirects with the
// authorization code, so it must be the operator's canonical URL, not a value
// assembled from X-Forwarded-Proto / Host headers — both attacker-influenceable
// at a misconfigured reverse proxy. The previous behavior derived the URL from
// those headers with a warning; that left the trust gap open in every unconfigured
// deploy. Fail-loud forces the operator to state the base URL explicitly (set
// oidc.redirectBaseUrl) and makes the unsafe default impossible.
func (h *SSOHandler) resolveCallbackURL(c *gin.Context, orgSlug string) (string, error) {
	path := "/api/v1/auth/sso/" + orgSlug + "/callback"
	if base := h.svc.RedirectBaseURL(); base != "" {
		return strings.TrimRight(base, "/") + path, nil
	}
	if h.logger != nil {
		h.logger.Warn("OIDC redirect base URL is not configured; refusing to derive callback URL from X-Forwarded-* headers (set oidc.redirectBaseUrl)",
			"slug", orgSlug, "host", c.Request.Host)
	}
	return "", sso.ErrRedirectBaseURLNotSet
}

func (h *SSOHandler) setStateCookie(c *gin.Context, cookie *sso.SignedCookie) {
	maxAge := int(cookie.MaxAge.Seconds())
	// SameSite=Lax so the cookie survives the top-level IdP → callback GET
	// redirect while staying unavailable to cross-site POST/script fetches.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(cookie.Name, cookie.Value, maxAge, "/", "", true, true)
}

func (h *SSOHandler) clearStateCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(h.svc.CookieName(), "", -1, "/", "", true, true)
}

func (h *SSOHandler) frontendRedirectWithSuccess() string {
	return h.appendSSOParam(h.frontendURL, "success")
}

func (h *SSOHandler) frontendRedirectWithError(err error) string {
	base := h.loginRedirectBase()
	return h.appendSSOParam(base, errorReason(err))
}

// loginRedirectBase returns the frontend URL targeting /login so that SSO
// error tokens (?sso=...) are not stripped by the SPA's redirect chain
// (root → /chat → /login drops query params). Error redirects always land
// on the login page; success redirects go to the root (the user is
// authenticated and GuestOnly routes them to /chat).
func (h *SSOHandler) loginRedirectBase() string {
	if h.frontendURL == "" {
		return "/login"
	}
	return strings.TrimRight(h.frontendURL, "/") + "/login"
}

func (h *SSOHandler) appendSSOParam(base, status string) string {
	if base == "" {
		base = "/"
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "sso=" + status
}

// errorReason maps an SSO service error to a short, client-safe status token.
// It never leaks internal detail.
func errorReason(err error) string {
	switch {
	case errors.Is(err, sso.ErrAutoProvisionOff):
		return "provisioning_disabled"
	case errors.Is(err, sso.ErrUserSuspended):
		return "suspended"
	case errors.Is(err, sso.ErrEmailUnverified):
		return "email_unverified"
	case errors.Is(err, sso.ErrSSONotConfigured):
		return "not_configured"
	case errors.Is(err, sso.ErrStateExpired), errors.Is(err, sso.ErrStateInvalid):
		return "state_invalid"
	case errors.Is(err, sso.ErrRedirectBaseURLNotSet):
		return "config_error"
	default:
		return "error"
	}
}

// respondSSOMutationError maps ApplyConfigMutation errors to HTTP responses.
func respondSSOMutationError(c *gin.Context, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "server key not configured"):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": msg})
	case strings.Contains(msg, "client secret is required"):
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
	case strings.Contains(msg, "invalid role"):
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save sso config"})
	}
}
