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
	// LadderHeartbeatLapseMetricName — renewal evidence suppressed by
	// the heartbeat-lapse arm (#2043). Deliberately NOT a label on
	// LeaseRenewalsMetricName: a lapse suppression is the OPPOSITE of a
	// renewal decision, and the renewals counter's forensic value is
	// exactly its honesty ("renewals with a movement/heartbeat source
	// on sessions that emitted nothing" is the #2040 signature) — a
	// non-renewal label there would blur the scrape the incident made
	// necessary. The issue's R3 offered "or the nudge payload carries
	// it" as the alternative arm; this counter plus the payload reason
	// NudgeReasonHeartbeatLapse are that arm, always-on.
	LadderHeartbeatLapseMetricName = "board_ladder_heartbeat_lapse_total"
	// HeartbeatNoAdvanceCyclesMetricName — motionless heartbeat
	// responses counted against the no-advance budget (#2044): every
	// response whose declared watermark did not cross the claim mint,
	// renewing (below cap) or declined (at cap). The orbit's
	// always-on signature — the counterfactual scrape for the
	// keystone incident (a presence-only worker holding its slot for
	// hours reads as a monotonically climbing count with zero stage
	// advance anywhere else).
	HeartbeatNoAdvanceCyclesMetricName = "board_heartbeat_noadvance_cycles_total"
	// DueMarksMetricName / label — due-mark writes by trigger
	// (#2061/#2065 story 2). trigger=event: the turn-end(no-advance)
	// consumer's guarded write made a live claim lease-due (the
	// entry-acceleration path). trigger=sweep_lapse: rows the sweep's
	// lease-due scan discovered (the backstop's own entry count —
	// rows the event path pre-marked converge INTO this count one
	// sweep later, so rate(event)/rate(sweep_lapse) is the event
	// path's share of ladder entries, the adoption signal #2060 R6).
	DueMarksMetricName = "board_due_marks_total"
	// TurnAdvancesMetricName — turn-end events whose turn carried a
	// board advance (advance=yes): telemetry ONLY (#2065 — the
	// advance leg due-marks nothing; the counter exists so the
	// no-advance share of emitted turn-ends is derivable).
	TurnAdvancesMetricName = "board_turn_advances_total"
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

	ladderHeartbeatLapse = promauto.NewCounter(prometheus.CounterOpts{
		Name: LadderHeartbeatLapseMetricName,
		Help: "Board ladder heartbeat-lapse arms fired (#2043): lease-due rows whose worker-anchored progress clock (last_progress_at) is stale past HeartbeatLapseThreshold (nil = lapsed by default) had their observation renewal evidence (movement/busy/post-nudge reaction) overridden into the quiet arms (nudge → heartbeat-or-wedge). The suppressed-renewal count is the two-clock split's always-on signature — the 2026-11-15 incident was invisible because renewal masqueraded as progress.",
	})

	heartbeatNoAdvanceCycles = promauto.NewCounter(prometheus.CounterOpts{
		Name: HeartbeatNoAdvanceCyclesMetricName,
		Help: "Motionless heartbeat responses counted against the no-advance budget (#2044): heartbeat answers whose declared watermark (observedProgressAt, stored into last_progress_at) never crossed the claim mint (stage_entered_at) — presence without advancement, renewing below NoAdvanceHeartbeatCap and declining at it. The heartbeat-orbit signature: climbing count + zero stage advance = a worker satisfying the nudge contract's letter with none of its substance.",
	})

	dueMarksByTrigger = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: DueMarksMetricName,
		Help: "Board claim due-mark entries by trigger (#2061/#2065): trigger=event counts guarded due-mark writes issued by the turn-end(no-advance) consumer (entry acceleration — the row enters the existing quiet arms one sweep after the worker's turn ends without a board advance); trigger=sweep_lapse counts rows the reconciler's lease-due scan discovered (the backstop's own entry count, including rows the event path pre-marked — they converge into the sweep one cycle later). rate(event)/rate(sweep_lapse) is the event path's share of ladder entries, the adoption/health signal (#2060 R6).",
	}, []string{"trigger"})

	turnAdvances = promauto.NewCounter(prometheus.CounterOpts{
		Name: TurnAdvancesMetricName,
		Help: "Board turn-end events whose turn carried a board advance (advance=yes; the advance set is record_progress | stage move | verdict). Telemetry ONLY — the advance leg due-marks nothing and touches no lease (#2065); emitted so the no-advance share of turn-ends is derivable against the emitter's published count.",
	})
)

// Trigger label values for DueMarksMetricName.
const (
	DueMarkTriggerEvent      = "event"
	DueMarkTriggerSweepLapse = "sweep_lapse"
)

// LeaseRenewals exposes the renewal counter (by source) for tests and
// dashboards — the StampWriteFailures accessor pattern: the counters are
// package-private; this is the read handle the increment pins assert
// deltas through (#2042 AC5).
func LeaseRenewals() *prometheus.CounterVec           { return leaseRenewalsBySource }
func LadderNudgesCounter() prometheus.Counter         { return ladderNudges }
func LadderWedgesCounter() prometheus.Counter         { return ladderWedges }
func LadderHeartbeatLapseCounter() prometheus.Counter { return ladderHeartbeatLapse }

// HeartbeatNoAdvanceCyclesCounter exposes the no-advance budget's
// cycle counter for tests and dashboards.
func HeartbeatNoAdvanceCyclesCounter() prometheus.Counter { return heartbeatNoAdvanceCycles }

// DueMarks exposes the due-mark counter (by trigger) for tests and
// dashboards — the accessor pattern above.
func DueMarks() *prometheus.CounterVec { return dueMarksByTrigger }

// TurnAdvancesCounter exposes the turn-end advance telemetry counter
// for tests and dashboards.
func TurnAdvancesCounter() prometheus.Counter { return turnAdvances }
