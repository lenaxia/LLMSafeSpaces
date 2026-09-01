// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentpush isolates the live-push flow that builds a
// workspace's secret batch (session-independent, via the one builder)
// and delivers it to the running workspace pod's agentd via HTTP. It
// exists as a service (not a handler method) because multiple call
// sites need it:
//
//   - SetBindings (handler) — user toggled a binding in the settings drawer.
//   - ReloadSecrets (handler) — explicit POST /workspaces/:id/reload-secrets.
//   - secretautopush — watcher-driven push after pod recreation
//     (worklog 0591), the reason this package was extracted.
//
// Batch construction carries no session identity (design 0052 §4.1,
// US-70.2): the builder decrypts user entries via the server-side DEK
// unwrap, so every pusher — request-bound or background — produces the
// same batch for the same workspace state.
//
// See worklog 0589 for the design rationale — this package is the
// concrete satisfier of the SecretPusher interface defined by consumers.
package agentpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// BatchBuilder is the minimum surface Push needs to build the payload:
// the one workspace batch builder, degrading loudly (machine-readable
// reason) when the user's DEK cannot be unwrapped. Satisfied by
// *secrets.SecretService.BuildWorkspaceBatch.
type BatchBuilder interface {
	BuildWorkspaceBatch(ctx context.Context, ownerUserID, workspaceID string) (*secrets.Batch, *secrets.BuildDegrade, error)
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

// PasswordProvider resolves the workspace's agentd Basic-auth password.
// agentd rejects unauthenticated reload-secrets posts (#848); satisfied
// by interfaces.WorkspacePasswordProvider (US-46.11).
type PasswordProvider interface {
	WorkspacePassword(ctx context.Context, workspaceID string) (string, error)
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
	// ErrNoPasswordProvider — the service was constructed without a
	// password provider. A wiring bug; agentd enforces Basic auth on
	// reload-secrets (#848) so the dispatch cannot succeed.
	ErrNoPasswordProvider = errors.New("agentpush: password provider not configured")
)

// Service is the concrete SecretPusher.
type Service struct {
	builder     BatchBuilder
	podResolver PodIPResolver
	passwords   PasswordProvider
	modelCache  ModelCache
	logger      pkginterfaces.LoggerInterface
	httpClient  *http.Client
	metricsHook func(outcome string) // optional; nil = no metric
}

// New builds a Service. Only builder is required; podResolver may be
// nil during early wiring in which case Push returns ErrNoPodIPResolver.
// modelCache and logger are optional.
func New(builder BatchBuilder, opts ...Option) *Service {
	s := &Service{
		builder:    builder,
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

// WithPasswordProvider installs the workspace-password lookup required
// for the authenticated reload-secrets dispatch (#848).
func WithPasswordProvider(p PasswordProvider) Option {
	return func(s *Service) { s.passwords = p }
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

// Push builds the workspace batch (the one builder — session identity
// plays no role in what decrypts) and posts the mixed-fleet legacy body
// to the workspace pod's agentd.
//
// A degraded build (user DEK unavailable) is still pushed — the
// server-KEK subset keeps the pod's platform providers alive — but the
// degrade is logged at Warn with its machine-readable reason (I10: no
// silent partials).
//
// Empty payloads ('[]') are still sent — agentd uses them to CLEAR its
// in-memory secret materialisations. Without this, an unbind would leave
// the live pod with stale plaintext until restart.
func (s *Service) Push(ctx context.Context, userID, workspaceID string) (Result, error) {
	batch, degrade, err := s.builder.BuildWorkspaceBatch(ctx, userID, workspaceID)
	if err != nil {
		s.emitMetric("inject_failed")
		s.warn("agentpush: build workspace batch failed",
			"workspaceID", workspaceID, "error", err.Error())
		return Result{}, fmt.Errorf("build workspace batch: %w", err)
	}
	if degrade != nil {
		s.warn("agentpush: workspace batch degraded; pushing server-KEK subset",
			"workspaceID", workspaceID, "reason", degrade.Reason)
	}
	secretsJSON := secrets.LegacyBatchJSON(*batch)

	if s.podResolver == nil {
		return Result{}, ErrNoPodIPResolver
	}
	podIP, err := s.podResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil || podIP == "" {
		s.emitMetric("no_pod")
		return Result{}, ErrNoRunningPod
	}

	if s.passwords == nil {
		s.emitMetric("reload_failed")
		return Result{}, ErrNoPasswordProvider
	}
	password, err := s.passwords.WorkspacePassword(ctx, workspaceID)
	if err != nil {
		s.emitMetric("reload_failed")
		s.warn("agentpush: resolve workspace password failed",
			"workspaceID", workspaceID, "error", err.Error())
		return Result{}, fmt.Errorf("resolve workspace password: %w", err)
	}

	agentdURL := fmt.Sprintf("http://%s:4097/v1/reload-secrets", podIP)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentdURL, bytes.NewReader(secretsJSON))
	if err != nil {
		s.emitMetric("reload_failed")
		return Result{}, fmt.Errorf("build reload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+password)))

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
