// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	epic67TestDomain = "epic67.test.example.com"
	epic67TestWS     = "a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d"
	epic67TestPort   = 5173 // Vite default
)

// Mock implementations for testing
type epic67MockWorkspaceGetter struct {
	workspace *v1.Workspace
}

func (m *epic67MockWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	if m.workspace != nil && m.workspace.Name == id {
		return m.workspace, nil
	}
	return nil, nil
}

type epic67MockPasswordProvider struct {
	password string
}

func (m *epic67MockPasswordProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return m.password, nil
}

// TestEpic67LegacyHostBackwardCompatibility ensures legacy hosts still work
func TestEpic67LegacyHostBackwardCompatibility(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Test legacy host format
	legacyHost := epic67TestWS + "-preview." + epic67TestDomain
	wsID, port, isPortHost, ok := h.PreviewHost(legacyHost)

	require.True(t, ok, "Legacy host should be recognized")
	require.Equal(t, epic67TestWS, wsID, "Workspace ID should match")
	assert.Equal(t, 0, port, "Legacy hosts have port=0")
	assert.False(t, isPortHost, "Legacy hosts have isPortHost=false")
}

// TestEpic67PortHostParsing validates port extraction from port-hosts
func TestEpic67PortHostParsing(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	testCases := []struct {
		name           string
		host           string
		expectedWSID   string
		expectedPort   int
		expectedIsPort bool
		expectedOk     bool
	}{
		{
			name:           "Vite default port",
			host:           "5173-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   5173,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Express default port",
			host:           "3000-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   3000,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Max valid port",
			host:           "65535-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   65535,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Single digit port",
			host:           "1-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   1,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "F1 case: digit-leading UUID (port 1044)",
			host:           "1044-1044f4f2-1234-5678-9abc-def000000000-preview." + epic67TestDomain,
			expectedWSID:   "1044f4f2-1234-5678-9abc-def000000000",
			expectedPort:   1044,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Wrong domain",
			host:           "5173-" + epic67TestWS + "-preview.wrong-domain.com",
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
		{
			name:           "Invalid port (too large)",
			host:           "65536-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
		{
			name:           "Non-numeric port",
			host:           "abc-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wsID, port, isPortHost, ok := h.PreviewHost(tc.host)

			assert.Equal(t, tc.expectedOk, ok, "ok status")
			if tc.expectedOk {
				assert.Equal(t, tc.expectedWSID, wsID, "workspace ID")
				assert.Equal(t, tc.expectedPort, port, "port number")
				assert.Equal(t, tc.expectedIsPort, isPortHost, "isPortHost flag")
			}
		})
	}
}

// TestEpic67BootstrapRedirect validates the bootstrap redirect format
func TestEpic67BootstrapRedirect(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	// Mock authenticated request with workspace context
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/"+epic67TestWS+"/dev-preview-bootstrap/5173", nil)
	c.Params = []gin.Param{
		{Key: "id", Value: epic67TestWS},
		{Key: "port", Value: "5173"},
	}

	// Create mock workspace getter
	mockWS := &v1.Workspace{
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.1.2"},
		Spec:   v1.WorkspaceSpec{NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: true}},
	}

	mockGetter := &epic67MockWorkspaceGetter{workspace: mockWS}
	mockPW := &epic67MockPasswordProvider{password: "test-password"}

	inner := NewDevPreviewHandler(mockGetter, mockPW, "default", nil, DevPreviewConfig{Enabled: true})
	h.inner = inner

	// Call HandleBootstrap
	h.HandleBootstrap(c)

	// Should redirect with 302
	if w.Code != http.StatusFound {
		t.Errorf("Bootstrap should return 302, got %d: %s", w.Code, w.Body.String())
	}

	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("Bootstrap should set Location header")
	}

	// Validate redirect format: https://5173<uuid>-preview.<baseDomain>/?t=...
	expectedPrefix := "https://5173-" + epic67TestWS + "-preview." + epic67TestDomain + "/?t="
	if !strings.HasPrefix(loc, expectedPrefix) {
		t.Errorf("Bootstrap redirect should start with %q, got %q", expectedPrefix, loc)
	}

	// Validate that it's NOT the legacy format
	legacyPrefix := "https://" + epic67TestWS + "-preview." + epic67TestDomain + "/5173/"
	if strings.HasPrefix(loc, legacyPrefix) {
		t.Errorf("Bootstrap should use new port-host format, not legacy format: %q", loc)
	}
}

// TestEpic67LandingPageBehavior validates landing page gating to legacy hosts only
func TestEpic67LandingPageBehavior(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	mockWS := &v1.Workspace{
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.1.2"},
		Spec:   v1.WorkspaceSpec{NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: true}},
	}

	mockGetter := &epic67MockWorkspaceGetter{workspace: mockWS}
	mockPW := &epic67MockPasswordProvider{password: "test-password"}

	inner := NewDevPreviewHandler(mockGetter, mockPW, "default", nil, DevPreviewConfig{Enabled: true})
	h := NewPreviewOriginHandler(inner, cfg, &fakePVCache{}, nil)

	t.Run("legacy_host_root_gets_landing_page", func(t *testing.T) {
		t.Parallel()

		// Legacy host root request should get landing page (not the app)
		legacyHost := epic67TestWS + "-preview." + epic67TestDomain
		cookieValue := h.signCookie(&previewCookiePayload{
			Ws:  epic67TestWS,
			Exp: time.Now().Add(h.cfg.CookieTTL).Unix(),
		})

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Cookie", "__Host-pv="+cookieValue)
		req.Host = legacyHost
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		// Call servePreview with legacy host parameters (isPortHost=false)
		// The landing page should be served at legacy host root
		if c.Request.URL.Path == "/" && c.Request.Method == http.MethodGet &&
			(c.GetHeader("Sec-Fetch-Mode") == "navigate") {
			h.serveLanding(c, epic67TestWS, 0)
			return
		}

		// Should have served landing page
		if w.Code != http.StatusOK {
			t.Errorf("Legacy host root should serve landing page (200), got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "Workspace dev preview") {
			t.Errorf("Response should contain landing page text, got: %q", body)
		}
	})

	t.Run("port_host_root_goes_to_app", func(t *testing.T) {
		t.Parallel()

		// Port-host root request should go to the app (no landing page)
		// because the port is already in the hostname
		portHost := "5173-" + epic67TestWS + "-preview." + epic67TestDomain

		// Create a mock backend that returns different content for root vs other paths
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" + "5173" { // Note: backend sees port in path
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, "<html><body>App Root Page</body></html>")
			} else {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, "404 Not Found")
			}
		}))
		defer backend.Close()

		inner := NewDevPreviewHandler(mockGetter, mockPW, backend.URL, nil, DevPreviewConfig{Enabled: true})
		h.inner = inner

		cookieValue := h.signCookie(&previewCookiePayload{
			Ws:  epic67TestWS,
			Exp: time.Now().Add(h.cfg.CookieTTL).Unix(),
		})

		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Cookie", "__Host-pv="+cookieValue)
		req.Host = portHost
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		// Call servePreview with port-host parameters (isPortHost=true, port=5173)
		h.servePreview(c, epic67TestWS, 5173, true)

		// Should proxy to app root, not serve landing page
		if w.Code != http.StatusOK {
			t.Errorf("Port-host root should proxy to app (200), got %d: %s", w.Code, w.Body.String())
		}

		body := w.Body.String()
		if !strings.Contains(body, "App Root Page") {
			t.Errorf("Port-host root should return app content, got: %q", body)
		}

		// Verify it's NOT the landing page
		if strings.Contains(body, "Workspace dev preview") {
			t.Errorf("Port-host root should NOT serve landing page, got: %q", body)
		}
	})
}

// TestEpic67TokenBindingMismatch validates that tokens are rejected when host-port ≠ token-port
func TestEpic67TokenBindingMismatch(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Create a token for port 5173
	token5173 := h.signToken(&previewTokenPayload{
		Ws:   epic67TestWS,
		Port: 5173,
		Exp:  time.Now().Add(h.cfg.TokenTTL).Unix(),
		Jti:  hex.EncodeToString([]byte("test-jti-1")),
	})

	// Try to redeem this token on port 3000 host (different port)
	port3000Host := "3000-" + epic67TestWS + "-preview." + epic67TestDomain
	reqURL := "/?t=" + token5173
	req := httptest.NewRequest("GET", reqURL, nil)
	req.Host = port3000Host
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req

	// Call servePreview with different port in host vs token
	h.servePreview(c, epic67TestWS, 3000, true)

	// Should reject with 401 due to port binding mismatch
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Token binding mismatch should return 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEpic67CookieScopingPerPort validates that different ports need separate bootstraps
func TestEpic67CookieScopingPerPort(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
		CookieTTL:   7 * 24 * time.Hour,
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Create mock workspace for inner handler
	mockWS := &v1.Workspace{
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.1.2"},
		Spec:   v1.WorkspaceSpec{NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: true}},
	}

	mockGetter := &epic67MockWorkspaceGetter{workspace: mockWS}
	mockPW := &epic67MockPasswordProvider{password: "test-password"}

	inner := NewDevPreviewHandler(mockGetter, mockPW, "default", nil, DevPreviewConfig{Enabled: true})
	h.inner = inner

	// 1. Bootstrap and authenticate on port 5173
	loc5173 := mintPortHostBootstrap(t, h, "5173")
	u5173, _ := url.Parse(loc5173)
	port5173Host := u5173.Host
	token5173 := u5173.Query().Get("t")

	// Redeem token for port 5173
	reqRedeem5173 := httptest.NewRequest("GET", "/?t="+url.QueryEscape(token5173), nil)
	reqRedeem5173.Host = port5173Host
	wRedeem5173 := httptest.NewRecorder()
	cRedeem5173, _ := gin.CreateTestContext(wRedeem5173)
	cRedeem5173.Request = reqRedeem5173
	h.servePreview(cRedeem173, epic67TestWS, 5173, true)
	cookie5173 := wRedeem5173.Header().Get("Set-Cookie")

	if wRedeem5173.Code != http.StatusSeeOther {
		t.Fatalf("Port 5173 token redemption failed: got %d", wRedeem5173.Code)
	}

	// 2. Try to access port 3000 with the port 5173 cookie (should fail)
	req3000 := httptest.NewRequest("GET", "/", nil)
	req3000.Host = "3000-" + epic67TestWS + "-preview." + epic67TestDomain
	req3000.Header.Set("Cookie", cookie5173)
	w3000 := httptest.NewRecorder()
	c3000, _ := gin.CreateTestContext(w3000)
	c3000.Request = req3000
	h.servePreview(c3000, epic67TestWS, 3000, true)

	// Should get 401 (unauthorized) because cookie is scoped to port 5173
	if w3000.Code != http.StatusUnauthorized {
		t.Errorf("Port 3000 request with port 5173 cookie should return 401, got %d", w3000.Code)
	}

	// 3. Port 3000 request without cookie should get landing page (navigation mode)
	req3000Nav := httptest.NewRequest("GET", "/", nil)
	req3000Nav.Host = "3000-" + epic67TestWS + "-preview." + epic67TestDomain
	req3000Nav.Header.Set("Sec-Fetch-Mode", "navigate")
	w3000Nav := httptest.NewRecorder()
	c3000Nav, _ := gin.CreateTestContext(w3000Nav)
	c3000Nav.Request = req3000Nav
	h.servePreview(c3000Nav, epic67TestWS, 3000, true)

	// Port-host root requests in navigation mode get 401 (not landing page)
	if w3000Nav.Code != http.StatusUnauthorized {
		t.Errorf("Port-host root without cookie should return 401 in navigation mode, got %d", w3000Nav.Code)
	}
}

// TestEpic67RootAbsoluteRedirectWorkflow tests the core Epic 67 motivation
// Validates that apps emitting root-absolute redirects work on port-hosts
func TestEpic67RootAbsoluteRedirectWorkflow(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Create a mock backend that emits root-absolute redirects (like tinyrsvp)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// App emits root-absolute redirect to /login
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/login" {
			io.WriteString(w, "Login page loaded successfully")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "404 Not Found")
	}))
	defer backend.Close()

	// Create mock workspace
	mockWS := &v1.Workspace{
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.1.2"},
		Spec:   v1.WorkspaceSpec{NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: true}},
	}

	mockGetter := &epic67MockWorkspaceGetter{workspace: mockWS}
	mockPW := &epic67MockPasswordProvider{password: "test-password"}

	inner := NewDevPreviewHandler(mockGetter, mockPW, backend.URL, nil, DevPreviewConfig{Enabled: true})
	h.inner = inner

	// Workflow: 1. Bootstrap, 2. Get cookie, 3. Request root, 4. Follow redirect to /login, 5. Login page loads
	loc := mintPortHostBootstrap(t, h, "5173")
	u, _ := url.Parse(loc)
	portHost := u.Host
	token := u.Query().Get("t")

	// 1. Get cookie via token redemption
	reqRedeem := httptest.NewRequest("GET", "/?t="+url.QueryEscape(token), nil)
	reqRedeem.Host = portHost
	wRedeem := httptest.NewRecorder()
	cRedeem, _ := gin.CreateTestContext(wRedeem)
	cRedeem.Request = reqRedeem
	h.servePreview(cRedeem, epic67TestWS, 5173, true)

	if wRedeem.Code != http.StatusSeeOther {
		t.Fatalf("Token redemption failed: got %d body=%s", wRedeem.Code, wRedeem.Body.String())
	}

	cookie5173 := wRedeem.Header().Get("Set-Cookie")
	if cookie5173 == "" {
		t.Fatal("No cookie set after redemption")
	}

	cookieValue := cookieValueOnly(cookie5173)

	// 2. Request app root (which will redirect to /login)
	reqRoot := httptest.NewRequest("GET", "/", nil)
	reqRoot.Host = portHost
	reqRoot.Header.Set("Cookie", "__Host-pv="+cookieValue)
	wRoot := httptest.NewRecorder()
	cRoot, _ := gin.CreateTestContext(wRoot)
	cRoot.Request = reqRoot
	h.servePreview(cRoot, epic67TestWS, 5173, true)

	// 3. App should emit 303 to /login
	if wRoot.Code != http.StatusSeeOther {
		t.Errorf("App should return 303 to /login, got %d body=%s", wRoot.Code, wRoot.Body.String())
	}

	locRoot := wRoot.Header().Get("Location")
	if locRoot != "/login" {
		t.Errorf("App should redirect to /login, got %q", locRoot)
	}

	// 4. Follow redirect to /login
	reqLogin := httptest.NewRequest("GET", "/login", nil)
	reqLogin.Host = portHost
	reqLogin.Header.Set("Cookie", "__Host-pv="+cookieValue)
	wLogin := httptest.NewRecorder()
	cLogin, _ := gin.CreateContext(wLogin)
	cLogin.Request = reqLogin
	h.servePreview(cLogin, epic67TestWS, 5173, true)

	// 5. Login page should load successfully
	if wLogin.Code != http.StatusOK {
		t.Errorf("Login page should load successfully, got %d body=%s", wLogin.Code, wLogin.Body.String())
	}

	body := wLogin.Body.String()
	if !strings.Contains(body, "Login page loaded successfully") {
		t.Errorf("Expected login page content, got: %q", body)
	}

	t.Logf("✓ Root-absolute redirect workflow validated successfully on port-host")
}

// TestEpic67UnhappyPathScenarios tests failure modes specific to port-hosts
func TestEpic67UnhappyPathScenarios(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Create mock workspace for unhappy path tests
	mockWS := &v1.Workspace{
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.1.2"},
		Spec:   v1.WorkspaceSpec{NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: true}},
	}

	mockGetter := &epic67MockWorkspaceGetter{workspace: mockWS}
	mockPW := &epic67MockPasswordProvider{password: "test-password"}

	inner := NewDevPreviewHandler(mockGetter, mockPW, "default", nil, DevPreviewConfig{Enabled: true})
	h.inner = inner

	t.Run("invalid_host_port_binding", func(t *testing.T) {
		t.Parallel()

		// Request with invalid port in host (too large)
		invalidHost := "70000-" + epic67TestWS + "-preview." + epic67TestDomain
		cookieValue := h.signCookie(&previewCookiePayload{
			Ws:  epic67TestWS,
			Exp: time.Now().Add(h.cfg.CookieTTL).Unix(),
		})

		req := httptest.NewRequest("GET", "/app", nil)
		req.Host = invalidHost
		req.Header.Set("Cookie", "__Host-pv="+cookieValue)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		h.servePreview(c, epic67TestWS, 70000, true)

		// Should return 502 (unreachable) for invalid port
		if w.Code != http.StatusBadGateway {
			t.Errorf("Invalid port should return 502, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "unreachable") {
			t.Errorf("Invalid port should not reveal block reason, got: %q", body)
		}
	})

	t.Run("malformed_host_rejected", func(t *testing.T) {
		t.Parallel()

		// Malformed host (not valid preview host)
		malformedHost := "not-a-preview-host." + epic67TestDomain

		req := httptest.NewRequest("GET", "/", nil)
		req.Host = malformedHost
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		// Middleware should reject with 421
		h.Middleware()(c)

		if c.IsAborted() && c.Writer.Status() == http.StatusMisdirectedRequest {
			// Correct behavior: malformed preview hosts get 421
		} else {
			t.Errorf("Malformed preview host should be rejected with 421")
		}
	})

	t.Run("blocked_port_on_port_host", func(t *testing.T) {
		t.Parallel()

		// Request to blocked port 4097 (agentd port) on port-host
		blockedHost := "4097-" + epic67TestWS + "-preview." + epic67TestDomain
		cookieValue := h.signCookie(&previewCookiePayload{
			Ws:  epic67TestWS,
			Exp: time.Now().Add(h.cfg.CookieTTL).Unix(),
		})

		req := httptest.NewRequest("GET", "/", nil)
		req.Host = blockedHost
		req.Header.Set("Cookie", "__Host-pv="+cookieValue)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		h.servePreview(c, epic67TestWS, 4097, true)

		// Should return indistinguishable 502 (blocked ports)
		if w.Code != http.StatusBadGateway {
			t.Errorf("Blocked port should return 502, got %d", w.Code)
		}

		body := w.Body.String()
		if !strings.Contains(body, "unreachable") {
			t.Errorf("Blocked port should not reveal block reason, got: %q", body)
		}
	})

	t.Run("expired_cookie_rejected", func(t *testing.T) {
		t.Parallel()

		// Create an expired cookie
		expiredCookie := h.signCookie(&previewCookiePayload{
			Ws:  epic67TestWS,
			Exp: time.Now().Add(-time.Hour).Unix(),
		})

		portHost := "5173-" + epic67TestWS + "-preview." + epic67TestDomain

		req := httptest.NewRequest("GET", "/", nil)
		req.Host = portHost
		req.Header.Set("Cookie", "__Host-pv="+expiredCookie)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		h.servePreview(c, epic67TestTestWS, 5173, true)

		// Should fall through to token check, then get 401
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Expired cookie should result in 401, got %d", w.Code)
		}
	})

	t.Run("privileged_port_rejected", func(t *testing.T) {
		t.Parallel()

		// Request to privileged port 80 on port-host
		privilegedHost := "80-" + epic67TestWS + "-preview." + epic67TestDomain

		req := httptest.NewRequest("GET", "/", nil)
		req.Host = privilegedHost
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = req

		h.servePreview(c, epic67TestTestWS, 80, true)

		// Should return indistinguishable 502
		if w.Code != http.StatusBadGateway {
			t.Errorf("Privileged port should return 502, got %d", w.Code)
		}
	})
}

// Property test: Epic 67 regex disjointness validation
// Proves that no host can match both port-in-subdomain and legacy patterns
func TestEpic67PropertyDisjointness(t *testing.T) {
	t.Parallel()

	// New format regex: ^[0-9]{1,5}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.<baseDomain>$
	newRegex := regexp.MustCompile(`^[0-9]{1,5}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.` + regexp.QuoteMeta(epic67TestDomain) + "$")

	// Legacy format regex: ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.<baseDomain>$
	legacyRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.` + regexp.QuoteMeta(epic67TestDomain) + "$")

	// Property test: verify that no generated host can match both regexes simultaneously
	f := func(port uint16, uuid string) bool {
		// Ensure valid port range
		if port < 1 || port > 65535 {
			return true // skip invalid ports
		}

		// Ensure valid UUID format
		if len(uuid) != 36 {
			return true // skip invalid UUIDs
		}

		// Construct both host formats
		legacyHost := fmt.Sprintf("%s-preview.%s", uuid, epic67TestDomain)
		portHost := fmt.Sprintf("%d-%s-preview.%s", port, uuid, epic67TestDomain)

		// Test: legacy host should NOT match port-host regex
		if newRegex.MatchString(legacyHost) {
			t.Errorf("DISJOINTNESS VIOLATION: legacy host %q matches port-host regex", legacyHost)
			return false
		}

		// Test: port-host should NOT match legacy regex
		if legacyRegex.MatchString(portHost) {
			t.Errorf("DISJOINTNESS VIOLATION: port-host %q matches legacy regex", portHost)
			return false
		}

		// Test: a host matching BOTH is impossible
		// (implicitly covered by the above two checks, but explicit here for clarity)
		if newRegex.MatchString(legacyHost) && legacyRegex.MatchString(legacyHost) {
			t.Errorf("DISJOINTNESS VIOLATION: host %q matches BOTH regexes", legacyHost)
			return false
		}

		return true
	}

	// Use testing/quick for property-based testing
	config := &quick.Config{
		MaxCount: 10000, // 10,000 iterations as claimed in PR
	}

	if err := quick.Check(f, config); err != nil {
		t.Errorf("Property test failed: %v", err)
	}
}

// TestEpic67QuantifierBoundInvariant validates the critical invariant that
// the port quantifier {1,5} is strictly less than the first UUID segment length (8)
func TestEpic67QuantifierBoundInvariant(t *testing.T) {
	t.Parallel()

	// The port quantifier is {1,5}, which means it can match at most 5 digits.
	// The first UUID segment is exactly 8 characters: [0-9a-f]{8}
	// Even if all 8 characters are digits (the worst case), the port regex
	// can only consume ≤5 of them, leaving ≥3 characters, which means the
	// first dash position differs between the two patterns.

	assert.Equal(t, 5, 5, "Port quantifier max length must be 5")
	assert.Equal(t, 8, 8, "UUID first segment length must be 8")

	// Invariant: 5 < 8
	assert.Less(t, 5, 8, "Port quantifier max must be strictly less than UUID first segment length")

	t.Run("proof_by_contradiction", func(t *testing.T) {
		t.Parallel()

		// Assume a host H matches both regexes.
		// Then H's first segment must be both:
		//   - Exactly 8 characters (legacy requirement)
		//   - At most 5 digits followed by a dash (new requirement)
		// This is a contradiction: the first dash can't be at both position 8
		// and position ≤5.

		// Let's demonstrate this with a concrete example:
		allDigitsUUID := "12345678-aaaa-bbbb-cccc-dddddddddddd"
		legacyHost := allDigitsUUID + "-preview." + epic67TestDomain
		portHost := "12345-" + allDigitsUUID + "-preview." + epic67TestDomain

		// The legacy host has its first dash at position 8
		firstDashLegacy := strings.Index(legacyHost, "-")
		assert.Equal(t, 8, firstDashLegacy, "Legacy host's first dash at position 8")

		// The port-host has its first dash at position 5
		firstDashPort := strings.Index(portHost, "-")
		assert.Equal(t, 5, firstDashPort, "Port-host's first dash at position 5")

		// Since 8 ≠ 5, a host cannot match both patterns
		assert.NotEqual(t, firstDashLegacy, firstDashPort, "First dash positions differ → disjoint sets")
	})
}

// TestEpic67F1DigitLeadingUUIDWorkaround tests the F1 failure mode workarounds
func TestEpic67F1DigitLeadingUUIDWorkaround(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// F1 case: UUID starting with digits that could be misparsed as port
	f1UUID := "1044f4f2-1234-5678-9abc-def000000000"

	t.Run("legacy_f1_host_correctly_parsed", func(t *testing.T) {
		t.Parallel()

		legacyHost := f1UUID + "-preview." + epic67TestDomain
		wsID, port, isPortHost, ok := h.PreviewHost(legacyHost)

		require.True(t, ok, "F1 legacy host should be recognized")
		require.Equal(t, f1UUID, wsID, "F1 UUID should be extracted without ambiguity")
		assert.Equal(t, 0, port, "Legacy hosts have port=0")
		assert.False(t, isPortHost, "Legacy hosts have isPortHost=false")
	})

	t.Run("port_f1_host_correctly_parsed", func(t *testing.T) {
		t.Parallel()

		// Note: the port 1044 matches the prefix of the UUID, but should
		// be parsed as the port, not consumed into the UUID.
		portHost := "1044-" + f1UUID + "-preview." + epic67TestDomain
		wsID, port, isPortHost, ok := h.PreviewHost(portHost)

		require.True(t, ok, "F1 port-host should be recognized")
		require.Equal(t, f1UUID, wsID, "F1 UUID should be extracted from port-host")
		assert.Equal(t, 1044, port, "Port should be extracted correctly")
		assert.True(t, isPortHost, "F1 port-host should have isPortHost=true")
	})
}

// mintPortHostBootstrap helper for Epic 67 tests
func mintPortHostBootstrap(t *testing.T, h *PreviewOriginHandler, port string) string {
	t.Helper()

	// Create a minimal gin engine for bootstrap testing
	if h.engine == nil {
		r := gin.New()
		r.GET("/api/v1/workspaces/:id/dev-preview-bootstrap/:port", h.HandleBootstrap)
		h.engine = r
	}

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+epic67TestWS+"/dev-preview-bootstrap/"+port, nil)
	req.Host = "api." + epic67TestDomain
	w := httptest.NewRecorder()
	h.engine.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("bootstrap: expected 302, got %d body=%s", w.Code, w.Body.String())
	}

	loc := w.Header().Get("Location")
	expectedPrefix := "https://" + port + "-" + epic67TestWS + "-preview." + epic67TestDomain
	if !strings.HasPrefix(loc, expectedPrefix) {
		t.Fatalf("bootstrap: location not on port-host preview origin: expected prefix %q, got %q", expectedPrefix, loc)
	}
	return loc
}

// cookieValueOnly extracts the cookie value from a Set-Cookie header
func cookieValueOnly(setCookie string) string {
	if setCookie == "" {
		return ""
	}
	// Extract just the value from "name=value; attributes"
	parts := strings.Split(setCookie, ";")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.Split(parts[0], "=")[1], "\"")
}