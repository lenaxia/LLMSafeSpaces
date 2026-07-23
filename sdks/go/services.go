// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
	"encoding/json"
	"fmt"
)

// WorkspacesService handles workspace operations.
type WorkspacesService struct{ c *Client }

func (s *WorkspacesService) List(ctx context.Context, limit, offset int) (*WorkspaceListResult, error) {
	var result WorkspaceListResult
	err := s.c.do(ctx, "GET", fmt.Sprintf("/workspaces?limit=%d&offset=%d", limit, offset), nil, &result)
	return &result, err
}

func (s *WorkspacesService) Create(ctx context.Context, req CreateWorkspaceRequest) (*Workspace, error) {
	var ws Workspace
	err := s.c.do(ctx, "POST", "/workspaces", req, &ws)
	return &ws, err
}

func (s *WorkspacesService) Get(ctx context.Context, id string) (*Workspace, error) {
	var ws Workspace
	err := s.c.do(ctx, "GET", "/workspaces/"+id, nil, &ws)
	return &ws, err
}

func (s *WorkspacesService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/workspaces/"+id, nil, nil)
}

func (s *WorkspacesService) Suspend(ctx context.Context, id string) error {
	return s.c.do(ctx, "POST", "/workspaces/"+id+"/suspend", nil, nil)
}

func (s *WorkspacesService) Restart(ctx context.Context, id string) error {
	return s.c.do(ctx, "POST", "/workspaces/"+id+"/restart", nil, nil)
}

// RefreshCompute re-syncs the workspace's resource defaults with the platform's
// current configuration and rebuilds the pod so it picks up the latest runtime
// image version. Returns the bumped restart generation.
func (s *WorkspacesService) RefreshCompute(ctx context.Context, id string) (*RefreshWorkspaceResult, error) {
	var resp RefreshWorkspaceResult
	err := s.c.do(ctx, "POST", "/workspaces/"+id+"/refresh-compute", nil, &resp)
	return &resp, err
}

func (s *WorkspacesService) Rename(ctx context.Context, id, name string) error {
	return s.c.do(ctx, "PUT", "/workspaces/"+id, map[string]string{"name": name}, nil)
}

func (s *WorkspacesService) Activate(ctx context.Context, id string) (*ActivateWorkspaceResponse, error) {
	var resp ActivateWorkspaceResponse
	err := s.c.do(ctx, "POST", "/workspaces/"+id+"/activate", nil, &resp)
	return &resp, err
}

func (s *WorkspacesService) GetStatus(ctx context.Context, id string) (*WorkspaceStatus, error) {
	var st WorkspaceStatus
	err := s.c.do(ctx, "GET", "/workspaces/"+id+"/status", nil, &st)
	return &st, err
}

func (s *WorkspacesService) SetBindings(ctx context.Context, id string, secretIDs []string) error {
	body := map[string][]string{"secretIds": secretIDs}
	return s.c.do(ctx, "PUT", "/workspaces/"+id+"/bindings", body, nil)
}

func (s *WorkspacesService) GetBindings(ctx context.Context, id string) (*BindingsResponse, error) {
	var resp BindingsResponse
	err := s.c.do(ctx, "GET", "/workspaces/"+id+"/bindings", nil, &resp)
	return &resp, err
}

func (s *WorkspacesService) ReloadSecrets(ctx context.Context, id string) (*ReloadResult, error) {
	var result ReloadResult
	err := s.c.do(ctx, "POST", "/workspaces/"+id+"/reload-secrets", nil, &result)
	return &result, err
}

func (s *WorkspacesService) SetEnv(ctx context.Context, id string, vars map[string]string) error {
	body := map[string]any{"vars": vars}
	return s.c.do(ctx, "PUT", "/workspaces/"+id+"/env", body, nil)
}

func (s *WorkspacesService) GetEnv(ctx context.Context, id string) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/workspaces/"+id+"/env", nil, &result)
	return result, err
}

func (s *WorkspacesService) DeleteEnv(ctx context.Context, id, varName string) error {
	return s.c.do(ctx, "DELETE", "/workspaces/"+id+"/env/"+varName, nil, nil)
}

func (s *WorkspacesService) GetModels(ctx context.Context, id string) (*ModelListResponse, error) {
	var resp ModelListResponse
	err := s.c.do(ctx, "GET", "/workspaces/"+id+"/models", nil, &resp)
	return &resp, err
}

func (s *WorkspacesService) SetModel(ctx context.Context, id, model string) error {
	return s.c.do(ctx, "PUT", "/workspaces/"+id+"/model", map[string]string{"model": model}, nil)
}

func (s *WorkspacesService) ReloadAgent(ctx context.Context, id string) error {
	return s.c.do(ctx, "POST", "/workspaces/"+id+"/agent/reload", nil, nil)
}

// SessionsService handles session operations.
type SessionsService struct{ c *Client }

func (s *SessionsService) Ensure(ctx context.Context, workspaceID string) (*EnsureSessionResponse, error) {
	var resp EnsureSessionResponse
	err := s.c.do(ctx, "POST", "/workspaces/"+workspaceID+"/sessions/new", nil, &resp)
	return &resp, err
}

func (s *SessionsService) SendMessage(ctx context.Context, workspaceID, sessionID, content string) (*MessageResponse, error) {
	body := map[string]any{
		"content": content,
		"parts":   []map[string]string{{"type": "text", "text": content}},
	}
	var raw json.RawMessage
	err := s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/sessions/%s/message", workspaceID, sessionID), body, &raw)
	if err != nil {
		return nil, err
	}
	text := extractText(raw)
	return &MessageResponse{Raw: raw, Content: text}, nil
}

func (s *SessionsService) GetHistory(ctx context.Context, workspaceID, sessionID string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	err := s.c.do(ctx, "GET", fmt.Sprintf("/workspaces/%s/sessions/%s/message", workspaceID, sessionID), nil, &result)
	return result, err
}

func (s *SessionsService) Abort(ctx context.Context, workspaceID, sessionID string) error {
	return s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/sessions/%s/abort", workspaceID, sessionID), nil, nil)
}

func (s *SessionsService) List(ctx context.Context, workspaceID string) ([]SessionListItem, error) {
	var result []SessionListItem
	err := s.c.do(ctx, "GET", "/workspaces/"+workspaceID+"/sessions", nil, &result)
	return result, err
}

func (s *SessionsService) GetActive(ctx context.Context, workspaceID string) (*ActiveSessionsResponse, error) {
	var resp ActiveSessionsResponse
	err := s.c.do(ctx, "GET", "/workspaces/"+workspaceID+"/sessions/active", nil, &resp)
	return &resp, err
}

func (s *SessionsService) Rename(ctx context.Context, workspaceID, sessionID, title string) error {
	return s.c.do(ctx, "PUT",
		fmt.Sprintf("/workspaces/%s/sessions/%s/title", workspaceID, sessionID),
		map[string]string{"title": title}, nil)
}

func (s *SessionsService) Get(ctx context.Context, workspaceID, sessionID string) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", fmt.Sprintf("/workspaces/%s/sessions/%s", workspaceID, sessionID), nil, &result)
	return result, err
}

func (s *SessionsService) SendPromptAsync(ctx context.Context, workspaceID, sessionID, message string) error {
	body := map[string]string{"message": message}
	return s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/sessions/%s/prompt", workspaceID, sessionID), body, nil)
}

func (s *SessionsService) Delete(ctx context.Context, workspaceID, sessionID string) error {
	return s.c.do(ctx, "DELETE", fmt.Sprintf("/workspaces/%s/sessions/%s", workspaceID, sessionID), nil, nil)
}

func (s *SessionsService) Enqueue(ctx context.Context, workspaceID, sessionID, text string) (string, error) {
	var resp struct {
		MessageID string `json:"messageID"`
	}
	err := s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/sessions/%s/queue", workspaceID, sessionID),
		map[string]string{"text": text}, &resp)
	return resp.MessageID, err
}

func (s *SessionsService) ListQueue(ctx context.Context, workspaceID, sessionID string) ([]QueuedMessage, error) {
	var resp struct {
		Messages []QueuedMessage `json:"messages"`
	}
	err := s.c.do(ctx, "GET", fmt.Sprintf("/workspaces/%s/sessions/%s/queue", workspaceID, sessionID), nil, &resp)
	return resp.Messages, err
}

func (s *SessionsService) DismissQueued(ctx context.Context, workspaceID, sessionID, messageID string) error {
	return s.c.do(ctx, "DELETE", fmt.Sprintf("/workspaces/%s/sessions/%s/queue/%s", workspaceID, sessionID, messageID), nil, nil)
}

func (s *SessionsService) MarkSeen(ctx context.Context, workspaceID, sessionID string) error {
	return s.c.do(ctx, "PUT", fmt.Sprintf("/workspaces/%s/sessions/%s/seen", workspaceID, sessionID), nil, nil)
}

// AuthService handles authentication operations.
type AuthService struct{ c *Client }

func (s *AuthService) Me(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/auth/me", nil, &result)
	return result, err
}

func (s *AuthService) CreateAPIKey(ctx context.Context, name string) (*APIKey, error) {
	var key APIKey
	err := s.c.do(ctx, "POST", "/auth/api-keys", map[string]string{"name": name}, &key)
	return &key, err
}

func (s *AuthService) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var keys []APIKey
	err := s.c.do(ctx, "GET", "/auth/api-keys", nil, &keys)
	return keys, err
}

func (s *AuthService) DeleteAPIKey(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/auth/api-keys/"+id, nil, nil)
}

// SecretsService handles secret operations.
type SecretsService struct{ c *Client }

func (s *SecretsService) Create(ctx context.Context, name, secretType, value string) (*SecretResponse, error) {
	body := map[string]string{"name": name, "type": secretType, "value": value}
	var resp SecretResponse
	err := s.c.do(ctx, "POST", "/secrets", body, &resp)
	return &resp, err
}

func (s *SecretsService) List(ctx context.Context) ([]SecretResponse, error) {
	// The API returns {"secrets": [...]}. Decode once; no second HTTP request.
	var wrapper struct {
		Secrets []SecretResponse `json:"secrets"`
	}
	err := s.c.do(ctx, "GET", "/secrets", nil, &wrapper)
	if err != nil {
		return nil, err
	}
	return wrapper.Secrets, nil
}

func (s *SecretsService) Get(ctx context.Context, id string) (*SecretResponse, error) {
	var resp SecretResponse
	err := s.c.do(ctx, "GET", "/secrets/"+id, nil, &resp)
	return &resp, err
}

func (s *SecretsService) Update(ctx context.Context, id, value string) error {
	return s.c.do(ctx, "PUT", "/secrets/"+id, map[string]string{"value": value}, nil)
}

func (s *SecretsService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/secrets/"+id, nil, nil)
}

func (s *SecretsService) Reveal(ctx context.Context, id, password string) (string, error) {
	var resp struct {
		Value string `json:"value"`
	}
	err := s.c.do(ctx, "POST", "/secrets/"+id+"/reveal", map[string]string{"password": password}, &resp)
	return resp.Value, err
}

func (s *SecretsService) GetAuditLog(ctx context.Context) ([]AuditEntry, error) {
	var wrapper struct {
		Entries []AuditEntry `json:"entries"`
	}
	err := s.c.do(ctx, "GET", "/secrets/audit", nil, &wrapper)
	return wrapper.Entries, err
}

func (s *SecretsService) GetBindingsForSecret(ctx context.Context, id string) ([]string, error) {
	var wrapper struct {
		Workspaces []string `json:"workspaces"`
	}
	err := s.c.do(ctx, "GET", "/secrets/"+id+"/bindings", nil, &wrapper)
	return wrapper.Workspaces, err
}

// TerminalService handles terminal operations.
type TerminalService struct{ c *Client }

func (s *TerminalService) GetTicket(ctx context.Context, workspaceID string) (*TerminalTicket, error) {
	var ticket TerminalTicket
	err := s.c.do(ctx, "POST", "/workspaces/"+workspaceID+"/terminal/ticket", nil, &ticket)
	return &ticket, err
}

func extractText(raw json.RawMessage) string {
	var obj struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	var sb string
	for _, p := range obj.Parts {
		if p.Type == "text" {
			sb += p.Text
		}
	}
	return sb
}

// UserSettingsService handles user settings.
type UserSettingsService struct{ c *Client }

func (s *UserSettingsService) Get(ctx context.Context) (*UserSettings, error) {
	var result UserSettings
	err := s.c.do(ctx, "GET", "/users/me/settings", nil, &result)
	return &result, err
}

func (s *UserSettingsService) GetSchema(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/users/me/settings/schema", nil, &result)
	return result, err
}

func (s *UserSettingsService) Set(ctx context.Context, key string, value any) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "PUT", "/users/me/settings/"+key, map[string]any{"value": value}, &result)
	return result, err
}

// AccountService handles account key management.
type AccountService struct{ c *Client }

func (s *AccountService) RotateKey(ctx context.Context, password string) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "POST", "/account/rotate-key", map[string]string{"password": password}, &result)
	return result, err
}

func (s *AccountService) ChangePassword(ctx context.Context, oldPassword, newPassword string) error {
	body := map[string]string{"oldPassword": oldPassword, "newPassword": newPassword}
	return s.c.do(ctx, "POST", "/account/change-password", body, nil)
}

func (s *AccountService) Recover(ctx context.Context, userID, recoveryKey, newPassword string) (map[string]any, error) {
	var result map[string]any
	body := map[string]string{"userId": userID, "recoveryKey": recoveryKey, "newPassword": newPassword}
	err := s.c.do(ctx, "POST", "/account/recover", body, &result)
	return result, err
}

// --- Provider Credentials (Epic 30) ---

// ProviderCredentialResponse is the API response for a provider credential.
//
// Epic 55 identity model:
//   - Kind: SDK-class enum (openai, anthropic, openai_compatible, ...).
//     Determines which adapter opencode loads.
//   - Slug: per-owner unique identity AND the literal key in
//     agent-config.json's provider map. opencode persists this as
//     `providerID` on session records.
//   - Name: free-form UX display label.
//
// ModelContextLimits and ModelOutputLimits map model IDs to per-model token
// limits. Both maps MUST be populated together for a given model id for the
// limit to take effect in the running workspace: opencode's config JSON Schema
// requires both `limit.context` and `limit.output` to be set whenever the
// `limit` block is present. If only one is set, the server stores the value
// but agent-config.json omits the entire limit block for that model and
// opencode falls back to built-in defaults.
type ProviderCredentialResponse struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Kind               string         `json:"kind"`
	Slug               string         `json:"slug"`
	BaseURL            string         `json:"baseURL,omitempty"`
	ModelAllowlist     []string       `json:"modelAllowlist"`
	ModelContextLimits map[string]int `json:"modelContextLimits"`
	ModelOutputLimits  map[string]int `json:"modelOutputLimits"`
	CreatedAt          string         `json:"createdAt"`
	UpdatedAt          string         `json:"updatedAt"`
}

// ProviderCredentialsService handles user provider credential operations.
type ProviderCredentialsService struct{ c *Client }

// Create posts a new user-scoped provider credential. Kind selects the SDK
// class (e.g. "openai", "anthropic", "openai_compatible"); slug is a
// slug-safe per-owner identity that becomes the agent-config.json provider
// key. Slug must match ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$.
func (s *ProviderCredentialsService) Create(ctx context.Context, name, kind, slug, apiKey, baseURL string) (*ProviderCredentialResponse, error) {
	body := map[string]string{"name": name, "kind": kind, "slug": slug, "apiKey": apiKey}
	if baseURL != "" {
		body["baseURL"] = baseURL
	}
	var result ProviderCredentialResponse
	err := s.c.do(ctx, "POST", "/provider-credentials", body, &result)
	return &result, err
}

func (s *ProviderCredentialsService) List(ctx context.Context) ([]ProviderCredentialResponse, error) {
	var result []ProviderCredentialResponse
	err := s.c.do(ctx, "GET", "/provider-credentials", nil, &result)
	return result, err
}

func (s *ProviderCredentialsService) Get(ctx context.Context, id string) (*ProviderCredentialResponse, error) {
	var result ProviderCredentialResponse
	err := s.c.do(ctx, "GET", "/provider-credentials/"+id, nil, &result)
	return &result, err
}

func (s *ProviderCredentialsService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/provider-credentials/"+id, nil, nil)
}

func (s *ProviderCredentialsService) Bind(ctx context.Context, credID, workspaceID string) error {
	var result map[string]any
	return s.c.do(ctx, "POST", "/provider-credentials/"+credID+"/bind/"+workspaceID, nil, &result)
}

func (s *ProviderCredentialsService) Unbind(ctx context.Context, credID, workspaceID string) error {
	return s.c.do(ctx, "DELETE", "/provider-credentials/"+credID+"/bind/"+workspaceID, nil, nil)
}

// AdminProviderCredentialsService handles admin provider credential operations.
type AdminProviderCredentialsService struct{ c *Client }

func (s *AdminProviderCredentialsService) List(ctx context.Context) ([]ProviderCredentialResponse, error) {
	var result []ProviderCredentialResponse
	err := s.c.do(ctx, "GET", "/admin/provider-credentials", nil, &result)
	return result, err
}

// UsageService handles usage and quota queries.
type UsageService struct{ c *Client }

func (s *UsageService) Get(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/usage", nil, &result)
	return result, err
}

func (s *UsageService) GetWorkspace(ctx context.Context, workspaceID string) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/workspaces/"+workspaceID+"/usage", nil, &result)
	return result, err
}

func (s *UsageService) GetQuota(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "GET", "/usage/quota", nil, &result)
	return result, err
}

// InputRequestsService handles agent question and permission requests.
type InputRequestsService struct{ c *Client }

func (s *InputRequestsService) ListQuestions(ctx context.Context, workspaceID string) ([]map[string]any, error) {
	var result []map[string]any
	err := s.c.do(ctx, "GET", "/workspaces/"+workspaceID+"/question", nil, &result)
	return result, err
}

func (s *InputRequestsService) ReplyQuestion(ctx context.Context, workspaceID, requestID string, body map[string]any) error {
	return s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/question/%s/reply", workspaceID, requestID), body, nil)
}

func (s *InputRequestsService) RejectQuestion(ctx context.Context, workspaceID, requestID string) error {
	return s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/question/%s/reject", workspaceID, requestID), nil, nil)
}

func (s *InputRequestsService) ListPermissions(ctx context.Context, workspaceID string) ([]map[string]any, error) {
	var result []map[string]any
	err := s.c.do(ctx, "GET", "/workspaces/"+workspaceID+"/permission", nil, &result)
	return result, err
}

func (s *InputRequestsService) ReplyPermission(ctx context.Context, workspaceID, requestID string, body map[string]any) error {
	return s.c.do(ctx, "POST", fmt.Sprintf("/workspaces/%s/permission/%s/reply", workspaceID, requestID), body, nil)
}

// AuthService additions: auth lifecycle methods.
func (s *AuthService) Register(ctx context.Context, username, email, password string) (map[string]any, error) {
	body := map[string]string{"username": username, "email": email, "password": password}
	var result map[string]any
	err := s.c.do(ctx, "POST", "/auth/register", body, &result)
	return result, err
}

func (s *AuthService) Logout(ctx context.Context) error {
	return s.c.do(ctx, "POST", "/auth/logout", nil, nil)
}

func (s *AuthService) RequestPasswordReset(ctx context.Context, email string) error {
	return s.c.do(ctx, "POST", "/auth/password-reset/request", map[string]string{"email": email}, nil)
}

func (s *AuthService) ConfirmPasswordReset(ctx context.Context, token, newPassword string) error {
	return s.c.do(ctx, "POST", "/auth/password-reset/confirm", map[string]string{"token": token, "newPassword": newPassword}, nil)
}

func (s *AuthService) VerifyEmail(ctx context.Context, token string) error {
	return s.c.do(ctx, "POST", "/auth/verify-email", map[string]string{"token": token}, nil)
}

func (s *AuthService) ResendVerification(ctx context.Context, email string) error {
	return s.c.do(ctx, "POST", "/auth/verify-email/resend", map[string]string{"email": email}, nil)
}

func (s *AuthService) Lookup(ctx context.Context, email string) (string, error) {
	var result struct {
		RedirectURL string `json:"redirectUrl"`
	}
	err := s.c.do(ctx, "POST", "/auth/lookup", map[string]string{"email": email}, &result)
	return result.RedirectURL, err
}

func (s *AuthService) UnlockDek(ctx context.Context, password string) error {
	return s.c.do(ctx, "POST", "/auth/unlock-dek", map[string]string{"password": password}, nil)
}

// ProbeService handles anonymous credential model probing.
type ProbeService struct{ c *Client }

func (s *ProbeService) ProbeModels(ctx context.Context, apiKey, baseURL string) (map[string]any, error) {
	var result map[string]any
	err := s.c.do(ctx, "POST", "/probe-models", map[string]string{"apiKey": apiKey, "baseURL": baseURL}, &result)
	return result, err
}
