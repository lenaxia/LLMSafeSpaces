// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Epic 67 unit + workflow tests. Reuses the Epic 66 fixture
// (newPreviewOriginFixture, mintPortHostBootstrap, cookieValueOnly) from
// preview_origin_test.go — same package, no redeclarations.
//
// Proxy-reaching requests always go through a REAL httptest.NewServer
// (never a bare ResponseRecorder): httputil.ReverseProxy needs
// CloseNotifier/deadline support the recorder lacks.

const (
	epic67TestDomain = "epic67.test.example.com"
	epic67TestWS     = "a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d"
)

// Byte-exact T3 body: blocked/privileged/dead ports must be
// indistinguishable (THREAT-MODEL T3).
const epic67UnreachableBody = "workspace dev-preview endpoint unreachable\n"

var (
	epic67NewRE    = regexp.MustCompile(`^[0-9]{1,5}-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.` + regexp.QuoteMeta(epic67TestDomain) + `$`)
	epic67LegacyRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-preview\.` + regexp.QuoteMeta(epic67TestDomain) + `$`)
)

// --- host parsing ---

// TestEpic67LegacyHostBackwardCompatibility ensures legacy hosts still parse.
func TestEpic67LegacyHostBackwardCompatibility(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:     true,
		BaseDomain:  epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}
	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	legacyHost := epic67TestWS + "-preview." + epic67TestDomain
	wsID, port, isPortHost, ok := h.PreviewHost(legacyHost)

	if !ok {
		t.Fatal("Legacy host should be recognized")
	}
	if wsID != epic67TestWS {
		t.Errorf("workspace ID mismatch: got %q want %q", wsID, epic67TestWS)
	}
	if port != 0 {
		t.Errorf("legacy hosts must have port=0, got %d", port)
	}
	if isPortHost {
		t.Error("legacy hosts must have isPortHost=false")
	}
}

// TestEpic67PortHostParsing validates port extraction from port-hosts,
// including the F1 digit-leading-UUID population.
func TestEpic67PortHostParsing(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:     true,
		BaseDomain:  epic67TestDomain,
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
		{"Vite default port", "5173-" + epic67TestWS + "-preview." + epic67TestDomain, epic67TestWS, 5173, true, true},
		{"Express default port", "3000-" + epic67TestWS + "-preview." + epic67TestDomain, epic67TestWS, 3000, true, true},
		{"Max valid port", "65535-" + epic67TestWS + "-preview." + epic67TestDomain, epic67TestWS, 65535, true, true},
		{"Single digit port", "1-" + epic67TestWS + "-preview." + epic67TestDomain, epic67TestWS, 1, true, true},
		{"F1 digit-leading UUID", "1044-1044f4f2-1234-5678-9abc-def000000000-preview." + epic67TestDomain, "1044f4f2-1234-5678-9abc-def000000000", 1044, true, true},
		{"F1 all-digit first segment", "65535-99999999-1234-5678-9abc-def000000000-preview." + epic67TestDomain, "99999999-1234-5678-9abc-def000000000", 65535, true, true},
		{"Wrong domain", "5173-" + epic67TestWS + "-preview.wrong-domain.com", "", 0, false, false},
		{"Port too large (6 digits)", "65536-" + epic67TestWS + "-preview." + epic67TestDomain, "", 0, false, false},
		{"Non-numeric port", "abc-" + epic67TestWS + "-preview." + epic67TestDomain, "", 0, false, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wsID, port, isPortHost, ok := h.PreviewHost(tc.host)
			if ok != tc.expectedOk {
				t.Fatalf("ok = %v, want %v", ok, tc.expectedOk)
			}
			if !tc.expectedOk {
				return
			}
			if wsID != tc.expectedWSID || port != tc.expectedPort || isPortHost != tc.expectedIsPort {
				t.Errorf("got (ws=%q port=%d isPortHost=%v), want (ws=%q port=%d isPortHost=%v)",
					wsID, port, isPortHost, tc.expectedWSID, tc.expectedPort, tc.expectedIsPort)
			}
		})
	}
}

// --- bootstrap redirect ---

// TestEpic67BootstrapRedirect validates the owner bootstrap redirects to
// the port-host shape and never the legacy path shape.
func TestEpic67BootstrapRedirect(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	for _, port := range []string{"5173", "3000", "65535", "1024"} {
		loc := mintPortHostBootstrap(t, r, port) // asserts prefix + 302 internally
		if !strings.Contains(loc, "?t=v1.") {
			t.Errorf("port %s: bootstrap location missing token: %q", port, loc)
		}
		// Must NOT be the legacy path shape.
		legacyPrefix := "https://" + pvWS + "-preview." + pvDomain + "/" + port
		if strings.HasPrefix(loc, legacyPrefix) {
			t.Errorf("port %s: bootstrap must use port-host shape, got legacy: %q", port, loc)
		}
	}
}

// --- landing page gating ---

// TestEpic67LandingPageBehavior: legacy bare-root keeps the Epic 66 landing
// page; port-host root with a session proxies to the APP root (the agentd
// hop receives the full preview path including the port segment).
func TestEpic67LandingPageBehavior(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stand-in for agentd: receives /v1/dev-preview/<port><subPath>.
		io.WriteString(w, "AGENTD:"+r.URL.Path)
	}))
	// t.Cleanup (not defer): parallel subtests outlive the parent function,
	// and a deferred Close would tear the backend down mid-subtest.
	t.Cleanup(backend.Close)
	_, r, _ := newPreviewOriginFixture(t, backend)

	t.Run("legacy_host_root_gets_landing_page", func(t *testing.T) {
		t.Parallel()
		// serveLanding renders directly (no proxy) — recorder is safe.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = pvHost
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("legacy root: expected 200 landing, got %d body=%s", w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, "Workspace dev preview") {
			t.Errorf("legacy root: expected landing page content, got %q", body)
		}
	})

	t.Run("port_host_root_goes_to_app", func(t *testing.T) {
		t.Parallel()
		loc := mintPortHostBootstrap(t, r, "5173")
		u, _ := url.Parse(loc)
		portHost := u.Host

		// Redeem token (303 + cookie; no proxy — recorder safe).
		req := httptest.NewRequest(http.MethodGet, "/?t="+url.QueryEscape(u.Query().Get("t")), nil)
		req.Host = portHost
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("redemption: expected 303, got %d body=%s", w.Code, w.Body.String())
		}
		cookieVal := cookieValueOnly(w.Header().Get("Set-Cookie"))

		// Authenticated root on port-host → app root via REAL server
		// (ReverseProxy needs CloseNotifier support).
		ts := httptest.NewServer(r)
		defer ts.Close()
		req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req2.Host = portHost
		req2.Header.Set("Cookie", "__Host-pv="+cookieVal)
		req2.Header.Set("Sec-Fetch-Mode", "navigate")
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("port-host root: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("port-host root: expected 200, got %d body=%s", resp.StatusCode, body)
		}
		if strings.Contains(string(body), "Workspace dev preview") {
			t.Errorf("port-host root must NOT serve the landing page, got %q", body)
		}
		// The agentd hop carries the port segment; the APP path is "/".
		if want := "AGENTD:/v1/dev-preview/5173/"; !strings.Contains(string(body), want) {
			t.Errorf("agentd hop path: expected %q in %q", want, body)
		}
	})
}

// --- token binding ---

// TestEpic67TokenBindingMismatch: a token minted for one port must be
// rejected when redeemed on a different port's host (preview_origin.go
// token-binding check), for multiple port pairs.
func TestEpic67TokenBindingMismatch(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	for _, pair := range [][2]string{{"5173", "3000"}, {"3000", "8080"}, {"8080", "5173"}} {
		minted, replay := pair[0], pair[1]
		loc := mintPortHostBootstrap(t, r, minted)
		u, _ := url.Parse(loc)
		token := u.Query().Get("t")

		// Redeem on the WRONG port host.
		wrongHost := replay + "-" + pvWS + "-preview." + pvDomain
		req := httptest.NewRequest(http.MethodGet, "/?t="+url.QueryEscape(token), nil)
		req.Host = wrongHost
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("token(%s) on host(%s): expected 401, got %d body=%s",
				minted, replay, w.Code, w.Body.String())
		}
	}
}

// --- cookie scoping (F5) ---

// TestEpic67CookieScopingPerPort: __Host-pv is a HOST-only cookie, so a
// browser never sends a 5173-host cookie to a 3000-host. Simulating that
// (no Cookie header on the second host): navigations get the deep-link
// landing page (one-click re-bootstrap), non-navigations get 401.
func TestEpic67CookieScopingPerPort(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "APP")
	}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	// Bootstrap + redeem on 5173.
	loc := mintPortHostBootstrap(t, r, "5173")
	u, _ := url.Parse(loc)
	req := httptest.NewRequest(http.MethodGet, "/?t="+url.QueryEscape(u.Query().Get("t")), nil)
	req.Host = u.Host
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("5173 redemption: expected 303, got %d", w.Code)
	}
	if cookieValueOnly(w.Header().Get("Set-Cookie")) == "" {
		t.Fatal("5173 redemption: no cookie set")
	}

	// Same workspace, different port host, browser sends NO cookie.
	otherHost := "3000-" + pvWS + "-preview." + pvDomain

	nav := httptest.NewRequest(http.MethodGet, "/", nil)
	nav.Host = otherHost
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	wn := httptest.NewRecorder()
	r.ServeHTTP(wn, nav)
	if wn.Code != http.StatusOK || !strings.Contains(wn.Body.String(), "Workspace dev preview") {
		t.Errorf("3000 navigation without cookie: expected 200 landing, got %d body=%s", wn.Code, wn.Body.String())
	}

	api := httptest.NewRequest(http.MethodGet, "/", nil)
	api.Host = otherHost
	wa := httptest.NewRecorder()
	r.ServeHTTP(wa, api)
	if wa.Code != http.StatusUnauthorized {
		t.Errorf("3000 non-navigation without cookie: expected 401, got %d", wa.Code)
	}
}

// --- core motivation: root-absolute redirects ---

// TestEpic67RootAbsoluteRedirectWorkflow is THE Epic 67 regression test
// (tinyrsvp incident): an app emitting 303 Location: /login must complete
// the round trip on a port-host — the Location stays root-absolute because
// the port lives in the host. On a LEGACY host the same root-absolute URL
// loses the /<port>/ prefix and dies with the indistinguishable 502.
func TestEpic67RootAbsoluteRedirectWorkflow(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/dev-preview/5173/":
			// The app's root emits a ROOT-ABSOLUTE redirect (the breakage class).
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusSeeOther)
		case "/v1/dev-preview/5173/login":
			io.WriteString(w, "Login page loaded successfully")
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "404 Not Found")
		}
	}))
	defer backend.Close()
	_, r, _ := newPreviewOriginFixture(t, backend)

	// 1. Bootstrap + redeem → session cookie.
	loc := mintPortHostBootstrap(t, r, "5173")
	u, _ := url.Parse(loc)
	portHost := u.Host
	req := httptest.NewRequest(http.MethodGet, "/?t="+url.QueryEscape(u.Query().Get("t")), nil)
	req.Host = portHost
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redemption: expected 303, got %d body=%s", w.Code, w.Body.String())
	}
	cookieVal := cookieValueOnly(w.Header().Get("Set-Cookie"))

	ts := httptest.NewServer(r)
	defer ts.Close()

	// A client that does NOT auto-follow: the 303's Location is the object
	// under test — following it would hide exactly what we must assert.
	noRedirect := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// 2. GET / on the port-host → app's 303 must surface VERBATIM.
	reqRoot, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	reqRoot.Host = portHost
	reqRoot.Header.Set("Cookie", "__Host-pv="+cookieVal)
	respRoot, err := noRedirect.Do(reqRoot)
	if err != nil {
		t.Fatalf("root request: %v", err)
	}
	defer respRoot.Body.Close()
	io.ReadAll(respRoot.Body)

	if respRoot.StatusCode != http.StatusSeeOther {
		t.Fatalf("app root: expected 303, got %d", respRoot.StatusCode)
	}
	if got := respRoot.Header.Get("Location"); got != "/login" {
		t.Fatalf("root-absolute Location NOT preserved: got %q, want exactly \"/login\" — "+
			"the browser must resolve it against %q so the port survives", got, portHost)
	}

	// 3. Browser follows /login against the SAME port-host → app page loads.
	reqLogin, _ := http.NewRequest(http.MethodGet, ts.URL+"/login", nil)
	reqLogin.Host = portHost
	reqLogin.Header.Set("Cookie", "__Host-pv="+cookieVal)
	respLogin, err := noRedirect.Do(reqLogin)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer respLogin.Body.Close()
	body, _ := io.ReadAll(respLogin.Body)
	if respLogin.StatusCode != http.StatusOK || !strings.Contains(string(body), "Login page loaded successfully") {
		t.Fatalf("login round trip failed: %d %q", respLogin.StatusCode, body)
	}

	// 4. CONTRAST — the same root-absolute URL on the LEGACY host: the
	// /<port>/ prefix is gone, port parsing fails, T3 502. This is the
	// exact Epic 66 breakage Epic 67 fixes.
	reqLegacy, _ := http.NewRequest(http.MethodGet, ts.URL+"/login", nil)
	reqLegacy.Host = pvHost // legacy shape, no port prefix in path
	reqLegacy.Header.Set("Cookie", "__Host-pv="+cookieVal)
	respLegacy, err := noRedirect.Do(reqLegacy)
	if err != nil {
		t.Fatalf("legacy contrast request: %v", err)
	}
	defer respLegacy.Body.Close()
	lbody, _ := io.ReadAll(respLegacy.Body)
	if respLegacy.StatusCode != http.StatusBadGateway || string(lbody) != epic67UnreachableBody {
		t.Fatalf("legacy contrast: expected indistinguishable 502 %q, got %d %q",
			epic67UnreachableBody, respLegacy.StatusCode, lbody)
	}
}

// --- unhappy paths (T3 focus) ---

// TestEpic67UnhappyPathScenarios covers port-host failure modes. The T3
// subtest compares the BLOCKED-port response byte-for-byte with a genuinely
// DEAD port's response (closed listener → proxy ErrorHandler) — they must
// be identical.
func TestEpic67UnhappyPathScenarios(t *testing.T) {
	t.Parallel()

	// t.Cleanup: parallel subtests outlive the parent function.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "APP")
	}))
	t.Cleanup(backend.Close)
	pv, r, _ := newPreviewOriginFixture(t, backend)

	cookieVal := func() string {
		t.Helper()
		loc := mintPortHostBootstrap(t, r, "5173")
		u, _ := url.Parse(loc)
		req := httptest.NewRequest(http.MethodGet, "/?t="+url.QueryEscape(u.Query().Get("t")), nil)
		req.Host = u.Host
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return cookieValueOnly(w.Header().Get("Set-Cookie"))
	}()

	// getViaHost issues an authenticated GET / against host through a REAL
	// server (proxy path requires CloseNotifier support). Takes the
	// SUBTEST's t: parallel subtests must not touch the parent's.
	getViaHost := func(t *testing.T, host, cookie string) (int, string) {
		t.Helper()
		ts := httptest.NewServer(r)
		defer ts.Close()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req.Host = host
		if cookie != "" {
			req.Header.Set("Cookie", "__Host-pv="+cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request to %s: %v", host, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("blocked_port_equals_dead_port_T3", func(t *testing.T) {
		t.Parallel()

		// Blocked port (4097 = agentd user mux): refused pre-proxy.
		blockedCode, blockedBody := getViaHost(t, "4097-"+pvWS+"-preview."+pvDomain, cookieVal)
		if blockedCode != http.StatusBadGateway {
			t.Fatalf("blocked port: expected 502, got %d", blockedCode)
		}

		// Genuinely dead port: point a SECOND fixture's agentd hop at a
		// closed listener and request a valid, unblocked port.
		deadTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadAddr := deadTS.Listener.Addr().String()
		deadTS.Close()
		deadPort := deadAddr[strings.LastIndex(deadAddr, ":")+1:]

		_, r2, inner2 := newPreviewOriginFixture(t, backend)
		inner2.agentdPort = deadPort

		ts2 := httptest.NewServer(r2)
		defer ts2.Close()
		req, _ := http.NewRequest(http.MethodGet, ts2.URL+"/", nil)
		req.Host = "5173-" + pvWS + "-preview." + pvDomain
		req.Header.Set("Cookie", "__Host-pv="+cookieVal)
		deadResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("dead port request: %v", err)
		}
		defer deadResp.Body.Close()
		db, _ := io.ReadAll(deadResp.Body)

		// T3: byte-exact identity of status AND body.
		if deadResp.StatusCode != blockedCode || string(db) != blockedBody {
			t.Fatalf("T3 VIOLATION: blocked=(%d,%q) dead=(%d,%q) — port scanner oracle",
				blockedCode, blockedBody, deadResp.StatusCode, db)
		}
		if blockedBody != epic67UnreachableBody {
			t.Fatalf("T3 body drifted from canonical unreachable body: %q", blockedBody)
		}
	})

	t.Run("privileged_port_502", func(t *testing.T) {
		t.Parallel()
		code, body := getViaHost(t, "80-"+pvWS+"-preview."+pvDomain, cookieVal)
		if code != http.StatusBadGateway || body != epic67UnreachableBody {
			t.Errorf("privileged port: expected 502 %q, got %d %q", epic67UnreachableBody, code, body)
		}
	})

	t.Run("expired_cookie_401", func(t *testing.T) {
		t.Parallel()
		expired := pv.signCookie(&previewCookiePayload{
			Ws:  pvWS,
			Exp: time.Now().Add(-time.Hour).Unix(),
		})
		code, _ := getViaHost(t, "5173-"+pvWS+"-preview."+pvDomain, expired)
		if code != http.StatusUnauthorized {
			t.Errorf("expired cookie: expected 401, got %d", code)
		}
	})

	t.Run("malformed_host_421", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "garbage-preview." + pvDomain
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("malformed preview host: expected 421, got %d", w.Code)
		}
	})
}

// --- property tests: regex disjointness ---

// epic67GenUUID generates a canonical lowercase UUID. When digitStress is
// true the first segment is ALL digits — the F1 population (~62% of real
// UUIDs lead with a digit; all-digit first segments are the adversarial
// worst case for host disambiguation).
func epic67GenUUID(rng *rand.Rand, digitStress bool) string {
	hexd := func(n int) string {
		const hex = "0123456789abcdef"
		b := make([]byte, n)
		for i := range b {
			b[i] = hex[rng.Intn(16)]
		}
		return string(b)
	}
	digd := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.Intn(10))
		}
		return string(b)
	}
	first := hexd(8)
	if digitStress {
		first = digd(8)
	}
	return first + "-" + hexd(4) + "-" + hexd(4) + "-" + hexd(4) + "-" + hexd(12)
}

// TestEpic67PropertyDisjointness: 10,000 deterministic iterations over
// VALID canonical UUIDs (half F1-stressed) proving no host matches both
// the port-host and legacy patterns, and each generated host matches
// exactly the pattern it was constructed for.
func TestEpic67PropertyDisjointness(t *testing.T) {
	t.Parallel()

	const iterations = 10000
	rng := rand.New(rand.NewSource(67)) // deterministic

	for i := 0; i < iterations; i++ {
		uuid := epic67GenUUID(rng, i%2 == 0)
		port := 1 + rng.Intn(65535)

		legacyHost := uuid + "-preview." + epic67TestDomain
		portHost := fmt.Sprintf("%d-%s-preview.%s", port, uuid, epic67TestDomain)

		if epic67NewRE.MatchString(legacyHost) {
			t.Fatalf("iter %d: DISJOINTNESS VIOLATION — legacy host %q matches port-host RE", i, legacyHost)
		}
		if epic67LegacyRE.MatchString(portHost) {
			t.Fatalf("iter %d: DISJOINTNESS VIOLATION — port host %q matches legacy RE", i, portHost)
		}
		if !epic67LegacyRE.MatchString(legacyHost) {
			t.Fatalf("iter %d: generated legacy host %q fails legacy RE (bad generator)", i, legacyHost)
		}
		if !epic67NewRE.MatchString(portHost) {
			t.Fatalf("iter %d: generated port host %q fails port-host RE (bad generator)", i, portHost)
		}
	}
}

// TestEpic67QuantifierBoundInvariant proves the disjointness mechanism
// exhaustively at the boundary: the port quantifier max (5) is strictly
// below the UUID first-segment length (8), so the FIRST dash of any
// port-host label sits at position ≤5 while any legacy label's sits at 8
// — one string cannot satisfy both. Also proves the {1,5} quantifier is
// actually enforced (6+ leading digits match neither pattern).
func TestEpic67QuantifierBoundInvariant(t *testing.T) {
	t.Parallel()

	const portMax = 5
	const uuidFirstSeg = 8
	if portMax >= uuidFirstSeg {
		t.Fatal("invariant broken: port quantifier max must be < 8")
	}

	// Exhaustive boundary sweep: every port length 1..5 crossed with every
	// digit-prefix length 1..8 of the UUID's first segment.
	digits := "12345678"
	filler := "abcdefabcdef" // ≥8 hex chars
	for portLen := 1; portLen <= portMax; portLen++ {
		portStr := digits[:portLen]
		for prefixLen := 1; prefixLen <= uuidFirstSeg; prefixLen++ {
			// UUID first segment starts with `prefixLen` digits, rest hex.
			first := digits[:prefixLen] + filler[:uuidFirstSeg-prefixLen]
			if len(first) != uuidFirstSeg {
				t.Fatalf("generator bug: first segment %q has len %d", first, len(first))
			}
			uuid := first + "-aaaa-bbbb-cccc-dddddddddddd"

			portLabel := portStr + "-" + uuid
			legacyLabel := uuid

			pd := strings.Index(portLabel, "-")
			ld := strings.Index(legacyLabel, "-")
			if pd == ld {
				t.Fatalf("portLen=%d prefixLen=%d: first-dash positions collide at %d — disjointness would fail",
					portLen, prefixLen, pd)
			}
			if pd > portMax {
				t.Fatalf("portLen=%d: port-host first dash at %d > quantifier max %d", portLen, pd, portMax)
			}
			if ld != uuidFirstSeg {
				t.Fatalf("legacy first dash at %d, want %d", ld, uuidFirstSeg)
			}

			portHost := portLabel + "-preview." + epic67TestDomain
			legacyHost := legacyLabel + "-preview." + epic67TestDomain
			if epic67LegacyRE.MatchString(portHost) || !epic67NewRE.MatchString(portHost) {
				t.Fatalf("portLen=%d prefixLen=%d: port host %q misclassified", portLen, prefixLen, portHost)
			}
			if epic67NewRE.MatchString(legacyHost) || !epic67LegacyRE.MatchString(legacyHost) {
				t.Fatalf("portLen=%d prefixLen=%d: legacy host %q misclassified", portLen, prefixLen, legacyHost)
			}
		}
	}

	// Quantifier enforcement: a 6+ digit leading segment matches NEITHER
	// pattern (not a preview host at all → 421 upstream).
	for _, sixPort := range []string{"123456", "1234567", "12345678"} {
		host := sixPort + "-1044f4f2-1234-5678-9abc-def000000000-preview." + epic67TestDomain
		if epic67NewRE.MatchString(host) || epic67LegacyRE.MatchString(host) {
			t.Errorf("%d-digit leading segment %q must match neither pattern", len(sixPort), host)
		}
	}
}

// TestEpic67F1DigitLeadingUUIDWorkaround: multiple digit-leading UUIDs,
// including ports whose digits collide with the UUID's own prefix.
func TestEpic67F1DigitLeadingUUIDWorkaround(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:     true,
		BaseDomain:  epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}
	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	f1Cases := []struct {
		uuid string
		port string
	}{
		{"1044f4f2-1234-5678-9abc-def000000000", "1044"},  // port == UUID prefix
		{"1044f4f2-1234-5678-9abc-def000000000", "104"},   // partial prefix
		{"1044f4f2-1234-5678-9abc-def000000000", "1044"},  // duplicate-prefix stress
		{"99999999-1234-5678-9abc-def000000000", "65535"}, // all-digit segment, max port
		{"00000001-1234-5678-9abc-def000000000", "1"},     // near-zero digit segment
	}

	for _, tc := range f1Cases {
		portHost := tc.port + "-" + tc.uuid + "-preview." + epic67TestDomain
		wsID, port, isPortHost, ok := h.PreviewHost(portHost)
		if !ok || wsID != tc.uuid || !isPortHost {
			t.Errorf("F1 host %q: got (ws=%q port=%d isPort=%v ok=%v), want ws=%q isPort=true",
				portHost, wsID, port, isPortHost, ok, tc.uuid)
		}

		legacyHost := tc.uuid + "-preview." + epic67TestDomain
		wsID2, _, isPort2, ok2 := h.PreviewHost(legacyHost)
		if !ok2 || wsID2 != tc.uuid || isPort2 {
			t.Errorf("F1 legacy host %q: got (ws=%q isPort=%v ok=%v), want ws=%q isPort=false",
				legacyHost, wsID2, isPort2, ok2, tc.uuid)
		}
	}
}
