package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockWorkflowStore implements workflowStore for testing.
type mockWorkflowStore struct {
	workflows    map[string]*wf.WorkflowRow
	lastCreated  *wf.WorkflowRow
	createErr    error
	statuses     map[string]string
	runStatuses  map[string]string
	getRunErr    error
	updateRunErr error
}

func newMockWorkflowStore() *mockWorkflowStore {
	return &mockWorkflowStore{
		workflows:   make(map[string]*wf.WorkflowRow),
		statuses:    make(map[string]string),
		runStatuses: make(map[string]string),
	}
}

func (m *mockWorkflowStore) preSetStatus(runID, status string) {
	m.runStatuses[runID] = status
}

func (m *mockWorkflowStore) CreateWorkflow(_ context.Context, row *wf.WorkflowRow) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.workflows[row.ID] = row
	m.lastCreated = row
	return nil
}

func (m *mockWorkflowStore) ListWorkflows(_ context.Context, ownerType, ownerID string) ([]*wf.WorkflowRow, error) {
	var out []*wf.WorkflowRow
	for _, r := range m.workflows {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mockWorkflowStore) GetWorkflow(_ context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	return r, nil
}

func (m *mockWorkflowStore) UpdateWorkflow(_ context.Context, ownerType, ownerID, workflowID string, upd *wf.WorkflowUpdate) (*wf.WorkflowRow, error) {
	r, ok := m.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return nil, wf.ErrNotFound
	}
	if upd.Name != nil {
		r.Name = *upd.Name
	}
	if upd.Status != nil {
		r.Status = *upd.Status
	}
	if upd.Description != nil {
		r.Description = *upd.Description
	}
	if upd.SpecJSON != nil {
		r.SpecJSON = upd.SpecJSON
	}
	if upd.SpecYAML != nil {
		r.SpecYAML = *upd.SpecYAML
	}
	if upd.TargetWorkspaceID != nil {
		if *upd.TargetWorkspaceID == "" {
			r.TargetWorkspaceID = nil
		} else {
			wsID := *upd.TargetWorkspaceID
			r.TargetWorkspaceID = &wsID
		}
	}
	if upd.OnMissingWorkspace != nil {
		r.OnMissingWorkspace = *upd.OnMissingWorkspace
	}
	return r, nil
}

func (m *mockWorkflowStore) DeleteWorkflow(_ context.Context, ownerType, ownerID, workflowID string) error {
	r, ok := m.workflows[workflowID]
	if !ok || r.OwnerType != ownerType || r.OwnerID != ownerID {
		return wf.ErrNotFound
	}
	delete(m.workflows, workflowID)
	return nil
}

func (m *mockWorkflowStore) CountWorkflowsByOwner(_ context.Context, ownerType, ownerID string) (int, error) {
	count := 0
	for _, r := range m.workflows {
		if r.OwnerType == ownerType && r.OwnerID == ownerID {
			count++
		}
	}
	return count, nil
}

func (m *mockWorkflowStore) CreateWorkflowRun(_ context.Context, row *wf.WorkflowRunRow) error {
	m.workflows[row.ID] = &wf.WorkflowRow{ID: row.ID}
	return nil
}

func (m *mockWorkflowStore) GetWorkflowRun(_ context.Context, runID string) (*wf.WorkflowRunRow, error) {
	if m.getRunErr != nil {
		return nil, m.getRunErr
	}
	status := "queued"
	if s, ok := m.runStatuses[runID]; ok {
		status = s
	}
	return &wf.WorkflowRunRow{ID: runID, Status: status}, nil
}

func (m *mockWorkflowStore) UpdateWorkflowRunStatus(_ context.Context, runID, status string, _ *string, _ json.RawMessage, _ json.RawMessage) error {
	if m.updateRunErr != nil {
		return m.updateRunErr
	}
	m.statuses[runID] = status
	return nil
}

func (m *mockWorkflowStore) ListWorkflowRuns(_ context.Context, _ string, _, _ int) ([]*wf.WorkflowRunRow, error) {
	return nil, nil
}

func (m *mockWorkflowStore) ListNodeRuns(_ context.Context, _ string) ([]*wf.WorkflowNodeRunRow, error) {
	return nil, nil
}

func (m *mockWorkflowStore) ListWorkflowRunsByWorkspace(_ context.Context, _ string) ([]*wf.WorkflowRunRow, error) {
	return nil, nil
}

func (m *mockWorkflowStore) ListSessionOrigins(_ context.Context, _ string) ([]*wf.SessionOriginRow, error) {
	return []*wf.SessionOriginRow{
		{SessionID: "ses_1", WorkspaceID: "ws_1", Origin: "routine", Title: "Nightly check"},
	}, nil
}

func TestWorkflowListSessionOrigins(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	h := NewUserWorkflowsHandler(store, quota)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws_1"}}
	h.ListSessionOrigins(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	origins := resp["origins"].([]any)
	require.Len(t, origins, 1)
	first := origins[0].(map[string]any)
	assert.Equal(t, "ses_1", first["sessionId"])
	assert.Equal(t, "routine", first["origin"])
}

func TestWorkflowListActiveRunsByWorkspace(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	h := NewUserWorkflowsHandler(store, quota)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws_1"}}
	h.ListActiveRunsByWorkspace(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	runs, ok := resp["runs"].([]any)
	assert.True(t, ok)
	assert.Empty(t, runs)
}

func TestWorkflowListSessionOrigins_MissingWorkspaceID(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	h := NewUserWorkflowsHandler(store, quota)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.ListSessionOrigins(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// mockQuotaChecker implements workflowQuotaChecker.
type mockQuotaChecker struct {
	values map[string]int
}

func (m *mockQuotaChecker) GetInt(_ context.Context, key string) (int, error) {
	return m.values[key], nil
}

func setupWorkflowRouter(t *testing.T, store workflowStore, quota workflowQuotaChecker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewUserWorkflowsHandler(store, quota)
	group := r.Group("/api/v1/me/workflows")
	group.Use(func(c *gin.Context) { c.Set("userID", "test-user"); c.Next() })
	group.GET("", h.UserList)
	group.POST("", h.UserCreate)
	group.GET("/:id", h.UserGet)
	group.PUT("/:id", h.UserUpdate)
	group.DELETE("/:id", h.UserDelete)

	runs := r.Group("/api/v1/me/runs")
	runs.Use(func(c *gin.Context) { c.Set("userID", "test-user"); c.Next() })
	runs.GET("/:runId", h.GetRun)
	runs.POST("/:runId/cancel", h.CancelRun)
	return r
}

func doWFRequest(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWorkflowCreate_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{"workflows.maxPerUser": 50}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"def handler(i): return {}"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "my-workflow",
		"specYaml": validSpec,
	})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "my-workflow", resp["name"])
	assert.Equal(t, "my-workflow", resp["slug"])
	assert.Equal(t, "draft", resp["status"])
}

func TestWorkflowCreate_InvalidSpec(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	// Spec with a cycle.
	cyclicSpec := `{"nodes":[{"id":"a","type":"script","data":{"language":"python","handler":"x"}},{"id":"b","type":"script","data":{"language":"python","handler":"x"}}],"edges":[{"source":"a","target":"b"},{"source":"b","target":"a"}]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "bad-workflow",
		"specYaml": cyclicSpec,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["error"], "validation failed")
}

func TestWorkflowCreate_InvalidName(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "",
		"specYaml": `{}`,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowCreate_QuotaExceeded(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{"workflows.maxPerUser": 0}} // 0 = unlimited

	// Create one workflow, then set quota to 1.
	quota.values["workflows.maxPerUser"] = 1
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	// First succeeds (count 0 < 1).
	w1 := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "first", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w1.Code)

	// Second fails (count 1 >= 1).
	w2 := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "second", "specYaml": validSpec})
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestWorkflowGet_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	// Create first.
	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`
	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Get it back.
	w2 := doWFRequest(t, r, "GET", "/api/v1/me/workflows/"+workflowID, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var got map[string]any
	json.Unmarshal(w2.Body.Bytes(), &got)
	assert.Equal(t, "test", got["name"])
}

func TestWorkflowGet_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "GET", "/api/v1/me/workflows/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWorkflowList(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "wf-a", "specYaml": validSpec})
	doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "wf-b", "specYaml": validSpec})

	w := doWFRequest(t, r, "GET", "/api/v1/me/workflows", nil)
	require.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	workflows := resp["workflows"].([]any)
	assert.Len(t, workflows, 2)
}

func TestWorkflowUpdate_StatusOnly(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`
	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Update only status → name preserved.
	w2 := doWFRequest(t, r, "PUT", "/api/v1/me/workflows/"+workflowID, map[string]any{
		"status": "active",
	})
	require.Equal(t, http.StatusOK, w2.Code)

	var updated map[string]any
	json.Unmarshal(w2.Body.Bytes(), &updated)
	assert.Equal(t, "active", updated["status"])
	assert.Equal(t, "test", updated["name"])
}

func TestWorkflowUpdate_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "PUT", "/api/v1/me/workflows/nonexistent", map[string]any{
		"status": "active",
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWorkflowDelete_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`
	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Delete.
	w2 := doWFRequest(t, r, "DELETE", "/api/v1/me/workflows/"+workflowID, nil)
	assert.Equal(t, http.StatusOK, w2.Code)

	// Get → 404.
	w3 := doWFRequest(t, r, "GET", "/api/v1/me/workflows/"+workflowID, nil)
	assert.Equal(t, http.StatusNotFound, w3.Code)
}

func TestWorkflowDelete_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "DELETE", "/api/v1/me/workflows/nonexistent", nil)
	assert.Equal(t, 404, w.Code)
}

func TestWorkflowCancelRun_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "POST", "/api/v1/me/runs/run-1/cancel", nil)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "canceled", store.statuses["run-1"])
}

func TestWorkflowCancelRun_AlreadyTerminal(t *testing.T) {
	store := newMockWorkflowStore()
	store.preSetStatus("run-2", "succeeded")
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "POST", "/api/v1/me/runs/run-2/cancel", nil)
	assert.Equal(t, 409, w.Code)
}

func TestWorkflowCancelRun_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	store.getRunErr = wf.ErrNotFound
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "POST", "/api/v1/me/runs/nonexistent/cancel", nil)
	assert.Equal(t, 404, w.Code)
}

func TestWorkflowCancelRun_StoreError(t *testing.T) {
	store := newMockWorkflowStore()
	store.updateRunErr = errors.New("DB down")
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "POST", "/api/v1/me/runs/run-1/cancel", nil)
	assert.Equal(t, 500, w.Code)
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"My Workflow", "my-workflow"},
		{"process_meetings", "process-meetings"},
		{"  Trimmed  ", "trimmed"},
		{"---edges---", "edges"},
		{"", "workflow"},
		{"UPPER", "upper"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, slugify(tt.input))
		})
	}
}

func TestWorkflowCreate_SlugProvided(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "My Workflow Name",
		"slug":     "custom-slug",
		"specYaml": validSpec,
	})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, "custom-slug", resp["slug"])
}

func TestWorkflowCreate_StoreError(t *testing.T) {
	store := newMockWorkflowStore()
	store.createErr = errors.New("internal DB error")
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "test",
		"specYaml": validSpec,
	})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestWorkflowCreate_OnMissingWorkspace_Create(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{"workflows.maxPerUser": 50}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":               "auto-create-wf",
		"specYaml":           validSpec,
		"onMissingWorkspace": "create",
	})
	require.Equal(t, http.StatusCreated, w.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "create", resp["onMissingWorkspace"])
	assert.Equal(t, "create", store.lastCreated.OnMissingWorkspace)
}

func TestWorkflowCreate_OnMissingWorkspace_DefaultsToAbort(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{"workflows.maxPerUser": 50}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "abort-wf",
		"specYaml": validSpec,
	})
	require.Equal(t, http.StatusCreated, w.Code)

	assert.Equal(t, "abort", store.lastCreated.OnMissingWorkspace)
}

func TestWorkflowCreate_OnMissingWorkspace_InvalidValue(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{"workflows.maxPerUser": 50}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	w := doWFRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":               "bad-policy",
		"specYaml":           validSpec,
		"onMissingWorkspace": "skip",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowUpdate_OnMissingWorkspace(t *testing.T) {
	store := newMockWorkflowStore()
	store.workflows["wf-up"] = &wf.WorkflowRow{
		ID: "wf-up", OwnerType: "user", OwnerID: "test-user",
		Name: "test", Slug: "test", Status: "draft",
		OnMissingWorkspace: "abort",
		SpecYAML:           "{}", SpecJSON: json.RawMessage(`{}`),
	}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "PUT", "/api/v1/me/workflows/wf-up", map[string]any{
		"onMissingWorkspace": "create",
	})
	require.Equal(t, http.StatusOK, w.Code)

	updated := store.workflows["wf-up"]
	assert.Equal(t, "create", updated.OnMissingWorkspace)
}

func TestWorkflowUpdate_OnMissingWorkspace_Invalid(t *testing.T) {
	store := newMockWorkflowStore()
	store.workflows["wf-bad"] = &wf.WorkflowRow{
		ID: "wf-bad", OwnerType: "user", OwnerID: "test-user",
		Name: "test", Slug: "test", Status: "draft",
		OnMissingWorkspace: "abort",
		SpecYAML:           "{}", SpecJSON: json.RawMessage(`{}`),
	}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "PUT", "/api/v1/me/workflows/wf-bad", map[string]any{
		"onMissingWorkspace": "wait",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestWorkflowUpdate_TargetWorkspaceID(t *testing.T) {
	store := newMockWorkflowStore()
	store.workflows["wf-ws"] = &wf.WorkflowRow{
		ID: "wf-ws", OwnerType: "user", OwnerID: "test-user",
		Name: "test", Slug: "test", Status: "draft",
		SpecYAML: "{}", SpecJSON: json.RawMessage(`{}`),
	}
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doWFRequest(t, r, "PUT", "/api/v1/me/workflows/wf-ws", map[string]any{
		"targetWorkspaceId": "ws-target-1",
	})
	require.Equal(t, http.StatusOK, w.Code)

	updated := store.workflows["wf-ws"]
	require.NotNil(t, updated.TargetWorkspaceID)
	assert.Equal(t, "ws-target-1", *updated.TargetWorkspaceID)
}
