// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// V2Delivery is re-exported from pkg/agent (the canonical location).
// Callers should use agent.V2Delivery; this alias exists for backward
// compatibility.
type V2Delivery = agent.V2Delivery

// Re-export V2 delivery constants.
const (
	V2DeliveryQueue = agent.V2DeliveryQueue
	V2DeliverySteer = agent.V2DeliverySteer
)

// V2 prompt/interrupt error sentinels. These are distinct from the V1
// V2 prompt/interrupt error sentinels re-exported from pkg/agent
// (the canonical location). Callers branch on these to decide retry vs.
// surface-to-user.
var (
	ErrV2PromptConflict  = agent.ErrV2PromptConflict
	ErrV2SessionNotFound = agent.ErrV2SessionNotFound
)

// V2ModelRef is the per-prompt model override in opencode's OBJECT wire
// form ({"modelID", "providerID"}), matching the V1 /message schema
// (packages/sdk/openapi.json @v1.18.10: model is an object, both fields
// required, additionalProperties false). The V2 prompt endpoint's route
// group is dormant on 1.18.10 (its OpenAPI exposes no model property at
// all), but the override still uses the object form — it mirrors V1's
// schema so the send does not replay the string-form regression
// (2026-08-17 all-sessions-502, #909) on revival.
type V2ModelRef struct {
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
}

type v2PromptBody struct {
	Text string `json:"text"`
	// Model is the per-prompt override in object form; omitted
	// (omitempty on the pointer) when the send carries no override so the
	// session default applies. Callers must never send a bare ID or a
	// "provider/model" string — build it via modelOverride, which applies
	// the Provider-authoritative split rules and rejects unexpressible
	// shapes. The wire shape of the ref is capability-selected at send
	// time (see modelRefWire): opencode >= 1.18.15 requires
	// {"id","providerID"}; <= 1.18.14 used {"modelID","providerID"} and
	// silently rejects the other shape (#1119 stress finding 5).
	Model any `json:"model,omitempty"`
}

// v2PromptRequest is the body for POST /api/session/:sid/prompt.
type v2PromptRequest struct {
	Prompt   v2PromptBody `json:"prompt"`
	Delivery V2Delivery   `json:"delivery"`
}

// V2PromptResponse is re-exported from pkg/agent.
type V2PromptResponse = agent.V2PromptResponse

// PromptV2 sends a prompt to an opencode V2 session endpoint with the given
// delivery mode. It POSTs to /api/session/:sid/prompt using the same Basic
// auth (opencode + password) as every other opencode call.
//
// The response is admit-and-schedule (non-streaming): opencode queues the
// prompt internally and returns immediately. The body is read+discarded on
// non-2xx; on 2xx the data payload is decoded and returned.
//
// F17: the caller MUST NOT supply a message id — this method omits it
// unconditionally. F18: the prompt body is {text:"..."} (plain string),
// not the parts-based contract shape; see the v2PromptRequest doc comment.
func (c *Client) PromptV2(ctx context.Context, sessionID, text string, delivery V2Delivery) (*V2PromptResponse, error) {
	return c.PromptV2WithModel(ctx, sessionID, text, delivery, nil)
}

// PromptV2WithModel is PromptV2 with an optional per-prompt model override
// in object form (nil = session default).
func (c *Client) PromptV2WithModel(ctx context.Context, sessionID, text string, delivery V2Delivery, model *V2ModelRef) (*V2PromptResponse, error) {
	wire, err := c.modelRefWireFor(ctx, sessionID, model)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(v2PromptRequest{
		Prompt:   v2PromptBody{Text: text, Model: wire},
		Delivery: delivery,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal V2 prompt body: %w", err)
	}

	url := fmt.Sprintf("%s/api/session/%s/prompt", c.baseURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build V2 prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/session/%s/prompt: %w", sessionID, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusConflict {
		// Multi-wrap: ErrHTTPStatus keeps the outbox's definitive-
		// rejection classification (agent PROCESSED the request); the
		// specific sentinel stays addressable for callers that branch
		// on it.
		return nil, fmt.Errorf("%w: %w: session %s: HTTP 409", agent.ErrHTTPStatus, ErrV2PromptConflict, sessionID)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %w: session %s: HTTP 404", agent.ErrHTTPStatus, ErrV2SessionNotFound, sessionID)
	}
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: POST /api/session/%s/prompt returned %d: %s", agent.ErrHTTPStatus, sessionID, resp.StatusCode, string(errBody))
	}

	var envelope struct {
		Data V2PromptResponse `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode V2 prompt response: %w", err)
	}
	return &envelope.Data, nil
}

// InterruptV2 sends a non-destructive interrupt to an opencode V2 session.
// It POSTs to /api/session/:sid/interrupt with Basic auth.
//
// The spike confirmed: returns HTTP 204 (No Content) both when a turn is
// in-flight AND when the session is idle (no-op success). Interrupt is
// non-destructive (F8): admitted-but-unpromoted queue entries survive and
// run on the next execution.wake. This replaces the V1 destructive abort
// that cleared the Redis queue.
func (c *Client) InterruptV2(ctx context.Context, sessionID string) error {
	url := fmt.Sprintf("%s/api/session/%s/interrupt", c.baseURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return fmt.Errorf("build V2 interrupt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /api/session/%s/interrupt: %w", sessionID, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST /api/session/%s/interrupt returned %d: %s", sessionID, resp.StatusCode, string(errBody))
	}
	return nil
}

// V2Message is one message from the V2 store's history view
// (GET /api/session/:sid/message → {data:[...]}). Captured live against
// opencode 1.18.15 (design 0052); differs from the V1 shape in three
// load-bearing ways:
//
//   - role discriminates on top-level `type` (user/assistant/system),
//     not info.role
//   - user prompt text is a top-level `text` string (no parts)
//   - assistant content lives in `content[]` where a tool part carries
//     name/id at the top level and input/output/times inside `state`
//     (closer to the legacy nested shape than 1.18.10's flat V1 shape)
type V2Message struct {
	ID   string `json:"id"`
	Type string `json:"type"` // user | assistant | system
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`

	// user
	Text string `json:"text,omitempty"`

	// assistant
	Agent   string            `json:"agent,omitempty"`
	Model   *V2ModelInMessage `json:"model,omitempty"`
	Content []V2ContentPart   `json:"content,omitempty"`
	Finish  string            `json:"finish,omitempty"`
	Cost    float64           `json:"cost,omitempty"`
	Tokens  *V2Tokens         `json:"tokens,omitempty"`
}

// V2ModelInMessage is the model attribution on a V2 assistant message.
type V2ModelInMessage struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// V2Tokens mirrors the opencode token accounting object (same field
// names on the V1 and V2 surfaces).
type V2Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Reason int64 `json:"reasoning"`
	Cache  struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// V2ContentPart is one assistant content item. Text and tool are the
// types observed on 1.18.15; unknown types preserve raw JSON for the
// contract's Custom pressure-relief valve.
type V2ContentPart struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`

	// tool
	Name  string          `json:"name,omitempty"`
	State *V2ToolState    `json:"state,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

// V2ToolState is the tool execution state on a V2 content part.
type V2ToolState struct {
	Status  string          `json:"status,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Time       *struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran,omitempty"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time,omitempty"`
}

// UnmarshalJSON captures the raw part for Custom fallback.
func (p *V2ContentPart) UnmarshalJSON(data []byte) error {
	type alias V2ContentPart
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = V2ContentPart(a)
	p.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// MessagesV2 fetches the V2 store's message list for a session
// (descending, newest first — same ordering contract as the V1
// endpoint). Streaming-decoded with a 64MB bound, mirroring
// GetHistory's large-transcript posture.
func (c *Client) MessagesV2(ctx context.Context, sessionID string) ([]V2Message, error) {
	url := fmt.Sprintf("%s/api/session/%s/message", c.baseURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build V2 messages request: %w", err)
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/session/%s/message (V2): %w", sessionID, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /api/session/%s/message (V2) returned %d: %s", sessionID, resp.StatusCode, string(errBody))
	}

	var envelope struct {
		Data []V2Message `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode V2 messages: %w", err)
	}
	return envelope.Data, nil
}
