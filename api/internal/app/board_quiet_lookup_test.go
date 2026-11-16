// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_quiet_lookup_test.go — the #2047 production seam's PG pins
// (review r2: the SQL join, the lease-state derivation, and the answers
// composition were otherwise exercised only through the evaluator-side
// fake — a broken filter fails silent, nil facts → arm inert).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

const (
	qlWorkspaceA = "11111111-2222-3333-4444-555555555501"
	qlWorkspaceB = "11111111-2222-3333-4444-555555555502"
	qlProjectA   = "22222222-3333-4444-5555-666666666601"
	qlProjectB   = "22222222-3333-4444-5555-666666666602"
	qlEpicA      = "33333333-4444-5555-6666-777777777701"
	qlEpicB      = "33333333-4444-5555-6666-777777777702"
)

func seedQuietLookupBoard(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('ql-u1', 'ql-u1', 'ql@example.test', 'h', true, 'user')
		ON CONFLICT (id) DO NOTHING;
		INSERT INTO workspaces (id, user_id, name, namespace)
		VALUES ('`+qlWorkspaceA+`', 'ql-u1', 'ql-ws', 'ql-ns'),
		       ('`+qlWorkspaceB+`', 'ql-u1', 'ql-ws-other', 'ql-ns-other')
		ON CONFLICT (id) DO NOTHING;
		INSERT INTO projects (id, short_id, user_id, name, target_workspace_id)
		VALUES ('`+qlProjectA+`', 'prj-ql-a', 'ql-u1', 'ql-p', '`+qlWorkspaceA+`'),
		       ('`+qlProjectB+`', 'prj-ql-b', 'ql-u1', 'ql-p-other', '`+qlWorkspaceB+`')
		ON CONFLICT (id) DO NOTHING;
		INSERT INTO epics (id, short_id, project_id, title, project_name, status)
		VALUES ('`+qlEpicA+`', 'epc-ql-a', '`+qlProjectA+`', 'E', 'ql-p', 'active'),
		       ('`+qlEpicB+`', 'epc-ql-b', '`+qlProjectB+`', 'E', 'ql-p-other', 'active')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err, "seed board FK chain")
}

// The integration pin: the fact set (scope, stage, pending_input,
// assignee filters), the lease-state derivation, and the liveness
// composition — plus the nil-on-degraded contract.
func TestIntegration_QuietLookup_InProgressAssignees(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM tasks WHERE short_id LIKE 'tsk-ql-%'`)
	require.NoError(t, err)
	seedQuietLookupBoard(t, pool)

	// Identity row for the FRAMED assignee (quiet known); ses_ql_b
	// stays unindexed (quiet unknown — the nil leg).
	seedLivenessSession(t, pool, "ws-quiet-lookup", "ses_ql_a", "ql-a-teapot-carries", 3, false)

	seed := `
		INSERT INTO tasks (id, short_id, project_id, epic_id, title, project_name, epic_name,
		                   stage, assignee_session, lease_expires_at, last_progress_at, pending_input)
		VALUES
		  -- the live-lease silent worker (the incident's shape)
		  ('44444444-5555-6666-7777-888888888801', 'tsk-ql-live', '` + qlProjectA + `', '` + qlEpicA + `',
		   'live', 'ql-p', 'E', 'in_progress', 'ses_ql_a',
		   now() + interval '30 minutes', now() - interval '45 minutes', false),
		  -- the lapsed-lease worker, unindexed (unknown quiet)
		  ('44444444-5555-6666-7777-888888888802', 'tsk-ql-lapsed', '` + qlProjectA + `', '` + qlEpicA + `',
		   'lapsed', 'ql-p', 'E', 'in_progress', 'ses_ql_b',
		   now() - interval '5 minutes', now() - interval '45 minutes', false),
		  -- pending_input: awaiting a HUMAN — excluded
		  ('44444444-5555-6666-7777-888888888803', 'tsk-ql-pending', '` + qlProjectA + `', '` + qlEpicA + `',
		   'pending', 'ql-p', 'E', 'in_progress', 'ses_ql_c',
		   now() + interval '30 minutes', now() - interval '45 minutes', true),
		  -- backlog: never a worker slot — excluded
		  ('44444444-5555-6666-7777-888888888804', 'tsk-ql-backlog', '` + qlProjectA + `', '` + qlEpicA + `',
		   'backlog', 'ql-p', 'E', 'backlog', 'ses_ql_d', NULL, NULL, false),
		  -- ANOTHER workspace's project — excluded by scoping
		  ('44444444-5555-6666-7777-888888888805', 'tsk-ql-other-ws', '` + qlProjectB + `', '` + qlEpicB + `',
		   'other', 'ql-p-other', 'E', 'in_progress', 'ses_ql_e',
		   now() + interval '30 minutes', now() - interval '45 minutes', false)`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed tasks: %v", err)
	}

	hub := newBoardLivenessHub(pool)
	tape := &tapeBusyFn{answers: map[string]bool{"ses_ql_a": true}}
	hub.SetBusySource(tape.busy)
	hub.ObserveFrame("ws-quiet-lookup", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_START,
		InternalSessionId: "ses_ql_a",
	})
	hub.mu.Lock()
	hub.framesWorkerAt["ses_ql_a"] = time.Now().Add(-45 * time.Minute)
	hub.mu.Unlock()

	lookup := newQuietBoardLookup(pool, hub)
	facts := lookup.InProgressAssignees(ctx, qlWorkspaceA)

	require.Len(t, facts, 2, "exactly the two in_progress assignees of THIS workspace's projects")
	byShort := map[string]handlers.QuietTaskFact{}
	for _, f := range facts {
		byShort[f.TaskShortID] = f
	}

	live := byShort["tsk-ql-live"]
	require.Equal(t, "ses_ql_a", live.AssigneeSession)
	assert.Equal(t, "live", live.LeaseState)
	assert.Equal(t, "prj-ql-a", live.ProjectID)
	require.NotNil(t, live.QuietSeconds, "the framed assignee's quiet is KNOWN")
	assert.GreaterOrEqual(t, *live.QuietSeconds, 45*60)
	assert.True(t, live.Busy, "the derived-busy truth composes into the fact")

	lapsed := byShort["tsk-ql-lapsed"]
	require.Equal(t, "ses_ql_b", lapsed.AssigneeSession)
	assert.Equal(t, "lapsed", lapsed.LeaseState)
	assert.Nil(t, lapsed.QuietSeconds, "the unindexed assignee's quiet is UNKNOWN (nil, never 0)")
	assert.False(t, lapsed.Busy)

	// The explicit-empty contract: a workspace whose projects hold no
	// claimed in_progress rows yields a NON-nil empty set (the
	// departure-delta signal).
	otherFacts := lookup.InProgressAssignees(ctx, qlWorkspaceB)
	require.NotNil(t, otherFacts)
	assert.Empty(t, otherFacts)

	// The nil contract: a degraded pool read returns nil (never a
	// fabricated empty set — the evaluator distinguishes the two).
	closedPool := livenessPool(t)
	closedPool.Close()
	require.Nil(t, newQuietBoardLookup(closedPool, hub).InProgressAssignees(ctx, qlWorkspaceA))
}
