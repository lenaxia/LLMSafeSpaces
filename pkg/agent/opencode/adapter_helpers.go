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
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// doPost is the shared POST helper for the Adapter. Adds Basic auth
// and JSON content type; the caller handles the response.
func (a *Adapter) doPost(ctx context.Context, c *Client, path string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("POST %s: marshal body: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("POST %s: build request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return resp, nil
}

// doGet is the shared GET helper for the Adapter.
func (a *Adapter) doGet(ctx context.Context, c *Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: build request: %w", path, err)
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp, nil
}

// readBody reads up to limit bytes from resp.Body. Bounds the cost
// of a misbehaving upstream that streams unbounded output.
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// ErrHTTPStatus marks errors where the agent PROCESSED the request and
// answered with a status >= 400: the outcome is definitive (rejected),
// not ambiguous. Callers driving at-least-once retries use this to
// distinguish "safe to retry" (rejection) from "outcome unknown"
// (transport cut / timeout mid-request — see VerifyDelivery).
var ErrHTTPStatus = errors.New("opencode http status")

// httpError reads a small portion of resp.Body and returns an error
// that includes the status code and body excerpt. Caller has already
// deferred-Close'd the body.
func (a *Adapter) httpError(path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%w: %s returned %d: %s", ErrHTTPStatus, path, resp.StatusCode, string(body))
}

// parseProviderCatalogForContract extracts the model catalog from
// opencode's /provider response and converts it to the platform's
// session.ModelInfo shape. Returns empty slice when the response
// is unparseable (the caller treats it as "no models available").
//
// opencode's /provider returns:
//
//	{
//	  "connected": ["openai", "anthropic", ...],
//	  "all": [
//	    {"id":"openai","models":{"gpt-4o":{"id":"gpt-4o","name":"GPT-4o","limit":{"context":128000,"output":16384}}, ...}},
//	    ...
//	  ]
//	}
//
// We surface one ModelInfo per model from every connected provider;
// the caller's UI groups by Provider. The opencode-relay block
// (providerID=="opencode-relay") is included so free-tier models
// appear when relay injection ran.
func parseProviderCatalogForContract(body []byte) ([]session.ModelInfo, error) {
	var raw struct {
		Connected []string `json:"connected"`
		All       []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Limit struct {
					Context int `json:"context"`
					Output  int `json:"output"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"all"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("opencode provider catalog: parse: %w", err)
	}
	connected := make(map[string]bool, len(raw.Connected))
	for _, id := range raw.Connected {
		connected[id] = true
	}

	var out []session.ModelInfo
	for _, p := range raw.All {
		if !connected[p.ID] {
			continue
		}
		for _, m := range p.Models {
			id := m.ID
			if id == "" {
				continue
			}
			out = append(out, session.ModelInfo{
				ID:            id,
				Provider:      p.ID,
				DisplayName:   m.Name,
				ContextWindow: int64(m.Limit.Context),
				MaxOutput:     int64(m.Limit.Output),
			})
		}
	}
	return out, nil
}
