// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// newAutomationRouter mounts the full pod-automation surface with the
// REAL delegated handlers (user TriggersHandler/WorkflowsHandler) over
// mock stores — the delegation-not-duplication contract is tested as an
// integrated chain, not a fake downstream.
func newAutomationRouter(t *testing.T, reviewer TokenReviewer, lookup bootstrapWorkspaceLookup) (*gin.Engine, *mockTriggerStore, *mockWorkflowStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	wfStore := newMockWorkflowStore()
	h := &PodAutomationHandler{
		tokenReviewer:     reviewer,
		lookup:            lookup,
		triggers:          NewUserTriggersHandler(trigStore, nil, &mockEncryptor{}),
		workflows:         NewUserWorkflowsHandler(wfStore, nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.GET("/internal/v1/automation/triggers", h.TriggerList)
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)
	r.GET("/internal/v1/automation/triggers/:id", h.TriggerGet)
	r.PUT("/internal/v1/automation/triggers/:id", h.TriggerUpdate)
	r.DELETE("/internal/v1/automation/triggers/:id", h.TriggerDelete)
	r.GET("/internal/v1/automation/triggers/:id/fires", h.TriggerFires)
	r.POST("/internal/v1/automation/triggers/:id/rotate-secret", h.TriggerRotateWebhookSecret)
	r.GET("/internal/v1/automation/workflows", h.WorkflowList)
	r.POST("/internal/v1/automation/workflows", h.WorkflowCreate)
	r.GET("/internal/v1/automation/workflows/:id", h.WorkflowGet)
	r.PUT("/internal/v1/automation/workflows/:id", h.WorkflowUpdate)
	r.DELETE("/internal/v1/automation/workflows/:id", h.WorkflowDelete)
	r.POST("/internal/v1/automation/workflows/:id/runs", h.WorkflowRun)
	r.GET("/internal/v1/automation/workflows/:id/runs", h.WorkflowRuns)
	return r, trigStore, wfStore
}

func automationLookup() *fakeBootstrapLookup {
	return &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
}

func automationReviewer() *fakeTokenReviewer {
	return &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
}

func doAutomation(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPodAutomation_AuthMatrix(t *testing.T) {
	r, _, _ := newAutomationRouter(t, &fakeTokenReviewer{}, automationLookup())

	w := doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no token")

	reviewer := &fakeTokenReviewer{err: errTokenNotAuthenticated}
	r, _, _ = newAutomationRouter(t, reviewer, automationLookup())
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "rejected token")

	reviewer = &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-OTHER"}
	r, _, _ = newAutomationRouter(t, reviewer, automationLookup())
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "SA/workspace mismatch")

	reviewer = &fakeTokenReviewer{username: "system:serviceaccount:other-ns:workspace-ws-1"}
	r, _, _ = newAutomationRouter(t, reviewer, automationLookup())
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "namespace mismatch")

	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers", "tok", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing workspaceID entirely")

	reviewer = &fakeTokenReviewer{err: assert.AnError}
	r, _, _ = newAutomationRouter(t, reviewer, automationLookup())
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "review transport failure")

	reviewer = automationReviewer()
	r, _, _ = newAutomationRouter(t, reviewer, &fakeBootstrapLookup{err: assert.AnError})
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "lookup failure")

	r, _, _ = newAutomationRouter(t, reviewer, &fakeBootstrapLookup{ws: nil})
	w = doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "workspace not found")
}

// In-body workspaceID on creates: the auth path accepts the seam's
// body-stamped spelling, and a mismatched one is rejected pre-delegation.
func TestPodAutomation_BodyWorkspaceID(t *testing.T) {
	r, _, _ := newAutomationRouter(t, automationReviewer(), automationLookup())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{"workspaceID":"ws-OTHER"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)

	w = doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "no workspaceID in query or body")
}

// The full create chain: body survives to the REAL TriggersHandler
// (multi-buffer replay), binds, and lands in the store owned by the
// resolved owner with the FORCED workspace scoping.
func TestPodAutomation_DelegatedTriggerCreate(t *testing.T) {
	r, trigStore, _ := newAutomationRouter(t, automationReviewer(), automationLookup())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"workspaceId":"ws-ATTACKER",
		"name":"nightly-report",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"prompt":"summarize the day"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		assert.Equal(t, "user-7", row.OwnerID, "delegation binds the resolved owner")
		assert.Equal(t, types.WorkflowOwnerUser, row.OwnerType)
		assert.Equal(t, "nightly-report", row.Name)
		assert.Equal(t, "cron", row.SourceType)
		assert.Equal(t, "summarize the day", row.Prompt)
		require.NotNil(t, row.WorkspaceID, "trigger targets a workspace")
		assert.Equal(t, "ws-1", *row.WorkspaceID, "routine target FORCED to this pod's workspace — never ws-ATTACKER")
	}
}

// The tool-wrapper shape ({"trigger":{...}}) must reach the delegated
// handler UNWRAPPED — the real handler binds flat CreateTriggerRequest.
func TestPodAutomation_WrapperBodyRejectedByRealHandler(t *testing.T) {
	r, _, _ := newAutomationRouter(t, automationReviewer(), automationLookup())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"trigger":{"name":"x","sourceType":"cron","sourceConfig":{"expr":"5 * * * *"},"prompt":"p"}
	}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "wrapper body fails flat binding — the tool layer must unwrap")
}

// The update chain: query-borne workspaceID (patches carry no scoping
// fields), body survives to the REAL update bind.
func TestPodAutomation_DelegatedTriggerUpdate(t *testing.T) {
	r, trigStore, _ := newAutomationRouter(t, automationReviewer(), automationLookup())
	id := "trig-1"
	trigStore.triggers[id] = &wf.TriggerRow{ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", Name: "old", Enabled: true}

	w := doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok", `{"enabled":false,"prompt":"new prompt"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := trigStore.triggers[id]
	require.NotNil(t, row)
	assert.False(t, row.Enabled, "patch applied through delegation")
	assert.Equal(t, "new prompt", row.Prompt)
}

// The run chain: {"input":...} body + query-borne workspaceID reach the
// REAL runWorkflow; the run lands queued with the caller's input.
func TestPodAutomation_DelegatedWorkflowRun(t *testing.T) {
	r, _, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	target := "ws-1"
	wfStore.workflows["wf-1"] = &wf.WorkflowRow{ID: "wf-1", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &target}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/workflows/wf-1/runs?workspaceID=ws-1", "tok", `{"input":{"topic":"ship"}}`)
	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	require.NotNil(t, wfStore.lastRun, "run row created")
	assert.Equal(t, "wf-1", wfStore.lastRun.WorkflowID)
	assert.Equal(t, types.RunStatusQueued, wfStore.lastRun.Status)
	assert.JSONEq(t, `{"topic":"ship"}`, string(wfStore.lastRun.Input), "input survives the replay verbatim")
}

// A body larger than one transport buffer must survive the
// capture-then-replay path intact (the single-Read bug class).
func TestPodAutomation_LargeBodyReplay(t *testing.T) {
	r, trigStore, _ := newAutomationRouter(t, automationReviewer(), automationLookup())

	bigPrompt := strings.Repeat("x", 64*1024)
	body := `{"workspaceID":"ws-1","name":"big","sourceType":"cron","sourceConfig":{"expr":"5 * * * *"},"prompt":"` + bigPrompt + `"}`
	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", body)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	for _, row := range trigStore.triggers {
		assert.Len(t, row.Prompt, 64*1024, "full body replayed, no truncation")
	}
}

// Rotate through the automation surface: the REAL rotate handler runs
// delegated as the owner and the new credential lands in the store.
func TestPodAutomation_DelegatedRotate(t *testing.T) {
	r, trigStore, _ := newAutomationRouter(t, automationReviewer(), automationLookup())
	id := "trig-hook"
	trigStore.triggers[id] = &wf.TriggerRow{ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", Name: "hook", Enabled: true, SourceType: "webhook"}
	// Seed the webhook row via the shared store contract.
	require.NoError(t, trigStore.CreateWebhook(context.Background(), &wf.WebhookRow{ID: "wh-1", TriggerID: id, SecretCipher: []byte("old"), KeyVersion: 1}))

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers/"+id+"/rotate-secret?workspaceID=ws-1", "tok", "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		WebhookSecret string `json:"webhookSecret"`
		WebhookURL    string `json:"webhookUrl"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.WebhookSecret)
	assert.Equal(t, "/api/v1/hooks/"+id, resp.WebhookURL, "advertises the trigger-id URL")
	hook, err := trigStore.GetWebhookByTriggerID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, []byte("enc:"+resp.WebhookSecret), hook.SecretCipher, "store carries the encrypted new secret")
}

func TestScopeTriggerCreateBody(t *testing.T) {
	out, wfID, err := scopeTriggerCreateBody([]byte(`{"name":"x","workspaceId":"ws-OTHER","prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, wfID, "routine body carries no workflow target")
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`, "DTO spelling forced to this pod's workspace")
	assert.NotContains(t, string(out), "ws-OTHER")
	assert.Contains(t, string(out), `"prompt":"p"`, "sibling fields preserved verbatim")

	out, wfID, err = scopeTriggerCreateBody([]byte(`{"name":"x","workflowId":"wf-9","prompt":"p","workspaceID":"ws-CALLER","workspaceId":"ws-CALLER2"}`), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "wf-9", wfID, "workflow target surfaced for the scoping check")
	assert.Contains(t, string(out), `"workflowId":"wf-9"`, "workflow target preserved")
	assert.NotContains(t, string(out), `"workspaceId"`, "routine stamp must NOT ride along — the DTO rejects both together")
	assert.NotContains(t, string(out), `"workspaceID"`, "resolver spelling must not leak into the delegated body (case-insensitive DTO bind)")
	assert.NotContains(t, string(out), "ws-CALLER", "caller-supplied workspace values never survive")

	out, wfID, err = scopeTriggerCreateBody([]byte(`{"name":"x","workflow_id":"wf-9","prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "wf-9", wfID, "snake_case alias normalized onto the DTO spelling")
	assert.Contains(t, string(out), `"workflowId":"wf-9"`)
	assert.NotContains(t, string(out), "workflow_id", "alias removed — one canonical spelling on the wire")

	// Case-variant workspace key: the decoder binds DTO fields
	// case-insensitively and map keys marshal sorted, so a surviving
	// variant would override the forced stamp (review finding on #1412).
	out, wfID, err = scopeTriggerCreateBody([]byte(`{"name":"x","workspaceid":"ws-EVIL","prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, wfID)
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`, "case-variant caller key stripped, stamp survives")
	assert.NotContains(t, string(out), "ws-EVIL")

	// Contradictory alias duplicates are an explicit error, not a pick.
	_, _, err = scopeTriggerCreateBody([]byte(`{"name":"x","workflow_id":"wf-1","workflowId":"wf-2"}`), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different values")

	// Non-string workflowId surfaces its specific error.
	_, _, err = scopeTriggerCreateBody([]byte(`{"name":"x","workflowId":123}`), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflowId must be a string")

	// A JSON null body unmarshals to a nil map WITHOUT error — the guard
	// must reject it before the stamp writes into the nil map (panic
	// class; round-2 review finding).
	_, _, err = scopeTriggerCreateBody([]byte(`null`), "ws-1")
	require.Error(t, err, "null body must not reach the stamp (nil-map panic)")
	assert.Contains(t, err.Error(), "trigger body must be a JSON object")

	// An empty object is a valid (if minimal) body — no panic, stamped.
	out, _, err = scopeTriggerCreateBody([]byte(`{}`), "ws-1")
	require.NoError(t, err)
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`)

	_, _, err = scopeTriggerCreateBody([]byte(`not json`), "ws-1")
	assert.Error(t, err, "non-object bodies are an explicit error, not a silent skip")
}

// Workflow-targeted (DAG) triggers through the automation surface: the
// workflow must exist, belong to the resolved owner, and target THIS
// pod's workspace — the DAG equivalent of the routine workspace stamp.
func TestPodAutomation_WorkflowTargetedTriggerCreate(t *testing.T) {
	r, trigStore, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	target := "ws-1"
	other := "ws-OTHER"
	wfStore.workflows["wf-ok"] = &wf.WorkflowRow{ID: "wf-ok", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &target}
	wfStore.workflows["wf-away"] = &wf.WorkflowRow{ID: "wf-away", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &other}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"name":"dag-trigger",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"workflowId":"wf-ok"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		require.NotNil(t, row.WorkflowID, "trigger targets the workflow")
		assert.Equal(t, "wf-ok", *row.WorkflowID)
		assert.Nil(t, row.WorkspaceID, "workflow-targeted trigger carries no routine workspace")
	}

	// Workflow targeting another workspace: the pod-scoping rule.
	w = doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"name":"cross-ws",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"workflowId":"wf-away"
	}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "cross-workspace DAG scheduling rejected")
	assert.Contains(t, w.Body.String(), "does not target this workspace")

	// Unknown workflow: distinguishable from the scoping rejection.
	w = doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"name":"ghost",
		"sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},
		"workflowId":"wf-nope"
	}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "missing workflow reported explicitly")
}

func TestPodAutomation_LoggerWired(t *testing.T) {
	h := &PodAutomationHandler{}
	assert.False(t, h.HasLogger())
	h.SetLogger(lmocks.NewMockLogger())
	assert.True(t, h.HasLogger())
}

// mockAutomationAudit captures audit events from the delegated handlers.
type mockAutomationAudit struct {
	events []string
}

func (m *mockAutomationAudit) LogAuditEvent(_ context.Context, domain, actorID, action, targetID string, _ *string, _ map[string]any) error {
	m.events = append(m.events, domain+"/"+actorID+"/"+action+"/"+targetID)
	return nil
}
func (m *mockAutomationAudit) LogOrgEvent(_ context.Context, _, _, _, _ string, _ map[string]any) error {
	return nil
}

// Credential issuance must be auditable: rotation through the automation
// surface leaves a trigger.rotate_webhook_secret event naming the
// resolved owner as actor — and never the secret itself.
func TestPodAutomation_RotateIsAudited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	trigStore := newMockTriggerStore()
	audit := &mockAutomationAudit{}
	th := NewUserTriggersHandler(trigStore, nil, &mockEncryptor{})
	th.SetAudit(audit)
	h := &PodAutomationHandler{
		tokenReviewer: automationReviewer(), lookup: automationLookup(),
		triggers: th, workflows: NewUserWorkflowsHandler(newMockWorkflowStore(), nil),
		expectedNamespace: testRenameNamespace,
	}
	r := gin.New()
	r.POST("/internal/v1/automation/triggers/:id/rotate-secret", h.TriggerRotateWebhookSecret)

	id := "trig-aud"
	trigStore.triggers[id] = &wf.TriggerRow{ID: id, OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", Name: "hook", Enabled: true, SourceType: "webhook"}
	require.NoError(t, trigStore.CreateWebhook(context.Background(), &wf.WebhookRow{ID: "wh-a", TriggerID: id, SecretCipher: []byte("old"), KeyVersion: 1}))

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers/"+id+"/rotate-secret?workspaceID=ws-1", "tok", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, audit.events, 1, "rotation leaves exactly one audit event")
	assert.Equal(t, "triggers/user-7/trigger.rotate_webhook_secret/"+id, audit.events[0])
	assert.NotContains(t, w.Body.String()+fmt.Sprint(audit.events), "whs_", "no secret material leaks into response or audit")
}

// --- #1426: the scoping rule holds on the update path too ---

func TestScopeTriggerUpdateBody(t *testing.T) {
	// Unrelated patch: verbatim passthrough.
	out, wfID, err := scopeTriggerUpdateBody([]byte(`{"enabled":false,"prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, wfID)
	assert.JSONEq(t, `{"enabled":false,"prompt":"p"}`, string(out), "no target keys -> untouched")

	// Workspace retarget attempt: forced to THIS workspace.
	out, _, err = scopeTriggerUpdateBody([]byte(`{"workspaceid":"ws-OTHER","enabled":true}`), "ws-1")
	require.NoError(t, err)
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`, "case-variant retarget forced to this pod's workspace")
	assert.NotContains(t, string(out), "ws-OTHER")
	assert.Contains(t, string(out), `"enabled":true`, "sibling fields ride along")

	// DAG retarget: gated by the caller (workflowId returned), no stamp.
	out, wfID, err = scopeTriggerUpdateBody([]byte(`{"workflowId":"wf-9","prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, "wf-9", wfID)
	assert.Contains(t, string(out), `"workflowId":"wf-9"`)
	assert.NotContains(t, string(out), `"workspaceId"`)

	// Clearing the DAG target without naming a workspace: becomes a
	// routine HERE, never a targetless zombie.
	out, wfID, err = scopeTriggerUpdateBody([]byte(`{"workflowId":""}`), "ws-1")
	require.NoError(t, err)
	assert.Empty(t, wfID)
	assert.Contains(t, string(out), `"workflowId":""`)
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`)

	// Same guards as create.
	_, _, err = scopeTriggerUpdateBody([]byte(`null`), "ws-1")
	require.Error(t, err)
	_, _, err = scopeTriggerUpdateBody([]byte(`{"workflowId":123}`), "ws-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workflowId must be a string")
}

// The full update chain: a routine trigger created for THIS workspace
// cannot be retargeted to another workspace via the pod seam.
func TestPodAutomation_TriggerUpdateCannotRetargetWorkspace(t *testing.T) {
	r, trigStore, _ := newAutomationRouter(t, automationReviewer(), automationLookup())

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1","name":"hostage","sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},"prompt":"p"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	id := created["id"].(string)

	w = doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok",
		`{"workspaceId":"ws-EVIL","enabled":false}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := trigStore.triggers[id]
	require.NotNil(t, row)
	require.NotNil(t, row.WorkspaceID)
	assert.Equal(t, "ws-1", *row.WorkspaceID, "retarget to another workspace silently forced back to this pod's")
	assert.False(t, row.Enabled, "sibling patch fields still apply")
}

// The full update chain: DAG retarget is gated on existence + ownership +
// THIS workspace, exactly like create.
func TestPodAutomation_TriggerUpdateWorkflowTargetGating(t *testing.T) {
	r, trigStore, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	target := "ws-1"
	other := "ws-OTHER"
	wfStore.workflows["wf-ok"] = &wf.WorkflowRow{ID: "wf-ok", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &target}
	wfStore.workflows["wf-away"] = &wf.WorkflowRow{ID: "wf-away", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &other}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1","name":"gated","sourceType":"cron",
		"sourceConfig":{"expr":"5 * * * *"},"prompt":"p"
	}`)
	require.Equal(t, http.StatusCreated, w.Code)
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	id := created["id"].(string)

	// Retarget to a workflow in ANOTHER workspace: rejected.
	w = doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok",
		`{"workflowId":"wf-away"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "does not target this workspace")

	// Retarget to a nonexistent workflow: rejected.
	w = doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok",
		`{"workflowId":"wf-nope"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Retarget to THIS workspace's workflow: allowed, stored.
	w = doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok",
		`{"workflowId":"wf-ok"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	row := trigStore.triggers[id]
	require.NotNil(t, row.WorkflowID)
	assert.Equal(t, "wf-ok", *row.WorkflowID)

	// Clearing the DAG target lands the routine HERE.
	w = doAutomation(t, r, "PUT", "/internal/v1/automation/triggers/"+id+"?workspaceID=ws-1", "tok",
		`{"workflowId":""}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	row = trigStore.triggers[id]
	assert.Nil(t, row.WorkflowID, "DAG target cleared")
	require.NotNil(t, row.WorkspaceID, "routine target stamped")
	assert.Equal(t, "ws-1", *row.WorkspaceID)
}
