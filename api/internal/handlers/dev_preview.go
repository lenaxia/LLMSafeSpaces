// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 66: Dev Preview — API-side handler.
//
// GET /api/v1/workspaces/:id/dev-preview/:port/* — proxies to
// http://<podIP>:<agentdPort>/v1/dev-preview/:port/* which agentd
// forwards to localhost:<port>.
//
// Auth: inherits AuthMiddleware (JWT cookie or Bearer header) +
// WorkspaceAccessMiddleware (ownership check) from idGroup. No new auth.
//
// The handler enforces: opt-in (spec.networkAccess.devPreview), port
// denylist, per-workspace connection cap, response size cap, and
// operator kill-switch. It injects Basic auth for the agentd call.
//
// G34: only an allowlist of client headers is forwarded to the pod
// (Content-Type, Accept, X-Request-ID). The caller's Cookie, Origin,
// Referer, and Authorization are stripped — they describe the caller's
// relationship with the API server, not with the tenant pod. Same
// invariant as proxy.go's doProxy (proxy_helpers.go:copyRequestHeaders).

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// DevPreviewConfig holds operator-configurable settings.
type DevPreviewConfig struct {
	// Enabled is the global kill-switch. When false, all dev-preview
	// requests return 503 regardless of the per-workspace CRD flag.
	Enabled bool
	// MaxResponseBytes caps the response body (default 50 MiB).
	MaxResponseBytes int64
	// MaxConnsPerWorkspace caps concurrent dev-preview connections
	// per workspace (default 50), separate from the agent-session budget.
	MaxConnsPerWorkspace int
}

// DevPreviewHandler proxies authenticated HTTP/WS requests to in-workspace
// dev servers via agentd on port 4097.
type DevPreviewHandler struct {
	wsGetter   WorkspaceGetter
	pwProvider WorkspacePasswordProvider
	namespace  string
	logger     pkginterfaces.LoggerInterface
	config     DevPreviewConfig
	agentdPort string // overridable for tests; defaults to "4097"
	transport  http.RoundTripper
	connCount  map[string]int
	connMu     sync.Mutex
}

// WorkspacePasswordProvider is the password-provider interface (mirrors
// ProxyHandler.WorkspacePassword). Implemented by ProxyHandler in
// production; the dev-preview handler shares the password cache via
// this interface rather than duplicating the K8s Secret fetch path.
type WorkspacePasswordProvider interface {
	WorkspacePassword(ctx context.Context, workspaceID string) (string, error)
}

// NewDevPreviewHandler constructs the handler. The agentdPort field
// defaults to "4097" and is overridable only for tests.
func NewDevPreviewHandler(
	wsGetter WorkspaceGetter,
	pwProvider WorkspacePasswordProvider,
	namespace string,
	logger pkginterfaces.LoggerInterface,
	config DevPreviewConfig,
) *DevPreviewHandler {
	port := "4097"
	return &DevPreviewHandler{
		wsGetter:   wsGetter,
		pwProvider: pwProvider,
		namespace:  namespace,
		logger:     logger,
		config:     config,
		agentdPort: port,
		transport: &http.Transport{
			MaxIdleConnsPerHost: 100,
		},
		connCount: make(map[string]int),
	}
}

var devPreviewDeniedAPorts = map[int]string{
	4096: "opencode (4096)",
	4097: "agentd user mux (4097)",
	4098: "agentd admin mux (4098)",
}

// HandleDevPreview is the gin handler for
// GET /api/v1/workspaces/:id/dev-preview/:port/*.
func (h *DevPreviewHandler) HandleDevPreview(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}

	if !h.config.Enabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dev preview is disabled on this instance"})
		return
	}

	portPath := c.Param("portPath")
	portStr := strings.TrimPrefix(portPath, "/")
	subPath := "/"
	if idx := strings.Index(portStr, "/"); idx >= 0 {
		subPath = portStr[idx:]
		portStr = portStr[:idx]
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "port must be numeric"})
		return
	}
	if port < 1 || port > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("port out of range: %d", port)})
		return
	}
	if port < 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("privileged port denied: %d", port)})
		return
	}
	if reason, denied := devPreviewDeniedAPorts[port]; denied {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("port denied: %s", reason)})
		return
	}

	if h.wsGetter == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dev preview not available"})
		return
	}

	workspace, err := h.wsGetter.GetWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	if workspace.Status.Phase != v1.WorkspacePhaseActive || workspace.Status.PodIP == "" {
		c.Header("Retry-After", "10")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "workspace not ready",
			"phase": workspace.Status.Phase,
		})
		return
	}

	if workspace.Spec.NetworkAccess == nil || !workspace.Spec.NetworkAccess.DevPreview {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dev preview not enabled for this workspace"})
		return
	}

	password, err := h.pwProvider.WorkspacePassword(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace credentials"})
		return
	}

	if !h.acquireConnection(workspaceID) {
		c.Header("Retry-After", "10")
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":      "dev preview connection limit reached",
			"retryAfter": 10,
		})
		return
	}
	defer h.releaseConnection(workspaceID)

	h.proxyToAgentd(c, workspace, port, subPath, password)
}

func (h *DevPreviewHandler) proxyToAgentd(c *gin.Context, workspace *v1.Workspace, port int, subPath, password string) {
	podIP := workspace.Status.PodIP
	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(podIP, h.agentdPort),
	}

	basicAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))
	portStr := strconv.Itoa(port)
	agentdPath := "/v1/dev-preview/" + portStr + subPath

	maxBytes := h.config.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 50 * 1024 * 1024
	}

	proxy := &httputil.ReverseProxy{
		Transport: h.transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.Path = agentdPath
			if r.In.URL.RawQuery != "" {
				r.Out.URL.RawQuery = r.In.URL.RawQuery
			}
			r.Out.Host = "localhost:" + portStr

			// G34: strip all caller headers, then re-add only the allowlist.
			// The caller's Cookie (contains JWT), Origin, Referer describe
			// their relationship with the API server, not with the tenant
			// pod, and must not reach untrusted code inside the pod.
			r.Out.Header = http.Header{}
			copyRequestHeaders(r.In.Header, r.Out.Header)
			r.Out.Header.Set("Authorization", basicAuth)
			r.Out.Header.Set("X-Forwarded-For", r.In.RemoteAddr)

			// P0-2 (redesign-2026-08-19): re-establish protocol-upgrade
			// headers after the G34 wipe. ReverseProxy detects the upgrade
			// and sets Connection/Upgrade on the outbound request BEFORE
			// Rewrite runs; the wipe above would strip them, degrading WS
			// handshakes to plain GETs at the dev server (verified in the
			// field — HMR broken end-to-end; see redesign REGRESSION.md).
			// Connection/Upgrade/Sec-WebSocket-* are transport descriptors,
			// not caller credentials: forwarding them is required by
			// epic-66 ("WS support is mandatory for HMR") and leaks nothing
			// about the caller's relationship with the API server.
			if upType := requestUpgradeType(r.In.Header); upType != "" {
				r.Out.Header.Set("Connection", "Upgrade")
				r.Out.Header.Set("Upgrade", upType)
				for k, vs := range r.In.Header {
					if strings.HasPrefix(strings.ToLower(k), "sec-websocket-") {
						for _, v := range vs {
							r.Out.Header.Add(k, v)
						}
					}
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// P0-1 (redesign-2026-08-19/DESIGN.md §5.5): force no-store on
			// HTML at the edge. The chain in front of this handler (CDN,
			// browser) has been observed serving stale HTML across app
			// changes, which makes developers debug code that never reached
			// the browser. Only HTML is forced: hashed assets keep their
			// app-set caching. Idempotent when the app already sends
			// no-store; overrides app-set long-cache on HTML.
			if isHTMLMediaType(resp.Header.Get("Content-Type")) {
				resp.Header.Set("Cache-Control", "no-store")
				resp.Header.Del("Expires")
			}
			// Pre-reject oversized declared Content-Length (A6 — both
			// response shapes are normal; this handles the sized case).
			if resp.ContentLength > maxBytes {
				return fmt.Errorf("response exceeds size cap (%d > %d)", resp.ContentLength, maxBytes)
			}
			// P0-2 (redesign-2026-08-19): protocol switches (101) must keep
			// their body unwrapped. ReverseProxy's handleUpgradeResponse
			// type-asserts the body to io.ReadWriteCloser for the
			// bidirectional copy; cappedReader would break it ("non-writable
			// body"). Upgrade streams are inherently unbounded (HMR), so the
			// byte cap does not apply to them.
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return nil
			}
			// Wrap the body to count bytes for chunked streams (A6).
			resp.Body = &cappedReader{rc: resp.Body, max: maxBytes}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if err != nil && strings.Contains(err.Error(), "size cap") {
				http.Error(w, "dev preview response exceeded size cap", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "workspace dev-preview endpoint unreachable", http.StatusBadGateway)
		},
	}

	c.Writer.Header().Del("Content-Length")
	proxy.ServeHTTP(c.Writer, c.Request)
}

// requestUpgradeType mirrors httputil's upgradeType: returns the requested
// protocol (e.g. "websocket") when the request asks for a switch, else "".
// Connection token lists may be comma-separated ("keep-alive, Upgrade").
func requestUpgradeType(h http.Header) string {
	if !headerContainsToken(h.Values("Connection"), "upgrade") {
		return ""
	}
	return h.Get("Upgrade")
}

// headerContainsToken reports whether any comma-separated token in the
// values equals want (case-insensitive, whitespace-trimmed).
func headerContainsToken(values []string, want string) bool {
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), want) {
				return true
			}
		}
	}
	return false
}

// isHTMLMediaType reports whether a Content-Type value denotes HTML
// (including parameters such as charset). Non-parsable values are treated
// as non-HTML so the no-store override stays narrow (P0-1).
func isHTMLMediaType(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "text/html"
}

// cappedReader wraps an io.ReadCloser and returns an error when the
// total bytes read exceed max (A6 — handles Transfer-Encoding: chunked
// responses that have no Content-Length). The ReverseProxy's
// ErrorHandler maps this to a 413 via the "size cap" string match.
type cappedReader struct {
	rc   io.ReadCloser
	max  int64
	read int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.read += int64(n)
	if c.read > c.max {
		return n, fmt.Errorf("response exceeds size cap (%d > %d)", c.read, c.max)
	}
	return n, err
}

func (c *cappedReader) Close() error {
	return c.rc.Close()
}

func (h *DevPreviewHandler) acquireConnection(workspaceID string) bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	max := h.config.MaxConnsPerWorkspace
	if max <= 0 {
		max = 50
	}
	if h.connCount[workspaceID] >= max {
		return false
	}
	h.connCount[workspaceID]++
	return true
}

func (h *DevPreviewHandler) releaseConnection(workspaceID string) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.connCount[workspaceID] > 0 {
		h.connCount[workspaceID]--
	}
	if h.connCount[workspaceID] == 0 {
		delete(h.connCount, workspaceID)
	}
}
