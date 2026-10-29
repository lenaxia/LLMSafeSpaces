// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_metrics.go — session idle-subscriptions v1, §8:
// the observability surface. One counter family,
// llmsafespaces_session_notifications_total, carries every design-named
// outcome as a label value — the relayInjectorOutcomes pattern (one
// CounterVec, closed label taxonomy, zap logs at the emission sites)
// — registered via promauto on the default registry like every other
// agentd family (sessionstate_metrics.go's plumbing class; the
// sessionstate module itself stays prometheus-free).
//
// Outcome taxonomy (design §8, verbatim names as label values):
//
//	sent                        — a notification POST succeeded
//	suppressed_already_notified — §4.4: last send_message postdates the
//	                              last edit/write/bash for this pair
//	debounced                   — §4.3: the armed timer was canceled by a
//	                              return-to-busy inside the window
//	skipped_subtask             — §4.6: either side of the edge is a
//	                              subtask (settle-time skip or refusal)
//	skipped_pending_input       — §4.2: the settle is blocked on a user
//	                              ask (not "complete")
//	refused_self                — §7.1 registration refusal
//	refused_cycle               — §7.2 registration refusal
//	refused_cap                 — §4.1 cap refusal
//	refused_rate                — §4.5 per-subscriber storm backstop
//	outbox_retried_delivered    — ruling 4: a boot outbox retry landed
//	outbox_retried_dropped      — ruling 4: retry-once exhausted, dropped

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Notification outcome label values (§8's counters; the outbox retries
// split delivered/dropped under the outbox_retried prefix).
const (
	notifyOutcomeSent                 = "sent"
	notifyOutcomeSuppressed           = "suppressed_already_notified"
	notifyOutcomeDebounced            = "debounced"
	notifyOutcomeSkippedSubtask       = "skipped_subtask"
	notifyOutcomeSkippedPendingInput  = "skipped_pending_input"
	notifyOutcomeRefusedSelf          = "refused_self"
	notifyOutcomeRefusedCycle         = "refused_cycle"
	notifyOutcomeRefusedCap           = "refused_cap"
	notifyOutcomeRefusedRate          = "refused_rate"
	notifyOutcomeOutboxRetriedSent    = "outbox_retried_delivered"
	notifyOutcomeOutboxRetriedDropped = "outbox_retried_dropped"
)

var sessionNotifyMetrics = struct {
	outcomes *prometheus.CounterVec
}{
	outcomes: promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "llmsafespaces_session_notifications_total",
		Help: "Session idle-subscription notification outcomes (design §8): sent, suppressed_already_notified (the §4.4 send-postdates-mutation rule), debounced (§4.3 re-busy cancel), skipped_subtask / skipped_pending_input (gates), refused_{self,cycle,cap} (registration guards), refused_rate (per-subscriber storm backstop), outbox_retried_{delivered,dropped} (ruling 4 boot retry).",
	}, []string{"outcome"}),
}

// notifyCount ticks one outcome. Cheap (a map + atomic add); safe from
// any goroutine — registration paths, the Run loop, timer legs, and the
// boot outbox retry all share it.
func notifyCount(outcome string) {
	sessionNotifyMetrics.outcomes.WithLabelValues(outcome).Inc()
}
