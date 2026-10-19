// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package board implements the Task Board backend (design 0067, issue
// #1696/#1698): the BoardService action layer, the Postgres-arbitrated
// dispatch engine (atomic claims, leases, evidence heartbeats, DRR
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
	// (#1685 semantics: elapsed time is not liveness). Default 15m.
	LeaseDuration time.Duration
	// ThrottleBackoff is the cooldown applied by report_throttle to both
	// the task (cooldown_until) and the project (slot_cooldown_until).
	// Default 5m.
	ThrottleBackoff time.Duration
	// DRRQuantum is the default per-epic deficit-round-robin round
	// allowance (epics.quantum). Default 1.
	DRRQuantum int
	// SweepInterval is the reconciler cadence (advisory for the sweep
	// runner; the sweep itself is stateless). Default 30s.
	SweepInterval time.Duration
}

// DefaultEngineConfig returns the R6 code defaults.
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   15 * time.Minute,
		ThrottleBackoff: 5 * time.Minute,
		DRRQuantum:      1,
		SweepInterval:   30 * time.Second,
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
	// the reconciler, cancel, and manager pending_input exits — the two
	// named caller-side overrides).
	ClaimToken string
	// WorkspaceID is the resolved pod workspace on the agent surface
	// (design §4.4 caller-workspace binding, structural: an in-workspace
	// worker may only touch tasks whose project targets ITS workspace).
	// Empty on manager surfaces (REST/external MCP scope by ownership).
	WorkspaceID string
}

// Actor kinds.
const (
	ActorUser   = "user"
	ActorSystem = "system"
)

// Surfaces (design §4.3): the four projections.
const (
	SurfaceREST        = "rest"         // (a) REST for UI + SDKs
	SurfaceExternalMCP = "external_mcp" // (b) customer-facing platform MCP
	SurfaceAgent       = "agent"        // (c) agent-internal registry (workers)
	SurfaceReconciler  = "reconciler"   // system actor, engine-internal
)

// Stages (ruling R2's binding set).
const (
	StageBacklog      = "backlog"
	StageInProgress   = "in_progress"
	StageInReview     = "in_review"
	StageDone         = "done"
	StagePendingInput = "pending_input"
	// Composed so misspell's fixer never rewrites the literal (the stage
	// value is the design-doc verbatim term; the DB CHECK depends on it).
	StageCancelled = "cancel" + "led" //nolint:misspell
)

// PendingInput origins (§4.1: four origins with distinct exits).
const (
	PendingOriginWorker              = "worker"
	PendingOriginEscalated           = "escalated"
	PendingOriginDependencyAbandoned = "dependency_abandoned"
	PendingOriginDriftReview         = "drift_review"
)

// Priorities (P0-P3, default P2).
const (
	PriorityP0 = "P0"
	PriorityP1 = "P1"
	PriorityP2 = "P2"
	PriorityP3 = "P3"
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
