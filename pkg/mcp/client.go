// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcp implements the LLMSafeSpaces MCP server.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// maxResponseBody limits API response reads to 10MB to prevent OOM.
	maxResponseBody = 10 * 1024 * 1024
	// maxSSELineSize limits individual SSE event lines to 1MB.
	maxSSELineSize = 1 * 1024 * 1024
	// maxSSETotal limits total accumulated SSE content to 50MB.
	maxSSETotal = 50 * 1024 * 1024
	// requestTimeout is the default per-request timeout for non-SSE API calls.
	requestTimeout = 30 * time.Second
	// maxMessageSize limits the message body sent to session_message.
	maxMessageSize = 1 * 1024 * 1024 // 1MB
)

// validID matches safe identifiers (alphanumeric, hyphens, dots, underscores, max 253 chars).
// Underscores are required for opencode IDs (ses_abc, que_xyz, per_123).
var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-]{0,252}$`)

// validateID checks that an ID is safe to embed in a URL path.
func validateID(id, fieldName string) error {
	if id == "" {
		return fmt.Errorf("%s is required", fieldName)
	}
	if !validID.MatchString(id) {
		return fmt.Errorf("%s contains invalid characters", fieldName)
	}
	return nil
}

// APIClient defines the interface for calling the LLMSafeSpaces REST API.
// All operations are workspace-centric — the sandbox layer is internal.
type APIClient interface {
	CreateWorkspace(ctx context.Context, req CreateWorkspaceReq) (*WorkspaceResp, error)
	ActivateWorkspace(ctx context.Context, workspaceID string) (*ActivateResp, error)
	SuspendWorkspace(ctx context.Context, workspaceID string) error
	RefreshWorkspace(ctx context.Context, workspaceID string) (*RefreshWorkspaceResp, error)
	CreateSession(ctx context.Context, workspaceID string) (*SessionResp, error)
	GetHistory(ctx context.Context, workspaceID, sessionID string) ([]Message, error)
	SendMessage(ctx context.Context, workspaceID, sessionID, message string, timeout time.Duration) (string, error)

	// Agent input — question & permission replies (Epic 16 US-16.6)
	QuestionReply(ctx context.Context, workspaceID, requestID string, answers [][]string) error
	QuestionReject(ctx context.Context, workspaceID, requestID string) error
	PermissionReply(ctx context.Context, workspaceID, requestID, reply, message string) error

	// Credential management
	CreateCredential(ctx context.Context, req CreateCredentialReq) (*CredentialResp, error)
	ListCredentials(ctx context.Context) ([]CredentialResp, error)
	DeleteCredential(ctx context.Context, credentialID string) error
	BindCredential(ctx context.Context, workspaceID string, credentialIDs []string) error

	// Model management
	ListModels(ctx context.Context, workspaceID string) (json.RawMessage, error)
	SetModel(ctx context.Context, workspaceID, model string) error

	// Workflow management (Epic 64)
	ListWorkflows(ctx context.Context) (json.RawMessage, error)
	GetWorkflow(ctx context.Context, workflowID string) (json.RawMessage, error)
	CreateWorkflow(ctx context.Context, name, specYAML, status string) (json.RawMessage, error)
	UpdateWorkflow(ctx context.Context, workflowID, name, status, specYAML string) (json.RawMessage, error)
	RunWorkflow(ctx context.Context, workflowID, input, workspaceID string) (json.RawMessage, error)
	GetWorkflowRunStatus(ctx context.Context, runID string) (json.RawMessage, error)
	CancelWorkflowRun(ctx context.Context, runID string) error

	// Trigger management (Epic 64)
	ListTriggers(ctx context.Context) (json.RawMessage, error)
	CreateTrigger(ctx context.Context, name, sourceType, sourceConfig, workspaceID, workflowID, prompt, memoryMode, captureMode, preserveSession string) (json.RawMessage, error)
	UpdateTrigger(ctx context.Context, triggerID, enabled string) (json.RawMessage, error)
	DeleteTrigger(ctx context.Context, triggerID string) error
}

// CreateWorkspaceReq is the request body for workspace creation.
type CreateWorkspaceReq struct {
	Name    string `json:"name,omitempty"`
	Runtime string `json:"runtime"`
}

// WorkspaceResp is the response from workspace creation.
type WorkspaceResp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Phase   string `json:"phase"`
}

// ActivateResp is the response from workspace activation.
type ActivateResp struct {
	Resumed   string `json:"resumed"`
	Suspended string `json:"suspended,omitempty"`
}

// RefreshWorkspaceResp is the response from workspace compute refresh.
type RefreshWorkspaceResp struct {
	RestartGeneration int64 `json:"restartGeneration"`
}

// SessionResp is the response from session creation.
type SessionResp struct {
	ID string `json:"id"`
}

// Message represents a chat message in session history.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CreateCredentialReq is the request for creating an LLM provider credential.
//
// Epic 55 identity model: Kind is the SDK-class enum (openai, anthropic,
// openai_compatible, ...) that selects the adapter the agent loads; Slug is
// the per-owner unique identity that becomes the agent-config.json provider
// key. Both are required — the API's llm-provider value validator rejects a
// credential missing either.
type CreateCredentialReq struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Slug        string `json:"slug"`
	APIKey      string `json:"apiKey"`
	BaseURL     string `json:"baseURL,omitempty"`
	Default     string `json:"default,omitempty"`
	WorkspaceID string `json:"workspaceID,omitempty"` // if set, auto-binds after creation
}

// CredentialResp is the response from credential operations.
type CredentialResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// --- Request/response body types (US-46.9: typed request structs) ---

// questionEventResult is the structured result returned when a question event
// is detected during SSE streaming.
type questionEventResult struct {
	Type    string          `json:"type"`
	Request json.RawMessage `json:"request"`
}

// questionReplyRequest is the body for POST /question/:id/reply.
type questionReplyRequest struct {
	Answers [][]string `json:"answers"`
}

// permissionReplyRequest is the body for POST /permission/:id/reply.
type permissionReplyRequest struct {
	Reply   string `json:"reply"`
	Message string `json:"message,omitempty"`
}

// llmProviderValue is the JSON-encoded "value" field of an llm-provider
// secret. It mirrors the wire shape of secrets.LLMProviderData (Epic 55):
// kind + slug carry the credential identity; apiKey/baseURL/default configure
// it. The API server unmarshals this into LLMProviderData and validates that
// kind, slug, and apiKey are all present.
type llmProviderValue struct {
	Kind    string `json:"kind"`
	Slug    string `json:"slug"`
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseURL,omitempty"`
	Default string `json:"default,omitempty"`
}

// createSecretRequest is the body for POST /api/v1/secrets.
type createSecretRequest struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Value    string            `json:"value"`
	Metadata map[string]string `json:"metadata"`
}

// HTTPClient implements APIClient using HTTP calls to the LLMSafeSpaces API.
// It resolves workspace → sandbox internally for session/message operations.
type HTTPClient struct {
	BaseURL    string
	HTTPClient *http.Client
	APIKey     string
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path string, body any, result any) error {
	// Apply per-request timeout if context has no deadline
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Limit response body reads to prevent OOM
	limited := io.LimitReader(resp.Body, maxResponseBody)

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(limited)
		// Sanitize: truncate long error bodies to avoid leaking internal details
		errMsg := string(respBody)
		if len(errMsg) > 512 {
			errMsg = errMsg[:512] + "...(truncated)"
		}
		return fmt.Errorf("API error %d: %s", resp.StatusCode, errMsg)
	}

	if result != nil {
		if err := json.NewDecoder(limited).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// doRequestRaw executes an HTTP request and returns the status code + raw JSON body.
func (c *HTTPClient) doRequestRaw(ctx context.Context, method, path string, body any) (int, json.RawMessage, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxResponseBody)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		errMsg := string(raw)
		if len(errMsg) > 512 {
			errMsg = errMsg[:512] + "...(truncated)"
		}
		return resp.StatusCode, raw, fmt.Errorf("API error %d: %s", resp.StatusCode, errMsg)
	}

	return resp.StatusCode, json.RawMessage(raw), nil
}

func (c *HTTPClient) CreateWorkspace(ctx context.Context, req CreateWorkspaceReq) (*WorkspaceResp, error) {
	var resp WorkspaceResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) ActivateWorkspace(ctx context.Context, workspaceID string) (*ActivateResp, error) {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return nil, err
	}
	var resp ActivateResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/activate", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) SuspendWorkspace(ctx context.Context, workspaceID string) error {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/suspend", nil, nil)
}

func (c *HTTPClient) RefreshWorkspace(ctx context.Context, workspaceID string) (*RefreshWorkspaceResp, error) {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return nil, err
	}
	var resp RefreshWorkspaceResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/refresh-compute", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateSession resolves workspace → sandbox, then creates a session via the proxy.
func (c *HTTPClient) CreateSession(ctx context.Context, workspaceID string) (*SessionResp, error) {
	var resp SessionResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *HTTPClient) GetHistory(ctx context.Context, workspaceID, sessionID string) ([]Message, error) {
	if err := validateID(sessionID, "session_id"); err != nil {
		return nil, err
	}
	var msgs []Message
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/sessions/"+sessionID+"/message", nil, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// SendMessage sends a prompt via prompt_async, then subscribes to SSE events
// until session.idle is received or timeout expires.
func (c *HTTPClient) SendMessage(ctx context.Context, workspaceID, sessionID, message string, timeout time.Duration) (string, error) {
	if err := validateID(sessionID, "session_id"); err != nil {
		return "", err
	}
	if len(message) > maxMessageSize {
		return "", fmt.Errorf("message too large (%d bytes, max %d)", len(message), maxMessageSize)
	}

	// 1. Fire prompt_async
	body := map[string]string{"message": message}
	path := fmt.Sprintf("/api/v1/workspaces/%s/sessions/%s/prompt", workspaceID, sessionID)
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return "", err
	}

	// 2. Subscribe to SSE events and wait for session.idle
	sseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	eventsURL := fmt.Sprintf("%s/api/v1/workspaces/%s/events", c.BaseURL, workspaceID)
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return "", fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// SSE failed or timed out — fall back to polling history
		return c.fallbackHistory(ctx, workspaceID, sessionID)
	}
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var response strings.Builder
	for scanner.Scan() {
		// Guard against unbounded accumulation
		if response.Len() > maxSSETotal {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event struct {
				Type      string          `json:"type"`
				SessionID string          `json:"session_id"`
				Status    string          `json:"status"`
				EventType string          `json:"event_type"`
				Content   string          `json:"content"`
				Data      json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(data), &event) == nil {
				// Detect session idle: direct session.status event from broker
				if event.Type == "session.status" && event.Status == "idle" && event.SessionID == sessionID {
					break
				}

				// Detect question asked: agent is waiting for user input
				if event.Type == "agent.question" {
					// Verify it's for our session by checking the data
					var qData struct {
						SessionID string `json:"session_id"`
					}
					if json.Unmarshal(event.Data, &qData) == nil && qData.SessionID == sessionID {
						// Return structured question result
						result := questionEventResult{
							Type:    "question",
							Request: json.RawMessage(event.Data),
						}
						out, _ := json.Marshal(result)
						return string(out), nil
					}
				}

				// Detect permission asked: auto-approve in headless mode
				if event.Type == "agent.permission" {
					var pData struct {
						ID        string `json:"id"`
						SessionID string `json:"session_id"`
					}
					if json.Unmarshal(event.Data, &pData) == nil && pData.SessionID == sessionID {
						go func() { _ = c.PermissionReply(ctx, workspaceID, pData.ID, "always", "") }()
					}
				}

				if event.Content != "" {
					response.WriteString(event.Content)
				}
			}
		}
	}

	if response.Len() > 0 {
		return response.String(), nil
	}

	// Fallback: poll history (using parent context, not the timed-out SSE context)
	return c.fallbackHistory(ctx, workspaceID, sessionID)
}

func (c *HTTPClient) fallbackHistory(ctx context.Context, workspaceID, sessionID string) (string, error) {
	var msgs []Message
	histPath := fmt.Sprintf("/api/v1/workspaces/%s/sessions/%s/message", workspaceID, sessionID)
	if err := c.doJSON(ctx, http.MethodGet, histPath, nil, &msgs); err != nil {
		return "", fmt.Errorf("fallback history fetch: %w", err)
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content, nil
	}
	return "", nil
}

// validQuestionID matches opencode question request IDs.
var validQuestionID = regexp.MustCompile(`^que_[a-zA-Z0-9]+$`)

// validPermissionID matches opencode permission request IDs.
var validPermissionID = regexp.MustCompile(`^per_[a-zA-Z0-9_]+$`)

func (c *HTTPClient) QuestionReply(ctx context.Context, workspaceID, requestID string, answers [][]string) error {
	if !validQuestionID.MatchString(requestID) {
		return fmt.Errorf("invalid question request ID: %s", requestID)
	}
	body := questionReplyRequest{Answers: answers}
	path := fmt.Sprintf("/api/v1/workspaces/%s/question/%s/reply", workspaceID, requestID)
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

func (c *HTTPClient) QuestionReject(ctx context.Context, workspaceID, requestID string) error {
	if !validQuestionID.MatchString(requestID) {
		return fmt.Errorf("invalid question request ID: %s", requestID)
	}
	path := fmt.Sprintf("/api/v1/workspaces/%s/question/%s/reject", workspaceID, requestID)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

func (c *HTTPClient) PermissionReply(ctx context.Context, workspaceID, requestID, reply, message string) error {
	if !validPermissionID.MatchString(requestID) {
		return fmt.Errorf("invalid permission request ID: %s", requestID)
	}
	validReplies := map[string]bool{"once": true, "always": true, "reject": true}
	if !validReplies[reply] {
		return fmt.Errorf("reply must be 'once', 'always', or 'reject'")
	}
	body := permissionReplyRequest{Reply: reply, Message: message}
	path := fmt.Sprintf("/api/v1/workspaces/%s/permission/%s/reply", workspaceID, requestID)
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// --- Credential management ---

func (c *HTTPClient) CreateCredential(ctx context.Context, req CreateCredentialReq) (*CredentialResp, error) {
	value := llmProviderValue{
		Kind:    req.Kind,
		Slug:    req.Slug,
		APIKey:  req.APIKey,
		BaseURL: req.BaseURL,
		Default: req.Default,
	}
	valueJSON, _ := json.Marshal(value)

	name := req.Name
	if name == "" {
		name = req.Slug
	}

	body := createSecretRequest{
		Name:     name,
		Type:     "llm-provider",
		Value:    string(valueJSON),
		Metadata: map[string]string{},
	}

	var resp CredentialResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/secrets", body, &resp); err != nil {
		return nil, err
	}

	// Auto-bind if workspace specified.
	if req.WorkspaceID != "" {
		if err := c.BindCredential(ctx, req.WorkspaceID, []string{resp.ID}); err != nil {
			return &resp, fmt.Errorf("created credential %s but bind failed: %w", resp.ID, err)
		}
	}

	return &resp, nil
}

func (c *HTTPClient) ListCredentials(ctx context.Context) ([]CredentialResp, error) {
	// The API wraps the list: {"secrets": [...]} (SecretsHandler.
	// ListSecrets). Decoding a bare array failed on every call once the
	// wrapper landed — surfaced by the MCP canary (#880).
	var resp struct {
		Secrets []CredentialResp `json:"secrets"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/secrets", nil, &resp); err != nil {
		return nil, err
	}
	all := resp.Secrets
	// Filter to llm-provider type.
	var filtered []CredentialResp
	for _, cr := range all {
		if cr.Type == "llm-provider" {
			filtered = append(filtered, cr)
		}
	}
	return filtered, nil
}

func (c *HTTPClient) DeleteCredential(ctx context.Context, credentialID string) error {
	if err := validateID(credentialID, "credential_id"); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/secrets/"+credentialID, nil, nil)
}

func (c *HTTPClient) BindCredential(ctx context.Context, workspaceID string, credentialIDs []string) error {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return err
	}
	body := map[string][]string{"secretIds": credentialIDs}
	return c.doJSON(ctx, http.MethodPut, "/api/v1/workspaces/"+workspaceID+"/bindings", body, nil)
}

// --- Model management ---

func (c *HTTPClient) ListModels(ctx context.Context, workspaceID string) (json.RawMessage, error) {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/models", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *HTTPClient) SetModel(ctx context.Context, workspaceID, model string) error {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return err
	}
	body := map[string]string{"model": model}
	return c.doJSON(ctx, http.MethodPut, "/api/v1/workspaces/"+workspaceID+"/model", body, nil)
}

// --- Epic 64: Workflow & trigger API methods ---

func (c *HTTPClient) ListWorkflows(ctx context.Context) (json.RawMessage, error) {
	return c.doRaw(ctx, http.MethodGet, "/api/v1/me/workflows", nil)
}

func (c *HTTPClient) GetWorkflow(ctx context.Context, workflowID string) (json.RawMessage, error) {
	if err := validateID(workflowID, "workflow_id"); err != nil {
		return nil, err
	}
	return c.doRaw(ctx, http.MethodGet, "/api/v1/me/workflows/"+workflowID, nil)
}

func (c *HTTPClient) CreateWorkflow(ctx context.Context, name, specYAML, status string) (json.RawMessage, error) {
	body := map[string]string{"name": name, "specYaml": specYAML, "status": status}
	return c.doRaw(ctx, http.MethodPost, "/api/v1/me/workflows", body)
}

func (c *HTTPClient) UpdateWorkflow(ctx context.Context, workflowID, name, status, specYAML string) (json.RawMessage, error) {
	if err := validateID(workflowID, "workflow_id"); err != nil {
		return nil, err
	}
	body := map[string]string{"name": name, "status": status, "specYaml": specYAML}
	return c.doRaw(ctx, http.MethodPut, "/api/v1/me/workflows/"+workflowID, body)
}

func (c *HTTPClient) RunWorkflow(ctx context.Context, workflowID, input, workspaceID string) (json.RawMessage, error) {
	if err := validateID(workflowID, "workflow_id"); err != nil {
		return nil, err
	}
	body := map[string]string{"input": input, "workspaceId": workspaceID}
	return c.doRaw(ctx, http.MethodPost, "/api/v1/me/workflows/"+workflowID+"/runs", body)
}

func (c *HTTPClient) GetWorkflowRunStatus(ctx context.Context, runID string) (json.RawMessage, error) {
	if err := validateID(runID, "run_id"); err != nil {
		return nil, err
	}
	return c.doRaw(ctx, http.MethodGet, "/api/v1/me/runs/"+runID, nil)
}

func (c *HTTPClient) CancelWorkflowRun(ctx context.Context, runID string) error {
	if err := validateID(runID, "run_id"); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodPost, "/api/v1/me/runs/"+runID+"/cancel", nil, nil)
}

func (c *HTTPClient) ListTriggers(ctx context.Context) (json.RawMessage, error) {
	return c.doRaw(ctx, http.MethodGet, "/api/v1/me/triggers", nil)
}

func (c *HTTPClient) CreateTrigger(ctx context.Context, name, sourceType, sourceConfig, workspaceID, workflowID, prompt, memoryMode, captureMode, preserveSession string) (json.RawMessage, error) {
	body := map[string]any{
		"name": name, "sourceType": sourceType, "sourceConfig": json.RawMessage(sourceConfig),
	}
	if workspaceID != "" {
		body["workspaceId"] = workspaceID
	}
	if workflowID != "" {
		body["workflowId"] = workflowID
	}
	if prompt != "" {
		body["prompt"] = prompt
	}
	if memoryMode != "" {
		body["memoryMode"] = memoryMode
	}
	if captureMode != "" {
		body["captureMode"] = captureMode
	}
	if preserveSession != "" {
		body["preserveSession"] = preserveSession
	}
	return c.doRaw(ctx, http.MethodPost, "/api/v1/me/triggers", body)
}

func (c *HTTPClient) UpdateTrigger(ctx context.Context, triggerID, enabled string) (json.RawMessage, error) {
	if err := validateID(triggerID, "trigger_id"); err != nil {
		return nil, err
	}
	body := map[string]string{"enabled": enabled}
	return c.doRaw(ctx, http.MethodPut, "/api/v1/me/triggers/"+triggerID, body)
}

func (c *HTTPClient) DeleteTrigger(ctx context.Context, triggerID string) error {
	if err := validateID(triggerID, "trigger_id"); err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/me/triggers/"+triggerID, nil, nil)
}

// doRaw executes an HTTP request and returns the raw JSON response body.
func (c *HTTPClient) doRaw(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	_, raw, err := c.doRequestRaw(ctx, method, path, body)
	return raw, err
}
