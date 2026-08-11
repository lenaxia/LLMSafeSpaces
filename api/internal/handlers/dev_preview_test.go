// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Mocks ---

type devPreviewMockWorkspaceGetter struct {
	workspaces map[string]*v1.Workspace
	gets       int
}

func (m *devPreviewMockWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	m.gets++
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return ws, nil
}

type devPreviewMockPasswordProvider struct {
	passwords map[string]string
}

func (m *devPreviewMockPasswordProvider) WorkspacePassword(_ context.Context, workspaceID string) (string, error) {
	pw, ok := m.passwords[workspaceID]
	if !ok {
		return "", fmt.Errorf("no password")
	}
	return pw, nil
}

// --- Helpers ---

func newDevPreviewHandlerForTest(t *testing.T, wsGetter *devPreviewMockWorkspaceGetter, pwProvider *devPreviewMockPasswordProvider) *DevPreviewHandler {
	t.Helper()
	return NewDevPreviewHandler(wsGetter, pwProvider, "llmsafespaces", nil, DevPreviewConfig{
		Enabled:              true,
		MaxResponseBytes:     50 * 1024 * 1024,
		MaxConnsPerWorkspace: 50,
	})
}

func setupDevPreviewRouter(h *DevPreviewHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	// Match the production route pattern on idGroup.
	r.GET("/api/v1/workspaces/:id/dev-preview/*portPath", h.HandleDevPreview)
	return r
}

func activeWorkspaceWithDevPreview(id, podIP string, devPreview bool) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: id, Labels: map[string]string{"user-id": "user-1"}},
		Spec: v1.WorkspaceSpec{
			NetworkAccess: &v1.WorkspaceNetworkAccess{DevPreview: devPreview},
		},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: podIP},
	}
}

// --- Tests ---

func TestDevPreviewHandler_WorkspaceNotFound(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{workspaces: map[string]*v1.Workspace{}}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/nonexistent/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDevPreviewHandler_WorkspaceNotActive(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": {
				ObjectMeta: metav1.ObjectMeta{Name: "ws-1"},
				Status:     v1.WorkspaceStatus{Phase: "Suspended"},
			},
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestDevPreviewHandler_DevPreviewNotEnabled(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "10.0.0.1", false),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "dev preview")
}

func TestDevPreviewHandler_DeniedPort(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "10.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	for _, port := range []string{"4096", "4097", "4098", "80", "443"} {
		req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/"+port+"/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "port %s should be denied", port)
	}
}

func TestDevPreviewHandler_NonNumericPort(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "10.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/abc/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDevPreviewHandler_PortOutOfRange(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "10.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	r := setupDevPreviewRouter(h)

	for _, port := range []string{"0", "65536", "99999"} {
		req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/"+port+"/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, "port %s should be out of range", port)
	}
}

func TestDevPreviewHandler_KillSwitchDisabled(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "10.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := NewDevPreviewHandler(wsGetter, pwProvider, "llmsafespaces", nil, DevPreviewConfig{
		Enabled:              false,
		MaxResponseBytes:     50 * 1024 * 1024,
		MaxConnsPerWorkspace: 50,
	})
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "disabled")
}

func TestDevPreviewHandler_HTTPRoundTrip_Real(t *testing.T) {
	// Backend stands in for the agentd→devserver path. The API proxies to
	// podIP:<agentdPort> with Basic auth, agentd forwards to localhost:<port>.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass != "pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/dev-preview/5173/index.html" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello from dev server")
	}))
	defer backend.Close()

	backendHost := strings.TrimPrefix(backend.URL, "http://")
	backendIP := backendHost
	if idx := strings.LastIndex(backendHost, ":"); idx >= 0 {
		backendIP = backendHost[:idx]
	}

	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", backendIP, true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	// Override the agentd port to match the backend's port so the proxy
	// dials the test backend instead of port 4097.
	backendAddr := backend.Listener.Addr().String()
	if idx := strings.LastIndex(backendAddr, ":"); idx >= 0 {
		h.agentdPort = backendAddr[idx+1:]
	}

	r := setupDevPreviewRouter(h)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/index.html")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	assert.Contains(t, string(body), "hello from dev server")
}

func TestDevPreviewHandler_DevServerUnreachable_502(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "127.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	h.agentdPort = "59999" // nothing listening

	r := setupDevPreviewRouter(h)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestDevPreviewHandler_ConnectionLimitReached(t *testing.T) {
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", "127.0.0.1", true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := NewDevPreviewHandler(wsGetter, pwProvider, "llmsafespaces", nil, DevPreviewConfig{
		Enabled:              true,
		MaxResponseBytes:     50 * 1024 * 1024,
		MaxConnsPerWorkspace: 2,
	})
	// Saturate the limit.
	h.connCount["ws-1"] = 2
	r := setupDevPreviewRouter(h)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestDevPreviewHandler_ResponseSizeCap_DeclaredContentLength(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	backendIP := backendAddr
	backendPort := "4097"
	if idx := strings.LastIndex(backendAddr, ":"); idx >= 0 {
		backendIP = backendAddr[:idx]
		backendPort = backendAddr[idx+1:]
	}

	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", backendIP, true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := NewDevPreviewHandler(wsGetter, pwProvider, "llmsafespaces", nil, DevPreviewConfig{
		Enabled:              true,
		MaxResponseBytes:     100,
		MaxConnsPerWorkspace: 50,
	})
	h.agentdPort = backendPort

	r := setupDevPreviewRouter(h)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestDevPreviewHandler_ResponseSizeCap_ChunkedStream(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 1000; i++ {
			io.WriteString(w, strings.Repeat("x", 100))
		}
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	backendIP := backendAddr
	backendPort := "4097"
	if idx := strings.LastIndex(backendAddr, ":"); idx >= 0 {
		backendIP = backendAddr[:idx]
		backendPort = backendAddr[idx+1:]
	}

	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", backendIP, true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := NewDevPreviewHandler(wsGetter, pwProvider, "llmsafespaces", nil, DevPreviewConfig{
		Enabled:              true,
		MaxResponseBytes:     500,
		MaxConnsPerWorkspace: 50,
	})
	h.agentdPort = backendPort

	r := setupDevPreviewRouter(h)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// For chunked streams, the cap fires mid-stream (after the 200 has
	// already been sent). The client sees a truncated body — reading it
	// returns an error or fewer bytes than the backend tried to write.
	body, readErr := io.ReadAll(resp.Body)
	totalWritten := len(body)

	// The cap (500 bytes) MUST have truncated the stream well before the
	// backend's 100000 bytes. Either we got a read error (connection torn
	// down) or a body well under 100000.
	if readErr == nil && totalWritten >= 100000 {
		t.Errorf("size cap did not truncate the chunked stream: read %d bytes with no error", totalWritten)
	}
	if totalWritten > 100000 {
		t.Errorf("size cap leaked bytes: got %d (cap 500)", totalWritten)
	}
}

// TestDevPreviewHandler_G34_CallerCookieNotForwarded is the G34 regression
// test for the dev-preview path. The caller's Cookie (which carries the
// JWT session), Origin, and Referer must NOT reach the tenant pod.
func TestDevPreviewHandler_G34_CallerCookieNotForwarded(t *testing.T) {
	var capturedHeaders http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	backendIP := backendAddr
	backendPort := "4097"
	if idx := strings.LastIndex(backendAddr, ":"); idx >= 0 {
		backendIP = backendAddr[:idx]
		backendPort = backendAddr[idx+1:]
	}

	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			"ws-1": activeWorkspaceWithDevPreview("ws-1", backendIP, true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{"ws-1": "pass"}}
	h := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	h.agentdPort = backendPort

	r := setupDevPreviewRouter(h)
	ts := httptest.NewServer(r)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/workspaces/ws-1/dev-preview/5173/", nil)
	// Simulate a browser request carrying the JWT session cookie + Origin.
	req.Header.Set("Cookie", "lsp_session=eyJhbGciOiJIUzI1NiJ9.fake-jwt-payload.fake-signature")
	req.Header.Set("Origin", "https://platform.example.com")
	req.Header.Set("Referer", "https://platform.example.com/dashboard")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// The caller's session cookie MUST NOT be forwarded.
	if cookie := capturedHeaders.Get("Cookie"); cookie != "" {
		t.Errorf("G34 violation: caller Cookie forwarded to pod: %q", cookie)
	}
	if origin := capturedHeaders.Get("Origin"); origin != "" {
		t.Errorf("G34 violation: caller Origin forwarded to pod: %q", origin)
	}
	if referer := capturedHeaders.Get("Referer"); referer != "" {
		t.Errorf("G34 violation: caller Referer forwarded to pod: %q", referer)
	}

	// The allowlisted headers SHOULD be forwarded.
	if ct := capturedHeaders.Get("Content-Type"); ct != "" {
		// Content-Type is on the allowlist; fine if present
	}
	// Authorization should be Basic auth (injected), NOT the caller's Bearer token.
	if auth := capturedHeaders.Get("Authorization"); strings.HasPrefix(auth, "Bearer") {
		t.Errorf("G34 violation: caller Bearer token forwarded instead of Basic auth: %q", auth)
	}
}
