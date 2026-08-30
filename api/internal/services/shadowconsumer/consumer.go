// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package shadowconsumer is the Epic 69 S1 shadow consumer (US-69.5): the
// API subscribes on-demand to a pod's ABI stream with the reference
// client, folds it, and diffs the fold against its own API-side dialect
// derivation (ReferenceFold — the proxy-tracker semantics). Zero
// unexplained divergence across the scenario suite is the S1 exit.
//
// Shadow only: nothing here feeds a production traffic path. The package
// is deliberately disposable after S1 (design 0055: "only the comparator
// is disposable").
package shadowconsumer

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
)

// Fold is the ABI-side view handed to the comparator (an immutable
// abiclient snapshot).
type Fold = abiclient.SessionState

// ABISource streams one pod's ABI surface via the reference client.
type ABISource struct {
	c *abiclient.Client
}

func NewABISource(hc *http.Client, baseURL string) *ABISource {
	return &ABISource{c: abiclient.New(hc, baseURL)}
}

// Stream folds the pod stream, invoking onUpdate per applied frame, until
// ctx is canceled. Reconnects on transport errors with backoff — a shadow
// consumer must survive pod restarts.
func (s *ABISource) Stream(ctx context.Context, onUpdate func(*Fold)) error {
	backoff := 500 * time.Millisecond
	const maxBackoff = 15 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.c.Stream(ctx, onUpdate)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			time.Sleep(backoff)
			if backoff*2 > maxBackoff {
				backoff = maxBackoff
			} else {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
	}
}

// ReferenceState is the API-side derivation snapshot (the tracker facts
// the comparator diffs against). Observed is the count of projectable
// events folded so far — the comparator's lag guard (the ABI fold arrives
// via the async stream; diffing against a stale fold would manufacture
// phantom divergences).
type ReferenceState struct {
	Sessions map[string]ReferenceSession
	Observed int
}

// ReferenceSession is one session's API-side derivation. PartIDs is the
// in-flight part set (distinct part IDs upserted, cleared on idle/death —
// comparable to the ABI fold's in-flight parts).
type ReferenceSession struct {
	Busy    bool
	PartIDs map[string]bool
}

func (r ReferenceSession) PartCount() int { return len(r.PartIDs) }

// ReferenceFold is the API-side dialect fold — the same semantics the
// proxy tracker derives from raw opencode events (busy from session.status,
// in-flight parts from message.part.updated upserts), kept independent
// from the pod-side translation by construction (it consumes the DIALECT,
// not the contract).
type ReferenceFold struct {
	mu       sync.Mutex
	sessions map[string]ReferenceSession
	observed int
}

func NewReferenceFold(workspaceID string) *ReferenceFold {
	return &ReferenceFold{sessions: map[string]ReferenceSession{}}
}

// ObserveDialect folds one raw opencode SSE payload.
func (r *ReferenceFold) ObserveDialect(raw []byte) {
	sid, status, partID, ok := parseDialect(raw)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed++
	s := r.sessions[sid]
	if s.PartIDs == nil {
		s.PartIDs = map[string]bool{}
	}
	switch {
	case status == "busy":
		s.Busy = true
	case status == "idle":
		s.Busy = false
		s.PartIDs = map[string]bool{} // turn over: in-flight clears
	case partID != "":
		s.PartIDs[partID] = true
	}
	r.sessions[sid] = s
}

// ObserveHarnessDeath drops every session record — the API tracker's
// onAgentDied semantics: after agent death the derived session set is not
// authoritative and is rebuilt from the store on the next reconcile (the
// same rule the ABI side enforces via reseed-from-store).
func (r *ReferenceFold) ObserveHarnessDeath() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions = map[string]ReferenceSession{}
}

// ObserveReconcile folds the API tracker's reconcile-on-generation-change:
// in-flight parts clear (live-turn state is not store truth) and busy
// follows the store. Mirrors proxy_events.go reconcileSessionState.
func (r *ReferenceFold) ObserveReconcile(sessionBusy map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.sessions {
		s.PartIDs = map[string]bool{}
		if busy, ok := sessionBusy[id]; ok {
			s.Busy = busy
		} else {
			s.Busy = false
		}
		r.sessions[id] = s
	}
	for id, busy := range sessionBusy {
		if _, ok := r.sessions[id]; !ok {
			r.sessions[id] = ReferenceSession{Busy: busy, PartIDs: map[string]bool{}}
		}
	}
}

func (r *ReferenceFold) Snapshot() ReferenceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := ReferenceState{Sessions: make(map[string]ReferenceSession, len(r.sessions)), Observed: r.observed}
	for k, v := range r.sessions {
		out.Sessions[k] = v
	}
	return out
}
