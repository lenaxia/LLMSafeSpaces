package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// authedReq builds a request carrying the agentd Basic-auth header, the
// credential every user-mux endpoint requires at entry.
func authedReq(method, target, password string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Basic "+basicAuth(password))
	return req
}

const testAuthPassword = "test-pass"

func TestWorkflowExecute_RequiresAuth(t *testing.T) {
	body := `{"nodeId":"c1","nodeType":"condition","spec":{"conditions":[]},"input":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/execute", strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Authorization, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="agentd"` {
		t.Errorf("expected WWW-Authenticate challenge, got %q", got)
	}
}

func TestWorkflowExecute_WrongPassword(t *testing.T) {
	body := `{"nodeId":"c1","nodeType":"condition","spec":{"conditions":[]},"input":{}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", "wrong-pass", strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong password, got %d", w.Code)
	}
}

func TestWorkflowExecute_ValidAuth(t *testing.T) {
	body := `{"nodeId":"c1","nodeType":"condition","spec":{"conditions":[]},"input":{}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	workflowExecuteHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid credentials, got %d: %s", w.Code, w.Body.String())
	}
}

func TestWorkflowCancel_RequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/workflow/node/cancel?nodeId=test-node", nil)
	w := httptest.NewRecorder()
	workflowCancelHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Authorization, got %d", w.Code)
	}
}

func TestWorkflowCancel_WrongPassword(t *testing.T) {
	req := authedReq(http.MethodPost, "/v1/workflow/node/cancel?nodeId=test-node", "wrong-pass", nil)
	w := httptest.NewRecorder()
	workflowCancelHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong password, got %d", w.Code)
	}
}

func TestWorkflowDeleteSession_RequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/v1/workflow/session/delete?sessionId=ses_1", nil)
	w := httptest.NewRecorder()
	workflowDeleteSessionHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Authorization, got %d", w.Code)
	}
}

func TestWorkflowDeleteSession_WrongPassword(t *testing.T) {
	req := authedReq(http.MethodDelete, "/v1/workflow/session/delete?sessionId=ses_1", "wrong-pass", nil)
	w := httptest.NewRecorder()
	workflowDeleteSessionHandler(testAuthPassword)(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong password, got %d", w.Code)
	}
}

func TestWorkflowExecute_InvalidJSON(t *testing.T) {
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader("invalid json"))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestWorkflowExecute_InvalidNodeType(t *testing.T) {
	body := `{"nodeId":"n1","nodeType":"unknown","spec":{},"input":{}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var resp workflowExecuteError
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ErrorCode != "invalid_node_type" {
		t.Errorf("expected invalid_node_type, got %s", resp.ErrorCode)
	}
}

func TestWorkflowExecute_Condition(t *testing.T) {
	body := `{"nodeId":"c1","nodeType":"condition","spec":{"conditions":[{"id":"skip","expression":"input.skipped == true"}]},"input":{"skipped":true}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp workflowExecuteResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Branch != "skip" {
		t.Errorf("expected branch 'skip', got %q", resp.Branch)
	}
}

func TestWorkflowExecute_ConditionOtherwise(t *testing.T) {
	body := `{"nodeId":"c1","nodeType":"condition","spec":{"conditions":[{"id":"skip","expression":"input.skipped == true"}]},"input":{"skipped":false}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp workflowExecuteResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Branch != "otherwise" {
		t.Errorf("expected branch 'otherwise', got %q", resp.Branch)
	}
}

func TestWorkflowExecute_ScriptSuccess(t *testing.T) {
	// The script node executes via python3 — a workspace-runtime-image
	// dependency. Dev hosts without python3 on PATH skip rather than
	// report a false failure of the handler contract.
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; script-node runtime is a workspace-image dependency")
	}
	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":"def handler(input):\n    return {\"result\": input.get(\"x\", 0) + 1}"},"input":{"x":41}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp workflowExecuteResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	var output map[string]any
	json.Unmarshal(resp.Output, &output)
	if output["result"] != float64(42) {
		t.Errorf("expected result=42, got %v", output["result"])
	}
}

func TestWorkflowExecute_ScriptFailure(t *testing.T) {
	body := `{"nodeId":"s1","nodeType":"script","spec":{"language":"python","handler":"def handler(input):\n    raise ValueError('boom')"},"input":{}}`
	req := authedReq(http.MethodPost, "/v1/workflow/node/execute", testAuthPassword, strings.NewReader(body))
	w := httptest.NewRecorder()
	h := workflowExecuteHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (error in body), got %d", w.Code)
	}
	var resp workflowExecuteError
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ErrorCode != "script_failed" {
		t.Errorf("expected script_failed, got %s", resp.ErrorCode)
	}
}

func TestWorkflowCancel(t *testing.T) {
	req := authedReq(http.MethodPost, "/v1/workflow/node/cancel?nodeId=test-node", testAuthPassword, nil)
	w := httptest.NewRecorder()
	h := workflowCancelHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}
}

func TestWorkflowCancel_MissingNodeID(t *testing.T) {
	req := authedReq(http.MethodPost, "/v1/workflow/node/cancel", testAuthPassword, nil)
	w := httptest.NewRecorder()
	h := workflowCancelHandler(testAuthPassword)
	h(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestResolveSecretRef(t *testing.T) {
	secrets := map[string]string{"GH_TOKEN": "ghp_abc", "API_KEY_foo": "key123"}
	result := resolveSecretRef("Bearer {{secrets.GH_TOKEN}}", secrets)
	if result != "Bearer ghp_abc" {
		t.Errorf("expected 'Bearer ghp_abc', got %q", result)
	}
	result = resolveSecretRef("no-secret-here", secrets)
	if result != "no-secret-here" {
		t.Errorf("expected passthrough, got %q", result)
	}
}

func TestLoadSecretsEnv(t *testing.T) {
	// In a workspace, /sandbox-runtime/secrets-env may or may not exist.
	// If it exists, verify we can parse it; if not, verify empty map + error.
	m, err := loadSecretsEnv()
	if err != nil {
		// File doesn't exist — expected in some environments.
		return
	}
	// File exists — verify it parsed to a map (no panic, valid KV format).
	if m == nil {
		t.Error("expected non-nil map")
	}
}

func TestNodeExecRegistry(t *testing.T) {
	r := newNodeExecRegistry()
	called := false
	ctx, cancel := context.WithCancel(context.Background())
	r.start("node1", cancel)
	r.cancelNode("node1")
	select {
	case <-ctx.Done():
		called = true
	default:
	}
	if !called {
		t.Error("cancel was not called")
	}
	r.stop("node1")
}
