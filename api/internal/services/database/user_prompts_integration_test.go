// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package database

// #1499: the user_prompts store semantics against real Postgres — CRUD
// round-trip, the (user_id, name) unique constraint, cross-user
// isolation, and per-user updates. Runs in the secrets-integration CI
// job (path-triggered); skips locally without TEST_DATABASE_URL.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
)

func TestIntegration_UserPrompts_CRUDRoundTrip(t *testing.T) {
	h := testharness.New(t)
	svc := &Service{DB: h.SQLDB()}
	ctx := h.NewContext()

	_, _ = h.Pool().Exec(ctx, `DELETE FROM user_prompts WHERE user_id IN ('int-prompt-u1', 'int-prompt-u2')`)

	created, err := svc.CreateUserPrompt(ctx, "int-prompt-u1", "Weekly summary", "Summarize the week.")
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)

	listed, err := svc.ListUserPrompts(ctx, "int-prompt-u1")
	require.NoError(t, err)
	require.Len(t, listed, 1)

	_, err = svc.CreateUserPrompt(ctx, "int-prompt-u1", "Weekly summary", "dup")
	require.ErrorIs(t, err, ErrPromptNameTaken, "the (user_id, name) constraint surfaces typed")

	// Cross-user isolation: same name under another user is fine, and
	// u2 cannot touch u1's row.
	_, err = svc.CreateUserPrompt(ctx, "int-prompt-u2", "Weekly summary", "other user")
	require.NoError(t, err, "name uniqueness is per user")
	_, err = svc.UpdateUserPrompt(ctx, "int-prompt-u2", created.ID, nil, nil)
	require.ErrorIs(t, err, ErrPromptNotFound, "u2 cannot update u1's prompt")
	err = svc.DeleteUserPrompt(ctx, "int-prompt-u2", created.ID)
	require.ErrorIs(t, err, ErrPromptNotFound, "u2 cannot delete u1's prompt")

	newName := "Renamed"
	newContent := "New body"
	updated, err := svc.UpdateUserPrompt(ctx, "int-prompt-u1", created.ID, &newName, &newContent)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", updated.Name)
	assert.Equal(t, "New body", updated.Content)

	require.NoError(t, svc.DeleteUserPrompt(ctx, "int-prompt-u1", created.ID))
	_, err = svc.UpdateUserPrompt(ctx, "int-prompt-u1", created.ID, &newName, nil)
	require.ErrorIs(t, err, ErrPromptNotFound, "deleted rows are gone")
}
