// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * boardRules — the Task Board's client-side rule mirrors (design 0067
 * §4.1/§4.8, issue #1702): stage vocabulary, the manager-surface view of
 * the engine's stage machine (pkg/board/stage.go), the OR4 pr:N anchor
 * form, and the 0066-shape did-you-mean suggestion helper.
 *
 * These mirrors are HINTS, never arbiters: every drag still calls the
 * server's move-stage endpoint and the engine's guard outcome (409 +
 * code) is rendered verbatim — the server is the single arbiter (§4.2);
 * a mirror drift shows up as an honest 409, not a client-side veto.
 *
 * Doctrine notes: (i) the error-code set below is BACKEND-DERIVED — it
 * comes from api/boardErrorCodes.json, the golden fixture the Go-side
 * parity test (api/internal/handlers/board_guard_codes_parity_test.go)
 * pins against a source-scan of pkg/board's emitted codes, so neither
 * side can drift alone; (ii) the OR4 client veto is deliberately
 * STRICTER than the engine (store.go's Sscanf accepts pr:01 and PR:12
 * passes as a free tag) — the client enforces the documented ruling
 * form; no invalid state is creatable, and the asymmetry is one-directional.
 */

// --- Stages (ruling R2's binding vocabulary) ----------------------------------

export const STAGE_BACKLOG = "backlog";
export const STAGE_IN_PROGRESS = "in_progress";
export const STAGE_IN_REVIEW = "in_review";
export const STAGE_DONE = "done";
export const STAGE_PENDING_INPUT = "pending_input";
export const STAGE_CANCELLED = "cancelled";

export const BOARD_STAGES = [
  STAGE_BACKLOG,
  STAGE_IN_PROGRESS,
  STAGE_IN_REVIEW,
  STAGE_DONE,
  STAGE_PENDING_INPUT,
  STAGE_CANCELLED,
] as const;

export type BoardStage = (typeof BOARD_STAGES)[number];

/** The four through-flow columns; Pending Input + Cancelled render as
 * collapsible side columns (§4.8 "Kanban"). */
export const MAIN_STAGES: BoardStage[] = [
  STAGE_BACKLOG,
  STAGE_IN_PROGRESS,
  STAGE_IN_REVIEW,
  STAGE_DONE,
];

export const SIDE_STAGES: BoardStage[] = [STAGE_PENDING_INPUT, STAGE_CANCELLED];

export const STAGE_LABELS: Record<BoardStage, string> = {
  [STAGE_BACKLOG]: "Backlog",
  [STAGE_IN_PROGRESS]: "In Progress",
  [STAGE_IN_REVIEW]: "In Review",
  [STAGE_DONE]: "Done",
  [STAGE_PENDING_INPUT]: "Pending Input",
  [STAGE_CANCELLED]: "Cancelled",
};

/** pending_origin (§4.1) — the health grouping's by-origin labels. */
export const PENDING_ORIGIN_LABELS: Record<string, string> = {
  worker: "Worker question",
  escalated: "Escalated (attempts cap)",
  dependency_abandoned: "Dependency abandoned",
  drift_review: "Drift review",
};

// --- The stage machine, manager-surface view ----------------------------------
//
// The engine's enumerated set (stage.go moveStageTx): in_progress →
// {in_review, done, pending_input}, in_review → {in_progress, done,
// pending_input}, pending_input → {backlog, in_progress}. backlog rows
// leave via the engine's claim (task_start is the manual path), and
// from-stage in_progress writes additionally require the LIVE CLAIM
// TOKEN — which the REST manager surface never holds (§4.2: in-flight
// writes fail the token guard and snap back). So the UI's drag-affordance
// mirror for the owner omits in_progress sources; a raced/claimed drag
// still attempts and renders the engine's honest 409 (claim_token /
// stage_transition).

const MANAGER_DRAG_TRANSITIONS = new Set<string>([
  `${STAGE_IN_REVIEW}→${STAGE_IN_PROGRESS}`,
  `${STAGE_IN_REVIEW}→${STAGE_DONE}`,
  `${STAGE_IN_REVIEW}→${STAGE_PENDING_INPUT}`,
  `${STAGE_PENDING_INPUT}→${STAGE_BACKLOG}`,
  `${STAGE_PENDING_INPUT}→${STAGE_IN_PROGRESS}`,
]);

/** Whether the owner's drag affordance should offer from→to at all
 * (mirror of stage.go's legal map minus the token-guarded in_progress
 * sources — the engine remains the arbiter). */
export function isManagerDragAllowed(from: string, to: string): boolean {
  return MANAGER_DRAG_TRANSITIONS.has(`${from}→${to}`);
}

/** Dropping onto done engages the engine's Done guard (ruling R2: ALL
 * acceptance criteria met + evidence_url). The client mirrors it only to
 * pre-collect the evidence URL; the guard's 409 is still rendered
 * verbatim when it fires. */
export function doneReadiness(task: { acceptance_criteria?: Array<{ text: string; met: boolean }> | null }): {
  allMet: boolean;
  unmet: string[];
} {
  const criteria = task.acceptance_criteria ?? [];
  // The engine's vacuous-truth mirror (store.go criteriaAllMet): zero
  // criteria = all met; Done then needs only the evidence URL.
  const unmet = criteria.filter((c) => !c.met).map((c) => c.text);
  return { allMet: unmet.length === 0, unmet };
}

// --- OR4: the pr:N PR-drift anchor (owner ruling OR4) --------------------------

/** The ONE PR-drift anchor form the reconciler's RC2 pass consumes
 * (explicit tag only — never free-text): ^pr:[1-9][0-9]*$ */
export const PR_ANCHOR_PATTERN = /^pr:[1-9][0-9]*$/;

/** Detects anything that LOOKS like a pr-anchor attempt (any case) so a
 * near-miss (pr:0, pr:01, PR:12, pr:x) is caught instead of silently
 * becoming a plain tag the engine ignores. */
const PR_ANCHOR_ATTEMPT = /^pr:/i;

export interface TagValidationError {
  tag: string;
  reason: string;
}

/** Validates the OR4 anchor form across a tag set (create/edit inputs).
 * Non-anchor tags pass untouched — tags are free-form vocabulary. */
export function validateBoardTags(tags: string[]): TagValidationError[] {
  const errors: TagValidationError[] = [];
  for (const tag of tags) {
    if (PR_ANCHOR_ATTEMPT.test(tag) && !PR_ANCHOR_PATTERN.test(tag)) {
      let reason: string;
      if (/[A-Z]/.test(tag)) {
        reason = "pr: anchors are lowercase `pr:<number>` (ruling OR4)";
      } else if (/^pr:0/.test(tag) || tag === "pr:0") {
        reason = "pr: anchors must be pr:<PR number> ≥ 1 — no zero (ruling OR4)";
      } else if (/^pr:\d+/.test(tag)) {
        reason = "pr: anchors carry no leading zeros (ruling OR4)";
      } else {
        reason = "pr: anchors must be pr:<PR number>, digits only (ruling OR4)";
      }
      errors.push({ tag, reason });
    }
  }
  return errors;
}

/** The anchor tag in a set, if present (for the card's PR chip). */
export function prAnchorOf(tags: string[] | undefined | null): string | undefined {
  return tags?.find((t) => PR_ANCHOR_PATTERN.test(t));
}

// --- Did-you-mean (design 0066 shape, client-side over loaded rows) ------------

/** Bounded Levenshtein (≤ max) — the 0066 editDistanceBounded shape:
 * early-exit rows past the bound so long ids stay cheap. */
function editDistanceBounded(a: string, b: string, max: number): number {
  if (Math.abs(a.length - b.length) > max) return max + 1;
  // fill(0) initializes every cell; the non-null assertions below are
  // structurally safe (indices always within bounds by the loops).
  const prev = new Array<number>(b.length + 1).fill(0);
  const cur = new Array<number>(b.length + 1).fill(0);
  for (let j = 0; j <= b.length; j++) prev[j] = j;
  for (let i = 1; i <= a.length; i++) {
    cur[0] = i;
    let rowMin = cur[0]!;
    const ac = a.charCodeAt(i - 1);
    for (let j = 1; j <= b.length; j++) {
      const cost = ac === b.charCodeAt(j - 1) ? 0 : 1;
      const v = Math.min(prev[j - 1]! + cost, prev[j]! + 1, cur[j - 1]! + 1);
      cur[j] = v;
      if (v < rowMin) rowMin = v;
    }
    if (rowMin > max) return max + 1;
    for (let j = 0; j <= b.length; j++) prev[j] = cur[j]!;
  }
  return prev[b.length]!;
}

export interface Suggestible {
  shortId: string;
  title: string;
}

/** Top-N did-you-mean suggestions over short ids + titles (0066 shape:
 * distance ≤ 2, suggest-only, used on search misses and 404 renderings
 * alike). Case-insensitive on ids — resolution is too. */
export function suggestBoardItems<T extends Suggestible>(
  query: string,
  candidates: T[],
  limit = 3,
): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return [];
  const scored: Array<{ item: T; score: number }> = [];
  for (const c of candidates) {
    const id = c.shortId.toLowerCase();
    const title = c.title.toLowerCase();
    let best = Math.min(
      editDistanceBounded(q, id, 2),
      editDistanceBounded(q, title, 2),
    );
    // Prefix hits are strong suggestions even past distance 2 (the
    // truncated-id case: "tsk-abc" → tsk-abc12345).
    if (id.startsWith(q) || title.startsWith(q)) best = Math.min(best, 1);
    if (best <= 2) scored.push({ item: c, score: best });
  }
  scored.sort((a, b) => a.score - b.score || a.item.shortId.localeCompare(b.item.shortId));
  return scored.slice(0, limit).map((s) => s.item);
}

// --- Guard codes + error shape (renderBoardError's client mirror) --------------

export const BOARD_GUARD_MESSAGES: Record<string, string> = {
  stage_transition: "That stage move is not one the machine enumerates.",
  done_guard: "Done requires ALL acceptance criteria met + an evidence URL (ruling R2).",
  claim_token: "The task holds a live worker claim — stage writes need its claim token; the card snapped back.",
  manager_surface_only: "Pending-input exits are human decisions (manager surfaces only).",
  workspace_binding: "The caller's workspace is not this project's target.",
  epic_state: "The epic's state refuses this action.",
  project_state: "The project's state refuses this action.",
  authz: "You do not own this board.",
  validation: "A field failed the engine's validation (priority, or a required name/title).",
  // DependencyError's codes (ruling R4a — the dependency-write 409 class
  // the server actually emits; GuardDependency is defined engine-side but
  // never returned, so it has no mirror here).
  cycle: "That dependency would create a cycle (ruling R4a).",
  cross_project: "Dependencies stay within one project (ruling R4).",
  self: "A task cannot depend on itself (ruling R4).",
};

import boardErrorCodesGolden from "../api/boardErrorCodes.json";

/** Every error code the backend can emit on the board surface — the
 * parity set BOARD_GUARD_MESSAGES must cover exactly. BACKEND-DERIVED
 * (r3): the golden fixture is the shared contract artifact — the Go-side
 * parity test (board_guard_codes_parity_test.go) pins it against a
 * source-scan of pkg/board's GuardError/DependencyError constructions,
 * and the vitest parity test pins this export (and the hint map) against
 * the same fixture. Neither side can drift alone. */
export const BOARD_ERROR_CODES: readonly string[] = boardErrorCodesGolden;

export interface DescribedBoardError {
  message: string;
  code?: string;
  suggestions: string[];
  hint?: string;
}

/** Renders a board ApiClientError body into display pieces: message,
 * guard code (409s carry one), and did-you-mean suggestions (404s carry
 * them). Suggestions are clickable chips that fill the invoking input. */
export function describeBoardError(err: {
  message?: string;
  status?: number;
  body?: { error?: string; code?: string; suggestions?: string[] };
}): DescribedBoardError {
  const body = err.body ?? {};
  const code = body.code;
  const suggestions = Array.isArray(body.suggestions) ? body.suggestions.filter((s) => typeof s === "string") : [];
  const hint = code ? BOARD_GUARD_MESSAGES[code] : undefined;
  return {
    message: body.error ?? err.message ?? "Board request failed",
    code,
    suggestions,
    ...(hint !== undefined ? { hint } : {}),
  };
}

// --- Priorities ----------------------------------------------------------------

export const BOARD_PRIORITIES = ["P0", "P1", "P2", "P3"] as const;
export type BoardPriority = (typeof BOARD_PRIORITIES)[number];
