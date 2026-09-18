// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// staging_client.go — the production HTTP implementations of the two
// staging seams (worklog D1): the controller→API credential source (the
// internal llm-providers endpoint) and the controller→router internal API
// (mint + rotate). Both follow the CachedOrgStatusClient pattern.

// CachedLLMProviderSource is the LLMProviderSource backed by the API
// service's internal llm-providers endpoint with a short success-only
// cache. Unlike org-status there is NO stale-serve: a fetch failure fails
// the staging pass loudly (credentials must be fresh — a stale set could
// seal revoked material and miss newly-bound providers).
type CachedLLMProviderSource struct {
	baseURL    string
	token      string
	ttl        time.Duration
	httpClient *http.Client
	entries    sync.Map // ownerID/ws -> *cachedProviders
}

type cachedProviders struct {
	mu        sync.Mutex
	providers []secrets.LLMProviderData
	fetchedAt time.Time
}

// NewCachedLLMProviderSource constructs the source. baseURL is the API
// service root; token is the X-Internal-Token (LLMSAFESPACES_INTERNAL_TOKEN
// — the same Secret that authenticates the org-status call, mounted on both
// sides by the chart).
func NewCachedLLMProviderSource(baseURL, token string, ttl time.Duration) *CachedLLMProviderSource {
	if ttl <= 0 {
		ttl = relayProviderCacheTTL
	}
	return &CachedLLMProviderSource{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		ttl:        ttl,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *CachedLLMProviderSource) LLMProviders(ctx context.Context, ownerUserID, workspaceID string) ([]secrets.LLMProviderData, error) {
	if c == nil || c.baseURL == "" {
		return nil, fmt.Errorf("llm-provider source not configured (api-service-url unset?)")
	}
	key := ownerUserID + "/" + workspaceID
	v, _ := c.entries.LoadOrStore(key, &cachedProviders{})
	e := v.(*cachedProviders)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.providers != nil && time.Since(e.fetchedAt) < c.ttl {
		return e.providers, nil
	}
	fetched, err := c.fetch(ctx, ownerUserID, workspaceID)
	if err != nil {
		return nil, err
	}
	e.providers, e.fetchedAt = fetched, time.Now()
	return fetched, nil
}

func (c *CachedLLMProviderSource) fetch(ctx context.Context, ownerUserID, workspaceID string) ([]secrets.LLMProviderData, error) {
	url := fmt.Sprintf("%s/api/v1/internal/workspaces/%s/llm-providers?ownerUserID=%s",
		c.baseURL, url.PathEscape(workspaceID), url.QueryEscape(ownerUserID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call api: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned status %d", resp.StatusCode)
	}
	var body struct {
		Providers []secrets.LLMProviderData `json:"providers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return body.Providers, nil
}

// HTTPRelayRouterClient is the RelayRouterClient against the llm-relay
// router's internal API (US-72.2): POST /internal/v1/tokens and
// POST /internal/v1/keys/rotate, both authenticated by the controller-held
// mint key (Authorization: Bearer, constant-time compared server-side).
type HTTPRelayRouterClient struct {
	routerURL  string
	namespace  string
	mintSecret func(ctx context.Context) (string, error)
	httpClient *http.Client
}

// NewHTTPRelayRouterClient constructs the client. reader resolves the
// llm-relay-mint-key Secret in namespace (the controller creates it in the
// staging pass / startup guard; reads are cheap because mint and rotate are
// rare operations).
func NewHTTPRelayRouterClient(routerURL, namespace string, reader client.Reader) *HTTPRelayRouterClient {
	c := &HTTPRelayRouterClient{
		routerURL:  strings.TrimRight(routerURL, "/"),
		namespace:  namespace,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	c.mintSecret = func(ctx context.Context) (string, error) {
		sec := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Name: secrets.RelayMintKeyName, Namespace: namespace}, sec); err != nil {
			return "", fmt.Errorf("reading %s: %w", secrets.RelayMintKeyName, err)
		}
		key := string(sec.Data[secrets.RelayMintKeyDataKey])
		if key == "" {
			return "", fmt.Errorf("%s has an empty %q", secrets.RelayMintKeyName, secrets.RelayMintKeyDataKey)
		}
		return key, nil
	}
	return c
}

func (c *HTTPRelayRouterClient) post(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	key, err := c.mintSecret(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.routerURL+path, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call router: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("router returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (c *HTTPRelayRouterClient) MintToken(ctx context.Context, req RelayMintRequest) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.post(ctx, "/internal/v1/tokens", req, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("router minted an empty token")
	}
	return out.Token, nil
}

func (c *HTTPRelayRouterClient) RotateKeys(ctx context.Context) (RelayRotateReceipt, error) {
	var receipt RelayRotateReceipt
	if err := c.post(ctx, "/internal/v1/keys/rotate", struct{}{}, &receipt); err != nil {
		return RelayRotateReceipt{}, err
	}
	if receipt.KeyID == "" || receipt.Generation == 0 || len(receipt.PublicKey) == 0 {
		return RelayRotateReceipt{}, fmt.Errorf("router returned an incomplete rotate receipt (keyID=%q generation=%d)", receipt.KeyID, receipt.Generation)
	}
	return receipt, nil
}
