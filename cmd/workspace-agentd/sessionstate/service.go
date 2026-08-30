// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Handler returns the ABI surface: the generated connect service behind
// Basic auth (I8 — every route authenticated, zero exceptions). The
// returned mount path is the connect procedure root; agentd's user mux
// serves it as-is.
func (a *Authority) Handler() (string, http.Handler) {
	path, svc := abiconnect.NewHarnessABIServiceHandler(a)
	return path, a.auth(svc)
}

// auth enforces the design-0051 §D1 Basic gate with constant-time
// comparison for every configured password (the D6.1 mixed-generation
// pair {agentdPassword, workspacePassword} in production wiring).
func (a *Authority) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		match := false
		for _, pw := range a.cfg.Passwords {
			if pw == "" {
				continue
			}
			expected := "Basic " + base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+pw))
			if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1 {
				match = true // keep comparing: uniform work regardless of position
			}
		}
		if !match {
			w.Header().Set("WWW-Authenticate", `Basic realm="agentd"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- ABI service implementation (US-69.2 stage) -------------------------
//
// Events is live now (the fanout machinery is this story's deliverable).
// The four mutating/reading ops return NotSupported until US-69.4 wires
// their endpoint semantics — the surface EXISTS and is authenticated +
// rate-limited, which is what I8's "every route" audit requires.

func (a *Authority) Events(ctx context.Context, req *connect.Request[abiv1.EventsRequest], stream *connect.ServerStream[abiv1.StreamFrame]) error {
	frames, cancel, err := a.Stream(ctx)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-frames:
			if !ok {
				return nil
			}
			if err := stream.Send(f); err != nil {
				return err
			}
		}
	}
}

func (a *Authority) GetSnapshot(ctx context.Context, req *connect.Request[abiv1.GetSnapshotRequest]) (*connect.Response[abiv1.SessionSnapshot], error) {
	// Reads are NOT rate-limited (I8 bounds deliveries/actions — the
	// mutating ops; the comparator polls snapshots legitimately).
	sid := req.Msg.GetSessionId()
	if sid == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errText("session_id is required"))
	}
	a.mu.Lock()
	rec := a.sessions[sid]
	var snap *abiv1.SessionSnapshot
	if rec != nil {
		snap = sessionSnapshotLocked(sid, rec)
	}
	stamp := a.seq
	a.mu.Unlock()
	if snap == nil {
		return nil, connect.NewError(connect.CodeNotFound, errText("unknown session: "+sid))
	}
	_ = stamp
	return connect.NewResponse(snap), nil
}

func (a *Authority) Deliver(ctx context.Context, req *connect.Request[abiv1.DeliveryRequest]) (*connect.Response[abiv1.DeliveryAck], error) {
	if err := a.limiter.allow(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	return nil, notSupported("abi.deliver", "delivery ledger lands in US-69.7")
}

func (a *Authority) GetDeliveryStatus(ctx context.Context, req *connect.Request[abiv1.GetDeliveryStatusRequest]) (*connect.Response[abiv1.DeliveryStatus], error) {
	return nil, notSupported("abi.delivery_status", "delivery ledger lands in US-69.7")
}

func (a *Authority) Act(ctx context.Context, req *connect.Request[abiv1.ActionRequest]) (*connect.Response[abiv1.ActionResult], error) {
	if err := a.limiter.allow(req.Msg.GetSessionId()); err != nil {
		return nil, err
	}
	return nil, notSupported("abi.actions", "typed actions land in US-69.9")
}

func notSupported(capability, detail string) error {
	err := connect.NewError(connect.CodeUnimplemented, errText(detail))
	d, derr := connect.NewErrorDetail(&abiv1.NotSupported{Capability: capability, Detail: detail})
	if derr == nil {
		err.AddDetail(d)
	}
	return err
}

type errText string

func (e errText) Error() string { return string(e) }

// --- per-session rate limiter (I8) ---------------------------------------

type sessionLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	burst   int
	refill  float64 // tokens per second
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newSessionLimiter(cfg RateLimitConfig) *sessionLimiter {
	return &sessionLimiter{
		buckets: map[string]*bucket{},
		burst:   cfg.Burst,
		refill:  cfg.RefillPerSec,
	}
}

// allow consumes one token for the session; a denied call returns a
// connect ResourceExhausted error (HTTP 429 on the connect protocol).
func (l *sessionLimiter) allow(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errText("session_id is required"))
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[sessionID]
	if !ok {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[sessionID] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.refill
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return connect.NewError(connect.CodeResourceExhausted, errText("per-session rate limit exceeded"))
	}
	b.tokens--
	// Opportunistic pruning keeps the map bounded by ACTIVE sessions.
	if len(l.buckets) > 4096 {
		for id, bb := range l.buckets {
			if now.Sub(bb.last) > 10*time.Minute {
				delete(l.buckets, id)
			}
		}
	}
	return nil
}
