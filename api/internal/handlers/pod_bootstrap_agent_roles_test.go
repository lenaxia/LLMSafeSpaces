// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// pod_bootstrap_agent_roles_test.go — the vocabulary-delivery pins for
// review round 2 of the role-only model (PR #1990 F1): the bootstrap
// response carries the workspace's addressable agent-role vocabulary
// (platform + the workspace's org — the same composition ListForMe
// and the board role directory serve), the pod stages it and the
// ConfigWriter materializes named opencode agents from it (the A8
// resolution seam). Reader unwired ⇒ the field is omitted (the pod
// boots with no named agents); list failures are non-fatal.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakeAgentRoleReader struct {
	platform []*types.AgentRole
	org      map[string][]*types.AgentRole
	orgErr   map[string]error
	err      error

	orgCalled bool
}

func (f *fakeAgentRoleReader) ListAgentRoles(_ context.Context, scope, orgID string) ([]*types.AgentRole, error) {
	if scope == "platform" {
		return f.platform, f.err
	}
	f.orgCalled = true
	if err, ok := f.orgErr[orgID]; ok {
		return nil, err
	}
	return f.org[orgID], nil
}

func newAgentRolesRouter(t *testing.T, lookup *fakeBootstrapLookup, reader bootstrapAgentRoleReader) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-abc"},
		&fakeBootstrapInjector{}, lookup, nil, testBootstrapNamespace)
	if reader != nil {
		h.SetAgentRoleReader(reader)
	}
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)
	return r
}

func decodeBootstrapRoles(t *testing.T, w *httptest.ResponseRecorder) []types.MaterializedAgentRole {
	t.Helper()
	var resp struct {
		AgentRoles []types.MaterializedAgentRole `json:"agentRoles"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.AgentRoles
}

func TestPodBootstrap_AgentRoles_PlatformPlusOrgComposition(t *testing.T) {
	orgID := "org-7"
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", OrgID: &orgID}}
	reader := &fakeAgentRoleReader{
		platform: []*types.AgentRole{{
			ID: "plat-role", Scope: "platform", Name: "Reviewer", Slug: "reviewer",
			Config: types.RoleConfig{System: strPtr("RECORD VERDICTS"), Mode: strPtr("primary")},
		}},
		org: map[string][]*types.AgentRole{
			orgID: {{
				ID: "org-role", Scope: "org", Name: "Auditor", Slug: "auditor",
				Config: types.RoleConfig{System: strPtr("AUDIT"), Model: strPtr("openai/gpt-4o"), Tools: json.RawMessage(`{"bash":false}`)},
			}},
		},
	}
	router := newAgentRolesRouter(t, lookup, reader)

	w := doBootstrap(t, router, "tok", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)

	roles := decodeBootstrapRoles(t, w)
	require.Len(t, roles, 2)
	assert.Equal(t, "plat-role", roles[0].ID)
	assert.Equal(t, "reviewer", roles[0].Slug)
	assert.Equal(t, "RECORD VERDICTS", roles[0].System, "the role's system prompt must ride the delivery (it becomes the materialized agent's prompt)")
	assert.Equal(t, "primary", roles[0].Mode)
	assert.Empty(t, roles[0].Model, "absent config fields stay empty — no phantom defaults")
	assert.Equal(t, "org-role", roles[1].ID)
	assert.Equal(t, "openai/gpt-4o", roles[1].Model)
	assert.Equal(t, json.RawMessage(`{"bash":false}`), roles[1].Tools)
	require.True(t, reader.orgCalled, "the workspace's org scope must be consulted")
}

func TestPodBootstrap_AgentRoles_StandaloneWorkspaceSkipsOrg(t *testing.T) {
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	reader := &fakeAgentRoleReader{platform: []*types.AgentRole{{ID: "plat-role", Slug: "reviewer"}}}
	router := newAgentRolesRouter(t, lookup, reader)

	w := doBootstrap(t, router, "tok", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)
	roles := decodeBootstrapRoles(t, w)
	require.Len(t, roles, 1)
	assert.False(t, reader.orgCalled, "no org on the workspace — no org scope read")
}

func TestPodBootstrap_AgentRoles_OrgFailureNonFatal(t *testing.T) {
	orgID := "org-7"
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1", OrgID: &orgID}}
	reader := &fakeAgentRoleReader{
		platform: []*types.AgentRole{{ID: "plat-role"}},
		orgErr:   map[string]error{orgID: assert.AnError},
	}
	router := newAgentRolesRouter(t, lookup, reader)

	w := doBootstrap(t, router, "tok", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code, "an org list failure is non-fatal — the platform scope still delivers")
	roles := decodeBootstrapRoles(t, w)
	require.Len(t, roles, 1)
}

func TestPodBootstrap_AgentRoles_ReaderUnwiredOmitted(t *testing.T) {
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newAgentRolesRouter(t, lookup, nil)

	w := doBootstrap(t, router, "tok", `{"workspaceID":"ws-abc"}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), `"agentRoles"`,
		"no reader wired (tests/local) — the field is omitted, the pod materializes no named agents")
}

func TestPodBootstrap_AgentRoles_HasReaderReflectsWiring(t *testing.T) {
	h := NewPodBootstrapHandler(&fakeTokenReviewer{}, &fakeBootstrapInjector{}, &fakeBootstrapLookup{}, nil, testBootstrapNamespace)
	assert.False(t, h.HasAgentRoleReader())
	h.SetAgentRoleReader(&fakeAgentRoleReader{})
	assert.True(t, h.HasAgentRoleReader())
}

func TestMaterializedAgentRoles_ProjectionGuards(t *testing.T) {
	system := "S"
	out := materializedAgentRoles([]*types.AgentRole{
		nil,
		{ID: "   "},
		{ID: "r1", Slug: "rev", Name: "Reviewer", Config: types.RoleConfig{
			System: &system, Description: strPtr("desc"),
		}},
	})
	require.Len(t, out, 1, "nil rows and blank IDs are skipped")
	assert.Equal(t, "r1", out[0].ID)
	assert.Equal(t, "S", out[0].System)
	assert.Equal(t, "desc", out[0].Description)
	assert.Empty(t, out[0].Model)
	// Empty vocabulary renders nil (omitempty on the wire — no phantom []).
	assert.Nil(t, materializedAgentRoles([]*types.AgentRole{{ID: " "}}))
}
