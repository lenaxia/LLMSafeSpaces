// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretautopush wires the workspace watcher's per-CRD-event
// callback to a fire-and-forget push of the workspace's secret batch to
// the workspace pod's agentd. Runs when:
//
//   - CRD phase == Active
//   - CRD status.UserCredsPresent == false (controller has scraped
//     agentd and confirmed no user-DEK content is materialized)
//   - user has at least one binding in user_secret_bindings for this
//     workspace
//
// Any of those checks failing → skip. The next watch event or the
// controller's next health scrape will retry naturally.
//
// The batch builder is session-independent (US-70.2): no DEK priming,
// no jti handoff — the pusher decrypts user entries via the
// server-side DEK unwrap on its own. This service is deliberately
// thin: composition of bindings storage and agentpush.Service. See
// worklog 0591 for the design rationale and the alternatives
// considered. US-70.5 demolishes this service in favor of reconcile.
package secretautopush

import (
	"context"
	"sync"
	"time"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// BindingsChecker reports whether a workspace has any bound user
// secrets. If false, there's nothing user-DEK-encrypted to push — the
// service skips.
type BindingsChecker interface {
	UserHasBoundSecrets(ctx context.Context, workspaceID string) (bool, error)
}

// SecretPusher is the concrete push side (satisfied by
// *agentpush.Service): builds the workspace batch (session-free) and
// delivers it.
type SecretPusher interface {
	Push(ctx context.Context, userID, workspaceID string) error
}

// Service is the auto-push consumer. Construct with New; wire
// OnWorkspaceUpdate into the workspace watcher's
// SetWorkspaceUpdateCallback in app.New.
type Service struct {
	bindings BindingsChecker
	pusher   SecretPusher
	logger   pkginterfaces.LoggerInterface
	metrics  MetricsHook

	// inFlightMu guards inFlight.
	inFlightMu sync.Mutex
	// inFlight is the set of workspaceIDs currently being pushed.
	// Presence means "skip a duplicate fire"; removal happens in the
	// push goroutine's defer.
	inFlight map[string]struct{}
}

// MetricsHook is a callback for outcome recording. Optional — nil is
// silently skipped. Outcomes: "success" | "no_bindings" |
// "no_creds_yet" | "bindings_error" | "push_error" |
// "skipped_in_flight" | "skipped_not_active" | "skipped_ucp_true".
type MetricsHook func(outcome string)

// Option configures the Service.
type Option func(*Service)

// WithLogger installs a logger for auto-push events.
func WithLogger(l pkginterfaces.LoggerInterface) Option {
	return func(s *Service) { s.logger = l }
}

// WithMetricsHook installs an outcome callback.
func WithMetricsHook(fn MetricsHook) Option {
	return func(s *Service) { s.metrics = fn }
}

// New constructs a Service. bindings + pusher are required; logger and
// metrics are optional.
func New(bindings BindingsChecker, pusher SecretPusher, opts ...Option) *Service {
	s := &Service{
		bindings: bindings,
		pusher:   pusher,
		inFlight: make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// OnWorkspaceUpdate is the workspace watcher's per-event callback. See
// package docstring for the filter conditions. Returns fast: any
// actual push happens in a fire-and-forget goroutine so the watch
// loop is never blocked.
func (s *Service) OnWorkspaceUpdate(ws *v1.Workspace) {
	if ws == nil {
		return
	}
	// Filter: Active phase only. Non-Active workspaces have no
	// reachable agentd or are mid-terminating.
	if ws.Status.Phase != v1.WorkspacePhaseActive {
		s.emit("skipped_not_active")
		return
	}
	// Filter: UserCredsPresent MUST be explicitly false. nil means
	// "controller hasn't scraped" — treating as false would produce
	// a stampede on API restart. true means agentd already has creds.
	if ws.Status.UserCredsPresent == nil {
		s.emit("no_creds_yet")
		return
	}
	if *ws.Status.UserCredsPresent {
		s.emit("skipped_ucp_true")
		return
	}
	// Filter: workspace owner must be non-empty.
	userID := ws.Spec.Owner.UserID
	if userID == "" {
		return
	}

	// Acquire in-flight lock keyed on workspaceID. If already in
	// flight, skip — the running push will handle this same state.
	s.inFlightMu.Lock()
	if _, exists := s.inFlight[ws.Name]; exists {
		s.inFlightMu.Unlock()
		s.emit("skipped_in_flight")
		return
	}
	s.inFlight[ws.Name] = struct{}{}
	s.inFlightMu.Unlock()

	// Fire-and-forget. The goroutine owns the lock removal via defer.
	// Bounded by autoPushTimeout so a hung agentd (accepting TCP but
	// never responding) or a hung DEK retrieval doesn't leak the
	// goroutine indefinitely — the underlying HTTP clients have their
	// own timeouts, but this is defense-in-depth.
	ctx, cancel := context.WithTimeout(context.Background(), autoPushTimeout)
	go func() {
		defer cancel()
		s.run(ctx, ws.Name, userID)
	}()
}

// autoPushTimeout bounds one auto-push attempt (bindings query + DEK
// fetch + push HTTP call + optional cache writes). Set generously
// relative to the underlying HTTP clients (agentpush uses 5s per
// call; DEKRetriever's PG query is sub-100ms; bindings check is a
// single indexed lookup): 30s covers the worst case plus retries
// with margin. Not user-tunable — this is a defense-in-depth guard,
// not a business-logic knob.
const autoPushTimeout = 30 * time.Second

// run performs the actual bindings-check + push. Runs on a fresh
// context.Background() so the watch loop's ctx (which may cancel on
// shutdown) doesn't abort in-flight pushes mid-send. The per-workspace
// lock ensures at most one goroutine is here for a given workspaceID.
//
// NOTE (epic #1158): the bindings check queries
// user_secret_bindings.workspace_id, a uuid column, with the CR name —
// for non-uuid CR names this errors with "invalid input syntax for
// type uuid" (the error-loop noted in the epic). The check is retained
// as-is: US-70.5 demolishes this service in favor of the reconcile
// loop; fixing the query here would be throwaway.
func (s *Service) run(ctx context.Context, workspaceID, userID string) {
	defer func() {
		s.inFlightMu.Lock()
		delete(s.inFlight, workspaceID)
		s.inFlightMu.Unlock()
	}()

	// Bindings check. Skip if no bindings or if the query errors.
	// Both cases have the same effect: no push. The distinction
	// exists only for observability (bindings_error vs no_bindings).
	has, err := s.bindings.UserHasBoundSecrets(ctx, workspaceID)
	if err != nil {
		s.warn("secretautopush: bindings check failed; skipping push",
			"workspaceID", workspaceID, "error", err.Error())
		s.emit("bindings_error")
		return
	}
	if !has {
		s.emit("no_bindings")
		return
	}

	if err := s.pusher.Push(ctx, userID, workspaceID); err != nil {
		s.warn("secretautopush: push failed",
			"workspaceID", workspaceID, "error", err.Error())
		s.emit("push_error")
		return
	}
	s.info("secretautopush: pushed workspace secrets after pod recreation",
		"workspaceID", workspaceID)
	s.emit("success")
}

func (s *Service) emit(outcome string) {
	if s.metrics != nil {
		s.metrics(outcome)
	}
}

func (s *Service) warn(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Warn(msg, fields...)
	}
}

func (s *Service) info(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Info(msg, fields...)
	}
}
