// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// control_plane_auth_test.go — design 0051 US-3 / §D1 per-endpoint
// credential table, pinned at the handler seam:
//
//	Control plane (resync-secrets, agent/reload, workflow/*):
//	    agentdPassword OR workspace password (mixed-generation window, D6.1)
//	/v1/mcp:      workspace password ONLY
//	/v1/dev-preview/: workspace password ONLY
//
// The OR on control-plane routes is the D6.1 mixed-generation-window
// requirement: the API server dispatches ONE credential to pods of any
// generation, so the sidecar accepts both while the fleet converges.
// The strict end-state (workspace password → 401 on control plane, V4)
// is US-5's canary-graduation gate, not US-3's.
//
// /v1/mcp and dev-preview accept ONLY the workspace password: their
// callers (opencode; the API server's preview proxy) live in uid-1000
// space by design — the agentdPassword must not unlock them.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

const (
	testWorkspacePW = "workspace-pw-us3"
	testAgentdPW    = "agentd-pw-us3"
)

// authedStatus runs one authenticated POST through the handler and
// returns the status code. 401 = rejected at the auth gate; any other
// status = the credential was ACCEPTED (the handler's downstream
// behavior is not under test).
func authedStatus(t *testing.T, h http.HandlerFunc, password string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/x", bytes.NewReader(body))
	req.SetBasicAuth(agentd.AuthUsername, password)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code
}

// TestControlPlaneAuth_ResyncSecretsAcceptsBoth: resync-secrets (the
// surviving secrets control-plane route, US-70.5) accepts
// agentdPassword (the §D1 secret) AND the workspace password
// (mixed-generation window), and rejects unknown credentials. The
// handler runs against an unreachable API URL; any non-401 status
// proves the auth gate passed.
func TestControlPlaneAuth_ResyncSecretsAcceptsBoth(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	h := resyncSecretsHandler(resyncDeps{
		cfg:    materializeConfig{home: dir, secretsEnvPath: filepath.Join(dir, "secrets-env")},
		apply:  applySecretsDeps{OpencodePassword: testWorkspacePW, ControlPlanePassword: testAgentdPW},
		apiURL: "http://127.0.0.1:1", workspaceID: "ws-auth",
		tokenPath: filepath.Join(dir, "token"), batchPath: filepath.Join(dir, "secrets.json"),
	})

	require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testAgentdPW, nil),
		"agentdPassword must pass the resync-secrets auth gate")
	require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testWorkspacePW, nil),
		"workspace password stays valid on control-plane routes during the mixed-generation window (D6.1)")
	require.Equal(t, http.StatusUnauthorized, authedStatus(t, h, "wrong-pw", nil),
		"unknown credentials rejected")
}

// TestControlPlaneAuth_SingleContainerModeUnchanged: an empty
// ControlPlanePassword (single-container wiring) reduces to today's
// behavior — workspace password only.
func TestControlPlaneAuth_SingleContainerModeUnchanged(t *testing.T) {
	withTestLogger(t)
	dir := t.TempDir()
	h := resyncSecretsHandler(resyncDeps{
		cfg:    materializeConfig{home: dir, secretsEnvPath: filepath.Join(dir, "secrets-env")},
		apply:  applySecretsDeps{OpencodePassword: testWorkspacePW},
		apiURL: "http://127.0.0.1:1", workspaceID: "ws-auth",
		tokenPath: filepath.Join(dir, "token"), batchPath: filepath.Join(dir, "secrets.json"),
	})
	require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testWorkspacePW, nil))
	require.Equal(t, http.StatusUnauthorized, authedStatus(t, h, testAgentdPW, nil))
}

// TestControlPlaneAuth_WorkflowAndAgentReloadAcceptBoth: agent/reload and
// the workflow routes share the two-credential acceptance via the
// variadic form (sidecar wiring passes BOTH; single-container passes one).
func TestControlPlaneAuth_WorkflowAndAgentReloadAcceptBoth(t *testing.T) {
	withTestLogger(t)
	handlers := map[string]http.HandlerFunc{
		"agent-reload":         agentReloadHandler(log, testWorkspacePW, testAgentdPW),
		"workflow-exec":        workflowExecuteHandler(testWorkspacePW, testAgentdPW),
		"workflow-cancel":      workflowCancelHandler(testWorkspacePW, testAgentdPW),
		"workflow-del-session": workflowDeleteSessionHandler(testWorkspacePW, testAgentdPW),
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testAgentdPW, []byte("{}")),
				"agentdPassword accepted")
			require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testWorkspacePW, []byte("{}")),
				"workspace password accepted (mixed window)")
			require.Equal(t, http.StatusUnauthorized, authedStatus(t, h, "wrong-pw", []byte("{}")),
				"unknown rejected")
		})
	}
}

// TestMCPEndpoint_WorkspacePasswordOnly: /v1/mcp keeps the workspace
// password carve-out — agentdPassword must NOT unlock it.
func TestMCPEndpoint_WorkspacePasswordOnly(t *testing.T) {
	h := mcpHandler(testWorkspacePW)
	require.NotEqual(t, http.StatusUnauthorized, authedStatus(t, h, testWorkspacePW, []byte("{}")),
		"workspace password passes MCP auth")
	require.Equal(t, http.StatusUnauthorized, authedStatus(t, h, testAgentdPW, []byte("{}")),
		"agentdPassword must NOT authenticate on /v1/mcp (per-endpoint table)")
}

// TestDevPreview_WorkspacePasswordOnly: same carve-out for dev-preview.
func TestDevPreview_WorkspacePasswordOnly(t *testing.T) {
	h := devPreviewHandler(testWorkspacePW)
	require.Equal(t, http.StatusUnauthorized, authedStatus(t, h, testAgentdPW, nil),
		"agentdPassword must NOT authenticate on /v1/dev-preview (per-endpoint table)")
}

// TestCheckBasicAuthAny_EmptyCredentialNeverMatches: an unset
// control-plane password (single-container mode) must not open an
// authenticatable "opencode:" empty-password path.
func TestCheckBasicAuthAny_EmptyCredentialNeverMatches(t *testing.T) {
	emptyReq := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	emptyReq.SetBasicAuth(agentd.AuthUsername, "")
	require.False(t, checkBasicAuthAny(emptyReq, "", testWorkspacePW),
		"an empty credential entry must be skipped — an empty-password request must not match it")

	validReq := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	validReq.SetBasicAuth(agentd.AuthUsername, testWorkspacePW)
	require.True(t, checkBasicAuthAny(validReq, testWorkspacePW, ""),
		"order-independence: the valid entry matches regardless of position")
	require.False(t, checkBasicAuthAny(validReq),
		"no credentials configured → nothing matches")
}
