// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ghActionsDispatcher implements buildDispatcher by calling the GitHub
// Actions API (workflow_dispatch endpoint). Production implementation.
type ghActionsDispatcher struct {
	apiToken   string
	owner      string
	repo       string
	workflowID string
	ref        string
	client     *http.Client
}

// NewGHActionsDispatcher constructs a production dispatcher. The ref
// parameter is the git ref to dispatch against (e.g. "main", "master").
func NewGHActionsDispatcher(apiToken, owner, repo, workflowID, ref string) buildDispatcher {
	if ref == "" {
		ref = "main"
	}
	return &ghActionsDispatcher{
		apiToken:   apiToken,
		owner:      owner,
		repo:       repo,
		workflowID: workflowID,
		ref:        ref,
		client:     &http.Client{Timeout: 15 * time.Second},
	}
}

// dispatchURL is the format string for the GH Actions workflow_dispatch
// endpoint. Overridable in tests.
var dispatchURL = "https://api.github.com/repos/%s/%s/actions/workflows/%s/dispatches"

type ghDispatchPayload struct {
	Ref    string            `json:"ref"`
	Inputs map[string]string `json:"inputs"`
}

func (d *ghActionsDispatcher) Dispatch(ctx context.Context, req dispatchRequest) (int64, error) {
	url := fmt.Sprintf(dispatchURL, d.owner, d.repo, d.workflowID)

	payload := ghDispatchPayload{
		Ref: d.ref,
		Inputs: map[string]string{
			"build_id":       req.BuildID,
			"callback_url":   req.CallbackURL,
			"callback_token": req.CallbackToken,
			"hash":           req.Hash,
			"base_name":      req.BaseName,
			"base_version":   req.BaseVersion,
			"architectures":  joinArchs(req.Architectures),
			"dockerfile":     req.Dockerfile,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("gh dispatch: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("gh dispatch: request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+d.apiToken)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("gh dispatch: call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("gh dispatch: unexpected status %d: %s", resp.StatusCode, string(raw))
	}

	// The workflow_dispatch response has no run ID in the body. The API
	// returns 204/201 with no content. We return 0 — the callback endpoint
	// authenticates via the per-build token, not the GH run ID. The run ID
	// is only needed if we implement on-read status derivation later.
	return 0, nil
}

func joinArchs(archs []string) string {
	result := ""
	for i, a := range archs {
		if i > 0 {
			result += ","
		}
		result += a
	}
	return result
}
