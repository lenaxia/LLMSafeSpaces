// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"context"
)

// User prompt library (#1499) — the caller's saved prompts
// (/me/prompts). Owner-scoped CRUD; the composer @-recall surface
// reads the same list. Distinct from the platform prompt-POLICY
// surface (platform/org/workspace prompts).

// UserPromptsService manages the caller's saved prompts.
type UserPromptsService struct{ c *Client }

func (s *UserPromptsService) List(ctx context.Context) ([]UserPrompt, error) {
	var resp struct {
		Prompts []UserPrompt `json:"prompts"`
	}
	if err := s.c.do(ctx, "GET", "/me/prompts", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Prompts, nil
}

func (s *UserPromptsService) Create(ctx context.Context, req CreateUserPromptRequest) (*UserPrompt, error) {
	var resp struct {
		Prompt UserPrompt `json:"prompt"`
	}
	if err := s.c.do(ctx, "POST", "/me/prompts", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Prompt, nil
}

func (s *UserPromptsService) Update(ctx context.Context, id string, req UpdateUserPromptRequest) (*UserPrompt, error) {
	var resp struct {
		Prompt UserPrompt `json:"prompt"`
	}
	if err := s.c.do(ctx, "PUT", "/me/prompts/"+id, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Prompt, nil
}

func (s *UserPromptsService) Delete(ctx context.Context, id string) error {
	return s.c.do(ctx, "DELETE", "/me/prompts/"+id, nil, nil)
}
