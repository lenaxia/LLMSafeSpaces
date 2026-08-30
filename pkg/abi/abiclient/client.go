// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package abiclient is the reference Go consumer of the harness ABI
// (design 0055 M1, US-69.4): it implements the client side of the
// stamped-snapshot sync protocol — apply in order, discard seq ≤ S,
// re-snapshot on projection.reseeded. The S1 comparator (US-69.5) and the
// API's shadow consumer (S2) share this implementation so the discard rule
// exists exactly once.
//
// This is a consumer of generated wire types — no hand-written wire
// structs (TestNoHandWrittenWire governs the surface path).
package abiclient

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// Client talks to one pod's ABI surface (agentd :4097 behind Basic auth —
// supply an http.Client whose transport injects the credential).
//
// Concurrency contract: one Client is used by ONE goroutine (the fold is
// single-threaded by design — the discard rule owns ordering). The states
// handed to Stream's onUpdate callback are immutable snapshots (deep
// copies) safe to retain or read from any goroutine.
type Client struct {
	svc abiconnect.HarnessABIServiceClient
}

func New(httpClient *http.Client, baseURL string, opts ...connect.ClientOption) *Client {
	return &Client{svc: abiconnect.NewHarnessABIServiceClient(httpClient, baseURL, opts...)}
}

// SessionState is the client-side fold: the pod snapshot plus live events
// with seq > Seq applied. Equivalent to the server projection's view by
// construction (TestDiscardRulePropertyFuzz).
type SessionState struct {
	Seq      uint64
	Sessions map[string]*abiv1.SessionSnapshot
}

func newState() *SessionState {
	return &SessionState{Sessions: map[string]*abiv1.SessionSnapshot{}}
}

// clone returns an immutable deep copy (handed to callbacks; the live fold
// keeps mutating the original).
func (s *SessionState) clone() *SessionState {
	out := &SessionState{Seq: s.Seq, Sessions: make(map[string]*abiv1.SessionSnapshot, len(s.Sessions))}
	for k, v := range s.Sessions {
		out.Sessions[k] = cloneSessionSnapshot(v)
	}
	return out
}

// GetSnapshot fetches one session's authoritative snapshot (I12) — a pure
// projection read; zero harness calls.
func (c *Client) GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
	resp, err := c.svc.GetSnapshot(ctx, connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: sessionID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// syncIdleWindow is how long Sync waits for further frames before
// declaring the fold current (frames arrive promptly; live pods emit
// continuously during turns — the window only needs to outlast a burst).
const syncIdleWindow = 150 * time.Millisecond

// Sync opens the events stream, applies the snapshot frame, and drains
// frames until the stream goes idle — a consistent one-shot state fetch.
// Cancel ctx to stop early.
func (c *Client) Sync(ctx context.Context) (*SessionState, error) {
	st, _, err := c.syncInner(ctx, syncIdleWindow)
	return st, err
}

func (c *Client) syncInner(ctx context.Context, idle time.Duration) (*SessionState, *abiv1.CapabilityReport, error) {
	s, err := c.svc.Events(ctx, connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = s.Close() }()
	st := newState()
	var rep *abiv1.CapabilityReport
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	for {
		if idleTimer == nil {
			idleTimer = time.NewTimer(idle)
			idleC = idleTimer.C
		} else {
			idleTimer.Reset(idle)
		}
		type frameOrErr struct {
			f   *abiv1.StreamFrame
			ok  bool
			err error
		}
		recv := make(chan frameOrErr, 1)
		go func() {
			f, ok := func() (*abiv1.StreamFrame, bool) {
				if s.Receive() {
					return s.Msg(), true
				}
				return nil, false
			}()
			recv <- frameOrErr{f, ok, nil}
		}()
		select {
		case r := <-recv:
			if !r.ok {
				if err := s.Err(); err != nil {
					return nil, nil, err
				}
				return st, rep, nil
			}
			if rep == nil {
				if snap := r.f.GetSnapshot(); snap != nil {
					applySnapshot(st, snap)
					rep = snap.GetCapabilities()
					continue
				}
			}
			if rr := r.f.GetReseeded(); rr != nil {
				fresh, frep, err := c.syncInner(ctx, idle)
				if err != nil {
					return nil, nil, err
				}
				return fresh, frep, nil
			}
			applyEvent(st, r.f.GetEvent())
		case <-idleC:
			return st, rep, nil
		case <-ctx.Done():
			return st, rep, ctx.Err()
		}
	}
}

// Stream keeps the folded state current: it applies the snapshot, then live
// events, invoking onUpdate after each applied frame. On
// projection.reseeded (mandatory re-snapshot) it transparently reconnects.
// Runs until ctx is canceled; returns the ctx error.
func (c *Client) Stream(ctx context.Context, onUpdate func(*SessionState)) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, err := c.svc.Events(ctx, connect.NewRequest(&abiv1.EventsRequest{}))
		if err != nil {
			return err
		}
		st := newState()
		for s.Receive() {
			f := s.Msg()
			seeded := false
			if snap := f.GetSnapshot(); snap != nil && st.Seq == 0 {
				applySnapshot(st, snap)
				seeded = true
			}
			if !seeded {
				if r := f.GetReseeded(); r != nil {
					// Mandatory re-snapshot (I3): drop the connection and
					// resync from a fresh stamped snapshot.
					_ = s.Close()
					break
				}
				applyEvent(st, f.GetEvent())
			}
			if onUpdate != nil {
				onUpdate(st.clone())
			}
		}
		if err := s.Err(); err != nil && ctx.Err() == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// Capabilities fetches the pod's capability report (rides the events
// stream's snapshot frame — there is deliberately no sixth op).
func (c *Client) Capabilities(ctx context.Context) (*abiv1.CapabilityReport, error) {
	_, rep, err := c.syncInner(ctx, syncIdleWindow)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return nil, connect.NewError(connect.CodeInternal, errText("stream closed without a snapshot frame"))
	}
	return rep, nil
}

// Act issues a typed action (surface-gated until US-69.9; undeclared
// actions return typed NotSupported via connect error details).
func (c *Client) Act(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	resp, err := c.svc.Act(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func applySnapshot(st *SessionState, snap *abiv1.SnapshotFrame) {
	st.Seq = snap.GetAtSeq()
	for _, s := range snap.GetSnapshot().GetSessions() {
		st.Sessions[s.GetSessionId()] = cloneSessionSnapshot(s)
	}
}

// applyEvent implements the client discard rule: events with seq ≤ the
// snapshot stamp (or already applied) are dropped; the rest fold.
func applyEvent(st *SessionState, seqed *abiv1.SequencedEvent) {
	if seqed == nil {
		return
	}
	if seqed.Seq <= st.Seq {
		return // discard ≤ S (duplicate application direction)
	}
	st.Seq = seqed.Seq
	evt := seqed.GetEvent()
	if evt == nil {
		return
	}
	sid := evt.GetSessionId()
	snap := st.Sessions[sid]
	if snap == nil {
		// A session observed before any status event is UNKNOWN — the
		// same convention as the server projection (never UNSPECIFIED:
		// the zero value is not a valid status).
		snap = &abiv1.SessionSnapshot{SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_UNKNOWN}
		st.Sessions[sid] = snap
	}
	switch evt.GetType() {
	case abiv1.EventType_EVENT_TYPE_SESSION_STATUS:
		snap.Status = evt.GetStatus()
		if evt.GetStatus() == abiv1.SessionStatus_SESSION_STATUS_IDLE {
			// Turn over: the server projection clears in-flight parts on
			// idle — the fold must mirror it or reconnects show stale
			// parts.
			snap.InFlightParts = nil
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_REQUEST:
		if in := evt.GetInput(); in != nil {
			snap.PendingInputs = append(snap.PendingInputs, in)
		}
	case abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED:
		if in := evt.GetInput(); in != nil {
			next := snap.PendingInputs[:0]
			for _, p := range snap.PendingInputs {
				if p.GetId() != in.GetId() {
					next = append(next, p)
				}
			}
			snap.PendingInputs = next
		}
	case abiv1.EventType_EVENT_TYPE_PART_START, abiv1.EventType_EVENT_TYPE_PART_END:
		if p := evt.GetPart(); p != nil {
			upsertPart(snap, p)
		}
	case abiv1.EventType_EVENT_TYPE_PART_DELTA:
		pid := evt.GetPartId()
		for i, p := range snap.InFlightParts {
			if p.GetId() == pid {
				snap.InFlightParts[i].Payload = &abiv1.Part_Text{Text: p.GetText() + evt.GetDelta()}
			}
		}
	}
}

func upsertPart(snap *abiv1.SessionSnapshot, p *abiv1.Part) {
	for i, existing := range snap.InFlightParts {
		if existing.GetId() == p.GetId() {
			snap.InFlightParts[i] = p
			return
		}
	}
	snap.InFlightParts = append(snap.InFlightParts, p)
}

func cloneSessionSnapshot(s *abiv1.SessionSnapshot) *abiv1.SessionSnapshot {
	out := &abiv1.SessionSnapshot{
		SessionId:     s.GetSessionId(),
		Status:        s.GetStatus(),
		QueueDepth:    s.GetQueueDepth(),
		InFlightParts: append([]*abiv1.Part(nil), s.GetInFlightParts()...),
		PendingInputs: append([]*abiv1.InputRequest(nil), s.GetPendingInputs()...),
	}
	return out
}

type errText string

func (e errText) Error() string { return string(e) }
