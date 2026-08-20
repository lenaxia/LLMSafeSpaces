// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 66 Phase 1 (redesign-2026-08-19/DESIGN.md §4–5): per-workspace
// preview origins. Preview traffic moves OFF the API origin onto
// <workspace-uuid>-preview.<baseDomain>, killing the T1 threat (same-origin
// credentialed API calls from prompt-injected preview JS) by architecture:
// the platform session cookie is host-scoped to the API host and never
// reaches preview hosts; preview hosts hold nothing but the throwaway
// __Host-pv cookie.
//
// Flow:
//
//	tool/dev URL → GET api…/workspaces/:id/dev-preview/bootstrap/:port
//	              (existing AuthMiddleware + WorkspaceAccessMiddleware —
//	              ownership enforced where it already is)
//	            → 302 https://<ws>-preview.<baseDomain>/<port>/?t=<token>
//	            → preview host validates one-time HMAC token (jti consumed
//	              via cache SetNX), sets __Host-pv, 303 → clean path
//	            → subsequent requests authenticate via __Host-pv only
//
// The preview pipeline proxies through the SAME agentd path as the
// path-based handler (inner *DevPreviewHandler), inheriting P0-1 (edge
// no-store on HTML), P0-2 (WS forwarding), G34 header hygiene, the
// response size cap, and the per-workspace connection cap.
//
// Port policy on preview hosts is INDISTINGUISHABLE-FROM-DEAD (THREAT-MODEL
// T3): blocklisted ports return the same status and body as ports with
// nothing listening, so the preview path is not a port-scanner oracle.
// Denial reasons go to server logs only.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// PreviewOriginConfig is operator config for the preview-origin pipeline
// (config.PreviewOrigin → env LLMSAFESPACES_PREVIEW_ORIGIN_*).
type PreviewOriginConfig struct {
	Enabled bool
	// BaseDomain is the registrable domain preview hosts live under;
	// preview hosts are <uuid>-preview.<BaseDomain>.
	BaseDomain string
	// APIOriginURL is the externally reachable API origin for bootstrap
	// links on the landing page. Empty → derived as
	// "https://api.<BaseDomain>" (the standard deployment shape).
	APIOriginURL string
	// TokenSecret is the HMAC-SHA256 key for bootstrap tokens and the
	// __Host-pv cookie. Required when Enabled (fail-closed at config
	// load, mirrors the Turnstile guard).
	TokenSecret []byte
	// TokenTTL bounds the bootstrap token lifetime (default 5m).
	TokenTTL time.Duration
	// CookieTTL bounds the preview session cookie (default 7d). The
	// workspace-active check runs per request, so suspend kills access
	// regardless of cookie lifetime.
	CookieTTL time.Duration
}

const previewCookieName = "__Host-pv"

// previewTokenPayload is the signed body of a bootstrap token.
type previewTokenPayload struct {
	Ws   string `json:"ws"`
	Port int    `json:"port"`
	Exp  int64  `json:"exp"`
	Jti  string `json:"jti"`
}

// previewCookiePayload is the signed body of the __Host-pv cookie.
// Workspace-scoped (not port-scoped): ports within a workspace are one
// trust domain (DESIGN §4 "URL scheme").
type previewCookiePayload struct {
	Ws  string `json:"ws"`
	Exp int64  `json:"exp"`
}

// PreviewOriginHandler serves <ws>-preview.<baseDomain> hosts. It wraps
// the path-based DevPreviewHandler to reuse its proxy machinery, gates,
// and caps.
type PreviewOriginHandler struct {
	inner *DevPreviewHandler
	cfg   PreviewOriginConfig
	cache CacheStore
	log   pkginterfaces.LoggerInterface
}

// CacheStore is the subset of interfaces.CacheService used here (jti
// one-time consumption via SetNX).
type CacheStore interface {
	SetNX(ctx context.Context, key, value string, expiration time.Duration) (bool, error)
}

// uuidRE matches canonical lowercase UUIDs (workspace names).
var previewUUIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewPreviewOriginHandler constructs the handler. inner must be the same
// DevPreviewHandler instance the path-based route uses.
func NewPreviewOriginHandler(inner *DevPreviewHandler, cfg PreviewOriginConfig, cache CacheStore, log pkginterfaces.LoggerInterface) *PreviewOriginHandler {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 5 * time.Minute
	}
	if cfg.CookieTTL <= 0 {
		cfg.CookieTTL = 7 * 24 * time.Hour
	}
	return &PreviewOriginHandler{inner: inner, cfg: cfg, cache: cache, log: log}
}

// PreviewHost extracts the workspace ID when host is a preview host
// (<uuid>-preview.<BaseDomain>). ok=false means "not a preview host —
// pass through to the API router" (which includes every other subdomain
// of BaseDomain, e.g. api. and the apex).
func (h *PreviewOriginHandler) PreviewHost(host string) (wsID string, ok bool) {
	host = hostOnly(host)
	suffix := "-preview." + h.cfg.BaseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	if !previewUUIDRe.MatchString(label) {
		return "", false
	}
	return label, true
}

// IsMalformedPreviewHost reports whether host ends in -preview.<BaseDomain>
// but the label before it is not a canonical workspace UUID (e.g.
// garbage-preview.…). These get 421 rather than falling through to API
// routes: they are ours to answer and answer misleadingly we must not.
func (h *PreviewOriginHandler) IsMalformedPreviewHost(host string) bool {
	host = hostOnly(host)
	suffix := "-preview." + h.cfg.BaseDomain
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	_, ok := h.PreviewHost(host)
	return !ok
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// Middleware is the engine-level interceptor. Register BEFORE the API
// middleware chain (Security/CORS/RateLimit): preview hosts must not
// receive the API CORS policy, consume the API rate budget, or match any
// /api/* route (T5). Non-preview hosts pass through untouched.
func (h *PreviewOriginHandler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		host := hostOnly(c.Request.Host)
		if !strings.HasSuffix(host, "-preview."+h.cfg.BaseDomain) {
			return // not preview-shaped at all — API traffic
		}
		wsID, ok := h.PreviewHost(host)
		if !ok {
			c.AbortWithStatus(http.StatusMisdirectedRequest)
			return
		}
		h.servePreview(c, wsID)
	}
}

// servePreview always aborts the request.
func (h *PreviewOriginHandler) servePreview(c *gin.Context, wsID string) {
	if !h.cfg.Enabled || h.inner == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// T5: preview hosts never serve API routes — enforced by path shape:
	// everything must be /<port>[/<subpath>]. /api/* fails port parsing
	// below and is indistinguishable from any other invalid port.
	portStr := strings.TrimPrefix(c.Request.URL.Path, "/")
	subPath := "/"
	if idx := strings.Index(portStr, "/"); idx >= 0 {
		subPath = portStr[idx:]
		portStr = portStr[:idx]
	}
	port, err := strconv.Atoi(portStr)

	// Human front door (UX): a bare navigation to the preview host root
	// (hostname typed/bookmarked without a port) serves the landing page
	// instead of the indistinguishable 502. Authenticated users get it
	// too — the landing's form is how they pick a port.
	if c.Request.URL.Path == "/" && c.Request.Method == http.MethodGet &&
		(c.GetHeader("Sec-Fetch-Mode") == "navigate" || h.hasValidPreviewCookie(c, wsID)) {
		h.serveLanding(c, wsID, 0)
		return
	}

	// Indistinguishable-from-dead (T3): invalid/blocklisted/dead ports all
	// answer EXACTLY what the proxy's ErrorHandler answers for a dead
	// agentd hop — same status AND body — so the preview path is not a
	// port-scanner oracle. Real reason → server log only.
	unreachable := func(reason string) {
		if h.log != nil {
			h.log.Info("preview-origin: refused", "host", c.Request.Host, "path", c.Request.URL.Path, "reason", reason)
		}
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.Header("X-Content-Type-Options", "nosniff")
		c.String(http.StatusBadGateway, "workspace dev-preview endpoint unreachable\n")
	}

	if err != nil || port < 1 || port > 65535 {
		unreachable("port not numeric or out of range")
		return
	}
	if port < 1024 {
		unreachable("privileged port")
		return
	}
	if _, denied := devPreviewDeniedAPorts[port]; denied {
		unreachable("blocklisted port")
		return
	}

	// Authentication: __Host-pv cookie, else one-time ?t redemption.
	// An invalid/expired/mismatched cookie falls through to the token (or
	// landing) branches — never proxies.
	if cookie, cerr := c.Cookie(previewCookieName); cerr == nil && cookie != "" {
		if cp, ok := h.verifyCookie(cookie); ok && cp.Ws == wsID {
			h.proxyAuthenticated(c, wsID, port, subPath)
			return
		}
	}

	tok := c.Query("t")
	if tok == "" {
		// Human front door: unauthenticated BROWSER NAVIGATIONS (address
		// bar, bookmark, link click — Sec-Fetch-Mode: navigate, which
		// fetch()/XHR cannot set) get the landing page with a one-click
		// bootstrap link for this port, instead of a bare 401. Everything
		// else (API clients, subresources, curl) keeps the 401. The
		// landing contains no secrets: the workspace UUID is already in
		// the hostname, the port in the path.
		if c.Request.Method == http.MethodGet && c.GetHeader("Sec-Fetch-Mode") == "navigate" {
			h.serveLanding(c, wsID, port)
			return
		}
		h.unauthorized(c)
		return
	}
	tp, ok := h.verifyToken(tok)
	if !ok || tp.Ws != wsID || tp.Port != port {
		h.unauthorized(c)
		return
	}
	if !h.consumeJti(tp) {
		h.unauthorized(c)
		return
	}

	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(previewCookieName, h.signCookie(&previewCookiePayload{
		Ws:  wsID,
		Exp: time.Now().Add(h.cfg.CookieTTL).Unix(),
	}), int(h.cfg.CookieTTL.Seconds()), "/", "", true, true)
	// Same-origin redirect to drop ?t from the URL; never cached.
	c.Header("Cache-Control", "no-store")
	c.Header("Location", "/"+strconv.Itoa(port)+subPath)
	c.AbortWithStatus(http.StatusSeeOther)
}

func (h *PreviewOriginHandler) unauthorized(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatus(http.StatusUnauthorized)
}

// hasValidPreviewCookie reports whether the request carries a signature-
// valid, unexpired __Host-pv cookie bound to this workspace.
func (h *PreviewOriginHandler) hasValidPreviewCookie(c *gin.Context, wsID string) bool {
	cookie, err := c.Cookie(previewCookieName)
	if err != nil || cookie == "" {
		return false
	}
	cp, ok := h.verifyCookie(cookie)
	return ok && cp.Ws == wsID
}

// apiOrigin returns the externally reachable API origin for bootstrap
// links. Explicitly configured APIOriginURL wins (deployments where the
// API lives off the api.<baseDomain> convention); otherwise derived.
func (h *PreviewOriginHandler) apiOrigin() string {
	if h.cfg.APIOriginURL != "" {
		return h.cfg.APIOriginURL
	}
	return "https://api." + h.cfg.BaseDomain
}

// serveLanding renders the human front door of a preview origin.
//
// port > 0: straight to the bootstrap link for that port (deep-link case:
// cookie absent/expired on /<port>/…).
// port == 0 (bare host root): a no-JS GET form collecting the port; the
// form submits to the bootstrap's ?port= query form.
//
// Content policy (load-bearing): STATIC HTML + CSS only — no scripts, no
// credentials, no secrets. The workspace UUID is already in the hostname
// and the port in the URL, so revealing them here leaks nothing to the
// visitor. This is the ONLY unauthenticated HTML the preview origin ever
// serves; it must never ask for or display a password — the origin serves
// agent-authored, prompt-injectable content behind this door, and a login
// look-alike here would train users into credential phishing. Identity is
// proven exclusively on the API origin (which never serves user content)
// via the bootstrap redirect.
func (h *PreviewOriginHandler) serveLanding(c *gin.Context, wsID string, port int) {
	short := wsID
	if len(short) > 8 {
		short = short[:8] + "…"
	}
	bootstrapBase := h.apiOrigin() + "/api/v1/workspaces/" + wsID + "/dev-preview-bootstrap"

	// The port is ALWAYS editable (prefilled from the URL when present,
	// else the 5173 default): switching ports must not require editing
	// the address bar.
	portVal := 5173
	if port > 0 {
		portVal = port
	}
	cta := `<form class="card" method="get" action="` + bootstrapBase + `">` +
		`<label for="port">Dev server port</label>` +
		`<input id="port" name="port" type="number" min="1024" max="65535" value="` + strconv.Itoa(portVal) + `" required>` +
		`<button class="btn" type="submit">Open preview&nbsp;→</button></form>`

	body := `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Workspace ` + short + ` — dev preview</title>
<style>
:root{color-scheme:dark}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0f172a;color:#e2e8f0;
 font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;line-height:1.6}
main{max-width:34rem;padding:2.5rem 1.5rem;text-align:center}
h1{font-size:1.05rem;color:#7dd3fc;margin:0 0 .25rem}
.ws{color:#94a3b8;font-size:.85rem;margin-bottom:1.75rem}
.card{background:rgba(30,41,59,.65);border:1px solid rgba(148,163,184,.2);border-radius:10px;
 padding:1.25rem 1.5rem;margin:0 auto 1.5rem;display:inline-flex;flex-direction:column;gap:.6rem}
label{color:#94a3b8;font-size:.8rem}
input{background:#0b1120;color:#e2e8f0;border:1px solid rgba(148,163,184,.3);border-radius:6px;
 padding:.5rem .75rem;font:inherit;text-align:center;width:9rem}
.btn{display:inline-block;background:#0ea5e9;color:#fff;text-decoration:none;border:none;border-radius:8px;
 padding:.65rem 1.6rem;font:inherit;font-weight:600;cursor:pointer}
.btn:hover{background:#0284c7}
.note{color:#64748b;font-size:.75rem;margin-top:1.5rem}
details.how{text-align:left;margin:1.5rem auto 0;max-width:30rem;color:#94a3b8;font-size:.8rem}
details.how summary{cursor:pointer;color:#7dd3fc;font-size:.8rem}
details.how ul{margin:.6rem 0 0 1.1rem;padding:0;display:grid;gap:.45rem}
</style></head><body><main>
<h1>Workspace dev preview</h1>
<div class="ws">` + short + ` · this preview's session is missing or expired</div>
` + cta + `
<details class="how"><summary>How dev preview works</summary>
<ul>
<li>Your dev server runs inside the workspace; this origin tunnels to it over HTTPS — no ports exposed, no NetworkPolicy changes.</li>
<li>Access is workspace-owner-only. The button opens via the API origin (where your login lives); it hands this origin a one-time ticket that becomes a 7-day preview session for this browser.</li>
<li>While a session is active this URL serves your app directly, including WebSocket hot-reload.</li>
<li>Nothing on this page asks for a password — that is by design: workspace content is agent-generated, so credentials are only ever entered on the API origin.</li>
</ul></details>
<p class="note">Opens via the API origin — you must be logged in as this workspace's owner.<br>
No password is ever asked on this page, by design.</p>
</main></body></html>`

	c.Header("Cache-Control", "no-store")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
	c.Abort()
}

// proxyAuthenticated runs the same gates as the path-based handler
// (workspace active, dev-preview opt-in, password, connection cap) and
// then proxies via the shared machinery.
func (h *PreviewOriginHandler) proxyAuthenticated(c *gin.Context, wsID string, port int, subPath string) {
	workspace, err := h.inner.wsGetter.GetWorkspace(c.Request.Context(), wsID)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if workspace.Status.Phase != v1.WorkspacePhaseActive || workspace.Status.PodIP == "" {
		c.Header("Retry-After", "10")
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if workspace.Spec.NetworkAccess == nil || !workspace.Spec.NetworkAccess.DevPreview {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if !h.inner.acquireConnection(wsID) {
		c.Header("Retry-After", "10")
		c.AbortWithStatus(http.StatusTooManyRequests)
		return
	}
	defer h.inner.releaseConnection(wsID)

	password, err := h.inner.pwProvider.WorkspacePassword(c.Request.Context(), wsID)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	h.inner.proxyToAgentd(c, workspace, port, subPath, password)
}

// HandleBootstrap is registered on the API origin inside the existing
// idGroup chain (AuthMiddleware + WorkspaceAccessMiddleware): the caller
// is an authenticated owner (T7-verified middleware does the check — this
// handler never sees strangers). It mints a one-time token and redirects
// to the preview host.
func (h *PreviewOriginHandler) HandleBootstrap(c *gin.Context) {
	if !h.cfg.Enabled || h.inner == nil || !h.inner.config.Enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dev preview is disabled"})
		return
	}
	wsID := c.Param("id")
	if !previewUUIDRe.MatchString(wsID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return
	}
	// Path form (/dev-preview-bootstrap/:port) is primary; the ?port=
	// query form backs the landing page's no-JS form (bare host root,
	// where the port is user-supplied).
	portParam := c.Param("port")
	if portParam == "" {
		portParam = c.Query("port")
	}
	port, err := strconv.Atoi(portParam)
	if err != nil || port < 1024 || port > 65535 {
		// Indistinguishable-from-dead applies to the unauthenticated
		// preview surface; bootstrap is owner-authenticated, so a plain
		// 400 with no topology detail is fine here.
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid port"})
		return
	}
	if _, denied := devPreviewDeniedAPorts[port]; denied {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid port"})
		return
	}

	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	tok := h.signToken(&previewTokenPayload{
		Ws:   wsID,
		Port: port,
		Exp:  time.Now().Add(h.cfg.TokenTTL).Unix(),
		Jti:  hex.EncodeToString(jti),
	})

	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, fmt.Sprintf("https://%s-preview.%s/%d/?t=%s",
		wsID, h.cfg.BaseDomain, port, tok))
}

// --- signing ---

func (h *PreviewOriginHandler) mac(data []byte) []byte {
	m := hmac.New(sha256.New, h.cfg.TokenSecret)
	m.Write(data)
	return m.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (h *PreviewOriginHandler) signToken(p *previewTokenPayload) string {
	body, _ := json.Marshal(p)
	return "v1." + b64(body) + "." + b64(h.mac(body))
}

func (h *PreviewOriginHandler) verifyToken(tok string) (*previewTokenPayload, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, false
	}
	if !hmac.Equal(sig, h.mac(body)) {
		return nil, false
	}
	var p previewTokenPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false
	}
	if time.Now().Unix() > p.Exp {
		return nil, false
	}
	return &p, true
}

func (h *PreviewOriginHandler) signCookie(p *previewCookiePayload) string {
	body, _ := json.Marshal(p)
	return "v1." + b64(body) + "." + b64(h.mac(body))
}

func (h *PreviewOriginHandler) verifyCookie(v string) (*previewCookiePayload, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return nil, false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, false
	}
	if !hmac.Equal(sig, h.mac(body)) {
		return nil, false
	}
	var p previewCookiePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false
	}
	if time.Now().Unix() > p.Exp {
		return nil, false
	}
	return &p, true
}

// consumeJti enforces one-time bootstrap tokens via cache SetNX.
// Duplicate (already-consumed) → false, fail-CLOSED. Cache infrastructure
// error → true, fail-OPEN with a warning: availability over strict
// one-timeness during a cache outage; the token TTL still bounds replay.
func (h *PreviewOriginHandler) consumeJti(p *previewTokenPayload) bool {
	if h.cache == nil {
		return true // no store wired (tests); TTL is the only bound
	}
	ok, err := h.cache.SetNX(context.Background(), "devpreview:pvo:jti:"+p.Jti, "1", h.cfg.TokenTTL)
	if err != nil {
		if h.log != nil {
			h.log.Warn("preview-origin: jti store error, failing open", "error", err.Error())
		}
		return true
	}
	return ok
}
