// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	"github.com/lenaxia/llmsafespaces/api/internal/utilities"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/version"
)

// TurnstileRouterConfig is the routing-side view of Cloudflare Turnstile
// CAPTCHA configuration used by the /register handler. Only the three
// fields the router needs at wire-up time; registerAuthRoutes constructs
// the fuller middleware.TurnstileConfig from this by dropping in a
// production HTTP client and logger.
//
// This is kept separate from middleware.TurnstileConfig (rather than
// re-using that type here) because middleware.TurnstileConfig contains
// an *http.Client field that's only meaningful for tests. Exposing that
// on RouterConfig — and therefore on every app.go call site that builds
// a router — would invite confusion about whether operators are meant
// to plug an httptrace-instrumented client, a custom timeout, etc. The
// separation costs us one mapping step (app.go:911-914); the
// alternative would cost us a "please just leave this nil unless
// you're writing a test" caveat at every call site.
//
// When Enabled is true, SecretKey must be non-empty (config.Load
// enforces this at startup, fail-closed). VerifyURL defaults to
// Cloudflare's production siteverify endpoint if empty.
type TurnstileRouterConfig struct {
	Enabled   bool
	SecretKey string
	VerifyURL string
}

// RouterConfig defines configuration for the router
type RouterConfig struct {
	// Debug enables debug mode
	Debug bool

	// LoggingConfig is the configuration for the logging middleware
	LoggingConfig middleware.LoggingConfig

	// RateLimitConfig is the configuration for the rate limiting middleware
	RateLimitConfig middleware.RateLimitConfig

	// PerRouteRateLimitConfig is the configuration for stricter per-route
	// rate limits applied on top of the global RateLimitConfig. Closes
	// G35 (/account/recover) and G41 (/secrets/:id/reveal) — endpoints
	// that take credentials as direct input and therefore warrant a
	// tighter cap than the global 100/min/IP. Defaults to enabled with
	// sensible limits; operators can disable by setting Enabled=false.
	PerRouteRateLimitConfig middleware.PerRouteRateLimitConfig

	// SecurityConfig is the configuration for the security middleware
	SecurityConfig middleware.SecurityConfig

	// TracingConfig is the configuration for the tracing middleware
	TracingConfig middleware.TracingConfig

	// SettingsHandler is the optional settings handler for admin/user settings routes
	SettingsHandler *handlers.SettingsHandler

	// InstanceSettings provides access to instance settings for feature flags
	InstanceSettings *settings.InstanceService

	// SecretsHandler is the handler for secret management endpoints (optional)
	SecretsHandler *handlers.SecretsHandler

	// ModelsHandler handles model listing and selection (optional).
	// Extracted from SecretsHandler (US-29.5).
	ModelsHandler *handlers.ModelsHandler

	// WorkspaceEnvHandler is the handler for workspace env-var endpoints (optional).
	// Extracted from SecretsHandler (US-29.4).
	WorkspaceEnvHandler *handlers.WorkspaceEnvHandler

	// AdminProviderCredentialsHandler handles admin credential CRUD (optional)
	AdminProviderCredentialsHandler *handlers.AdminProviderCredentialsHandler

	// UserProviderCredentialsHandler handles user credential CRUD (optional)
	UserProviderCredentialsHandler *handlers.UserProviderCredentialsHandler

	// RotateKeyHandler is the handler for key rotation (optional)
	RotateKeyHandler *handlers.RotateKeyHandler

	// UnlockDEKHandler is the soft-unlock endpoint for re-deriving the
	// DEK without forcing logout (Epic 56). Optional — when nil the
	// /auth/unlock-dek route is not registered, which is appropriate
	// for tests that don't exercise key material.
	UnlockDEKHandler *handlers.UnlockDEKHandler

	// OrgsHandler handles org CRUD routes (optional)
	OrgsHandler *handlers.OrgsHandler

	// OrgCredentialsHandler handles org credential routes (optional)
	OrgCredentialsHandler *handlers.OrgCredentialsHandler

	// TerminalHandler is the handler for WebSocket terminal proxy (optional)
	TerminalHandler *handlers.TerminalHandler

	// AgentReloadHandler handles POST /api/v1/workspaces/:id/agent/reload (optional)
	AgentReloadHandler *handlers.AgentReloadHandler

	// BulkReloadHandler handles POST /api/v1/users/me/agents/reload (optional)
	BulkReloadHandler *handlers.BulkReloadHandler

	UsageHandler         *handlers.UsageHandler
	WebhookHandler       *handlers.StripeWebhookHandler
	InvitationsHandler   *handlers.InvitationsHandler
	EmailHandler         *handlers.EmailHandler
	EmailVerifyHandler   *handlers.EmailVerifyHandler
	PasswordResetHandler *handlers.PasswordResetHandler
	PolicyHandler        *handlers.PolicyHandler
	PromptHandler        *handlers.PromptHandler
	AgentRoleHandler     *handlers.AgentRoleHandler
	AuditHandler         *handlers.AuditHandler

	// RelayAdminHandler handles relay admin setup + status endpoints (optional)
	RelayAdminHandler *handlers.RelayAdminHandler

	// PlatformInfoHandler serves the admin "Versions" display — running
	// component versions read from deployed Deployment image tags. Optional.
	PlatformInfoHandler *handlers.PlatformInfoHandler

	// AdminSessionHandler handles admin-only session recovery endpoints (optional).
	// US-44.11: force-abort a workspace session stuck in activeSess after the
	// workspace pod was deleted/unreachable.
	AdminSessionHandler *handlers.AdminSessionHandler

	// PlatformAdminHandler handles platform-admin org/user suspension
	// endpoints (US-43.19, D19/D20). Mounted behind AuthMiddleware + AdminGuard.
	PlatformAdminHandler *handlers.PlatformAdminHandler

	// InternalOrgStatusHandler, when non-nil, registers the cluster-internal
	// GET /api/v1/internal/orgs/:orgID/status endpoint that the controller
	// polls to drive org-suspension of workspaces (D20). It is intentionally
	// NOT behind AuthMiddleware; access is gated by a mandatory X-Internal-Token
	// shared-secret header (see InternalOrgStatusHandler — the endpoint FAILS
	// CLOSED with 403 when LLMSAFESPACES_INTERNAL_TOKEN is unset). An optional
	// API NetworkPolicy (chart value networkPolicy.apiIngressRestricted) adds
	// L3/L4 defense-in-depth; the token is the load-bearing control.
	InternalOrgStatusHandler *handlers.InternalOrgStatusHandler

	// PodBootstrapHandler, when non-nil, registers POST /internal/v1/pod-bootstrap
	// — the secretless credential injection endpoint (Epic 35). The workspace
	// init container presents a projected SA token; the handler validates it via
	// TokenReview and returns decrypted secrets. NOT behind AuthMiddleware (the
	// init container has no user identity); auth is the TokenReview itself.
	PodBootstrapHandler *handlers.PodBootstrapHandler

	CookieName string

	// CookieDomain (Epic 54, US-54.3): when non-empty, set as the Domain
	// attribute on the lsp_session cookie so the session survives root→subdomain
	// redirects under wildcard subdomain routing. When empty (default), the
	// cookie is host-only — current behavior, single-host deploys.
	CookieDomain string

	// SSOHandler handles org-admin SSO config CRUD + the public OIDC login flow
	// (start/callback) and claimed-domain discovery (US-43.10, D17).
	SSOHandler *handlers.SSOHandler

	// LoginDiscoveryHandler handles POST /api/v1/auth/lookup — the email-led
	// login discovery endpoint (Epic 54, US-54.1). Returns a single redirectUrl
	// pointing the browser at the user's org subdomain (or direct SSO start URL
	// when subdomain routing is disabled). Enumeration-safe: uniform 200 +
	// uniform body shape across all non-validation branches; DB errors masked.
	LoginDiscoveryHandler *handlers.LoginDiscoveryHandler

	// Turnstile, when Enabled is true, gates POST /auth/register with a
	// Cloudflare Turnstile CAPTCHA middleware. The middleware fails
	// closed: any of {missing token, verify request fails, verify
	// response says not-success} → 401. When Enabled is false, the
	// route runs without the middleware.
	//
	// SecretKey must be non-empty when Enabled is true (config.Load
	// enforces this at startup, fail-closed). VerifyURL defaults to
	// Cloudflare's production siteverify endpoint if empty.
	Turnstile TurnstileRouterConfig
}

// cookieName returns the session cookie name, falling back to "lsp_session" when empty.
func (r RouterConfig) cookieName() string {
	if r.CookieName == "" {
		return "lsp_session"
	}
	return r.CookieName
}

// DefaultRouterConfig returns the default router configuration
func DefaultRouterConfig() RouterConfig {
	rlCfg := middleware.DefaultRateLimitConfig()
	// The /events SSE endpoint is a long-lived connection, not a per-request
	// API call. Exempt it from the token-bucket rate limiter so reconnects
	// after network drops don't trigger 429s.
	rlCfg.ExemptPaths = []string{"/events", "/session-events"}
	return RouterConfig{
		Debug:           false,
		LoggingConfig:   middleware.DefaultLoggingConfig(),
		RateLimitConfig: rlCfg,
		// G35: /account/recover takes userID + recoveryKey as direct
		// input. The recovery key is 128-bit random so brute-force is
		// mathematically infeasible, but the endpoint still does
		// Argon2id work (re-derives the DEK under the new password),
		// making it a CPU-exhaustion DoS target. The authRatePerMinute
		// constant (20) was defined for exactly this purpose but was
		// never wired (dead code before this PR). authRateBurst (5)
		// allows legitimate users a few rapid attempts if they fat-
		// finger the recovery key, while still capping automated
		// guessing from a single IP well below the global 100/min.
		PerRouteRateLimitConfig: middleware.PerRouteRateLimitConfig{
			Enabled: true,
			Routes: map[string]middleware.RouteRateLimit{
				"/api/v1/account/recover": {
					Limit:  authRatePerMinute,
					Burst:  authRateBurst,
					Window: time.Minute,
				},
				// G41/G6: /secrets/:id/reveal takes the user's password
				// as input to re-authenticate before decrypting. The
				// endpoint is a credential-bearing target — without a
				// per-endpoint cap, the global 100/min/IP limiter lets
				// a single IP attempt 100 password guesses per minute.
				// 5/min/burst-5 matches the password-verify rate a
				// legitimate user would produce (re-reveal a few
				// secrets in quick succession) while making brute-force
				// impractical (5 attempts/min → 7,200/day; bcrypt cost
				// 12 makes each attempt ~250ms, so 30min of CPU per
				// 7,200 guesses — well below practical brute-force
				// thresholds for strong passwords).
				"/api/v1/secrets/:id/reveal": {
					Limit:  5,
					Burst:  5,
					Window: time.Minute,
				},
			},
		},
		SecurityConfig: middleware.DefaultSecurityConfig(),
		TracingConfig:  middleware.DefaultTracingConfig(),
	}
}

// NewRouter creates a new Gin router with all routes configured.
// proxyHandler may be nil — proxy routes are not registered in that case.
func NewRouter(services interfaces.Services, logger *apilogger.Logger, proxyHandler *handlers.ProxyHandler, config ...RouterConfig) *gin.Engine {
	// Use default config if none provided
	cfg := DefaultRouterConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	// Set Gin mode
	if cfg.Debug {
		gin.SetMode(gin.DebugMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create router
	router := gin.New()

	// Add middleware in the correct order
	router.Use(middleware.RecoveryMiddleware(logger))
	router.Use(middleware.TracingMiddleware(logger, cfg.TracingConfig))
	router.Use(middleware.SecurityMiddleware(logger, cfg.SecurityConfig))
	router.Use(middleware.LoggingMiddleware(logger, cfg.LoggingConfig))
	router.Use(middleware.MetricsMiddleware(services.GetMetrics()))
	router.Use(middleware.RateLimitMiddleware(services.GetRateLimiter(), logger, cfg.RateLimitConfig, cfg.InstanceSettings))
	// G35: stricter per-route limits applied AFTER the global limiter so
	// the global budget is consumed first (defense-in-depth — both
	// middleware must allow the request). The per-route buckets are
	// keyed separately (see PerRouteRateLimitMiddleware doc), so a user
	// hitting /recover cannot deplete the budget for /secrets/:id/reveal
	// or vice versa.
	router.Use(middleware.PerRouteRateLimitMiddleware(services.GetRateLimiter(), logger, cfg.PerRouteRateLimitConfig))
	router.Use(middleware.ErrorHandlerMiddleware(logger))

	if services.GetMetering() != nil {
		router.Use(middleware.NewMeteringMiddleware(services.GetMetering()).Handler())
	}

	// F1.1.4 (Epic 17): the previous `/api/v1/workspaces/:id/stream`
	// group had middleware attached but no handlers — dead code that
	// existed only because an earlier API design wired SSE here. The
	// current session SSE endpoint is `/api/v1/workspaces/:id/session-events`
	// registered below; the user-scoped event stream is at `/api/v1/events`.
	// The actual WebSocket terminal endpoint is
	// `/api/v1/workspaces/:id/terminal/...` which gets the WebSocket
	// security middleware via its own router group.
	_ = router // wsGroup removal — kept the var to avoid unused-import warnings if a future commit re-adds /stream.

	// Auth routes (public — no auth middleware)
	authGroup := router.Group("/api/v1/auth")
	registerAuthRoutes(authGroup, services, cfg.InstanceSettings, logger, cfg.cookieName(), cfg.CookieDomain, cfg.SSOHandler, cfg.Turnstile)

	// US-49.5: Password reset via email (public — the token IS the credential
	// for confirm; request is always 202 with no enumeration).
	if cfg.PasswordResetHandler != nil {
		authGroup.POST("/password-reset/request", cfg.PasswordResetHandler.Request)
		authGroup.POST("/password-reset/confirm", cfg.PasswordResetHandler.Confirm)
	}

	// US-49.6: Email verification (public — the token IS the credential).
	if cfg.EmailVerifyHandler != nil {
		authGroup.POST("/verify-email", cfg.EmailVerifyHandler.Verify)
		authGroup.POST("/verify-email/resend", cfg.EmailVerifyHandler.Resend)
	}

	// Epic 54, US-54.1: Email-led login discovery (public — enumeration-safe).
	// Resolves an email to a single redirectUrl pointing at the user's org.
	// Always returns 200 with { redirectUrl } on valid input; DB errors masked.
	if cfg.LoginDiscoveryHandler != nil {
		authGroup.POST("/lookup", cfg.LoginDiscoveryHandler.Lookup)
	}

	// Authenticated workspace routes
	workspaceGroup := router.Group("/api/v1/workspaces")
	workspaceGroup.Use(services.GetAuth().AuthMiddleware())

	// Design 0041 D1/D3: every /:id workspace route funnels through
	// WorkspaceAccessMiddleware, the single ownership gate (D5 creator-membership
	// + D6 org-admin). List/Create have no :id and stay on workspaceGroup.
	idGroup := workspaceGroup.Group("/:id")
	idGroup.Use(middleware.WorkspaceAccessMiddleware(services.GetWorkspace()))

	registerWorkspaceRoutes(workspaceGroup, idGroup, services, proxyHandler, cfg)

	// Epic 27b: Bulk agent reload across all pending workspaces.
	if cfg.BulkReloadHandler != nil {
		userGroup := router.Group("/api/v1/users/me")
		userGroup.Use(services.GetAuth().AuthMiddleware())
		userGroup.POST("/agents/reload", cfg.BulkReloadHandler.BulkReload)
	}

	// Sessions/active endpoint — needs proxyHandler for active session data.
	// Registered on idGroup so it inherits WorkspaceAccessMiddleware.
	if proxyHandler != nil {
		idGroup.GET("/sessions/active", func(c *gin.Context) {
			userID := services.GetAuth().GetUserID(c)
			if userID == "" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
				return
			}
			workspaceID := c.Param("id")
			// Get active sessions keyed by workspace ID directly.
			active := proxyHandler.GetActiveSessions(c.Request.Context(), workspaceID)
			if active == nil {
				active = []string{}
			}
			c.JSON(http.StatusOK, types.ActiveSessionsResponse{
				Active:    active,
				MaxActive: getMaxActiveSessions(c.Request.Context(), cfg.InstanceSettings),
			})
		})
	}

	// Authenticated workspace CRUD routes (Create, List, Get, Delete, Status).
	// List/Create have no :id and intentionally bypass WorkspaceAccessMiddleware;
	// Get/Delete/etc. are registered on idGroup inside registerWorkspaceRoutes
	// and therefore inherit the middleware.
	// Proxy routes — registered on idGroup when a ProxyHandler is provided.
	if proxyHandler != nil {
		registerProxyRoutes(idGroup, proxyHandler)

		// S28.3: User-scoped SSE stream (authenticated, rate-limit exempt)
		eventsGroup := router.Group("/api/v1")
		eventsGroup.Use(services.GetAuth().AuthMiddleware())
		eventsGroup.GET("/events", proxyHandler.StreamUserEvents)
	}

	// Terminal proxy routes (WebSocket terminal to sandbox pod)
	if cfg.TerminalHandler != nil {
		// Ticket endpoint — on idGroup so WorkspaceAccessMiddleware runs first.
		// The handler keeps its existing label-based check (Story 2 removes it).
		idGroup.POST("/terminal/ticket", cfg.TerminalHandler.HandleTicket)
		// WebSocket endpoint — on the ROOT router (auth via one-time ticket, not JWT).
		// Ticket-based auth is by design (design 0041 edge case 3); the ticket was
		// issued after middleware verification, so it inherits the ownership check.
		router.GET("/api/v1/workspaces/:id/terminal", cfg.TerminalHandler.HandleTerminal)
	}

	// Settings routes (admin + user)
	if cfg.SettingsHandler != nil {
		registerSettingsRoutes(router, services, cfg.SettingsHandler)
	}

	// Admin provider credentials routes (Epic 30)
	if cfg.AdminProviderCredentialsHandler != nil {
		adminCreds := router.Group("/api/v1/admin/provider-credentials")
		adminCreds.Use(services.GetAuth().AuthMiddleware())
		adminCreds.Use(middleware.AdminGuard())
		adminCreds.POST("", cfg.AdminProviderCredentialsHandler.Create)
		adminCreds.GET("", cfg.AdminProviderCredentialsHandler.List)
		adminCreds.GET("/:id", cfg.AdminProviderCredentialsHandler.Get)
		adminCreds.PUT("/:id", cfg.AdminProviderCredentialsHandler.Update)
		adminCreds.DELETE("/:id", cfg.AdminProviderCredentialsHandler.Delete)
		adminCreds.GET("/:id/models", cfg.AdminProviderCredentialsHandler.ProbeModels)
		adminCreds.POST("/:id/auto-apply", cfg.AdminProviderCredentialsHandler.CreateAutoApply)
		adminCreds.GET("/:id/auto-apply", cfg.AdminProviderCredentialsHandler.ListAutoApply)
		adminCreds.DELETE("/:id/auto-apply/:targetType/:targetId", cfg.AdminProviderCredentialsHandler.DeleteAutoApply)
	}

	// Anon credential probe — authenticated but credential-free.
	// Used by the user credential form to fetch models before saving.
	{
		probeGroup := router.Group("/api/v1/probe-models")
		probeGroup.Use(services.GetAuth().AuthMiddleware())
		probeGroup.POST("", handlers.ProbeModelsAnon)
	}

	// User provider credentials routes (Epic 30)
	if cfg.UserProviderCredentialsHandler != nil {
		userCreds := router.Group("/api/v1/provider-credentials")
		userCreds.Use(services.GetAuth().AuthMiddleware())
		userCreds.POST("", cfg.UserProviderCredentialsHandler.Create)
		userCreds.GET("", cfg.UserProviderCredentialsHandler.List)
		userCreds.GET("/:id", cfg.UserProviderCredentialsHandler.Get)
		userCreds.GET("/:id/models", cfg.UserProviderCredentialsHandler.ProbeModels)
		userCreds.DELETE("/:id", cfg.UserProviderCredentialsHandler.Delete)
		userCreds.GET("/:id/bindings", cfg.UserProviderCredentialsHandler.ListBindings)
		userCreds.POST("/:id/bind/:workspaceId", cfg.UserProviderCredentialsHandler.Bind)
		userCreds.DELETE("/:id/bind/:workspaceId", cfg.UserProviderCredentialsHandler.Unbind)
	}

	if cfg.UsageHandler != nil {
		usage := router.Group("/api/v1/usage")
		usage.Use(services.GetAuth().AuthMiddleware())
		usage.GET("", cfg.UsageHandler.GetUsage)
		usage.GET("/workspaces/:id", cfg.UsageHandler.GetWorkspaceUsage)
		usage.GET("/quota", cfg.UsageHandler.GetQuotaStatus)

		adminUsage := router.Group("/api/v1/admin/usage")
		adminUsage.Use(services.GetAuth().AuthMiddleware())
		adminUsage.Use(middleware.AdminGuard())
		adminUsage.GET("/:ownerId", cfg.UsageHandler.AdminGetUsage)

		adminBilling := router.Group("/api/v1/admin/billing")
		adminBilling.Use(services.GetAuth().AuthMiddleware())
		adminBilling.Use(middleware.AdminGuard())
		adminBilling.GET("/status", cfg.UsageHandler.AdminBillingStatus)
		adminBilling.GET("/dlq", cfg.UsageHandler.AdminGetDLQ)
		adminBilling.POST("/dlq/:id/retry", cfg.UsageHandler.AdminRetryDLQ)
		adminBilling.POST("/dlq/:id/discard", cfg.UsageHandler.AdminDiscardDLQ)
	}

	if cfg.WebhookHandler != nil {
		router.POST("/api/v1/webhooks/stripe", cfg.WebhookHandler.HandleWebhook)
	}

	// Relay admin routes (Epic 43)
	if cfg.RelayAdminHandler != nil {
		relayAdmin := router.Group("/api/v1/admin/relay")
		relayAdmin.Use(services.GetAuth().AuthMiddleware())
		relayAdmin.Use(middleware.AdminGuard())
		relayAdmin.GET("/setup", cfg.RelayAdminHandler.GetSetup)
		relayAdmin.GET("/status", cfg.RelayAdminHandler.GetStatus)
		relayAdmin.POST("/oci-creds", cfg.RelayAdminHandler.SaveOCICreds)
		relayAdmin.POST("/gcp-creds", cfg.RelayAdminHandler.SaveGCPCreds)
		relayAdmin.POST("/aws-creds", cfg.RelayAdminHandler.SaveAWSCreds)
		relayAdmin.POST("/deploy", cfg.RelayAdminHandler.Deploy)
		relayAdmin.POST("/rotate/:id", cfg.RelayAdminHandler.Rotate)
		relayAdmin.POST("/pause", cfg.RelayAdminHandler.Pause)
		relayAdmin.POST("/resume", cfg.RelayAdminHandler.Resume)
	}

	// Platform info — running component versions for the admin "Versions" tab.
	if cfg.PlatformInfoHandler != nil {
		platformInfo := router.Group("/api/v1/admin/platform-info")
		platformInfo.Use(services.GetAuth().AuthMiddleware())
		platformInfo.Use(middleware.AdminGuard())
		platformInfo.GET("", cfg.PlatformInfoHandler.GetPlatformInfo)
	}

	// Admin session recovery routes (US-44.11)
	if cfg.AdminSessionHandler != nil {
		adminSessions := router.Group("/api/v1/admin/workspaces/:workspaceId/sessions")
		adminSessions.Use(services.GetAuth().AuthMiddleware())
		adminSessions.Use(middleware.AdminGuard())
		adminSessions.POST("/:sessionId/force-abort", cfg.AdminSessionHandler.ForceAbortSession)
	}

	// US-43.20: Cross-org audit view (platform admin only).
	if cfg.AuditHandler != nil {
		platformAudit := router.Group("/api/v1/admin/audit")
		platformAudit.Use(services.GetAuth().AuthMiddleware())
		platformAudit.Use(middleware.AdminGuard())
		platformAudit.GET("", cfg.AuditHandler.ListCrossOrg)
	}

	// Platform-admin system prompt (agent customization, Phase 1).
	if cfg.PromptHandler != nil {
		promptAdmin := router.Group("/api/v1/admin/prompt")
		promptAdmin.Use(services.GetAuth().AuthMiddleware())
		promptAdmin.Use(middleware.AdminGuard())
		promptAdmin.GET("", cfg.PromptHandler.GetPlatform)
		promptAdmin.PUT("", cfg.PromptHandler.SetPlatform)
	}

	if cfg.AgentRoleHandler != nil {
		roleAdmin := router.Group("/api/v1/admin/agent-roles")
		roleAdmin.Use(services.GetAuth().AuthMiddleware())
		roleAdmin.Use(middleware.AdminGuard())
		roleAdmin.GET("", cfg.AgentRoleHandler.ListPlatform)
		roleAdmin.POST("", cfg.AgentRoleHandler.CreatePlatform)
		roleAdmin.GET("/:id", cfg.AgentRoleHandler.GetPlatform)
		roleAdmin.PUT("/:id", cfg.AgentRoleHandler.UpdatePlatform)
		roleAdmin.DELETE("/:id", cfg.AgentRoleHandler.DeletePlatform)
	}

	// AuthMiddleware + AdminGuard so only users.role='admin' can call them.
	// US-43.18: the same group also hosts the dashboard list endpoints
	// (GET /admin/orgs, GET /admin/users).
	if cfg.PlatformAdminHandler != nil {
		suspendGrp := router.Group("/api/v1/admin")
		suspendGrp.Use(services.GetAuth().AuthMiddleware())
		suspendGrp.Use(middleware.AdminGuard())
		suspendGrp.GET("/orgs", cfg.PlatformAdminHandler.ListOrgs)
		suspendGrp.GET("/users", cfg.PlatformAdminHandler.ListUsers)
		suspendGrp.POST("/orgs/:id/suspend", cfg.PlatformAdminHandler.SuspendOrg)
		suspendGrp.POST("/orgs/:id/unsuspend", cfg.PlatformAdminHandler.UnsuspendOrg)
		suspendGrp.POST("/users/:id/suspend", cfg.PlatformAdminHandler.SuspendUser)
		suspendGrp.POST("/users/:id/unsuspend", cfg.PlatformAdminHandler.UnsuspendUser)
	}

	// Epic 49 US-49.4: Admin email test-send. Registered independently of
	// settings so it does not silently vanish if SettingsHandler becomes
	// conditional. Admin-only (AuthMiddleware + AdminGuard).
	if cfg.EmailHandler != nil {
		emailAdmin := router.Group("/api/v1/admin/email")
		emailAdmin.Use(services.GetAuth().AuthMiddleware())
		emailAdmin.Use(middleware.AdminGuard())
		emailAdmin.POST("/test", cfg.EmailHandler.TestSend)
	}

	// US-43.19 / D20: cluster-internal org-status endpoint polled by the
	// workspace controller. NOT behind AuthMiddleware (the controller has no
	// user identity); gated by a mandatory X-Internal-Token shared-secret
	// header (fail-closed 403 when unset). An optional API NetworkPolicy adds
	// L3/L4 defense-in-depth.
	if cfg.InternalOrgStatusHandler != nil {
		router.GET("/api/v1/internal/orgs/:orgID/status", cfg.InternalOrgStatusHandler.GetOrgStatus)
	}

	// Epic 35 US-35.3: pod bootstrap endpoint. Auth is K8s TokenReview (no JWT).
	if cfg.PodBootstrapHandler != nil {
		router.POST("/internal/v1/pod-bootstrap", cfg.PodBootstrapHandler.Bootstrap)
	}

	// Secret management routes (Epic 10)
	if cfg.SecretsHandler != nil {
		secretsGroup := router.Group("/api/v1/secrets")
		secretsGroup.Use(services.GetAuth().AuthMiddleware())
		secretsGroup.POST("", cfg.SecretsHandler.CreateSecret)
		secretsGroup.GET("", cfg.SecretsHandler.ListSecrets)
		secretsGroup.GET("/audit", cfg.SecretsHandler.GetAuditLog)
		secretsGroup.GET("/:id", cfg.SecretsHandler.GetSecret)
		secretsGroup.PUT("/:id", cfg.SecretsHandler.UpdateSecret)
		secretsGroup.DELETE("/:id", cfg.SecretsHandler.DeleteSecret)
		secretsGroup.POST("/:id/reveal", cfg.SecretsHandler.RevealSecret)
		secretsGroup.GET("/:id/bindings", cfg.SecretsHandler.GetSecretBindings)

		// Secrets/models routes — registered on idGroup so they inherit
		// WorkspaceAccessMiddleware. Story 2 removes the now-redundant
		// SecretService.verifyWorkspaceOwner + handler-level meta.UserID checks.
		// Env routes are registered via WorkspaceEnvHandler below (US-29.4).
		idGroup.PUT("/bindings", cfg.SecretsHandler.SetBindings)
		idGroup.GET("/bindings", cfg.SecretsHandler.GetBindings)
		idGroup.POST("/reload-secrets", cfg.SecretsHandler.ReloadSecrets)
	}

	// Model routes (US-29.5: extracted from SecretsHandler)
	// Registered on idGroup so they inherit WorkspaceAccessMiddleware.
	if cfg.ModelsHandler != nil {
		idGroup.GET("/models", cfg.ModelsHandler.ListModels)
		idGroup.PUT("/model", cfg.ModelsHandler.SetModel)
	}

	// Workspace env-var routes (US-29.4: extracted from SecretsHandler).
	// Registered on idGroup so they inherit WorkspaceAccessMiddleware.
	if cfg.WorkspaceEnvHandler != nil {
		idGroup.PUT("/env", cfg.WorkspaceEnvHandler.SetWorkspaceEnv)
		idGroup.GET("/env", cfg.WorkspaceEnvHandler.GetWorkspaceEnv)
		idGroup.DELETE("/env/:name", cfg.WorkspaceEnvHandler.DeleteWorkspaceEnv)
	}

	// Key rotation endpoint (Epic 10)
	if cfg.RotateKeyHandler != nil {
		accountGroup := router.Group("/api/v1/account")
		accountGroup.Use(services.GetAuth().AuthMiddleware())
		accountGroup.POST("/rotate-key", cfg.RotateKeyHandler.RotateKey)
		accountGroup.POST("/change-password", cfg.RotateKeyHandler.ChangePassword)
		router.POST("/api/v1/account/recover", cfg.RotateKeyHandler.RecoverAccount)
	}

	// Soft-unlock endpoint (Epic 56). Behind AuthMiddleware so the
	// middleware stashes the matched JWT signing key on the gin context —
	// the handler then forwards it to KeyService.UnlockDEKWithSigningKey
	// to rewrap the durable jwt_sessions row under the SAME key the
	// JWT validated under (not the active signing key — see the [HIGH]
	// regression case from PR #411 review pass 1 and worklog 0550).
	if cfg.UnlockDEKHandler != nil {
		unlockGroup := router.Group("/api/v1/auth")
		unlockGroup.Use(services.GetAuth().AuthMiddleware())
		unlockGroup.POST("/unlock-dek", cfg.UnlockDEKHandler.Unlock)
	}

	// Org CRUD routes (Epic 11)
	if cfg.OrgsHandler != nil {
		registerOrgRoutes(router, services, cfg.OrgsHandler, cfg.OrgCredentialsHandler, cfg.InvitationsHandler, cfg.PolicyHandler, cfg.PromptHandler, cfg.AgentRoleHandler, cfg.AuditHandler, cfg.SSOHandler)
	}

	// Metrics endpoint.
	//
	// F1.1.3 (Epic 17): pre-fix /metrics was unauthenticated, leaking
	// internal counters (request rates per route, error rates, etc.)
	// to any pod that could route to the API service. We now require
	// `Authorization: Bearer <token>` if the env var
	// LLMSAFESPACES_METRICS_TOKEN is set. Operators who want
	// Prometheus to scrape unauthenticated should leave the env unset
	// (matching the pre-fix behavior with explicit opt-in).
	router.GET("/metrics", func(c *gin.Context) {
		token := os.Getenv("LLMSAFESPACES_METRICS_TOKEN")
		if token != "" && c.GetHeader("Authorization") != "Bearer "+token {
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		promhttp.Handler().ServeHTTP(c.Writer, c.Request)
	})

	// Liveness probe — always returns 200 if the process is responding.
	// Use this for Kubernetes livenessProbe. Includes the build version so
	// operators can verify which version is running via a simple curl.
	livenessHandler := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"version": version.Version,
		})
	}
	router.GET("/livez", livenessHandler)

	// Legacy alias retained for backwards compatibility with deployments
	// that already point at /health. Equivalent to /livez.
	router.GET("/health", livenessHandler)

	// Readiness probe — verifies that all upstream dependencies (Postgres,
	// Redis) are reachable. Returns 503 if any dependency is down. Use this
	// for Kubernetes readinessProbe so the pod is removed from Service
	// endpoints when its dependencies are unavailable.
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		// F1.1.1 (Epic 17): pre-fix the failure list contained the
		// raw `err.Error()` from the driver, which can include the
		// connection string, hostname, port, and sometimes the
		// password depending on the driver. We now log the detailed
		// error server-side and return only a generic component
		// status to the client.
		var failures []string

		db := services.GetDatabase()
		if db == nil {
			failures = append(failures, "database: not configured")
		} else if err := db.Ping(ctx); err != nil {
			logger.Warn("/readyz: database ping failed",
				"error", err.Error())
			failures = append(failures, "database: unreachable")
		}

		cache := services.GetCache()
		if cache == nil {
			failures = append(failures, "cache: not configured")
		} else if err := cache.Ping(ctx); err != nil {
			logger.Warn("/readyz: cache ping failed",
				"error", err.Error())
			failures = append(failures, "cache: unreachable")
		}

		if len(failures) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"status":   "unhealthy",
				"failures": failures,
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	return router
}

const (
	maxAuthBodyBytes  = 1 << 20 // 1 MiB max for auth request bodies
	authRatePerMinute = 20
	authRateBurst     = 5
)

// sanitizeBindError returns a user-safe error message for binding failures
// without leaking internal struct details.
func sanitizeBindError(err error) string {
	return "invalid request body"
}

// setSessionCookie sets the HttpOnly session cookie on the response.
// maxAge is in seconds and must match the JWT's TTL.
// cookieName is the cookie name from RouterConfig (defaults to "lsp_session").
// cookieDomain is the Domain attribute (empty = host-only; set when wildcard
// subdomain routing is enabled so the cookie is visible across subdomains).
func setSessionCookie(c *gin.Context, token string, maxAge int, cookieName, cookieDomain string) {
	c.SetCookie(cookieName, token, maxAge, "/", cookieDomain, true, true)
}

// API key management routes.
func registerAuthRoutes(rg *gin.RouterGroup, services interfaces.Services, instanceSettings *settings.InstanceService, logger *apilogger.Logger, cookieName, cookieDomain string, ssoHandler *handlers.SSOHandler, turnstile TurnstileRouterConfig) {
	authSvc := services.GetAuth()

	// Public: feature flag discovery
	rg.GET("/config", func(c *gin.Context) {
		regEnabled := true // default
		instanceName := "LLMSafeSpaces"
		motd := ""
		if instanceSettings != nil {
			if v, err := instanceSettings.GetBool(c.Request.Context(), settings.KeyAuthRegistrationEnabled.Name()); err == nil {
				regEnabled = v
			}
			if v, err := instanceSettings.GetString(c.Request.Context(), settings.KeyInstanceName.Name()); err == nil && v != "" {
				instanceName = v
			}
			if v, err := instanceSettings.GetString(c.Request.Context(), settings.KeyInstanceMOTD.Name()); err == nil {
				motd = v
			}
		}
		// OIDCEnabled is true when at least one org has configured SSO. Falls
		// back to false on DB error or when SSO is not wired.
		oidcEnabled := false
		if ssoHandler != nil {
			oidcEnabled = ssoHandler.OIDCEnabled(c.Request.Context())
		}
		c.JSON(http.StatusOK, types.AuthConfig{
			RegistrationEnabled: regEnabled,
			OIDCEnabled:         oidcEnabled,
			InstanceName:        instanceName,
			MOTD:                motd,
		})
	})

	// US-43.10: public OIDC SSO endpoints. Start + callback carry the org slug
	// in the path; domains powers login-page discovery. All three are anonymous.
	if ssoHandler != nil {
		rg.GET("/sso/domains", ssoHandler.Domains)
		rg.GET("/sso/:orgSlug/start", ssoHandler.Start)
		rg.GET("/sso/:orgSlug/callback", ssoHandler.Callback)
	}

	// Build the /register handler chain. When Turnstile is enabled, the
	// middleware runs first (fails-closed on any token issue) and only
	// then invokes the register handler. When disabled, we register the
	// same handler naked.
	registerHandler := func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthBodyBytes)
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeBindError(err)})
			return
		}
		resp, err := authSvc.Register(c.Request.Context(), req)
		if err != nil {
			respondWithError(c, err)
			return
		}
		maxAge := int(resp.TokenTTL.Seconds())
		if maxAge <= 0 {
			maxAge = 86400 // safe fallback: matches default tokenDuration
		}
		setSessionCookie(c, resp.Token, maxAge, cookieName, cookieDomain)
		c.JSON(http.StatusCreated, resp)
	}
	if turnstile.Enabled {
		rg.POST("/register", middleware.Turnstile(middleware.TurnstileConfig{
			SecretKey: turnstile.SecretKey,
			VerifyURL: turnstile.VerifyURL,
			Logger:    logger.ZapLogger(),
		}), registerHandler)
	} else {
		rg.POST("/register", registerHandler)
	}

	rg.POST("/login", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthBodyBytes)
		var req types.LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeBindError(err)})
			return
		}
		// G13: propagate client IP through context so the lockout
		// logic can key on email+IP, preventing an attacker who knows
		// the victim's email from locking them out from a different IP.
		loginCtx := auth.WithClientIP(c.Request.Context(), c.ClientIP())
		resp, err := authSvc.Login(loginCtx, req)
		if err != nil {
			if errors.Is(err, auth.ErrEmailNotVerified) {
				c.JSON(http.StatusForbidden, gin.H{"error": err.Error(), "emailVerified": false})
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		maxAge := int(resp.TokenTTL.Seconds())
		if maxAge <= 0 {
			maxAge = 86400 // safe fallback: matches default tokenDuration
		}
		setSessionCookie(c, resp.Token, maxAge, cookieName, cookieDomain)
		c.JSON(http.StatusOK, resp)
	})

	// Public: logout
	//
	// G18 (Epic 17 Phase 4 RT-4.13): the JWT must be added to the
	// revocation cache so subsequent ValidateToken calls reject it.
	// Pre-fix this handler only cleared the cookie, leaving the token
	// replayable by anyone who captured it (including via Authorization
	// header re-supply).
	//
	// Token sources, in priority order:
	//   1. Authorization: Bearer <jwt> header
	//   2. lsp_session cookie
	//
	// Filtering: API keys (lsp_ prefix) are NOT revoked here. Their
	// lifecycle is /api-keys/:id DELETE; calling RevokeToken on them
	// would return a JWT-parse error which we'd then have to ignore.
	// The router uses the literal "lsp_" prefix to match the chart's
	// default Auth.APIKeyPrefix; operators who change the prefix get
	// best-effort revoke-and-log on API keys, which is harmless.
	//
	// Failure semantics: RevokeToken errors do NOT propagate. Logout
	// must always succeed from the user's perspective; the cookie is
	// cleared and 204 returned regardless. Any revocation failure is
	// logged at Warn for observability.
	rg.POST("/logout", func(c *gin.Context) {
		token := utilities.ExtractToken(c, utilities.TokenExtractorConfig{
			HeaderName: "Authorization",
			TokenType:  "Bearer",
			CookieName: cookieName,
		})
		if token != "" && !utilities.IsAPIKey(token, "lsp_") {
			if err := authSvc.RevokeToken(c.Request.Context(), token); err != nil {
				logger.Warn("auth.logout: RevokeToken failed (proceeding with cookie clear)",
					"error", err.Error())
			}
		}
		c.SetCookie(cookieName, "", -1, "/", cookieDomain, true, true)
		c.Status(http.StatusNoContent)
	})

	// Authenticated: current user info
	meGroup := rg.Group("")
	meGroup.Use(authSvc.AuthMiddleware())
	meGroup.GET("/me", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		user, err := services.GetDatabase().GetUser(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch user"})
			return
		}
		c.JSON(http.StatusOK, user)
	})

	apiKeyGroup := rg.Group("")
	apiKeyGroup.Use(authSvc.AuthMiddleware())
	apiKeyGroup.POST("/api-keys", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthBodyBytes)
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		var req types.CreateAPIKeyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": sanitizeBindError(err)})
			return
		}
		sessionID, _ := c.Get("sessionID")
		sid, _ := sessionID.(string)
		// Epic 56: forward the matched JWT signing key (nil for API-key
		// auth) so CreateAPIKey's DEK-wrapping path can rehydrate from
		// jwt_sessions on Redis miss.
		var matchedKey []byte
		if v, ok := c.Get("jwt_signing_key"); ok {
			matchedKey, _ = v.([]byte)
		}
		apiKey, err := authSvc.CreateAPIKey(c.Request.Context(), userID, req, sid, matchedKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, apiKey)
	})
	apiKeyGroup.GET("/api-keys", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		keys, err := authSvc.ListAPIKeys(c.Request.Context(), userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if keys == nil {
			keys = []*types.APIKey{}
		}
		c.JSON(http.StatusOK, keys)
	})
	apiKeyGroup.DELETE("/api-keys/:id", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if err := authSvc.DeleteAPIKey(c.Request.Context(), userID, c.Param("id")); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	})
}

// registerWorkspaceRoutes registers the workspace List/Create routes on rg
// (no :id, intentionally bypassing WorkspaceAccessMiddleware — List is scoped
// per-user in the service, Create has no target yet) and every /:id route on
// idGroup (which has AuthMiddleware + WorkspaceAccessMiddleware inherited
// from its parent).
//
// All routes require authentication (the group already has auth middleware applied).
//
// proxyHandler may be nil; it is only used to trigger the optional
// session-parent backfill on the /sessions endpoint and is otherwise unused.
func registerWorkspaceRoutes(rg *gin.RouterGroup, idGroup *gin.RouterGroup, services interfaces.Services, proxyHandler *handlers.ProxyHandler, cfg RouterConfig) {
	wsSvc := services.GetWorkspace()
	authSvc := services.GetAuth()

	rg.GET("", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		limit := 20
		offset := 0
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if v := c.Query("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}
		result, err := wsSvc.ListWorkspaces(c.Request.Context(), userID, types.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	rg.POST("", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		// G32 (Epic 17): per-user workspace quota. When the env var
		// LLMSAFESPACES_MAX_WORKSPACES_PER_USER is set to a positive
		// integer, count the user's existing non-deleted workspaces
		// and reject CreateWorkspace if at or above the limit.
		// Default unset = unbounded (single-tenant deployments).
		if maxWS := os.Getenv("LLMSAFESPACES_MAX_WORKSPACES_PER_USER"); maxWS != "" {
			if cap, parseErr := strconv.Atoi(maxWS); parseErr == nil && cap > 0 {
				_, page, err := services.GetDatabase().ListWorkspaces(c.Request.Context(), userID, 1, 0)
				if err == nil && page != nil && page.Total >= cap {
					c.JSON(http.StatusTooManyRequests, gin.H{
						"error": "workspace quota exceeded",
						"limit": cap,
					})
					return
				}
			}
		}

		var req types.CreateWorkspaceRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		ctx := c.Request.Context()
		if sid, exists := c.Get("sessionID"); exists {
			ctx = workspace.ContextWithSessionID(ctx, sid.(string))
		}
		ws, err := wsSvc.CreateWorkspace(ctx, userID, req)
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusCreated, ws)
	})

	idGroup.GET("", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		ws, err := wsSvc.GetWorkspace(c.Request.Context(), userID, c.Param("id"))
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusOK, ws)
	})

	idGroup.PUT("", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		var body struct {
			Name string `json:"name" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		if err := wsSvc.RenameWorkspace(c.Request.Context(), userID, c.Param("id"), body.Name); err != nil {
			respondWithError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	idGroup.DELETE("", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if err := wsSvc.DeleteWorkspace(c.Request.Context(), userID, c.Param("id")); err != nil {
			respondWithError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	idGroup.POST("/suspend", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if err := wsSvc.SuspendWorkspace(c.Request.Context(), userID, c.Param("id")); err != nil {
			respondWithError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	})

	// POST /:id/resume was removed — use POST /:id/activate instead.
	// activate enforces credential injection before transitioning to Resuming,
	// which resume did not. Keeping resume would create pods without credentials.

	// Epic 21 Change A — declarative recovery from Failed (and force-restart
	// from Active). Bumps spec.restartGeneration; controller observes and
	// transitions back through Pending. Idempotent at the spec layer.
	idGroup.POST("/restart", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		restartCtx := c.Request.Context()
		if sid, exists := c.Get("sessionID"); exists {
			restartCtx = workspace.ContextWithSessionID(restartCtx, sid.(string))
		}
		if err := wsSvc.RestartWorkspace(restartCtx, userID, c.Param("id")); err != nil {
			respondWithError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	})

	// Refresh Compute: re-sync the workspace CRD with the platform's current
	// defaults (resources, security level, storage class, max active sessions)
	// and bump spec.restartGeneration so the controller rebuilds the pod,
	// re-resolving spec.runtime to its latest image version.
	idGroup.POST("/refresh-compute", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		ctx := c.Request.Context()
		if sid, exists := c.Get("sessionID"); exists {
			ctx = workspace.ContextWithSessionID(ctx, sid.(string))
		}
		resp, err := wsSvc.RefreshWorkspaceCompute(ctx, userID, c.Param("id"))
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, resp)
	})

	// Epic 27a: explicit agent reload (disposes opencode without pod restart).
	if cfg.AgentReloadHandler != nil {
		idGroup.POST("/agent/reload", cfg.AgentReloadHandler.Reload)
	}

	idGroup.GET("/status", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		status, err := wsSvc.GetWorkspaceStatus(c.Request.Context(), userID, c.Param("id"))
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusOK, status)
	})

	idGroup.POST("/activate", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		ctx := c.Request.Context()
		if sid, exists := c.Get("sessionID"); exists {
			ctx = workspace.ContextWithSessionID(ctx, sid.(string))
		}
		resp, err := wsSvc.ActivateWorkspace(ctx, userID, c.Param("id"))
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	})

	idGroup.GET("/sessions", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		workspaceID := c.Param("id")
		sessions, err := wsSvc.ListWorkspaceSessions(c.Request.Context(), userID, workspaceID)
		if err != nil {
			respondWithError(c, err)
			return
		}
		// Trigger a one-shot async backfill of parent_session_id from the
		// authoritative opencode /session list. No-op when the workspace
		// has already been backfilled this process lifetime, so the steady-
		// state cost is a single map lookup per request. Useful for
		// sessions that pre-date the parent_session_id migration.
		// Skipped when proxyHandler is nil (router built without proxy).
		if proxyHandler != nil {
			proxyHandler.BackfillSessionParents(c.Request.Context(), workspaceID)
			activeIDs := proxyHandler.GetActiveSessions(c.Request.Context(), workspaceID)
			if len(activeIDs) > 0 {
				activeSet := make(map[string]struct{}, len(activeIDs))
				for _, id := range activeIDs {
					activeSet[id] = struct{}{}
				}
				for i := range sessions {
					if _, ok := activeSet[sessions[i].ID]; ok {
						sessions[i].Status = "active"
					}
				}
			}
		}
		c.JSON(http.StatusOK, sessions)
	})

	idGroup.POST("/sessions/new", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		resp, err := wsSvc.EnsureSession(c.Request.Context(), userID, c.Param("id"))
		if err != nil {
			respondWithError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	})

	idGroup.PUT("/sessions/:sessionId/title", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		var body struct {
			Title string `json:"title" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "title is required"})
			return
		}
		wsID := c.Param("id")
		sID := c.Param("sessionId")
		if err := wsSvc.RenameSession(c.Request.Context(), userID, wsID, sID, body.Title); err != nil {
			respondWithError(c, err)
			return
		}
		// Also rename in the opencode agent so the frontend's periodic title
		// fetch (useSessionTitle hook) doesn't retrieve the old agent-side
		// title and overwrite the user-assigned one.
		if proxyHandler != nil {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := proxyHandler.RenameSessionInAgent(ctx, wsID, sID, body.Title); err != nil {
					// Log-only: the DB rename succeeded; agent rename is best-effort
					log.Printf("RenameSessionInAgent failed for session %s: %v", sID, err)
				}
			}()
		}
		c.Status(http.StatusNoContent)
	})

	idGroup.PUT("/sessions/:sessionId/seen", func(c *gin.Context) {
		userID := authSvc.GetUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if err := wsSvc.MarkSessionSeen(c.Request.Context(), userID, c.Param("id"), c.Param("sessionId")); err != nil {
			respondWithError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	// Agent customization: workspace-level prompt + role selection.
	// Registered on idGroup so WorkspaceAccessMiddleware runs first.
	if cfg.PromptHandler != nil {
		idGroup.GET("/prompt", cfg.PromptHandler.GetWorkspacePrompt)
		idGroup.PUT("/prompt", cfg.PromptHandler.SetWorkspacePrompt)
	}
	if cfg.AgentRoleHandler != nil {
		idGroup.GET("/agent-role", cfg.AgentRoleHandler.GetWorkspaceRole)
		idGroup.PUT("/agent-role", cfg.AgentRoleHandler.SetWorkspaceRole)
		idGroup.DELETE("/agent-role", cfg.AgentRoleHandler.ClearWorkspaceRole)
		idGroup.GET("/effective-agent-role", cfg.AgentRoleHandler.GetEffectiveWorkspaceRole)
	}
}

// registerProxyRoutes adds all /api/v1/workspaces/:id proxy routes on the
// provided idGroup. idGroup already has AuthMiddleware + WorkspaceAccessMiddleware
// applied, so every proxy handler inherits the single ownership gate.
func registerProxyRoutes(idGroup *gin.RouterGroup, proxyHandler *handlers.ProxyHandler) {
	idGroup.POST("/sessions/:sessionId/message", proxyHandler.SendMessage)
	idGroup.POST("/sessions/:sessionId/prompt", proxyHandler.SendPromptAsync)
	idGroup.POST("/sessions/:sessionId/queue", proxyHandler.EnqueueMessage)
	idGroup.GET("/sessions/:sessionId/queue", proxyHandler.ListQueue)
	idGroup.DELETE("/sessions/:sessionId/queue/:messageId", proxyHandler.DeleteQueueMessage)
	idGroup.GET("/sessions/:sessionId/message", proxyHandler.GetHistory)
	idGroup.GET("/sessions/:sessionId", proxyHandler.GetSession)
	idGroup.POST("/sessions/:sessionId/abort", proxyHandler.AbortSession)
	idGroup.DELETE("/sessions/:sessionId", proxyHandler.DeleteSession)
	idGroup.GET("/session-events", proxyHandler.StreamEvents)

	// Question/Permission input request routes (Epic 16)
	idGroup.GET("/question", proxyHandler.ListQuestions)
	idGroup.POST("/question/:requestID/reply", proxyHandler.QuestionReply)
	idGroup.POST("/question/:requestID/reject", proxyHandler.QuestionReject)
	idGroup.GET("/permission", proxyHandler.ListPermissions)
	idGroup.POST("/permission/:requestID/reply", proxyHandler.PermissionReply)
}

// respondWithError maps API errors to HTTP responses. It uses errors.As to
// find an *apierrors.APIError or a *pkgerrors.StatusError anywhere in the
// error chain (handles both direct returns and fmt.Errorf-wrapped errors),
// falling back to a duck-typed StatusCode() check, then 500 for plain errors.
func respondWithError(c *gin.Context, err error) {
	var apiErr *apierrors.APIError
	if errors.As(err, &apiErr) {
		c.JSON(apiErr.StatusCode(), gin.H{"error": apiErr.Error()})
		return
	}
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		c.JSON(statusErr.StatusCode(), gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// registerSettingsRoutes adds admin and user settings routes.
func registerSettingsRoutes(router *gin.Engine, services interfaces.Services, h *handlers.SettingsHandler) {
	authMW := services.GetAuth().AuthMiddleware()

	// Admin settings (authenticated + admin guard)
	admin := router.Group("/api/v1/admin/settings")
	admin.Use(authMW)
	admin.Use(middleware.AdminGuard())
	admin.GET("", h.GetAdminSettings)
	admin.GET("/schema", h.GetAdminSettingsSchema)
	admin.PUT("/:key", h.SetAdminSetting)

	// User settings (authenticated)
	user := router.Group("/api/v1/users/me/settings")
	user.Use(authMW)
	user.GET("", h.GetUserSettings)
	user.GET("/schema", h.GetUserSettingsSchema)
	user.PUT("/:key", h.SetUserSetting)
}

// registerCredentialRoutes adds admin credential set CRUD routes.
// getMaxActiveSessions reads the max active sessions setting, falling back to 5.
func getMaxActiveSessions(ctx context.Context, instanceSettings *settings.InstanceService) int {
	if instanceSettings != nil {
		if v, err := instanceSettings.GetInt(ctx, settings.KeyWorkspaceDefaultMaxActiveSessions.Name()); err == nil && v > 0 {
			return v
		}
	}
	return 5
}

// registerOrgRoutes adds all /api/v1/orgs routes.
func registerOrgRoutes(router *gin.Engine, services interfaces.Services, h *handlers.OrgsHandler, credH *handlers.OrgCredentialsHandler, invH *handlers.InvitationsHandler, polH *handlers.PolicyHandler, promptH *handlers.PromptHandler, roleH *handlers.AgentRoleHandler, audH *handlers.AuditHandler, ssoH *handlers.SSOHandler) {
	authMW := services.GetAuth().AuthMiddleware()

	orgGroup := router.Group("/api/v1/orgs")
	orgGroup.Use(authMW)
	orgGroup.POST("", h.Create)
	orgGroup.GET("", h.List)

	orgIDGroup := orgGroup.Group("/:id")
	orgIDGroup.Use(middleware.OrgMemberGuard(h))
	orgIDGroup.GET("", h.Get)
	orgIDGroup.GET("/workspaces", h.ListWorkspaces)
	orgIDGroup.GET("/members", h.ListMembers)
	if invH != nil {
		orgIDGroup.GET("/invitations", invH.List)
	}

	orgAdminGroup := orgGroup.Group("/:id")
	orgAdminGroup.Use(middleware.OrgAdminGuard(h))
	orgAdminGroup.PUT("", h.Update)
	orgAdminGroup.DELETE("", h.Delete)
	orgAdminGroup.POST("/members", h.AddMember)
	orgAdminGroup.DELETE("/members/:userID", h.RemoveMember)
	orgAdminGroup.PUT("/members/:userID", h.ChangeMemberRole)
	orgAdminGroup.POST("/members/:userID/verify", h.VerifyMember)
	orgAdminGroup.POST("/billing/checkout", h.Checkout)
	orgAdminGroup.POST("/billing/portal", h.Portal)
	if invH != nil {
		orgAdminGroup.POST("/invitations", invH.Create)
		orgAdminGroup.DELETE("/invitations/:invID", invH.Delete)
		orgAdminGroup.POST("/invitations/:invID/resend", invH.Resend)
		// Force-verify the user account associated with a pending
		// invitation (epic-43 follow-up — the invitee already has an
		// unverified users row; this admin override sets
		// users.email_verified=true so they can log in. The invitation
		// itself stays pending — the user must still click the link
		// to accept and join the org).
		orgAdminGroup.POST("/invitations/:invID/verify-user", invH.VerifyUserForInvitation)
	}

	if credH != nil {
		orgAdminGroup.POST("/credentials", credH.Create)
		orgAdminGroup.GET("/credentials", credH.List)
		orgAdminGroup.PUT("/credentials/:credID", credH.Update)
		orgAdminGroup.DELETE("/credentials/:credID", credH.Delete)
		orgAdminGroup.GET("/credentials/:credID/models", credH.ProbeModels)
		orgAdminGroup.POST("/credentials/:credID/auto-apply", credH.CreateAutoApply)
		orgAdminGroup.GET("/credentials/:credID/auto-apply", credH.ListAutoApply)
		orgAdminGroup.DELETE("/credentials/:credID/auto-apply", credH.DeleteAutoApply)
	}

	if polH != nil {
		orgAdminGroup.GET("/policies", polH.Get)
		// Feature-gated policy mutations (Business+ per billing.PlanTiers).
		// Reads remain open so members can see what's enforced; writes
		// require the plan to include the policy feature.
		featurePolicy := orgAdminGroup.Group("", middleware.FeatureGuard(h, "policies"))
		featurePolicy.PUT("/policies/:key", polH.Put)
		featurePolicy.DELETE("/policies/:key", polH.Delete)
	}

	if promptH != nil {
		orgIDGroup.GET("/prompt", promptH.GetOrg)
		orgAdminGroup.PUT("/prompt", promptH.SetOrg)
	}

	if roleH != nil {
		orgAdminGroup.GET("/agent-roles", roleH.ListOrg)
		orgAdminGroup.POST("/agent-roles", roleH.CreateOrg)
		orgAdminGroup.GET("/agent-roles/:roleId", roleH.GetOrg)
		orgAdminGroup.PUT("/agent-roles/:roleId", roleH.UpdateOrg)
		orgAdminGroup.DELETE("/agent-roles/:roleId", roleH.DeleteOrg)
	}

	if audH != nil {
		// Audit log access requires Business+ plan (per billing.PlanTiers).
		orgAdminGroup.GET("/audit", middleware.FeatureGuard(h, "audit"), audH.List)
	}

	// US-43.10: org-admin SSO config CRUD + domain verification (D17 Q-S2).
	if ssoH != nil {
		orgAdminGroup.GET("/sso", ssoH.Get)
		orgAdminGroup.PUT("/sso", ssoH.Put)
		orgAdminGroup.DELETE("/sso", ssoH.Delete)
		orgAdminGroup.POST("/sso/domains/:domain/verify", ssoH.VerifyDomain)
		orgAdminGroup.POST("/sso/verification-token/rotate", ssoH.RotateToken)
	}

	// Public invitation routes (token is the credential).
	if invH != nil {
		publicInv := router.Group("/api/v1/invitations")
		publicInv.GET("/:token", invH.GetByToken)
		authedInv := publicInv.Group("", authMW)
		authedInv.POST("/:token/accept", invH.Accept)
		authedInv.POST("/:token/decline", invH.Decline)
	}
}
