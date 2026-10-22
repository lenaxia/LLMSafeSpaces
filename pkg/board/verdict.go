package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// #1832 item 5 (1/2): structured review verdicts. The verdict CORE is
// forge-agnostic (owner ruling on PR-B): a verdict is a validated
// payload persisted as an ordered per-task record (a board_events
// action) plus a guarded transition through the EXISTING stage machine
// — approve → done still passes the Done guard (ALL criteria met +
// evidence URL), fail → in_progress rides the bounce (fresh claim
// minted, attempts UNCHANGED — a bounce is an iteration inside the
// verdict chain, never a failure-ledger entry; the amended #1832
// semantics). Forge adapters (PR reviews, etc.) map INTO this core;
// they never re-implement it.

// VerdictKind is the verdict core's two-value outcome.
type VerdictKind string

const (
	VerdictApprove VerdictKind = "approve"
	VerdictFail    VerdictKind = "fail"
)

// VerdictFindingSeverity — the severity enum (blocker: must fix before
// merge; major: must fix before or in immediate follow-up; minor:
// advisory). The enum is closed; unknown severities are rejected at
// validation, mirroring the server's unknown-key posture.
type VerdictFindingSeverity string

const (
	SeverityBlocker VerdictFindingSeverity = "blocker"
	SeverityMajor   VerdictFindingSeverity = "major"
	SeverityMinor   VerdictFindingSeverity = "minor"
)

// VerdictFinding localizes one defect: severity + pointer (file:line,
// or the criterion text when the finding maps to a criterion) + the
// optional 0-based acceptance-criterion index it refutes.
type VerdictFinding struct {
	Severity       VerdictFindingSeverity `json:"severity"`
	Pointer        string                 `json:"pointer"`
	CriterionIndex *int                   `json:"criterionIndex,omitempty"`
}

// VerdictInput is the wire payload a reviewer submits.
type VerdictInput struct {
	Verdict     VerdictKind      `json:"verdict"`
	Findings    []VerdictFinding `json:"findings"`
	Reviewer    string           `json:"reviewer"`
	EvidenceURL string           `json:"evidenceUrl,omitempty"`
}

// Verdict is a persisted record (the ordered chain B2 surfaces in the
// next claimant's charter envelope).
type Verdict struct {
	Verdict    VerdictKind      `json:"verdict"`
	Findings   []VerdictFinding `json:"findings"`
	Reviewer   string           `json:"reviewer"`
	Timestamp  time.Time        `json:"timestamp"`
	TaskStage  string           `json:"taskStage"`  // the stage the task was in when the verdict landed
	Applied    bool             `json:"applied"`    // did the guarded transition succeed
	Transition string           `json:"transition"` // "" | done | in_progress | refused:<code>
}

// ValidateVerdict checks the payload against the task's criteria:
// verdict enum, at least the reviewer identity, severity enum, pointer
// presence, criterion-index bounds. A fail verdict SHOULD carry at
// least one finding (a fail with nothing to fix is a review smell) —
// enforced as validation.
func ValidateVerdict(in VerdictInput, criteria []AcceptanceCriterion) error {
	if in.Verdict != VerdictApprove && in.Verdict != VerdictFail {
		return fmt.Errorf("verdict must be %q or %q", VerdictApprove, VerdictFail)
	}
	if in.Reviewer == "" {
		return fmt.Errorf("reviewer identity is required")
	}
	if in.Verdict == VerdictFail && len(in.Findings) == 0 {
		return fmt.Errorf("a fail verdict requires at least one finding (what to fix)")
	}
	n := len(criteria)
	for i, f := range in.Findings {
		switch f.Severity {
		case SeverityBlocker, SeverityMajor, SeverityMinor:
		default:
			return fmt.Errorf("finding %d: severity must be blocker, major, or minor", i)
		}
		if f.Pointer == "" {
			return fmt.Errorf("finding %d: pointer is required (file:line or criterion text)", i)
		}
		if f.CriterionIndex != nil && (*f.CriterionIndex < 0 || *f.CriterionIndex >= n) {
			return fmt.Errorf("finding %d: criterionIndex %d out of bounds (task has %d criteria)", i, *f.CriterionIndex, n)
		}
	}
	return nil
}

// RecordVerdict validates + records the verdict and applies its
// transition through the EXISTING guarded stage machine, atomically
// (one review_verdict event carries record + transition outcome; the
// requestID is the idempotency key). A refused transition (approve
// with unmet criteria / missing evidence; bounce on a paused row) does
// NOT lose the record: the verdict persists with Applied=false and
// Transition="refused:<code>" — the chain keeps the evidence, the task
// stays in its stage, and the reviewer renders the refusal. A fail
// verdict rides the bounce: fresh claim minted for the recorded
// assignee, attempts UNCHANGED (a bounce is an iteration inside the
// chain, never a failure-ledger entry — the amended #1832 semantics).
func (s *Service) RecordVerdict(ctx context.Context, actor Actor, taskID, shortIDOrID string, in VerdictInput, requestID string) (*Task, *Verdict, error) {
	ref := pick(taskID, shortIDOrID)
	t, err := s.resolveTaskForActor(ctx, actor, ref)
	if err != nil {
		return nil, nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, nil, err
	}
	if err := ValidateVerdict(in, t.AcceptanceCriteria); err != nil {
		return nil, nil, &GuardError{Code: "validation", Detail: err.Error()}
	}

	v := Verdict{Verdict: in.Verdict, Findings: in.Findings, Reviewer: in.Reviewer, TaskStage: t.Stage}
	target := StageDone
	if in.Verdict == VerdictFail {
		target = StageInProgress
	}

	var pendingNudge nudge
	buildOutcome := func(applied bool, transition, resultingStage string) map[string]any {
		return map[string]any{
			"verdict": v.Verdict, "reviewer": v.Reviewer, "findings": v.Findings,
			"applied": applied, "transition": transition,
			"taskStage": v.TaskStage, "resulting_stage": resultingStage,
		}
	}
	outcome, dup, err := (&eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "review_verdict", itemKind: "task", itemID: t.ID, actor: actor}).run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		res, n, txErr := s.moveStageTx(ctx, tx, actor, t.ID, target, in.EvidenceURL)
		pendingNudge = n
		if txErr != nil {
			var guard *GuardError
			if errors.As(txErr, &guard) {
				// The refusal IS an outcome, not an error: the record
				// persists, the task stays put, the chain shows why.
				v.Applied = false
				v.Transition = "refused:" + guard.Code
				return buildOutcome(false, v.Transition, v.TaskStage), nil
			}
			return nil, txErr // db-class failures roll back — no phantom verdicts
		}
		v.Applied = true
		v.Transition = target
		pendingNudge = n
		return buildOutcome(true, target, res.ResultingStage), nil
	})
	if err != nil {
		return nil, nil, err
	}
	if dup != nil {
		// 200-with-original (§4.2): a replayed requestId returns the
		// ORIGINAL outcome from the event row, not a freshly-built row.
		if orig := dup.Result; orig != nil {
			if b, err := json.Marshal(orig); err == nil {
				var original Verdict
				if json.Unmarshal(b, &original) == nil && original.Verdict != "" {
					out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
					if err != nil {
						return nil, &original, err
					}
					return out, &original, nil
				}
			}
		}
	}
	_ = outcome
	// Deliveries fire only AFTER commit (r1-C5); the bounce's fresh
	// claim nudge and the done-cascade drive ride the established paths.
	pendingNudge.fire(ctx, s)
	if v.Applied && v.Transition == StageDone {
		s.pokeDrive(p.ID)
	}
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, &v, err
	}
	return out, &v, nil
}

// TaskVerdicts renders the task's ordered verdict chain (the record
// PR-B2 embeds in the next claimant's charter envelope). Owner
// perimeter identical to the task read.
func (s *Service) TaskVerdicts(ctx context.Context, actor Actor, taskID, shortIDOrID string) ([]Verdict, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	rows, err := s.q().Query(ctx, `
		SELECT payload, created_at FROM board_events
		WHERE project_id = $1 AND item_kind = 'task' AND item_id = $2 AND action = 'review_verdict'
		ORDER BY created_at ASC`, p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Verdict, 0, 4)
	for rows.Next() {
		var payload []byte
		var at time.Time
		if err := rows.Scan(&payload, &at); err != nil {
			return nil, err
		}
		var rec Verdict
		if err := json.Unmarshal(payload, &rec); err != nil {
			return nil, fmt.Errorf("board: verdict payload decode: %w", err)
		}
		rec.Timestamp = at
		out = append(out, rec)
	}
	return out, rows.Err()
}
