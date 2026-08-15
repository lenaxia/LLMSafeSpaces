// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"go.uber.org/zap"
)

// OpenCodeClient is the HTTP client agentd uses to talk to the local
// opencode serve process. All requests use HTTP basic auth with the
// workspace password; the base URL comes from the package-level
// agentAddrAtomic (set once at boot, overridable in tests via
// setAgentAddr).
type OpenCodeClient struct {
	password string
	client   *http.Client
}

func (c *OpenCodeClient) doRequest(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", getAgentAddr()+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)
	return c.client.Do(req)
}

func (c *OpenCodeClient) IsHealthy(ctx context.Context) (bool, string, error) {
	resp, err := c.doRequest(ctx, "/global/health")
	if err != nil {
		return false, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, "", err
	}
	return result.Healthy, result.Version, nil
}

func (c *OpenCodeClient) ConnectedProviders(ctx context.Context) ([]string, error) {
	resp, err := c.doRequest(ctx, "/provider")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Connected []string `json:"connected"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Connected, nil
}

func (c *OpenCodeClient) ConfiguredProviderCount(ctx context.Context) (int, error) {
	resp, err := c.doRequest(ctx, "/config/providers")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Providers []struct{} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}
	return len(result.Providers), nil
}

// ModelContextLimit queries /config/providers for the context window limit of a given model.
// Returns 0 if the model or limit cannot be found.
func (c *OpenCodeClient) ModelContextLimit(ctx context.Context, modelID, providerID string) int64 {
	resp, err := c.doRequest(ctx, "/config/providers")
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Providers []struct {
			ID     string `json:"id"`
			Models map[string]struct {
				ID    string `json:"id"`
				Limit struct {
					Context int64 `json:"context"`
				} `json:"limit"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0
	}
	for _, p := range result.Providers {
		if providerID != "" && p.ID != providerID {
			continue
		}
		if m, ok := p.Models[modelID]; ok && m.Limit.Context > 0 {
			return m.Limit.Context
		}
		// Fallback: search all models in this provider
		for _, m := range p.Models {
			if m.ID == modelID && m.Limit.Context > 0 {
				return m.Limit.Context
			}
		}
	}
	return 0
}

func (c *OpenCodeClient) ListSessions(ctx context.Context) ([]agentd.SessionInfo, error) {
	resp, err := c.doRequest(ctx, "/session")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var sessions []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Tokens *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Cache     struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Model *struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, err
	}
	result := make([]agentd.SessionInfo, len(sessions))
	for i, s := range sessions {
		info := agentd.SessionInfo{ID: s.ID, Title: s.Title, Status: "idle"}
		if s.Tokens != nil {
			info.Tokens = &agentd.SessionTokens{
				Input:      s.Tokens.Input,
				Output:     s.Tokens.Output,
				Reasoning:  s.Tokens.Reasoning,
				CacheRead:  s.Tokens.Cache.Read,
				CacheWrite: s.Tokens.Cache.Write,
			}
		}
		if s.Model != nil {
			info.Model = s.Model.ID
		}
		// If title wasn't in list, fetch it individually
		if info.Title == "" {
			if title := c.fetchSessionTitle(ctx, s.ID); title != "" {
				info.Title = title
			}
		}
		result[i] = info
	}
	return result, nil
}

func (c *OpenCodeClient) fetchSessionTitle(ctx context.Context, sessionID string) string {
	resp, err := c.doRequest(ctx, "/session/"+sessionID)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var s struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s.Title
}

// fetchSessionPromptTokens recovers the most recent assistant prompt-token
// usage for a session by scanning its message history. Used by the
// fillGaps background loop to backfill sessions that started before the
// SSE tracker was connected.
func (c *OpenCodeClient) fetchSessionPromptTokens(ctx context.Context, sessionID string) int64 {
	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.doRequest(fetchCtx, "/session/"+sessionID+"/message?limit=20")
	if err != nil {
		log.Debug("fetchSessionPromptTokens: request failed", zap.Error(err), zap.String("sessionID", sessionID))
		return 0
	}
	defer func() { _ = resp.Body.Close() }()

	var messages []struct {
		Info struct {
			Role   string `json:"role"`
			Tokens *struct {
				Input int64 `json:"input"`
				Cache struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&messages); err != nil {
		log.Debug("fetchSessionPromptTokens: decode failed", zap.Error(err), zap.String("sessionID", sessionID))
		return 0
	}

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Info.Role == "assistant" && messages[i].Info.Tokens != nil {
			return messages[i].Info.Tokens.Input + messages[i].Info.Tokens.Cache.Read + messages[i].Info.Tokens.Cache.Write
		}
	}
	log.Debug("fetchSessionPromptTokens: no assistant message with tokens found", zap.String("sessionID", sessionID))
	return 0
}
