// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// In-package PG pins for wrapUpReviewerForProject (epic wrap-up review,
// low-level design §6): the mint/spawn-side persona resolution the
// wrapup-engine story consumes. In-package because the seam under test
// is unexported — its three consumers (mint, reconciler re-claim,
// spawnFor) are sibling stories; these pins land the contract first.
package board

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/google/uuid"
)

// wrapupReviewerPool mirrors the harness's own DSN contract.
func wrapupReviewerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@localhost:5433/llmsafespaces_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("Skipping PG integration test: %v", err)
	}
	return pool
}

// TestWrapUpReviewerForProject — the resolution ladder, in order:
// (1) NO config anywhere → the DEFAULT persona (the seeded
// wrapup-reviewer slug), canonicalized to the role's id by
// applyPersonaRole; (2) an explicit config at ANY tier wins over the
// default; (3) the project tier wins over the user tier; (4) an
// unresolvable reference (role deleted after storage — the delete
// path has no wrapUpReviewer cleanup) DEGRADES to nil: never a
// role-less spawn, the re-arm heals once the vocabulary carries the
// role again; (5) a drifted stored value (read fails closed) degrades
// to nil the same way.
func TestWrapUpReviewerForProject(t *testing.T) {
	pool := wrapupReviewerPool(t)
	defer pool.Close()
	ctx := context.Background()

	userID := "board-test-user"
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1, $1, 'board-test@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING`, userID)
	require.NoError(t, err)

	cleanup := func() {
		_, _ = pool.Exec(ctx, `TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects,
			board_config_defaults, board_project_config_overrides CASCADE`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// setUserWrapUpRaw writes the user-tier wrapUpReviewer value directly
	// (raw SQL — bypasses the set belts so drift shapes can land too).
	setUserWrapUpRaw := func(t *testing.T, value string) {
		t.Helper()
		_, err := pool.Exec(ctx,
			`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'wrapUpReviewer', $2::jsonb)
			 ON CONFLICT (user_id, key) DO UPDATE SET value = EXCLUDED.value`, userID, value)
		require.NoError(t, err)
	}

	seedProjectRow := func(t *testing.T) *Project {
		t.Helper()
		wsID := uuid.New().String()
		_, err := pool.Exec(ctx,
			`INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			 VALUES ($1, $2, $3, 'default', now(), now())`, wsID, userID, "ws-"+wsID[:8])
		require.NoError(t, err)
		svc := NewService(pool, EngineConfig{})
		p, err := svc.CreateProject(ctx, Actor{Kind: ActorUser, UserID: userID, Surface: SurfaceREST},
			ProjectInput{Name: "proj-" + wsID, TargetWorkspaceID: wsID, WorkerSlots: 2, Tags: []string{}}, "")
		require.NoError(t, err)
		return p
	}

	// The healthy directory carries the seeded wrap-up reviewer plus
	// one custom org role — the vocabulary shape after startup seeding.
	healthy := &wrapupDir{roles: []AgentRoleRef{wrapupRoleFix, customRoleFix}}
	svc := NewService(pool, EngineConfig{}, WithRoleDirectory(healthy))

	t.Run("default resolves and canonicalizes", func(t *testing.T) {
		p := seedProjectRow(t)
		got := svc.wrapUpReviewerForProject(ctx, p)
		require.NotNil(t, got, "no config anywhere renders the default persona — never nil while the role resolves")
		assert.Equal(t, wrapupRoleFix.ID, got.RoleID,
			"the default persona's slug reference canonicalizes to the role id (the id rides the dispatch)")
	})

	t.Run("explicit config wins over the default", func(t *testing.T) {
		p := seedProjectRow(t)
		setUserWrapUpRaw(t, `"custom"`)
		got := svc.wrapUpReviewerForProject(ctx, p)
		require.NotNil(t, got)
		assert.Equal(t, customRoleFix.ID, got.RoleID, "an explicit user default replaces the default persona")
	})

	t.Run("project tier wins over the user tier", func(t *testing.T) {
		p := seedProjectRow(t)
		_, err := pool.Exec(ctx,
			`INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, 'wrapUpReviewer', '"wrapup-reviewer"'::jsonb)`, p.ID)
		require.NoError(t, err)
		got := svc.wrapUpReviewerForProject(ctx, p)
		require.NotNil(t, got)
		assert.Equal(t, wrapupRoleFix.ID, got.RoleID, "the project override (slug form) wins and canonicalizes")
	})

	t.Run("unresolvable degrades to nil never role-less", func(t *testing.T) {
		p := seedProjectRow(t)
		// Stored while the vocabulary carried the role; the role was
		// deleted after storage (no cleanup seam).
		setUserWrapUpRaw(t, `"custom"`)
		narrow := NewService(pool, EngineConfig{}, WithRoleDirectory(&wrapupDir{roles: []AgentRoleRef{wrapupRoleFix}}))
		assert.Nil(t, narrow.wrapUpReviewerForProject(ctx, p),
			"an unresolvable reference degrades to nil — never a role-less spawn; the re-arm heals it")

		// And it HEALS: the same row resolves again once the vocabulary
		// carries the role (the re-arm/re-seed shape).
		got := svc.wrapUpReviewerForProject(ctx, p)
		require.NotNil(t, got)
		assert.Equal(t, customRoleFix.ID, got.RoleID)
	})

	t.Run("stored drift degrades to nil", func(t *testing.T) {
		p := seedProjectRow(t)
		// A drifted value past the belts (manual SQL): the read fails
		// closed (StorageDriftError) and resolution degrades to nil —
		// the same never-a-role-less-spawn posture.
		setUserWrapUpRaw(t, `17`)
		assert.Nil(t, svc.wrapUpReviewerForProject(ctx, p))
	})
}
