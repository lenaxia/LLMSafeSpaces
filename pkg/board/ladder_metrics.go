// ladder_metrics.go — the #2040 always-on ladder telemetry (issue #2042).
//
// The 2026-11-15 incident was invisible for hours because every ladder
// decision surface was silent-by-default: renewals write no audit
// events by design ("renewals never flood the trail"), and the lease
// event bus is knob-gated (OFF in this deployment). A single renewal
// counter with a source label would have identified the poisoning in
// one scrape — renewals with a movement/heartbeat source on sessions
// that emitted nothing are the incident's signature. These counters
// are knob-independent and always armed; the Redis lease-event bus
// stays a separate, optional projection.

package board

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// LeaseRenewalsMetricName / label — renewal decisions by source.
	LeaseRenewalsMetricName = "board_lease_renewals_total"
	// LadderNudgesMetricName — sentinels armed (delivery attempted).
	LadderNudgesMetricName = "board_ladder_nudges_total"
	// LadderWedgesMetricName — ordinal wedge verdicts landed.
	LadderWedgesMetricName = "board_ladder_wedges_total"
)

var (
	leaseRenewalsBySource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: LeaseRenewalsMetricName,
		Help: "Seamless-lease renewal decisions by rung source (movement/heartbeat_response/post_nudge_reaction/busy_gate). Opaque (management-frame) movement has ZERO renewal authority (#2042) — it arms quiet arms, never renewals; a movement-class renewal on a session with no worker activity is the evidence-poisoning signature (#2040).",
	}, []string{"source"})

	ladderNudges = promauto.NewCounter(prometheus.CounterOpts{
		Name: LadderNudgesMetricName,
		Help: "Board ladder nudges armed (delivery attempted, before outcome).",
	})

	ladderWedges = promauto.NewCounter(prometheus.CounterOpts{
		Name: LadderWedgesMetricName,
		Help: "Board ladder ordinal wedge verdicts (sentinel positioned, worker-activity silence after it).",
	})
)
