// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
)

type stubPodIPResolver struct {
	ip   string
	err  error
	gotW string
}

func (s *stubPodIPResolver) GetWorkspacePodIP(_ context.Context, _, workspaceID string) (string, error) {
	s.gotW = workspaceID
	return s.ip, s.err
}

func relayCheckerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	addr := srv.Listener.Addr().(*net.TCPAddr)
	return addr.Port
}

// TestNewRelayChecker_RelayInjected verifies the checker reads relay_injected
// from /v1/readyz with Bearer auth and returns it faithfully.
func TestNewRelayChecker_RelayInjected(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"injected", `{"relay_injected":true}`, true},
		{"not injected", `{"relay_injected":false}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			ipRes := &stubPodIPResolver{ip: "127.0.0.1"}
			pw := func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil }
			checker := newRelayChecker(srv.Client(), relayCheckerPort(t, srv), ipRes, pw)

			got := checker(context.Background(), "u1", "ws-1")
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if gotPath != "/v1/readyz" {
				t.Errorf("path = %q, want /v1/readyz", gotPath)
			}
			if gotAuth != "Bearer pw" {
				t.Errorf("auth = %q, want Bearer pw", gotAuth)
			}
			if ipRes.gotW != "ws-1" {
				t.Errorf("resolver saw workspaceID %q, want ws-1", ipRes.gotW)
			}
		})
	}
}

// TestNewRelayChecker_OversizedBody_DecodeFails proves the 16 KiB read limit
// (worklog 0372 H4) is enforced: a >16 KiB JSON value is truncated mid-value
// and Decode fails, so the checker returns false instead of reading unbounded
// data.
func TestNewRelayChecker_OversizedBody_DecodeFails(t *testing.T) {
	huge := `{"relay_injected":true,"junk":"` + strings.Repeat("x", 32*1024) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	checker := newRelayChecker(srv.Client(), relayCheckerPort(t, srv),
		&stubPodIPResolver{ip: "127.0.0.1"},
		func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil })

	if got := checker(context.Background(), "u1", "ws-1"); got {
		t.Fatalf("expected false for oversized body exceeding 16 KiB read limit, got true")
	}
}

// TestNewRelayChecker_NonOKStatus ensures a non-200 readyz yields false.
func TestNewRelayChecker_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	checker := newRelayChecker(srv.Client(), relayCheckerPort(t, srv),
		&stubPodIPResolver{ip: "127.0.0.1"},
		func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil })

	if got := checker(context.Background(), "u1", "ws-1"); got {
		t.Fatalf("expected false for 503, got true")
	}
}

// TestNewRelayChecker_ResolveFailures confirms resolver/password failures and
// empty values short-circuit to false without hitting the pod.
func TestNewRelayChecker_ResolveFailures(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer srv.Close()

	tests := []struct {
		name   string
		ipRes  handlers.PodIPResolver
		pwFunc func(context.Context, string) ([]string, error)
	}{
		{
			"empty pod ip",
			&stubPodIPResolver{ip: ""},
			func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil },
		},
		{
			"resolver error",
			&stubPodIPResolver{err: errors.New("no pod")},
			func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil },
		},
		{
			"empty password",
			&stubPodIPResolver{ip: "127.0.0.1"},
			func(_ context.Context, _ string) ([]string, error) { return nil, nil },
		},
		{
			"password error",
			&stubPodIPResolver{ip: "127.0.0.1"},
			func(_ context.Context, _ string) ([]string, error) { return nil, errors.New("no secret") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := newRelayChecker(&http.Client{Timeout: time.Second}, relayCheckerPort(t, srv), tt.ipRes, tt.pwFunc)
			if got := checker(context.Background(), "u1", "ws-1"); got {
				t.Fatalf("expected false for %s, got true", tt.name)
			}
		})
	}
	if calls != 0 {
		t.Errorf("pod endpoint hit %d times during short-circuit cases; expected 0", calls)
	}
}

// TestBuildRelayChecker_WiresResolverAndPassword confirms buildRelayChecker
// returns a non-nil checker that honors the resolver/password seam and
// short-circuits to false on resolve failure. The port is bound to the
// agentd admin constant (verified by code review against the original
// fetchRelayInjected contract).
func TestBuildRelayChecker_WiresResolverAndPassword(t *testing.T) {
	if buildRelayChecker(&stubPodIPResolver{ip: ""}, nil) == nil {
		t.Fatalf("buildRelayChecker returned nil checker")
	}
	checker := buildRelayChecker(&stubPodIPResolver{ip: ""},
		func(_ context.Context, _ string) ([]string, error) { return []string{"pw"}, nil })
	if got := checker(context.Background(), "u1", "ws-1"); got {
		t.Fatalf("expected false when pod IP resolves empty, got true")
	}
}
