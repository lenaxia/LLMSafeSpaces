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
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ghActionsDispatcher implements buildDispatcher by calling the GitHub
// Actions API (workflow_dispatch endpoint). Production implementation.
//
// Authentication: uses a GitHub App (not a PAT). The App credentials
// (appID + privateKey) are used to mint a short-lived JWT, which is
// exchanged for an installation access token. The token is cached for
// 50 minutes (tokens last 1 hour). This is the same model the workflow
// itself uses via tj-actions/github-app-token.
type ghActionsDispatcher struct {
	appID      string
	privateKey string
	owner      string
	repo       string
	workflowID string
	ref        string
	client     *http.Client

	// Cached installation token (avoids re-minting on every dispatch).
	// Protected by tokenMu — concurrent dispatches must not race on the cache.
	tokenMu        sync.Mutex
	cachedToken    string
	cachedTokenExp time.Time
}

// NewGHActionsDispatcher constructs a production dispatcher. The appID
// and privateKey are the GitHub App credentials. The ref parameter is
// the git ref to dispatch against (e.g. "main", "master").
func NewGHActionsDispatcher(appID, privateKey, owner, repo, workflowID, ref string) buildDispatcher {
	if ref == "" {
		ref = "main"
	}
	return &ghActionsDispatcher{
		appID:      appID,
		privateKey: privateKey,
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

// ghBaseURL is the GitHub API base URL. Overridable in tests.
var ghBaseURL = "https://api.github.com"

type ghDispatchPayload struct {
	Ref    string            `json:"ref"`
	Inputs map[string]string `json:"inputs"`
}

func (d *ghActionsDispatcher) Dispatch(ctx context.Context, req dispatchRequest) (int64, error) {
	token, err := d.getInstallationToken(ctx)
	if err != nil {
		return 0, fmt.Errorf("gh dispatch: get token: %w", err)
	}

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
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return 0, fmt.Errorf("gh dispatch: call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// GitHub's workflow_dispatch endpoint returns 204 No Content on success
	// (not 201 Created). Accept either as success to be robust to API changes.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("gh dispatch: unexpected status %d: %s", resp.StatusCode, string(raw))
	}

	return 0, nil
}

// getInstallationToken returns a cached installation token or mints a
// new one. Tokens last 1 hour; we cache for 50 minutes to avoid edge
// cases near expiry.
func (d *ghActionsDispatcher) getInstallationToken(ctx context.Context) (string, error) {
	d.tokenMu.Lock()
	cached := d.cachedToken
	exp := d.cachedTokenExp
	d.tokenMu.Unlock()

	// Check cache (50-minute window).
	if cached != "" && time.Now().Before(exp) {
		return cached, nil
	}

	// Step 1: Mint a JWT signed with the App's private key.
	jwtToken, err := d.mintAppJWT()
	if err != nil {
		return "", fmt.Errorf("mint app jwt: %w", err)
	}

	// Step 2: Find the installation ID for this App on this owner/repo.
	installID, err := d.getInstallationID(ctx, jwtToken)
	if err != nil {
		return "", fmt.Errorf("get installation id: %w", err)
	}

	// Step 3: Exchange the JWT for an installation access token.
	token, err := d.createInstallationToken(ctx, jwtToken, installID)
	if err != nil {
		return "", fmt.Errorf("create installation token: %w", err)
	}

	d.tokenMu.Lock()
	d.cachedToken = token
	d.cachedTokenExp = time.Now().Add(50 * time.Minute)
	d.tokenMu.Unlock()

	return token, nil
}

// mintAppJWT creates a signed JWT for GitHub App authentication.
func (d *ghActionsDispatcher) mintAppJWT() (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(d.privateKey))
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}

	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		Issuer:    d.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(key)
}

// getInstallationID finds the App's installation on the owner/repo.
func (d *ghActionsDispatcher) getInstallationID(ctx context.Context, appJWT string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/installation", ghBaseURL, d.owner, d.repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(raw))
	}

	var installation struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&installation); err != nil {
		return 0, fmt.Errorf("decode installation: %w", err)
	}
	return installation.ID, nil
}

// createInstallationToken exchanges the App JWT for an installation
// access token (the token used for API calls as the App).
func (d *ghActionsDispatcher) createInstallationToken(ctx context.Context, appJWT string, installID int64) (string, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", ghBaseURL, installID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(raw))
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	return tokenResp.Token, nil
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
