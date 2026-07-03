// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestCheckAgentHealth_MirrorsUserCredsPresentFromHealthz is the
// load-bearing test for the "controller as scrape-and-mirror
// intermediary" role (worklog 0591). The controller polls agentd's
// /v1/healthz on the existing 15s cadence; when the response includes
// `userCredsPresent`, the controller MUST mirror it into
// ws.Status.UserCredsPresent so the API's workspace watcher can
// consume it via the CRD.
//
// Both boolean values are tested to ensure the pointer round-trip
// preserves value (a &false gotcha would silently mirror as &true
// on some copy paths).
func TestCheckAgentHealth_MirrorsUserCredsPresentFromHealthz(t *testing.T) {
	cases := []struct {
		name           string
		agentdResponse agentd.HealthzResponse
		wantUserCreds  *bool
	}{
		{
			name: "agentd reports user creds present",
			agentdResponse: agentd.HealthzResponse{
				Healthy: true, Version: "test", UptimeSeconds: 10,
				UserCredsPresent: true,
			},
			wantUserCreds: boolPtr(true),
		},
		{
			name: "agentd reports no user creds",
			agentdResponse: agentd.HealthzResponse{
				Healthy: true, Version: "test", UptimeSeconds: 10,
				UserCredsPresent: false,
			},
			wantUserCreds: boolPtr(false),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, restore := healthzServerOnAdminPort(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/healthz", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.agentdResponse)
			})
			defer server.Close()
			defer restore()

			ws := activeReachableWorkspace(t, server)
			r := reconcilerFor(t)
			r.checkAgentHealth(context.Background(), ws)

			require.NotNil(t, ws.Status.UserCredsPresent,
				"controller MUST mirror the healthz.userCredsPresent field "+
					"onto CRD status (nil would signal 'not scraped yet' to "+
					"the API's watcher, suppressing the auto-push)")
			assert.Equal(t, *tc.wantUserCreds, *ws.Status.UserCredsPresent,
				"pointer round-trip must preserve the actual boolean value")
		})
	}
}

// TestCheckAgentHealth_ClearsUserCredsPresentOnUnreachable proves the
// staleness-recovery contract: if the pod becomes unreachable (network
// hiccup, pod recreation mid-scrape), the controller MUST clear
// UserCredsPresent to nil. A stale "true" from a previous pod would
// suppress the API's auto-push after the new pod comes up with no
// user-DEK content.
func TestCheckAgentHealth_ClearsUserCredsPresentOnUnreachable(t *testing.T) {
	// Server that hijacks + closes the connection — simulates unreachable agentd.
	server, restore := healthzServerOnAdminPort(t, func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	})
	defer server.Close()
	defer restore()

	ws := activeReachableWorkspace(t, server)
	// Prime status with a stale "true" from a previous scrape.
	stale := true
	ws.Status.UserCredsPresent = &stale

	r := reconcilerFor(t)
	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.UserCredsPresent,
		"unreachable pod MUST clear UserCredsPresent to nil — the field "+
			"reflects the CURRENT pod's state, not the last-known-good "+
			"value from a previous pod. A stale &true would suppress the "+
			"API's auto-push after recreation.")
}

// TestCheckAgentHealth_LeavesUserCredsPresentUnsetWhenNotYetScraped
// covers the pre-scrape state: the pod is Active but the controller
// hasn't yet completed a health scrape (either shouldRunHealthCheck
// deferred it, or the pod has no IP yet). UserCredsPresent MUST remain
// nil so the API's watcher treats it as "unknown, do not push."
func TestCheckAgentHealth_LeavesUserCredsPresentUnsetWhenNotYetScraped(t *testing.T) {
	ws := makeWorkspace("ws-nopodip", "default", v1.WorkspacePhaseActive)
	// Empty PodIP → checkAgentHealth returns early at the guard.

	r := reconcilerFor(t)
	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.UserCredsPresent,
		"pre-scrape state MUST keep UserCredsPresent=nil so the API "+
			"treats it as 'unknown, do not push'")
}

// --- helpers ---

func boolPtr(b bool) *bool { return &b }

// healthzServerOnAdminPort starts an httptest.Server bound to
// 127.0.0.1:agentdAdminPort so `checkAgentHealth` (which hardcodes the
// port into the URL) reaches it. Returns the server and a restore fn
// callers should defer.
//
// The agentdAdminPort const is not overridden — instead we bind the
// httptest server to it directly. That means only one such test can
// run at a time within a package, but Go serializes t.Run subtests
// by default so this works.
func healthzServerOnAdminPort(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func()) {
	t.Helper()
	// Use the real admin port — checkAgentHealth uses agentd.AgentdAdminPort
	// via the agentdAdminPort var, which we don't override here to avoid
	// touching production constants from tests.
	addr := "127.0.0.1:" + strconv.Itoa(agentdAdminPort)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot bind to admin port %d (already in use?): %v", agentdAdminPort, err)
	}
	server := &httptest.Server{
		Listener: l,
		Config:   &http.Server{Handler: handler},
	}
	server.Start()
	return server, func() {}
}

// activeReachableWorkspace returns an Active workspace whose PodIP is
// the loopback the healthzServerOnAdminPort test server is listening on.
func activeReachableWorkspace(t *testing.T, server *httptest.Server) *v1.Workspace {
	t.Helper()
	ws := makeWorkspace("ws-h", "default", v1.WorkspacePhaseActive)
	ws.Status.PodIP = extractHostname(server.URL)
	ws.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
	return ws
}

func extractHostname(u string) string {
	s := strings.TrimPrefix(u, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i]
	}
	return s
}

// TestCheckAgentHealth_ClearsUserCredsPresentOnUndecodable proves the
// clearing contract for the "healthz responded but response body is
// unparseable" path. Same reasoning as unreachable: we can't trust
// any prior value; clear to nil so the API's watcher doesn't observe
// stale state.
func TestCheckAgentHealth_ClearsUserCredsPresentOnUndecodable(t *testing.T) {
	server, restore := healthzServerOnAdminPort(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("this is not JSON"))
	})
	defer server.Close()
	defer restore()

	ws := activeReachableWorkspace(t, server)
	stale := true
	ws.Status.UserCredsPresent = &stale

	r := reconcilerFor(t)
	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.UserCredsPresent,
		"undecodable healthz response MUST clear UserCredsPresent — "+
			"same reasoning as unreachable: no trustworthy signal available")
}

// TestCheckAgentHealth_ClearsUserCredsPresentOnUnhealthy proves the
// clearing contract for the "healthz decoded but Healthy=false" path.
// An unhealthy agentd cannot reliably report its cache state; a stale
// UCP=true from a previous scrape must not survive.
func TestCheckAgentHealth_ClearsUserCredsPresentOnUnhealthy(t *testing.T) {
	server, restore := healthzServerOnAdminPort(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{
			Healthy:          false, // agent process not responding
			Version:          "test",
			UptimeSeconds:    10,
			UserCredsPresent: true, // even if agentd reports true, we discard
		})
	})
	defer server.Close()
	defer restore()

	ws := activeReachableWorkspace(t, server)
	stale := true
	ws.Status.UserCredsPresent = &stale

	r := reconcilerFor(t)
	r.checkAgentHealth(context.Background(), ws)

	assert.Nil(t, ws.Status.UserCredsPresent,
		"Healthy=false MUST clear UserCredsPresent — an unhealthy agent's "+
			"cache-state signal cannot be trusted; discarding it prevents "+
			"suppressing the API's push after the pod recovers")
}
