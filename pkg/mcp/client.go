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

	"google.golang.org/protobuf/encoding/protojson"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
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
	UploadFile(ctx context.Context, workspaceID, filename string, content []byte) (*UploadResp, error)

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
	UpdateWorkflow(ctx context.Context, workflowID string, name, status, specYAML *string) (json.RawMessage, error)
	RunWorkflow(ctx context.Context, workflowID, input, workspaceID string) (json.RawMessage, error)
	GetWorkflowRunStatus(ctx context.Context, runID string) (json.RawMessage, error)
	CancelWorkflowRun(ctx context.Context, runID string) error

	// Trigger management (Epic 64)
	ListTriggers(ctx context.Context) (json.RawMessage, error)
	CreateTrigger(ctx context.Context, name, sourceType, sourceConfig, workspaceID, workflowID, prompt, memoryMode, captureMode, preserveSession string) (json.RawMessage, error)
	UpdateTrigger(ctx context.Context, triggerID string, enabled *bool) (json.RawMessage, error)
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

// SessionResp is the response from session creation. The production
// route POST /workspaces/:id/sessions/new returns
// types.EnsureSessionResponse, whose session identifier is serialized
// as "sessionId" (#1033).
type SessionResp struct {
	ID string `json:"sessionId"`
}

// Message is one entry of session history, in the platform session
// contract shape (design 0049): every message is id/type/parts, and
// prose lives in text parts — the legacy {role, content} decode never
// matched the API response and always decoded empty (#1034 cluster).
type Message struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

// TextContent returns the concatenated text parts of the message.
func (m Message) TextContent() string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
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

// CreateSession creates a session via POST /workspaces/:id/sessions/new
// (design 0041). The response is types.EnsureSessionResponse; the
// session identifier arrives in the sessionId field.
func (c *HTTPClient) CreateSession(ctx context.Context, workspaceID string) (*SessionResp, error) {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return nil, err
	}
	var resp SessionResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/sessions/new", nil, &resp); err != nil {
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

// SendMessage sends a prompt via prompt_async, then subscribes to the
// ABI contract stream (GET /workspaces/:id/contract-events — protojson
// StreamFrames, US-69.11) until the target session goes IDLE or the
// agent asks for input.
func (c *HTTPClient) SendMessage(ctx context.Context, workspaceID, sessionID, message string, timeout time.Duration) (string, error) {
	if err := validateID(sessionID, "session_id"); err != nil {
		return "", err
	}
	if len(message) > maxMessageSize {
		return "", fmt.Errorf("message too large (%d bytes, max %d)", len(message), maxMessageSize)
	}

	// 1. Fire prompt_async. The /prompt handler accepts the parts-based
	// body shape ({parts:[{type:text,...}]}) — a bare {"message": ...}
	// key extracts no text and is rejected (#1034).
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": message}},
	}
	path := fmt.Sprintf("/api/v1/workspaces/%s/sessions/%s/prompt", workspaceID, sessionID)
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return "", err
	}

	// 2. Consume the contract stream until the turn completes. The
	// sseCtx bounds the whole wait (including reconnects); the parent
	// ctx stays alive for the history fallback and the async
	// permission auto-approve.
	sseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	text, question, _ := c.awaitTurnCompletion(sseCtx, ctx, workspaceID, sessionID)
	if question != nil {
		out, merr := json.Marshal(question)
		if merr == nil {
			return string(out), nil
		}
	}
	if text != "" {
		// Live text wins — including partial text when the wait budget
		// expired mid-turn (the history fallback would answer with a
		// stale prior message).
		return text, nil
	}

	// Fallback: poll history (using parent context, not the timed-out SSE context)
	return c.fallbackHistory(ctx, workspaceID, sessionID)
}

// errContractUnseeded marks a stream that ended before delivering its
// snapshot frame — a broken endpoint (or a pre-snapshot cut). Retrying
// cannot help within a call; the history fallback answers instead.
var errContractUnseeded = fmt.Errorf("contract stream ended without a snapshot frame")

// contractReconnectPause spaces reconnect attempts so a wedged endpoint
// cannot be hot-looped for the whole wait budget.
const contractReconnectPause = 200 * time.Millisecond

// awaitTurnCompletion drives the contract-stream client rules (snapshot
// first, seq discard, resync → reconnect) against the three outcomes
// SendMessage cares about: the target session going IDLE (done), an
// input request (question result / headless permission auto-approve),
// and PART_DELTA accumulation for live text. replyCtx outlives the
// stream context and backs the async permission replies.
func (c *HTTPClient) awaitTurnCompletion(ctx, replyCtx context.Context, workspaceID, sessionID string) (string, *questionEventResult, error) {
	var response strings.Builder
	var lastSeq uint64

	for {
		if ctx.Err() != nil {
			return response.String(), nil, ctx.Err()
		}

		url := fmt.Sprintf("%s/api/v1/workspaces/%s/contract-events", c.BaseURL, workspaceID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return response.String(), nil, fmt.Errorf("create stream request: %w", err)
		}
		req.Header.Set("Accept", "text/event-stream")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			// Stream failed or timed out — surface partial text; the
			// caller falls back to history otherwise.
			return response.String(), nil, err
		}

		read := c.readContractStream(resp.Body, workspaceID, sessionID, &lastSeq, &response, replyCtx)
		_ = resp.Body.Close()

		if read.question != nil || read.idle {
			return response.String(), read.question, nil
		}
		if read.reconnect {
			// Explicit resync/reseed notice, or a non-snapshot first
			// frame (protocol violation — possibly before seeding):
			// reconnect for a fresh stamped snapshot.
			select {
			case <-ctx.Done():
				return response.String(), nil, ctx.Err()
			case <-time.After(contractReconnectPause):
			}
			continue
		}
		if !read.seeded {
			return response.String(), nil, errContractUnseeded
		}

		// Seeded but the stream ended (cut mid-wait): reconnect — a
		// fresh connection delivers a fresh stamped snapshot and the
		// seq discard rule dedups replayed events.
		select {
		case <-ctx.Done():
			return response.String(), nil, ctx.Err()
		case <-time.After(contractReconnectPause):
		}
	}
}

// contractStreamRead is one connection's outcome.
type contractStreamRead struct {
	seeded    bool
	idle      bool
	question  *questionEventResult
	reconnect bool
}

// readContractStream parses the SSE body (data/event/comment lines,
// per the SSE framing) and folds ABI StreamFrames until the target
// session goes IDLE, an input request arrives, or the body ends.
// Parse errors on individual frames are tolerated (skipped) — one bad
// frame never kills the wait.
func (c *HTTPClient) readContractStream(body io.Reader, workspaceID, sessionID string, lastSeq *uint64, response *strings.Builder, replyCtx context.Context) contractStreamRead {
	var read contractStreamRead
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: true}

	handle := func(eventName, data string) {
		// The slow-consumer sentinel: drop everything and reconnect for
		// a fresh stamped snapshot.
		if eventName == "resync" {
			read.reconnect = true
			return
		}
		var frame abiv1.StreamFrame
		if err := unmarshal.Unmarshal([]byte(data), &frame); err != nil {
			return // tolerated
		}
		switch f := frame.GetFrame().(type) {
		case *abiv1.StreamFrame_Snapshot:
			snap := f.Snapshot
			read.seeded = true
			if snap.GetAtSeq() > *lastSeq {
				*lastSeq = snap.GetAtSeq()
			}
			// A snapshot whose target session is already IDLE completes
			// the wait immediately (checked before any event).
			for _, s := range snap.GetSnapshot().GetSessions() {
				if s.GetSessionId() == sessionID && s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_IDLE {
					read.idle = true
					return
				}
			}
		case *abiv1.StreamFrame_Event:
			if !read.seeded {
				// Non-snapshot first frame violates the protocol —
				// reconnect (the Go reference client's rule).
				read.reconnect = true
				return
			}
			seqed := f.Event
			if seqed.GetSeq() <= *lastSeq {
				return // discard rule: seq ≤ snapshot stamp / already applied
			}
			*lastSeq = seqed.GetSeq()
			evt := seqed.GetEvent()
			if evt == nil {
				return
			}
			sid := evt.GetSessionId()
			if sid == "" && evt.GetInput() != nil {
				sid = evt.GetInput().GetSessionId()
			}
			if sid != sessionID {
				return // other sessions on the pod-wide stream never bleed in
			}
			switch evt.GetType() {
			case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
				if evt.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_IDLE {
					read.idle = true
				}
			case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
				if in := evt.GetInput(); in != nil {
					if in.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION {
						// Headless auto-approve (unchanged flow): the
						// reply POST rides replyCtx, not the stream ctx.
						inID := in.GetId()
						go func() { _ = c.PermissionReply(replyCtx, workspaceID, inID, "always", "") }()
						return
					}
					// QUESTION (or unspecified — surface it rather than
					// hang the turn): structured result for the caller.
					if raw, merr := protojson.Marshal(in); merr == nil {
						read.question = &questionEventResult{Type: "question", Request: raw}
					}
				}
			case abiv1.EventType_EVENT_TYPE_PART_DELTA:
				response.WriteString(evt.GetDelta())
			}
		case *abiv1.StreamFrame_Reseeded:
			// Mandatory re-snapshot (I3): reconnect for a fresh snapshot.
			read.reconnect = true
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)

	var eventName string
	var data strings.Builder
	dispatch := func() {
		if data.Len() > 0 || eventName != "" {
			handle(eventName, data.String())
		}
		eventName = ""
		data.Reset()
	}
	for scanner.Scan() {
		// Guard against unbounded accumulation
		if response.Len() > maxSSETotal {
			return read
		}
		line := scanner.Text()
		switch {
		case line == "":
			dispatch()
			if read.idle || read.question != nil || read.reconnect {
				return read
			}
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(d)
		default:
			// retry: and unknown fields are ignored
		}
	}
	dispatch()
	return read
}

func (c *HTTPClient) fallbackHistory(ctx context.Context, workspaceID, sessionID string) (string, error) {
	var msgs []Message
	histPath := fmt.Sprintf("/api/v1/workspaces/%s/sessions/%s/message", workspaceID, sessionID)
	if err := c.doJSON(ctx, http.MethodGet, histPath, nil, &msgs); err != nil {
		return "", fmt.Errorf("fallback history fetch: %w", err)
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].TextContent(), nil
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

// UpdateWorkflow issues a partial update via PUT /me/workflows/:id. The
// API binds types.UpdateWorkflowRequest (pointer fields; nil = keep
// existing), so only the provided arguments are sent — omitted fields
// must not appear in the body at all, or their empty-string values fail
// validation server-side (#1036).
func (c *HTTPClient) UpdateWorkflow(ctx context.Context, workflowID string, name, status, specYAML *string) (json.RawMessage, error) {
	if err := validateID(workflowID, "workflow_id"); err != nil {
		return nil, err
	}
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if status != nil {
		body["status"] = *status
	}
	if specYAML != nil {
		body["specYaml"] = *specYAML
	}
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

// UpdateTrigger issues a partial update via PUT /me/triggers/:id. The
// API binds types.UpdateTriggerRequest.Enabled *bool — a nil enabled
// sends no key (keep existing); a present value must be a JSON boolean
// (#1035).
func (c *HTTPClient) UpdateTrigger(ctx context.Context, triggerID string, enabled *bool) (json.RawMessage, error) {
	if err := validateID(triggerID, "trigger_id"); err != nil {
		return nil, err
	}
	body := map[string]any{}
	if enabled != nil {
		body["enabled"] = *enabled
	}
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
