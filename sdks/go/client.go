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
	Workflows                *WorkflowsService
	Triggers                 *TriggersService
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
	c.Workflows = &WorkflowsService{c: c}
	c.Triggers = &TriggersService{c: c}
	return c
}

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.decode(resp, result)
}

// doWithHeader performs a request, decodes the JSON body into result, and
// also returns the response headers (e.g. pagination cursors).
func (c *Client) doWithHeader(ctx context.Context, method, path string, body, result any) (http.Header, error) {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := c.decode(resp, result); err != nil {
		return nil, err
	}
	return resp.Header, nil
}

// send builds and executes an authenticated request, returning the raw
// response. Callers own Body.Close.
func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	url := c.baseURL + "/api/v1" + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.email != "" {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, parseError(resp)
	}
	return resp, nil
}

// decode reads a successful response body into result following the same
// 204/202/empty-body contract as the historical do().
func (c *Client) decode(resp *http.Response, result any) error {
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
		Error      string `json:"error"`
		Message    string `json:"message"`
		Reason     string `json:"reason"`
		RetryAfter int    `json:"retryAfter"`
	}
	json.NewDecoder(resp.Body).Decode(&errResp)
	msg := errResp.Message
	if msg == "" {
		msg = errResp.Error
	}
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{
		Status:     resp.StatusCode,
		Message:    msg,
		Reason:     errResp.Reason,
		RetryAfter: errResp.RetryAfter,
	}
}
