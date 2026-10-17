// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"encoding/json"
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
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
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
	Priority    string    `json:"priority"`
	ProjectName string    `json:"project_name"`
	Tags        []string  `json:"tags"`
	VFT         int64     `json:"vft"`
	Deficit     int64     `json:"deficit"`
	Quantum     int       `json:"quantum"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Derived progress (ruling R2: never stored): Done over (total − StageCancelled),
	// optionally criteria-weighted. Rendered by board_read only. A zero
	// denominator renders no-progress (IsNull true), never 0/0.
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
	Priority           string                `json:"priority"`
	ProjectName        string                `json:"project_name"`
	EpicName           string                `json:"epic_name"`
	Tags               []string              `json:"tags"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
	IsBlocked          bool                  `json:"is_blocked"`
	BlockedBy          []string              `json:"blocked_by,omitempty"` // derived: non-done dependency short IDs
	AssigneeSession    string                `json:"assignee_session,omitempty"`
	ClaimToken         string                `json:"claim_token,omitempty"`
	LeaseExpiresAt     *time.Time            `json:"lease_expires_at,omitempty"`
	LastProgressAt     *time.Time            `json:"last_progress_at,omitempty"`
	Attempts           int                   `json:"attempts"`
	ClaimIncremented   bool                  `json:"claim_incremented_attempts"`
	CooldownUntil      *time.Time            `json:"cooldown_until,omitempty"`
	StageEnteredAt     time.Time             `json:"stage_entered_at"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
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
	AbandonedStaged int `json:"abandoned_staged"` // RC3 canceled-dependency flips
	PendingStaged   int `json:"pending_staged"`   // pending-input auto-stage (worker origin)
	Resumes         int `json:"resumes"`          // pending-input resolutions (lease re-armed)
	EpicCompletions int `json:"epic_completions"` // RC4 net
}
