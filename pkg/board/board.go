// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package board implements the Task Board backend (design 0067, issue
// #1696/#1698): the BoardService action layer, the Postgres-arbitrated
// dispatch engine (atomic claims, leases, movement-observed renewals, DRR
// fairness), and the self-healing convergence reconciler (RC1-RC5).
//
// Rulings conformed (2026-10-16, #1696): R1 epic+task only; R2 field model
// (no story points — measured stage timestamps only); R3/R4 blocking is
// purely structural via board_dependencies (no criteria evaluator); R5 no
// DAG entities; R6 attempts-cap N=3 default as a platform-deploy knob
// (lease duration + fairness windows join the same class); R7 user-level
// only boards; R8 single target workspace per project; R9 always-fresh
// workers (one fresh-charter session per claimed task, claim-then-spawn).
//
// There is NO in-memory scheduler state: Postgres arbitrates every race
// (atomic claim CTE, guarded reclaims, claim tokens), so any API replica
// can run the sweep and a crashed dispatcher loses nothing.
package board

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel error classes. Handlers map these to HTTP/MCP outcomes; the
// parity matrix (design §4.3) pins that mapping per surface.
var (
	// ErrNotImplemented is returned by skeleton stubs (red-first lane
	// discipline: the kill-tests pin behavior before the impl lands).
	ErrNotImplemented = errors.New("board: not implemented")

	// ErrNoCandidate is returned by ClaimNext when no claimable task
	// exists in scope (an empty claim is a normal outcome, not an error).
	ErrNoCandidate = errors.New("board: no claimable task in scope")
)

// EngineConfig carries the platform-deploy tunables (ruling R6): code
// defaults + override via helm values.yaml → env → api/internal/config.
// Never user-facing, never per-project (worker_slots stays project data).
type EngineConfig struct {
	// AttemptsCap is N — the attempts ceiling. The (N+1)th claim attempt
	// (pre-increment attempts >= N) escalates to pending_input instead of
	// granting. Default 3.
	AttemptsCap int
	// LeaseDuration is how long a claim holds without progress evidence
	// (#1685 semantics: elapsed time is not liveness). Default 45m
	// (000050: wall-clock is the DEATH detector, not the liveness
	// detector — the RC1 ladder renews on observed movement and nudges
	// the quiet case, so the window only bounds how long a PROVABLY
	// gone assignee's row can stay pinned; 15m burned live workers
	// mid-verification on long tool calls the watermark never stamped).
	LeaseDuration time.Duration
	// NudgeMeasureWindow is the nudge rung's measurement window (#2100,
	// lease-evidence-core design §3): the sentinel-armed row re-enters
	// the victims scan at +window, so the challenge's cost — the
	// park→nudge→verdict latency — is DECOUPLED from LeaseDuration
	// (which keeps its events-dead backstop and mid-turn coverage
	// duties; the 45→20m lease retune stays a separately-gated phase
	// and is NOT coupled here). Default 10m (≥3× the worst observed
	// legitimate response latency); <=0 clamps to the default at
	// validation. The deploy surface (config loader) floors the knob
	// at 5m; the engine honors any positive value so the integration
	// pins can run sub-floor windows exactly like the 200ms
	// LeaseDuration precedent.
	NudgeMeasureWindow time.Duration
	// HeartbeatLapseThreshold is how stale the worker-anchored progress
	// clock (tasks.last_progress_at) may get on a lease-due row before
	// the heartbeat-lapse arm fires (#2043, the 2026-11-15 incident's
	// remediation): past the threshold, ALL observation renewal
	// evidence (movement, busy, post-nudge reaction) is overridden and
	// the row proceeds through the quiet arms (nudge →
	// heartbeat-or-wedge). A heartbeat response or any worker progress
	// write resets the arm by refreshing the clock. Nil last_progress_at
	// is lapsed-by-default. Default 90m (2 lease windows: one window of
	// nudge measurement + one of response grace before the arm hardens).
	HeartbeatLapseThreshold time.Duration
	// NoAdvanceHeartbeatCap is K — the heartbeat-orbit budget (#2044,
	// the third #2040 orbit channel): the ceiling on CONSECUTIVE
	// heartbeat responses whose declared watermark never crosses the
	// claim mint (last_progress_at > stage_entered_at, one clock
	// domain). The first K motionless responses renew (presence is
	// alive, the budget counts); the K+1th DECLINES — arrival recorded,
	// sentinel kept armed, lease not extended — and the next lapse
	// demotes every renewal arm for that lapse (the heartbeat's own
	// PART frames are presence, not reaction, after #2042's allowlist),
	// landing the existing wedge verdict. Motion (a declared watermark
	// past the mint), any non-heartbeat renewal, and every claim mint
	// reset the count. Reviewer persona rows are structurally exempt
	// (the ladder never arms sentinels on in_review). Default 3
	// (≈ 2¼ h of presence-only orbiting on the 45 m lease before the
	// verdict — the same epoch-scale bound as boardBusyRenewCap).
	NoAdvanceHeartbeatCap int
	// ThrottleBackoff is the cooldown applied by report_throttle to both
	// the task (cooldown_until) and the project (slot_cooldown_until).
	// Default 5m.
	ThrottleBackoff time.Duration
	// FailureBackoff is the BASE auto-return-to-ready backoff for a
	// failed attempt (#1813): the lease-lapse reclaim parks the row
	// behind cooldown_until = now() + FailureBackoff << (n-1) (doubling
	// per CONSECUTIVE failure, capped at 1h). A successful turn or the
	// pending_input human exit clears the overlay + count. Default 60s.
	FailureBackoff time.Duration
	// DRRQuantum is the default per-epic deficit-round-robin round
	// allowance (epics.quantum). Default 1.
	DRRQuantum int
	// SweepInterval is the reconciler cadence (advisory for the sweep
	// runner; the sweep itself is stateless). Default 30s.
	SweepInterval time.Duration
	// UserClaimCap bounds concurrently in_progress tasks PER USER
	// (#1747 round 3 backpressure). Over-cap is NEVER an error: claims
	// quietly wait; task reads surface waiting_on=concurrency_limit.
	// Default 8.
	UserClaimCap int
	// GlobalClaimCap bounds concurrently in_progress tasks per instance
	// (#1747 round 3). Default 64.
	GlobalClaimCap int
	// MaxEdgesPerNode is the DAG blast-radius bound (#1747): edges per
	// task node, both directions counted. Default 16.
	MaxEdgesPerNode int
	// MaxNodesPerEpic is the DAG blast-radius bound (#1747): tasks per
	// epic (create + import_plan enforce it). Default 64.
	MaxNodesPerEpic int
	// WrapUpConsecutiveCap bounds consecutive wrap-up review closes per
	// epic before a human ack is required (epic wrap-up review, §2 step
	// 7): at a drain with wrapup_count >= cap and no pending ack the
	// engine arms wrapup_ack_pending (loud event, no mint) — a pedantic
	// reviewer cannot loop forever. The cap bounds the MINT loop only; a
	// PASS verdict is the terminus and is never cap-gated. Default 3.
	WrapUpConsecutiveCap int
	// PressureSampleTTL bounds how long a pod's resource-pressure report
	// stays authoritative without a refresh (#1747 round 3). Default 2m.
	PressureSampleTTL time.Duration
}

// DefaultEngineConfig returns the R6 code defaults (+ the #1747
// backpressure/graph-limit defaults).
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		AttemptsCap:             3,
		FailureBackoff:          time.Minute,
		LeaseDuration:           45 * time.Minute,
		NudgeMeasureWindow:      10 * time.Minute,
		HeartbeatLapseThreshold: 90 * time.Minute,
		NoAdvanceHeartbeatCap:   3,
		ThrottleBackoff:         5 * time.Minute,
		DRRQuantum:              1,
		SweepInterval:           30 * time.Second,
		UserClaimCap:            8,
		GlobalClaimCap:          64,
		MaxEdgesPerNode:         16,
		MaxNodesPerEpic:         64,
		WrapUpConsecutiveCap:    3,
		PressureSampleTTL:       2 * time.Minute,
	}
}

// Actor identifies who performed a board action (board_events.actor).
type Actor struct {
	// Kind is "user" or "system" (the reconciler is a system actor,
	// exempt from the claim-token guard by construction).
	Kind string
	// UserID is the acting user (workers delegate as the owning user,
	// ruling R7; empty for system actors).
	UserID string
	// Surface is the projection the action arrived on.
	Surface string
	// ClaimToken is the in-flight guard on caller surfaces (empty for
	// the reconciler, delete's clean abort, and the manager pending_input
	// exits — the named caller-side overrides).
	ClaimToken string
	// WorkspaceID is the resolved pod workspace on the agent surface
	// (design §4.4 caller-workspace binding, structural: an in-workspace
	// agent may only touch boards whose project targets ITS workspace —
	// #1747 extends the binding from worker reads to manager actions).
	// Empty on manager surfaces (REST/external MCP scope by ownership).
	WorkspaceID string
	// SessionID attributes the action to the creating session on the
	// agent surface (#1747: "audit events attribute every row to the
	// creating session") — the harness-attested injected identity. It
	// also feeds grant differentiation: a session that is (or was) a
	// board worker is refused manager actions. Empty on surfaces whose
	// callers hold no session identity (REST/external MCP: the user is
	// the actor; the engine).
	SessionID string
}

// Actor kinds.
const (
	ActorUser   = "user"
	ActorSystem = "system"
)

// Surfaces (design §4.3): the four projections.
const (
	SurfaceREST         = "rest"          // (a) REST for UI + SDKs
	SurfaceExternalMCP  = "external_mcp"  // (b) customer-facing platform MCP
	SurfaceAgent        = "agent"         // (c) agent-internal registry (workers)
	SurfaceAgentManager = "agent_manager" // (c-manager) the pod seam's manager projection (#1832 WS2; open to any in-workspace session — the manager-grant gate is repealed, ruling 2026-10-27 / tsk-copper-refectory-generates)
	SurfaceReconciler   = "reconciler"    // system actor, engine-internal
)

// nudgeMeasureWindowOrDefault renders the validated measurement window
// (the reconciler may be constructed with a hand-built config; the
// rung must never extend by zero).
func (c EngineConfig) nudgeMeasureWindowOrDefault() time.Duration {
	if c.NudgeMeasureWindow <= 0 {
		return 10 * time.Minute
	}
	return c.NudgeMeasureWindow
}

// failureBackoff is the #1813 auto-return-to-ready delay for the nth
// consecutive failure: base << (n-1), capped at one hour (a crash-looping
// row must never park forever — return-to-ready is automatic).
func (c EngineConfig) failureBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	b := c.FailureBackoff
	if b <= 0 {
		b = time.Minute // misconfigured/zero base: the documented default
	}
	for i := 1; i < n && b < time.Hour; i++ {
		b *= 2
	}
	if b > time.Hour {
		b = time.Hour
	}
	return b
}

// Stages (ruling R2's binding set, as amended by #1747 round 3: the
// eliminated cancel terminal state is GONE — delete_task removes rows
// outright; the machine is forward-moving with the one enumerated
// backward bounce and the human pending_input exits).
const (
	StageBacklog    = "backlog"
	StageInProgress = "in_progress"
	StageInReview   = "in_review"
	StageDone       = "done"
	// StageFailed is the failure terminus (000050: the attempts-exhaustion
	// escalation and the wedge verdict land here; rearm is the exit —
	// NEVER a pending_input costume, per the owner ruling "exhausted
	// retries should be failed status").
	StageFailed = "failed"
	// StagePendingInput remains a WIRE literal only (000050: flag-not-
	// stage). moveStage still ACCEPTS it as the flag-op alias — set the
	// pending_input flag (enter) / clear it + take the enumerated exit
	// (leave) — but no row STORES it; the storable vocabulary below
	// excludes it and the DB CHECK enforces that.
	StagePendingInput = "pending_input"
)

// stageVocabulary is the STORABLE stage set's single source (the read
// filter's vocabulary, the batch move's target vocabulary, and the
// adapters' presence checks all render from here — #1836 review r1:
// the inline literal maps had begun to drift).
var stageVocabulary = []string{StageBacklog, StageInProgress, StageInReview, StageDone, StageFailed}

// wireStageVocabulary is the acceptance set: the storable stages plus
// the pending_input flag-op alias (000050 — surfaces send the literal;
// the engine maps it to flag semantics and never persists it).
var wireStageVocabulary = append(append([]string{}, stageVocabulary...), StagePendingInput)

// StageVocabulary renders the five storable stages (joined) for
// caller-facing validation errors.
func StageVocabulary() string {
	return strings.Join(stageVocabulary, ", ")
}

// WireStageVocabulary renders the WIRE acceptance set (joined) — the
// storable stages plus the pending_input flag-op alias (000050: surfaces
// send the literal; the engine maps it to flag semantics).
func WireStageVocabulary() string {
	return strings.Join(wireStageVocabulary, ", ")
}

// StageInVocabulary reports whether a literal is one of the WIRE
// stages — the storable set PLUS the pending_input flag-op alias
// (000050: surfaces still send the literal; the engine maps it).
func StageInVocabulary(stage string) bool {
	for _, s := range wireStageVocabulary {
		if s == stage {
			return true
		}
	}
	return false
}

// PendingInput origins (§4.1: four origins with distinct exits).
const (
	PendingOriginWorker              = "worker"
	PendingOriginEscalated           = "escalated"
	PendingOriginDependencyAbandoned = "dependency_abandoned" // historical only: cancel is eliminated (#1747), nothing new lands this origin
	PendingOriginDriftReview         = "drift_review"
)

// Project / epic lifecycle statuses.
const (
	ProjectActive   = "active"
	ProjectArchived = "archived"

	EpicDraft     = "draft"
	EpicActive    = "active"
	EpicPaused    = "paused"
	EpicCompleted = "completed"
	EpicArchived  = "archived"
)

// NotFoundError is the 404-class (resolution failure) — carries
// did-you-mean suggestions per design 0066's helper shape (top-3,
// Levenshtein <= 2, suggest-only, 404-class-only).
type NotFoundError struct {
	Item        string
	Identifier  string
	Suggestions []string
}

func (e *NotFoundError) Error() string {
	if len(e.Suggestions) > 0 {
		return fmt.Sprintf("board: %s %q not found (did you mean: %v?)", e.Item, e.Identifier, e.Suggestions)
	}
	return fmt.Sprintf("board: %s %q not found", e.Item, e.Identifier)
}

// GuardError is the 409-class (stage machine / done guard / claim-token /
// workspace-binding violations). Code names the guard for the parity matrix.
type GuardError struct {
	Code   string
	Detail string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("board: guard %s: %s", e.Code, e.Detail)
}

// Guard codes.
const (
	GuardStage            = "stage_transition"     // not an enumerated transition
	GuardDone             = "done_guard"           // criteria unmet or evidence missing
	GuardClaimToken       = "claim_token"          // missing/mismatched token on an in-flight write
	GuardManagerSurface   = "manager_surface_only" // pending_input exits are human exits
	GuardWorkspaceBinding = "workspace_binding"    // caller-workspace != project target (403)
	GuardEpicState        = "epic_state"           // epic lifecycle refuses the action
	GuardDependency       = "dependency"           // edge rejected (same-project/self/acyclic)
	GuardProjectState     = "project_state"        // project lifecycle refuses the action
	GuardAuthz            = "authz"                // user-level ownership (R7)

	// #1747 additions: the pause lever, backpressure caps, and DAG
	// blast-radius limits.
	GuardPaused       = "task_paused"       // pause gates claiming, forward writes, and claim mints (#1747)
	GuardConcurrency  = "concurrency_limit" // user/global concurrent-claim cap refuses a manual dispatch
	GuardGraphLimit   = "graph_limit"       // edges-per-node / nodes-per-epic cap (blast-radius bound)
	GuardBackpressure = "backpressure"      // workspace resource pressure gates claiming (informational class)
	// S5 (the DEPOSED directive): a LIVE healthy claim refuses
	// deposition — the lease has not lapsed, or the assignee shows life.
	GuardClaimLive = "claim_live"
	// Claim-wait lifecycle (typed waits): the story already carries an
	// active event-wait — ONE per story (migration 000060 / board_wait).
	GuardWaitDeclared = "wait_already_declared"
	// #1699 (#1772): malformed arguments (rendered 400).
	GuardInput = "invalid_input"
)

// DependencyError is the dependency-write 409 (ruling R4a) — carries
// did-you-mean suggestions naming the near-miss existing tasks.
type DependencyError struct {
	Code        string // "cycle" | "cross_project" | "self"
	Detail      string
	Suggestions []string
}

func (e *DependencyError) Error() string {
	if len(e.Suggestions) > 0 {
		return fmt.Sprintf("board: dependency %s: %s (did you mean: %v?)", e.Code, e.Detail, e.Suggestions)
	}
	return fmt.Sprintf("board: dependency %s: %s", e.Code, e.Detail)
}

// DuplicateError is the idempotent-replay outcome ("200-with-original,
// never an error" — the outbox Duplicate{AcceptedID} contract). Result
// carries the original event's recorded outcome so callers re-render it.
type DuplicateError struct {
	EventID string
	Action  string
	Result  map[string]any
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("board: duplicate requestId %s (action %s)", e.EventID, e.Action)
}
