// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1729: cross-workspace routine triggers (owner-scoped) — red-first
// authz pins.
//
// The automation surface may create routine triggers that run in a
// DIFFERENT workspace, provided the resolved trigger OWNER has
// owner/admin rights on the target (the platform's existing
// CheckOwnership semantics: creator or org admin). Nonexistent targets
// answer 404, unauthorized targets 403 — the cross-workspace disclosure
// policy. Omitting targetWorkspaceId keeps the forced-stamp behavior
// byte-identical (#1412).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// fakeWorkspaceAuthorizer models (*workspace.Service).ResolveWorkspace +
// CheckOwnership for the #1729 gate: metas answers existence (NotFound
// for the rest), creator or org-admin passes, everyone else is
// Forbidden — the disclosure policy's 404/403 split.
type fakeWorkspaceAuthorizer struct {
	metas  map[string]*types.WorkspaceMetadata
	admins map[string]map[string]bool // orgID → set of admin userIDs
}

func (f *fakeWorkspaceAuthorizer) ResolveWorkspace(_ context.Context, workspaceID string) (*types.WorkspaceMetadata, error) {
	meta, ok := f.metas[workspaceID]
	if !ok {
		return nil, apierrors.NewNotFoundError("workspace", workspaceID, fmt.Errorf("workspace not found"))
	}
	return meta, nil
}

func (f *fakeWorkspaceAuthorizer) CheckOwnership(_ context.Context, userID string, meta *types.WorkspaceMetadata) error {
	if meta == nil {
		return apierrors.NewForbiddenError("workspace access denied", fmt.Errorf("nil metadata"))
	}
	if meta.UserID == userID {
		return nil
	}
	if meta.OrgID != nil && f.admins[*meta.OrgID][userID] {
		return nil
	}
	return apierrors.NewForbiddenError("workspace access denied", fmt.Errorf("user %s does not have access to workspace %s", userID, meta.ID))
}

// crossWSAuthorizer is the #1729 fixture set:
//
//	ws-1    — the creating pod's workspace (owner user-7)
//	ws-2    — another workspace owned by user-7 (the owner→own case)
//	ws-evil — a REAL workspace owned by user-9 (the non-owned 403 case)
//	ws-org  — org-attributed workspace created by user-9; user-7 is an
//	          org admin (the admin half of "owner/admin rights")
func crossWSAuthorizer() *fakeWorkspaceAuthorizer {
	org := "org-42"
	return &fakeWorkspaceAuthorizer{
		metas: map[string]*types.WorkspaceMetadata{
			"ws-1":    {ID: "ws-1", UserID: "user-7"},
			"ws-2":    {ID: "ws-2", UserID: "user-7"},
			"ws-evil": {ID: "ws-evil", UserID: "user-9"},
			"ws-org":  {ID: "ws-org", UserID: "user-9", OrgID: &org},
		},
		admins: map[string]map[string]bool{org: {"user-7": true}},
	}
}

// newCrossWSAutomationRouter mounts the pod-automation surface with the
// REAL delegated user handlers over mock stores, wiring the #1729
// ownership authorizer (mirrors newAutomationRouter; separate helper so
// the shared harness stays untouched).
func newCrossWSAutomationRouter(t *testing.T, authorizer *fakeWorkspaceAuthorizer) (*gin.Engine, *mockTriggerStore, *mockWorkflowStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	wfStore := newMockWorkflowStore()
	th := NewUserTriggersHandler(trigStore, nil, &mockEncryptor{})
	th.SetWorkspaceOwnershipAuthorizer(authorizer)
	h := &PodAutomationHandler{
		tokenReviewer:     automationReviewer(),
		lookup:            automationLookup(),
		triggers:          th,
		workflows:         NewUserWorkflowsHandler(wfStore, nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)
	r.PUT("/internal/v1/automation/triggers/:id", h.TriggerUpdate)
	return r, trigStore, wfStore
}

const crossWSCreateBody = `{
	"workspaceID":"ws-1",
	"name":"cross-ws-routine",
	"sourceType":"cron",
	"sourceConfig":{"expr":"5 * * * *"},
	"prompt":"run in the target workspace"
}`

// Owner targeting their OWN other workspace: created, and the routine
// target is the NAMED workspace — not the creating pod's.
func TestPodAutomation_CrossWorkspaceRoutineCreate_OwnedTarget(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"name":"cross-ws-routine",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"run in the target workspace"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		assert.Equal(t, "user-7", row.OwnerID, "trigger stays owned by the resolved owner")
		require.NotNil(t, row.WorkspaceID, "routine target must be set")
		assert.Equal(t, "ws-2", *row.WorkspaceID, "routine target is the NAMED workspace")
	}
	assert.Contains(t, w.Body.String(), `"workspaceId":"ws-2"`, "response reports the target workspace")
}

// Owner targeting a workspace they hold NO rights on: 403, nothing
// persisted.
func TestPodAutomation_CrossWorkspaceRoutineCreate_NonOwnedTarget(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-evil",
		"name":"cross-ws-routine",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "non-owned target must be rejected: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "target workspace", "rejection names the target workspace")
	assert.Empty(t, trigStore.triggers, "rejected create must not persist a trigger")
}

// Owner targeting a NONEXISTENT workspace: 404 (the cross-workspace
// disclosure policy — same answer as any non-enumerable id).
func TestPodAutomation_CrossWorkspaceRoutineCreate_NonexistentTarget(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-ghost",
		"name":"cross-ws-routine",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "nonexistent target must 404: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "target workspace not found")
	assert.Empty(t, trigStore.triggers, "rejected create must not persist a trigger")
}

// The admin half of "owner/admin rights": an org workspace created by
// ANOTHER user is a legal target for the org's admin.
func TestPodAutomation_CrossWorkspaceRoutineCreate_OrgAdminTarget(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-org",
		"name":"cross-ws-routine",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		require.NotNil(t, row.WorkspaceID)
		assert.Equal(t, "ws-org", *row.WorkspaceID, "org-admin target allowed")
	}
}

// A pod identity that does not match the resolved workspace never gets
// as far as target authz (pre-existing pin; the cross-workspace feature
// must not weaken it).
func TestPodAutomation_CrossWorkspaceRoutineCreate_IdentityMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	h := &PodAutomationHandler{
		tokenReviewer:     &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-OTHER"},
		lookup:            automationLookup(),
		triggers:          NewUserTriggersHandler(trigStore, nil, &mockEncryptor{}),
		workflows:         NewUserWorkflowsHandler(newMockWorkflowStore(), nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"name":"cross-ws-routine",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, trigStore.triggers)
}

// targetWorkspaceId and a workflow target are mutually exclusive — the
// workflow-targeting rules are unchanged by #1729.
func TestPodAutomation_CrossWorkspaceRoutineCreate_WorkflowAndTargetRejected(t *testing.T) {
	r, trigStore, wfStore := newCrossWSAutomationRouter(t, crossWSAuthorizer())
	wsID := "ws-1"
	row := &wf.WorkflowRow{ID: "wf-1", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &wsID}
	// Seed both mock stores (the seam's workflow gate reads the
	// workflows-handler store; the delegated create's existence check
	// reads the triggers-handler store).
	trigStore.workflows["wf-1"] = row
	wfStore.workflows["wf-1"] = row

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"workflowId":"wf-1",
		"name":"both-targets",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"}
	}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "workflowId + targetWorkspaceId must be rejected: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "targetWorkspaceId")
	assert.Empty(t, trigStore.triggers)
}

// Backward compatibility: omitting the param keeps the #1412 forced
// stamp, byte-identical delegated body, and a plain owned-routine
// response.
func TestPodAutomation_CrossWorkspaceRoutineCreate_ParamOmittedUnchanged(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", crossWSCreateBody)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		require.NotNil(t, row.WorkspaceID)
		assert.Equal(t, "ws-1", *row.WorkspaceID, "omitted param → forced stamp to the pod's workspace")
		assert.Nil(t, row.WorkflowID)
	}
	assert.Contains(t, w.Body.String(), `"workspaceId":"ws-1"`)

	// The seam's delegated body for a param-less create is pinned
	// byte-for-byte (sorted-key marshal + the stamp): #1729 must not
	// perturb the #1412 wire shape.
	out, wfID, _, err := scopeTriggerCreateBody([]byte(crossWSCreateBody), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, wfID)
	assert.JSONEq(t, `{"name":"cross-ws-routine","prompt":"run in the target workspace","sourceConfig":{"expr":"5 * * * *"},"sourceType":"cron","workspaceId":"ws-1"}`, string(out))
}

// capturingAudit records trigger.create metadata for the #1729 audit pin.
type capturingAudit struct {
	metas []map[string]any
}

func (m *capturingAudit) LogAuditEvent(_ context.Context, _, _, action, _ string, _ *string, meta map[string]any) error {
	if action == "trigger.create" {
		m.metas = append(m.metas, meta)
	}
	return nil
}
func (m *capturingAudit) LogOrgEvent(_ context.Context, _, _, _, _ string, _ map[string]any) error {
	return nil
}

// The creation event records BOTH ends: the creating-session workspace
// (the pod's) and the routine's target workspace.
func TestPodAutomation_CrossWorkspaceRoutineCreate_AuditRecordsBothWorkspaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	audit := &capturingAudit{}
	th := NewUserTriggersHandler(trigStore, nil, &mockEncryptor{})
	th.SetAudit(audit)
	th.SetWorkspaceOwnershipAuthorizer(crossWSAuthorizer())
	h := &PodAutomationHandler{
		tokenReviewer:     automationReviewer(),
		lookup:            automationLookup(),
		triggers:          th,
		workflows:         NewUserWorkflowsHandler(newMockWorkflowStore(), nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"name":"audited-cross-ws",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, audit.metas, 1, "exactly one trigger.create event")
	meta := audit.metas[0]
	assert.Equal(t, "ws-2", meta["targetWorkspaceId"], "audit names the routine's target workspace")
	assert.Equal(t, "ws-1", meta["creatingWorkspaceId"], "audit names the creating-session workspace")
}

// Fail closed: a targetWorkspaceId create on a handler built WITHOUT the
// ownership gate answers 500 — never a silent existence-only create.
func TestPodAutomation_CrossWorkspaceRoutineCreate_UnwiredFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	h := &PodAutomationHandler{
		tokenReviewer:     automationReviewer(),
		lookup:            automationLookup(),
		triggers:          NewUserTriggersHandler(trigStore, nil, &mockEncryptor{}),
		workflows:         NewUserWorkflowsHandler(newMockWorkflowStore(), nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"targetWorkspaceId":"ws-2",
		"name":"unwired",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"p"
	}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "unwired ownership gate must fail closed: %s", w.Body.String())
	assert.Empty(t, trigStore.triggers)
}

// Update patches carrying targetWorkspaceId (any spelling) are rejected
// loudly — the key is create-only, and a silent ignore would tell the
// caller a retarget happened that did not (#1426 keeps updates scoped).
func TestPodAutomation_CrossWorkspaceRoutineUpdate_TargetKeyRejected(t *testing.T) {
	r, trigStore, _ := newCrossWSAutomationRouter(t, crossWSAuthorizer())
	id := "trig-x"
	wsID := "ws-2"
	trigStore.triggers[id] = &wf.TriggerRow{
		ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7",
		Name: "x", Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"5 * * * *"}`), WorkspaceID: &wsID,
	}

	for _, body := range []string{
		`{"targetWorkspaceId":"ws-1"}`,
		`{"targetWorkspaceId":""}`,
		`{"TargetWorkspaceId":"ws-1"}`,
		`{"target_workspace_id":"ws-1"}`,
	} {
		w := doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok", body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "update must reject the create-only key: %s → %s", body, w.Body.String())
		assert.Contains(t, w.Body.String(), "create-only")
	}
	assert.Equal(t, "ws-2", *trigStore.triggers[id].WorkspaceID, "rejected patch must not move the target")
}
