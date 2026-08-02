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
	"sort"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// LLMExplainerConfig configures the LLM-based build-failure explainer.
type LLMExplainerConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

// llmExplainer implements failureExplainer by calling the platform's
// existing LLM infrastructure (LiteLLM/vLLM in-cluster, design/0046 #22).
// It asks the LLM to: (1) explain the build failure in plain language,
// and (2) attribute it to a single extension if possible.
//
// Degradation: if the LLM is unavailable, returns a fallback string and
// no attribution — the caller (callback handler) proceeds normally.
type llmExplainer struct {
	cfg    LLMExplainerConfig
	client *http.Client
}

// NewLLMExplainer constructs an LLM explainer. Timeout is 15s — this runs
// in the callback path, not a user request path, so a slow LLM response
// is acceptable but unbounded is not.
func NewLLMExplainer(cfg LLMExplainerConfig) *llmExplainer {
	return &llmExplainer{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// chatCompletionRequest is the OpenAI-compatible chat completions payload.
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionResponse is the response shape we parse.
type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// explainResponse is the structured JSON we ask the LLM to return.
type explainResponse struct {
	Explanation         string `json:"explanation"`
	AttributedExtension string `json:"attributedExtension,omitempty"`
}

// fallbackExplanation is returned when the LLM is unavailable or fails.
const fallbackExplanation = "this combination failed to build; contact your administrator for details"

// Explain calls the LLM with the build-failure log tail and resolved
// values, asking for a plain-language explanation + attribution.
// On any error (LLM down, timeout, parse failure), returns the fallback
// string with no attribution — the callback proceeds normally.
func (e *llmExplainer) Explain(ctx context.Context, logTail string, rv imagefactory.ResolvedValues) (string, string, error) {
	if e.cfg.BaseURL == "" || e.cfg.Model == "" {
		return fallbackExplanation, "", nil
	}

	prompt := buildExplainPrompt(logTail, rv)
	body, err := json.Marshal(chatCompletionRequest{
		Model: e.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: explainSystemPrompt},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return fallbackExplanation, "", nil
	}

	url := e.cfg.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fallbackExplanation, "", nil
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fallbackExplanation, "", nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fallbackExplanation, "", nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fallbackExplanation, "", nil
	}

	var chatResp chatCompletionResponse
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		return fallbackExplanation, "", nil
	}
	if len(chatResp.Choices) == 0 {
		return fallbackExplanation, "", nil
	}

	content := chatResp.Choices[0].Message.Content
	var parsed explainResponse
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		// LLM didn't return JSON — use the raw content as the explanation.
		if content != "" {
			return content, "", nil
		}
		return fallbackExplanation, "", nil
	}
	if parsed.Explanation == "" {
		return fallbackExplanation, "", nil
	}
	return parsed.Explanation, parsed.AttributedExtension, nil
}

const explainSystemPrompt = `You are a DevOps assistant explaining Docker image build failures.
Analyze the build log tail and the list of installed extensions.
Return ONLY a JSON object with these fields:
  {"explanation": "<one or two sentences in plain language>", "attributedExtension": "<extension ID that caused the failure, or empty if unclear>"}
Do not include markdown, code fences, or any text outside the JSON.`

func buildExplainPrompt(logTail string, rv imagefactory.ResolvedValues) string {
	ids := make([]string, 0, len(rv))
	for id := range rv {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sb strings.Builder
	for _, id := range ids {
		v := rv[id]
		sb.WriteString(fmt.Sprintf("- %s (type=%s, value=%s)\n", id, v.Type, v.Value))
	}
	return fmt.Sprintf("The following Docker image build failed.\n\nExtensions installed:\n%s\n\nBuild log tail:\n%s\n\nExplain why this build failed and which extension (if any) is responsible.", sb.String(), logTail)
}
