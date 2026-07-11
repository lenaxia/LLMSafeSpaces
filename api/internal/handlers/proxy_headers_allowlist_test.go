// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// G34: the proxy must forward only an explicit allowlist of client headers
// into the tenant pod, and must strip hop-by-hop headers in both directions.
// The previous behaviour copied every client header verbatim, which forwarded
// Authorization, Cookie, Origin, Referer, and every X-Forwarded-* header into
// the sandbox before overwriting Authorization with the opencode basic-auth
// password — leaking the caller's session material to untrusted agent code.

func TestCopyRequestHeaders_AllowlistOnly(t *testing.T) {
	tests := []struct {
		name       string
		setHeader  func(http.Header)
		wantHeader string
		wantValue  string
		wantKept   bool
	}{
		{
			name: "content-type forwarded",
			setHeader: func(h http.Header) {
				h.Set("Content-Type", "application/json")
			},
			wantHeader: "Content-Type",
			wantValue:  "application/json",
			wantKept:   true,
		},
		{
			name: "accept forwarded",
			setHeader: func(h http.Header) {
				h.Set("Accept", "application/json")
			},
			wantHeader: "Accept",
			wantValue:  "application/json",
			wantKept:   true,
		},
		{
			name: "accept-encoding dropped (transport handles gzip transparently)",
			setHeader: func(h http.Header) {
				h.Set("Accept-Encoding", "gzip")
			},
			wantHeader: "Accept-Encoding",
			wantKept:   false,
		},
		{
			name: "authorization dropped",
			setHeader: func(h http.Header) {
				h.Set("Authorization", "Bearer callers-jwt")
			},
			wantHeader: "Authorization",
			wantKept:   false,
		},
		{
			name: "cookie dropped",
			setHeader: func(h http.Header) {
				h.Set("Cookie", "lsp_session=caller-session")
			},
			wantHeader: "Cookie",
			wantKept:   false,
		},
		{
			name: "origin dropped",
			setHeader: func(h http.Header) {
				h.Set("Origin", "https://app.example.com")
			},
			wantHeader: "Origin",
			wantKept:   false,
		},
		{
			name: "referer dropped",
			setHeader: func(h http.Header) {
				h.Set("Referer", "https://app.example.com/workspaces")
			},
			wantHeader: "Referer",
			wantKept:   false,
		},
		{
			name: "x-forwarded-host dropped",
			setHeader: func(h http.Header) {
				h.Set("X-Forwarded-Host", "app.example.com")
			},
			wantHeader: "X-Forwarded-Host",
			wantKept:   false,
		},
		{
			name: "x-forwarded-proto dropped",
			setHeader: func(h http.Header) {
				h.Set("X-Forwarded-Proto", "https")
			},
			wantHeader: "X-Forwarded-Proto",
			wantKept:   false,
		},
		{
			name: "forwarded dropped",
			setHeader: func(h http.Header) {
				h.Set("Forwarded", "for=10.0.0.1")
			},
			wantHeader: "Forwarded",
			wantKept:   false,
		},
		{
			name: "hop-by-hop connection dropped",
			setHeader: func(h http.Header) {
				h.Set("Connection", "keep-alive")
			},
			wantHeader: "Connection",
			wantKept:   false,
		},
		{
			name: "arbitrary x-acme-custom dropped",
			setHeader: func(h http.Header) {
				h.Set("X-Acme-Custom", "anything")
			},
			wantHeader: "X-Acme-Custom",
			wantKept:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := http.Header{}
			tt.setHeader(src)

			dst := http.Header{}
			copyRequestHeaders(src, dst)

			if tt.wantKept {
				assert.Equal(t, tt.wantValue, dst.Get(tt.wantHeader),
					"%s must be forwarded", tt.wantHeader)
			} else {
				assert.Empty(t, dst.Get(tt.wantHeader),
					"%s must be dropped", tt.wantHeader)
			}
		})
	}
}

func TestCopyRequestHeaders_XForwardedForNotCopiedFromCaller(t *testing.T) {
	src := http.Header{}
	src.Set("X-Forwarded-For", "10.0.0.99, 10.0.0.1")

	dst := http.Header{}
	copyRequestHeaders(src, dst)

	assert.Empty(t, dst.Get("X-Forwarded-For"),
		"caller-controlled X-Forwarded-For must not reach the tenant pod; the proxy sets its own after this copy")
}

func TestCopyResponseHeaders_HopByHopStripped(t *testing.T) {
	hopByHop := []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailers",
		"Transfer-Encoding",
		"Upgrade",
	}

	for _, h := range hopByHop {
		t.Run(h, func(t *testing.T) {
			src := http.Header{}
			src.Set(h, "anything")
			src.Set("Content-Type", "text/event-stream")

			dst := http.Header{}
			copyResponseHeaders(src, dst)

			assert.Empty(t, dst.Get(h), "%s must be stripped from upstream responses", h)
			assert.Equal(t, "text/event-stream", dst.Get("Content-Type"))
		})
	}
}

func TestCopyResponseHeaders_StripsSetCookieMultipleValues(t *testing.T) {
	src := http.Header{}
	src.Add("Set-Cookie", "session=abc123")
	src.Add("Set-Cookie", "csrf=xyz")

	dst := http.Header{}
	copyResponseHeaders(src, dst)

	assert.Empty(t, dst.Values("Set-Cookie"), "all Set-Cookie values must be stripped")
}

// End-to-end wiring check: the proxy really applies copyRequestHeaders when
// building the upstream request, and the caller's Authorization never reaches
// the tenant pod — only the opencode basic-auth credential the proxy itself
// injects. This is the regression that G34 documents.
func TestProxy_G34_CallerAuthorizationNotForwarded(t *testing.T) {
	var capturedAuthorization string
	var capturedCookie string
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuthorization = r.Header.Get("Authorization")
		capturedCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{}`))
	})
	env.setupWorkspacePodWithT(t, "ws-leak", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-leak")
	env.setupPasswordWithT(t, "ws-leak", "test-password")
	env.setupWorkspaceWithT(t, "ws-leak", 5)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/workspaces/ws-leak/sessions/ses-1/message",
		nil)
	req.Header.Set("Authorization", "Bearer callers-jwt-abc")
	req.Header.Set("Cookie", "lsp_session=caller-session")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	require.NotEqual(t, http.StatusInternalServerError, w.Code,
		"proxy should reach the upstream, not fail before it")

	// The only Authorization header on the upstream request must be the
	// opencode basic-auth credential the proxy itself injected — not the
	// caller's Bearer JWT.
	assert.NotContains(t, capturedAuthorization, "callers-jwt-abc",
		"caller's Bearer JWT must not reach the tenant pod (G34)")
	assert.Contains(t, capturedAuthorization, "Basic",
		"proxy must inject HTTP Basic auth for opencode")
	assert.Empty(t, capturedCookie,
		"caller's Cookie header must not be forwarded to the tenant pod (G34)")
}
