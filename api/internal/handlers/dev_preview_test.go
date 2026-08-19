// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gorilla/websocket"
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

	// Authorization should be Basic auth (injected), NOT the caller's Bearer token.
	if auth := capturedHeaders.Get("Authorization"); strings.HasPrefix(auth, "Bearer") {
		t.Errorf("G34 violation: caller Bearer token forwarded instead of Basic auth: %q", auth)
	}
}

func TestDevPreviewHandler_WebSocketUpgrade_RoundTrip(t *testing.T) {
	// P0-2 (redesign-2026-08-19): WS upgrades must traverse the API edge
	// end-to-end. The G34 header wipe previously stripped
	// Connection/Upgrade, degrading handshakes to plain GETs at the dev
	// server (HMR broken in the field). Echo round-trip through the full
	// chain: client -> API ReverseProxy -> agentd stand-in -> echo server.
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Backend stands in for the agentd hop: it receives the agentd
		// path and performs the upgrade exactly as agentd's ReverseProxy
		// would forward it to the dev server.
		if r.URL.Path != "/v1/dev-preview/5173/ws" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Connection") != "Upgrade" || r.Header.Get("Upgrade") != "websocket" {
			// The field-observed failure mode: upgrade headers stripped.
			http.Error(w, "not a websocket upgrade: Connection="+r.Header.Get("Connection")+" Upgrade="+r.Header.Get("Upgrade"), http.StatusBadRequest)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("backend upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(mt, msg); err != nil {
			return
		}
	}))
	defer backend.Close()

	r := newDevPreviewRoundTripRouter(t, backend)
	ts := httptest.NewServer(r)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/workspaces/ws-1/dev-preview/5173/ws"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		body := ""
		if resp != nil && resp.Body != nil {
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
		}
		t.Fatalf("WS dial through proxy failed: %v (status=%v body=%s)", err, resp, body)
	}
	defer conn.Close()

	sent := []byte("hello-through-the-tunnel")
	if err := conn.WriteMessage(websocket.TextMessage, sent); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(got) != string(sent) {
		t.Fatalf("echo mismatch: sent %q got %q", sent, got)
	}
}

// newDevPreviewRoundTripRouter wires a handler+router against a live test
// backend standing in for agentd, with the agentd port overridden.
func newDevPreviewRoundTripRouter(t *testing.T, backend *httptest.Server) *gin.Engine {
	t.Helper()
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
	if addr := backend.Listener.Addr().String(); strings.LastIndex(addr, ":") >= 0 {
		h.agentdPort = addr[strings.LastIndex(addr, ":")+1:]
	}
	return setupDevPreviewRouter(h)
}

func TestRequestUpgradeType(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want string
	}{
		{"standard upgrade", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, "websocket"},
		{"connection comma list", http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"websocket"}}, "websocket"},
		{"case-insensitive tokens", http.Header{"Connection": {"upgrade"}, "Upgrade": {"WebSocket"}}, "WebSocket"},
		{"non-upgrade connection", http.Header{"Connection": {"keep-alive"}, "Upgrade": {"websocket"}}, ""},
		{"missing connection", http.Header{"Upgrade": {"websocket"}}, ""},
		{"missing upgrade value", http.Header{"Connection": {"Upgrade"}}, ""},
		{"empty headers", http.Header{}, ""},
		{"other protocol", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"h2c"}}, "h2c"},
		{"whitespace padded tokens", http.Header{"Connection": {"  Upgrade  "}, "Upgrade": {"websocket"}}, "websocket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestUpgradeType(tc.h); got != tc.want {
				t.Errorf("requestUpgradeType(%v) = %q, want %q", tc.h, got, tc.want)
			}
		})
	}
}

func TestHeaderContainsToken(t *testing.T) {
	cases := []struct {
		name   string
		values []string
		want   bool
	}{
		{"single exact", []string{"upgrade"}, true},
		{"single case-mixed", []string{"Upgrade"}, true},
		{"comma list", []string{"keep-alive, Upgrade"}, true},
		{"comma list no space", []string{"keep-alive,Upgrade"}, true},
		{"whitespace padded", []string{"  upgrade  "}, true},
		{"multiple header values", []string{"keep-alive", "upgrade"}, true},
		{"no match", []string{"keep-alive"}, false},
		{"empty slice", nil, false},
		{"empty string", []string{""}, false},
		{"substring is not a token", []string{"upgrades"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerContainsToken(tc.values, "upgrade"); got != tc.want {
				t.Errorf("headerContainsToken(%q, upgrade) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestDevPreviewHandler_PlainGetNotUpgraded(t *testing.T) {
	// P0-2 edge: a request WITHOUT upgrade headers must pass through as a
	// plain GET — the Rewrite fix must not synthesize upgrade headers for
	// non-upgrade traffic (partial header set: neither Connection nor
	// Upgrade present).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connection") == "Upgrade" || r.Header.Get("Upgrade") != "" {
			http.Error(w, "unexpected upgrade headers on plain request", http.StatusBadRequest)
			return
		}
		io.WriteString(w, "plain-response")
	}))
	defer backend.Close()

	r := newDevPreviewRoundTripRouter(t, backend)
	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/ws")
	if err != nil {
		t.Fatalf("plain GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "plain-response" {
		t.Fatalf("plain GET mangled: status=%d body=%q", resp.StatusCode, body)
	}
}

func TestDevPreviewHandler_PartialUpgradeHeaders_ForwardedFaithfully(t *testing.T) {
	// P0-2 edge: partial/malformed handshakes. Sent over raw TCP because
	// Go's client transport strips hop-by-hop headers (Connection et al.)
	// from outbound requests. Two properties:
	//  a) a full-but-malformed handshake (bad Sec-WebSocket-Version) is
	//     forwarded faithfully; the dev server's 400 + corrected version
	//     header must propagate back unchanged.
	//  b) a partial header set (Connection: Upgrade, no Upgrade value) is
	//     NOT treated as an upgrade — no headers are synthesized, the
	//     request proxies as plain HTTP.
	var gotConn, gotVersion string
	var gotPlainGet int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Sec-WebSocket-Version") != "":
			gotConn = r.Header.Get("Connection")
			gotVersion = r.Header.Get("Sec-WebSocket-Version")
			w.Header().Set("Sec-WebSocket-Version", "13")
			http.Error(w, "malformed handshake", http.StatusBadRequest)
		default:
			gotPlainGet++
			if r.Header.Get("Upgrade") != "" || r.Header.Get("Connection") == "Upgrade" {
				http.Error(w, "upgrade headers synthesized for partial set", http.StatusBadRequest)
				return
			}
			io.WriteString(w, "plain")
		}
	}))
	defer backend.Close()

	r := newDevPreviewRoundTripRouter(t, backend)
	ts := httptest.NewServer(r)
	defer ts.Close()

	rawRequest := func(headers string) (int, http.Header) {
		t.Helper()
		conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, "GET /api/v1/workspaces/ws-1/dev-preview/5173/ws HTTP/1.1\r\nHost: t\r\n"+headers+"\r\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header
	}

	// (a) full handshake with invalid version → backend 400 propagates.
	code, hdr := rawRequest("Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 8\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
	if code != http.StatusBadRequest {
		t.Fatalf("dev server 400 must propagate, got %d", code)
	}
	if hdr.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("corrected version header must propagate, got %q", hdr.Get("Sec-WebSocket-Version"))
	}
	if gotConn != "Upgrade" {
		t.Errorf("Connection not forwarded faithfully on upgrade: %q", gotConn)
	}
	if gotVersion != "8" {
		t.Errorf("Sec-WebSocket-Version not forwarded faithfully: %q", gotVersion)
	}

	// (b) partial set → plain proxying, nothing synthesized.
	code, _ = rawRequest("Connection: Upgrade\r\n")
	if code != http.StatusOK {
		t.Fatalf("partial header set should proxy as plain request, got %d", code)
	}
	if gotPlainGet != 1 {
		t.Errorf("expected exactly one plain GET at backend, got %d", gotPlainGet)
	}
}

func TestDevPreviewHandler_WSBackendRejectsUpgrade_Propagates(t *testing.T) {
	// P0-2 unhappy path: the dev server refuses the upgrade (e.g. endpoint
	// only serves HTTP). The WS client must see the backend's non-101
	// response (bad handshake), not a hang or a synthesized error.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no websocket here", http.StatusForbidden)
	}))
	defer backend.Close()

	r := newDevPreviewRoundTripRouter(t, backend)
	ts := httptest.NewServer(r)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/workspaces/ws-1/dev-preview/5173/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("dial should fail when backend rejects the upgrade")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("backend 403 must propagate, got resp=%v", resp)
	}
}

func TestDevPreviewHandler_WSUnreachablePort_502(t *testing.T) {
	// P0-2 unhappy path: nothing listening behind the agentd hop → the
	// proxy's ErrorHandler must answer 502 promptly (no hang), including
	// for upgrade requests.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backendAddr := backend.Listener.Addr().String()
	backend.Close() // free the port; nothing listens now

	backendHost := backendAddr
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
	if idx := strings.LastIndex(backendAddr, ":"); idx >= 0 {
		h.agentdPort = backendAddr[idx+1:]
	}
	ts := httptest.NewServer(setupDevPreviewRouter(h))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/workspaces/ws-1/dev-preview/5173/ws"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		t.Fatal("dial should fail when nothing is listening")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 from ErrorHandler, got resp=%v", resp)
	}
}
