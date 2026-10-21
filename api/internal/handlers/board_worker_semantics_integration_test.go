// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_worker_semantics_integration_test.go — #1701: the claim-token
// semantics pinned END-TO-END through the agent surface — the REAL
// BoardService (Postgres arbitration, no fakes on the action path)
// behind the REAL pod-identity seam (resolve: TokenReview → SA
// principal → workspace pin → delegated owner). What the agentd
// board_* tools exercise in production, this suite pins at the seam
// they call:
//
//   - claim-token refusal: no token / wrong token → 409 claim_token
//   - token single-use + expiry: a forward exit consumes the token;
//     a lapsed-and-reclaimed claim mints a new one — the old token is
//     dead on every in-flight write (design §4.4, §6 case 1)
//   - attempts accounting: the claim is the only increment; a live
//     429 rolls it back (§4.4 throttle)
//   - foreign-task 404-class: a pod never touches a task whose
//     project targets another workspace (r1-S1 structural binding)
//   - the manager-surface refusal: a worker holding a STILL-VALID
//     token cannot exit pending_input (§4.2 — the token does not
//     unlock self-exit)
//
// Runs against TEST_DATABASE_URL (skips when unreachable), the
// pkg/board harness conventions: the schema is expected to exist
// (migrations applied by the CI job).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// The pod identity the fakes pin: workspace <wsPod> owned by user-7.
// The workspace id must be a UUID — it rides projects.target_workspace_id.
const (
	wsPod     = "11111111-1111-1111-1111-111111111101" // this pod
	wsForeign = "22222222-2222-2222-2222-222222222202" // another workspace's project
)

func TestIntegrationBoardWorkerSemantics(t *testing.T) {
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

	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-7', 'user-7', 'board1701@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err, "users row must exist (FK target)")

	cfg := board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: time.Hour,
		DRRQuantum:      1,
		SweepInterval:   time.Second,
	}
	pending := &pendingFlag{}
	svc := board.NewService(pool, cfg,
		// R9 shape: every granted claim stamps a spawned assignee (the
		// pending-input pass keys on it).
		board.WithWorkerSpawner(func(_ context.Context, c *board.Claim) (string, error) {
			return "sess-" + c.ShortID, nil
		}),
		board.WithPendingInputSource(pending),
	)
	recon := board.NewReconciler(pool, cfg, board.WithPendingInputSource(pending))

	// The seam router, mounted exactly as the production router mounts
	// it — with the REAL service installed and the REAL resolve chain
	// (fake TokenReview + workspace lookup: the auth shape, not a stub
	// of the handler).
	// The pod identity, UUID-shaped (the production resolve chain: the
	// SA principal's workspace must be a UUID — it is the projects'
	// target_workspace_id).
	h := &PodAutomationHandler{
		tokenReviewer: &fakeTokenReviewer{
			username: "system:serviceaccount:" + testRenameNamespace + ":workspace-" + wsPod,
		},
		lookup:            &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: wsPod, UserID: "user-7"}},
		expectedNamespace: testRenameNamespace,
		board:             svc,
		boardEngine:       svc.Config(),
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	b := r.Group("/internal/v1/automation/board")
	b.GET("/tasks/:id", h.BoardTaskGet)
	b.POST("/tasks/heartbeat", h.BoardHeartbeat)
	b.POST("/tasks/throttle", h.BoardReportThrottle)
	b.POST("/tasks/move-stage", h.BoardMoveStage)

	// --- fixtures ------------------------------------------------------------

	// seed creates one project/epic/task targeting the pod's workspace
	// (in-scope) or the foreign one. Isolation between arms relies on
	// each arm leaving NO claimable backlog row behind (terminal,
	// in-flight, pending_input, or cooldown-backlog) — claimFor asserts
	// the grant landed on THIS arm's task, so a leak fails loud.
	seed := func(t *testing.T, foreign bool) (taskShortID, projectID string) {
		targetWorkspace := wsPod
		if foreign {
			targetWorkspace = wsForeign
		}
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
			VALUES ($1::uuid, 'user-7', $1::text, 'default', now(), now()) ON CONFLICT (id) DO NOTHING`, targetWorkspace)
		require.NoError(t, err)
		owner := board.Actor{Kind: board.ActorUser, UserID: "user-7", Surface: board.SurfaceREST}
		p, err := svc.CreateProject(ctx, owner,
			board.ProjectInput{Name: "p-" + targetWorkspace, TargetWorkspaceID: targetWorkspace, WorkerSlots: 2, Tags: []string{}}, "")
		require.NoError(t, err)
		e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "e-" + targetWorkspace, Outcome: "o"}, "")
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE epics SET status='active' WHERE id=$1`, e.ID)
		require.NoError(t, err)
		task, err := svc.CreateTask(ctx, owner, e.ID, "",
			board.TaskInput{Title: "t-" + targetWorkspace, Tags: []string{},
				AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1", Met: true}}}, "")
		require.NoError(t, err)
		return task.ShortID, p.ID
	}

	// claimFor claims THE seeded task in its workspace and returns the
	// live claim (asserting the grant landed on the expected row — a
	// loud failure if a stale backlog candidate leaked from an earlier
	// arm). In-scope arms claim through the pod's workspace; the
	// foreign arm claims through its own.
	claimFor := func(t *testing.T, ws, wantShortID string) *board.Claim {
		t.Helper()
		c, err := svc.ClaimNext(ctx, "worker-"+ws, ws)
		require.NoError(t, err)
		require.NotNil(t, c.Task)
		require.False(t, c.Escalated)
		assert.Equal(t, wantShortID, c.Task.ShortID, "the claim must land on this arm's task (isolation violated?)")
		assert.NotEmpty(t, c.ClaimToken, "a grant mints the claim token (atomic issuance, §4.4)")
		return c
	}

	boardGet := func(t *testing.T, shortID string) (int, map[string]any) {
		t.Helper()
		w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/tasks/"+shortID+"?workspaceID="+wsPod, "")
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			body = map[string]any{"_raw": w.Body.String()}
		}
		return w.Code, body
	}

	heartbeat := func(t *testing.T, shortID, token, observed string) *httptest.ResponseRecorder {
		t.Helper()
		return doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/heartbeat",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"observedProgressAt":%q}`, wsPod, shortID, token, observed))
	}

	// --- 1: claim-token refusal ------------------------------------------------

	t.Run("claim_token_refusal", func(t *testing.T) {
		shortID, _ := seed(t, false)
		claim := claimFor(t, wsPod, shortID)

		// No token at all: the shape refusal would catch it in agentd;
		// the seam pins the authoritative guard — a token-less
		// in-flight write is a 409 claim_token, never a silent pass.
		w := heartbeat(t, shortID, "", time.Now().UTC().Format(time.RFC3339))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), board.GuardClaimToken)

		// Wrong token: same class — the guard compares against the live
		// row's token, not the caller's say-so.
		w = heartbeat(t, shortID, "not-the-token", time.Now().UTC().Format(time.RFC3339))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), board.GuardClaimToken)

		w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/throttle",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":"also-wrong"}`, wsPod, shortID))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

		w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":"","toStage":"in_review"}`, wsPod, shortID))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())

		// The RIGHT token passes the guard (the stage machine may have
		// its own opinions; the token is not what refuses).
		w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"toStage":"in_review"}`, wsPod, shortID, claim.ClaimToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	// --- 2: evidence-gated heartbeat (#1685 through the seam) ------------------

	t.Run("heartbeat_evidence_gate", func(t *testing.T) {
		shortID, _ := seed(t, false)
		claim := claimFor(t, wsPod, shortID)

		// Evidence BEFORE the stored watermark (the claim stamped
		// last_progress_at=now()): no extension — elapsed time is not
		// liveness.
		stale := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		w := heartbeat(t, shortID, claim.ClaimToken, stale)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"extended":false`, "no new evidence → no lease extension")

		// Evidence past the watermark: extends.
		fresh := time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
		w = heartbeat(t, shortID, claim.ClaimToken, fresh)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"extended":true`)
	})

	// --- 3: token single-use + expiry (§6 case 1) -------------------------------

	t.Run("token_single_use_and_expiry", func(t *testing.T) {
		shortID, _ := seed(t, false)
		claim := claimFor(t, wsPod, shortID)

		// Forward exit consumes the token: → in_review clears it (work
		// done, nothing is leased), and the SAME token now 409s.
		w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"toStage":"in_review"}`, wsPod, shortID, claim.ClaimToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		w = heartbeat(t, shortID, claim.ClaimToken, time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
		require.Equal(t, http.StatusConflict, w.Code, "the exit consumed the token (single use)")
		assert.Contains(t, w.Body.String(), board.GuardClaimToken)

		// The read no longer carries the token to ANY same-workspace
		// reader — it retired with the claim.
		code, body := boardGet(t, shortID)
		require.Equal(t, http.StatusOK, code)
		assert.NotContains(t, body, "claim_token", "a cleared token is absent from the read model")

		// Expiry: a second task lapses, the sweep reclaims it, the next
		// claim mints a FRESH token — the old one is dead everywhere.
		shortID2, _ := seed(t, false)
		claim2 := claimFor(t, wsPod, shortID2)
		_, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at = now() - interval '1 millisecond' WHERE short_id=$1`, shortID2)
		require.NoError(t, err)
		_, err = recon.Sweep(ctx)
		require.NoError(t, err)
		// #1813: drop the reclaim's failure backoff — this arm pins
		// token single-use, not the park.
		_, err = pool.Exec(ctx, `UPDATE tasks SET cooldown_until=NULL WHERE short_id=$1`, shortID2)
		require.NoError(t, err)

		w = heartbeat(t, shortID2, claim2.ClaimToken, time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
		require.Equal(t, http.StatusConflict, w.Code, "a reclaimed claim's token is dead")
		assert.Contains(t, w.Body.String(), board.GuardClaimToken)

		w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"toStage":"in_review"}`, wsPod, shortID2, claim2.ClaimToken))
		require.Equal(t, http.StatusConflict, w.Code, "the old token cannot move the reclaimed task either")

		// The re-claim (a fresh worker per R9) mints a NEW token that
		// DOES work.
		claim3 := claimFor(t, wsPod, shortID2)
		assert.NotEqual(t, claim2.ClaimToken, claim3.ClaimToken, "re-claim mints a fresh token")
		w = heartbeat(t, shortID2, claim3.ClaimToken, time.Now().UTC().Add(time.Minute).Format(time.RFC3339))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	// --- 4: attempts accounting (§4.4 through the seam) -------------------------

	t.Run("attempts_accounting", func(t *testing.T) {
		shortID, projectID := seed(t, false)

		_, body := boardGet(t, shortID)
		require.Equal(t, float64(0), body["attempts"], "fresh task: zero attempts")

		claim := claimFor(t, wsPod, shortID) // the ONLY increment
		_, body = boardGet(t, shortID)
		require.Equal(t, float64(1), body["attempts"], "a granted claim counts one attempt")

		// A live 429: requeue WITHOUT attempt penalty — the increment
		// rolls back (net zero), the row cools down in backlog.
		w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/throttle",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"retryAfterSeconds":3600}`, wsPod, shortID, claim.ClaimToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		_, body = boardGet(t, shortID)
		require.Equal(t, board.StageBacklog, body["stage"], "throttle requeues to backlog")
		assert.Equal(t, float64(0), body["attempts"], "a provider-refused claim was not an attempt (no penalty)")

		// The token retired with the throttle; attempts climb again on
		// the NEXT grant (the (N)th real claim still counts).
		_, err := pool.Exec(ctx, `UPDATE tasks SET cooldown_until=NULL WHERE short_id=$1`, shortID)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE projects SET slot_cooldown_until=NULL WHERE id=$1`, projectID)
		require.NoError(t, err)
		claimFor(t, wsPod, shortID)
		_, body = boardGet(t, shortID)
		require.Equal(t, float64(1), body["attempts"], "re-claim after cooldown: the increment belongs to the grant")
	})

	// --- 5: foreign-task 404-class (r1-S1 structural binding) -------------------

	t.Run("foreign_task_404_class", func(t *testing.T) {
		// The SAME owner owns a project targeting ANOTHER workspace; its
		// task is claimed there. This pod (ws-1) can neither see nor
		// touch it — resolution itself is workspace-scoped: a plain 404,
		// no suggestions, the token never compared.
		shortID, _ := seed(t, true)
		claim := claimFor(t, wsForeign, shortID)

		w := doBoard(t, r, http.MethodGet, "/internal/v1/automation/board/tasks/"+shortID+"?workspaceID="+wsPod, "")
		require.Equal(t, http.StatusNotFound, w.Code, "foreign task: workspace-scoped miss")
		var miss struct {
			Suggestions []string `json:"suggestions"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &miss))
		assert.Empty(t, miss.Suggestions, "no cross-tenant did-you-mean: the scoped resolution cannot see the foreign row")

		w = heartbeat(t, shortID, claim.ClaimToken, time.Now().UTC().Format(time.RFC3339))
		require.Equal(t, http.StatusNotFound, w.Code, "the REAL token of a foreign task buys nothing on this pod")
		assert.NotContains(t, w.Body.String(), board.GuardClaimToken, "the guard class is 404, not a token 409 — resolution failed first")

		w = doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"toStage":"in_review"}`, wsPod, shortID, claim.ClaimToken))
		require.Equal(t, http.StatusNotFound, w.Code)
	})

	// --- 6: pending_input self-exit refusal (the token does not unlock it) -----

	t.Run("pending_input_self_exit_refused", func(t *testing.T) {
		shortID, _ := seed(t, false)
		claim := claimFor(t, wsPod, shortID)

		// The worker's session holds an unanswered question: the sweep
		// stages the task pending_input — KEEPING the token (the claim
		// is not dead; the human is deciding).
		pending.set("sess-" + shortID)
		_, err := recon.Sweep(ctx)
		require.NoError(t, err)
		code, body := boardGet(t, shortID)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, board.StagePendingInput, body["stage"], w3(body))

		// The still-held token does NOT unlock the exit: a delegated
		// worker is not the human the exit exists for (§4.2).
		w := doBoard(t, r, http.MethodPost, "/internal/v1/automation/board/tasks/move-stage",
			fmt.Sprintf(`{"workspaceID":%q,"shortId":%q,"claimToken":%q,"toStage":"in_progress"}`, wsPod, shortID, claim.ClaimToken))
		require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), board.GuardManagerSurface, "manager_surface_only — the agent-surface refusal")
	})
}

// pendingFlag is the mutable pending-input fixture (per-assignee) —
// implements board.PendingInputSource.
type pendingFlag struct {
	v map[string]bool
}

func (p *pendingFlag) PendingInput(_ context.Context, assignee string) (board.PendingInputState, error) {
	return board.PendingInputState{HasPendingInput: p.v[assignee]}, nil
}

func (p *pendingFlag) set(assignee string) {
	if p.v == nil {
		p.v = map[string]bool{}
	}
	p.v[assignee] = true
}

// w3 renders a map body for assertion messages.
func w3(body map[string]any) string {
	b, _ := json.Marshal(body)
	return string(b)
}
