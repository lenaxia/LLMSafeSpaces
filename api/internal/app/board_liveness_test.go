// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_liveness_test.go — the seamless-lease opencode adapter's pins.
//
// Unit legs (no PG): the sentinel message contract (the agentmessage-v1
// grammar's additive lease-nudge keys + the surgical prose — owner
// ruling: "do NOT restart; assess; advance or declare failed"), the
// frame-index/nudge-mark arithmetic, and the nudger's delivery through
// the V1 sender seam with the send-mark recorded BEFORE delivery.
//
// Integration legs (PG-gated, the phrase-bounce pattern): the
// session_index honesty mapping (dead-evidence vs no-evidence), the DB
// movement delta, the ordinal answer's two legs (frames after the
// send-mark; message_count above the at-send snapshot), and the
// DECISIVE production-wiring pin — a real board.Reconciler + the
// production hub where a TOOL-PART START frame keeps a claimed,
// lease-expired row in_progress (the live incident's exact shape: a
// worker mid-command longer than the lease window does not lapse).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// --- Unit: the sentinel message ---------------------------------------------

func TestLeaseNudgeText_SentinelGrammarAndProse(t *testing.T) {
	p := board.NudgePayload{NudgeID: "nudge-1234", TaskID: "task-uuid", TaskShortID: "tsk-quiet-worker"}
	text := leaseNudgeText(p.TaskShortID, p)

	lines := strings.SplitN(text, "\n", 2)
	require.Len(t, lines, 2, "sentinel line + prose")

	// The v1 marker line: prefix, one-line JSON, suffix.
	require.True(t, strings.HasPrefix(lines[0], "<!-- lsp:agent-message-v1 {"), "v1 prefix")
	require.True(t, strings.HasSuffix(lines[0], " -->"), "v1 suffix")
	payload := strings.TrimSuffix(strings.TrimPrefix(lines[0], "<!-- lsp:agent-message-v1 "), " -->")
	var sent leaseNudgeSentinel
	require.NoError(t, json.Unmarshal([]byte(payload), &sent))
	assert.Equal(t, "lease-nudge", sent.Mode)
	assert.Equal(t, "tsk-quiet-worker", sent.TaskID)
	assert.Equal(t, "nudge-1234", sent.NudgeID)
	assert.Equal(t, "advance|fail", sent.Expects)

	// The surgical prose (owner ruling, verbatim intent).
	assert.Contains(t, lines[1], "Do NOT restart")
	assert.Contains(t, lines[1], "declare it failed")
	assert.Contains(t, lines[1], "tsk-quiet-worker", "the prose names the task")
	assert.Contains(t, lines[1], "renews the lease", "the worker is told any response counts")
}

// --- Unit: the frame index + nudge marks (no PG) ------------------------------

func TestLeaseLivenessHub_FrameIndexNoPoolIsNoEvidence(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.ObserveFrame("ws", &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START,
		// internal = the raw harness id the board stores as assignee
		SessionId:         "florid-teapot-verifies",
		InternalSessionId: "ses_raw_worker_1",
	})
	res := hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_raw_worker_1"}}})
	o, ok := res["ses_raw_worker_1"]
	require.True(t, ok)
	assert.False(t, o.Observed, "no pool → no-evidence for every ref (never death)")
	assert.False(t, o.MovedSinceLastSweep)
	assert.False(t, o.Exists)
}

func TestLeaseLivenessHub_NilObserverAndEmptyBatch(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.ObserveFrame("ws", nil) // must not panic
	res := hub.ObserveAssignees(board.SessionObservations{})
	assert.Empty(t, res)
}

// --- Unit: the nudger (fake V1 sender) ---------------------------------------

type recordingV1Sender struct {
	mu    sync.Mutex
	sent  []recordedSend
	resps []error // one per send, cycling when shorter
}

type recordedSend struct {
	userID, workspaceID, sessionID, text string
}

func (f *recordingV1Sender) ResolveSessionRef(_ context.Context, _, ref string) (string, error) {
	return ref, nil // raw passthrough (the comms pins cover resolution)
}

func (f *recordingV1Sender) Send(_ context.Context, userID, workspaceID, sessionID, text string, _ session.SendOpts) (*session.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, recordedSend{userID, workspaceID, sessionID, text})
	if len(f.resps) > 0 {
		err := f.resps[len(f.sent)%len(f.resps)-1]
		if err != nil {
			return nil, err
		}
	}
	return &session.Message{}, nil
}

func (f *recordingV1Sender) Abort(context.Context, string, string, string) error { return nil }

func (f *recordingV1Sender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func TestLeaseNudger_DeliversSentinelAndMarksBeforeDelivery(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	sender := &recordingV1Sender{}
	scope := func(context.Context, string) (string, string, error) { return "user-1", "ws-1", nil }
	nudger := newBoardNudger(scope, sender, hub)

	p := board.NudgePayload{NudgeID: "n-1", TaskID: "task-9", TaskShortID: "tsk-x"}
	err := nudger(context.Background(), "ses_worker", p)
	require.NoError(t, err)

	require.Equal(t, 1, sender.count())
	s := sender.sent[0]
	assert.Equal(t, "user-1", s.userID, "scope resolution feeds the adapter addressing")
	assert.Equal(t, "ws-1", s.workspaceID)
	assert.Equal(t, "ses_worker", s.sessionID)
	assert.Contains(t, s.text, `"nudgeId":"n-1"`, "the delivered message carries the ordinal anchor")

	// The send-mark exists the moment delivery has been attempted —
	// the observe path can answer the ordinal question for this nudge.
	hub.mu.Lock()
	_, marked := hub.nudges["n-1"]
	hub.mu.Unlock()
	assert.True(t, marked, "the send-mark is recorded before delivery")
}

func TestLeaseNudger_ScopeErrorIsTheSeamError(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	sender := &recordingV1Sender{}
	scope := func(context.Context, string) (string, string, error) { return "", "", context.DeadlineExceeded }
	nudger := newBoardNudger(scope, sender, hub)
	err := nudger(context.Background(), "ses_worker", board.NudgePayload{NudgeID: "n-2", TaskID: "t"})
	require.Error(t, err, "a nudge that could not be attempted reports it (the engine's wall-clock fall-back keys on this)")
	assert.Zero(t, sender.count())
}

// --- Integration (PG): the honesty mapping + deltas ---------------------------

func livenessPool(t *testing.T) *pgxpool.Pool {
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

func seedLivenessSession(t *testing.T, pool *pgxpool.Pool, workspace, raw, phrase string, count int, deleted bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO session_index (workspace_id, session_id, short_id, message_count, updated_at, deleted_at)
		VALUES ($1, $2, NULLIF($3,''), $4, now(), CASE WHEN $5 THEN now() ELSE NULL END)
		ON CONFLICT (workspace_id, session_id) DO UPDATE SET
		  message_count = EXCLUDED.message_count, deleted_at = EXCLUDED.deleted_at`,
		workspace, raw, phrase, count, deleted)
	require.NoError(t, err)
}

func TestIntegrationLeaseLiveness_HonestyMappingAndDeltas(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM session_index WHERE workspace_id = 'ws-lease-liveness'`)
	require.NoError(t, err)

	hub := newBoardLivenessHub(pool)
	seedLivenessSession(t, pool, "ws-lease-liveness", "ses_alive", "alive-teapot-verifies", 3, false)
	seedLivenessSession(t, pool, "ws-lease-liveness", "ses_dead", "dead-teapot-verifies", 1, true)

	res := hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{
		{SessionID: "ses_alive"},
		{SessionID: "ses_dead"},
		{SessionID: "ses_never_indexed"},
	}})

	alive := res["ses_alive"]
	assert.True(t, alive.Observed)
	assert.True(t, alive.Exists)
	assert.False(t, alive.MovedSinceLastSweep, "first observation: the DB baseline starts now")

	deadObs := res["ses_dead"]
	assert.True(t, deadObs.Observed)
	assert.False(t, deadObs.Exists, "a soft-deleted row is dead-evidence")
	assert.False(t, deadObs.MovedSinceLastSweep)

	absent := res["ses_never_indexed"]
	assert.False(t, absent.Observed, "an unindexed session is NO-evidence, never death")

	// The DB delta leg: a message lands between sweeps → movement.
	seedLivenessSession(t, pool, "ws-lease-liveness", "ses_alive", "alive-teapot-verifies", 4, false)
	res = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_alive"}}})
	assert.True(t, res["ses_alive"].MovedSinceLastSweep, "message_count delta is renewal evidence")

	// The frame leg beats the DB: no DB change, but a tool-part frame.
	hub.ObserveFrame("ws-lease-liveness", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_START,
		InternalSessionId: "ses_alive",
	})
	res = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_alive"}}})
	assert.True(t, res["ses_alive"].MovedSinceLastSweep, "a PART_START frame alone is renewal evidence (tool-call START)")
}

func TestIntegrationLeaseLiveness_OrdinalAnswerBothLegs(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM session_index WHERE workspace_id = 'ws-lease-liveness-ordinal'`)
	require.NoError(t, err)

	hub := newBoardLivenessHub(pool)
	seedLivenessSession(t, pool, "ws-lease-liveness-ordinal", "ses_quiet", "quiet-teapot-verifies", 2, false)
	// Baseline the cursor (first observation is the baseline).
	_ = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_quiet"}}})

	// The sentinel goes out; its send-mark snapshots the DB state.
	hub.markNudge(ctx, "nudge-A", "ses_quiet")

	// Leg 1 — frames after the mark: the worker answered anything.
	hub.ObserveFrame("ws-lease-liveness-ordinal", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_MESSAGE_START,
		InternalSessionId: "ses_quiet",
	})
	res := hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{
		{SessionID: "ses_quiet", NudgeID: "nudge-A"},
	}})
	assert.True(t, res["ses_quiet"].EventsAfterNudge, "a frame received after the send-mark is post-sentinel activity")

	// A FRESH episode with no post-sentinel anything: the wedge shape.
	seedLivenessSession(t, pool, "ws-lease-liveness-ordinal", "ses_wedge", "wedge-teapot-verifies", 5, false)
	_ = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_wedge"}}})
	hub.markNudge(ctx, "nudge-B", "ses_wedge")
	res = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{
		{SessionID: "ses_wedge", NudgeID: "nudge-B"},
	}})
	assert.False(t, res["ses_wedge"].EventsAfterNudge, "silence after a positioned sentinel (the wedge verdict's input)")

	// Leg 2 — the DB backstop: a message landed after the send snapshot
	// (no frames were ever seen for this session — the stream-less pod).
	seedLivenessSession(t, pool, "ws-lease-liveness-ordinal", "ses_wedge", "wedge-teapot-verifies", 6, false)
	res = hub.ObserveAssignees(board.SessionObservations{Refs: []board.AssigneeRef{
		{SessionID: "ses_wedge", NudgeID: "nudge-B"},
	}})
	assert.True(t, res["ses_wedge"].EventsAfterNudge, "message_count above the at-send snapshot is post-sentinel activity")
}

// TestIntegrationLeaseLiveness_ProductionWiring_ToolStartFrameRenews — the
// DECISIVE wiring pin (the acceptance criterion): a real board engine +
// the production hub over a real schema, where a claimed row whose lease
// has EXPIRED stays in_progress because the usagestream observer stamped
// a TOOL-PART START frame — the live incident's exact shape (a worker
// mid-command longer than the lease window does not lapse).
func TestIntegrationLeaseLiveness_ProductionWiring_ToolStartFrameRenews(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('lease-liveness-user', 'lease-liveness-user', 'll@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO board_config_defaults (user_id, key, value)
		VALUES ('lease-liveness-user', 'stagePersonas', '{}'::jsonb)
		ON CONFLICT (user_id, key) DO NOTHING
	`)
	require.NoError(t, err)

	hub := newBoardLivenessHub(pool)
	cfg := board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   200 * time.Millisecond,
		ThrottleBackoff: time.Hour,
		FailureBackoff:  500 * time.Millisecond,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	}
	svc := board.NewService(pool, cfg,
		board.WithLivenessSource(hub),
		board.WithNudgeSender(func(context.Context, string, board.NudgePayload) error {
			return nil // never expected: a moving worker is never nudged
		}),
	)
	owner := board.Actor{Kind: board.ActorUser, UserID: "lease-liveness-user", Surface: board.SurfaceREST}

	ws := uuid.New().String()
	_, err = pool.Exec(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'lease-liveness-user', $2, 'default', now(), now())`, ws, "ws-"+ws[:8])
	require.NoError(t, err)
	p, err := svc.CreateProject(ctx, owner, board.ProjectInput{
		Name: "proj-lease-liveness", TargetWorkspaceID: ws, WorkerSlots: 2, Tags: []string{},
	}, "")
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "ep", Outcome: "ship"}, "")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE epics SET status='active' WHERE id=$1`, e.ID)
	require.NoError(t, err)
	task, err := svc.CreateTask(ctx, owner, e.ID, "", board.TaskInput{
		Title: "mid-command", Description: "charter", Tags: []string{},
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}},
	}, "")
	require.NoError(t, err)

	c, err := svc.ClaimNext(ctx, "worker-lease", ws)
	require.NoError(t, err)
	require.Equal(t, task.ID, c.TaskID)
	assignee := "ses_midcmd_" + strings.ReplaceAll(task.ShortID, "-", "_")
	_, err = pool.Exec(ctx, `
		UPDATE tasks SET assignee_session=$2 WHERE id=$1`, task.ID, assignee)
	require.NoError(t, err)
	seedLivenessSession(t, pool, ws, assignee, "", 1, false)

	recon := board.NewReconciler(pool, cfg,
		board.WithLivenessSource(hub),
		board.WithNudgeSender(func(context.Context, string, board.NudgePayload) error {
			t.Fatal("a moving worker must never be nudged")
			return nil
		}),
	)

	// The mid-command shape, three lease windows long: each window the
	// worker's tool produces SOMETHING (a part start, a delta), and
	// each expiry the sweep must renew on that evidence.
	for round := 0; round < 3; round++ {
		_, err = pool.Exec(ctx,
			`UPDATE tasks SET lease_expires_at = now() - interval '1 millisecond' WHERE id=$1`, task.ID)
		require.NoError(t, err)
		hub.ObserveFrame(ws, &abiv1.Event{
			Type:              abiv1.EventType_EVENT_TYPE_PART_START,
			InternalSessionId: assignee,
			SessionId:         "midcmd-teapot-verifies",
		})
		_, err = recon.Sweep(ctx)
		require.NoError(t, err)

		var stage string
		var token *string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT stage, claim_token::text FROM tasks WHERE id=$1`, task.ID).Scan(&stage, &token))
		assert.Equal(t, board.StageInProgress, stage, "round %d: the mid-command worker does not lapse", round)
		require.NotNil(t, token)
	}
}
