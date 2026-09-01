// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package usagestream is the API's busy-gated consumer of a pod's ABI
// contract stream (US-69.11, design 0055 S3 + D1-B + the owner's billing
// disposition): while any session of a workspace is in flight, the API
// holds ONE subscription to the pod's event stream and derives —
//
//   - per-step usage (MESSAGE_END tokens/model) for billing/metering,
//     with deterministic idempotency keys so the DB's unique constraint
//     makes multi-replica billing exactly-once;
//   - the user-stream state bridge (SESSION_STATUS → session.status,
//     INPUT_REQUEST/RESOLVED → agent.question/permission) — Epic 28's
//     cross-workspace surface keeps its wire shape, with the source
//     swapped from the retired tracker's dialect translation;
//   - context-usage persistence and session-title persistence;
//   - agent-death detection (the subscription dying after delivering
//     frames).
//
// When the fold reports no busy session for the settle window, the gate
// drops the connection — on idle pods the API holds no pod streams
// (scale-to-zero; D1-B). The display path (/contract-events) is a
// separate, browser-refcounted consumer; this one is activity-gated.
package usagestream

import (
	"context"
	"sync"
	"time"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// Resolve yields the pod's ABI base URL + password for a workspace.
type Resolve func(ctx context.Context, workspaceID string) (baseURL, password string, err error)

// Client is the subset of abiclient.Client the consumer needs (the test
// seam).
type Client interface {
	Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error
}

// NewClient builds a Client for a resolved pod endpoint.
type NewClient func(baseURL, password string) Client

// Usage is one billable step: the MESSAGE_END cost record plus the
// identity needed for a deterministic idempotency key.
type Usage struct {
	SessionID  string
	MessageID  string
	Seq        uint64
	ModelID    string
	ProviderID string

	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// Billing receives billable usage. Implementations build idempotency
// keys from (WorkspaceID, MessageID, Seq) — deterministic across
// replicas — and record metering + metrics.
type Billing interface {
	RecordUsage(workspaceID string, u Usage)
}

// BillingFunc adapts a function to Billing.
type BillingFunc func(workspaceID string, u Usage)

func (f BillingFunc) RecordUsage(workspaceID string, u Usage) { f(workspaceID, u) }

// Bridge receives the derived state changes the platform still owns:
// the Epic 28 user-stream event surface, context/title persistence, and
// agent-death notification.
type Bridge interface {
	// SessionStatus fires on BUSY (or COMPACTING) and IDLE transitions.
	SessionStatus(workspaceID, sessionID string, busy bool)
	// InputRequested / InputResolved carry the unified pending-input
	// lifecycle.
	InputRequested(workspaceID string, req *abiv1.InputRequest)
	InputResolved(workspaceID, sessionID, inputID string)
	// SessionTitle fires when a session update carries a title.
	SessionTitle(workspaceID, sessionID, title string)
	// ContextUsed carries the per-step context occupancy numerator
	// (input + cacheRead + cacheWrite).
	ContextUsed(workspaceID, sessionID string, used int64)
	// AgentDied fires when the pod stream dies after delivering frames.
	AgentDied(workspaceID string)
}

// Logger is the minimal seam (the API's LoggerInterface satisfies it).
type Logger interface {
	Warn(msg string, keysAndValues ...interface{})
}

// Default gate tuning.
const (
	DefaultIdleDrop = 30 * time.Second // all-idle settle window before the gate drops
	DefaultRetry    = 2 * time.Second  // reconnect backoff after a stream error
)

// Config wires the consumer. Resolve and NewClient are required; the
// rest may be nil (defaults apply).
type Config struct {
	Resolve   Resolve
	NewClient NewClient
	Billing   Billing
	Bridge    Bridge
	Logger    Logger

	// IdleDrop is the settle window the fold must report zero busy
	// sessions for before the gate closes (default 30s).
	IdleDrop time.Duration
	// Retry is the reconnect backoff after a stream error (default 2s).
	Retry time.Duration
}

// Consumer owns one busy-gated pod subscription per workspace.
type Consumer struct {
	cfg   Config
	mu    sync.Mutex
	gates map[string]*gate
}

type gate struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	busyNow   bool      // the last published fold state had a busy session
	idleSince time.Time // when the fold last reported all-idle (zero while busy)
	frames    bool      // any frame received on the current connection
}

func New(cfg Config) *Consumer {
	if cfg.IdleDrop <= 0 {
		cfg.IdleDrop = DefaultIdleDrop
	}
	if cfg.Retry <= 0 {
		cfg.Retry = DefaultRetry
	}
	return &Consumer{cfg: cfg, gates: map[string]*gate{}}
}

// Open arms the workspace's gate (idempotent). The subscription starts
// immediately; activity keeps it alive.
func (c *Consumer) Open(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.gates[workspaceID]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &gate{cancel: cancel, done: make(chan struct{}), idleSince: time.Now()}
	c.gates[workspaceID] = g
	//nolint:contextcheck // the gate's lifecycle is activity-driven
	// (Open/Close + idle-drop), not request-scoped.
	go c.run(ctx, workspaceID, g)
}

// Close drops the workspace's gate (idempotent).
func (c *Consumer) Close(workspaceID string) {
	c.mu.Lock()
	g, ok := c.gates[workspaceID]
	if ok {
		delete(c.gates, workspaceID)
	}
	c.mu.Unlock()
	if ok {
		g.cancel()
		<-g.done
	}
}

// Gates reports the number of open gates (the scale-to-zero observable).
func (c *Consumer) Gates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.gates)
}

func (c *Consumer) run(ctx context.Context, workspaceID string, g *gate) {
	defer func() {
		c.mu.Lock()
		if c.gates[workspaceID] == g {
			delete(c.gates, workspaceID)
		}
		c.mu.Unlock()
		g.cancel()
		close(g.done)
	}()

	// Idle watchdog: a quiet stream publishes no state, so the drop rule
	// needs its own clock.
	idleCheck := time.NewTicker(c.cfg.IdleDrop / 4)
	defer idleCheck.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-idleCheck.C:
				g.mu.Lock()
				idle := !g.busyNow && !g.idleSince.IsZero() && time.Since(g.idleSince) >= c.cfg.IdleDrop
				g.mu.Unlock()
				if idle {
					g.cancel()
					return
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		baseURL, password, err := c.cfg.Resolve(ctx, workspaceID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.warn("usagestream: pod resolve failed", workspaceID, err)
			if !c.sleep(ctx, c.cfg.Retry) {
				return
			}
			continue
		}
		cl := c.cfg.NewClient(baseURL, password)
		g.mu.Lock()
		g.frames = false
		g.mu.Unlock()
		err = cl.Stream(ctx, func(st *abiclient.SessionState) {
			c.onState(workspaceID, g, st)
		}, abiclient.WithAppliedEvents(func(evt *abiv1.Event, seq uint64) {
			c.onEvent(workspaceID, g, evt, seq)
		}))
		if ctx.Err() != nil {
			return
		}
		// The stream died on its own: surface death (only after frames —
		// a never-connected stream is a resolve/transport problem, not a
		// dead agent), then retry with backoff.
		g.mu.Lock()
		hadFrames := g.frames
		g.mu.Unlock()
		if hadFrames {
			c.cfg.Bridge.AgentDied(workspaceID)
		}
		c.warn("usagestream: pod stream ended", workspaceID, err)
		if !c.sleep(ctx, c.cfg.Retry) {
			return
		}
	}
}

// onState applies the idle-drop rule on every folded-state publication:
// the gate survives while the CURRENT fold has a busy session; once the
// fold reports all-idle, the settle window starts (busy again cancels
// it). A quiet-but-busy stream (long turn, no events) stays connected.
func (c *Consumer) onState(workspaceID string, g *gate, st *abiclient.SessionState) {
	busy := false
	for _, s := range st.Sessions {
		if s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_BUSY ||
			s.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_COMPACTING {
			busy = true
			break
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.busyNow = busy
	if busy {
		g.idleSince = time.Time{}
	} else if g.idleSince.IsZero() {
		g.idleSince = time.Now()
	}
}

func (c *Consumer) onEvent(workspaceID string, g *gate, evt *abiv1.Event, seq uint64) {
	g.mu.Lock()
	g.frames = true
	g.mu.Unlock()

	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_MESSAGE_END:
		msg := evt.GetMessage()
		if msg == nil || msg.GetType() != abiv1.MessageType_MESSAGE_TYPE_ASSISTANT {
			return
		}
		cost := msg.GetCost()
		if cost == nil || (cost.GetInputTokens() == 0 && cost.GetOutputTokens() == 0 && cost.GetTotalTokens() == 0 && cost.GetCostUsd() == 0) {
			return
		}
		if c.cfg.Billing != nil {
			u := Usage{
				SessionID:        evt.GetSessionId(),
				MessageID:        msg.GetId(),
				Seq:              seq,
				InputTokens:      cost.GetInputTokens(),
				OutputTokens:     cost.GetOutputTokens(),
				ReasoningTokens:  cost.GetReasoningTokens(),
				CacheReadTokens:  cost.GetCacheReadTokens(),
				CacheWriteTokens: cost.GetCacheWriteTokens(),
				CostUSD:          cost.GetCostUsd(),
			}
			if m := msg.GetModel(); m != nil {
				u.ModelID = m.GetId()
				u.ProviderID = m.GetProvider()
			}
			c.cfg.Billing.RecordUsage(workspaceID, u)
		}
		if c.cfg.Bridge != nil {
			c.cfg.Bridge.ContextUsed(workspaceID, evt.GetSessionId(),
				cost.GetInputTokens()+cost.GetCacheReadTokens()+cost.GetCacheWriteTokens())
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		if c.cfg.Bridge == nil {
			return
		}
		switch evt.GetStatus() {
		case abiv1.SessionStatus_SESSION_STATUS_BUSY, abiv1.SessionStatus_SESSION_STATUS_COMPACTING:
			c.cfg.Bridge.SessionStatus(workspaceID, evt.GetSessionId(), true)
		case abiv1.SessionStatus_SESSION_STATUS_IDLE:
			c.cfg.Bridge.SessionStatus(workspaceID, evt.GetSessionId(), false)
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if c.cfg.Bridge != nil && evt.GetInput() != nil {
			c.cfg.Bridge.InputRequested(workspaceID, evt.GetInput())
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if c.cfg.Bridge != nil && evt.GetInput() != nil {
			c.cfg.Bridge.InputResolved(workspaceID, evt.GetSessionId(), evt.GetInput().GetId())
		}
	case abiv1.EventType_EVENT_TYPE_SESSION_UPDATED:
		if c.cfg.Bridge != nil {
			if s := evt.GetSession(); s != nil && s.GetTitle() != "" {
				c.cfg.Bridge.SessionTitle(workspaceID, s.GetId(), s.GetTitle())
			}
		}
	}
}

// handleError is the death-detection seam for connections that failed
// before any state callback could run (tests and edge wiring).
func (c *Consumer) handleError(workspaceID string, hadFrames bool) {
	if hadFrames && c.cfg.Bridge != nil {
		c.cfg.Bridge.AgentDied(workspaceID)
	}
}

func (c *Consumer) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Consumer) warn(msg, workspaceID string, err error) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Warn(msg, "workspaceID", workspaceID, "error", err)
	}
}
