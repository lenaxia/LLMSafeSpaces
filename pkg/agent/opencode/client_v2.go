// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// V2Delivery selects how opencode's V2 session runner admits a prompt.
//
// "queue" — the prompt is admitted to the durable SessionInput table and
// promoted when the session would otherwise go idle (F2, F8). This is the
// default for the inboard-session-queue epic (US-63.3).
//
// "steer" — the prompt is injected at the next safe boundary without
// aborting in-flight tools (F-spike). Deferred to a follow-up epic; the V2
// API supports it but US-63.3 defaults to "queue".
type V2Delivery string

const (
	V2DeliveryQueue V2Delivery = "queue"
	V2DeliverySteer V2Delivery = "steer"
)

// V2 prompt/interrupt error sentinels. These are distinct from the V1
// errors in agent_client.go because the V2 API has different failure modes
// (PromptConflictError on caller-supplied id, SessionNotFound on unknown
// session). Callers branch on these to decide retry vs. surface-to-user.
var (
	ErrV2PromptConflict  = errors.New("opencode V2: prompt conflict (id collision)")
	ErrV2SessionNotFound = errors.New("opencode V2: session not found")
)

// v2PromptRequest is the body for POST /api/session/:sid/prompt.
//
// F18 (spike-verified): opencode 1.18.10 requires {prompt:{text:"..."}}, NOT
// {prompt:{parts:[...]}} — the parts-based shape from the Epic 65 contract
// is newer than 1.18.10 and returns 400 InvalidRequestError. When opencode
// is bumped to a version that accepts parts, this struct must change and a
// schema test must pin the new shape (US-63.2 acceptance criterion).
//
// F17: the id field is intentionally OMITTED. opencode generates the message
// id via SessionMessage.ID.create(); a caller-supplied id risks
// PromptConflictError (409) on collision — the same class of bug as the V1
// message-id hack, avoided by simply not sending one.
type v2PromptRequest struct {
	Prompt   v2PromptBody `json:"prompt"`
	Delivery V2Delivery   `json:"delivery"`
	// Resume is intentionally omitted: F15 confirmed no resume endpoint or
	// parameter exists on 1.18.10. If a future opencode version adds it,
	// add it here with a spike-verified schema pin.
}

type v2PromptBody struct {
	Text string `json:"text"`
}

// V2PromptResponse is the data payload returned by a successful
// POST /api/session/:sid/prompt. The spike confirmed HTTP 200 with
// {data:{admittedSeq, id, sessionID, prompt, delivery, timeCreated}}.
type V2PromptResponse struct {
	AdmittedSeq int    `json:"admittedSeq"`
	ID          string `json:"id"`
	SessionID   string `json:"sessionID"`
	TimeCreated string `json:"timeCreated,omitempty"`
}

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
	body, err := json.Marshal(v2PromptRequest{
		Prompt:   v2PromptBody{Text: text},
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
