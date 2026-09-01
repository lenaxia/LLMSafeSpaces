// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentpush isolates live change delivery to running workspace
// pods. US-70.3 flipped delivery from batch-body push to notify →
// re-pull: Notify posts an EMPTY, authenticated request to the pod's
// agentd /v1/resync-secrets endpoint and the pod re-pulls its batch
// through the conditional bootstrap path (fresh SA token, apply-guard,
// terminal rev anchoring). The pod-side legacy /v1/reload-secrets body
// endpoint remains for mixed-fleet compatibility but has no in-tree
// caller (US-70.5 demolishes it).
//
// Notify exists as a service (not a handler method) because multiple
// call sites need it:
//
//   - SetBindings (handler) — user toggled a binding in the settings drawer.
//   - ReloadSecrets (handler) — explicit POST /workspaces/:id/reload-secrets.
//   - workspace env mutations (handler) — env vars are bound secrets.
//   - MCP server bind/mutation handlers.
//   - secretautopush — watcher-driven notify after pod recreation.
//   - secretsreconcile — the level-triggered reconcile loop.
//   - ForceRevokeSecret fan-out — revocation is absence (I12).
//
// Notify never carries secret material: it only shrinks latency. If a
// notify fails, the reconcile loop converges the workspace on a later
// pass (I3: notify failure costs latency, never correctness).
//
// M2 (validation): a 429 that carries retryAfterMs schedules ONE
// deferred retry (capped 2.5s) so a rapid second mutation does not
// lose its notify until the next 60s loop pass. The retry runs
// detached from any request context, never blocks the bind path, is
// canceled by Stop, and never schedules a second retry — if the retry
// also 429s, the reconcile loop owns convergence.
package agentpush

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// notifyRetryMaxDelay caps the deferred retry wait a 429's retryAfterMs
// can request — anything slower belongs to the reconcile loop (M2).
const notifyRetryMaxDelay = 2500 * time.Millisecond

// PodIPResolver looks up the running pod IP for a workspace. Returns an
// empty string (and nil error, or a wrapped one) when no pod is running.
type PodIPResolver interface {
	GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error)
}

// ModelCache is invalidated after a notify the pod acted on (applied)
// so ListModels refetches against the fresh provider set. Optional;
// nil skips the eviction.
type ModelCache interface {
	Evict(workspaceID string)
}

// PasswordProvider resolves the workspace's agentd Basic-auth password.
// agentd rejects unauthenticated posts on its user mux (#848); satisfied
// by interfaces.WorkspacePasswordProvider (US-46.11).
type PasswordProvider interface {
	WorkspacePassword(ctx context.Context, workspaceID string) (string, error)
}

// NotifyResult reports what agentd did with the resync request.
type NotifyResult struct {
	Status     string `json:"status"`
	AppliedRev string `json:"appliedRev"`
	Restarted  bool   `json:"restarted"`
}

// Sentinels — callers switch on these to map to HTTP status codes.
var (
	// ErrNoPodIPResolver — the service was constructed without a resolver.
	// A wiring bug; the service can't deliver anything and should not have
	// been used. Return 503 at the HTTP boundary.
	ErrNoPodIPResolver = errors.New("agentpush: pod IP resolver not configured")
	// ErrNoRunningPod — the workspace exists but has no reachable pod
	// right now (Pending, Suspended, or recreating). Not a hard failure:
	// user-initiated callers surface 409, background callers log at info
	// and record the "no_pod" metric outcome (transient during pod boot
	// races).
	ErrNoRunningPod = errors.New("agentpush: workspace has no running pod")
	// ErrNoPasswordProvider — the service was constructed without a
	// password provider. A wiring bug; agentd enforces Basic auth on the
	// user mux (#848) so the dispatch cannot succeed.
	ErrNoPasswordProvider = errors.New("agentpush: password provider not configured")
)

// Service is the concrete notifier.
type Service struct {
	podResolver       PodIPResolver
	passwords         PasswordProvider
	modelCache        ModelCache
	logger            pkginterfaces.LoggerInterface
	httpClient        *http.Client
	notifyMetricsHook func(outcome string)

	// M2 deferred-retry lifecycle: stopCh (with stopOnce) cancels
	// pending retries; retryMu guards the timer set and the stopped
	// flag so Stop drains deterministically; retryWg tracks scheduled +
	// in-flight retries so Stop can wait for one already past
	// cancellation (bounded by the client timeout).
	stopCh   chan struct{}
	stopOnce sync.Once
	retryMu  sync.Mutex
	stopped  bool
	timers   map[*pendingRetry]struct{}
	retryWg  sync.WaitGroup
}

// pendingRetry is one armed deferred retry. done is closed by the
// timer callback (fired or canceled-before-work) so later scheduling
// rounds can prune the set without touching time.Timer's racy Stop
// semantics.
type pendingRetry struct {
	timer *time.Timer
	done  chan struct{}
}

// New builds a Service from options alone: a notify carries no payload,
// so there is no builder dependency. podResolver/passwords may be nil
// during early wiring, in which case Notify returns the corresponding
// sentinel.
func New(opts ...Option) *Service {
	s := &Service{
		httpClient: defaultHTTPClient(),
		stopCh:     make(chan struct{}),
		timers:     make(map[*pendingRetry]struct{}),
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
// for the authenticated dispatch (#848).
func WithPasswordProvider(p PasswordProvider) Option {
	return func(s *Service) { s.passwords = p }
}

// WithModelCache installs the cache to evict after the pod applied a
// resync.
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

// WithNotifyMetricsHook installs an outcome-recording callback for
// notify dispatches. Outcomes: "success", "rate_limited", "retry_scheduled"
// (a 429's deferred retry was armed; it records its own outcome when it
// fires), "no_pod", "failed". Optional; nil is silently skipped.
func WithNotifyMetricsHook(hook func(outcome string)) Option {
	return func(s *Service) { s.notifyMetricsHook = hook }
}

// defaultHTTPClient bounds the dispatch so a hung agent can't block the
// caller's request goroutine indefinitely. 5s covers a healthy agent
// (sub-100ms in practice) with margin for the pod's in-process
// conditional pull (rate-limited on the pod side when repeated).
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

// Notify asks the workspace pod's agentd to re-pull its secret batch via
// POST /v1/resync-secrets: an empty body, the workspace-password Basic
// credential, and nothing else. The pod performs the v2 conditional
// bootstrap pull in-process and reports what it applied.
//
// Outcome contract:
//
//   - 200 (applied | not_modified) → nil error, NotifyResult decoded.
//   - 429 rate_limited → nil error: the pod is rate-limiting pulls.
//     When the 429 carries retryAfterMs, ONE deferred retry is armed
//     (min(retryAfterMs, 2.5s); M2) and the dispatch counts as
//     "retry_scheduled"; without retryAfterMs (or after Stop) it stays
//     "rate_limited" — the pod resyncs under its own budget either way.
//   - 502 failed (pull_failed | pull_unauthorized) and every transport
//     failure → error. Callers treat these as latency loss only; the
//     reconcile loop retries with backoff (I3).
func (s *Service) Notify(ctx context.Context, userID, workspaceID string) (NotifyResult, error) {
	return s.notifyOnce(ctx, userID, workspaceID, true)
}

// notifyOnce resolves the collaborators and dispatches one resync
// request. allowRetry=false on the deferred retry itself (M2: one
// retry only — a second 429 gives up and the loop owns it).
func (s *Service) notifyOnce(ctx context.Context, userID, workspaceID string, allowRetry bool) (NotifyResult, error) {
	if s.podResolver == nil {
		return NotifyResult{}, ErrNoPodIPResolver
	}
	podIP, err := s.podResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil || podIP == "" {
		s.emitNotify("no_pod")
		return NotifyResult{}, ErrNoRunningPod
	}

	if s.passwords == nil {
		s.emitNotify("failed")
		return NotifyResult{}, ErrNoPasswordProvider
	}
	password, err := s.passwords.WorkspacePassword(ctx, workspaceID)
	if err != nil {
		s.emitNotify("failed")
		s.warn("agentpush: resolve workspace password failed",
			"workspaceID", workspaceID, "error", err.Error())
		return NotifyResult{}, fmt.Errorf("resolve workspace password: %w", err)
	}

	return s.dispatchResync(ctx, podIP, password, userID, workspaceID, allowRetry)
}

// dispatchResync performs the authenticated empty POST and maps the
// response onto the outcome contract above.
func (s *Service) dispatchResync(ctx context.Context, podIP, password, userID, workspaceID string, allowRetry bool) (NotifyResult, error) {
	agentdURL := fmt.Sprintf("http://%s:%d/v1/resync-secrets", podIP, agentd.AgentdPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentdURL, http.NoBody)
	if err != nil {
		s.emitNotify("failed")
		return NotifyResult{}, fmt.Errorf("build resync request: %w", err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+password)))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.emitNotify("failed")
		s.warn("agentpush: resync notify request failed",
			"workspaceID", workspaceID, "error", err.Error())
		return NotifyResult{}, fmt.Errorf("failed to reach workspace agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var result NotifyResult
		_ = json.NewDecoder(resp.Body).Decode(&result)
		if s.modelCache != nil && result.Status == "applied" {
			s.modelCache.Evict(workspaceID)
		}
		s.emitNotify("success")
		return result, nil
	case http.StatusTooManyRequests:
		var limited struct {
			RetryAfterMs int64 `json:"retryAfterMs"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&limited)
		delay := time.Duration(limited.RetryAfterMs) * time.Millisecond
		if allowRetry && delay > 0 && s.scheduleRetry(context.WithoutCancel(ctx), delay, userID, workspaceID) {
			s.emitNotify("retry_scheduled")
			s.info("agentpush: pod rate-limited the resync notify; one deferred retry scheduled",
				"workspaceID", workspaceID)
		} else {
			s.emitNotify("rate_limited")
			s.info("agentpush: pod rate-limited the resync notify; pod will resync under its own budget",
				"workspaceID", workspaceID)
		}
		return NotifyResult{Status: "rate_limited"}, nil
	default:
		var agentErr struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&agentErr)
		reason := agentErr.Reason
		if reason == "" {
			reason = agentErr.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		s.emitNotify("failed")
		s.warn("agentpush: agent resync failed",
			"workspaceID", workspaceID, "status", resp.StatusCode, "reason", reason)
		return NotifyResult{}, fmt.Errorf("agent resync failed: %s", reason)
	}
}

// scheduleRetry arms ONE deferred resync retry min(delay,
// notifyRetryMaxDelay) from now. It never blocks the caller (the bind
// path returns immediately; the attempt runs in the timer's own
// goroutine on a detached context — the originating request is long
// gone by then). Returns false when the service is shutting down, so
// the 429 degrades to the plain rate_limited outcome.
func (s *Service) scheduleRetry(ctx context.Context, delay time.Duration, userID, workspaceID string) bool {
	if delay <= 0 {
		return false
	}
	if delay > notifyRetryMaxDelay {
		delay = notifyRetryMaxDelay
	}
	select {
	case <-s.stopCh:
		return false
	default:
	}
	pr := &pendingRetry{done: make(chan struct{})}
	s.retryWg.Add(1)
	pr.timer = time.AfterFunc(delay, func() {
		// canceled-before-work also closes done so the set prunes.
		defer s.retryWg.Done()
		defer close(pr.done)
		select {
		case <-s.stopCh:
			return
		default:
		}
		// Detached context: the shared client's 5s timeout bounds the
		// attempt. allowRetry=false — one retry only (M2).
		if _, err := s.notifyOnce(ctx, userID, workspaceID, false); err != nil {
			s.info("agentpush: deferred resync retry failed",
				"workspaceID", workspaceID, "error", err.Error())
		}
	})
	s.retryMu.Lock()
	if s.stopped { // Stop raced the stopCh check above
		s.retryMu.Unlock()
		if pr.timer.Stop() { // never fires — release its wg count here
			s.retryWg.Done()
		}
		return false
	}
	// Prune entries whose callbacks finished; the set stays bounded by
	// in-flight retries, not by lifetime 429 volume.
	for k := range s.timers {
		select {
		case <-k.done:
			delete(s.timers, k)
		default:
		}
	}
	s.timers[pr] = struct{}{}
	s.retryMu.Unlock()
	return true
}

// Stop cancels pending deferred retries and waits for one already in
// flight (bounded by the HTTP client timeout). Idempotent; the service
// stays usable for direct (non-deferred) notifies afterwards.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.retryMu.Lock()
		s.stopped = true
		for pr := range s.timers {
			if pr.timer.Stop() {
				// The callback will never run, so its wg count must be
				// released here. Stop()==false means the callback
				// already ran or is running and will release it itself.
				s.retryWg.Done()
			}
		}
		s.timers = nil
		s.retryMu.Unlock()
		s.retryWg.Wait()
	})
}

func (s *Service) emitNotify(outcome string) {
	if s.notifyMetricsHook != nil {
		s.notifyMetricsHook(outcome)
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
