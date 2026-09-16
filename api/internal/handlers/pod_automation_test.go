package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func newAutomationTestRouter(t *testing.T, reviewer *fakeTokenReviewer, lookup *fakeBootstrapLookup) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	th := &TriggersHandler{}
	wh := &WorkflowsHandler{}
	// Delegate methods are hard to fake on concrete types; test the
	// resolve+delegate mechanics via the exported surface with a stub
	// downstream is not possible without interfaces — so this router
	// tests RESOLVE behavior (auth/scoping) and the delegation is pinned
	// at the wiring level (userID injection verified via the real
	// handlers' own contract: any 2xx means delegation ran).
	h := &PodAutomationHandler{tokenReviewer: reviewer, lookup: lookup, triggers: th, workflows: wh, expectedNamespace: testRenameNamespace}
	r.POST("/internal/v1/automation/triggers", h.TriggerCreate)
	r.GET("/internal/v1/automation/triggers", h.TriggerList)
	r.GET("/internal/v1/automation/triggers/:id", h.TriggerGet)
	r.DELETE("/internal/v1/automation/triggers/:id", h.TriggerDelete)
	return r
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
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	r := newAutomationTestRouter(t, &fakeTokenReviewer{}, lookup)

	w := doAutomation(t, r, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no token")

	reviewer := &fakeTokenReviewer{err: errTokenNotAuthenticated}
	r2 := newAutomationTestRouter(t, reviewer, lookup)
	w = doAutomation(t, r2, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code, "rejected token")

	reviewer = &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-OTHER"}
	r3 := newAutomationTestRouter(t, reviewer, lookup)
	w = doAutomation(t, r3, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "SA/workspace mismatch")

	reviewer = &fakeTokenReviewer{username: "system:serviceaccount:other-ns:workspace-ws-1"}
	r4 := newAutomationTestRouter(t, reviewer, lookup)
	w = doAutomation(t, r4, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "namespace mismatch")

	w = doAutomation(t, r4, "GET", "/internal/v1/automation/triggers", "tok", "")
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing workspaceID entirely")

	r5 := newAutomationTestRouter(t, &fakeTokenReviewer{err: assert.AnError}, lookup)
	w = doAutomation(t, r5, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "review transport failure")

	r6 := newAutomationTestRouter(t, &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}, &fakeBootstrapLookup{err: assert.AnError})
	w = doAutomation(t, r6, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "lookup failure")

	r7 := newAutomationTestRouter(t, &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}, &fakeBootstrapLookup{ws: nil})
	w = doAutomation(t, r7, "GET", "/internal/v1/automation/triggers?workspaceID=ws-1", "tok", "")
	assert.Equal(t, http.StatusNotFound, w.Code, "workspace not found")
}

// Body-path workspaceID: creates carry it in-body (the seam stamps it).
func TestPodAutomation_BodyWorkspaceID(t *testing.T) {
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	r := newAutomationTestRouter(t, reviewer, lookup)

	w := doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "auth passes; the 400 is the real TriggersHandler rejecting the minimal body — delegation ran")
	assert.NotEqual(t, http.StatusUnauthorized, w.Code)
	assert.NotEqual(t, http.StatusForbidden, w.Code)

	// Wrong in-body workspace: identity mismatch.
	w = doAutomation(t, r, "POST", "/internal/v1/automation/triggers", "tok", `{"workspaceID":"ws-OTHER"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestForceTriggerWorkspace(t *testing.T) {
	out := forceTriggerWorkspace([]byte(`{"name":"x","workspaceId":"ws-OTHER"}`), "ws-1")
	assert.Contains(t, string(out), `"workspaceId":"ws-1"`, "DTO spelling forced to this pod's workspace")
	assert.NotContains(t, string(out), "ws-OTHER")
}

func TestPodAutomation_LoggerWired(t *testing.T) {
	h := &PodAutomationHandler{tokenReviewer: &fakeTokenReviewer{}, lookup: &fakeBootstrapLookup{}, expectedNamespace: "ns"}
	assert.False(t, h.HasLogger())
	h.SetLogger(lmocks.NewMockLogger())
	assert.True(t, h.HasLogger())
}
