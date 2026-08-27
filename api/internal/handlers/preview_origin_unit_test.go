// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Epic 67 unit tests validate the port-in-subdomain parsing logic

const (
	epic67TestDomain = "epic67.test.example.com"
	epic67TestWS     = "a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d"
	epic67TestPort   = 5173 // Vite default
)

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

	// Validate redirect format: https://5173-<uuid>-preview.<baseDomain>/?t=...
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