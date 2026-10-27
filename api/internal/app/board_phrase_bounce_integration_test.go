// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_phrase_bounce_integration_test.go — the #1959c DECISIVE
// production-wiring pin (review r1 on PR #1967, F1 / missing-test 1):
// the PRODUCTION turn-evidence source (boardTurnEvidence — raw-keyed
// session_index) AND the PRODUCTION delivery closure
// (newBoardRedispatcher + boardResolvingSender + boardPhraseLookupFor)
// wired into a REAL board.Service, original implementer identified by
// the stored PHRASE. Under the pre-r1 seam-only fix this falls back at
// the turn-evidence arm ("never indexed = never turned") without the
// resolver ever running — the exact defeat the review pinned; the
// engine-side resolution (redispatchBounce resolves BEFORE the evidence
// consult) is the fix under test. Born-RED evidence for this file is
// CI-side by construction (no local Postgres in the lane environment;
// the expectation is recorded verbatim in the worklog).
//
// Also carries the boardPhraseLookupFor DB pins (missing-test 2): the
// PRODUCTION registry binding — happy resolve, the pgx ErrNoRows miss
// translated to the documented ("", nil) contract, and the
// wrong-workspace phrase that must not resolve.

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

const (
	integTestUser = "user-phrase-bounce-1959c"
	integPhrase   = "florid-teapot-verifies"
	integRawID    = "ses_phrase_bounce_raw_1959c"
)

func phraseBouncePool(t *testing.T) *pgxpool.Pool {
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
	t.Cleanup(pool.Close)
	return pool
}

// recordingSpawner captures spawns (the fallback arm's observable).
type recordingSpawner struct {
	mu     sync.Mutex
	claims []*board.Claim
}

func (r *recordingSpawner) Spawn(_ context.Context, c *board.Claim) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *c
	r.claims = append(r.claims, &cp)
	return "ses-spawned-" + c.ShortID, nil
}

func (r *recordingSpawner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.claims)
}

// TestIntegrationBoardPhraseBounceProductionWiring — the full production
// chain over a real schema: fail verdict → bounce mint → engine-side
// phrase resolution → raw-keyed turn evidence (PASSES on the resolved
// raw id) → V1 re-dispatch POST landing on the original implementer's
// LIVE raw session. No fresh spawn fires.
func TestIntegrationBoardPhraseBounceProductionWiring(t *testing.T) {
	pool := phraseBouncePool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1, $1, $2, 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`, integTestUser, integTestUser+"@example.com")
	require.NoError(t, err)

	wsID := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, 'default', now(), now())`, wsID, integTestUser, "ws-"+wsID[:8])
	require.NoError(t, err)

	// The registry binding: the original implementer's LIVE raw session,
	// turned twice, phrase-minted (the design-0066 registration shape).
	_, err = pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, short_id, message_count)
		VALUES ($1::uuid, $2, $3, 2)`, wsID, integRawID, integPhrase)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM session_index WHERE workspace_id=$1::uuid AND session_id=$2`, wsID, integRawID)
	})

	// The stub opencode knows ONLY the raw id (phrase ids 404 — the
	// real-shape semantics the unit pins already carry).
	stub := newStubRealOpencode(t, integRawID)
	adapter := newBoardCommsAdapter(t, stub.Server)

	spawner := &recordingSpawner{}
	sender := newBoardResolvingSender(adapter, boardPhraseLookupFor(pool))
	owner := board.Actor{Kind: board.ActorUser, UserID: integTestUser, Surface: board.SurfaceREST}
	svc := board.NewService(pool, board.DefaultEngineConfig(),
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithBounceRedispatcher(newBoardRedispatcher(boardScopeFor(pool), sender)),
		board.WithTurnEvidenceSource(&boardTurnEvidence{pool}),
	)

	p, err := svc.CreateProject(ctx, owner, board.ProjectInput{
		Name: "proj-phrase-bounce", TargetWorkspaceID: wsID, WorkerSlots: 2, Tags: []string{},
	}, "")
	require.NoError(t, err)
	personas := map[string]board.PersonaConfig{"in_review": {Name: "reviewer"}}
	_, err = svc.SetProjectBoardConfig(ctx, owner, p.ID, "", &board.BoardConfigOverride{StagePersonas: &personas})
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "epic-phrase-bounce", Outcome: "ship it"}, "")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE epics SET status='active' WHERE id=$1`, e.ID)
	require.NoError(t, err)
	task, err := svc.CreateTask(ctx, owner, e.ID, "", board.TaskInput{
		Title: "phrase-bounce", Description: "seeded charter", Tags: []string{},
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}, {Text: "c2"}},
	}, "")
	require.NoError(t, err)

	claim, err := svc.ClaimNext(ctx, "worker-"+wsID, wsID)
	require.NoError(t, err)
	require.NotNil(t, claim)
	implementer := claim.Assignee // the sync spawn-stamp landed
	require.NotEmpty(t, implementer)

	// Forward exit → in_review: the reviewer persona claim mints in the
	// same transaction; its token is the row's claim_token.
	_, err = svc.MoveStage(ctx, board.Actor{Kind: board.ActorUser, UserID: integTestUser, Surface: board.SurfaceAgent, ClaimToken: claim.ClaimToken},
		"", task.ShortID, board.StageInReview, "file:///ws/phrase-bounce.txt", "")
	require.NoError(t, err)
	var reviewerToken string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT claim_token::text FROM tasks WHERE id=$1`, task.ID).Scan(&reviewerToken))
	require.NotEmpty(t, reviewerToken)

	// The stored implementer identity takes the PHRASE form (the live
	// 0.44.3 indictment — the row HOLDS a phrase).
	_, err = pool.Exec(ctx, `UPDATE tasks SET implementer_session=$2 WHERE id=$1`, task.ID, integPhrase)
	require.NoError(t, err)
	spawnsBefore := spawner.count()

	// The fail verdict fires the bounce through the PRODUCTION chain.
	_, v, err := svc.RecordVerdict(ctx, board.Actor{Kind: board.ActorUser, UserID: integTestUser, Surface: board.SurfaceAgent, ClaimToken: reviewerToken},
		"", task.ShortID, board.VerdictInput{
			Verdict:  board.VerdictFail,
			Reviewer: "rev-1959c-wiring",
			Findings: []board.VerdictFinding{{Severity: board.SeverityMajor, Pointer: "criterion 0: no evidence"}},
		}, "")
	require.NoError(t, err)
	require.True(t, v.Applied, "the fail verdict must fire the bounce")

	posted, _, texts, _ := stub.snapshot()
	require.Len(t, posted, 1,
		"exactly one V1 re-dispatch POST — the production chain delivered through the resolving seam")
	assert.Equal(t, integRawID, posted[0],
		"the POST must land on the ORIGINAL implementer's RAW id — the engine resolved the phrase upstream of the evidence consult and the delivery")
	require.Len(t, texts, 1)
	assert.Contains(t, texts[0], "BOARD BOUNCE RE-DISPATCH")
	assert.Contains(t, texts[0], task.ShortID)
	assert.Equal(t, spawnsBefore, spawner.count(),
		"no fresh spawn — the phrase-addressed original is LIVE and reached")
}

// TestIntegrationBoardPhraseLookupBinding — the PRODUCTION registry
// binding's DB pins (missing-test 2): happy resolve; the pgx ErrNoRows
// miss translated to the documented ("", nil) contract; the
// wrong-workspace phrase that must not resolve; and the wrapper's miss
// branch — the liveness error naming the phrase.
func TestIntegrationBoardPhraseLookupBinding(t *testing.T) {
	pool := phraseBouncePool(t)
	ctx := context.Background()
	// Self-sufficient on a fresh database (review r2's blocking finding):
	// this pin must not rely on the wiring test having inserted the user.
	_, err := pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1, $1, $1, 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`, integTestUser)
	require.NoError(t, err)
	wsID := uuid.NewString()
	otherWS := uuid.NewString()
	for _, w := range []string{wsID, otherWS} {
		_, err := pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1::uuid, $2, $3, 'default', now(), now())`, w, integTestUser, "ws-"+w[:8])
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, short_id, message_count)
		VALUES ($1::uuid, $2, $3, 1)`, wsID, integRawID, integPhrase)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM session_index WHERE session_id=$1`, integRawID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM workspaces WHERE id::text = ANY($1)`, []string{wsID, otherWS})
	})

	lookup := boardPhraseLookupFor(pool)

	raw, err := lookup(ctx, wsID, integPhrase)
	require.NoError(t, err)
	assert.Equal(t, integRawID, raw, "happy resolve: the registry maps phrase→raw")

	raw, err = lookup(ctx, wsID, "Florid-Teapot-Verifies")
	require.NoError(t, err)
	assert.Equal(t, integRawID, raw, "the consult is case-insensitive (shortid.Normalize)")

	miss, err := lookup(ctx, wsID, "reaped-lantern-hushes")
	require.NoError(t, err, "a pgx ErrNoRows miss translates to the documented (\"\", nil) contract — never a wrapped no-rows error")
	assert.Empty(t, miss)

	cross, err := lookup(ctx, otherWS, integPhrase)
	require.NoError(t, err)
	assert.Empty(t, cross, "the registry is workspace-scoped: another workspace's phrase must not resolve")

	// The wrapper's miss branch over the production binding: the
	// liveness error that drives pkg/board's fresh-worker fallback.
	sender := newBoardResolvingSender(nil, lookup)
	_, err = sender.ResolveSessionRef(ctx, otherWS, integPhrase)
	require.Error(t, err)
	assert.Contains(t, err.Error(), integPhrase, "the miss names the phrase (the liveness answer's provenance)")
	assert.Contains(t, err.Error(), "no live session")

	if raw, err := sender.ResolveSessionRef(ctx, wsID, integPhrase); err != nil {
		t.Fatalf("happy resolve through the wrapper: %v", err)
	} else if raw != integRawID {
		t.Fatalf("happy resolve through the wrapper: got %q", raw)
	}
}
