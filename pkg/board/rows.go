// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AcceptanceCriterion is one checkable, LLM-verifiable criterion (ruling
// R2). Done = ALL criteria met + evidence — the criteria define Done, they
// never block (ruling R3's scope guard).
type AcceptanceCriterion struct {
	Text string `json:"text"`
	Met  bool   `json:"met"`
}

// Project is the DB row shape for projects (swimlane).
type Project struct {
	ID                string     `json:"id"`
	ShortID           string     `json:"short_id"`
	UserID            string     `json:"user_id"`
	Name              string     `json:"name"`
	TargetWorkspaceID string     `json:"target_workspace_id,omitempty"`
	WorkerSlots       int        `json:"worker_slots"`
	SlotCooldownUntil *time.Time `json:"slot_cooldown_until,omitempty"`
	Tags              []string   `json:"tags"`
	Status            string     `json:"status"`
	Paused            bool       `json:"paused"` // #1747: the scheduling lever — blocks claiming, never interrupts leases
	// Backpressure is the derived informational note when the project's
	// target workspace pod is under resource pressure (#1747 round 3:
	// "backpressure: disk 92%" — the engine stops claiming into the
	// workspace; the row says why).
	Backpressure string    `json:"backpressure,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Epic is the DB row shape for epics, including the DRR fairness state
// (vft/deficit/quantum — Postgres is the only fairness state, §4.4).
type Epic struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	ShortID     string    `json:"short_id"`
	Title       string    `json:"title"`
	Outcome     string    `json:"outcome"`
	Status      string    `json:"status"`
	ProjectName string    `json:"project_name"`
	Tags        []string  `json:"tags"`
	VFT         int64     `json:"vft"`
	Deficit     int64     `json:"deficit"`
	Quantum     int       `json:"quantum"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Backpressure (derived, informational — see Project.Backpressure).
	Backpressure string `json:"backpressure,omitempty"`
	// Derived progress (never stored, ruling R2): Done over ALL tasks
	// (#1747). A zero denominator renders no-progress (IsNull true),
	// never 0/0.
	ProgressDone    int  `json:"progress_done"`
	ProgressTotal   int  `json:"progress_total"`
	ProgressDerived bool `json:"progress_derived"`
}

// Task is the DB row shape for tasks, including the dispatch state
// (claim_token, lease, attempts, provenance flag — §4.4).
type Task struct {
	ID                 string                `json:"id"`
	ProjectID          string                `json:"project_id"`
	EpicID             string                `json:"epic_id"`
	ShortID            string                `json:"short_id"`
	Title              string                `json:"title"`
	Description        string                `json:"description"`
	Stage              string                `json:"stage"`
	PendingOrigin      *string               `json:"pending_origin,omitempty"`
	ProjectName        string                `json:"project_name"`
	EpicName           string                `json:"epic_name"`
	Tags               []string              `json:"tags"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
	IsBlocked          bool                  `json:"is_blocked"`
	BlockedBy          []string              `json:"blocked_by,omitempty"` // derived: non-done dependency short IDs
	// Paused is the task-level scheduling lever (#1747): blocks claim
	// candidacy, claim mints, and forward stage-writes. Never a stage —
	// the five-stage vocabulary is binding; a paused in-flight worker
	// received a HALT directive and finishes its current turn.
	Paused bool `json:"paused"`
	// The failed-state attribute overlay (#1813): a task whose worker
	// attempt failed (lease-lapse reclaim — the overlay's ONLY writer)
	// carries a VISIBLE failure marker — NEVER a stage (the five-stage
	// vocabulary is binding). An all-success attempts-cap exhaustion
	// carries NO marker (its signal: pending_origin='escalated').
	// Reset by any successful turn (in_review/done entry) and the
	// pending_input human exit.
	LastAttemptFailed bool       `json:"last_attempt_failed"`
	FailureCount      int        `json:"failure_count"`
	LastFailureAt     *time.Time `json:"last_failure_at,omitempty"`
	AssigneeSession   string     `json:"assignee_session,omitempty"`
	ClaimToken        string     `json:"claim_token,omitempty"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at,omitempty"`
	LastProgressAt    *time.Time `json:"last_progress_at,omitempty"`
	Attempts          int        `json:"attempts"`
	ClaimIncremented  bool       `json:"claim_incremented_attempts"`
	CooldownUntil     *time.Time `json:"cooldown_until,omitempty"`
	// WaitingOn (derived, informational): "concurrency_limit" when the
	// row is ready but the per-user or per-instance concurrent-claim cap
	// is saturated (#1747 round 3 — over-cap is never an error; the row
	// says why it waits). Empty when not waiting.
	WaitingOn string `json:"waiting_on,omitempty"`
	// Backpressure (derived, informational — see Project.Backpressure).
	Backpressure   string    `json:"backpressure,omitempty"`
	StageEnteredAt time.Time `json:"stage_entered_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Transition is the DB row shape for board_transitions (measured actuals —
// ruling R2: cycle time is derived from this log, never an estimate column).
type Transition struct {
	ID          int64           `json:"id"`
	TaskID      string          `json:"task_id"`
	FromStage   string          `json:"from_stage"`
	ToStage     string          `json:"to_stage"`
	EvidenceURL string          `json:"evidence_url,omitempty"`
	Actor       json.RawMessage `json:"actor"`
	At          time.Time       `json:"at"`
}

// Event is the DB row shape for board_events (idempotency + audit).
type Event struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id"`
	ItemKind  string          `json:"item_kind,omitempty"`
	ItemID    string          `json:"item_id,omitempty"`
	Action    string          `json:"action"`
	Actor     json.RawMessage `json:"actor"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Claim is the outcome of a successful (or escalated) claim.
type Claim struct {
	TaskID         string    `json:"task_id"`
	ShortID        string    `json:"short_id"`
	ProjectID      string    `json:"project_id"`
	EpicID         string    `json:"epic_id"`
	WorkspaceID    string    `json:"workspace_id"`          // the project's target — the spawn's destination (R9)
	ClaimToken     string    `json:"claim_token,omitempty"` // empty on escalation (grant withheld)
	Assignee       string    `json:"assignee,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"` // zero on escalation
	// Escalated is true when the grant was withheld: the task's
	// pre-increment attempts >= N and it was staged to pending_input
	// (origin escalated) instead. The caller claims the next candidate.
	Escalated bool  `json:"escalated"`
	Task      *Task `json:"task,omitempty"`
}

// SearchFilters scopes a board search (design §4.8's search box: project
// focus, epic chips, tag filters, stage column). All fields optional; the
// query is required.
type SearchFilters struct {
	Project string // short id or UUID (project focus)
	Epic    string // short id or UUID (epic chip)
	Stage   string
	Tags    []string
}

// SearchResult is the search projection: matching task rows plus 0066-shape
// did-you-mean suggestions, populated only on zero-hit near-misses
// (suggest-only, never crossing the authorization perimeter).
type SearchResult struct {
	Results     []Task   `json:"results"`
	Suggestions []string `json:"suggestions"`
}

// HealthLease is one in-flight claim on the health panel (§4.8).
type HealthLease struct {
	TaskID          string    `json:"task_id"`
	ShortID         string    `json:"short_id"`
	Title           string    `json:"title"`
	ProjectID       string    `json:"project_id"`
	AssigneeSession string    `json:"assignee_session,omitempty"`
	LeaseExpiresAt  time.Time `json:"lease_expires_at"`
}

// HealthTaskCooldown is one throttled task's cooldown (§4.4 throttle path).
type HealthTaskCooldown struct {
	TaskID        string    `json:"task_id"`
	ShortID       string    `json:"short_id"`
	ProjectID     string    `json:"project_id"`
	CooldownUntil time.Time `json:"cooldown_until"`
}

// HealthProjectCooldown is one project's worker-slot cooldown (new claims
// paused while in-flight leases run their course).
type HealthProjectCooldown struct {
	ProjectID     string    `json:"project_id"`
	ShortID       string    `json:"short_id"`
	CooldownUntil time.Time `json:"cooldown_until"`
}

// HealthReclaim is one recent lease reclaim, sourced from board_events
// (the reconciler's deterministic-id rows).
type HealthReclaim struct {
	TaskID string    `json:"task_id"`
	At     time.Time `json:"at"`
}

// HealthProjectSlots reports per-project worker budget in use (the epic
// drawer's slots-in-use figure).
type HealthProjectSlots struct {
	ProjectID   string `json:"project_id"`
	ShortID     string `json:"short_id"`
	WorkerSlots int    `json:"worker_slots"`
	InUse       int    `json:"in_use"`
}

// Health is the board health snapshot (design §4.2 board_read(health) /
// §4.8's health panel): active leases + expiry, recent reclaims, task and
// project-slot cooldowns, pending_input by origin, per-project slots.
type Health struct {
	ActiveLeases         []HealthLease           `json:"active_leases"`
	TaskCooldowns        []HealthTaskCooldown    `json:"task_cooldowns"`
	ProjectCooldowns     []HealthProjectCooldown `json:"project_cooldowns"`
	PendingInputByOrigin map[string]int          `json:"pending_input_by_origin"`
	RecentReclaims       []HealthReclaim         `json:"recent_reclaims"`
	ProjectSlots         []HealthProjectSlots    `json:"project_slots"`
}

// HeartbeatResult reports the evidence gate's decision: a heartbeat with no
// progress evidence does not extend the lease (#1685: elapsed time is not
// liveness).
type HeartbeatResult struct {
	Extended       bool      `json:"extended"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	LastProgressAt time.Time `json:"last_progress_at"`
}

// SweepResult summarizes one reconciler pass (health panel feed).
type SweepResult struct {
	Dispatched      int `json:"dispatched"`       // §4.4 sweep-backstop drive
	Reclaims        int `json:"reclaims"`         // RC1
	DriftDone       int `json:"drift_done"`       // RC2 merged, criteria met
	DriftReview     int `json:"drift_review"`     // RC2 merged, criteria unmet
	DriftReentry    int `json:"drift_reentry"`    // RC2 closed-unmerged → in_progress
	DepRepairs      int `json:"dep_repairs"`      // RC3 is_blocked recomputes
	PendingStaged   int `json:"pending_staged"`   // pending-input auto-stage (worker origin)
	Resumes         int `json:"resumes"`          // pending-input resolutions (lease re-armed)
	EpicCompletions int `json:"epic_completions"` // RC4 net
}

// DeleteResult is the delete_task/delete_epic outcome (#1747 round 3:
// cancel is eliminated — delete replaces it). Delete removes the object,
// NOT the history: every delete event embeds the final object snapshot
// (per-task on cascade), and each orphaned dependent (a task that lost
// its depends_on edge) gets its own "unblocked_by_delete" audit event —
// affected-party visibility, warning-only.
type DeleteResult struct {
	ItemKind string `json:"item_kind"`
	ID       string `json:"id"`
	ShortID  string `json:"short_id"`
	// CascadeTasks: epic deletes list every removed task's short id
	// (each carries its own snapshot event).
	CascadeTasks []string `json:"cascade_tasks,omitempty"`
	// OrphanedDependents: dependents that lost an edge to the deleted
	// row(s) — surfaced so the deleter sees who was affected. NO
	// auto-reconnect ever (#1747 6261560142): A→B→C, delete B ⇒ C
	// becomes ready, no A→C is created — the deleter re-adds edges
	// explicitly, or passes reconnect=true.
	OrphanedDependents []string `json:"orphaned_dependents,omitempty"`
	// Reconnected: with reconnect=true, the inherited edges
	// ("orphan→upstream" short-id pairs) — deduplicated, cycle-guarded.
	Reconnected []string `json:"reconnected,omitempty"`
}

// PlanTaskInput is one node of an import_plan payload; DependsOn names
// OTHER KEYS in the same payload (the graph is self-contained in v1).
type PlanTaskInput struct {
	Key                string                `json:"key"`
	Title              string                `json:"title"`
	Description        string                `json:"description"`
	Tags               []string              `json:"tags"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptanceCriteria"`
	DependsOn          []string              `json:"dependsOn"`
}

// PlanInput is the import_plan payload: one epic + its tasks + edges,
// materialized atomically (all-or-nothing) or validated dry-run (#1747:
// the user's plan-approval moment).
type PlanInput struct {
	EpicTitle string          `json:"epicTitle"`
	Outcome   string          `json:"outcome"`
	Tags      []string        `json:"tags"`
	Tasks     []PlanTaskInput `json:"tasks"`
}

// PlanGraph is the rendered graph (dry-run validate mode).
type PlanGraph struct {
	Nodes []PlanNode `json:"nodes"`
	Edges []PlanEdge `json:"edges"` // from → to ("from depends on to")
}

// PlanNode is one node of the rendered graph (the plan render's shape).
type PlanNode struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// PlanEdge is one rendered edge: from depends on to.
type PlanEdge struct {
	From string `json:"from"` // the dependent (source) task's key
	To   string `json:"to"`   // the prerequisite's key
}

// PlanValidation is the dry-run render: graph + warnings + the initial
// ready set (deps satisfied, claimable once activated), BEFORE any
// write — the plan-approval moment.
type PlanValidation struct {
	Valid           bool      `json:"valid"`
	Error           string    `json:"error,omitempty"`
	Graph           PlanGraph `json:"graph"`
	Warnings        []string  `json:"warnings"`
	InitialReadySet []string  `json:"initial_ready_set"` // task keys, plan order
}

// ImportResult is the import_plan outcome (write or dry-run).
type ImportResult struct {
	DryRun      bool            `json:"dry_run"`
	Plan        *PlanValidation `json:"plan,omitempty"` // dry-run render
	ProjectID   string          `json:"project_id"`
	EpicID      string          `json:"epic_id,omitempty"`
	EpicShortID string          `json:"epic_short_id,omitempty"`
	// TaskKeys maps each payload key to its minted short id (write mode).
	TaskKeys map[string]string `json:"task_keys,omitempty"`
	// InitialReadySet mirrors PlanValidation.InitialReadySet with minted
	// short ids (write mode).
	InitialReadySet []string `json:"initial_ready_set,omitempty"`
	Warnings        []string `json:"warnings"`
}

// PressureSample is one workspace-pod resource-pressure report (#1747
// round 3: natural backpressure — the pod's agentd reports CPU/memory/
// disk over the seam; static thresholds + hysteresis live reporter-side).
type PressureSample struct {
	CPU      bool      `json:"cpu"`
	Memory   bool      `json:"memory"`
	Disk     bool      `json:"disk"`
	CPURate  float64   `json:"cpu_rate"`
	MemRate  float64   `json:"mem_rate"`
	DiskRate float64   `json:"disk_rate"`
	At       time.Time `json:"at"`
}

// Pressured reports whether the sample trips any dimension.
func (p PressureSample) Pressured() bool { return p.CPU || p.Memory || p.Disk }

// Summary renders the informational note ("disk 92%") — the surfaced
// form on project/epic/task reads.
func (p PressureSample) Summary() string {
	var parts []string
	if p.CPU {
		parts = append(parts, fmt.Sprintf("cpu %.0f%%", p.CPURate*100))
	}
	if p.Memory {
		parts = append(parts, fmt.Sprintf("memory %.0f%%", p.MemRate*100))
	}
	if p.Disk {
		parts = append(parts, fmt.Sprintf("disk %.0f%%", p.DiskRate*100))
	}
	if len(parts) == 0 {
		return ""
	}
	return "backpressure: " + strings.Join(parts, ", ")
}

// Worker directives (#1747 pause-must-pause): the engine→session
// messages delivered over the existing orchestrator↔worker message
// machinery at the pause/resume levers. They land at the current turn's
// boundary — a halt finishes the thought, then stops; it is NOT an
// abort (delete still uses the abort seam).
const (
	DirectiveHalt   = "halt"
	DirectiveResume = "resume"
)
