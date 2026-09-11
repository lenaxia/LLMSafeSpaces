// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package canary is the epic-71 / 0c production canary (#1312,
// metrics-only): a per-workspace-class probe runner that turns "stale
// prompt / silent click / wedged session" into counters and histograms
// while the wave fixes land. Zero model spend by construction — the
// snapshot leg is a pure projection read (GetSnapshot of a synthetic
// session), and the resolve leg exercises resolve-by-absence
// (Act(answer_question) on a synthetic input; the 404 path
// short-circuits before any model call). Token-spending probes (real-ask
// L1, Deliver-path) stay gated behind their waves and an owner budget
// decision. Alerts are gated until L3/L4 are green (wave plan).
//
// The runner lives API-side: the canary is cross-workspace by charter,
// and the API owns workspace resolution, the §D1 transport, and the
// scrape surface. Every replica may run it — probes are idempotent
// reads/absence-resolves on synthetic ids, and per-replica series expose
// replica-local network paths.
package canary

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/obs"
)

// Client is the subset of abiclient.Client the canary needs (the test
// seam; abiclient.New satisfies it).
type Client interface {
	GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error)
	Act(ctx context.Context, req *abiv1.ActionRequest) (*abiv1.ActionResult, error)
}

// NewClient builds a Client for a resolved pod endpoint.
type NewClient func(baseURL, password string) Client

// PickTarget selects the workspace to probe for a class. Returning
// ("", nil) means no workspace of that class exists — a fleet-shape
// signal (no_target), not a failure.
type PickTarget func(ctx context.Context, class string) (workspaceID string, err error)

// Resolve yields the pod's ABI base URL + password for a workspace (the
// usagestream.Resolve shape).
type Resolve func(ctx context.Context, workspaceID string) (baseURL, password string, err error)

// Logger is the minimal seam (the API's LoggerInterface satisfies it).
type Logger interface {
	Warn(msg string, keysAndValues ...interface{})
	Info(msg string, keysAndValues ...interface{})
}

// Defaults. 60s aligns with the parked-sweeper cadence: the canary's
// charter is minute-granularity convergence visibility, not per-request
// latency (L-bound alerts tune the interval later). The per-leg timeout
// bounds one probe attempt well above the I12 snapshot budget (250ms)
// so slow-but-alive answers classify honestly instead of timing out.
const (
	DefaultInterval = 60 * time.Second
	DefaultTimeout  = 2 * time.Second
)

// Probe legs.
const (
	LegSnapshot = "snapshot"
	LegResolve  = "resolve"
)

// Outcome classifications — #1312's failure vocabulary (which leg, which
// invariant) rendered as label values:
//
//	resolve/not_found_error — S6 VIOLATED: absence surfaced as an error
//	                         instead of resolving (the pre-1a baseline).
//	resolve/resolved        — S6 satisfied (absence resolves cleanly).
//	snapshot/not_found      — the pod answered a synthetic session with a
//	                         typed not_found: the path is LIVE (expected
//	                         steady state on the synthetic leg).
//	*/timeout               — a wedge (the canary's primary quarry).
//	no_target/pick_error/unresolved_target — targeting failures, not pod
//	                         verdicts.
const (
	OutcomeOK               = "ok"
	OutcomeNotFound         = "not_found"
	OutcomeResolved         = "resolved"
	OutcomeNotFoundError    = "not_found_error"
	OutcomeNotSupported     = "not_supported"
	OutcomeInvalidArgument  = "invalid_argument"
	OutcomeUnauthenticated  = "unauthenticated"
	OutcomeTimeout          = "timeout"
	OutcomeUnavailable      = "unavailable"
	OutcomeError            = "error"
	OutcomeNoTarget         = "no_target"
	OutcomePickError        = "pick_error"
	OutcomeUnresolvedTarget = "unresolved_target"
)

// canaryIDPrefix is the synthetic-identifier discipline: real opencode
// session/input ids are harness-generated tokens (ses_/msg_ shapes);
// this prefix cannot collide, so the resolve leg can never resolve a
// real pending input (S10 isolation by construction).
const canaryIDPrefix = "canary-probe-"

// canaryAnswerText is the non-empty answer payload agentd's
// validateAction requires (answer_question without option_ids or
// custom_text is invalid_argument — the probe must never send one).
const canaryAnswerText = "canary probe"

func strPtr(s string) *string { return &s }

// SyntheticSessionID and SyntheticInputID derive stable, obviously-synthetic
// probe identifiers from the class name.
func SyntheticSessionID(class string) string { return canaryIDPrefix + "session-" + class }
func SyntheticInputID(class string) string   { return canaryIDPrefix + "input-" + class }

// Config wires the canary. Classes drive everything: an empty class list
// constructs a valid (idle) service — the enable knob is off.
type Config struct {
	// Interval between passes (default 60s).
	Interval time.Duration
	// Timeout bounds one probe attempt, per leg (default 2s).
	Timeout time.Duration
	// Classes are workspace-class selectors (v1: runtime-environment
	// names, e.g. "python:3.11" — the configured selector list).
	Classes []string
	// Pick, Resolve, NewClient are required when Classes is non-empty.
	Pick      PickTarget
	Resolve   Resolve
	NewClient NewClient
	Logger    Logger
}

// Service is the canary probe runner (janitor lifecycle: construct, go
// Run(ctx), terminate via ctx cancel).
type Service struct {
	cfg Config
}

// New validates the seam wiring and applies defaults.
func New(cfg Config) (*Service, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if len(cfg.Classes) > 0 {
		if cfg.Pick == nil {
			return nil, errors.New("canary: Pick is required when Classes is configured")
		}
		if cfg.Resolve == nil {
			return nil, errors.New("canary: Resolve is required when Classes is configured")
		}
		if cfg.NewClient == nil {
			return nil, errors.New("canary: NewClient is required when Classes is configured")
		}
	}
	return &Service{cfg: cfg}, nil
}

// Run blocks until ctx is canceled, probing every configured class once
// per interval and stamping the shared loop-liveness family at end of
// pass (the stamp is loop-owned: direct runOnce callers do not refresh
// it — a dead loop must be allowed to look stale).
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
			obs.StampLoopLastRun(obs.LoopCanaryProbe)
		}
	}
}

// runOnce probes every configured class once.
func (s *Service) runOnce(ctx context.Context) {
	for _, class := range s.cfg.Classes {
		s.probeClass(ctx, class)
	}
}

// probeClass resolves the class's target and runs both legs. Targeting
// failures are recorded on both legs — a probe that never reached a pod
// says nothing about the pod.
func (s *Service) probeClass(ctx context.Context, class string) {
	workspaceID, err := s.cfg.Pick(ctx, class)
	if err != nil {
		s.countBoth(class, OutcomePickError)
		s.warn("canary: target pick failed", class, err)
		return
	}
	if workspaceID == "" {
		s.countBoth(class, OutcomeNoTarget)
		return
	}
	baseURL, password, err := s.cfg.Resolve(ctx, workspaceID)
	if err != nil {
		s.countBoth(class, OutcomeUnresolvedTarget)
		s.warn("canary: target resolve failed", class, err)
		return
	}
	cl := s.cfg.NewClient(baseURL, password)
	s.probeSnapshot(ctx, class, cl)
	s.probeResolve(ctx, class, cl)
}

// probeSnapshot — leg 1: the snapshot-path liveness round-trip. A pure
// projection read (zero harness calls); a typed not_found for the
// synthetic session is the expected live-pod answer. A 2xx carrying a
// DIFFERENT session id than requested is wire drift, not liveness.
func (s *Service) probeSnapshot(ctx context.Context, class string, cl Client) {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	syntheticID := SyntheticSessionID(class)
	snap, err := cl.GetSnapshot(pctx, syntheticID)
	elapsed := time.Since(start)
	cancel()

	outcome := classifySnapshot(err)
	if err == nil && snap.GetSessionId() != syntheticID {
		outcome = OutcomeError
	}
	probeDuration.WithLabelValues(class, LegSnapshot).Observe(elapsed.Seconds())
	probeOutcomes.WithLabelValues(class, LegSnapshot, outcome).Inc()
}

// probeResolve — leg 2: S6 resolve-by-absence. Act(answer_question) on
// a synthetic input never reaches a model (the 404 short-circuit);
// CustomText is non-empty because agentd's validateAction requires an
// answer payload.
func (s *Service) probeResolve(ctx context.Context, class string, cl Client) {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	res, err := cl.Act(pctx, &abiv1.ActionRequest{
		SessionId: SyntheticSessionID(class),
		Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{
			InputId:    SyntheticInputID(class),
			CustomText: strPtr(canaryAnswerText),
		}},
	})
	elapsed := time.Since(start)
	cancel()

	outcome := OutcomeResolved
	if err != nil {
		outcome = classifyResolve(err)
	} else if res.GetAnswerQuestion() == nil {
		outcome = OutcomeError
	}
	probeDuration.WithLabelValues(class, LegResolve).Observe(elapsed.Seconds())
	probeOutcomes.WithLabelValues(class, LegResolve, outcome).Inc()
}

func classifySnapshot(err error) string {
	if err == nil {
		return OutcomeOK
	}
	if code, ok := classifyCommon(err); ok {
		return code
	}
	return OutcomeError
}

func classifyResolve(err error) string {
	if code, ok := classifyCommon(err); ok {
		if code == OutcomeNotFound {
			return OutcomeNotFoundError
		}
		return code
	}
	return OutcomeError
}

// classifyCommon maps an error to the shared outcome vocabulary; false
// means "no typed mapping — caller falls back to error".
func classifyCommon(err error) (string, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout, true
	}
	switch connect.CodeOf(err) {
	case connect.CodeNotFound:
		return OutcomeNotFound, true
	case connect.CodeInvalidArgument, connect.CodeOutOfRange:
		return OutcomeInvalidArgument, true
	case connect.CodeUnauthenticated:
		return OutcomeUnauthenticated, true
	case connect.CodeUnimplemented:
		return OutcomeNotSupported, true
	case connect.CodeDeadlineExceeded:
		return OutcomeTimeout, true
	case connect.CodeUnavailable:
		return OutcomeUnavailable, true
	}
	return "", false
}

func (s *Service) countBoth(class, outcome string) {
	probeOutcomes.WithLabelValues(class, LegSnapshot, outcome).Inc()
	probeOutcomes.WithLabelValues(class, LegResolve, outcome).Inc()
}

func (s *Service) warn(msg, class string, err error) {
	if s.cfg.Logger != nil {
		s.cfg.Logger.Warn(msg, "class", class, "error", err)
	}
}
