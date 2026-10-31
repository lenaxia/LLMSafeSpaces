// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_bus.go — the API-side board bus legs (design 0072 S6): the
// committed-board-events publisher, the stamp publisher, and the SSE
// consumer join — the unification the bus was built for.
//
// Before S6 the board's SSE plane was point-to-point: every committed
// board_events row was handed writer-replica-locally to the broker
// through an in-process channel (BoardEventPublisher), with a project
// lookup per delivery, and the stamp (the observation plane's key
// write) had NO event surface at all. Every consumer derived state
// independently; a subscriber on another replica saw nothing until the
// 30s poll or a reconnect reseed.
//
// With the bus armed:
//
//   - PUBLISHER (bus-first, v1 fallback): one board.{project}.event
//     entry per committed row, scope (owner + workspace) resolved at
//     PUBLISH time — delivery performs no project lookup (the retired
//     read). move_stage rows additionally publish the vocabulary
//     topic task.{id}.stage_changed. Landed stamps publish
//     task.{id}.stamped. A publish failure falls back to the v1 leg
//     VERBATIM, per-event — the S4 phantom-nudge discipline: success
//     means the bus owns the event (the v1 leg is NOT also called —
//     the retired leg stays retired), failure means v1 delivers it.
//
//   - SSE JOIN (the consumer): one per-replica consumer group
//     (sse-<hostname>) per workspace stream renders entries into the
//     SAME board.event frames the v1 leg renders (byte-parity
//     pinned) and fans them out through the SAME translate+fan-out
//     path — every replica's broker is fed, so a subscriber anywhere
//     sees the event. Per-replica groups (not one shared group)
//     preserve per-project ordering per replica and keep the
//     writer-replica echo today's v1 leg guarantees.
//
// Unarmed (the default) or unreachable-at-boot: nil producer, no
// consumer — the v1 point-to-point leg verbatim, byte-identical
// pre-S6 behavior (design 0072 §3's env-gate contract). The knob is
// ONE POSTURE with the agentd edge and S4's lease publisher
// (board.eventbus.*, falling back to board.leaseEventbus.* — arm
// with the controller's --workspace-eventbus-addr, same Redis).

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// Budgets mirror S4's lease posture: one bounded publish (and bus
// re-wire), one sweep-window breaker pause — a stalled Redis is never
// an acceptable observability bill, and the v1 fallback answers
// immediately while the breaker is open.
const (
	boardBusPublishBudget = 2 * time.Second
	boardBusBreakerPause  = 30 * time.Second
	// boardScopeCacheTTL bounds the publish-time scope resolution
	// cache (the retired leg's per-delivery project lookup becomes one
	// indexed read per project per TTL — the projects table is small
	// and near-static; a deleted project's cached scope publishes to a
	// stream with no subscribers, which is a no-op, never a wrong
	// delivery).
	boardScopeCacheTTL = 60 * time.Second
	// boardScopeCacheMax bounds the cache (project- and task-keyed).
	boardScopeCacheMax = 4096
	// boardBusQueue bounds the publisher's hand-off channels; overflow
	// of the EVENT channel falls back to v1 (never drop a deliverable
	// event while the v1 leg exists); overflow of the STAMP channel
	// drops (stamps have no v1 leg — pure observability, the S4 sink
	// posture).
	boardBusQueue = 256
	// boardSSEJoinInterval is the discovery cadence for the consumer
	// set (which workspaces carry board projects).
	boardSSEJoinInterval = 30 * time.Second
)

// The API-side visibility (expvar on /debug/vars, the S4 precedent).
var (
	boardBusEventsPublished expvar.Int
	boardBusEventsFailed    expvar.Int
	boardBusStampsPublished expvar.Int
	boardBusStampsFailed    expvar.Int
	boardBusSSEJoined       expvar.Int
	boardBusSSEFallback     expvar.Int
)

func init() {
	expvar.Publish("board_bus_events_published_total", &boardBusEventsPublished)
	expvar.Publish("board_bus_events_failed_total", &boardBusEventsFailed)
	expvar.Publish("board_bus_stamps_published_total", &boardBusStampsPublished)
	expvar.Publish("board_bus_stamps_failed_total", &boardBusStampsFailed)
	expvar.Publish("board_bus_sse_joined_total", &boardBusSSEJoined)
	expvar.Publish("board_bus_sse_fallback_total", &boardBusSSEFallback)
}

// ---- The transport (shared shape with S4's lease publisher) ----

// boardBusClient is one API-side bus transport: a shared redis
// client, one lazily-wired RedisBus per workspace, a fail-fast
// breaker, and per-leg expvar counters. The nil pointer is the OFF
// posture (every method nil-receiver-safe).
type boardBusClient struct {
	client *redis.Client
	name   string

	mu    sync.Mutex
	buses map[string]*eventbus.RedisBus

	// breakerNanos is the fail-fast gate (unix nanos; 0 = closed).
	breakerNanos atomic.Int64
}

// newBoardBusClient wires the transport from the raw knob spelling
// (nil on empty/malformed/unreachable — the callers' v1 legs answer).
func newBoardBusClient(addr, password, name string) *boardBusClient {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	opts, err := leaseEventbusOptions(addr, password)
	if err != nil {
		slog.Warn("board: event bus address malformed — the v1 legs stay point-to-point", "leg", name, "err", err.Error())
		return nil
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), boardBusPublishBudget)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("board: event bus unreachable — the v1 legs stay point-to-point", "leg", name, "err", err.Error())
		_ = client.Close()
		return nil
	}
	return &boardBusClient{client: client, name: name, buses: map[string]*eventbus.RedisBus{}}
}

func (b *boardBusClient) breakerOpen() bool {
	return time.Now().UnixNano() < b.breakerNanos.Load()
}

// busFor returns the workspace's bus, wiring it on first use (the
// constructor pings, so an evicted bus re-proves the transport).
func (b *boardBusClient) busFor(ctx context.Context, workspaceID string) (*eventbus.RedisBus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bus, ok := b.buses[workspaceID]; ok {
		return bus, nil
	}
	wctx, cancel := context.WithTimeout(ctx, boardBusPublishBudget)
	defer cancel()
	bus, err := eventbus.NewRedisBus(wctx, b.client, eventbus.StreamKey(workspaceID), 0)
	if err != nil {
		return nil, fmt.Errorf("board: event bus (%s) for workspace %s: %w", b.name, workspaceID, err)
	}
	b.buses[workspaceID] = bus
	return bus, nil
}

func (b *boardBusClient) evictBus(workspaceID string) {
	b.mu.Lock()
	delete(b.buses, workspaceID)
	b.mu.Unlock()
}

// publish appends one event: bounded, counted (per-leg counters
// injected by the caller — one transport, many legs, honest counts),
// fail-open. The breaker makes a downed Redis cost at most ONE budget
// per pause window.
func (b *boardBusClient) publish(ctx context.Context, workspaceID, topic string, payload map[string]string, published, failed *expvar.Int) error {
	if b == nil || b.client == nil {
		failed.Add(1)
		return errors.New("board: event bus unwired")
	}
	if b.breakerOpen() {
		failed.Add(1)
		return errors.New("board: event bus paused after a failed publish")
	}
	bus, err := b.busFor(ctx, workspaceID)
	if err != nil {
		b.trip(failed)
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, boardBusPublishBudget)
	defer cancel()
	if _, err := bus.Publish(pctx, eventbus.Event{Topic: topic, At: time.Now(), Payload: payload}); err != nil {
		b.trip(failed)
		b.evictBus(workspaceID)
		return err
	}
	published.Add(1)
	return nil
}

// trip opens the breaker for one pause window.
func (b *boardBusClient) trip(failed *expvar.Int) {
	failed.Add(1)
	b.breakerNanos.Store(time.Now().Add(boardBusBreakerPause).UnixNano())
	slog.Warn("board: event publish failed — bus paused one sweep window (v1 semantics unaffected)",
		"leg", b.name, "retry_in", boardBusBreakerPause.String())
}

// streams snapshots the currently-wired workspaces (shutdown's group
// destroy walks this).
func (b *boardBusClient) streams() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.buses))
	for ws := range b.buses {
		out = append(out, ws)
	}
	return out
}

// ---- Publish-time scope resolution (the retired read's replacement) ----

// boardBusScope is one resolved scope entry.
type boardBusScope struct {
	owner       string
	workspaceID string
	shortID     string // the task's short id ("" for non-task scopes)
}

// boardScopeCache caches project- and task-keyed scope lookups for
// boardBusScopeCacheTTL. Failures are NOT cached (a transient read
// must not pin a wrong fallback).
type boardScopeCache struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	at   map[string]time.Time
	val  map[string]boardBusScope
}

func newBoardScopeCache(pool *pgxpool.Pool) *boardScopeCache {
	return &boardScopeCache{pool: pool, at: map[string]time.Time{}, val: map[string]boardBusScope{}}
}

func (c *boardScopeCache) get(ctx context.Context, key string, q func(context.Context) (boardBusScope, error)) (boardBusScope, error) {
	c.mu.Lock()
	if v, ok := c.val[key]; ok && time.Since(c.at[key]) < boardScopeCacheTTL {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	v, err := q(ctx)
	if err != nil {
		return boardBusScope{}, err
	}
	c.mu.Lock()
	if len(c.val) >= boardScopeCacheMax && c.at[key].IsZero() {
		// Quarter-prune (the liveness hub's cap discipline).
		for k := range c.val {
			if len(c.val) < boardScopeCacheMax-boardScopeCacheMax/4 {
				break
			}
			delete(c.val, k)
			delete(c.at, k)
		}
	}
	c.val[key] = v
	c.at[key] = time.Now()
	c.mu.Unlock()
	return v, nil
}

// scopeForProject resolves a project's SSE scope (owner + target
// workspace).
func (c *boardScopeCache) scopeForProject(ctx context.Context, projectID string) (boardBusScope, error) {
	return c.get(ctx, "p:"+projectID, func(ctx context.Context) (boardBusScope, error) {
		var s boardBusScope
		err := c.pool.QueryRow(ctx,
			`SELECT user_id, target_workspace_id::text FROM projects WHERE id=$1`, projectID).
			Scan(&s.owner, &s.workspaceID)
		if err != nil {
			return boardBusScope{}, fmt.Errorf("board: bus scope project %s: %w", projectID, err)
		}
		return s, nil
	})
}

// scopeForTask resolves a task's scope and short id in one indexed
// read (the boardScopeFor shape + short_id).
func (c *boardScopeCache) scopeForTask(ctx context.Context, taskID string) (boardBusScope, error) {
	return c.get(ctx, "t:"+taskID, func(ctx context.Context) (boardBusScope, error) {
		var s boardBusScope
		err := c.pool.QueryRow(ctx, `
			SELECT p.user_id, p.target_workspace_id::text, COALESCE(t.short_id,'')
			FROM tasks t JOIN projects p ON p.id = t.project_id
			WHERE t.id=$1`, taskID).
			Scan(&s.owner, &s.workspaceID, &s.shortID)
		if err != nil {
			return boardBusScope{}, fmt.Errorf("board: bus scope task %s: %w", taskID, err)
		}
		return s, nil
	})
}

// ---- The publisher (bus-first EventPublisher + stamp sink) ----

// boardBusPublisher projects committed board events and landed stamps
// onto the per-workspace streams, bus-first with a verbatim v1
// fallback per event.
// boardScopeSource resolves publish-time scope (the interface exists
// so the publisher's tests inject scope without a database — the
// production implementation is boardScopeCache).
type boardScopeSource interface {
	scopeForProject(ctx context.Context, projectID string) (boardBusScope, error)
	scopeForTask(ctx context.Context, taskID string) (boardBusScope, error)
}

type boardBusPublisher struct {
	tr    *boardBusClient
	scope boardScopeSource
	v1    board.EventPublisher

	events     chan board.Event
	stamps     chan board.StampEvent
	closeOnce  sync.Once
	drainClose chan struct{}
}

// newBoardBusPublisher wires the S6 producers. Nil transport (the
// unarmed default) returns a nil publisher — every method is
// nil-receiver-safe and the wiring installs v1 verbatim.
func newBoardBusPublisher(tr *boardBusClient, scope boardScopeSource, v1 board.EventPublisher) *boardBusPublisher {
	if tr == nil || v1 == nil || scope == nil {
		return nil
	}
	p := &boardBusPublisher{
		tr: tr, scope: scope, v1: v1,
		events: make(chan board.Event, boardBusQueue), stamps: make(chan board.StampEvent, boardBusQueue),
		drainClose: make(chan struct{}),
	}
	go p.drainEvents()
	go p.drainStamps()
	return p
}

// Publisher returns the bus-first board.EventPublisher (nil receiver
// = nil — the wiring then installs v1 verbatim; a nil v1 at
// construction means engine-dark and the wrapper never existed).
func (p *boardBusPublisher) Publisher() board.EventPublisher {
	if p == nil {
		return nil
	}
	return func(evt board.Event) {
		select {
		case p.events <- evt:
		default:
			// Overflow: NEVER drop a deliverable event while v1
			// exists — the point-to-point leg answers this one.
			boardBusSSEFallback.Add(1)
			p.v1(evt)
		}
	}
}

// StampEventSink is the board.WithStampSink projection (nil-safe).
func (p *boardBusPublisher) StampEventSink(ctx context.Context, ev board.StampEvent) {
	if p == nil || ev.TaskID == "" {
		return
	}
	select {
	case p.stamps <- ev:
	default:
		// Stamps have no v1 leg: pure observability, drop with a warn
		// (the S4 lease-sink posture — the next stamp's event is the
		// recovery; the row itself already carries the identity).
		boardBusStampsFailed.Add(1)
		slog.WarnContext(ctx, "board: stamp event queue full — dropped (the stamp itself landed)",
			"task", ev.TaskShortID, "origin", ev.Origin)
	}
}

// Close stops the drainers (pending entries flush to v1 first).
func (p *boardBusPublisher) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() { close(p.drainClose) })
}

// drainEvents is the event leg's single drainer: per-project order
// preserved (the v1 single-drainer parity), scope resolved at publish
// time, v1 fallback per event.
func (p *boardBusPublisher) drainEvents() {
	ctx := context.Background()
	for {
		select {
		case <-p.drainClose:
			for {
				select {
				case evt := <-p.events:
					p.deliverEvent(ctx, evt)
				default:
					return
				}
			}
		case evt := <-p.events:
			p.deliverEvent(ctx, evt)
		}
	}
}

func (p *boardBusPublisher) deliverEvent(ctx context.Context, evt board.Event) {
	scope, err := p.scope.scopeForProject(ctx, evt.ProjectID)
	if err != nil {
		// Scope unresolved (project deleted, DB blip): the v1 leg's
		// own lookup decides — degraded exactly as today.
		boardBusSSEFallback.Add(1)
		p.v1(evt)
		return
	}
	shortID := ""
	if evt.ItemKind == "task" {
		if ts, err := p.scope.scopeForTask(ctx, evt.ItemID); err == nil {
			shortID = ts.shortID
		}
	}
	payload := eventbus.BoardEventPayload(evt.ID, evt.ItemKind, evt.ItemID, evt.Action,
		string(evt.Actor), string(evt.Payload), evt.CreatedAt.UTC().Format(time.RFC3339Nano),
		scope.owner, scope.workspaceID, shortID)
	if err := p.tr.publish(ctx, scope.workspaceID, eventbus.BoardEventTopic(evt.ProjectID), payload, &boardBusEventsPublished, &boardBusEventsFailed); err != nil {
		// THE FALLBACK (design 0072 §9.3): the bus could not take it;
		// the v1 leg delivers this event verbatim. On success the v1
		// leg is NOT called — the retired point-to-point path stays
		// retired (pinned).
		boardBusSSEFallback.Add(1)
		p.v1(evt)
		return
	}
	// The vocabulary topic for stage moves: task.{id}.stage_changed,
	// from/to parsed off the committed row's own payload
	// (moveStageResult). Fail-open warn — the board.event entry above
	// already carries the SSE plane.
	if evt.Action == "move_stage" {
		p.publishStageChanged(ctx, scope, evt, shortID)
	}
}

func (p *boardBusPublisher) publishStageChanged(ctx context.Context, scope boardBusScope, evt board.Event, shortID string) {
	var row struct {
		TaskID         string `json:"task_id"`
		FromStage      string `json:"from_stage"`
		ResultingStage string `json:"resulting_stage"`
	}
	_ = json.Unmarshal(evt.Payload, &row)
	taskID := row.TaskID
	if taskID == "" {
		taskID = evt.ItemID
	}
	source := "user"
	if strings.HasPrefix(evt.ID, "engine:") {
		source = "engine"
	} else if strings.HasPrefix(evt.ID, "reconciler:") {
		source = "reconciler"
	}
	var actorSession string
	if len(evt.Actor) > 0 {
		var a map[string]any
		if json.Unmarshal(evt.Actor, &a) == nil {
			actorSession, _ = a["session_id"].(string)
		}
	}
	if err := p.tr.publish(ctx, scope.workspaceID, eventbus.TaskStageTopic(taskID),
		eventbus.TaskStagePayload(taskID, shortID, evt.ProjectID, row.FromStage, row.ResultingStage, actorSession, source, evt.ID),
		&boardBusEventsPublished, &boardBusEventsFailed); err != nil {
		slog.WarnContext(ctx, "board: stage_changed vocabulary publish failed — dropped (the board.event entry carries the SSE plane)",
			"task", shortID, "err", err.Error())
	}
}

// drainStamps is the stamp leg's drainer (no v1 — pure projection).
func (p *boardBusPublisher) drainStamps() {
	ctx := context.Background()
	for {
		select {
		case <-p.drainClose:
			for {
				select {
				case ev := <-p.stamps:
					p.deliverStamp(ctx, ev)
				default:
					return
				}
			}
		case ev := <-p.stamps:
			p.deliverStamp(ctx, ev)
		}
	}
}

func (p *boardBusPublisher) deliverStamp(ctx context.Context, ev board.StampEvent) {
	scope, err := p.scope.scopeForTask(ctx, ev.TaskID)
	if err != nil {
		boardBusStampsFailed.Add(1)
		slog.WarnContext(ctx, "board: stamp event scope unresolved — dropped (the stamp itself landed)",
			"task", ev.TaskShortID, "origin", ev.Origin, "err", err.Error())
		return
	}
	payload := eventbus.TaskStampPayload(ev.TaskID, ev.TaskShortID, ev.ProjectID, ev.Assignee, ev.Origin, ev.LagSeconds,
		scope.owner, scope.workspaceID)
	if err := p.tr.publish(ctx, scope.workspaceID, eventbus.TaskStampedTopic(ev.TaskID), payload, &boardBusStampsPublished, &boardBusStampsFailed); err != nil {
		slog.WarnContext(ctx, "board: stamp event publish failed — dropped (the stamp itself landed)",
			"task", ev.TaskShortID, "origin", ev.Origin, "err", err.Error())
	}
}

// ---- The SSE consumer join ----

// boardSSEJoin runs the per-replica SSE consumer group on every
// workspace stream that carries board projects. Discovery is a cheap
// DISTINCT read on the projects table at a fixed cadence (Redis has
// no new-stream notification; the loop feeds consumers, it never
// gates delivery — an undiscovered workspace falls back to the 30s
// poll exactly as today).
type boardSSEJoin struct {
	tr       *boardBusClient
	group    string
	consumer string
	handler  eventbus.Handler
	discover func(context.Context) ([]string, error)

	wg     sync.WaitGroup
	mu     sync.Mutex
	stop   context.CancelFunc
	live   map[string]context.CancelFunc
	closed bool
}

// newBoardSSEJoin wires the join; nil (unwired transport or handler)
// is the OFF posture.
func newBoardSSEJoin(tr *boardBusClient, handler eventbus.Handler, discover func(context.Context) ([]string, error)) *boardSSEJoin {
	if tr == nil || handler == nil || discover == nil {
		return nil
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "api"
	}
	group := eventbus.GroupSSEPrefix + "-" + host
	return &boardSSEJoin{
		tr: tr, group: group, consumer: host, handler: handler, discover: discover,
		live: map[string]context.CancelFunc{},
	}
}

// Start runs discovery + consumer maintenance until ctx ends.
func (j *boardSSEJoin) Start(ctx context.Context) {
	if j == nil {
		return
	}
	baseCtx := ctx
	ctx, j.stop = context.WithCancel(ctx)
	defer j.teardown(baseCtx)
	j.syncOnce(ctx)
	ticker := time.NewTicker(boardSSEJoinInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.syncOnce(ctx)
		}
	}
}

// syncOnce reconciles the consumer set with the discovered
// workspaces: new streams get a consumer goroutine; gone streams
// release theirs.
func (j *boardSSEJoin) syncOnce(ctx context.Context) {
	ws, err := j.discover(ctx)
	if err != nil {
		slog.Warn("board: SSE join discovery failed — consumers hold (the poll belt covers)", "err", err.Error())
		return
	}
	want := map[string]bool{}
	for _, id := range ws {
		if id != "" {
			want[id] = true
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for id := range want {
		if _, ok := j.live[id]; ok {
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		j.live[id] = cancel
		j.wg.Add(1)
		go func(workspaceID string) {
			defer j.wg.Done()
			bus, err := j.tr.busFor(cctx, workspaceID)
			if err != nil {
				slog.Warn("board: SSE join — stream unwireable, consumer parked for this workspace",
					"workspace", workspaceID, "err", err.Error())
				return
			}
			boardBusSSEJoined.Add(1)
			err = bus.Consume(cctx, eventbus.ConsumeOptions{
				Group: j.group, Consumer: j.consumer,
				Topics:  []string{"board.*.event", "task.*.stamped"},
				StartAt: eventbus.StartLive,
				OnError: func(msg eventbus.Message, err error) {
					slog.Warn("board: SSE join consumer round failed (retrying)", "workspace", workspaceID, "err", err.Error())
				},
			}, j.handler)
			if err != nil {
				slog.Error("board: SSE join consumer exited", "workspace", workspaceID, "err", err.Error())
			}
		}(id)
	}
	for id, cancel := range j.live {
		if want[id] {
			continue
		}
		cancel()
		delete(j.live, id)
	}
}

// teardown releases every consumer and destroys this replica's group
// on every stream it touched — a graceful shutdown leaves no stalled
// sse-* group for S3's detector to flag (a crashed replica's leftover
// IS flagged; that is the detector doing its job).
func (j *boardSSEJoin) teardown(ctx context.Context) {
	j.mu.Lock()
	for _, cancel := range j.live {
		cancel()
	}
	j.live = map[string]context.CancelFunc{}
	j.closed = true
	j.mu.Unlock()
	j.wg.Wait()
	// The caller's ctx is canceled by the time teardown runs (Start's
	// defer fires after ctx ends) — the destroy keeps the ctx's VALUES
	// but strips its cancellation, bounded by one budget.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boardBusPublishBudget)
	defer cancel()
	for _, ws := range j.tr.streams() {
		if err := j.tr.client.XGroupDestroy(dctx, eventbus.StreamKey(ws), j.group).Err(); err != nil {
			slog.Warn("board: SSE join group destroy failed (a leftover sse group will be flagged by the bus-health detector)",
				"workspace", ws, "group", j.group, "err", err.Error())
		}
	}
}

// boardBusPublisherOrV1 is the install-time choice: the bus-first
// publisher when armed, the v1 leg verbatim when not (nil both = the
// engine-dark posture — no publisher installed, unchanged).
func boardBusPublisherOrV1(p *boardBusPublisher, v1 board.EventPublisher) board.EventPublisher {
	if p == nil {
		return v1
	}
	return p.Publisher()
}

// DiscoverBoardWorkspaces is the production discovery read: every
// workspace that carries a board project.
func DiscoverBoardWorkspaces(pool *pgxpool.Pool) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		rows, err := pool.Query(ctx, `SELECT DISTINCT target_workspace_id::text FROM projects`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, rows.Err()
	}
}
