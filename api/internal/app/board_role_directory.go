// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_role_directory.go — the agent-role vocabulary seam's production
// concrete (owner ruling 2026-10-29: "The column personas are wrong. I
// was thinking we can use the agent roles the platform already has.
// Rather than redefining them that way they can be reused."). The board
// Service consumes it through board.RoleDirectory: stage personas
// REFERENCE platform agent roles (id or slug) — the role is the single
// source of truth for model/tools/system-prompt — and set-config
// validates role references against this set. The round-2 ruling
// (2026-10-29: "Board stages should ONLY ever have roles configured,
// there is no custom definition in the board itself.") moves the last
// board-owned behavior — the default reviewer's verdict-channel
// charter — INTO the built-in reviewer agent role this file seeds:
// role seeding, never a persona migration.

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// boardRoleStore is the role-directory's narrow store seam: the
// PgOrgStore subset the directory reads and the seeding writes
// (satisfied structurally by *database.PgOrgStore; fakes pin the seam
// without a DB).
type boardRoleStore interface {
	ListAgentRoles(ctx context.Context, scope string, orgID string) ([]*types.AgentRole, error)
	CreateAgentRole(ctx context.Context, role *types.AgentRole, configJSON []byte) (*types.AgentRole, error)
	GetUserOrgID(ctx context.Context, userID string) (string, error)
}

// newBoardRoleDirectory renders one user's selectable vocabulary:
// platform roles plus their org's when they belong to one (the same
// composition the member-readable GET /api/v1/auth/me/agent-roles renders for the
// persona editor's dropdown — ONE vocabulary, two surfaces).
func newBoardRoleDirectory(store boardRoleStore) board.RoleDirectory {
	return &boardRoleDirectory{store: store}
}

type boardRoleDirectory struct {
	store boardRoleStore
}

func (d *boardRoleDirectory) AgentRolesForUser(ctx context.Context, userID string) ([]board.AgentRoleRef, error) {
	if d == nil || d.store == nil || userID == "" {
		return nil, nil
	}
	out := make([]board.AgentRoleRef, 0, 8)
	appendScope := func(scope, orgID string) error {
		roles, err := d.store.ListAgentRoles(ctx, scope, orgID)
		if err != nil {
			return err
		}
		for _, r := range roles {
			if r == nil {
				continue
			}
			out = append(out, board.AgentRoleRef{ID: r.ID, Slug: r.Slug, Name: r.Name, Scope: r.Scope})
		}
		return nil
	}
	if err := appendScope("platform", ""); err != nil {
		return nil, err
	}
	orgID, err := d.store.GetUserOrgID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if orgID != "" {
		if err := appendScope("org", orgID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// seedBuiltinReviewerRole seeds the built-in `reviewer` platform agent
// role when the vocabulary lacks one — the platform's startup half of
// the role-only ruling (2026-10-29): the default reviewer's
// verdict-channel charter (board_record_verdict, the criteria walk,
// the Done guard) IS behavior, so it lives in the ROLE's system prompt
// (board.DefaultReviewerRoleSystemPrompt, the single source), never in
// a board-layer persona override. With the role seeded, the default
// reviewer composition (board.DefaultReviewerPersona, the slug
// reference) resolves to a role deterministically and every spawned
// reviewer runs on it; an org/user overriding the reviewer points
// in_review at their own role instead. Idempotent: an existing role
// with the reviewer slug (any scope — an org may own their own) makes
// the seed a no-op. This is ROLE SEEDING, not a persona migration —
// no persona data is read, transformed, or written.
func seedBuiltinReviewerRole(ctx context.Context, store boardRoleStore) error {
	roles, err := store.ListAgentRoles(ctx, "platform", "")
	if err != nil {
		return err
	}
	for _, r := range roles {
		if r != nil && r.Slug == board.DefaultReviewerRoleSlug {
			return nil // already present — the seed is idempotent
		}
	}
	system := board.DefaultReviewerRoleSystemPrompt
	configJSON, err := types.MarshalRoleConfig(&types.RoleConfig{
		Version: types.RoleConfigVersion,
		System:  &system,
	})
	if err != nil {
		return err
	}
	_, err = store.CreateAgentRole(ctx, &types.AgentRole{
		Scope:       "platform",
		Name:        "Reviewer",
		Slug:        board.DefaultReviewerRoleSlug,
		Description: "Built-in board reviewer — the default reviewer's verdict-channel charter (board_record_verdict, the criteria walk, the Done guard) as its system prompt. Seeded by the platform; point stagePersonas.in_review here or at your own role.",
		IsDefault:   false,
	}, configJSON)
	return err
}
