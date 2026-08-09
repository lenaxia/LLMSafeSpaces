// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Epic 64: Workflow node execution endpoint for agentd.
//
// POST /v1/workflow/node/execute — dispatches a single workflow node
// (script/agent/http/condition) and returns its output. Called by the
// API server's workflow engine (US-64.8).
//
// POST /v1/workflow/node/cancel — kills an in-flight node execution.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/expr-lang/expr"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/workflows/scriptwrap"
)

type workflowExecuteRequest struct {
	NodeID   string          `json:"nodeId"`
	NodeType string          `json:"nodeType"`
	Spec     json.RawMessage `json:"spec"`
	Input    json.RawMessage `json:"input"`
	Timeout  string          `json:"timeout,omitempty"`
}

type workflowExecuteResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
	Branch string          `json:"branch,omitempty"`
}

type workflowExecuteError struct {
	ErrorCode string `json:"errorCode"`
	Detail    string `json:"detail"`
}

type inFlightExec struct {
	cancel context.CancelFunc
}

type nodeExecRegistry struct {
	execs map[string]*inFlightExec
}

func newNodeExecRegistry() *nodeExecRegistry {
	return &nodeExecRegistry{execs: make(map[string]*inFlightExec)}
}

func (r *nodeExecRegistry) start(nodeID string, cancel context.CancelFunc) {
	if nodeID == "" {
		return
	}
	r.execs[nodeID] = &inFlightExec{cancel: cancel}
}

func (r *nodeExecRegistry) stop(nodeID string) {
	delete(r.execs, nodeID)
}

func (r *nodeExecRegistry) cancelNode(nodeID string) {
	if e, ok := r.execs[nodeID]; ok {
		e.cancel()
	}
}

var registry = newNodeExecRegistry()

func workflowExecuteHandler(password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req workflowExecuteRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
			writeWorkflowError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("cannot decode request: %v", err))
			return
		}

		timeout := 10 * time.Minute
		if req.Timeout != "" {
			if d, err := time.ParseDuration(req.Timeout); err == nil && d > 0 {
				timeout = d
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		registry.start(req.NodeID, cancel)
		defer registry.stop(req.NodeID)

		switch req.NodeType {
		case "script":
			execScriptNode(ctx, w, &req)
		case "http":
			execHTTPNode(ctx, w, &req)
		case "condition":
			execConditionNode(ctx, w, &req)
		case "agent":
			execAgentNode(ctx, password, w, &req)
		default:
			writeWorkflowError(w, http.StatusBadRequest, "invalid_node_type", fmt.Sprintf("unsupported node type: %s", req.NodeType))
		}
	}
}

func workflowCancelHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.URL.Query().Get("nodeId")
		if nodeID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_node_id", "nodeId query parameter required")
			return
		}
		registry.cancelNode(nodeID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func workflowDeleteSessionHandler(password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+basicAuth(password) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_session_id", "sessionId query parameter required")
			return
		}
		deleteOpencodeSession(r.Context(), password, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}

func basicAuth(password string) string {
	return base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername + ":" + password))
}

func execScriptNode(ctx context.Context, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.ScriptNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse script spec: %v", err))
		return
	}

	var input map[string]any
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &input); err != nil {
			writeWorkflowError(w, http.StatusBadRequest, "invalid_input", fmt.Sprintf("cannot parse input: %v", err))
			return
		}
	}

	lang := scriptwrap.Language(data.Language)
	output, stderr, exitCode, err := scriptwrap.Execute(ctx, lang, data.Handler, input)
	if err != nil {
		if ctx.Err() != nil {
			writeWorkflowError(w, http.StatusGatewayTimeout, "script_timeout", "script execution timed out or was canceled")
			return
		}
		if exitCode != 0 {
			writeWorkflowError(w, http.StatusOK, "script_failed", fmt.Sprintf("exit %d: %s", exitCode, stderr))
			return
		}
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		writeWorkflowError(w, http.StatusOK, "script_output_invalid", fmt.Sprintf("handler returned non-object JSON: %s", string(output)))
		return
	}

	writeWorkflowSuccess(w, result)
}

func execHTTPNode(ctx context.Context, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.HTTPNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse http spec: %v", err))
		return
	}

	if data.Timeout != "" {
		if d, err := time.ParseDuration(data.Timeout); err == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}

	method := data.Method
	if method == "" {
		method = "GET"
	}

	var bodyReader io.Reader
	if data.Body != "" {
		bodyReader = strings.NewReader(data.Body)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, data.URL, bodyReader)
	if err != nil {
		writeWorkflowError(w, http.StatusOK, "script_failed", fmt.Sprintf("cannot create request: %v", err))
		return
	}

	secrets, _ := loadSecretsEnv()
	for k, v := range data.Headers {
		httpReq.Header.Set(k, resolveSecretRef(v, secrets))
	}

	start := time.Now()
	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			writeWorkflowError(w, http.StatusGatewayTimeout, "http_timeout", "HTTP request timed out")
			return
		}
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	headersMap := make(map[string]string)
	for k := range resp.Header {
		headersMap[k] = resp.Header.Get(k)
	}

	writeWorkflowSuccess(w, map[string]any{
		"status":      resp.StatusCode,
		"headers":     headersMap,
		"body":        string(respBody),
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func execConditionNode(_ context.Context, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.ConditionNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse condition spec: %v", err))
		return
	}

	var input map[string]any
	if len(req.Input) > 0 {
		_ = json.Unmarshal(req.Input, &input)
	}
	if input == nil {
		input = map[string]any{}
	}

	for _, c := range data.Conditions {
		env := map[string]any{"input": input}
		program, err := expr.Compile(c.Expression, expr.Env(env), expr.AsBool())
		if err != nil {
			continue
		}
		result, err := expr.Run(program, env)
		if err != nil {
			continue
		}
		if b, ok := result.(bool); ok && b {
			writeWorkflowSuccess(w, map[string]any{}, c.ID)
			return
		}
	}
	writeWorkflowSuccess(w, map[string]any{}, "otherwise")
}

func execAgentNode(ctx context.Context, password string, w http.ResponseWriter, req *workflowExecuteRequest) {
	var data wf.AgentNodeData
	if err := json.Unmarshal(req.Spec, &data); err != nil {
		writeWorkflowError(w, http.StatusBadRequest, "invalid_node_data", fmt.Sprintf("cannot parse agent spec: %v", err))
		return
	}

	prompt := data.Prompt
	if len(req.Input) > 0 {
		var input map[string]any
		_ = json.Unmarshal(req.Input, &input)
		for k, v := range input {
			prompt = strings.ReplaceAll(prompt, "{{."+k+"}}", fmt.Sprintf("%v", v))
		}
	}

	sessionMode := data.Session
	if sessionMode == "" {
		sessionMode = "ephemeral"
	}

	createdEphemeral := false
	sessionID := data.SessionID
	if sessionID == "" {
		sessionID = createOpencodeSession(ctx, password)
		if sessionID == "" {
			writeWorkflowError(w, http.StatusOK, "session_not_found", "failed to create ephemeral session")
			return
		}
		createdEphemeral = true
	}

	body := fmt.Sprintf(`{"agentID":%q,"parts":[{"type":"text","text":%q}]}`, data.Agent, prompt)
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://127.0.0.1:%d/session/%s/message", agentd.AgentPort, sessionID),
		strings.NewReader(body))
	if err != nil {
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}
	httpReq.SetBasicAuth(agentd.AuthUsername, password)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			writeWorkflowError(w, http.StatusGatewayTimeout, "script_timeout", "agent call timed out")
			return
		}
		writeWorkflowError(w, http.StatusOK, "script_failed", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		writeWorkflowError(w, http.StatusOK, "session_not_found", fmt.Sprintf("session %s not found", sessionID))
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeWorkflowError(w, http.StatusOK, "script_failed", fmt.Sprintf("opencode returned %d", resp.StatusCode))
		return
	}

	var msgResp struct {
		Info struct {
			ID     string `json:"id"`
			Agent  string `json:"agent"`
			Tokens struct {
				Input  int `json:"input"`
				Output int `json:"output"`
				Total  int `json:"total"`
			} `json:"tokens"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msgResp); err != nil {
		writeWorkflowError(w, http.StatusOK, "script_output_invalid", fmt.Sprintf("cannot parse opencode response: %v", err))
		return
	}

	var texts []string
	for _, p := range msgResp.Parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}

	result := map[string]any{
		"response":   strings.Join(texts, "\n"),
		"session_id": sessionID,
		"tokens":     msgResp.Info.Tokens,
		"prompt":     prompt,
		"parts":      msgResp.Parts,
	}

	if data.EnforceStructuredOutput && len(data.OutputSchema) > 0 {
		var parsed any
		if err := json.Unmarshal([]byte(strings.Join(texts, "")), &parsed); err != nil {
			writeWorkflowError(w, http.StatusOK, "schema_mismatch", fmt.Sprintf("agent output is not valid JSON: %v", err))
			return
		}
		result["response"] = parsed
	}

	if createdEphemeral && sessionMode == "ephemeral" {
		deleteOpencodeSession(ctx, password, sessionID)
		result["session_id"] = ""
		result["session_deleted"] = true
	}

	writeWorkflowSuccess(w, result)
}

func writeWorkflowSuccess(w http.ResponseWriter, output any, branch ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	out, _ := json.Marshal(output)
	resp := workflowExecuteResponse{Output: out}
	if len(branch) > 0 {
		resp.Branch = branch[0]
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func writeWorkflowError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(workflowExecuteError{ErrorCode: code, Detail: detail})
}

func loadSecretsEnv() (map[string]string, error) {
	data, err := os.ReadFile("/sandbox-runtime/secrets-env")
	if err != nil {
		return map[string]string{}, err
	}
	result := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		result[line[:idx]] = line[idx+1:]
	}
	return result, nil
}

func resolveSecretRef(s string, secrets map[string]string) string {
	if !strings.Contains(s, "{{secrets.") {
		return s
	}
	result := s
	for name, val := range secrets {
		result = strings.ReplaceAll(result, "{{secrets."+name+"}}", val)
	}
	return result
}

func createOpencodeSession(ctx context.Context, password string) string {
	req, _ := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://127.0.0.1:%d/session", agentd.AgentPort),
		strings.NewReader("{}"))
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var s struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return ""
	}
	return s.ID
}

func deleteOpencodeSession(ctx context.Context, password, sessionID string) {
	req, _ := http.NewRequestWithContext(ctx, "DELETE",
		fmt.Sprintf("http://127.0.0.1:%d/session/%s", agentd.AgentPort, sessionID), nil)
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
