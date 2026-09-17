// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// rearm_loop.go — the generalized "terminal fetch failure → loud degrade
// → bounded re-arm" loop (#910 / US-72.0).
//
// A producer whose reachability can fail terminally (the relay injector's
// free-models fetch is the first consumer; US-72.4's relay-only liveness
// is the named second) runs its bounded boot attempt, then — instead of
// degrading for the rest of the pod's lifetime — hands the SAME attempt
// to this loop. Each cycle:
//
//	waits (backoff: delays double from MinDelay, clamped at MaxDelay)
//	→ gates (Applied short-circuit, Busy, RestartDeferred)
//	→ attempts (one full bounded fetch/apply)
//	→ records exactly one outcome tick
//
// Delay advancement happens ONLY on a failed attempt — a gate skip is a
// defer, not a failure. Terminal exits: applied (disarm), non-retryable
// outcome (permanent skip, e.g. a personal key), already applied at gate
// time, ctx canceled. The loop is a single goroutine, so there is exactly
// one in-flight re-arm by construction. The no-stacking guarantee has TWO
// checkpoints: the RestartDeferred gate at cycle entry, and (the
// consumer's responsibility — see relayInjectorConfig.RestartDeferred)
// a re-check immediately before the consumer triggers its restart, since
// a deferral can appear during the attempt itself.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// rearmAttemptResult is one attempt's terminal report.
type rearmAttemptResult struct {
	// outcome is the attempt's final outcome string — the consumer's
	// own taxonomy (the injector reuses its existing metric labels).
	outcome string
	// retryable: true → schedule the next cycle (with backoff
	// advancement); false → permanent, disarm.
	retryable bool
	// applied: the desired state now holds — disarm. The success path
	// of every consumer.
	applied bool
}

// rearmLoopConfig parameterizes one re-arm loop. All gates are optional
// (nil skips the gate); Attempt is required.
type rearmLoopConfig struct {
	// Loop names this loop's metric series (e.g. "relay_injector").
	Loop string
	// Applied short-circuits the cycle when the desired state already
	// holds (the injector's HasRelay re-check — relay state may appear
	// through any other path mid-pod-life).
	Applied func() bool
	// Busy reports whether the shared resource is in use (busy
	// sessions). True skips the cycle — never attempt, never restart,
	// under a busy session.
	Busy func() bool
	// RestartDeferred reports whether a deferred restart is
	// outstanding (#910's named constraint: attempts must not stack
	// behind deferred kills — the ≤pollInterval window between the last
	// busy→idle transition and the deferred restart firing).
	RestartDeferred func() bool
	// Attempt performs one bounded fetch/apply cycle.
	Attempt func(ctx context.Context) rearmAttemptResult
	// MinDelay/MaxDelay bound the backoff (zero → #910 defaults 5m/30m:
	// waits double from the floor, clamp at the cap).
	MinDelay time.Duration
	MaxDelay time.Duration
}

const (
	defaultRearmMinDelay = 5 * time.Minute
	defaultRearmMaxDelay = 30 * time.Minute

	// rearmLoopRelayInjector is the relay injector's series name — the
	// design's `relay_rearm_outcome` metric.
	rearmLoopRelayInjector = "relay_injector"
)

// Loop-level cycle outcomes (attempt-level outcomes come from the
// consumer's attempt taxonomy — an applied cycle ticks the consumer's
// own success taxonomy, e.g. "success", which is why there is no
// loop-level "applied" outcome string).
const (
	rearmOutcomeAlreadyApplied  = "already_applied"
	rearmOutcomeBusy            = "busy"
	rearmOutcomeRestartDeferred = "restart_deferred"
	rearmOutcomeCanceled        = "canceled"
)

// rearmLoopOutcomes ticks exactly once per cycle with that cycle's final
// outcome. Generic vec (loop label) because the component is generalized
// — the injector is the first consumer, US-72.4's liveness loop the
// second; per the US-72.0 story both ride this one metric.
var rearmLoopOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "llmsafespaces_rearm_outcome_total",
	Help: "Re-arm loop cycle outcomes (the generalized terminal-fetch-failure recovery loop, #910/US-72.0): exactly one tick per cycle with the cycle's final outcome.",
}, []string{"loop", "outcome"})

// nextRearmDelay returns the next wait after a FAILED attempt: double the
// current delay, clamped at max. Gate skips never call this — a skip is
// not a failure.
func nextRearmDelay(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

// startRearmLoop spawns the bounded re-arm loop on its own goroutine and
// returns; the loop runs until a terminal exit (applied / non-retryable
// / already applied / ctx canceled). Consumers call it exactly once per
// producer lifetime (the injector: once per pod) — since the loop is a
// single goroutine, there is exactly one in-flight re-arm by
// construction; combined with the RestartDeferred gate, a re-arm attempt
// never stacks a restart behind a deferred kill.
func startRearmLoop(ctx context.Context, cfg rearmLoopConfig) {
	// Capture the logger at call-site so the loop does not race with
	// test code that reassigns the package-level log variable (same
	// discipline as startRelayInjector).
	lg := log
	go runRearmLoop(ctx, cfg, lg)
}

// runRearmLoop is startRearmLoop's body, on its own goroutine.
func runRearmLoop(ctx context.Context, cfg rearmLoopConfig, lg *zap.Logger) {
	if cfg.MinDelay <= 0 {
		cfg.MinDelay = defaultRearmMinDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultRearmMaxDelay
	}
	if cfg.MaxDelay < cfg.MinDelay {
		cfg.MaxDelay = cfg.MinDelay
	}

	delay := cfg.MinDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			rearmLoopOutcomes.WithLabelValues(cfg.Loop, rearmOutcomeCanceled).Inc()
			return
		case <-timer.C:
		}

		if cfg.Applied != nil && cfg.Applied() {
			rearmLoopOutcomes.WithLabelValues(cfg.Loop, rearmOutcomeAlreadyApplied).Inc()
			lg.Info("re-arm loop: desired state already applied; disarming",
				zap.String("loop", cfg.Loop))
			return
		}
		if cfg.Busy != nil && cfg.Busy() {
			rearmLoopOutcomes.WithLabelValues(cfg.Loop, rearmOutcomeBusy).Inc()
			continue
		}
		if cfg.RestartDeferred != nil && cfg.RestartDeferred() {
			rearmLoopOutcomes.WithLabelValues(cfg.Loop, rearmOutcomeRestartDeferred).Inc()
			continue
		}

		res := cfg.Attempt(ctx)
		rearmLoopOutcomes.WithLabelValues(cfg.Loop, res.outcome).Inc()
		if res.applied || !res.retryable {
			return
		}
		delay = nextRearmDelay(delay, cfg.MaxDelay)
	}
}
