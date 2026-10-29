// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_role_directory.go — the agent-role vocabulary seam's production
// concrete (owner ruling 2026-10-29: "The column personas are wrong. I
// was thinking we can use the agent roles the platform already has.
// Rather than redefining them that way they can be reused."). The board
// Service consumes it through board.RoleDirectory: stage personas
// REFERENCE platform agent roles (id or slug) instead of redefining
// model/tools/system-prompt per persona, set-config validates role
// references against this set, reads lazily migrate legacy name-only
// personas onto resolvable roles, and the default reviewer composition
// points at the reviewer role when one exists.

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// boardRoleStore is the role-directory's narrow store seam: the
// PgOrgStore subset the directory reads (satisfied structurally by
// *database.PgOrgStore; fakes pin the seam without a DB).
type boardRoleStore interface {
	ListAgentRoles(ctx context.Context, scope string, orgID string) ([]*types.AgentRole, error)
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
