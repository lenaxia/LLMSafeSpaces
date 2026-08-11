// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func backendPort(t *testing.T, backendURL string) string {
	t.Helper()
	u, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	return u.Port()
}

func TestDevPreview_Unauthorized(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/dev-preview/5173/", nil)
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestDevPreview_WrongPassword(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/dev-preview/5173/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("wrong-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestDevPreview_NonNumericPort(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/dev-preview/abc/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDevPreview_PortOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		port string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too high", "65536"},
		{"way too high", "99999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/dev-preview/"+tc.port+"/", nil)
			req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
			w := httptest.NewRecorder()
			h := devPreviewHandler("test-pass")
			h(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for port %s, got %d", tc.port, w.Code)
			}
		})
	}
}

func TestDevPreview_DeniedPorts(t *testing.T) {
	denied := []struct {
		name string
		port string
	}{
		{"opencode 4096", "4096"},
		{"agentd 4097", "4097"},
		{"agentd-admin 4098", "4098"},
		{"privileged 80", "80"},
		{"privileged 22", "22"},
		{"privileged 443", "443"},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/dev-preview/"+tc.port+"/", nil)
			req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
			w := httptest.NewRecorder()
			h := devPreviewHandler("test-pass")
			h(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for denied port %s, got %d", tc.port, w.Code)
			}
		})
	}
}

func TestDevPreview_RecursionAttempt(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/dev-preview/4097/v1/dev-preview/5173/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for recursion attempt via 4097, got %d", w.Code)
	}
}

func TestDevPreview_HTTPRoundTrip(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello from "+r.URL.Path)
	}))
	defer backend.Close()

	port := backendPort(t, backend.URL)

	req := httptest.NewRequest("GET", "/v1/dev-preview/"+port+"/index.html", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello from /index.html" {
		t.Errorf("unexpected body: %q", w.Body.String())
	}
}

func TestDevPreview_HostRewritten(t *testing.T) {
	var capturedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	port := backendPort(t, backend.URL)

	req := httptest.NewRequest("GET", "/v1/dev-preview/"+port+"/", nil)
	req.Host = "api.platform.example.com"
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if capturedHost == "api.platform.example.com" {
		t.Errorf("Host was NOT rewritten — backend saw the browser origin %q", capturedHost)
	}
	if !strings.HasPrefix(capturedHost, "localhost:") && !strings.HasPrefix(capturedHost, "127.0.0.1:") {
		t.Errorf("expected Host to be rewritten to localhost:<port>, got %q", capturedHost)
	}
}

func TestDevPreview_DevServerNotListening(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/dev-preview/59999/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	w := httptest.NewRecorder()
	h := devPreviewHandler("test-pass")
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for unreachable port, got %d", w.Code)
	}
}

func TestDevPreview_WebSocketUpgrade(t *testing.T) {
	// httptest.NewRecorder doesn't implement http.Hijacker, so a real WS
	// upgrade can't complete through it. The spike (PREVIEW-CONTRACT.md A1)
	// already proved httputil.ReverseProxy forwards WS upgrades end-to-end.
	// This test verifies the handler doesn't block the Upgrade — the proxy
	// must pass it through to the backend, which returns 101. We use a
	// real TCP connection via httptest.Server to get a Hijackable writer.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "not hijackable", http.StatusInternalServerError)
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
			return
		}
		_ = r.Header.Get("Connection")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.WriteHeader(http.StatusSwitchingProtocols)
		conn, _, _ := hj.Hijack()
		if conn != nil {
			conn.Close()
		}
	}))
	defer backend.Close()

	port := backendPort(t, backend.URL)

	handler := devPreviewHandler("test-pass")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := &http.Client{}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/dev-preview/"+port+"/", nil)
	req.Header.Set("Authorization", "Basic "+basicAuth("test-pass"))
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected 101 Switching Protocols, got %d", resp.StatusCode)
	}
}
