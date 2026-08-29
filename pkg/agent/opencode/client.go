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
	"math/rand"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// Client communicates with a running opencode instance's HTTP API.
// It implements credential injection via PUT /auth/:providerID (Control API)
// and instance disposal via POST /instance/dispose.
//
// Every opencode endpoint — including /auth/* and /instance/* — is gated
// by HTTP Basic auth with username `agentd.AuthUsername` and the
// per-pod password mounted at /sandbox-cfg/password
// (= OPENCODE_SERVER_PASSWORD env var). Calling these endpoints without
// auth produces 401 + WWW-Authenticate: Basic realm="Secure Area",
// which is what broke the live credential flow in worklog 0125.
type Client struct {
	baseURL    string
	password   string
	httpClient *http.Client
	logger     *zap.Logger

	// Capability detection (#1119): probed lazily once per client; see
	// capabilities.go. Upstream changes wire shapes across patch releases,
	// so the adapter detects instead of assuming.
	capsOnce sync.Once
	cached   Capabilities
	// capsSource, when set (by the adapter at send time), consults the
	// adapter-scoped positive-only cache instead of this per-client
	// probe — clients are constructed per resolve, so a per-client cache
	// would re-probe every message.
	capsSource func(ctx context.Context, sessionID string) (Capabilities, error)
}

// setCapabilitySource injects the adapter-scoped capability source.
func (c *Client) setCapabilitySource(fn func(ctx context.Context, sessionID string) (Capabilities, error)) {
	c.capsSource = fn
}

type nonRetryableError struct {
	provider   string
	statusCode int
	attempt    int
}

func (e *nonRetryableError) Error() string {
	return fmt.Sprintf("PUT /auth/%s (attempt %d): client error: HTTP %d", e.provider, e.attempt, e.statusCode)
}

// Option configures a Client at construction.
type Option func(*Client)

// WithHTTPClient injects a pre-configured *http.Client (e.g. one shared
// across many workspaces for connection pooling — M11-a). When unset,
// NewClient allocates a default client with a 10s timeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// NewClient creates a Client targeting the given opencode base URL.
//
// password is the value mounted at /sandbox-cfg/password inside the
// sandbox pod and exported to opencode as OPENCODE_SERVER_PASSWORD. It
// is the SAME secret used by every other agentd → opencode call (see
// cmd/workspace-agentd/main.go OpenCodeClient). Passing the empty
// string is allowed (so unit tests that don't need auth-gated paths
// still work) but will fail against a real opencode server with 401.
func NewClient(baseURL, password string, logger *zap.Logger, opts ...Option) *Client {
	if logger == nil {
		logger = zap.NewNop()
	}
	c := &Client{
		baseURL:  baseURL,
		password: password,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger: logger,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// PushCredentials writes each provider's API key to opencode's auth store
// via PUT /auth/:providerID. This writes to auth.json but does NOT trigger
// provider state refresh — call DisposeInstance or (future) RefreshProviders
// afterward to pick up the new credentials.
//
// Returns nil if providers is empty (no-op).
// Returns the first error encountered; subsequent providers are not attempted.
func (c *Client) PushCredentials(ctx context.Context, providers []secrets.LLMProviderData) error {
	if len(providers) == 0 {
		return nil
	}

	for _, p := range providers {
		if err := c.setAuth(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// DisposeInstance triggers POST /instance/dispose, which invalidates all
// InstanceState caches for the current instance. The opencode process stays
// alive; the next request triggers a fresh instance load with updated auth.
//
// In-flight LLM calls are aborted. Sessions persist in SQLite.
func (c *Client) DisposeInstance(ctx context.Context) error {
	url := c.baseURL + "/instance/dispose"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("POST /instance/dispose: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /instance/dispose: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort drain
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /instance/dispose returned %d", resp.StatusCode)
	}
	return nil
}

// Abort calls POST /session/:id/abort on opencode. This is the V1 abort
// path, which is the only interrupt endpoint that exists on opencode
// 1.18.10+. The V2 endpoint (POST /api/session/:id/interrupt) was removed
// in 1.18.10 — the v2/ route group was deleted entirely from
// packages/opencode/src/server/routes/instance/httpapi/groups/v2/.
// On 1.18.10 the V2 path returns 204 from a catch-all stub but does
// nothing (verified live: a long V1 turn keeps running after V2
// interrupt). The V1 /abort path returns 200 and actually stops the
// in-flight turn (verified live: session transitions to idle).
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	url := c.baseURL + "/session/" + sessionID + "/abort"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("POST /session/%s/abort: build request: %w", sessionID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /session/%s/abort: %w", sessionID, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST /session/%s/abort returned %d", sessionID, resp.StatusCode)
	}
	return nil
}

// StageCredentials writes provider credentials to opencode's auth.json
// (via PUT /auth/:providerID) but does NOT trigger provider-state refresh.
// The credentials are "staged" — they exist on disk but opencode's
// in-memory provider state is unchanged until DisposeInstance is called
// separately by the caller (typically via POST /api/v1/workspaces/:id/agent/reload).
//
// Returns nil if providers is empty (no-op).
func (c *Client) StageCredentials(ctx context.Context, providers []secrets.LLMProviderData) error {
	return c.PushCredentials(ctx, providers)
}

// GetSessionStatuses calls GET /session/status on opencode and returns
// the current status of all known sessions. The map key is the session ID;
// the value is the status type string: "idle", "busy", or "retry".
func (c *Client) GetSessionStatuses(ctx context.Context) (map[string]string, error) {
	url := c.baseURL + "/session/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build session/status request: %w", err)
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /session/status: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /session/status returned %d: %s", resp.StatusCode, string(errBody))
	}

	var raw map[string]struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode session/status: %w", err)
	}
	result := make(map[string]string, len(raw))
	for id, info := range raw {
		result[id] = info.Type
	}
	return result, nil
}

// retryWithBackoff invokes fn up to maxAttempts times with exponential
// backoff (plus jitter), returning nil on success or the last error.
func (c *Client) retryWithBackoff(ctx context.Context, maxAttempts int, initialDelay time.Duration, fn func(attempt int) error) error {
	var lastErr error
	for i := 1; i <= maxAttempts; i++ {
		lastErr = fn(i)
		if lastErr == nil {
			return nil
		}
		var nre *nonRetryableError
		if errors.As(lastErr, &nre) {
			return lastErr
		}
		if i < maxAttempts {
			delay := initialDelay*time.Duration(1<<(i-1)) + time.Duration(rand.Intn(500))*time.Millisecond //nolint:gosec // intentional jitter, not crypto
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return lastErr
}

// setAuth sends PUT /auth/:providerID with the credential payload.
// :providerID in the URL is the credential's slug — that is the literal
// key in agent-config.json's provider map and what opencode persists as
// `providerID` on session records (Epic 55).
func (c *Client) setAuth(ctx context.Context, p secrets.LLMProviderData) error {
	payload := authPayload{
		Type: "api",
		Key:  p.APIKey,
	}
	if p.BaseURL != "" {
		payload.Metadata = map[string]string{"baseURL": p.BaseURL}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal auth payload for %s: %w", p.Slug, err)
	}

	return c.retryWithBackoff(ctx, 3, 1*time.Second, func(attempt int) error {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		url := c.baseURL + "/auth/" + p.Slug
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create PUT request for %s: %w", p.Slug, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(agentd.AuthUsername, c.password)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.logger.Warn("PUT /auth failed, will retry",
				zap.String("slug", p.Slug),
				zap.String("kind", p.Kind),
				zap.Int("attempt", attempt),
				zap.Error(err),
			)
			return fmt.Errorf("PUT /auth/%s (attempt %d): %w", p.Slug, attempt, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		_, _ = io.Copy(io.Discard, resp.Body)

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return &nonRetryableError{
				provider:   p.Slug,
				statusCode: resp.StatusCode,
				attempt:    attempt,
			}
		}
		if resp.StatusCode >= 500 {
			c.logger.Warn("PUT /auth server error, will retry",
				zap.String("slug", p.Slug),
				zap.String("kind", p.Kind),
				zap.Int("attempt", attempt),
				zap.Int("status", resp.StatusCode),
			)
			return fmt.Errorf("PUT /auth/%s (attempt %d): server error: HTTP %d", p.Slug, attempt, resp.StatusCode)
		}
		return nil
	})
}

// authPayload matches opencode's Auth.Info schema for type:"api".
type authPayload struct {
	Type     string            `json:"type"`
	Key      string            `json:"key"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// providerCatalogReadLimit bounds the /provider response read. opencode
// returns every provider from models.dev (139+ providers × many models),
// which is ~5 MB in practice. 32 MiB leaves headroom for growth without
// allowing an unbounded read. Worklog 0372 (C2): the limit was regressed
// to 1 MiB during the US-29.5 extraction and silently truncated the catalog.
const providerCatalogReadLimit = 32 << 20

// ListModels calls GET /provider on opencode and returns the raw JSON body.
// The caller is responsible for parsing the response shape (it varies by
// opencode version). The body is size-limited to providerCatalogReadLimit.
func (c *Client) ListModels(ctx context.Context) ([]byte, error) {
	url := c.baseURL + "/provider"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET /provider: %w", err)
	}
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /provider: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET /provider returned %d: %s", resp.StatusCode, string(errBody))
	}
	return io.ReadAll(io.LimitReader(resp.Body, providerCatalogReadLimit))
}

// PatchConfig calls PATCH /global/config on opencode with the given config
// map. Used by SetModel to change the active model.
func (c *Client) PatchConfig(ctx context.Context, config map[string]any) error {
	body, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config patch: %w", err)
	}
	url := c.baseURL + "/global/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build PATCH /global/config: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PATCH /global/config: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PATCH /global/config returned %d", resp.StatusCode)
	}
	return nil
}
