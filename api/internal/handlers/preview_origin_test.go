// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- fixtures ---

const (
	pvWS     = "0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b" // canonical UUID label
	pvDomain = "example.com"
	pvHost   = pvWS + "-preview." + pvDomain
)

// fakePVCache implements CacheStore with map-backed SetNX (one-time jti).
type fakePVCache struct {
	mu   sync.Mutex
	seen map[string]string
}

func (f *fakePVCache) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]string{}
	}
	if _, ok := f.seen[key]; ok {
		return false, nil
	}
	f.seen[key] = value
	return true, nil
}

func newPreviewOriginFixture(t *testing.T, backend *httptest.Server) (*PreviewOriginHandler, *gin.Engine, *DevPreviewHandler) {
	t.Helper()
	backendHost := strings.TrimPrefix(backend.URL, "http://")
	backendIP := backendHost
	if idx := strings.LastIndex(backendHost, ":"); idx >= 0 {
		backendIP = backendHost[:idx]
	}
	wsGetter := &devPreviewMockWorkspaceGetter{
		workspaces: map[string]*v1.Workspace{
			pvWS: activeWorkspaceWithDevPreview(pvWS, backendIP, true),
		},
	}
	pwProvider := &devPreviewMockPasswordProvider{passwords: map[string]string{pvWS: "pass"}}
	inner := newDevPreviewHandlerForTest(t, wsGetter, pwProvider)
	if addr := backend.Listener.Addr().String(); strings.LastIndex(addr, ":") >= 0 {
		inner.agentdPort = addr[strings.LastIndex(addr, ":")+1:]
	}
	pv := NewPreviewOriginHandler(inner, PreviewOriginConfig{
		Enabled:     true,
		BaseDomain:  pvDomain,
		TokenSecret: []byte("test-secret-key"),
	}, &fakePVCache{}, nil)

	r := gin.New()
	r.Use(pv.Middleware())
	// A stand-in for the API surface: proves non-preview hosts pass through
	// untouched and preview hosts never reach API routes.
	r.GET("/api/v1/anything", func(c *gin.Context) { c.String(200, "api") })
	r.GET("/api/v1/workspaces/:id/dev-preview-bootstrap/:port", pv.HandleBootstrap)
	r.GET("/api/v1/workspaces/:id/dev-preview-bootstrap", pv.HandleBootstrap)
	return pv, r, inner
}

// mintBootstrap runs the owner bootstrap endpoint and returns the Location.
func mintBootstrap(t *testing.T, r *gin.Engine, port string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+pvWS+"/dev-preview-bootstrap/"+port, nil)
	req.Host = "api." + pvDomain
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("bootstrap: expected 302, got %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://"+pvHost+"/") {
		t.Fatalf("bootstrap: location not on preview host: %q", loc)
	}
	return loc
}

// mintPortHostBootstrap runs the owner bootstrap endpoint and returns the Location
// for Epic 67 port-host format (the new primary format).
func mintPortHostBootstrap(t *testing.T, r *gin.Engine, port string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+pvWS+"/dev-preview-bootstrap/"+port, nil)
	req.Host = "api." + pvDomain
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("bootstrap: expected 302, got %d body=%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	expectedPrefix := "https://" + port + "-" + pvWS + "-preview." + pvDomain
	if !strings.HasPrefix(loc, expectedPrefix) {
		t.Fatalf("bootstrap: location not on port-host preview origin: expected prefix %q, got %q", expectedPrefix, loc)
	}
	return loc
}

func cookieFromRedemption(t *testing.T, r *gin.Engine, loc string) string {
	t.Helper()
	u := strings.TrimPrefix(loc, "https://"+pvHost)
	req := httptest.NewRequest("GET", u, nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redemption: expected 303, got %d body=%s", w.Code, w.Body.String())
	}
	sc := w.Header().Get("Set-Cookie")
	if sc == "" {
		t.Fatal("redemption: no Set-Cookie")
	}
	return sc
}

// --- host parsing ---

func TestPreviewOrigin_HostParsing(t *testing.T) {
	pv := &PreviewOriginHandler{cfg: PreviewOriginConfig{BaseDomain: pvDomain}}
	cases := []struct {
		host   string
		wantWS string
		wantOK bool
	}{
		{pvHost, pvWS, true},
		{pvHost + ":443", pvWS, true}, // port stripped
		{"api." + pvDomain, "", false},
		{"safespaces-preview." + pvDomain, "", false}, // non-UUID label
		{"garbage-preview." + pvDomain, "", false},
		{"-preview." + pvDomain, "", false},
		{"a.b-preview." + pvDomain, "", false},
		{"example.org", "", false},
		{pvWS + "-preview.other.org", "", false},
		{pvWS + "-example." + pvDomain, "", false},
	}
	for _, tc := range cases {
		ws, _, _, ok := pv.PreviewHost(tc.host)
		if ok != tc.wantOK || ws != tc.wantWS {
			t.Errorf("PreviewHost(%q) = (%q,%v), want (%q,%v)", tc.host, ws, ok, tc.wantWS, tc.wantOK)
		}
	}
	if !pv.IsMalformedPreviewHost("garbage-preview." + pvDomain) {
		t.Error("garbage-preview should be malformed")
	}
	if pv.IsMalformedPreviewHost(pvHost) {
		t.Error("valid preview host should not be malformed")
	}
	if pv.IsMalformedPreviewHost("api." + pvDomain) {
		t.Error("non-preview host should not be malformed")
	}
}

// --- middleware branching ---

func TestPreviewOrigin_APITrafficPassesThrough(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	for _, host := range []string{"api." + pvDomain, pvDomain, "chat." + pvDomain, "other.org"} {
		req := httptest.NewRequest("GET", "/api/v1/anything", nil)
		req.Host = host
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || w.Body.String() != "api" {
			t.Errorf("host %s: API route should pass through, got %d %q", host, w.Code, w.Body.String())
		}
	}
}

func TestPreviewOrigin_MalformedPreviewHost_421(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = "garbage-preview." + pvDomain
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("malformed preview host: expected 421, got %d", w.Code)
	}
}

func TestPreviewOrigin_Disabled_404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	pv, r, _ := newPreviewOriginFixture(t, backend)
	pv.cfg.Enabled = false

	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("disabled: expected 404 (fail-closed), got %d", w.Code)
	}
}

// --- bootstrap + redemption ---

func TestPreviewOrigin_BootstrapAndRedemption(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello-from-pod")
	}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	if !strings.Contains(loc, "?t=v1.") {
		t.Fatalf("bootstrap location missing token: %q", loc)
	}

	sc := cookieFromRedemption(t, r, loc)
	for _, want := range []string{"__Host-pv=", "Secure", "HttpOnly", "Path=/", "SameSite=Lax"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie missing %q: %q", want, sc)
		}
	}
	if strings.Contains(sc, "Domain=") {
		t.Errorf("__Host- cookie must not carry Domain: %q", sc)
	}

	// Cookie-authenticated proxy round trip.
	// Proxied responses must go through a real server: httputil.ReverseProxy
	// requires a ResponseWriter with CloseNotifier, which a bare
	// httptest.ResponseRecorder does not implement.
	ts := httptest.NewServer(r)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/5173/", nil)
	req.Host = pvHost
	req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cookie proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "hello-from-pod" {
		t.Fatalf("cookie proxy: got %d %q", resp.StatusCode, body)
	}
}

// closedPort grabs a free TCP port and leaves it closed.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func cookieValueOnly(setCookie string) string {
	part := strings.Split(setCookie, ";")[0]
	return strings.TrimPrefix(part, "__Host-pv=")
}

func TestPreviewOrigin_TokenReplay_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	u := strings.TrimPrefix(loc, "https://"+pvHost)

	for i, expect := range []int{http.StatusSeeOther, http.StatusUnauthorized} {
		req := httptest.NewRequest("GET", u, nil)
		req.Host = pvHost
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != expect {
			t.Fatalf("redemption %d: expected %d, got %d", i+1, expect, w.Code)
		}
	}
}

func TestPreviewOrigin_TokenTamperAndMismatch_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	u := strings.TrimPrefix(loc, "https://"+pvHost)

	// Tampered signature.
	bad := u[:strings.LastIndex(u, ".")] + "AAAA"
	req := httptest.NewRequest("GET", bad, nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("tampered token: expected 401, got %d", w.Code)
	}

	// Right signature, wrong host (token bound to pvWS; attacker replays on
	// a different workspace's preview host — host check must fail).
	otherWS := "11111111-2222-4333-8444-555566667777"
	req = httptest.NewRequest("GET", strings.Replace(u, "/5173/", "/5173/", 1), nil)
	req.Host = otherWS + "-preview." + pvDomain
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-workspace token replay: expected 401, got %d", w.Code)
	}
}

func TestPreviewOrigin_ExpiredToken_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	pv, r, _ := newPreviewOriginFixture(t, backend)

	pv.cfg.TokenTTL = -time.Minute // mint already-expired
	loc := mintBootstrap(t, r, "5173")
	u := strings.TrimPrefix(loc, "https://"+pvHost)

	req := httptest.NewRequest("GET", u, nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired token: expected 401, got %d", w.Code)
	}
}

func TestPreviewOrigin_NoCredentials_401(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no cookie, no token: expected 401, got %d", w.Code)
	}
}

func TestPreviewOrigin_CookieForOtherWorkspace_Rejected(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	// Mint a cookie legitimately bound to pvWS, then present it on another
	// workspace's preview host.
	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)

	otherWS := "11111111-2222-4333-8444-555566667777"
	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = otherWS + "-preview." + pvDomain
	req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-workspace cookie: expected 401, got %d", w.Code)
	}
}

// --- T5 + port policy ---

func TestPreviewOrigin_APIPathsNeverServed_T5(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)

	for _, path := range []string{"/api/v1/me", "/api/v1/workspaces/" + pvWS + "/secrets", "/api/anything"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = pvHost
		req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		// "api" is not a number → port parse fails → indistinguishable 502.
		// What matters for T5: NOT the API route's 200 "api" body.
		if w.Code == 200 && w.Body.String() == "api" {
			t.Errorf("T5 violation: preview host served API route %s", path)
		}
	}
}

func TestPreviewOrigin_BlockedPortIndistinguishableFromDead_T3(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, inner := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)

	// Real server: the dead-port path exercises the ReverseProxy error
	// handler, which needs a CloseNotifier-capable ResponseWriter.
	ts := httptest.NewServer(r)
	defer ts.Close()

	// The agentd stand-in (the shared backend) is ALIVE and would 200 any
	// port. Repoint the inner agentd hop at a genuinely closed port so the
	// proxy ErrorHandler produces the authentic dead-hop shape that the
	// blocklisted/invalid denials must match byte-for-byte.
	inner.agentdPort = strconv.Itoa(closedPort(t))

	get := func(path string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = pvHost
		req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	cBlocked, bBlocked := get("/4097/") // agentd mux — blocklisted
	if cBlocked != http.StatusBadGateway {
		t.Errorf("expected 502 shape for blocklisted port, got %d", cBlocked)
	}
	deadCode, deadBody := get("/59999/")
	nonNumCode, nonNumBody := get("/api/")
	privCode, privBody := get("/80/")
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"dead", deadCode, deadBody},
		{"non-numeric", nonNumCode, nonNumBody},
		{"privileged", privCode, privBody},
	} {
		if tc.code != cBlocked || tc.body != bBlocked {
			t.Errorf("T3: %s port (%d,%q) differs from blocklisted (%d,%q)", tc.name, tc.code, tc.body, cBlocked, bBlocked)
		}
	}
}

// --- P0 carries through the new pipeline ---

func TestPreviewOrigin_HTMLNoStoreCarries(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=3600")
		io.WriteString(w, "<html></html>")
	}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)

	ts := httptest.NewServer(r)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/5173/index.html", nil)
	req.Host = pvHost
	req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("P0-1 regression: HTML through preview origin must be no-store, got %q", cc)
	}
}

func TestPreviewOrigin_WSUpgradeHeadersForwarded(t *testing.T) {
	// Raw TCP (Go's client strips Connection headers): a full handshake sent
	// to the PREVIEW host must reach the backend with Connection/Upgrade
	// intact — proving the preview middleware does not disturb the P0-2
	// upgrade passthrough (full 101+echo is covered by the handler tests;
	// the middleware delegates to the same proxy).
	var gotConn, gotUp string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotConn = r.Header.Get("Connection")
		gotUp = r.Header.Get("Upgrade")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "plain")
	}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)

	ts := httptest.NewServer(r)
	defer ts.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /5173/ws HTTP/1.1\r\nHost: "+pvHost+"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: __Host-pv="+cookieValueOnly(sc)+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	if gotConn != "Upgrade" || gotUp != "websocket" {
		t.Errorf("upgrade headers mangled through preview pipeline: Connection=%q Upgrade=%q", gotConn, gotUp)
	}
}

// --- bootstrap input validation ---

func TestPreviewOrigin_BootstrapBadInput(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	for _, port := range []string{"abc", "80", "4097", "70000"} {
		req := httptest.NewRequest("GET", "/api/v1/workspaces/"+pvWS+"/dev-preview-bootstrap/"+port, nil)
		req.Host = "api." + pvDomain
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("bootstrap port %q: expected 400, got %d", port, w.Code)
		}
	}

	// Non-UUID workspace id → 400 (never reaches Location building).
	req := httptest.NewRequest("GET", "/api/v1/workspaces/not-a-uuid/dev-preview-bootstrap/5173", nil)
	req.Host = "api." + pvDomain
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bootstrap non-uuid ws: expected 400, got %d", w.Code)
	}
}

// --- landing page (human front door) ---

func TestPreviewOrigin_Landing_DeepLink_Navigation(t *testing.T) {
	// Unauthenticated browser navigation (Sec-Fetch-Mode: navigate — a
	// forbidden header for fetch/XHR, so only real navigations send it)
	// gets the landing page with a one-click bootstrap link for the port
	// in the URL — not a bare 401.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/5173/status.html", nil)
	req.Host = pvHost
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("navigation without credentials: expected 200 landing, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("landing must be HTML, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, `action="`) || !strings.Contains(body, `name="port"`) {
		t.Errorf("landing must render the editable port form; body excerpt: %.200s", body)
	}
	if !strings.Contains(body, `value="5173"`) {
		t.Errorf("deep-link landing must prefill the URL's port; body excerpt: %.200s", body)
	}
	if !strings.Contains(body, "Open preview") {
		t.Error("landing missing CTA label")
	}
	if !strings.Contains(body, "How dev preview works") {
		t.Error("landing missing the explainer")
	}
	if strings.Contains(body, "password") && strings.Contains(body, "input type=\"password\"") {
		t.Error("SECURITY: landing must never contain a password field")
	}
	if w.Header().Get("X-Robots-Tag") == "" {
		t.Error("landing must set X-Robots-Tag")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("landing must be no-store")
	}
}

func TestPreviewOrigin_Landing_NonNavigation_Still401(t *testing.T) {
	// Same URL without the navigation header (fetch/XHR/curl): unchanged
	// 401 — subresources and API clients must not receive HTML.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = pvHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("non-navigation without credentials: expected 401, got %d", w.Code)
	}
}

func TestPreviewOrigin_Landing_BareRoot_Form(t *testing.T) {
	// Bare host root (no port in path): landing with a no-JS GET form
	// whose action is the bootstrap ?port= query form.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = pvHost
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("bare root navigation: expected 200 landing, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<form") || !strings.Contains(body, `name="port"`) {
		t.Error("bare-root landing must contain the port form")
	}
	if !strings.Contains(body, "/dev-preview-bootstrap") {
		t.Error("form action must target the bootstrap endpoint")
	}
}

func TestPreviewOrigin_Landing_ExpiredCookie_Navigation(t *testing.T) {
	// An invalid/expired cookie on a navigation falls through to the
	// landing (not a bare 401) — the common "7-day cookie expired while
	// deep-linking" path.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/5173/", nil)
	req.Host = pvHost
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Cookie", previewCookieName+"=v1.corrupted.signature")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Open preview") {
		t.Fatalf("invalid-cookie navigation: expected landing, got %d", w.Code)
	}
}

func TestPreviewOrigin_Token_Precedence_Over_Landing(t *testing.T) {
	// A navigation carrying a valid ?t= must redeem (303) — the landing
	// must never shadow the bootstrap redirect (every bootstrap open IS
	// a navigation).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	loc := mintBootstrap(t, r, "5173")
	u := strings.TrimPrefix(loc, "https://"+pvHost)
	req := httptest.NewRequest("GET", u, nil)
	req.Host = pvHost
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("token redemption must win over landing: expected 303, got %d", w.Code)
	}
}

func TestPreviewOrigin_Bootstrap_QueryPortForm(t *testing.T) {
	// /dev-preview-bootstrap?port=N — the landing form's target. Behaves
	// identically to the path form.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+pvWS+"/dev-preview-bootstrap?port=5173", nil)
	req.Host = "api." + pvDomain
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "https://"+pvHost+"/5173/") {
		t.Fatalf("query-form bootstrap: expected 302 to preview host, got %d %q", w.Code, w.Header().Get("Location"))
	}

	// Invalid ports rejected exactly like the path form.
	for _, q := range []string{"port=abc", "port=80", "port=4097"} {
		req = httptest.NewRequest("GET", "/api/v1/workspaces/"+pvWS+"/dev-preview-bootstrap?"+q, nil)
		req.Host = "api." + pvDomain
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("query bootstrap %q: expected 400, got %d", q, w.Code)
		}
	}
}

// --- Phase 2: relaxed CSP on preview origins (DESIGN §5.4) ---

// previewOriginCSPFixture returns a fixture router plus the constructed
// handler (for frameAncestors variants).
func previewOriginCSPFixture(t *testing.T, frameAncestors []string) (*PreviewOriginHandler, *gin.Engine) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The dev server sets its OWN CSP — the edge policy must win (Set,
		// not Add: exactly one header, ours).
		w.Header().Set("Content-Security-Policy", "default-src http:")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html></html>")
	}))
	t.Cleanup(backend.Close)
	pv, r, _ := newPreviewOriginFixture(t, backend)
	pv.cfg.FrameAncestors = frameAncestors
	fa := "'none'"
	if len(frameAncestors) > 0 {
		fa = strings.Join(frameAncestors, " ")
	}
	pv.csp = previewRelaxedCSPBase + fa
	// Mirror the constructor's landingCSP derivation (form-action extended
	// to the API bootstrap origin).
	apiOrigin := pv.cfg.APIOriginURL
	if apiOrigin == "" {
		apiOrigin = "https://api." + pvDomain
	}
	pv.landingCSP = strings.Replace(pv.csp, "form-action 'self'", "form-action 'self' "+apiOrigin, 1)
	return pv, r
}

func previewOriginCSPGet(t *testing.T, r *gin.Engine, path string) *http.Response {
	t.Helper()
	loc := mintBootstrap(t, r, "5173")
	sc := cookieFromRedemption(t, r, loc)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Host = pvHost
	req.Header.Set("Cookie", "__Host-pv="+cookieValueOnly(sc))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp
}

func TestPreviewOrigin_CSP_RelaxedPolicyOnProxiedResponses(t *testing.T) {
	_, r := previewOriginCSPFixture(t, nil)
	resp := previewOriginCSPGet(t, r, "/5173/index.html")

	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("P2: preview-origin responses must carry a CSP")
	}
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' 'unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"connect-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		// Token boundary: proxied app pages keep STRICT form-action
		// ("form-action 'self';" — directive ends immediately). The
		// landing's extended form-action is "form-action 'self' <origin>;"
		// and must never appear here.
		"form-action 'self';",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got: %s", want, csp)
		}
	}
	// THE load-bearing constraint: no bare ws:/wss: token anywhere —
	// connect-src stays 'self' (CSP3: 'self' covers same-origin wss).
	// A bare scheme would open an exfil channel and defeat the point.
	if strings.Contains(csp, "ws:") || strings.Contains(csp, "wss:") {
		t.Errorf("CSP must not contain bare ws:/wss: (exfil channel); got: %s", csp)
	}
	// Edge policy replaces, not merges with, the dev server's.
	if strings.Contains(csp, "http:") {
		t.Errorf("dev-server CSP leaked through; got: %s", csp)
	}
	if got := len(resp.Header.Values("Content-Security-Policy")); got != 1 {
		t.Errorf("exactly one CSP header expected (Set, not Add); got %d", got)
	}
}

func TestPreviewOrigin_CSP_FrameAncestorsConfig(t *testing.T) {
	_, r := previewOriginCSPFixture(t, []string{"https://chat.safespaces.dev"})
	resp := previewOriginCSPGet(t, r, "/5173/index.html")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://chat.safespaces.dev") {
		t.Errorf("configured frame-ancestors missing; got: %s", csp)
	}
	if strings.Contains(csp, "frame-ancestors 'none' https") {
		t.Errorf("'none' must not combine with explicit origins; got: %s", csp)
	}
}

func TestPreviewOrigin_CSP_LandingCarriesPolicy(t *testing.T) {
	_, r := previewOriginCSPFixture(t, nil)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = pvHost
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("landing: expected 200, got %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'unsafe-inline' 'unsafe-eval'") {
		t.Errorf("landing missing relaxed CSP; got: %q", csp)
	}
	// REGRESSION (landing form was blocked by its own CSP): form-action on
	// the LANDING must include the bootstrap API origin — the port form
	// submits there cross-origin.
	if !strings.Contains(csp, "form-action 'self' https://api."+pvDomain) {
		t.Errorf("landing form-action must allowlist the API origin; got: %q", csp)
	}
}

func TestDevPreviewHandler_PathRouteInjectsNoCSP(t *testing.T) {
	// The PATH-BASED route keeps the API's strict SecurityMiddleware
	// policy (which supplies CSP upstream of this handler). The proxy
	// itself must inject nothing — verifying proxyToAgentd("") cleanliness.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer backend.Close()

	ts := httptest.NewServer(newDevPreviewRoundTripRouter(t, backend))
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/workspaces/ws-1/dev-preview/5173/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "" {
		t.Errorf("path-based proxy must not inject CSP; got: %q", csp)
	}
}

// Epic 67 integration test: Validate port-host format end-to-end
func TestEpic67PortHostBootstrapAndRedemption(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo back the request path for validation
		io.WriteString(w, "OK: "+r.URL.Path)
	}))
	defer backend.Close()

	_, r, _ := newPreviewOriginFixture(t, backend)

	// 1. Bootstrap to port-host format
	port := "8080"
	loc := mintPortHostBootstrap(t, r, port)
	t.Logf("Bootstrap location: %s", loc)

	// 2. Validate location format
	expectedPrefix := "https://" + port + "-" + pvWS + "-preview." + pvDomain
	if !strings.HasPrefix(loc, expectedPrefix) {
		t.Fatalf("expected location to start with %q, got %q", expectedPrefix, loc)
	}

	// 3. Parse token from location
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("failed to parse location: %v", err)
	}
	token := u.Query().Get("t")
	if token == "" {
		t.Fatalf("location missing token: %q", loc)
	}

	// 4. Redeem token on port-host
	portHost := u.Host
	reqURL := "/?t=" + url.QueryEscape(token)
	req := httptest.NewRequest("GET", reqURL, nil)
	req.Host = portHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("token redemption: expected 303, got %d body=%s", w.Code, w.Body.String())
	}

	sc := w.Header().Get("Set-Cookie")
	if sc == "" {
		t.Fatal("redemption: no Set-Cookie")
	}

	// 5. Validate cookie
	if !strings.Contains(sc, "__Host-pv=") {
		t.Errorf("missing __Host-pv cookie: %q", sc)
	}

	// 6. Test authenticated request to port-host
	cookieValue := cookieValueOnly(sc)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Host = portHost
	req2.Header.Set("Cookie", "__Host-pv="+cookieValue)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	// Should proxy to backend successfully
	if w2.Code != http.StatusOK {
		t.Fatalf("authenticated request: expected 200, got %d body=%s", w2.Code, w2.Body.String())
	}

	body := w2.Body.String()
	if !strings.Contains(body, "OK: /"+port) {
		t.Errorf("expected backend path /%s, got response body: %q", port, body)
	}
}

// Epic 67 test: Validate root requests work correctly on port-hosts
func TestEpic67PortHostRootRequest(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			io.WriteString(w, "Root page")
		} else {
			io.WriteString(w, "Other page: "+r.URL.Path)
		}
	}))
	defer backend.Close()

	_, r, _ := newPreviewOriginFixture(t, backend)

	// Bootstrap and get cookie
	port := "3000"
	loc := mintPortHostBootstrap(t, r, port)
	u, _ := url.Parse(loc)
	token := u.Query().Get("t")

	portHost := u.Host

	// Redeem token
	req := httptest.NewRequest("GET", "/?t="+url.QueryEscape(token), nil)
	req.Host = portHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	sc := w.Header().Get("Set-Cookie")
	cookieValue := cookieValueOnly(sc)

	// Test root request on port-host (should go directly to app, not landing page)
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Host = portHost
	req2.Header.Set("Cookie", "__Host-pv="+cookieValue)
	req2.Header.Set("Sec-Fetch-Mode", "navigate")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("port-host root request: expected 200, got %d body=%s", w2.Code, w2.Body.String())
	}

	body := w2.Body.String()
	if !strings.Contains(body, "Root page") {
		t.Errorf("port-host root should return app root, got: %q", body)
	}
}

// Epic 67 test: Validate path preservation on port-hosts
func TestEpic67PortHostPathPreservation(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "Path: "+r.URL.Path)
	}))
	defer backend.Close()

	_, r, _ := newPreviewOriginFixture(t, backend)

	port := "5173"
	loc := mintPortHostBootstrap(t, r, port)
	u, _ := url.Parse(loc)
	token := u.Query().Get("t")
	portHost := u.Host

	// Redeem token
	req := httptest.NewRequest("GET", "/?t="+url.QueryEscape(token), nil)
	req.Host = portHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	sc := w.Header().Get("Set-Cookie")
	cookieValue := cookieValueOnly(sc)

	// Test various paths on port-host
	testPaths := []string{
		"/app",
		"/app/dashboard",
		"/api/v1/users",
		"/static/style.css",
	}

	for _, path := range testPaths {
		reqPath := httptest.NewRequest("GET", path, nil)
		reqPath.Host = portHost
		reqPath.Header.Set("Cookie", "__Host-pv="+cookieValue)
		wPath := httptest.NewRecorder()
		r.ServeHTTP(wPath, reqPath)

		if wPath.Code != http.StatusOK {
			t.Errorf("path %s: expected 200, got %d", path, wPath.Code)
			continue
		}

		body := wPath.Body.String()
		expectedPath := "/" + port + path
		if !strings.Contains(body, "Path: "+expectedPath) {
			t.Errorf("path %s: expected backend to see %s, got: %q", path, expectedPath, body)
		}
	}
}
