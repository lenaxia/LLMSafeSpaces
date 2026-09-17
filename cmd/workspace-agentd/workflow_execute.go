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
	"crypto/sha256"
	"encoding/hex"
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
	// WorkflowID/RunID identify the logical execution this dispatch
	// belongs to (#1327): the run's workflow/run IDs on the workflow
	// path, the routine's trigger/fire IDs on the routine path. The
	// agent node derives its harness dedupe key from them. Absent (an
	// older API server), the harness POST stays keyless exactly as
	// before — deriving from partial identity would hand every
	// identity-less dispatch the same key and collapse distinct
	// executions into one transcript message.
	WorkflowID string `json:"workflowId,omitempty"`
	RunID      string `json:"runId,omitempty"`
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

// Design 0051 US-3 (§D1): control-plane route. workspacePassword is BOTH
// an accepted credential and the CLIENT credential for agent-node
// opencode session calls (the sidecar retains it as a client secret by
// design). extraAuth carries additional ACCEPTED credentials (the
// agentdPassword in sidecar mode; D6.1 mixed-generation window).
func workflowExecuteHandler(workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, append([]string{workspacePassword}, extraAuth...)...) {
			rejectUnauthorized(w)
			return
		}
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
			execAgentNode(ctx, workspacePassword, w, &req)
		default:
			writeWorkflowError(w, http.StatusBadRequest, "invalid_node_type", fmt.Sprintf("unsupported node type: %s", req.NodeType))
		}
	}
}

// Design 0051 US-3 (§D1): control-plane route — see workflowExecuteHandler.
func workflowCancelHandler(passwords ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, passwords...) {
			rejectUnauthorized(w)
			return
		}
		nodeID := r.URL.Query().Get("nodeId")
		if nodeID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_node_id", "nodeId query parameter required")
			return
		}
		registry.cancelNode(nodeID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// Design 0051 US-3 (§D1): control-plane route — see workflowExecuteHandler.
func workflowDeleteSessionHandler(workspacePassword string, extraAuth ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, append([]string{workspacePassword}, extraAuth...)...) {
			rejectUnauthorized(w)
			return
		}
		sessionID := r.URL.Query().Get("sessionId")
		if sessionID == "" {
			writeWorkflowError(w, http.StatusBadRequest, "missing_session_id", "sessionId query parameter required")
			return
		}
		deleteOpencodeSession(r.Context(), workspacePassword, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
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
		// exitCode is the real process exit only when a process ran;
		// scriptwrap's sentinel -1 marks pre-execution failures whose
		// detail lives in err (e.g. "unsupported language: bash").
		// Reporting "exit -1: <empty stderr>" dropped exactly that
		// detail (#1414).
		if exitCode > 0 {
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

	// #1327: the agent POST carries the execution-derived dedupe key.
	// The pinned harness (opencode 1.18.15, G1 probe on PR #1323)
	// validates the msg_-prefix, uses a repeated messageID verbatim as
	// the user message's store ID, and UPSERTS on collision — so a key
	// stable across the retry/re-drive of the SAME logical node
	// execution bounds the session transcript to one message per
	// execution. Empty identity (older API servers) keeps today's
	// keyless wire body byte-for-byte.
	dedupeKey := workflowAgentMessageKey(req.WorkflowID, req.NodeID, req.RunID)
	body := fmt.Sprintf(`{"agentID":%q,"parts":[{"type":"text","text":%q}]}`, data.Agent, prompt)
	if dedupeKey != "" {
		body = fmt.Sprintf(`{"messageID":%q,"agentID":%q,"parts":[{"type":"text","text":%q}]}`, dedupeKey, data.Agent, prompt)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/message", getAgentAddr(), sessionID),
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

	msgResp, err := parseAgentNodeResponse(resp.Body)
	if err != nil {
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

// Key-shape constants for workflowAgentMessageKey (#1327). The prefix
// cap keeps the whole key within 64 chars (a common store-ID budget)
// while the per-component cap keeps one long component from evicting
// the others' readable share; the hash suffix carries uniqueness, so
// truncation only costs readability.
const (
	agentKeyComponentCap = 16
	agentKeyPrefixCap    = 55 // "msg_wf_" + components + separators; +1+"-"+8 hash → ≤64
)

// workflowAgentMessageKey derives the harness message dedupe key for a
// workflow agent-node execution: msg_wf_<workflow>_<node>_<run>-<hash8>
// (#1327). Retries/re-drives of the SAME logical node execution re-POST
// the same prompt; the harness upserts on a repeated msg_-prefixed
// messageID, so a key that is stable across the retry and distinct
// across executions bounds the session transcript to one message per
// logical execution. Any missing identity component returns "" — the
// caller then POSTs keyless (fail open): a partial-identity key would
// be shared by every identity-less dispatch and let distinct
// executions upsert each other's transcript messages.
//
// The hash suffix (sha256 over the RAW identity, unit-separator
// framed, first 4 bytes hex) keeps keys distinct where the readable
// prefix collides: node IDs are user-authored, and sanitization folds
// distinct raw IDs onto the same safe alphabet (a/b and a_b both
// sanitize to a_b — without the hash one node's POST could upsert
// another node's transcript message).
func workflowAgentMessageKey(workflowID, nodeID, runID string) string {
	if workflowID == "" || nodeID == "" || runID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(workflowID + "\x1f" + nodeID + "\x1f" + runID))
	prefix := fmt.Sprintf("msg_wf_%s_%s_%s",
		sanitizeKeyComponent(workflowID),
		sanitizeKeyComponent(nodeID),
		sanitizeKeyComponent(runID))
	if len(prefix) > agentKeyPrefixCap {
		prefix = prefix[:agentKeyPrefixCap]
	}
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

// sanitizeKeyComponent folds a key component onto the harness-safe
// alphabet [A-Za-z0-9._-] and caps its length.
func sanitizeKeyComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > agentKeyComponentCap {
		out = out[:agentKeyComponentCap]
	}
	return out
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
		fmt.Sprintf("%s/session", getAgentAddr()),
		strings.NewReader("{}"))
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	id, err := parseCreatedSessionID(resp.Body)
	if err != nil {
		return ""
	}
	return id
}

// agentNodeMessage is the opencode V1 message wire shape the workflow
// agent node consumes (info + text parts — the same surface the seam's
// SessionSend parses).
type agentNodeMessage struct {
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

// parseAgentNodeResponse strictly decodes the message 200 (leg-10: a
// drifted body must fail the node, never emit phantom output).
func parseAgentNodeResponse(r io.Reader) (agentNodeMessage, error) {
	var m agentNodeMessage
	err := decodeStrict(r, &m)
	return m, err
}

// parseCreatedSessionID strictly decodes the POST /session 200 (leg-10:
// drift yields no session ID, never a phantom one to delete later).
func parseCreatedSessionID(r io.Reader) (string, error) {
	var s struct {
		ID string `json:"id"`
	}
	if err := decodeStrict(r, &s); err != nil {
		return "", err
	}
	return s.ID, nil
}

func deleteOpencodeSession(ctx context.Context, password, sessionID string) {
	req, err := http.NewRequestWithContext(ctx, "DELETE", //nolint:gosec // G704: local-only, sessionID from opencode
		fmt.Sprintf("%s/session/%s", getAgentAddr(), sessionID), nil)
	if err != nil {
		// Malformed sessionID (control chars) makes the URL unparseable;
		// req would be nil and SetBasicAuth would panic.
		return
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := (&http.Client{}).Do(req) //nolint:gosec // G704: local-only
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
