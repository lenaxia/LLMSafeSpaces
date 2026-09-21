// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Typed user_prompts store errors (#1499): handlers map these to their
// HTTP shapes (404 / 409) instead of sniffing SQL states.
var (
	ErrPromptNotFound  = errors.New("prompt not found")
	ErrPromptNameTaken = errors.New("prompt name already in use")
)

// ListUserPrompts returns a user's prompts, most-recently-touched first.
// No pagination in v1 — personal libraries are dozens-scale; the cap is
// the documented defensive bound.
func (s *Service) ListUserPrompts(ctx context.Context, userID string) ([]types.UserPrompt, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, content, created_at, updated_at FROM user_prompts WHERE user_id = $1
		 ORDER BY updated_at DESC LIMIT 500`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]types.UserPrompt, 0)
	for rows.Next() {
		var p types.UserPrompt
		if err := rows.Scan(&p.ID, &p.Name, &p.Content, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateUserPrompt inserts one prompt; a unique violation on
// (user_id, name) surfaces as ErrPromptNameTaken.
func (s *Service) CreateUserPrompt(ctx context.Context, userID, name, content string) (*types.UserPrompt, error) {
	var p types.UserPrompt
	err := s.DB.QueryRowContext(ctx,
		`INSERT INTO user_prompts (user_id, name, content)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, content, created_at, updated_at`, userID, name, content,
	).Scan(&p.ID, &p.Name, &p.Content, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		return nil, ErrPromptNameTaken
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateUserPrompt applies non-nil fields; zero rows affected means the
// prompt does not exist (or belongs to another user) → ErrPromptNotFound.
func (s *Service) UpdateUserPrompt(ctx context.Context, userID, id string, name, content *string) (*types.UserPrompt, error) {
	sets := []string{"updated_at = now()"}
	args := []any{userID, id}
	if name != nil {
		args = append(args, *name)
		sets = append(sets, fmt.Sprintf("name = $%d", len(args)))
	}
	if content != nil {
		args = append(args, *content)
		sets = append(sets, fmt.Sprintf("content = $%d", len(args)))
	}
	var p types.UserPrompt
	err := s.DB.QueryRowContext(ctx,
		`UPDATE user_prompts SET `+strings.Join(sets, ", ")+
			` WHERE user_id = $1 AND id = $2
		 RETURNING id, name, content, created_at, updated_at`, args...,
	).Scan(&p.ID, &p.Name, &p.Content, &p.CreatedAt, &p.UpdatedAt)
	if isUniqueViolation(err) {
		return nil, ErrPromptNameTaken
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPromptNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DeleteUserPrompt removes one prompt; zero rows affected → not found.
func (s *Service) DeleteUserPrompt(ctx context.Context, userID, id string) error {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM user_prompts WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrPromptNotFound
	}
	return nil
}
