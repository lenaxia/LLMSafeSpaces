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

type v2PromptBody struct {
	Text string `json:"text"`
	// Model is the fully-qualified "providerID/modelID" selector; omitted
	// (omitempty) when the send carries no override so the session default
	// applies. Callers must never pass a bare ID — opencode splits on "/"
	// and a bare value parses as provider-with-empty-model (incident
	// 2026-08-16). Use qualifiedModelID to build it.
	Model string `json:"model,omitempty"`
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
	return c.PromptV2WithModel(ctx, sessionID, text, delivery, "")
}

// PromptV2WithModel is PromptV2 with an optional model override in
// fully-qualified "providerID/modelID" form ("" = session default).
func (c *Client) PromptV2WithModel(ctx context.Context, sessionID, text string, delivery V2Delivery, model string) (*V2PromptResponse, error) {
	body, err := json.Marshal(v2PromptRequest{
		Prompt:   v2PromptBody{Text: text, Model: model},
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
		return nil, fmt.Errorf("%w: session %s: HTTP 409", ErrV2PromptConflict, sessionID)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: session %s: HTTP 404", ErrV2SessionNotFound, sessionID)
	}
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("POST /api/session/%s/prompt returned %d: %s", sessionID, resp.StatusCode, string(errBody))
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
