// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_bus_test.go — the S6 producer/join pins over miniredis:
// bus-first with the v1 point-to-point leg as the VERBATIM fallback
// (retired on success — pinned), the stamp and stage-move event
// classes landing on the workspace stream, and the SSE join serving
// subscribers from the stream with frame parity to the v1 envelope.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// --- fixtures ---------------------------------------------------------------

// staticScope is the injected scope source: one project, one
// workspace, one short id.
type staticScope struct {
	gate chan struct{} // nil unless the test blocks resolution
}

func (s staticScope) scopeForProject(_ context.Context, projectID string) (boardBusScope, error) {
	if s.gate != nil {
		<-s.gate
	}
	if projectID != "prj-1" {
		return boardBusScope{}, errors.New("scope: unknown project")
	}
	return boardBusScope{owner: "user-1", workspaceID: "ws-1"}, nil
}

func (s staticScope) scopeForTask(_ context.Context, taskID string) (boardBusScope, error) {
	if s.gate != nil {
		<-s.gate
	}
	if taskID != "task-1" {
		return boardBusScope{}, errors.New("scope: unknown task")
	}
	return boardBusScope{owner: "user-1", workspaceID: "ws-1", shortID: "tsk-one"}, nil
}

// v1Spy records the events the v1 point-to-point leg received.
type v1Spy struct {
	mu   sync.Mutex
	seen []board.Event
}

func (v *v1Spy) publisher(evt board.Event) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = append(v.seen, evt)
}

func (v *v1Spy) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.seen)
}

func testEvent(action string) board.Event {
	return board.Event{
		ID: "evt-" + action, ProjectID: "prj-1", ItemKind: "task", ItemID: "task-1",
		Action: action, Actor: []byte(`{"kind":"user","user_id":"user-1"}`),
		Payload:   []byte(`{"task_id":"task-1","from_stage":"backlog","resulting_stage":"in_progress"}`),
		CreatedAt: time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC),
	}
}

// busStreamEntries reads the workspace stream through a fresh client.
func busStreamEntries(t *testing.T, addr, workspaceID string) []map[string]string {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	return streamEntries(t, addr, workspaceID)
}

// joinGroupCount probes a stream's consumer groups through the
// GENERIC Do (go-redis v8's structured XInfoGroups errors on
// miniredis's reply shape — the S3 health probe's documented trap).
func joinGroupCount(t *testing.T, addr, workspaceID string) int {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	v, err := client.Do(context.Background(), "XINFO", "GROUPS", eventbus.StreamKey(workspaceID)).Result()
	if err != nil {
		return -1
	}
	rows, ok := v.([]interface{})
	if !ok {
		return -1
	}
	return len(rows)
}

// joinReady waits for the join's group to exist on the workspace's
// stream (deterministic group-before-publish ordering in tests).
func joinReady(t *testing.T, addr, workspaceID string) {
	t.Helper()
	require.Eventually(t, func() bool { return joinGroupCount(t, addr, workspaceID) == 1 },
		3*time.Second, 20*time.Millisecond, "the join's consumer group exists")
}

// --- the publisher ----------------------------------------------------------

// UNARMED: nil transport → nil publisher → the wiring installs v1
// VERBATIM (the pre-S6 posture, byte-identical).
func TestBoardBusPublisher_UnarmedIsV1Verbatim(t *testing.T) {
	require.Nil(t, newBoardBusPublisher(nil, staticScope{}, func(board.Event) {}), "no transport, no producer")
	spy := &v1Spy{}
	// The unarmed install IS the v1 leg itself (not a wrapper): the
	// returned publisher delivers to the spy directly.
	v1 := boardBusPublisherOrV1(nil, spy.publisher)
	require.NotNil(t, v1)
	v1(testEvent("move_stage"))
	assert.Equal(t, 1, spy.count())
}

// THE RETIREMENT PIN: armed + successful publish ⇒ the v1 leg is NOT
// called (the point-to-point leg stays retired, not duplicated), the
// stream carries the board.{project}.event entry with the row + the
// publish-time scope, and a move_stage ALSO publishes the vocabulary
// topic task.{id}.stage_changed.
func TestBoardBusPublisher_BusFirstRetiresV1OnSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	require.NotNil(t, tr)
	spy := &v1Spy{}
	pub := newBoardBusPublisher(tr, staticScope{}, spy.publisher)
	require.NotNil(t, pub)
	publisher := boardBusPublisherOrV1(pub, spy.publisher)

	publisher(testEvent("move_stage"))

	require.Eventually(t, func() bool { return len(busStreamEntries(t, mr.Addr(), "ws-1")) >= 2 }, 2*time.Second, 10*time.Millisecond,
		"both entries land on the workspace stream")
	assert.Zero(t, spy.count(), "the v1 point-to-point leg is NOT called when the bus takes the event (retired on success)")

	entries := busStreamEntries(t, mr.Addr(), "ws-1")
	var boardEntry, stageEntry map[string]string
	for _, e := range entries {
		switch e["topic"] {
		case eventbus.BoardEventTopic("prj-1"):
			boardEntry = e
		case eventbus.TaskStageTopic("task-1"):
			stageEntry = e
		}
	}
	require.NotNil(t, boardEntry, "board.{project}.event entry present")
	assert.Equal(t, "evt-move_stage", boardEntry[eventbus.FieldBoardEventID])
	assert.Equal(t, "move_stage", boardEntry[eventbus.FieldBoardAction])
	assert.Equal(t, "task", boardEntry[eventbus.FieldBoardItemKind])
	assert.Equal(t, "task-1", boardEntry[eventbus.FieldBoardItemID])
	assert.Equal(t, `{"kind":"user","user_id":"user-1"}`, boardEntry[eventbus.FieldBoardActor])
	assert.Equal(t, "user-1", boardEntry[eventbus.FieldBoardOwner], "the scope rides the payload (no per-delivery lookup)")
	assert.Equal(t, "ws-1", boardEntry[eventbus.FieldBoardWorkspace])
	assert.Equal(t, "tsk-one", boardEntry[eventbus.FieldBoardShortID])

	require.NotNil(t, stageEntry, "task.{id}.stage_changed entry present")
	assert.Equal(t, "backlog", stageEntry[eventbus.FieldStageFrom])
	assert.Equal(t, "in_progress", stageEntry[eventbus.FieldStageTo])
	assert.Equal(t, "user", stageEntry[eventbus.FieldStageSource])
	assert.Equal(t, "evt-move_stage", stageEntry[eventbus.FieldBoardEventID])
}

// THE FALLBACK PIN: a dead transport hands the event to v1 VERBATIM —
// per-event, never a drop (design 0072 §9.3's preserve-the-fallback).
func TestBoardBusPublisher_FallbackOnPublishFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	spy := &v1Spy{}
	pub := newBoardBusPublisher(tr, staticScope{}, spy.publisher)
	publisher := boardBusPublisherOrV1(pub, spy.publisher)

	publisher(testEvent("create_task")) // lands on the bus
	require.Eventually(t, func() bool { return len(busStreamEntries(t, mr.Addr(), "ws-1")) == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Zero(t, spy.count())

	mr.Close()
	evt := testEvent("update_task")
	evt.ID = "evt-2"
	publisher(evt)
	require.Eventually(t, func() bool { return spy.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"the failed publish falls back to v1 — the event is never dropped")
	spy.mu.Lock()
	require.NotEmpty(t, spy.seen)
	assert.Equal(t, "evt-2", spy.seen[0].ID, "v1 receives the event verbatim")
	spy.mu.Unlock()

	// The breaker: while open, the next event fails fast to v1.
	start := time.Now()
	publisher(testEvent("delete_task"))
	require.Eventually(t, func() bool { return spy.count() == 2 }, 2*time.Second, 10*time.Millisecond)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the open breaker fails fast (no per-event budget against a dead transport)")
}

// Scope-resolution failure: the v1 leg's own lookup decides (degraded
// exactly as today — the bus never invents a scope).
func TestBoardBusPublisher_ScopeFailureFallsBack(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	spy := &v1Spy{}
	pub := newBoardBusPublisher(tr, staticScope{}, spy.publisher)
	publisher := boardBusPublisherOrV1(pub, spy.publisher)

	evt := testEvent("create_task")
	evt.ProjectID = "prj-unknown"
	publisher(evt)
	require.Eventually(t, func() bool { return spy.count() == 1 }, 2*time.Second, 10*time.Millisecond,
		"an unresolved scope hands the event to v1")
	assert.Empty(t, busStreamEntries(t, mr.Addr(), "ws-1"))
}

// Overflow of the hand-off channel: NEVER drop a deliverable event
// while the v1 leg exists.
func TestBoardBusPublisher_OverflowFallsBack(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	scope := staticScope{gate: make(chan struct{})}
	spy := &v1Spy{}
	pub := newBoardBusPublisher(tr, scope, spy.publisher)
	publisher := boardBusPublisherOrV1(pub, spy.publisher)

	// The drainer is parked on the gate: one event in the drainer,
	// boardBusQueue buffered, then the NEXT two overflow straight to
	// v1.
	for i := 0; i < boardBusQueue+3; i++ {
		publisher(testEvent("create_task"))
	}
	require.Equal(t, 2, spy.count(), "overflow hands the excess to v1 — never a drop")
	close(scope.gate)
}

// The stamp projection: task.{id}.stamped with the SSE scope.
func TestBoardBusPublisher_StampSink(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	pub := newBoardBusPublisher(tr, staticScope{}, func(board.Event) {})

	pub.StampEventSink(context.Background(), board.StampEvent{
		TaskID: "task-1", TaskShortID: "tsk-one", ProjectID: "prj-1",
		Assignee: "sess-w", Origin: "spawn", LagSeconds: 0.019,
	})

	require.Eventually(t, func() bool { return len(busStreamEntries(t, mr.Addr(), "ws-1")) == 1 }, 2*time.Second, 10*time.Millisecond)
	e := busStreamEntries(t, mr.Addr(), "ws-1")[0]
	assert.Equal(t, eventbus.TaskStampedTopic("task-1"), e["topic"])
	assert.Equal(t, "sess-w", e[eventbus.FieldStampAssignee])
	assert.Equal(t, "spawn", e[eventbus.FieldStampOrigin])
	assert.Equal(t, "0.019", e[eventbus.FieldStampLagSeconds])
	assert.Equal(t, "user-1", e[eventbus.FieldBoardOwner])
	assert.Equal(t, "ws-1", e[eventbus.FieldBoardWorkspace])

	// Nil-safety: the unarmed publisher swallows the sink.
	var unarmed *boardBusPublisher
	unarmed.StampEventSink(context.Background(), board.StampEvent{TaskID: "x"})
}

// --- the SSE join ------------------------------------------------------------

// THE PARITY PIN: a committed event, published bus-first, is served
// from the join to a subscribed user as the SAME board.event frame
// the v1 envelope contract defines (flat row fields, workspace id on
// the user copy) — the full stack over real transport legs.
func TestBoardSSEJoin_E2EFrameParity(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")

	broker := eventbroker.NewUserEventBroker()
	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	// The join: the REAL handlers render (same path the v1 leg uses),
	// fed by the join loop over the stream.
	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(broker, nil, busTestLogger{}),
		func(context.Context) ([]string, error) { return []string{"ws-1"}, nil })
	require.NotNil(t, join)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go join.Start(ctx)

	// The publisher: bus-first (v1 would be the fallback).
	spy := &v1Spy{}
	joinReady(t, mr.Addr(), "ws-1")
	pub := newBoardBusPublisher(tr, staticScope{}, spy.publisher)
	pub.Publisher()(testEvent("move_stage"))

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "board.event", evt.Type)
		assert.Equal(t, "prj-1", evt.ProjectID)
		assert.Equal(t, "evt-move_stage", evt.BoardEventID)
		assert.Equal(t, "task", evt.ItemKind)
		assert.Equal(t, "task-1", evt.ItemID)
		assert.Equal(t, "move_stage", evt.Action)
		assert.Equal(t, json.RawMessage(`{"kind":"user","user_id":"user-1"}`), json.RawMessage(evt.BoardActor))
		assert.Equal(t, "ws-1", evt.WorkspaceID, "the user copy carries the workspace id (the v1 fan-out shape)")
		require.NotNil(t, evt.CreatedAt)
		assert.Equal(t, time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC).UTC(), evt.CreatedAt.UTC())
	case <-time.After(3 * time.Second):
		t.Fatal("the join never served the event")
	}
	assert.Zero(t, spy.count(), "bus-first: v1 untouched")
}

// A landed stamp reaches SSE as an assignee_stamped board.event frame.
func TestBoardSSEJoin_StampFrame(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	broker := eventbroker.NewUserEventBroker()
	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(broker, nil, busTestLogger{}),
		func(context.Context) ([]string, error) { return []string{"ws-1"}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go join.Start(ctx)
	joinReady(t, mr.Addr(), "ws-1")

	newBoardBusPublisher(tr, staticScope{}, func(board.Event) {}).
		StampEventSink(context.Background(), board.StampEvent{
			TaskID: "task-1", TaskShortID: "tsk-one", ProjectID: "prj-1",
			Assignee: "sess-w", Origin: "backfill", LagSeconds: 12.5,
		})

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "board.event", evt.Type)
		assert.Equal(t, "assignee_stamped", evt.Action)
		assert.Equal(t, "task", evt.ItemKind)
		assert.Equal(t, "task-1", evt.ItemID)
		var inner map[string]any
		require.NoError(t, json.Unmarshal(evt.BoardPayload, &inner))
		assert.Equal(t, "sess-w", inner["assignee_session"])
		assert.Equal(t, "backfill", inner["assignee_origin"])
		assert.Equal(t, 12.5, inner["stamp_lag_seconds"])
	case <-time.After(3 * time.Second):
		t.Fatal("the stamp never reached SSE")
	}
}

// StartLive + patterns: entries published BEFORE the group existed are
// not replayed at subscribers (a woken-by-history feed would duplicate
// settled events), and the lease family never crosses the join.
func TestBoardSSEJoin_StartLiveAndPatterns(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	broker := eventbroker.NewUserEventBroker()
	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	// History + a foreign-family entry on the stream BEFORE the join.
	require.NoError(t, tr.publish(context.Background(), "ws-1", eventbus.BoardEventTopic("prj-1"),
		eventbus.BoardEventPayload("old", "task", "task-1", "create_task", "{}", "{}", time.Now().UTC().Format(time.RFC3339Nano), "user-1", "ws-1", ""),
		&boardBusEventsPublished, &boardBusEventsFailed))
	require.NoError(t, tr.publish(context.Background(), "ws-1", eventbus.LeaseNudgeTopic("task-1"),
		eventbus.LeaseNudgePayload("task-1", "tsk-one", "ndg", "sess-w"),
		&boardBusEventsPublished, &boardBusEventsFailed))

	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(broker, nil, busTestLogger{}),
		func(context.Context) ([]string, error) { return []string{"ws-1"}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go join.Start(ctx)

	// The join arms (the consumer exists), then a NEW entry flows.
	require.Eventually(t, func() bool { return joinGroupCount(t, mr.Addr(), "ws-1") == 1 },
		3*time.Second, 20*time.Millisecond, "the join's consumer group is created")

	require.NoError(t, tr.publish(context.Background(), "ws-1", eventbus.BoardEventTopic("prj-1"),
		eventbus.BoardEventPayload("new", "task", "task-1", "update_task", "{}", "{}", time.Now().UTC().Format(time.RFC3339Nano), "user-1", "ws-1", ""),
		&boardBusEventsPublished, &boardBusEventsFailed))

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "new", evt.BoardEventID, "only the post-join entry is served (StartLive)")
	case <-time.After(3 * time.Second):
		t.Fatal("the post-join event never arrived")
	}
	select {
	case evt := <-sub.Ch:
		t.Fatalf("history or lease family crossed the join: %+v", evt)
	case <-time.After(300 * time.Millisecond):
	}
}

// Discovery: a workspace appearing in the discovery set gets a
// consumer within one sync; the per-replica group name is stable
// across the run.
func TestBoardSSEJoin_DiscoveryAddsWorkspace(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	broker := eventbroker.NewUserEventBroker()
	sub, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", sub)

	var mu sync.Mutex
	served := map[string]bool{"ws-1": true}
	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(broker, nil, busTestLogger{}),
		func(context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			out := make([]string, 0, len(served))
			for id := range served {
				out = append(out, id)
			}
			return out, nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go join.Start(ctx)

	require.Eventually(t, func() bool { return joinGroupCount(t, mr.Addr(), "ws-1") == 1 },
		3*time.Second, 20*time.Millisecond)

	// A second workspace appears: discovery serves a consumer for it
	// within one interval.
	mu.Lock()
	served["ws-2"] = true
	mu.Unlock()
	require.Eventually(t, func() bool { return joinGroupCount(t, mr.Addr(), "ws-2") == 1 },
		3*boardSSEJoinInterval/2+2*time.Second, 50*time.Millisecond, "discovery arms a consumer for the new workspace")
}

// The group is per-replica and destroyed at teardown: a graceful
// shutdown leaves no stalled sse-* group for the S3 detector to flag.
func TestBoardSSEJoin_TeardownDestroysGroup(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(eventbroker.NewUserEventBroker(), nil, busTestLogger{}),
		func(context.Context) ([]string, error) { return []string{"ws-1"}, nil })
	require.NotNil(t, join)
	assert.Contains(t, join.group, eventbus.GroupSSEPrefix+"-", "the group is the per-replica sse-* name")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { join.Start(ctx); close(done) }()
	require.Eventually(t, func() bool { return joinGroupCount(t, mr.Addr(), "ws-1") == 1 },
		3*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("teardown did not finish")
	}
	assert.Zero(t, joinGroupCount(t, mr.Addr(), "ws-1"), "the replica's group is destroyed on graceful shutdown")
}

// busTestLogger satisfies the handlers' warnLogger (nopLogger is
// taken by a sibling test file with a wider interface).
type busTestLogger struct{}

func (busTestLogger) Warn(string, ...interface{}) {}

// r1 review findings 2–4: the stage_changed vocabulary entry classifies
// its source from the row's ACTOR kind (never the client-chosen event
// id), skips on an unparseable committed payload, and counts in its OWN
// counters (one committed row ≠ two "events published"; a soft failure
// never opens the transport-wide breaker).
func TestBoardBusPublisher_StageChangedHonesty(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	pub := newBoardBusPublisher(tr, staticScope{}, func(board.Event) {})
	publisher := pub.Publisher()

	eventsBefore := boardBusEventsPublished.Value()
	stageBefore := boardBusStagePublished.Value()

	publisher(testEvent("move_stage")) // actor kind=user

	require.Eventually(t, func() bool {
		return boardBusStagePublished.Value() == stageBefore+1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, eventsBefore+1, boardBusEventsPublished.Value(),
		"the board.event entry counts once in the events counter")
	for _, e := range busStreamEntries(t, mr.Addr(), "ws-1") {
		if e["topic"] == eventbus.TaskStageTopic("task-1") {
			assert.Equal(t, "user", e[eventbus.FieldStageSource], "actor kind=user classifies user")
		}
	}

	// A system actor classifies system — not a client-prefix forgery:
	// the event id is caller-supplied and says nothing.
	sysEvt := testEvent("move_stage")
	sysEvt.ID = "engine:forged-by-client"
	sysEvt.Actor = []byte(`{"kind":"system"}`)
	publisher(sysEvt)
	require.Eventually(t, func() bool {
		return boardBusStagePublished.Value() == stageBefore+2
	}, 2*time.Second, 10*time.Millisecond)
	for _, e := range busStreamEntries(t, mr.Addr(), "ws-1") {
		if e["topic"] == eventbus.TaskStageTopic("task-1") && e[eventbus.FieldBoardEventID] == "engine:forged-by-client" {
			assert.Equal(t, "system", e[eventbus.FieldStageSource],
				"the classification is the server-derived actor kind — a client-chosen event id cannot forge it")
		}
	}

	// An absent/unparseable actor attests NOTHING: the stage entry
	// carries an EMPTY source — never a defaulted attribution (r3
	// review's missing pin: the defaulted-user defect could silently
	// return without this).
	noActor := testEvent("move_stage")
	noActor.ID = "evt-noactor"
	noActor.Actor = []byte(`{not json`)
	publisher(noActor)
	require.Eventually(t, func() bool {
		for _, e := range busStreamEntries(t, mr.Addr(), "ws-1") {
			if e["topic"] == eventbus.TaskStageTopic("task-1") && e[eventbus.FieldBoardEventID] == "evt-noactor" {
				return e[eventbus.FieldStageSource] == ""
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "an unparseable actor publishes an EMPTY source")

	// An unparseable committed payload skips the vocabulary entry (the
	// board.event entry still lands — it IS the row).
	stageNow := boardBusStagePublished.Value()
	garbage := testEvent("move_stage")
	garbage.ID = "evt-garbage"
	garbage.Payload = []byte(`{not json`)
	publisher(garbage)
	require.Eventually(t, func() bool {
		for _, e := range busStreamEntries(t, mr.Addr(), "ws-1") {
			if e["topic"] == eventbus.BoardEventTopic("prj-1") && e[eventbus.FieldBoardEventID] == "evt-garbage" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "the board.event entry for the row lands")
	assert.Equal(t, stageNow, boardBusStagePublished.Value(),
		"an unparseable payload publishes NO stage_changed entry")
}

// r1 robustness 1 (landed r2 after a false first claim — the record
// owns it): a consumer whose wire fails RETIRES its live entry, so the
// NEXT sync re-arms it. The pin is deterministic in both directions:
// registration is asserted SYNCHRONOUSLY inside syncOnce before any
// Eventually runs (no first-check race), the retire is observed as the
// entry leaving live (times out RED without the retire code), and the
// healed-wire re-arm proves the next sync actually retries.
func TestBoardSSEJoin_ConsumerRetiresOnWireFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	join := newBoardSSEJoin(tr, handlers.BoardEventBusHandler(eventbroker.NewUserEventBroker(), nil, busTestLogger{}),
		func(context.Context) ([]string, error) { return []string{"ws-1"}, nil })
	require.NotNil(t, join)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The wire dies BEFORE the consumer's busFor; syncOnce still
	// REGISTERS the entry synchronously (asserted — no scheduling
	// dependence), and the doomed goroutine must then retire it.
	mr.Close()
	join.syncOnce(ctx)
	join.mu.Lock()
	require.Len(t, join.live, 1, "syncOnce registers the consumer before its wire attempt")
	join.mu.Unlock()
	require.Eventually(t, func() bool {
		join.mu.Lock()
		defer join.mu.Unlock()
		return len(join.live) == 0
	}, 3*time.Second, 10*time.Millisecond,
		"a failed wire retires the live entry — the next sync retries (RED without the retire)")

	// The re-arm: heal the wire, run the next sync — the consumer group
	// appears on the healed stream.
	mr2 := miniredis.RunT(t)
	tr.client = redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	join.syncOnce(ctx)
	require.Eventually(t, func() bool { return joinGroupCount(t, mr2.Addr(), "ws-1") == 1 },
		3*time.Second, 20*time.Millisecond, "the next sync re-arms the consumer on the healed wire")
}

// blockingScope is the ctx-aware fake the r4 review asked for: the
// task leg PARKS inside the scope query until the delivery budget
// expires (a hung DB), then — once unblocked — answers like
// staticScope. Its flag is mutable so one publisher can carry both
// the wedged delivery and the recovery in a single pin.
type blockingScope struct {
	blockTask atomic.Bool
}

func (b *blockingScope) scopeForProject(ctx context.Context, projectID string) (boardBusScope, error) {
	return staticScope{}.scopeForProject(ctx, projectID)
}

func (b *blockingScope) scopeForTask(ctx context.Context, taskID string) (boardBusScope, error) {
	if b.blockTask.Load() {
		// The hung query: it answers only when the ctx dies.
		<-ctx.Done()
		return boardBusScope{}, ctx.Err()
	}
	return staticScope{}.scopeForTask(ctx, taskID)
}

// TestBoardBusPublisher_ScopeBudgetBoundsAWedgedDelivery — r3's fix,
// guarded (r4's blocking finding): a scope query that never returns
// costs the drainer ONE delivery budget (the stamp drops, counted) —
// never the drainer: the NEXT stamp on the healed scope lands on the
// stream. Without the budget this pin hangs the suite (the wedged
// delivery never resolves) — the starvation defect cannot return
// silently.
func TestBoardBusPublisher_ScopeBudgetBoundsAWedgedDelivery(t *testing.T) {
	mr := miniredis.RunT(t)
	tr := newBoardBusClient(mr.Addr(), "", "board events")
	scope := blockingScope{}
	scope.blockTask.Store(true)
	pub := newBoardBusPublisher(tr, &scope, func(board.Event) {})

	failedBefore := boardBusStampsFailed.Value()
	pub.StampEventSink(context.Background(), board.StampEvent{
		TaskID: "task-1", TaskShortID: "tsk-one", ProjectID: "prj-1",
		Assignee: "sess-w", Origin: "spawn", LagSeconds: 1,
	})

	// The wedged delivery resolves through the budget: the scope read
	// dies with the ctx, the stamp drops, the counter says so.
	require.Eventually(t, func() bool { return boardBusStampsFailed.Value() == failedBefore+1 },
		boardBusScopeBudget+3*time.Second, 25*time.Millisecond,
		"the wedged scope read costs ONE budget — the stamp drop is counted")

	// The drainer SURVIVES: heal the scope, the next stamp lands.
	scope.blockTask.Store(false)
	pub.StampEventSink(context.Background(), board.StampEvent{
		TaskID: "task-1", TaskShortID: "tsk-one", ProjectID: "prj-1",
		Assignee: "sess-w", Origin: "backfill", LagSeconds: 2,
	})
	require.Eventually(t, func() bool {
		for _, e := range busStreamEntries(t, mr.Addr(), "ws-1") {
			if e["topic"] == eventbus.TaskStampedTopic("task-1") && e[eventbus.FieldStampOrigin] == "backfill" {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "the drainer delivers the next healthy stamp — no process-lifetime starvation")
}
