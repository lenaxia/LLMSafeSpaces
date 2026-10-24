// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// bulk.go — #1836's bulk claim/complete variants at the engine seam:
// BulkTaskStart (the manual claim batch) and BulkMoveStage (the stage
// batch — completing review sets is the lived pain). Each item is ONE
// delegation to the single-task seam (TaskStart / MoveStage): every
// landed item keeps its own transaction, its own board_events audit
// row, its own board_transitions row, its own spawn/directive
// side effects — the batch adds ONLY the per-item result envelope,
// never a second write path.
//
// Contract (issue #1836):
//   - Response = per-item result array {id, status: done|failed,
//     error?} — partial failures reported, never all-or-nothing
//     (import_plan stays the atomic multi-create by design).
//   - Idempotent retry: re-submitting the batch (or its failed subset)
//     is safe — an item the batch already landed (claim: in_progress;
//     move: already at the target stage) returns done, NOT a conflict.
//   - Per-item validation: one malformed/missing id sinks only its own
//     item; the rest of the batch still processes.
//   - Batch cap (BulkMaxItems) with a clear overflow error — a
//     whole-batch invalid_input refusal, nothing processed.
//
// Seam deltas vs the 2026-10-21 spec, validated on origin/main:
//   - #1864 ruling (bulk projections never carry live claim tokens):
//     the per-item rows carry id/status/escalated/error ONLY — the
//     claim token stays on the per-task read for the claimant's own
//     flow. BulkTaskStart does NOT echo TaskStart's Claim envelope.
//   - #1831/#1930: item refs are word-phrase short ids (legacy random
//     and UUID forms still resolve — the dual-accept window); the
//     per-item id echoes the caller's ref VERBATIM.
//   - Never-disclose perimeter: a foreign row renders the same
//     not_found item a miss renders (REST parity — no owner identity,
//     no cross-boundary suggestions).

import (
	"context"
	"fmt"
)

// BulkMaxItems caps one bulk batch (#1836 contract 4 — blast-radius
// bound; over-cap is a whole-batch refusal).
const BulkMaxItems = 50

// Per-item status vocabulary (#1836 contract 1).
const (
	BulkDone   = "done"
	BulkFailed = "failed"
)

// BulkItemNotFound is the per-item code for resolution misses AND
// foreign rows (the never-disclose class: both render the same item).
const BulkItemNotFound = "not_found"

// BulkItemError is one failed item's error: the engine's guard code
// (the same vocabulary renderBoardError maps) plus its detail.
type BulkItemError struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// BulkItemResult is one submitted ref's outcome. Ref echoes the
// caller's id verbatim (phrase, legacy, or UUID — self-identifying);
// Escalated carries the claim variant's attempts-cap escalation (the
// single-task Claim.Escalated flag — surfaced, never conflated with
// failure); Error is present exactly on failed items.
type BulkItemResult struct {
	Ref       string         `json:"id"`
	Status    string         `json:"status"`
	Escalated bool           `json:"escalated,omitempty"`
	Error     *BulkItemError `json:"error,omitempty"`
}

// bulkValidateBatch is the shared batch-level guard: presence and cap
// (#1836 contracts 3+4 — a batch-level problem poisons every item, so
// it refuses whole as invalid_input).
func bulkValidateBatch(refs []string) error {
	if len(refs) == 0 {
		return &GuardError{Code: GuardInput, Detail: "bulk taskIds must carry at least one task"}
	}
	if len(refs) > BulkMaxItems {
		return &GuardError{Code: GuardInput,
			Detail: fmt.Sprintf("bulk batch capped at %d items (got %d) — split the batch", BulkMaxItems, len(refs))}
	}
	return nil
}

// bulkItemRequestID derives the per-item idempotency key from the
// batch key: stable across retries of the same ref under the same
// batch requestID (so a re-submitted subset replays into the
// eventWriter's 200-with-original), distinct per item (two same-
// project items under one batch key never collide into one audit
// event). An absent batch key delegates to the single-task seams' own
// id generation. CALLER CONTRACT (the single-task rule, N-times
// amplified): one batch key per LOGICAL batch — reusing a key across
// two DIFFERENT batches replays the first batch's recorded outcomes
// as the second's (stale done items).
func bulkItemRequestID(requestID, ref string) string {
	if requestID == "" {
		return ""
	}
	return requestID + "#" + ref
}

// bulkFailedItem maps an item's single-task error to the per-item
// envelope. Every error class keeps its engine code verbatim (the
// parity vocabulary); resolution misses and foreign rows render the
// SAME bare not_found (miss ≡ foreign — the never-disclose shape: no
// did-you-mean on the bulk envelope, unlike the single-task 404 whose
// suggestions are safe to show there); unknown errors are honest
// internals (the single-task 500 body renders the same string).
func bulkFailedItem(ref string, err error) BulkItemResult {
	res := BulkItemResult{Ref: ref, Status: BulkFailed}
	switch e := err.(type) {
	case *NotFoundError:
		res.Error = &BulkItemError{Code: BulkItemNotFound, Detail: e.Error()}
	case *GuardError:
		res.Error = &BulkItemError{Code: e.Code, Detail: e.Detail}
	default:
		res.Error = &BulkItemError{Code: "internal_error", Detail: err.Error()}
	}
	return res
}

// bulkPerimeterOk resolves the item for the actor and applies the
// read-perimeter discipline the single-task paths enforce: an
// unresolvable ref is a miss; a row outside the actor's perimeter
// (foreign owner on the user surface; the agent surfaces are
// workspace-scoped from the start) renders the SAME not_found item a
// miss renders — never disclosed.
func (s *Service) bulkResolveItem(ctx context.Context, actor Actor, ref string) (*Task, bool) {
	t, err := s.resolveTaskForActor(ctx, actor, ref)
	if err != nil {
		return nil, false
	}
	if isAgentSurface(actor.Surface) && actor.WorkspaceID != "" {
		return t, true // workspace binding already scoped the resolve
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil || requireOwner(actor, p.UserID) != nil {
		return nil, false // foreign row: the miss class, never disclosed
	}
	return t, true
}

// BulkTaskStart is the bulk claim (#1836): each ready backlog item is
// claimed + spawned through the single-task TaskStart (draft∪active
// epics, all arbitration identical); an item already in_progress
// (a prior submission of this batch landed it) returns done — the
// idempotent-retry contract; every other refusal is that item's
// failed row with the single-task guard code.
func (s *Service) BulkTaskStart(ctx context.Context, actor Actor, refs []string, requestID string) ([]BulkItemResult, error) {
	if err := bulkValidateBatch(refs); err != nil {
		return nil, err
	}
	out := make([]BulkItemResult, 0, len(refs))
	for _, ref := range refs {
		out = append(out, s.bulkTaskStartOne(ctx, actor, ref, requestID))
	}
	return out, nil
}

func (s *Service) bulkTaskStartOne(ctx context.Context, actor Actor, ref, requestID string) BulkItemResult {
	t, ok := s.bulkResolveItem(ctx, actor, ref)
	if !ok {
		return bulkFailedItem(ref, &NotFoundError{Item: "task", Identifier: ref})
	}
	if t.Stage == StageInProgress {
		// The batch's own prior outcome: already claimed — done, not
		// conflict (#1836 contract 2). No write, no second spawn.
		return BulkItemResult{Ref: ref, Status: BulkDone}
	}
	c, err := s.TaskStart(ctx, actor, "", ref, bulkItemRequestID(requestID, ref))
	if err != nil {
		return bulkFailedItem(ref, err)
	}
	return BulkItemResult{Ref: ref, Status: BulkDone, Escalated: c.Escalated}
}

// BulkMoveStage is the bulk complete (#1836): each item is one
// server-arbitrated MoveStage (the full stage machine — Done guard,
// pause guards, claim-token arbitration, auto-review dispatch on
// →in_review, settle cascades on →done); an item already at the
// target stage returns done with no write (the idempotent-retry
// contract); a from-stage the machine does not enumerate for that
// item stays that item's failed row with the single-task guard code.
func (s *Service) BulkMoveStage(ctx context.Context, actor Actor, refs []string, toStage, evidenceURL, requestID string) ([]BulkItemResult, error) {
	if err := bulkValidateBatch(refs); err != nil {
		return nil, err
	}
	if !StageInVocabulary(toStage) {
		return nil, &GuardError{Code: GuardInput,
			Detail: "toStage must be one of the five stages: " + StageVocabulary()}
	}
	out := make([]BulkItemResult, 0, len(refs))
	for _, ref := range refs {
		out = append(out, s.bulkMoveStageOne(ctx, actor, ref, toStage, evidenceURL, requestID))
	}
	return out, nil
}

func (s *Service) bulkMoveStageOne(ctx context.Context, actor Actor, ref, toStage, evidenceURL, requestID string) BulkItemResult {
	t, ok := s.bulkResolveItem(ctx, actor, ref)
	if !ok {
		return bulkFailedItem(ref, &NotFoundError{Item: "task", Identifier: ref})
	}
	if t.Stage == toStage {
		// Already at the target stage — the batch's prior outcome:
		// done, not conflict (#1836 contract 2). No write.
		//
		// The pre-checks (this and the claim variant's in_progress
		// arm) are unlocked read-then-act by design: a concurrent
		// write landing between the read and the delegated write is
		// re-arbitrated UNDER THE ROW LOCK inside the single-task
		// seam — the item then reports failed with the machine's own
		// guard code (e.g. stage_transition), never a fabricated
		// outcome, and a retry converges (pinned by
		// TestBulk_PreCheckBoundaryDelegatesAuthority).
		return BulkItemResult{Ref: ref, Status: BulkDone}
	}
	if _, err := s.MoveStage(ctx, actor, "", ref, toStage, evidenceURL, bulkItemRequestID(requestID, ref)); err != nil {
		return bulkFailedItem(ref, err)
	}
	return BulkItemResult{Ref: ref, Status: BulkDone}
}

// (The five-stage vocabulary's single source is stageVocabulary in
// board.go — StageInVocabulary/StageVocabulary — shared with the
// adapters' presence checks; per-item from-stage enumeration stays
// the stage machine's.)
