// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// board_sse_phrase_integration_test.go — #1922 red-first pin for the
// SSE projection leg: the board.event frame's actor session id is
// phrase-translated at emission (the #1817/#1821 SSE precedent — the
// adjacent site the #1922 sweep found; the frontend does not render the
// actor today, but the stream is user/agent-readable). Degraded shape
// follows markDeprecatedSessionID exactly: raw passthrough +
// session_id_deprecated flag while the session is cold.
//
// Function name rides the TestIntegrationBoardSSELiveProjection prefix
// so the existing CI step's -run filter executes it (secrets-integration
// workflow, "SSE live-path pin for the board.event projection").

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestIntegrationBoardSSELiveProjectionActorPhrase(t *testing.T) {
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
	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-sse-phrase', 'user-sse-phrase', 'boardssephrase@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	wsID := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'user-sse-phrase', $2, 'default', now(), now())`, wsID, "ws-"+wsID[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM session_index WHERE workspace_id=$1`, wsID)
	})

	// The warm session: registered + phrase minted (design 0066).
	warmRaw := "ses_" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, short_id, title, message_count)
		VALUES ($1, $2, 'amber-heron-waits', 'warm', 1)`, wsID, warmRaw)
	require.NoError(t, err)
	coldRaw := "ses_" + uuid.NewString() // no index row — the cold window

	log, _ := logger.NewObserved()
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker(), logger: log}
	svc := board.NewService(pool, board.DefaultEngineConfig())
	pub := h.BoardEventPublisher(pool, log)
	require.NotNil(t, pub)
	svc.SetEventPublisher(pub)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-sse-phrase"); c.Next() })
	r.GET("/api/v1/events", h.StreamUserEvents)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)

	frames := make(chan string, 64)
	ready := make(chan struct{})
	go func() {
		req, _ := http.NewRequestWithContext(subCtx, http.MethodGet, srv.URL+"/api/v1/events", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			close(ready)
			return
		}
		close(ready)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				frames <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("SSE subscription did not establish within 5s")
	}

	owner := board.Actor{Kind: board.ActorUser, UserID: "user-sse-phrase", Surface: board.SurfaceREST}
	p, err := svc.CreateProject(ctx, owner,
		board.ProjectInput{Name: "sse-actor-phrase", TargetWorkspaceID: wsID, WorkerSlots: 2, Tags: []string{}}, "")
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "E"}, "")
	require.NoError(t, err)

	mgr := func(session string) board.Actor {
		return board.Actor{Kind: board.ActorUser, UserID: "user-sse-phrase",
			Surface: board.SurfaceAgentManager, WorkspaceID: wsID, SessionID: session}
	}
	warmTask, err := svc.CreateTask(ctx, mgr(warmRaw), e.ID, "", board.TaskInput{Title: "warm actor", Description: "sse actor-phrase pin charter: reproduce then fix, per lane protocol.", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}}}, "")
	require.NoError(t, err)
	coldTask, err := svc.CreateTask(ctx, mgr(coldRaw), e.ID, "", board.TaskInput{Title: "cold actor", Description: "sse actor-phrase pin charter: reproduce then fix, per lane protocol.", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}}}, "")
	require.NoError(t, err)

	actorOf := func(frame string) map[string]any {
		var evt struct {
			ItemID string         `json:"item_id"`
			Actor  map[string]any `json:"actor"`
		}
		require.NoError(t, json.Unmarshal([]byte(frame), &evt))
		return evt.Actor
	}
	awaitFrame := func(itemID string) map[string]any {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case frame := <-frames:
				if strings.Contains(frame, `"board.event"`) && strings.Contains(frame, `"item_id":"`+itemID+`"`) {
					return actorOf(frame)
				}
			case <-deadline:
				t.Fatalf("no board.event frame for item %s within 30s", itemID)
			}
		}
	}

	warmActor := awaitFrame(warmTask.ID)
	assert.Equal(t, "amber-heron-waits", warmActor["session_id"],
		"the live board.event actor carries the PHRASE for a warm session")
	assert.NotContains(t, warmActor, "session_id_deprecated", "warm actor carries no deprecation flag")

	coldActor := awaitFrame(coldTask.ID)
	assert.Equal(t, coldRaw, coldActor["session_id"],
		"the cold window passes the raw harness id through (never worse)")
	assert.Equal(t, "true", coldActor["session_id_deprecated"],
		"the degraded actor carries session_id_deprecated (the #1821 markDeprecatedSessionID shape)")
}

// TestIntegrationBoardSSELiveProjectionPayloadPhrase pins the payload
// leg (round-2 finding 3): delete events embed the task's full snapshot
// (snapshotOf) in the payload — the assignee_session inside that
// snapshot must translate at emission exactly like the actor. Born-RED
// on 850475f7 (boardEventDeliver translated only the actor).
func TestIntegrationBoardSSELiveProjectionPayloadPhrase(t *testing.T) {
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
	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('user-sse-payload', 'user-sse-payload', 'boardssepayload@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	wsID := uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'user-sse-payload', $2, 'default', now(), now())`, wsID, "ws-"+wsID[:8])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM session_index WHERE workspace_id=$1`, wsID)
	})

	log, _ := logger.NewObserved()
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker(), logger: log}
	svc := board.NewService(pool, board.DefaultEngineConfig())
	pub := h.BoardEventPublisher(pool, log)
	require.NotNil(t, pub)
	svc.SetEventPublisher(pub)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-sse-payload"); c.Next() })
	r.GET("/api/v1/events", h.StreamUserEvents)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	subCtx, cancelSub := context.WithCancel(context.Background())
	t.Cleanup(cancelSub)

	frames := make(chan string, 64)
	ready := make(chan struct{})
	go func() {
		req, _ := http.NewRequestWithContext(subCtx, http.MethodGet, srv.URL+"/api/v1/events", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			close(ready)
			return
		}
		close(ready)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				frames <- strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("SSE subscription did not establish within 5s")
	}

	owner := board.Actor{Kind: board.ActorUser, UserID: "user-sse-payload", Surface: board.SurfaceREST}
	p, err := svc.CreateProject(ctx, owner,
		board.ProjectInput{Name: "sse-payload-phrase", TargetWorkspaceID: wsID, WorkerSlots: 2, Tags: []string{}}, "")
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "E"}, "")
	require.NoError(t, err)

	// One WARM claimed task and one COLD claimed task, deleted in-flight:
	// the delete_event payload's snapshot carries the assignee.
	mkTask := func(title string) *board.Task {
		tk, err := svc.CreateTask(ctx, owner, e.ID, "", board.TaskInput{Title: title, Description: "sse payload-phrase pin charter: reproduce then fix, per lane protocol.", AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}}}, "")
		require.NoError(t, err)
		return tk
	}
	stamp := func(t *testing.T, tk *board.Task, session string) {
		_, err := pool.Exec(ctx, `
			UPDATE tasks SET stage='in_progress', assignee_session=$1,
				lease_expires_at=now()+interval '1 hour' WHERE id=$2`, session, tk.ID)
		require.NoError(t, err)
	}
	warmRaw := "ses_" + uuid.NewString()
	_, err = pool.Exec(ctx, `
		INSERT INTO session_index (workspace_id, session_id, short_id, title, message_count)
		VALUES ($1, $2, 'amber-heron-waits', 'warm', 1)`, wsID, warmRaw)
	require.NoError(t, err)
	warmTask := mkTask("warm payload")
	stamp(t, warmTask, warmRaw)
	coldRaw := "ses_" + uuid.NewString()
	coldTask := mkTask("cold payload")
	stamp(t, coldTask, coldRaw)

	_, err = svc.DeleteTask(ctx, owner, warmTask.ID, "", false, "")
	require.NoError(t, err)
	_, err = svc.DeleteTask(ctx, owner, coldTask.ID, "", false, "")
	require.NoError(t, err)

	snapshotOf := func(frame string) map[string]any {
		var evt struct {
			ItemID  string          `json:"item_id"`
			Payload json.RawMessage `json:"payload"`
		}
		require.NoError(t, json.Unmarshal([]byte(frame), &evt))
		var pl struct {
			Snapshot map[string]any `json:"snapshot"`
		}
		require.NoError(t, json.Unmarshal(evt.Payload, &pl))
		require.NotNil(t, pl.Snapshot, "delete_task payloads embed the task snapshot")
		return pl.Snapshot
	}
	awaitDelete := func(itemID string) map[string]any {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case frame := <-frames:
				if strings.Contains(frame, `"board.event"`) && strings.Contains(frame, `"item_id":"`+itemID+`"`) &&
					strings.Contains(frame, `"delete_task"`) {
					return snapshotOf(frame)
				}
			case <-deadline:
				t.Fatalf("no delete_task frame for item %s within 30s", itemID)
			}
		}
	}

	warmSnap := awaitDelete(warmTask.ID)
	assert.Equal(t, "amber-heron-waits", warmSnap["assignee_session"],
		"the delete frame's snapshot carries the PHRASE for a warm assignee")
	assert.Nil(t, warmSnap["assignee_session_deprecated"], "warm snapshot carries no deprecation flag")

	coldSnap := awaitDelete(coldTask.ID)
	assert.Equal(t, coldRaw, coldSnap["assignee_session"],
		"the cold window passes the raw id through the snapshot (never worse)")
	assert.Equal(t, true, coldSnap["assignee_session_deprecated"],
		"the degraded snapshot carries assignee_session_deprecated")
}
