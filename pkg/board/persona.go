// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// persona.go — generalized stage-persona claim-then-spawn (owner
// directive 2026-10-24: "Make it expandable so it works on any
// arbitrary stage and can accept an agent persona"). When a task
// ENTERS a stage that has a persona configured, the engine CLAIMS the
// row — the existing lease + claim-token semantics, verbatim: a
// re-claim-class mint (fresh claim_token + lease, assignee NULL until
// the guarded spawn-stamp, attempts UNCHANGED, provenance flag false)
// — and spawns a fresh session whose charter envelope carries the
// configured persona. Nothing is hardwired to in_review or to
// "reviewer": any persona-eligible stage (a stage a live claim can
// exist in — in_progress, in_review), any persona name, config-driven
// through the stagePersonas config key (user defaults overlaid with
// per-project overrides, the #1832 item 6 merge).
//
// This ONE mechanism absorbs the dormant parallel auto-review
// assignment path (#1832 item 5's ReviewAssigner — a token-less
// manager-surface direct dispatch that raced no lease): the legacy
// reviewColumn=auto + autoAssignReviewer=true composition now renders
// the default reviewer PERSONA for in_review and rides the same
// claim-then-spawn as every other persona (one lease, one token,
// heartbeat-gated liveness, reconciler re-arm on lapse — the
// sub-orchestrator board test's design recommendation, generalized).
//
// Semantics preserved exactly: claim-before-spawn (never a spawn
// without a lease); token-gated forward writes (the stage machine's
// existing guard rule is untouched); liveness = new progress evidence
// (the #1832 WS4 stillborn guard treats a zero-turn persona worker as
// stillborn — its refused renewals lapse the lease, and the lapse
// path below re-pools a FRESH persona worker with the #1813 failure
// accounting and backoff); attempts/cooldown accounting (persona
// mints never increment attempts; a lapse counts consecutively and
// parks behind doubling backoff; failure_count at the attempts cap
// escalates to pending_input — the human-only exit).
//
// Stages with NO persona configured behave byte-identically to today:
// →in_review clears token+lease and spawns nothing (the dormant path
// never fired under the shipped defaults either).

// DefaultReviewerPersona is the persona the LEGACY config composition
// (reviewColumn=auto AND autoAssignReviewer=true, no explicit
// stagePersonas.in_review) renders — the carried-over semantics of
// #1832 item 5's reviewer charter, now riding the generic mechanism.
// It exists ONLY as the legacy-composition default; an explicit
// stagePersonas entry (any name, any instructions) replaces it.
var DefaultReviewerPersona = PersonaConfig{
	Name: "reviewer",
	Instructions: "Verify EACH acceptance criterion adversarially, with evidence. Record ONE structured verdict: " +
		"board_record_verdict with verdict=approve (requires the task's evidence URL and ALL criteria met — the Done guard applies regardless) " +
		"or verdict=fail with findings [{severity blocker|major|minor, pointer, criterionIndex}]. A fail bounces the task to its next claimant " +
		"with your findings embedded. Never merge, never push.",
}

// personaForStage resolves the effective persona for one stage: an
// explicit stagePersonas entry wins; the legacy auto-review
// composition renders the default reviewer persona for in_review
// (existing stored configs keep their meaning — auto-review on IS
// in_review having a reviewer persona via the generic config); every
// other shape returns nil (today's behavior, byte-identical).
func personaForStage(eff BoardConfig, stage string) *PersonaConfig {
	if p, ok := eff.StagePersonas[stage]; ok {
		return &p
	}
	if stage == StageInReview && eff.ReviewColumn == ReviewColumnAuto && eff.AutoAssignReviewer {
		p := DefaultReviewerPersona
		return &p
	}
	return nil
}

// stagePersona resolves the effective persona for one stage of ONE
// project (config read failures degrade to no-persona — the transition
// still lands; the reconciler's re-arm re-reads and heals).
func (s *Service) stagePersona(ctx context.Context, p *Project, stage string) *PersonaConfig {
	eff, err := s.effectiveConfigForProject(ctx, p)
	if err != nil {
		return nil
	}
	return personaForStage(eff, stage)
}

// personaClaimSQL is the re-claim-class persona mint (the same column
// discipline as reclaimTx): fresh token + lease, attempts untouched
// (flag=false), progress watermark stamped now. The recorded identity
// is deliberately PRESERVED (not cleared): the forward exit retains it
// for the bounce/RC2 consumer today, and the persona worker's guarded
// spawn-stamp OVERWRITES it under the fresh token — between mint and
// stamp the bounce path still sees a recorded assignee (its
// never-token-less-in_progress invariant).
const personaClaimSQL = `
	claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8),
	last_progress_at=now(), claim_incremented_attempts=false`

// personaClaimMintTx applies the persona mint to a row already moved
// into its persona stage (inside the caller's transition transaction)
// and returns the minted token. The nudge the caller returns fires the
// spawn strictly post-commit (r1-C5: a token handed out for a mint
// that rolled back would strand a worker holding a dead claim).
func (s *Service) personaClaimMintTx(ctx context.Context, tx pgx.Tx, taskID, stage string) (string, error) {
	token := uuidNew()
	tag, err := tx.Exec(ctx, `UPDATE tasks SET `+personaClaimSQL+`
		WHERE id=$1 AND stage=$4 AND NOT paused`, taskID, token, s.cfg.LeaseDuration.Seconds(), stage)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", &GuardError{Code: GuardStage, Detail: "the row left its persona stage — server-arbitrated refusal (or paused)"}
	}
	return token, nil
}

// personaClaimEventTx records the mint's audit event (action
// persona_claim) with a per-episode deterministic-ish id: unique per
// mint (the mint's own nanosecond), inert on an in-transaction replay
// (the whole transaction — mint and event — is atomic; a retried
// transaction mints a fresh token AND a fresh id together).
func personaClaimEventTx(ctx context.Context, tx pgx.Tx, projectID, taskID, stage string, persona *PersonaConfig, origin string, attempts int) error {
	actor := Actor{Kind: ActorSystem, Surface: "engine"}
	if origin == "reconciler" {
		actor.Surface = SurfaceReconciler
	}
	id := origin + ":persona_claim:" + taskID + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := map[string]any{"task_id": taskID, "stage": stage, "attempts": attempts}
	if persona != nil {
		payload["persona"] = persona.Name
	}
	_, err := recordEventTx(ctx, tx, projectID, id, "persona_claim", "task", taskID, actor, payload)
	return err
}

// personaRearmCandidatesSQL is the re-arm pass's cheap prefilter:
// unclaimed in_review rows past their cooldown whose project (or its
// owner) holds ANY board-config row. It names user_id (the owner's
// defaults tier) so it lives HERE, beside effectiveConfigForProject —
// engine.go/reconciler.go stay ownership-agnostic by construction
// (§4.7, pinned by TestKill_SourceLevelOwnershipPins; the
// inFlightCountsForProject precedent: the owner column is the config
// seam's business alone).
const personaRearmCandidatesSQL = `
	SELECT t.id, t.project_id, t.failure_count
	FROM tasks t
	WHERE t.stage='in_review' AND t.claim_token IS NULL AND t.lease_expires_at IS NULL
	  AND (t.cooldown_until IS NULL OR t.cooldown_until < now())
	  AND (
	    EXISTS (SELECT 1 FROM board_project_config_overrides o WHERE o.project_id = t.project_id)
	    OR EXISTS (SELECT 1 FROM projects p3 JOIN board_config_defaults d ON d.user_id = p3.user_id
	               WHERE p3.id = t.project_id))`

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
	return s.verdictChainFor(ctx, s.q(), projectID, taskID)
}
