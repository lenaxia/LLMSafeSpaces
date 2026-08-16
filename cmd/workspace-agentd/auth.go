// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Shared Basic-auth helpers for agentd HTTP handlers. Used by the gated
// user-mux (port 4097) handlers — workflow execute/cancel/delete-session
// and dev-preview (#762), the MCP proxy (#847), and reload-secrets +
// agent/reload (#848). The mux is reachable from inside the workspace
// pod and — when the chart's NetworkPolicy is misconfigured, the CNI is
// buggy, or an operator opted out — from other pods. The auth check is
// the application-layer defense-in-depth on top of the network control
// (the NetPol remains the primary control).

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// basicAuth encodes the agentd Basic credential for the given password.
func basicAuth(password string) string {
	return base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername + ":" + password))
}

// checkBasicAuth reports whether the request carries a valid agentd
// Basic credential. The comparison is constant-time — same rationale as
// the relay-proxy X-Relay-Token check (timing side channels on secret
// comparison).
func checkBasicAuth(r *http.Request, password string) bool {
	expected := "Basic " + basicAuth(password)
	got := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// rejectUnauthorized writes the unified 401 + Basic challenge for
// failed checkBasicAuth.
func rejectUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="agentd"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
