// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretautopush wires the workspace watcher's per-CRD-event
// callback to a fire-and-forget push of user-DEK secrets to the
// workspace pod's agentd. Runs when:
//
//   - CRD phase == Active
//   - CRD status.UserCredsPresent == false (controller has scraped
//     agentd and confirmed no user-DEK content is materialized)
//   - user has at least one binding in user_secret_bindings for this
//     workspace
//   - a DEK is retrievable for the workspace's owner (any active JWT
//     session for the user works — see KeyService.GetDEKForUser)
//
// Any of those checks failing → skip. The next watch event or the
// controller's next health scrape will retry naturally.
//
// Idempotency: an in-flight lock keyed on workspaceID prevents
// concurrent pushes for the same workspace. Watcher may emit many
// Modified events for a single CRD update; the lock ensures we push
// exactly once and the subsequent events see "already in flight,
// skip." Lock releases when the push goroutine (successful or not)
// completes; a future recreation will re-acquire and re-push.
//
// This service is deliberately thin: composition of KeyService,
// bindings storage, and agentpush.Service. See worklog 0591 for the
// design rationale and the alternatives considered.
package secretautopush

import (
	"context"
	"sync"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// DEKRetriever exposes the KeyService.GetDEKForUser primitive. The
// consumer needs both the DEK plaintext (unused directly — it's cached
// in Redis as a side effect) and the jti (used as sessionID in the
// auth ctx handed to the pusher, so pusher's downstream GetDEK call
// hits the just-populated cache).
type DEKRetriever interface {
	GetDEKForUser(ctx context.Context, userID string) (dek []byte, jti string, err error)
}

// BindingsChecker reports whether a workspace has any bound user
// secrets. If false, there's nothing user-DEK-encrypted to push — the
// service skips.
type BindingsChecker interface {
	UserHasBoundSecrets(ctx context.Context, workspaceID string) (bool, error)
}

// SecretPusher is the concrete push side (satisfied by
// *agentpush.Service). The Push method is expected to read sessionID +
// matchedSigningKey from ctx (via agentpush.WithAuth) and use them for
// downstream GetDEK — so the caller must build the auth ctx before
// calling.
type SecretPusher interface {
	Push(ctx context.Context, userID, workspaceID string) error
}

// AuthContexter builds an auth-carrying context.Context that
// SecretPusher.Push can consume. Satisfied by a small wiring adapter
// in app.New that calls agentpush.WithAuth internally so this package
// doesn't import agentpush.
type AuthContexter interface {
	WithAuth(ctx context.Context, sessionID string, dek []byte) context.Context
}

// Service is the auto-push consumer. Construct with New; wire
// OnWorkspaceUpdate into the workspace watcher's
// SetWorkspaceUpdateCallback in app.New.
type Service struct {
	dek            DEKRetriever
	bindings       BindingsChecker
	pusher         SecretPusher
	authCtxBuilder AuthContexter
	logger         pkginterfaces.LoggerInterface
	metrics        MetricsHook

	// inFlightMu guards inFlight.
	inFlightMu sync.Mutex
	// inFlight is the set of workspaceIDs currently being pushed.
	// Presence means "skip a duplicate fire"; removal happens in the
	// push goroutine's defer.
	inFlight map[string]struct{}
}

// MetricsHook is a callback for outcome recording. Optional — nil is
// silently skipped. Outcomes: "success" | "no_bindings" |
// "no_creds_yet" | "dek_unavailable" | "bindings_error" |
// "push_error" | "skipped_in_flight" | "skipped_not_active" |
// "skipped_ucp_true".
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

// WithAuthContexter installs the auth-ctx builder. If unset, the
// service uses a no-op that doesn't attach any auth to the ctx (the
// pusher's downstream GetDEK will then rehydrate from Redis using the
// jti sessionID — which works iff the DEK was just cached by
// DEKRetriever.GetDEKForUser). Production wiring installs a real
// agentpush.WithAuth-based contexter for future-proofing.
func WithAuthContexter(a AuthContexter) Option {
	return func(s *Service) { s.authCtxBuilder = a }
}

// New constructs a Service. dek + bindings + pusher are required;
// logger and metrics are optional.
func New(dek DEKRetriever, bindings BindingsChecker, pusher SecretPusher, opts ...Option) *Service {
	s := &Service{
		dek:            dek,
		bindings:       bindings,
		pusher:         pusher,
		authCtxBuilder: noopAuthCtxBuilder{},
		inFlight:       make(map[string]struct{}),
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
	go s.run(context.Background(), ws.Name, userID)
}

// run performs the actual bindings-check + DEK-fetch + push. Runs on
// a fresh context.Background() so the watch loop's ctx (which may
// cancel on shutdown) doesn't abort in-flight pushes mid-send. The
// per-workspace lock ensures at most one goroutine is here for a
// given workspaceID.
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

	// DEK retrieval. GetDEKForUser writes back to Redis under the
	// returned jti — a subsequent agentpush.Push whose ctx carries
	// (jti as sessionID) hits the cache.
	_, jti, err := s.dek.GetDEKForUser(ctx, userID)
	if err != nil {
		s.warn("secretautopush: DEK unavailable; skipping push",
			"workspaceID", workspaceID, "userID", userID, "error", err.Error())
		s.emit("dek_unavailable")
		return
	}

	// Build the auth ctx so downstream InjectSecrets can locate the
	// DEK via the standard GetDEK(sessionID, ...) path.
	authCtx := s.authCtxBuilder.WithAuth(ctx, jti, nil)

	if err := s.pusher.Push(authCtx, userID, workspaceID); err != nil {
		s.warn("secretautopush: push failed",
			"workspaceID", workspaceID, "error", err.Error())
		s.emit("push_error")
		return
	}
	s.info("secretautopush: pushed user-DEK secrets after pod recreation",
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

// noopAuthCtxBuilder returns ctx unchanged. Used when no AuthContexter is
// wired in; the pusher's downstream GetDEK still works because the
// DEKRetriever call already populated the Redis cache under the jti.
type noopAuthCtxBuilder struct{}

func (noopAuthCtxBuilder) WithAuth(ctx context.Context, _ string, _ []byte) context.Context {
	return ctx
}
