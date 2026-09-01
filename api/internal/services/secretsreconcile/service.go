// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secretsreconcile is the level-triggered secrets-delivery
// reconcile loop (US-70.3, epic #1158 laws 2/4). Notify only shrinks
// latency; THIS loop is the correctness path: every period it derives
// each Active workspace's LIVE manifest (the same row-derivation the
// batch builder runs — zero decrypts), converges the stored revision
// row to it (minting any drift as a new seq), and compares the row's
// seq against the seq prefix of the pod's applied revision (CRD
// status.secretsDelivery.spawnedRev), re-notifying divergent
// workspaces with per-workspace exponential backoff + jitter.
//
// Why the LIVE manifest and not just the stored row (review finding on
// the lazy-refresh gap): mutations refresh the row lazily — the pull
// path mints at build time — so a failed notify followed by no pull
// leaves the row and the pod EQUALLY stale, and a row-vs-pod compare
// would call that converged. Effective-set changes no handler covers
// (org/global-default flips) were invisible to the old compare for the
// same reason. Comparing rows-as-they-are-NOW against the row closes
// both: the loop itself mints the drift, so a conditional pull flips
// 304→200 exactly when the loop observes the change.
//
// The compare is manifest-tier ONLY (AC-12b): the pass derives the
// live manifest from three row queries (credentials, bindings, MCP
// servers), reads the stored revision row, and performs at most one
// mint write per workspace — zero decrypt operations — so a full pass
// over a 1,000-workspace fleet sustains the period budget. There are
// no queues and no desired-state snapshots (level-triggered): each
// pass evaluates current state, so a workspace whose live manifest
// changed mid-backoff is picked up by the next eligible attempt
// automatically.
//
// Notify failure costs latency, never correctness (I3): a failed notify
// grows the backoff and the next pass retries. Pods unreachable at
// revoke time converge on their next contact; suspended pods converge
// on boot by construction. Mutation sites do NOT eagerly refresh the
// row (SetBindings/UpdateSecret rely on this loop); the one exception
// is ForceRevokeSecret, whose eager refresh keeps revoke latency from
// depending on loop timing.
package secretsreconcile

import (
	"context"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

const (
	// DefaultInterval is the period between reconcile passes.
	DefaultInterval = 60 * time.Second

	// notifyBackoffBase is the first post-attempt wait; each subsequent
	// attempt doubles it (I15 budget fairness).
	notifyBackoffBase = 5 * time.Second

	// notifyBackoffCap bounds the wait so a mixed-fleet legacy pod (no
	// resync endpoint) is retried at most every 10 minutes forever
	// instead of being hammered.
	notifyBackoffCap = 10 * time.Minute

	// jitterBand stretches a computed backoff by up to +25% so a fleet
	// that diverged together (API restart, shared outage) does not
	// notify in lockstep.
	jitterBand = 0.25
)

// ActiveWorkspace is one row of the per-pass enumeration: the CR name,
// its owner (spec.owner.userID), and the pod's applied revision
// (status.secretsDelivery.spawnedRev, "seq:manifestHash:contentHash").
type ActiveWorkspace struct {
	WorkspaceID string
	OwnerUserID string
	SpawnedRev  string
}

// WorkspaceLister enumerates Active workspaces with their delivery
// state. Satisfied by an adapter over the API's Kubernetes client
// (paginated CRD list, phase filtered in memory — status phase is not
// field-selectable).
type WorkspaceLister interface {
	ListActiveWorkspaces(ctx context.Context) ([]ActiveWorkspace, error)
}

// RevisionSource is the revision authority the loop converges against.
// It reads the LIVE manifest tier (the same decrypt-free row-derivation
// the batch builder runs, keyed by the workspace OWNER — the CRD
// spec.owner.userID the lister already carries), reads the stored
// revision row, and mints drift as a new seq with the store's bounded
// CAS semantics. Satisfied by *secrets.SecretService.
type RevisionSource interface {
	// ManifestFor computes the workspace's CURRENT manifest hash from
	// rows alone (zero decrypts).
	ManifestFor(ctx context.Context, ownerUserID, workspaceID string) (string, error)

	// CurrentRevision returns the stored revision row, ok=false when
	// none exists yet.
	CurrentRevision(ctx context.Context, workspaceID string) (int64, string, bool, error)

	// EnsureRevision converges the stored row to manifestHash: same
	// hash returns the stored seq; a different hash conditionally mints
	// the next seq (bounded CAS).
	EnsureRevision(ctx context.Context, workspaceID, manifestHash string) (int64, error)
}

// Notifier dispatches a resync notify to a workspace pod. Satisfied by
// *agentpush.Service.
type Notifier interface {
	Notify(ctx context.Context, userID, workspaceID string) error
}

// Divergence reasons (machine-readable, epic #1158 law 5). Note:
// reasonLegacyFormat is counted in
// secrets_delivery_divergent_total but deliberately does NOT mark the
// per-workspace convergence gauge divergent (M1b) — a legacy pod can
// never converge by notify, so the gauge would page per pod during a
// mixed-fleet rollout; observability rides the counter instead.
const (
	reasonMissingRev   = "missing_rev"
	reasonStaleSeq     = "stale_seq"
	reasonLegacyFormat = "legacy_format"
	reasonNotifyFailed = "notify_failed"
)

type backoffState struct {
	attempts     int
	nextEligible time.Time
}

// Service is the reconcile loop.
type Service struct {
	lister      WorkspaceLister
	revisions   RevisionSource
	notifier    Notifier
	logger      pkginterfaces.LoggerInterface
	interval    time.Duration
	backoffBase time.Duration
	backoffCap  time.Duration

	mu     sync.Mutex
	states map[string]*backoffState
	gauged map[string]struct{}

	now    func() time.Time
	jitter func() float64

	wg       sync.WaitGroup
	stopCh   chan struct{}
	stopOnce sync.Once
}

// Option configures a Service.
type Option func(*Service)

// WithLogger installs the logger for pass failures and divergences.
func WithLogger(l pkginterfaces.LoggerInterface) Option {
	return func(s *Service) { s.logger = l }
}

// WithInterval overrides the pass period (tests).
func WithInterval(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithBackoff overrides the notify backoff base and cap (tests).
func WithBackoff(base, cap time.Duration) Option {
	return func(s *Service) {
		if base > 0 {
			s.backoffBase = base
		}
		if cap > 0 {
			s.backoffCap = cap
		}
	}
}

// New constructs the reconcile Service. lister, revisions and notifier
// are required; a nil dependency fails the first pass loudly.
func New(lister WorkspaceLister, revisions RevisionSource, notifier Notifier, opts ...Option) *Service {
	s := &Service{
		lister:      lister,
		revisions:   revisions,
		notifier:    notifier,
		interval:    DefaultInterval,
		backoffBase: notifyBackoffBase,
		backoffCap:  notifyBackoffCap,
		states:      make(map[string]*backoffState),
		gauged:      make(map[string]struct{}),
		now:         time.Now,
		jitter:      rand.Float64,
		stopCh:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// IntervalFromEnv resolves the pass period from
// LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL, falling back to the given
// default when unset or unparseable (a bad override must never block
// startup).
func IntervalFromEnv(defaultInterval time.Duration) time.Duration {
	raw := os.Getenv("LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL")
	if raw == "" {
		return defaultInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultInterval
	}
	return d
}

// Start launches the loop. The first pass runs immediately (heals
// overnight divergence), then the ticker takes over. Start returns
// immediately; pass failures are logged, never propagated.
func (s *Service) Start(ctx context.Context) error {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx)
	}()
	return nil
}

// Stop terminates the loop and waits for the in-flight pass to finish.
// Idempotent.
func (s *Service) Stop() error {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
	return nil
}

func (s *Service) loop(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.pass(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.pass(ctx)
		}
	}
}

func (s *Service) pass(ctx context.Context) {
	if err := s.runPass(ctx); err != nil {
		s.warn("secretsreconcile: pass failed", "error", err.Error())
	}
}

// convergence is one workspace's classification for a pass.
type convergence struct {
	converged bool
	reason    string
}

// classify decides convergence from the stored revision row and the
// CRD-reported applied revision alone (AC-12b: zero decrypts, zero
// extra row loads). spawnRev is the pod's "seq:manifestHash:contentHash".
// The caller MUST already have converged the row to the live manifest
// (the mint step), so hasRow is always true on the loop path; the
// no-row arms remain for direct callers and pin the pure function's
// contract.
//
//   - seq match                 → converged (M1: convergence is only
//     certified by a PARSEABLE applied rev whose seq equals the stored
//     row's — anything else is divergence with a reason)
//   - empty spawnRev            → legacy_format IFF the stored manifest
//     hash IS the empty-set hash for the owner; otherwise missing_rev.
//     Validation M1: an unreported rev cannot certify convergence — a
//     legacy pod serving revoked plaintext reports nothing and must not
//     classify converged. The empty-manifest variant self-heals for v2
//     pods (the notify triggers a revisioned empty pull; the pod then
//     anchors and reports a parseable seq).
//   - non-numeric seq or bare hash → legacy_format (pre-US-70.2 pod)
//   - seq mismatch / missing row → stale_seq / missing_rev
func classify(spawnRev, ownerID string, seq int64, manifestHash string, hasRow bool) convergence {
	if spawnRev == "" {
		if hasRow && manifestHash == secrets.ManifestHash(ownerID, nil) {
			return convergence{reason: reasonLegacyFormat}
		}
		return convergence{reason: reasonMissingRev}
	}
	seqField, _, found := strings.Cut(spawnRev, ":")
	if !found {
		return convergence{reason: reasonLegacyFormat}
	}
	applied, err := strconv.ParseInt(seqField, 10, 64)
	if err != nil {
		return convergence{reason: reasonLegacyFormat}
	}
	if !hasRow {
		return convergence{reason: reasonMissingRev}
	}
	if applied == seq {
		return convergence{converged: true}
	}
	return convergence{reason: reasonStaleSeq}
}

// runPass walks the Active fleet once. A listing failure fails the pass
// (passes_total{error}); per-workspace revision read failures skip that
// workspace with a warning and a secrets_reconcile_skips_total{reason}
// tick — one bad row must not blind the rest of the fleet, and a
// "success" pass means the pass COMPLETED (per-workspace skips are
// counted separately, not as pass errors).
func (s *Service) runPass(ctx context.Context) error {
	workspaces, err := s.lister.ListActiveWorkspaces(ctx)
	if err != nil {
		metrics.RecordSecretsReconcilePass("error")
		return err
	}

	seen := make(map[string]struct{}, len(workspaces))
	for _, ws := range workspaces {
		seen[ws.WorkspaceID] = struct{}{}
		s.reconcileWorkspace(ctx, ws)
	}
	s.withdrawStaleGauges(seen)

	metrics.RecordSecretsReconcilePass("success")
	metrics.SetSecretsReconcilePassSuccess()
	return nil
}

func (s *Service) reconcileWorkspace(ctx context.Context, ws ActiveWorkspace) {
	// Step 1 — the LIVE manifest (zero decrypts), derived from the
	// rows the builder would read, keyed by the workspace's OWNER. A
	// read failure skips THIS workspace (counted, never fatal): the
	// rest of the pass still runs.
	liveHash, err := s.revisions.ManifestFor(ctx, ws.OwnerUserID, ws.WorkspaceID)
	if err != nil {
		metrics.RecordSecretsReconcileSkip("manifest_read")
		s.warn("secretsreconcile: live manifest read failed; skipping workspace this pass",
			"workspaceID", ws.WorkspaceID, "error", err.Error())
		return
	}

	// Step 2 — the stored revision row.
	seq, rowHash, hasRow, err := s.revisions.CurrentRevision(ctx, ws.WorkspaceID)
	if err != nil {
		metrics.RecordSecretsReconcileSkip("row_read")
		s.warn("secretsreconcile: read stored revision failed; skipping workspace this pass",
			"workspaceID", ws.WorkspaceID, "error", err.Error())
		return
	}

	// Step 3 — mint-on-drift: the row is lazily refreshed by the pull
	// path, so the LOOP owns converging it to the live manifest. A
	// minted seq moves expected past whatever the pod applied, flipping
	// the pod's next conditional pull from 304 to 200 exactly when this
	// loop observes the change. Same-hash rows mint nothing (≤1 write
	// per workspace per pass).
	if !hasRow || rowHash != liveHash {
		seq, err = s.revisions.EnsureRevision(ctx, ws.WorkspaceID, liveHash)
		if err != nil {
			metrics.RecordSecretsReconcileSkip("mint")
			s.warn("secretsreconcile: mint drift revision failed; skipping workspace this pass",
				"workspaceID", ws.WorkspaceID, "error", err.Error())
			return
		}
	}

	c := classify(ws.SpawnedRev, ws.OwnerUserID, seq, liveHash, true)
	// M1(b): a legacy-format pod can NEVER converge by notify — the old
	// mux 404s the resync endpoint — so marking the per-workspace gauge
	// divergent pages per pod during a mixed-fleet rollout. The
	// divergence counter still records it; the gauge stays converged so
	// neither the >15m page nor the SLO burn fires. Legacy pods surface
	// via secrets_delivery_divergent_total{reason="legacy_format"} and
	// converge with the fleet upgrade (US-70.5 re-points the gauge).
	gaugeConverged := c.converged || c.reason == reasonLegacyFormat
	metrics.SetSecretsDeliveryConverged(ws.WorkspaceID, gaugeConverged)
	s.mu.Lock()
	s.gauged[ws.WorkspaceID] = struct{}{}
	s.mu.Unlock()
	if c.converged {
		s.resetState(ws.WorkspaceID)
		return
	}

	metrics.RecordSecretsDeliveryDivergent(c.reason)
	if !s.eligible(ws.WorkspaceID) {
		return
	}
	if nerr := s.notifier.Notify(ctx, ws.OwnerUserID, ws.WorkspaceID); nerr != nil {
		metrics.RecordSecretsDeliveryDivergent(reasonNotifyFailed)
		s.warn("secretsreconcile: notify failed",
			"workspaceID", ws.WorkspaceID, "reason", c.reason, "error", nerr.Error())
	}
	s.bumpState(ws.WorkspaceID)
}

// withdrawStaleGauges removes convergence gauge series (and backoff
// state) for workspaces that left the Active set so the per-workspace
// label space tracks the fleet, not its history.
func (s *Service) withdrawStaleGauges(seen map[string]struct{}) {
	s.mu.Lock()
	var withdrawn []string
	for ws := range s.states {
		if _, ok := seen[ws]; !ok {
			delete(s.states, ws)
		}
	}
	for ws := range s.gauged {
		if _, ok := seen[ws]; !ok {
			delete(s.gauged, ws)
			withdrawn = append(withdrawn, ws)
		}
	}
	s.mu.Unlock()
	for _, ws := range withdrawn {
		metrics.DeleteSecretsDeliveryConverged(ws)
	}
}

func (s *Service) state(workspaceID string) *backoffState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[workspaceID]
}

func (s *Service) resetState(workspaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, workspaceID)
}

func (s *Service) eligible(workspaceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[workspaceID]
	if !ok {
		return true
	}
	return !s.now().Before(st.nextEligible)
}

func (s *Service) bumpState(workspaceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[workspaceID]
	if !ok {
		st = &backoffState{}
		s.states[workspaceID] = st
	}
	st.attempts++
	wait := s.backoffDuration(st.attempts)
	st.nextEligible = s.now().Add(wait)
}

// backoffDuration computes base×2^(attempts−1) capped, stretched by up
// to +jitterBand. Float math avoids int64 shift overflow for very
// large attempt counts.
func (s *Service) backoffDuration(attempts int) time.Duration {
	growth := 1.0
	for float64(s.backoffBase)*growth < float64(s.backoffCap) && attempts > 1 {
		growth *= 2
		attempts--
	}
	d := float64(s.backoffBase) * growth
	if d > float64(s.backoffCap) {
		d = float64(s.backoffCap)
	}
	d *= 1 + s.jitter()*jitterBand
	return time.Duration(d)
}

func (s *Service) warn(msg string, fields ...interface{}) {
	if s.logger != nil {
		s.logger.Warn(msg, fields...)
	}
}
