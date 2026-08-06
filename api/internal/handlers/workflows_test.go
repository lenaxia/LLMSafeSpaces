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
	workflows map[string]*wf.WorkflowRow
	createErr error
}

func newMockWorkflowStore() *mockWorkflowStore {
	return &mockWorkflowStore{workflows: make(map[string]*wf.WorkflowRow)}
}

func (m *mockWorkflowStore) CreateWorkflow(_ context.Context, row *wf.WorkflowRow) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.workflows[row.ID] = row
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
	return r
}

func doRequest(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
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

	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
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

	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
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

	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
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
	w1 := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "first", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w1.Code)

	// Second fails (count 1 >= 1).
	w2 := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "second", "specYaml": validSpec})
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestWorkflowGet_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	// Create first.
	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`
	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Get it back.
	w2 := doRequest(t, r, "GET", "/api/v1/me/workflows/"+workflowID, nil)
	require.Equal(t, http.StatusOK, w2.Code)

	var got map[string]any
	json.Unmarshal(w2.Body.Bytes(), &got)
	assert.Equal(t, "test", got["name"])
}

func TestWorkflowGet_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doRequest(t, r, "GET", "/api/v1/me/workflows/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWorkflowList(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`

	doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "wf-a", "specYaml": validSpec})
	doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "wf-b", "specYaml": validSpec})

	w := doRequest(t, r, "GET", "/api/v1/me/workflows", nil)
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
	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Update only status → name preserved.
	w2 := doRequest(t, r, "PUT", "/api/v1/me/workflows/"+workflowID, map[string]any{
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

	w := doRequest(t, r, "PUT", "/api/v1/me/workflows/nonexistent", map[string]any{
		"status": "active",
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestWorkflowDelete_Success(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	validSpec := `{"nodes":[{"id":"start","type":"script","data":{"language":"python","handler":"x"}}],"edges":[]}`
	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{"name": "test", "specYaml": validSpec})
	require.Equal(t, http.StatusCreated, w.Code)

	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	workflowID := created["id"].(string)

	// Delete.
	w2 := doRequest(t, r, "DELETE", "/api/v1/me/workflows/"+workflowID, nil)
	assert.Equal(t, http.StatusOK, w2.Code)

	// Get → 404.
	w3 := doRequest(t, r, "GET", "/api/v1/me/workflows/"+workflowID, nil)
	assert.Equal(t, http.StatusNotFound, w3.Code)
}

func TestWorkflowDelete_NotFound(t *testing.T) {
	store := newMockWorkflowStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	r := setupWorkflowRouter(t, store, quota)

	w := doRequest(t, r, "DELETE", "/api/v1/me/workflows/nonexistent", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
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

	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
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

	w := doRequest(t, r, "POST", "/api/v1/me/workflows", map[string]any{
		"name":     "test",
		"specYaml": validSpec,
	})
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
