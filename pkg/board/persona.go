// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"log/slog"
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
// exist in — in_progress, in_review), any referenced agent role,
// config-driven through the stagePersonas config key (user defaults
// overlaid with per-project overrides, the #1832 item 6 merge).
//
// ROLE-ONLY (owner ruling 2026-10-29): a persona IS a reference to a
// platform agent role — the board layer defines no behavior of its
// own (no name, no instructions; the referenced role's model/tools/
// system-prompt are the single source of truth, and the default
// reviewer's verdict-channel prose is the SEEDED reviewer role's
// system prompt, DefaultReviewerRoleSystemPrompt).
//
// This ONE mechanism absorbs the dormant parallel auto-review
// assignment path (#1832 item 5's ReviewAssigner — a token-less
// manager-surface direct dispatch that raced no lease): the legacy
// reviewColumn=auto + autoAssignReviewer=true composition now renders
// the default reviewer PERSONA (the built-in reviewer role reference)
// for in_review and rides the same claim-then-spawn as every other
// persona (one lease, one token, movement-observed liveness,
// reconciler re-arm on lapse — the sub-orchestrator board test's
// design recommendation, generalized).
//
// Semantics preserved exactly: claim-before-spawn (never a spawn
// without a lease); token-gated forward writes (the stage machine's
// existing guard rule is untouched); liveness = movement observation
// + the nudge ladder (a zero-turn persona worker never answers its
// sentinel — the wedge verdict fails it, and the re-arm pass below
// re-pools a FRESH persona worker with the #1813 failure
// accounting and backoff); attempts/cooldown accounting (persona
// mints never increment attempts; a lapse counts consecutively and
// parks behind doubling backoff; failure_count at the attempts cap
// escalates to failed/escalated — the human-only exit (rearm).
//
// Stages with NO persona configured behave byte-identically to today:
// →in_review clears token+lease and spawns nothing (the dormant path
// never fired under the shipped defaults either).

// DefaultReviewerRoleSlug is the built-in reviewer agent role's slug:
// the role the platform SEEDS at startup (see
// seedBuiltinReviewerRole) carrying DefaultReviewerRoleSystemPrompt as
// its system prompt. The default reviewer always resolves to a role —
// the verdict-channel contract (board_record_verdict, the criteria
// walk, the Done guard) rides the role's system prompt, deterministic
// by construction; an org/user overriding the reviewer points
// in_review at their own role.
const DefaultReviewerRoleSlug = "reviewer"

// DefaultReviewerRoleSystemPrompt is the built-in reviewer role's
// system prompt — the default-reviewer verdict-channel prose VERBATIM
// (it lived in this package as the persona's instructions before the
// role-only ruling; the prose IS behavior, so it lives in the role
// definition the platform seeds, and this constant is its single
// source). pkg/board pins it; the app layer seeds it into the agent
// roles table at startup.
const DefaultReviewerRoleSystemPrompt = "Verify EACH acceptance criterion adversarially, with evidence. Record ONE structured verdict: " +
	"board_record_verdict with verdict=approve (requires the task's evidence URL and ALL criteria met — the Done guard applies regardless) " +
	"or verdict=fail with findings [{severity blocker|major|minor, pointer, criterionIndex}]. A fail bounces the task to its next claimant " +
	"with your findings embedded. Never merge, never push. " +
	"Recording criteria flags: as the reviewer persona you hold this row's claim — record each verified criterion with " +
	"board_update_story {storyId, claimToken, setCriteriaMet:{index, met}} (#1946: your OWN claimed row only); " +
	"with every criterion flagged and the evidence URL in hand, board_move_stage→done completes the review."

// DefaultReviewerPersona is the persona the LEGACY config composition
// (reviewColumn=auto AND autoAssignReviewer=true, no explicit
// stagePersonas.in_review) renders — a ROLE REFERENCE to the built-in
// reviewer agent role (slug form; applyPersonaRole canonicalizes to
// the seeded role's id at spawn resolution). It exists ONLY as the
// legacy-composition default; an explicit stagePersonas entry (any
// available role) replaces it. The verdict-channel charter itself is
// the role's system prompt (DefaultReviewerRoleSystemPrompt) — the
// board layer composes nothing onto it (owner ruling 2026-10-29:
// behavior definition lives in the agent-roles system, nowhere else).
var DefaultReviewerPersona = PersonaConfig{RoleID: DefaultReviewerRoleSlug}

// Label is the persona's display/stamp identity: the role reference
// (the only authored value). personaUnresolvedLabel covers the empty
// shape applyPersonaRole's degradation never mints but the exported
// surface must still render for.
func (p PersonaConfig) Label() string {
	if p.RoleID != "" {
		return p.RoleID
	}
	return personaUnresolvedLabel
}

// personaForStage resolves the effective persona for one stage: an
// explicit stagePersonas key (set at ANY merged tier — even an empty
// {}, the opt-out) OWNS the stage mapping and suppresses the legacy
// fallthrough entirely; only with NO explicit key anywhere does the
// legacy auto-review composition render the default reviewer persona
// (the built-in reviewer role reference) for in_review (existing
// stored configs keep their meaning — auto-review on IS in_review
// having a reviewer persona via the generic config). Every other
// shape returns nil (today's behavior, byte-identical).
func personaForStage(eff BoardConfig, stage string) *PersonaConfig {
	if eff.StagePersonasSet {
		if p, ok := eff.StagePersonas[stage]; ok {
			return &p
		}
		return nil // the explicit map owns the stages it lists — and the ones it doesn't
	}
	if stage == StageInReview && eff.ReviewColumn == ReviewColumnAuto && eff.AutoAssignReviewer {
		p := DefaultReviewerPersona
		return &p
	}
	return nil
}

// Persona provenance vocabulary (PersonaProvenanceForStage's answers):
// WHERE one stage's effective persona comes from, in the same
// precedence personaForStage applies at spawn time.
const (
	// PersonaSourceProjectOverride: the stage is listed in the
	// project's explicit stagePersonas override.
	PersonaSourceProjectOverride = "project_override"
	// PersonaSourceUserDefault: the stage is listed in the user's
	// explicit stagePersonas defaults (inherited by the project).
	PersonaSourceUserDefault = "user_default"
	// PersonaSourceDefaultReviewer: no explicit map anywhere and the
	// legacy auto-review composition (reviewColumn=auto +
	// autoAssignReviewer) renders DefaultReviewerPersona for in_review.
	PersonaSourceDefaultReviewer = "default_reviewer"
	// PersonaSourceNoneExplicit: an explicit map OWNS the stage and
	// lists no persona for it (the explicit-key-owns-all-stages
	// semantics — rendered, never silent).
	PersonaSourceNoneExplicit = "none_explicit"
	// PersonaSourceNone: no persona, no map, no composition.
	PersonaSourceNone = "none"
)

// PersonaProvenanceForStage names WHERE one stage's effective persona
// comes from, from the two raw override maps (either may be nil) — the
// display twin of personaForStage's spawn-time resolution, sharing its
// precedence: a project explicit map owns every stage; else a user
// explicit map owns every stage; else the legacy composition renders
// the default reviewer for in_review; else none. eff is the tier's
// EFFECTIVE row (composition inputs read from it). Non-eligible stages
// render none.
func PersonaProvenanceForStage(user, project *BoardConfigOverride, eff BoardConfig, stage string) string {
	if !personaEligibleStages[stage] {
		return PersonaSourceNone
	}
	if project != nil && project.StagePersonas != nil {
		if _, ok := (*project.StagePersonas)[stage]; ok {
			return PersonaSourceProjectOverride
		}
		return PersonaSourceNoneExplicit
	}
	if user != nil && user.StagePersonas != nil {
		if _, ok := (*user.StagePersonas)[stage]; ok {
			return PersonaSourceUserDefault
		}
		return PersonaSourceNoneExplicit
	}
	if stage == StageInReview && eff.ReviewColumn == ReviewColumnAuto && eff.AutoAssignReviewer {
		return PersonaSourceDefaultReviewer
	}
	return PersonaSourceNone
}

// stagePersona resolves the effective persona for one stage of ONE
// project. Config-read failures degrade to no-persona WITH a warn
// line (the observability parity the absorbed maybeAutoAssignReview
// had) — the transition still lands; the reconciler's re-arm
// re-reads and heals. The resolved persona is ROLE-CANONICALIZED
// through the installed directory (applyPersonaRole): slug references
// resolve to canonical role ids, and an unresolvable reference
// degrades to persona-less (nil) — never a role-less spawn.
func (s *Service) stagePersona(ctx context.Context, p *Project, stage string) *PersonaConfig {
	eff, err := s.effectiveConfigForProject(ctx, p)
	if err != nil {
		slog.WarnContext(ctx, "board: stage-persona config read failed — degrading to no persona",
			"task_project", p.ShortID, "stage", stage, "err", err.Error())
		return nil
	}
	persona := personaForStage(eff, stage)
	if persona == nil {
		return nil
	}
	return s.applyPersonaRole(ctx, p, persona)
}

// applyPersonaRole resolves one resolved persona's role reference
// against the project owner's available vocabulary at spawn time and
// canonicalizes it (a slug reference stamps the role's canonical id —
// the id is what rides the dispatch; see boardSpawnRequest).
// Resolution outcomes:
//
//   - resolved → the canonical role id stamps the persona (the
//     reference rides the dispatch).
//   - unresolvable — the role was deleted after storage (the delete
//     path has no stagePersonas cleanup), the directory is blind
//     (nil), or the directory read FAILED — the persona DEGRADES to
//     NIL: the transition lands exactly like a persona-less stage
//     (no claim mint, no spawn — byte-identical to pre-persona), the
//     stale reference never rides the dispatch, and the reconciler's
//     re-arm pass re-reads and heals once the vocabulary carries the
//     role again (the seeded built-in reviewer makes the default
//     composition self-healing the same way). The config VIEW still
//     renders the stored reference (the owner sees and can fix the
//     drift). Never a failed spawn, never a role-less reviewer with
//     no verdict-channel prose — under the role-only ruling the ROLE
//     is the behavior, so an unresolvable role means there is no
//     behavior to spawn.
func (s *Service) applyPersonaRole(ctx context.Context, p *Project, persona *PersonaConfig) *PersonaConfig {
	if persona == nil || persona.RoleID == "" {
		return nil
	}
	roles, err := s.rolesForUserErr(ctx, p.UserID)
	if err != nil {
		slog.WarnContext(ctx, "board: agent-role directory read failed — persona degrades to persona-less (the re-arm heals it)",
			"task_project", p.ShortID, "role_ref", persona.RoleID, "err", err.Error())
		return nil
	}
	r := ResolveAgentRoleRef(persona.RoleID, roles)
	if r == nil {
		slog.WarnContext(ctx, "board: persona role reference no longer resolves — persona degrades to persona-less (the re-arm heals it)",
			"task_project", p.ShortID, "role_ref", persona.RoleID)
		return nil
	}
	out := *persona
	out.RoleID = r.ID
	return &out
}

// personaClaimSQL is the re-claim-class persona mint: fresh token +
// lease, attempts untouched (flag=false), progress watermark stamped
// now. The recorded identity columns are deliberately PRESERVED (not
// cleared): #1959 split the two consumers — assignee_session keeps
// its live-claim lifecycle (the persona worker's guarded spawn-stamp
// OVERWRITES it under the fresh token; the #1947 lapse pass clears it
// on death), while implementer_session (the bounce's identity input,
// captured at the forward exit) rides through the mint untouched and
// keeps the bounce targetable across the whole review lifecycle even
// while the reviewer's stamp is in flight. #1946: the mint stamps its
// PROVENANCE on the row (claim_persona=$4, the configured persona
// name) — the durable seam a reviewer-persona claim is distinguished
// from a task-worker claim on; every non-persona mint and every
// token-clearing write sets it NULL (migration 000050).
const personaClaimSQL = `
	claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8),
	last_progress_at=now(), claim_incremented_attempts=false, claim_persona=$4,
	nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL, busy_renews=0`

// personaClaimMintTx applies the persona mint to a row already moved
// into its persona stage (inside the caller's transition transaction)
// and returns the minted token. personaName stamps the claim's
// provenance (validated non-empty by the config decoder; the mint is
// only reached for a resolved persona). The nudge the caller returns
// fires the spawn strictly post-commit (r1-C5: a token handed out for
// a mint that rolled back would strand a worker holding a dead claim).
func (s *Service) personaClaimMintTx(ctx context.Context, tx pgx.Tx, taskID, stage, personaName string) (string, error) {
	token := uuidNew()
	tag, err := tx.Exec(ctx, `UPDATE tasks SET `+personaClaimSQL+`
		WHERE id=$1 AND stage=$5 AND NOT paused`, taskID, token, s.cfg.LeaseDuration.Seconds(), personaName, stage)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", &GuardError{Code: GuardStage, Detail: "the row left its persona stage — server-arbitrated refusal (or paused)"}
	}
	return token, nil
}

// personaClaimEventTx records the mint's audit event (action
// persona_claim, id unique per mint episode — the mint's own
// nanosecond; a retried transaction mints a fresh token AND a fresh
// id together) and returns it for post-commit SSE publication (the
// caller rides the nudge's events or commitPublishing's pending).
func personaClaimEventTx(ctx context.Context, tx pgx.Tx, projectID, taskID, stage string, persona *PersonaConfig, origin string, attempts int) (Event, error) {
	actor := Actor{Kind: ActorSystem, Surface: "engine"}
	if origin == "reconciler" {
		actor.Surface = SurfaceReconciler
	}
	id := origin + ":persona_claim:" + taskID + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
	payload := map[string]any{"task_id": taskID, "stage": stage, "attempts": attempts}
	if persona != nil {
		payload["persona"] = persona.Label()
	}
	return recordEventTx(ctx, tx, projectID, id, "persona_claim", "task", taskID, actor, payload)
}

// personaRearmCandidatesSQL selects the re-arm pass's candidate rows:
// unclaimed in_review rows past their cooldown that are NOT
// effectively paused (#1747: pause gates claiming and claim mints —
// the re-arm is a claim; the lapse pass's healing clear stays legal on
// paused rows exactly like rc1's in_progress reclaims). NO config
// prefilter: under the #1939 flipped floor the persona fires with ZERO
// stored config rows, so any EXISTS-gate would exclude the shipped
// default posture — Go's stagePersona resolution is the sole authority
// (a candidate with no resolved persona is skipped without a write).
// It lives HERE (not engine.go/reconciler.go) to keep the persona
// seam's query vocabulary beside its resolution logic — the
// ownership-agnostic pin (§4.7) holds either way: the SQL touches
// rows and pause flags only. 000050: pending_input is a FLAG on
// in_review rows — the NOT t.pending_input predicate keeps an
// owner-blocked park (drift_review) out of the persona re-arm feed
// (the claim-predicate pin: stage ∈ claimable AND !flag).
const personaRearmCandidatesSQL = `
	SELECT t.id, t.project_id, t.failure_count
	FROM tasks t
	JOIN epics e ON e.id = t.epic_id
	JOIN projects p ON p.id = t.project_id
	WHERE t.stage='in_review' AND t.claim_token IS NULL AND t.lease_expires_at IS NULL
	  AND NOT t.pending_input
	  AND NOT t.paused AND e.status <> 'paused' AND NOT p.paused
	  AND (t.cooldown_until IS NULL OR t.cooldown_until < now())`

// effectiveConfigForProject merges the owner's user-level defaults
// with the project's overrides (A1 semantics, read time).
func (s *Service) effectiveConfigForProject(ctx context.Context, p *Project) (BoardConfig, error) {
	eff := DefaultBoardConfig()
	if p.UserID != "" {
		u, err := s.userBoardConfigOverride(ctx, p.UserID)
		if err != nil {
			return eff, err
		}
		eff = MergeBoardConfig(eff, u)
	}
	ov, err := s.decodeProjectOverrides(ctx, p.ID)
	if err != nil {
		return eff, err
	}
	return MergeBoardConfig(eff, ov), nil
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
