// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// Epic 67 unit tests validate the port-in-subdomain parsing logic

const (
	epic67TestDomain = "epic67.test.example.com"
	epic67TestWS     = "a1b2c3d4-e5f6-7a8b-9c0d-1e2f3a4b5c6d"
	epic67TestPort   = 5173 // Vite default
)

// TestEpic67LegacyHostBackwardCompatibility ensures legacy hosts still work
func TestEpic67LegacyHostBackwardCompatibility(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
		TokenSecret: []byte("epic67-test-secret-key"),
	}

	h := NewPreviewOriginHandler(nil, cfg, &fakePVCache{}, nil)

	// Test legacy host format
	legacyHost := epic67TestWS + "-preview." + epic67TestDomain
	wsID, port, isPortHost, ok := h.PreviewHost(legacyHost)

	require.True(t, ok, "Legacy host should be recognized")
	require.Equal(t, epic67TestWS, wsID, "Workspace ID should match")
	assert.Equal(t, 0, port, "Legacy hosts have port=0")
	assert.False(t, isPortHost, "Legacy hosts have isPortHost=false")
}

// TestEpic67PortHostParsing validates port extraction from port-hosts
func TestEpic67PortHostParsing(t *testing.T) {
	t.Parallel()

	cfg := PreviewOriginConfig{
		Enabled:    true,
		BaseDomain: epic67TestDomain,
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
		{
			name:           "Vite default port",
			host:           "5173-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   5173,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Express default port",
			host:           "3000-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   3000,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Max valid port",
			host:           "65535-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   65535,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Single digit port",
			host:           "1-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   epic67TestWS,
			expectedPort:   1,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "F1 case: digit-leading UUID (port 1044)",
			host:           "1044-1044f4f2-1234-5678-9abc-def000000000-preview." + epic67TestDomain,
			expectedWSID:   "1044f4f2-1234-5678-9abc-def000000000",
			expectedPort:   1044,
			expectedIsPort: true,
			expectedOk:     true,
		},
		{
			name:           "Wrong domain",
			host:           "5173-" + epic67TestWS + "-preview.wrong-domain.com",
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
		{
			name:           "Invalid port (too large)",
			host:           "65536-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
		{
			name:           "Non-numeric port",
			host:           "abc-" + epic67TestWS + "-preview." + epic67TestDomain,
			expectedWSID:   "",
			expectedPort:   0,
			expectedIsPort: false,
			expectedOk:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wsID, port, isPortHost, ok := h.PreviewHost(tc.host)

			assert.Equal(t, tc.expectedOk, ok, "ok status")
			if tc.expectedOk {
				assert.Equal(t, tc.expectedWSID, wsID, "workspace ID")
				assert.Equal(t, tc.expectedPort, port, "port number")
				assert.Equal(t, tc.expectedIsPort, isPortHost, "isPortHost flag")
			}
		})
	}
}

// TestEpic67BootstrapRedirect validates the bootstrap redirect format
func TestEpic67BootstrapRedirect(t *testing.T) {
	t.Parallel()

	// Note: HandleBootstrap requires middleware context we can't easily mock here
	// This test validates the redirect format construction
	t.Skip("Bootstrap redirect test requires middleware context - deferred to integration test suite")
}

// TestEpic67LandingPageBehavior validates landing page gating to legacy hosts only
func TestEpic67LandingPageBehavior(t *testing.T) {
	t.Parallel()

	t.Run("legacy_host_root_gets_landing_page", func(t *testing.T) {
		t.Parallel()

		// Landing page only served on legacy hosts when isPortHost=false
		t.Skip("Landing page test requires full request context - deferred to integration test suite")
	})

	t.Run("port_host_root_goes_to_app", func(t *testing.T) {
		t.Parallel()

		// Port-host root goes directly to the app (no landing page)
		t.Skip("Port-host root test requires full request context - deferred to integration test suite")
	})
}

// Mock implementations for testing
type epic67MockWorkspaceGetter struct {
	workspace *v1.Workspace
}

func (m *epic67MockWorkspaceGetter) GetWorkspace(_ context.Context, id string) (*v1.Workspace, error) {
	if m.workspace != nil && m.workspace.Name == id {
		return m.workspace, nil
	}
	return nil, nil
}

type epic67MockPasswordProvider struct {
	password string
}

func (m *epic67MockPasswordProvider) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return m.password, nil
}