// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"net/http"
	"net/url"
	"strings"
)

// blockedResponseHeaders is the historical denylist used by
// copyResponseHeaders. Retained for defense-in-depth alongside the
// hop-by-hop strip — if a future maintainer narrows the hop-by-hop list,
// these headers remain suppressed regardless.
var blockedResponseHeaders = map[string]bool{
	"Www-Authenticate":   true,
	"Proxy-Authenticate": true,
	"Set-Cookie":         true,
}

// hopByHopHeaders is the RFC 7230 §6.1 hop-by-hop header set plus
// "Upgrade" (RFC 7230 §6.7). These must not be forwarded by a proxy in
// either direction: they describe the transport between two immediate
// HTTP peers and have no meaning end-to-end.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// forwardedRequestHeaders is the explicit allowlist of client headers the
// proxy forwards into the tenant pod. Everything else is dropped.
//
// Allowlist, not denylist: the caller's Authorization, Cookie, Origin and
// Referer describe the *caller's* relationship with the API server, none of
// which the untrusted agent code inside a tenant pod has any reason to see.
// The proxy sets its own Authorization (HTTP Basic for opencode) and
// X-Forwarded-For after this copy.
//
// Accept-Encoding is deliberately NOT on the allowlist: Go's http.Transport
// auto-negotiates gzip transparently when the request header is unset, but
// only auto-decompresses responses when *it* set the header. Forwarding the
// caller's Accept-Encoding would defeat that — opencode would gzip the
// response, the transport would not auto-decompress, and the bytes would be
// forwarded compressed to a client that may not have asked for gzip.
var forwardedRequestHeaders = map[string]bool{
	"Content-Type": true,
	"Accept":       true,
	"X-Request-ID": true,
}

func copyResponseHeaders(src http.Header, dst http.Header) {
	for k, vs := range src {
		canon := http.CanonicalHeaderKey(k)
		if blockedResponseHeaders[canon] {
			continue
		}
		if hopByHopHeaders[canon] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// copyRequestHeaders copies only the allowlisted client headers from src to dst.
// Hop-by-hop headers and caller credential/session headers are explicitly
// excluded. The proxy sets Authorization (HTTP Basic) and X-Forwarded-For
// separately, after this copy, so neither is on the allowlist.
func copyRequestHeaders(src http.Header, dst http.Header) {
	for k, vs := range src {
		if !forwardedRequestHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func stripVerboseQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	values.Del("verbose")
	values.Del("workspace")
	values.Del("directory")
	return values.Encode()
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "network is unreachable")
}
