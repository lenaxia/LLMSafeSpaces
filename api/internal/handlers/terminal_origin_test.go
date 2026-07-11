// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// G35: the terminal WebSocket Upgrader has historically used
// `CheckOrigin: func(r *http.Request) bool { return true }`, accepting any
// origin. That makes the WebSocket endpoint vulnerable to cross-site WebSocket
// hijacking from a malicious page in a browser that holds the user's session
// ticket. The fix is a real Origin policy:
//
//   - Same-origin requests (Origin host:port == request Host) are accepted.
//   - Origins in the operator-configured allowlist are accepted.
//   - Missing Origin header (non-browser clients) is accepted; these are
//     authenticated by the single-use ticket, not by cookies, so CSRF does
//     not apply.
//   - Everything else is rejected.

func TestCheckTerminalOrigin_SameOrigin(t *testing.T) {
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com:8080"
	req.Header.Set("Origin", "http://api.example.com:8080")

	assert.True(t, f(req), "same-origin request must be accepted")
}

func TestCheckTerminalOrigin_SameOriginHTTPS(t *testing.T) {
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://api.example.com")

	assert.True(t, f(req), "same-origin HTTPS request must be accepted")
}

func TestCheckTerminalOrigin_CrossSubdomainRejectedByDefault(t *testing.T) {
	// Frontend served on https://app.example.com calls API on
	// https://api.example.com — different subdomain, NOT same-origin.
	// Must be rejected unless explicitly allowlisted.
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://app.example.com")

	assert.False(t, f(req), "cross-subdomain request must be rejected by default")
}

func TestCheckTerminalOrigin_MissingOriginAccepted(t *testing.T) {
	// Non-browser clients (curl, MCP) do not send Origin. They authenticate
	// via the single-use ticket, not cookies, so CSRF is not a concern.
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	// no Origin header

	assert.True(t, f(req), "non-browser (no Origin) must be accepted")
}

func TestCheckTerminalOrigin_CrossOriginRejectedByDefault(t *testing.T) {
	f := newCheckOriginChecker(nil) // empty allowlist → same-origin only
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://evil.example.com")

	assert.False(t, f(req), "cross-origin request must be rejected by default")
}

func TestCheckTerminalOrigin_AllowlistAccepts(t *testing.T) {
	f := newCheckOriginChecker([]string{"https://app.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://app.example.com")

	assert.True(t, f(req), "explicitly allowlisted origin must be accepted")
}

func TestCheckTerminalOrigin_WildcardAcceptsAll(t *testing.T) {
	// Operators who really want the old behaviour can set ["*"].
	f := newCheckOriginChecker([]string{"*"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://evil.example.com")

	assert.True(t, f(req), "wildcard allowlist accepts any origin")
}

func TestCheckTerminalOrigin_AllowlistRejectsUnlisted(t *testing.T) {
	f := newCheckOriginChecker([]string{"https://app.example.com"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://evil.example.com")

	assert.False(t, f(req), "origin not in allowlist must be rejected")
}

func TestCheckTerminalOrigin_MalformedOriginRejected(t *testing.T) {
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "://not-a-url")

	assert.False(t, f(req), "malformed Origin header must be rejected")
}

func TestCheckTerminalOrigin_PortMismatchRejected(t *testing.T) {
	// api.example.com:8080 vs api.example.com:9090 — same host, different port.
	// Browser SOP treats these as different origins.
	f := newCheckOriginChecker(nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "api.example.com:8080"
	req.Header.Set("Origin", "http://api.example.com:9090")

	assert.False(t, f(req), "different port must be rejected as cross-origin")
}

// End-to-end wiring check: the handler's upgrader actually uses the new
// CheckOrigin. A connection attempt with a disallowed Origin must fail at
// the upgrade step.
func TestTerminal_G35_CrossOriginUpgradeRejected(t *testing.T) {
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{
		"ws-1": makeTerminalWorkspace("ws-1", string(v1.WorkspacePhaseActive)),
	}}
	// Empty allowlist → same-origin only.
	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)

	// Pre-populate a ticket so we get past the ticket check and reach the
	// upgrader.
	require.NoError(t, cache.Set(context.Background(), "terminal:ticket:tkt-cross-origin",
		`{"userID":"user-1","workspaceID":"ws-1"}`, ticketTTL))

	router := gin.New()
	gin.SetMode(gin.TestMode)
	router.GET("/api/v1/workspaces/:id/terminal", h.HandleTerminal)

	// gorilla/websocket's Dialer sends an Origin header derived from the URL.
	// We craft a request with a cross-origin Origin manually.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/workspaces/ws-1/terminal?ticket=tkt-cross-origin", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// gorilla returns 403 when CheckOrigin rejects.
	assert.Equal(t, http.StatusForbidden, w.Code,
		"cross-origin WebSocket upgrade must be rejected by the upgrader, got %d", w.Code)
}

func TestTerminal_G35_SameOriginUpgradePassesOriginCheck(t *testing.T) {
	// We can't drive a full WebSocket upgrade through httptest.NewRecorder
	// because gorilla needs to HTTP-hijack the connection. The behavioural
	// contract we care about is that the upgrader's CheckOrigin function
	// accepts same-origin — the rest of the upgrade path is exercised by
	// the proxy/handler integration tests. Construct the handler and call
	// its CheckOrigin directly.
	cache := newMockTerminalCache()
	wsGetter := &mockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}
	h := NewTerminalHandler(cache, wsGetter, "llmsafespaces", nil, nil)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/workspaces/ws-1/terminal?ticket=ignored", nil)
	req.Host = "api.example.com"
	req.Header.Set("Origin", "http://api.example.com")

	require.NotNil(t, h.upgrader.CheckOrigin, "upgrader must have a CheckOrigin function")
	assert.True(t, h.upgrader.CheckOrigin(req),
		"same-origin WebSocket upgrade must pass the upgrader's CheckOrigin")
}

// --- helpers ---

func makeTerminalWorkspace(name, phase string) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"user-id": "user-1"},
		},
		Status: v1.WorkspaceStatus{
			Phase:   v1.WorkspacePhase(phase),
			PodName: name + "-pod",
		},
	}
}
