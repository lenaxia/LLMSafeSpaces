// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// credential_probe.go — GET /:id/models handler for both admin and user credential flows.
//
// When the user configures a provider credential (admin or personal), the UI
// needs to show the full model list from that provider so the user can:
//   1. Select which models to allow (the allowlist)
//   2. Set a context limit per model (needed for contextTotal display)
//
// This endpoint decrypts the credential, calls the provider's /v1/models
// (OpenAI-compatible), and returns the list merged with any already-saved
// context limits so the UI can pre-populate the fields.
//
// The provider's /v1/models endpoint returns only {id, object, created,
// owned_by} — no context window data — for all standard OpenAI-compatible
// providers. Context limits must be user-entered; this endpoint just supplies
// the model IDs and any previously-saved limits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// ProbeModelEntry is one model returned by the probe endpoint.
type ProbeModelEntry struct {
	ID           string `json:"id"`
	ContextLimit int    `json:"contextLimit"` // 0 = unknown / not configured
	OutputLimit  int    `json:"outputLimit"`  // 0 = unknown / not configured
}

// ProbeModelsResponse is the response body for GET /:id/models.
type ProbeModelsResponse struct {
	Models  []ProbeModelEntry `json:"models"`
	BaseURL string            `json:"baseURL,omitempty"`
	// Warning is set when the /v1/models call failed. The response still
	// succeeds (200) but Models is empty so the UI shows a friendly message.
	Warning string `json:"warning,omitempty"`
}

// ProbeModelsRequest is the body for POST /api/v1/probe-models — a
// credential-free probe for use before a credential is saved.
type ProbeModelsRequest struct {
	APIKey  string `json:"apiKey" binding:"required" log:"-"` //nolint:gosec // G117 false positive — field has log:"-" tag, never marshaled to response
	BaseURL string `json:"baseURL" binding:"required"`
}

// ProbeModelsAnon handles POST /api/v1/probe-models.
// No credential ID needed — caller passes apiKey + baseURL directly.
// Auth is still required so arbitrary API keys can't be proxied by unauthenticated users.
// The baseURL is validated against SSRF rules before making any outbound request.
func ProbeModelsAnon(c *gin.Context) {
	var req ProbeModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "apiKey and baseURL are required"})
		return
	}

	// SSRF guard: reject private/internal URLs before making any outbound request.
	// This is the primary SSRF defense for the anon endpoint. Stored-credential
	// probes (GET /:id/models) are authenticated and the baseURL was user-supplied
	// at credential create time — they rely on network policy for additional isolation.
	if err := validateProbeBaseURL(req.BaseURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("baseURL rejected: %v", err)})
		return
	}

	pd := secrets.LLMProviderData{APIKey: req.APIKey, BaseURL: req.BaseURL}
	plaintext, err := json.Marshal(pd) //nolint:gosec // G117: marshaling for probeCredentialModels, never returned to caller
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	result := probeCredentialModels(c.Request.Context(), plaintext, probeCredentialLimits{})
	c.JSON(http.StatusOK, result)
}

// validateProbeBaseURL checks that a baseURL is safe to probe:
// - must parse as a valid URL
// - scheme must be https or http (not file://, ftp://, etc.)
// - host must not resolve to a private/loopback/internal address (SSRF guard)
//
// Private ranges blocked: loopback (127.x, ::1), link-local (169.254.x),
// RFC-1918 (10.x, 172.16-31.x, 192.168.x), IPv6 ULA (fc00::/7), and
// carrier-NAT shared space (100.64.0.0/10).
func validateProbeBaseURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("baseURL is empty")
	}
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("baseURL is not a valid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("baseURL scheme %q is not allowed (must be https or http)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("baseURL has no host")
	}
	// Reject bare IPs that are private without needing DNS.
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("baseURL %q is a private/internal address: SSRF not allowed", host)
		}
		return nil
	}
	// Reject private-looking hostnames before DNS resolution.
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") {
		return fmt.Errorf("baseURL hostname %q resolves to an internal address: SSRF not allowed", host)
	}
	// DNS resolution — check all returned addresses.
	resolver := &net.Resolver{}
	addrs, err := resolver.LookupHost(context.Background(), host)
	if err != nil {
		// DNS failure is not a security issue per se; let the subsequent HTTP
		// call fail naturally with a connect error.
		return nil
	}
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil && isPrivateIP(ip) {
			return fmt.Errorf("baseURL %q resolves to a private/internal address (%s): SSRF not allowed", host, addr)
		}
	}
	return nil
}

// isPrivateIP returns true for loopback, link-local, RFC-1918, and similar
// internal address ranges that must not be reachable via the probe endpoint.
func isPrivateIP(ip net.IP) bool {
	for _, cidr := range []string{
		"127.0.0.0/8",    // loopback
		"::1/128",        // IPv6 loopback
		"169.254.0.0/16", // link-local
		"fe80::/10",      // IPv6 link-local
		"10.0.0.0/8",     // RFC-1918
		"172.16.0.0/12",  // RFC-1918
		"192.168.0.0/16", // RFC-1918
		"fc00::/7",       // IPv6 ULA
		"100.64.0.0/10",  // shared address space (carrier NAT)
	} {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// probeCredentialModels takes the decrypted LLMProviderData, calls GET
// {baseURL}/v1/models with the stored API key, merges saved limits, and
// returns the probe response.
//
// plaintext must be the decrypted JSON-encoded LLMProviderData.
// savedLimits carries both the saved per-model context and output limits
// so the UI can pre-populate the model-config table; pass a zero
// probeCredentialLimits{} for the anon path where no credential row exists.
func probeCredentialModels(ctx context.Context, plaintext []byte, savedLimits probeCredentialLimits) ProbeModelsResponse {
	var pd secrets.LLMProviderData
	if err := json.Unmarshal(plaintext, &pd); err != nil {
		return ProbeModelsResponse{Warning: "credential data is unreadable"}
	}

	if pd.BaseURL == "" {
		// No custom BaseURL — this is a first-party provider (OpenAI, Anthropic, etc.)
		// whose model list is managed by opencode's internal catalog, not discoverable
		// via /v1/models without a provider-specific endpoint.
		return ProbeModelsResponse{
			BaseURL: "",
			Warning: "no baseURL configured — models for built-in providers cannot be discovered. Enter model IDs manually.",
		}
	}

	baseURL := pd.BaseURL
	if len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	url := baseURL + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ProbeModelsResponse{BaseURL: pd.BaseURL, Warning: fmt.Sprintf("failed to build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+pd.APIKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeModelsResponse{BaseURL: pd.BaseURL, Warning: fmt.Sprintf("failed to reach provider: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return ProbeModelsResponse{
			BaseURL: pd.BaseURL,
			Warning: fmt.Sprintf("provider returned HTTP %d: %s", resp.StatusCode, string(body)),
		}
	}

	var mlr struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mlr); err != nil {
		return ProbeModelsResponse{BaseURL: pd.BaseURL, Warning: fmt.Sprintf("failed to parse model list: %v", err)}
	}

	models := make([]ProbeModelEntry, 0, len(mlr.Data))
	for _, m := range mlr.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, ProbeModelEntry{
			ID:           m.ID,
			ContextLimit: savedLimits.Context[m.ID],
			OutputLimit:  savedLimits.Output[m.ID],
		})
	}

	return ProbeModelsResponse{
		BaseURL: pd.BaseURL,
		Models:  models,
	}
}

// ProbeModels handles GET /api/v1/admin/provider-credentials/:id/models.
// Admin variant — uses the platform KEK to decrypt.
func (h *AdminProviderCredentialsHandler) ProbeModels(c *gin.Context) {
	id := c.Param("id")
	resolveDecrypt := func(_ context.Context) (func(context.Context, []byte) ([]byte, error), string, int) {
		if h.provider != nil {
			return h.provider.Decrypt, "", 0
		}
		return nil, "master secret not configured", http.StatusServiceUnavailable
	}
	plaintext, limits, perr := getCredentialForProbe(c.Request.Context(), h.store, "admin", "_platform", id, resolveDecrypt)
	if perr != nil {
		c.JSON(perr.status, gin.H{"error": perr.msg})
		return
	}
	defer zeroBytes(plaintext)
	c.JSON(http.StatusOK, probeCredentialModels(c.Request.Context(), plaintext, limits))
}

// ProbeModels handles GET /api/v1/provider-credentials/:id/models.
// User variant — uses the session DEK to decrypt.
func (h *UserProviderCredentialsHandler) ProbeModels(c *gin.Context) {
	userID, sessionID := extractAuth(c)
	if userID == "" || sessionID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	matchedKey := extractMatchedSigningKey(c)

	resolveDecrypt := func(ctx context.Context) (func(context.Context, []byte) ([]byte, error), string, int) {
		dek, err := h.keys.GetDEK(ctx, sessionID, matchedKey)
		if err != nil {
			// Issue #593 Option C: same actionable message as the
			// Create endpoint. The probe path was the same opaque
			// "encryption unavailable" pre-fix and had the same
			// recovery paths the message points the caller at.
			if errors.Is(err, secrets.ErrDEKUnavailable) {
				return nil, "user credential encryption requires a password-authenticated session or an API key created with decryptAccess=true", http.StatusForbidden
			}
			return nil, "encryption key service unavailable", http.StatusServiceUnavailable
		}
		return func(_ context.Context, ct []byte) ([]byte, error) { return secrets.DecryptSecret(dek, ct) }, "", 0
	}
	plaintext, limits, perr := getCredentialForProbe(c.Request.Context(), h.store, "user", userID, c.Param("id"), resolveDecrypt)
	if perr != nil {
		c.JSON(perr.status, gin.H{"error": perr.msg})
		return
	}
	defer zeroBytes(plaintext)
	c.JSON(http.StatusOK, probeCredentialModels(c.Request.Context(), plaintext, limits))
}
