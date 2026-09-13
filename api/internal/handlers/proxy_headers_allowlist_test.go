// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// G34: the proxy must forward only an explicit allowlist of client headers
// into the tenant pod, and must strip hop-by-hop headers in both directions.
// The previous behavior copied every client header verbatim, which forwarded
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

// TestProxy_G34_CallerAuthorizationNotForwarded was deleted with the raw-proxy transport (#828 final batch — proxyToWorkspaceWithErrBody/doProxy and the legacy seams are gone; the contract either died with the transport or is pinned at the adapter seam).
