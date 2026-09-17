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
		wfStore:           wfStore,
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

func TestForceTriggerWorkspace(t *testing.T) {
	out, err := forceTriggerWorkspace([]byte(`{"name":"x","workspaceId":"ws-OTHER","prompt":"p"}`), "ws-1")
	require.NoError(t, err)
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`, "DTO spelling forced to this pod's workspace")
	assert.NotContains(t, string(out), "ws-OTHER")
	assert.Contains(t, string(out), `"prompt":"p"`, "sibling fields preserved verbatim")

	_, err = forceTriggerWorkspace([]byte(`not json`), "ws-1")
	assert.Error(t, err, "non-object bodies are an explicit error, not a silent skip")
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

// --- #1412: workflow-firing triggers through the automation surface ------

// Happy path: workflowId present → no workspace stamp (the user API
// rejects both), trigger stored linked to the workflow.
func TestPodAutomation_WorkflowTriggerCreate(t *testing.T) {
	r, trigStore, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	target := "ws-1"
	wfStore.workflows["wf-1"] = &wf.WorkflowRow{ID: "wf-1", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &target}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"name":"dag-hook","sourceType":"cron",
		"sourceConfig":{"expr":"0 4 * * *"},
		"workflowId":"wf-1"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	require.Len(t, trigStore.triggers, 1)
	for _, row := range trigStore.triggers {
		require.NotNil(t, row.WorkflowID, "linked to the workflow")
		assert.Equal(t, "wf-1", *row.WorkflowID)
		assert.Nil(t, row.WorkspaceID, "routine workspace NOT stamped on DAG triggers")
	}
}

// The snake_case alias must not silently vanish (#1412 defect 1).
func TestPodAutomation_WorkflowTriggerSnakeAlias(t *testing.T) {
	r, trigStore, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	target := "ws-1"
	wfStore.workflows["wf-1"] = &wf.WorkflowRow{ID: "wf-1", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &target}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1",
		"name":"dag-alias","sourceType":"cron",
		"sourceConfig":{"expr":"0 4 * * *"},
		"workflow_id":"wf-1"
	}`)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	for _, row := range trigStore.triggers {
		require.NotNil(t, row.WorkflowID, "alias normalized onto the DTO field")
	}
}

// Scoping: a workflow targeting ANOTHER workspace is refused — the pod
// cannot schedule runs outside its own workspace.
func TestPodAutomation_WorkflowTriggerWrongWorkspace(t *testing.T) {
	r, _, wfStore := newAutomationRouter(t, automationReviewer(), automationLookup())
	other := "ws-OTHER"
	wfStore.workflows["wf-x"] = &wf.WorkflowRow{ID: "wf-x", OwnerType: types.WorkflowOwnerUser, OwnerID: "user-7", TargetWorkspaceID: &other}

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1","name":"dag-x","sourceType":"cron",
		"sourceConfig":{"expr":"0 4 * * *"},"workflowId":"wf-x"
	}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "different workspace")
}

// Unknown workflow target: named 400, not a silent create.
func TestPodAutomation_WorkflowTriggerMissingWorkflow(t *testing.T) {
	r, _, _ := newAutomationRouter(t, automationReviewer(), automationLookup())
	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{
		"workspaceID":"ws-1","name":"dag-missing","sourceType":"cron",
		"sourceConfig":{"expr":"0 4 * * *"},"workflowId":"wf-nope"
	}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "workflow not found")
}
