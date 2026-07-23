// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package llmsafespaces provides a typed Go client for the LLMSafeSpaces API.
package llmsafespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the LLMSafeSpaces API client.
type Client struct {
	baseURL    string
	apiKey     string
	token      string
	email      string
	password   string
	httpClient *http.Client

	Workspaces               *WorkspacesService
	Sessions                 *SessionsService
	Auth                     *AuthService
	Secrets                  *SecretsService
	Terminal                 *TerminalService
	UserSettings             *UserSettingsService
	Account                  *AccountService
	ProviderCredentials      *ProviderCredentialsService
	AdminProviderCredentials *AdminProviderCredentialsService
	Prompts                  *PromptsService
	AgentRoles               *AgentRolesService
	Usage                    *UsageService
	InputRequests            *InputRequestsService
	Probe                    *ProbeService
}

// Option configures the client.
type Option func(*Client)

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithCredentials sets email/password for JWT authentication.
func WithCredentials(email, password string) Option {
	return func(c *Client) { c.email = email; c.password = password }
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithTimeout sets the default request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// New creates a new LLMSafeSpaces client.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	c.Workspaces = &WorkspacesService{c: c}
	c.Sessions = &SessionsService{c: c}
	c.Auth = &AuthService{c: c}
	c.Secrets = &SecretsService{c: c}
	c.Terminal = &TerminalService{c: c}
	c.UserSettings = &UserSettingsService{c: c}
	c.Account = &AccountService{c: c}
	c.ProviderCredentials = &ProviderCredentialsService{c: c}
	c.AdminProviderCredentials = &AdminProviderCredentialsService{c: c}
	c.Prompts = &PromptsService{c: c}
	c.AgentRoles = &AgentRolesService{c: c}
	c.Usage = &UsageService{c: c}
	c.InputRequests = &InputRequestsService{c: c}
	c.Probe = &ProbeService{c: c}
	return c
}

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	url := c.baseURL + "/api/v1" + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.email != "" {
		if err := c.login(ctx); err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return parseError(resp)
	}

	// 204 No Content has no body by definition. 202 Accepted MAY carry a
	// payload describing the accepted operation (RFC 7231 §6.3.3), so read
	// the body and decode only when it is non-empty AND a result is wanted
	// (preserving the void contract for callers like Suspend/Restart).
	if resp.StatusCode == 204 {
		return nil
	}
	if result == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, result)
}

func (c *Client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return &APIError{Status: resp.StatusCode, Message: "login failed"}
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	c.token = result.Token
	return nil
}

func parseError(resp *http.Response) error {
	var errResp struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&errResp)
	msg := errResp.Error
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}
