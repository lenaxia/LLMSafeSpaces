// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Board integration/kill-test harness (issue #1698, design 0067 §6).
//
// Runs against a real PostgreSQL (TEST_DATABASE_URL; skips when
// unreachable) because the invariants under test ARE Postgres arbitration:
// concurrent claims, guarded reclaims, deterministic event ids. A mock
// cannot prove atomicity — the kill-tests kill real racing transactions.
package board_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

const testUser = "board-test-user"

// fakeSpawner pins ruling R9 (fresh worker per granted claim).
type fakeSpawner struct {
	mu    sync.Mutex
	calls []string // one entry per spawn invocation
	err   error    // when set, every spawn fails
}

func (f *fakeSpawner) Spawn(_ context.Context, c *board.Claim) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c.ShortID)
	if f.err != nil {
		return "", f.err
	}
	return "sess-" + c.ShortID, nil
}

func (f *fakeSpawner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSpawner) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeAborter records worker aborts (#1747: delete replaces cancel;
// aborts fire post-commit). onAbort, when set, runs AT FIRE TIME —
// the hook the ordering pins use to observe the database while the
// abort is happening (the row must already be gone there).
type fakeAborter struct {
	mu      sync.Mutex
	calls   []string
	onAbort func(taskID, assignee string)
}

func (f *fakeAborter) Abort(_ context.Context, taskID, assignee string) error {
	f.mu.Lock()
	f.calls = append(f.calls, taskID+"/"+assignee)
	hook := f.onAbort
	f.mu.Unlock()
	if hook != nil {
		hook(taskID, assignee)
	}
	return nil
}

func (f *fakeAborter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAborter) callsSlice() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeDriftSource is the RC2 fixture host.
type fakeDriftSource struct {
	mu   sync.Mutex
	prs  map[int]board.PRState
	asks int
}

func (f *fakeDriftSource) GetPullRequest(_ context.Context, n int) (board.PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	if st, ok := f.prs[n]; ok {
		return st, nil
	}
	return board.PRState{}, fmt.Errorf("pr %d not found", n)
}

func (f *fakeDriftSource) set(n int, st board.PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[n] = st
}

func (f *fakeDriftSource) askCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asks
}

// fakePendingSource is the sessionstate pending-input projection fixture.
type fakePendingSource struct {
	mu   sync.Mutex
	pend map[string]bool
}

func (f *fakePendingSource) PendingInput(_ context.Context, assignee string) (board.PendingInputState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return board.PendingInputState{HasPendingInput: f.pend[assignee]}, nil
}

func (f *fakePendingSource) set(assignee string, pending bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pend[assignee] = pending
}

// failpoints is the crash-between-action-and-effect fault seam.
type failpoints struct {
	mu    sync.Mutex
	arm   map[string]error
	armFn map[string]func() error
	hits  map[string]int32
}

func newFailpoints() *failpoints {
	return &failpoints{arm: map[string]error{}, armFn: map[string]func() error{}, hits: map[string]int32{}}
}

func (f *failpoints) fp(where string) error {
	f.mu.Lock()
	f.hits[where]++
	err := f.arm[where]
	fn := f.armFn[where]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if fn != nil {
		return fn()
	}
	return nil
}

// armAtFn arms a functional failpoint (side-effect injections — the
// concurrency-race fixtures).
func (f *failpoints) armAtFn(where string, fn func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armFn[where] = fn
}

func (f *failpoints) armAt(where string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arm[where] = err
}

func (f *failpoints) disarm(where string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.arm, where)
}

// BoardSuite is the shared harness: a real PG, a Service with all seams
// faked, and seed helpers.
type BoardSuite struct {
	suite.Suite
	pool    *pgxpool.Pool
	cfg     board.EngineConfig
	spawner *fakeSpawner
	aborter *fakeAborter
	drift   *fakeDriftSource
	pending *fakePendingSource
	fps     *failpoints
	svc     *board.Service
	recon   *board.Reconciler
}

func TestBoardSuite(t *testing.T) {
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
	suite.Run(t, newBoardSuite(pool))
}

func newBoardSuite(pool *pgxpool.Pool) *BoardSuite {
	s := &BoardSuite{pool: pool}
	s.cfg = board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   200 * time.Millisecond, // short for expiry tests; lease arithmetic is still cfg-driven
		ThrottleBackoff: time.Hour,              // "long" so cooldowns gate until explicitly cleared
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	}
	return s
}

func (s *BoardSuite) SetupTest() {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects,
			board_config_defaults, board_project_config_overrides
		CASCADE
	`)
	s.Require().NoError(err)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ($1, $1, 'board-test@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`, testUser)
	s.Require().NoError(err, "users row must exist (FK target)")

	s.spawner = &fakeSpawner{}
	s.aborter = &fakeAborter{}
	s.drift = &fakeDriftSource{prs: map[int]board.PRState{}}
	s.pending = &fakePendingSource{pend: map[string]bool{}}
	s.fps = newFailpoints()
	s.svc = board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
	// The default test reconciler installs NO spawner: the §4.4 dispatch
	// drive is inert without a spawn seam (no lease churn), so RC-phase
	// tests observe reclaims in isolation. The drive itself is pinned by
	// TestKill_DispatchDriveResweepFeedsWork.
	s.recon = board.NewReconciler(s.pool, s.cfg,
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
}

// --- Seeds -------------------------------------------------------------------

// seedWorkspace inserts a fresh workspace row and returns its id.
func (s *BoardSuite) seedWorkspace() string {
	id := uuid.New().String()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		 VALUES ($1, $2, $3, 'default', now(), now())`, id, testUser, "ws-"+id[:8])
	s.Require().NoError(err)
	return id
}

func (s *BoardSuite) owner() board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceREST}
}

func (s *BoardSuite) agent(actorSession, token string) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgent, ClaimToken: token}
}

// seedProject creates an active project via the service (real path: mint,
// events) targeting the given workspace.
func (s *BoardSuite) seedProject(wsID string, slots int) *board.Project {
	if slots <= 0 {
		slots = 2
	}
	p, err := s.svc.CreateProject(context.Background(), s.owner(),
		board.ProjectInput{Name: "proj-" + wsID, TargetWorkspaceID: wsID, WorkerSlots: slots, Tags: []string{}}, "")
	s.Require().NoError(err)
	return p
}

// seedEpic creates a draft epic then sets the requested status directly.
func (s *BoardSuite) seedEpic(p *board.Project, status string) *board.Epic {
	e, err := s.svc.CreateEpic(context.Background(), s.owner(), p.ID, "",
		board.EpicInput{Title: "epic-" + p.ShortID, Outcome: "ship it"}, "")
	s.Require().NoError(err)
	if status != "" && status != board.EpicDraft {
		_, err := s.pool.Exec(context.Background(),
			`UPDATE epics SET status=$1 WHERE id=$2`, status, e.ID)
		s.Require().NoError(err)
	}
	return s.reloadEpic(e.ID)
}

// seedTask creates a backlog task via the service, optionally with pr:N tag
// and criteria pre-met for Done-guard fixtures.
func (s *BoardSuite) seedTask(e *board.Epic, title string, tags []string, criteriaMet bool) *board.Task {
	crit := []board.AcceptanceCriterion{{Text: "c1", Met: criteriaMet}, {Text: "c2", Met: criteriaMet}}
	if len(tags) == 0 {
		tags = []string{}
	}
	task, err := s.svc.CreateTask(context.Background(), s.owner(), e.ID, "",
		board.TaskInput{Title: title, Tags: tags, AcceptanceCriteria: crit}, "")
	s.Require().NoError(err)
	return task
}

// claimOrFail claims the next task for a synthetic worker in ws.
func (s *BoardSuite) claimOrFail(wsID string) *board.Claim {
	c, err := s.svc.ClaimNext(context.Background(), "worker-"+wsID, wsID)
	s.Require().NoError(err)
	s.Require().NotNil(c.Task)
	s.Require().False(c.Escalated, "test expected a grant, got escalation")
	return c
}

// expectNoCandidate asserts the empty-claim contract (ClaimNext's
// ErrNoCandidate — the r1-C5 error-class pin).
func (s *BoardSuite) expectNoCandidate(wsID string) {
	_, err := s.svc.ClaimNext(context.Background(), "worker-"+wsID, wsID)
	s.Require().ErrorIs(err, board.ErrNoCandidate)
}

// expireLease ages a task's lease past expiry (the clock seam: rather than
// sleeping lease windows, tests rewind lease_expires_at).
func (s *BoardSuite) expireLease(taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() - interval '1 millisecond' WHERE id=$1`, taskID)
	s.Require().NoError(err)
}

// clearCooldowns removes task + project cooldowns (test accelerator).
func (s *BoardSuite) clearCooldowns(p *board.Project, taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET cooldown_until=NULL WHERE id=$1`, taskID)
	s.Require().NoError(err)
	_, err = s.pool.Exec(context.Background(),
		`UPDATE projects SET slot_cooldown_until=NULL WHERE id=$1`, p.ID)
	s.Require().NoError(err)
}

func (s *BoardSuite) reloadTask(id string) *board.Task {
	t, err := s.svc.GetTask(context.Background(), id, "")
	s.Require().NoError(err)
	return t
}

func (s *BoardSuite) reloadEpic(id string) *board.Epic {
	e, err := s.svc.GetEpic(context.Background(), id, "")
	s.Require().NoError(err)
	return e
}

// taskRow reads raw columns the read model may not expose.
type taskRow struct {
	Stage         string
	PendingOrigin *string
	Assignee      *string
	ClaimToken    *string
	LeaseExp      *time.Time
	Attempts      int
	ClaimIncr     bool
	IsBlocked     bool
}

func (s *BoardSuite) rawTask(id string) taskRow {
	var r taskRow
	err := s.pool.QueryRow(context.Background(),
		`SELECT stage, pending_origin, assignee_session, claim_token::text, lease_expires_at,
		        attempts, claim_incremented_attempts, is_blocked
		 FROM tasks WHERE id=$1`, id).
		Scan(&r.Stage, &r.PendingOrigin, &r.Assignee, &r.ClaimToken, &r.LeaseExp, &r.Attempts, &r.ClaimIncr, &r.IsBlocked)
	s.Require().NoError(err)
	return r
}

func (s *BoardSuite) epicStatus(id string) string {
	var st string
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT status FROM epics WHERE id=$1`, id).Scan(&st))
	return st
}

func (s *BoardSuite) epicDRR(id string) (vft, deficit int64, quantum int) {
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT vft, deficit, quantum FROM epics WHERE id=$1`, id).Scan(&vft, &deficit, &quantum))
	return
}

func (s *BoardSuite) countEvents(action string) int {
	var n int
	q := `SELECT count(*) FROM board_events`
	args := []any{}
	if action != "" {
		q += ` WHERE action=$1`
		args = append(args, action)
	}
	s.Require().NoError(s.pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (s *BoardSuite) countTransitions(taskID, toStage string) int {
	var n int
	q := `SELECT count(*) FROM board_transitions WHERE task_id=$1`
	args := []any{taskID}
	if toStage != "" {
		q += ` AND to_stage=$2`
		args = append(args, toStage)
	}
	s.Require().NoError(s.pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func (s *BoardSuite) inProgressCount(projectID string) int {
	var n int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE project_id=$1 AND stage='in_progress'`, projectID).Scan(&n))
	return n
}

func (s *BoardSuite) sweep() board.SweepResult {
	res, err := s.recon.Sweep(context.Background())
	s.Require().NoError(err)
	return res
}
