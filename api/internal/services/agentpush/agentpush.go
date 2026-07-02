// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentpush isolates the live-push flow that decrypts a user's
// bound secret snapshot with their DEK and delivers it to the running
// workspace pod's agentd via HTTP. It exists as a service (not a handler
// method) because multiple call sites need it:
//
//   - SetBindings (handler) — user toggled a binding in the settings drawer.
//   - ReloadSecrets (handler) — explicit POST /workspaces/:id/reload-secrets.
//   - workspace.Service.GetWorkspaceStatus — auto-push on pod-identity
//     transition (worklog 0589), the reason this package was extracted.
//
// The service takes sessionID and matchedSigningKey from context (via the
// package-provided helpers) rather than as function args so callers that
// only have a context.Context (i.e., non-handler callers like the workspace
// status reader) can supply them the same way handlers do. Handlers that
// hold a *gin.Context should first build ctx = agentpush.WithAuth(ctx,
// sessionID, matchedSigningKey) before calling Push.
//
// See worklog 0589 for the design rationale — this package is the
// concrete satisfier of the SecretPusher interface defined by consumers
// (workspace.Service and the handlers).
package agentpush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// SecretInjector is the minimum surface Push needs to build the payload:
// decrypt all bound secrets for the workspace with the user's DEK,
// degrading to skip-with-audit for user-DEK entries when the DEK is
// unavailable. Satisfied by *secrets.SecretService.InjectSecrets.
type SecretInjector interface {
	InjectSecrets(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, workspaceID string) ([]byte, error)
}

// PodIPResolver looks up the running pod IP for a workspace. Returns an
// empty string (and nil error, or a wrapped one) when no pod is running.
type PodIPResolver interface {
	GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error)
}

// ModelCache is invalidated after a successful push so ListModels reflects
// the fresh provider set. Optional; nil skips the eviction.
type ModelCache interface {
	Evict(workspaceID string)
}

// Result summarizes what agentd did with the pushed payload.
type Result struct {
	Reloaded  int  `json:"reloaded"`
	Restarted bool `json:"restarted"`
}

// Sentinels — callers switch on these to map to HTTP status codes.
var (
	// ErrNoPodIPResolver — the service was constructed without a resolver.
	// A wiring bug; the service can't deliver anything and should not have
	// been used. Return 503 at the HTTP boundary.
	ErrNoPodIPResolver = errors.New("agentpush: pod IP resolver not configured")
	// ErrNoRunningPod — the workspace exists but has no reachable pod
	// right now (Pending, Suspended, or recreating). Not a hard failure
	// for the push flow: user-initiated callers surface 409, the
	// pod-recreation auto-push logs at info and increments the "no_pod"
	// metric outcome (this is transient, expected during pod boot races).
	ErrNoRunningPod = errors.New("agentpush: workspace has no running pod")
)

// Service is the concrete SecretPusher.
type Service struct {
	injector    SecretInjector
	podResolver PodIPResolver
	modelCache  ModelCache
	logger      pkginterfaces.LoggerInterface
	httpClient  *http.Client
	metricsHook func(outcome string) // optional; nil = no metric
}

// New builds a Service. Only injector is required; podResolver may be
// nil during early wiring in which case Push returns ErrNoPodIPResolver.
// modelCache and logger are optional.
func New(injector SecretInjector, opts ...Option) *Service {
	s := &Service{
		injector:   injector,
		httpClient: defaultHTTPClient(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Option configures a Service.
type Option func(*Service)

// WithPodIPResolver installs the pod-IP lookup.
func WithPodIPResolver(r PodIPResolver) Option {
	return func(s *Service) { s.podResolver = r }
}

// WithModelCache installs the cache to evict after a successful push.
func WithModelCache(c ModelCache) Option {
	return func(s *Service) { s.modelCache = c }
}

// WithLogger installs the logger used for non-fatal warnings.
func WithLogger(l pkginterfaces.LoggerInterface) Option {
	return func(s *Service) { s.logger = l }
}

// WithHTTPClient overrides the default 5s-timeout HTTP client (tests).
func WithHTTPClient(c *http.Client) Option {
	return func(s *Service) { s.httpClient = c }
}

// WithMetricsHook installs an outcome-recording callback used by the
// pod-recreation auto-push path. Outcomes: "success", "inject_failed",
// "reload_failed", "no_pod". Optional; nil is silently skipped.
func WithMetricsHook(hook func(outcome string)) Option {
	return func(s *Service) { s.metricsHook = hook }
}

// defaultHTTPClient bounds the reload call so a hung agent can't block
// the caller's request goroutine indefinitely. 5s covers a healthy
// agent (sub-100ms in practice) with margin for transient network jitter.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// Push runs InjectSecrets to build the encrypted payload with the user's
// DEK (from ctx auth) and posts it to the workspace pod's agentd. Callers
// MUST set sessionID and matchedSigningKey on ctx via WithAuth before
// calling; otherwise the injector degrades to skip-with-audit for
// user-DEK entries (phase-1 outcome).
//
// Empty payloads ('[]') are still sent — agentd uses them to CLEAR its
// in-memory secret materialisations. Without this, an unbind would leave
// the live pod with stale plaintext until restart.
func (s *Service) Push(ctx context.Context, userID, workspaceID string) (Result, error) {
	sessionID, matchedSigningKey := AuthFromContext(ctx)

	secretsJSON, err := s.injector.InjectSecrets(ctx, userID, sessionID, matchedSigningKey, workspaceID)
	if err != nil {
		s.emitMetric("inject_failed")
		s.warn("agentpush: inject secrets failed",
			"workspaceID", workspaceID, "error", err.Error())
		return Result{}, fmt.Errorf("inject secrets: %w", err)
	}

	if s.podResolver == nil {
		return Result{}, ErrNoPodIPResolver
	}
	podIP, err := s.podResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil || podIP == "" {
		s.emitMetric("no_pod")
		return Result{}, ErrNoRunningPod
	}

	agentdURL := fmt.Sprintf("http://%s:4097/v1/reload-secrets", podIP)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentdURL, bytes.NewReader(secretsJSON))
	if err != nil {
		s.emitMetric("reload_failed")
		return Result{}, fmt.Errorf("build reload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.emitMetric("reload_failed")
		s.warn("agentpush: reload request failed",
			"workspaceID", workspaceID, "error", err.Error())
		return Result{}, fmt.Errorf("failed to reach workspace agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var agentErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&agentErr)
		msg := "agent reload failed"
		if agentErr.Error != "" {
			msg = agentErr.Error
		}
		s.emitMetric("reload_failed")
		s.warn("agentpush: agent returned non-200",
			"workspaceID", workspaceID, "status", resp.StatusCode, "error", msg)
		return Result{}, fmt.Errorf("%s", msg)
	}

	var result Result
	_ = json.NewDecoder(resp.Body).Decode(&result)

	if s.modelCache != nil {
		s.modelCache.Evict(workspaceID)
	}
	s.emitMetric("success")
	return result, nil
}

func (s *Service) emitMetric(outcome string) {
	if s.metricsHook != nil {
		s.metricsHook(outcome)
	}
}

func (s *Service) warn(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Warn(msg, fields...)
	}
}

// --- context-carried auth ---

type authCtxKey struct{}

type authValues struct {
	sessionID         string
	matchedSigningKey []byte
}

// WithAuth returns ctx with sessionID and matchedSigningKey attached so
// Push can decrypt the caller's DEK-bound secrets. Handlers that hold a
// *gin.Context should extract these via extractAuth + extractMatchedSigningKey
// and call WithAuth before invoking a service method that will Push.
func WithAuth(ctx context.Context, sessionID string, matchedSigningKey []byte) context.Context {
	return context.WithValue(ctx, authCtxKey{}, authValues{
		sessionID:         sessionID,
		matchedSigningKey: matchedSigningKey,
	})
}

// AuthFromContext extracts the sessionID + matchedSigningKey previously
// attached via WithAuth. Returns zero values when unset (Push then relies
// on the injector's skip-with-audit degradation for user-DEK entries).
func AuthFromContext(ctx context.Context) (string, []byte) {
	v, ok := ctx.Value(authCtxKey{}).(authValues)
	if !ok {
		return "", nil
	}
	return v.sessionID, v.matchedSigningKey
}
