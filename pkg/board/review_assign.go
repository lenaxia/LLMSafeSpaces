package board

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// #1832 item 5 (2/2): config-gated review-claim auto-assign + the
// verdict chain in the next claimant's charter.
//
// Auto-assign fires when a task enters in_review AND the project's
// EFFECTIVE config says reviewColumn=auto AND autoAssignReviewer=true
// (A1's merge semantics). manual/off (or auto without auto-assign)
// keep today's behavior EXACTLY — no event, no dispatch (pinned). The
// reviewer is dispatched DIRECTLY through the review-assigner seam —
// never queued behind the global worker FIFO (the single-reviewer
// latency multiplier is the design driver): a reviewer is a
// manager-surface actor, holds no claim token, and races no lease.
//
// The verdict chain (B1 records) rides the Claim the spawner receives:
// after a fail-bounce the next claimant's charter carries every prior
// verdict + findings (iteration history, not failure count).

// ReviewAssigner dispatches a reviewer for one in_review task (the
// production impl spawns a fresh reviewer session through the agentd
// seam; tests install a capture fake).
type ReviewAssigner func(ctx context.Context, task *Task, project *Project) error

// WithReviewAssigner installs the review-assigner seam. nil (default)
// disables auto-assign entirely — manual/off behavior even under an
// auto config (the gate then records the intent event only).
func WithReviewAssigner(ra ReviewAssigner) Option {
	return func(s *Service) { s.reviewAssigner = ra }
}

// effectiveConfigForProject merges the owner's user-level defaults
// with the project's overrides (A1 semantics, read time).
func (s *Service) effectiveConfigForProject(ctx context.Context, p *Project) (BoardConfig, error) {
	eff := DefaultBoardConfig()
	if p.UserID != "" {
		if raw, err := s.store.loadBoardConfigDefaults(ctx, s.q(), p.UserID); err == nil {
			if o, err := decodeBoardConfigRows("user defaults", raw); err == nil {
				eff = MergeBoardConfig(eff, o)
			}
		} else {
			return eff, err
		}
	}
	if raw, err := s.store.loadBoardProjectConfigOverrides(ctx, s.q(), p.ID); err != nil {
		return eff, err
	} else if o, err := decodeBoardConfigRows("project overrides", raw); err != nil {
		return eff, err
	} else {
		eff = MergeBoardConfig(eff, o)
	}
	return eff, nil
}

// maybeAutoAssignReview runs post-commit on →in_review (MoveStage):
// gated dispatch per the effective config. Failures log and leave the
// manual path intact (the reviewer dispatch is best-effort exactly
// like the worker spawn; nothing strands).
func (s *Service) maybeAutoAssignReview(ctx context.Context, p *Project, t *Task) {
	eff, err := s.effectiveConfigForProject(ctx, p)
	if err != nil {
		slog.WarnContext(ctx, "board: review auto-assign config read failed", "task_id", t.ID, "err", err.Error())
		return
	}
	if eff.ReviewColumn != "auto" || !eff.AutoAssignReviewer {
		return // manual/off today-behavior: no event, no dispatch
	}
	if _, _, err := (&eventWriter{svc: s, projectID: p.ID, requestID: "", action: "review_assign", itemKind: "task", itemID: t.ID, actor: Actor{Kind: ActorSystem, Surface: SurfaceReconciler}}).run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		return map[string]any{"auto": true, "review_column": eff.ReviewColumn}, nil
	}); err != nil {
		slog.WarnContext(ctx, "board: review_assign event failed", "task_id", t.ID, "err", err.Error())
		return
	}
	if s.reviewAssigner == nil {
		return
	}
	go func() { //nolint:gosec,contextcheck // deliberate: dispatch outlives the request context (same posture as spawnAfter)
		sctx := context.Background()
		if err := s.reviewAssigner(sctx, t, p); err != nil {
			slog.WarnContext(sctx, "board: review dispatch failed — manual review remains available",
				"task_id", t.ID, "err", err.Error())
		}
	}()
}

// verdictChainFor loads the task's ordered verdict chain (B1 records)
// for the Claim payload — the next claimant's charter input after a
// fail-bounce.
func (s *Service) verdictChainFor(ctx context.Context, q Queryer, projectID, taskID string) []Verdict {
	rows, err := q.Query(ctx, `
		SELECT payload, created_at FROM board_events
		WHERE project_id = $1 AND item_kind = 'task' AND item_id = $2 AND action = 'review_verdict'
		ORDER BY created_at ASC, id ASC`, projectID, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]Verdict, 0, 2)
	for rows.Next() {
		var payload []byte
		var at time.Time
		if err := rows.Scan(&payload, &at); err != nil {
			return out
		}
		var v Verdict
		if err := json.Unmarshal(payload, &v); err == nil {
			v.Timestamp = at
			out = append(out, v)
		}
	}
	return out
}

// claimVerdictChain is the Claim-building wrapper (query via the
// service pool).
func (s *Service) claimVerdictChain(ctx context.Context, projectID, taskID string) []Verdict {
	if s.q() == nil {
		return nil
	}
	return s.verdictChainFor(ctx, s.q(), projectID, taskID)
}
