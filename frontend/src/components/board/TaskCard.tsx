// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from "react";
import { Link } from "react-router-dom";
import { Badge } from "../ui/Badge";
import { BusyIndicator } from "../ui/BusyIndicator";
import { KebabMenu, type KebabMenuItem } from "../ui/KebabMenu";
import { Link2, Play, AlertTriangle, Info } from "lucide-react";
import { cn } from "../../lib/utils";
import type { BoardTask } from "../../api/board";
import { STAGE_LABELS, PENDING_ORIGIN_LABELS, prAnchorOf, type BoardStage } from "../../lib/boardRules";

/**
 * The attempts-cap preview threshold — the R6 platform knob's DOCUMENTED
 * DEFAULT (3). The API does not surface the configured N (a deploy-level
 * knob, never per-project), so the preview keys on the default and the
 * title says so; the authoritative escalation is always the engine's
 * (pending_input/escalated), never this chip.
 */
export const ATTEMPTS_CAP_DEFAULT = 3;

/** Compact stage chip used inside the blocked-by view (live dep stages). */
function StageChip({ stage }: { stage: string }) {
  return (
    <span
      data-dep-stage={stage}
      className="rounded bg-muted px-1 py-px text-[10px] text-muted-foreground"
    >
      {STAGE_LABELS[stage as BoardStage] ?? stage}
    </span>
  );
}

interface TaskCardProps {
  task: BoardTask;
  /** Same-project rows for the blocked-by live stage chips (deps are
   * within-project by construction, ruling R4). */
  projectTasks: BoardTask[];
  /** True while an optimistic drag holds the card in a target column
   * (server not yet answered). */
  pending?: boolean;
  /** Snap-back flash: the server refused the optimistic move. */
  refused?: boolean;
  /** #1832 item 2: another epic in this swimlane is selected — this
   * card's epic is NOT it (rendered dimmed; the selection only ever
   * scopes display, never removes rows). */
  dimmed?: boolean;
  /** #1832 item 2: this card's epic is the swimlane's selection. */
  highlighted?: boolean;
  /** task_start (§4.2): the manual one-off dispatch — the designed UI
   * exit for backlog rows (backlog stage moves are not enumerated; the
   * engine's claim is the path). */
  onStart?: (task: BoardTask) => void;
  /** True while a task_start for this card is in flight. */
  starting?: boolean;
  /** #1832 item 3: the card's kebab menu (delete flow). Rendered
   * top-right; the trigger + items stopPropagation so a kebab click
   * never opens the editor. */
  kebabItems?: KebabMenuItem[];
  onOpen?: (task: BoardTask) => void;
  /** 2026-10-29 owner ruling: the project's target workspace id — the
   * assignee ref's chat-route first segment. Present whenever the
   * board knows the project (always, from the swimlane); absent only
   * in isolated renders, where the ref degrades to plain text. */
  workspaceId?: string;
  /** #1832 item 4: the details affordance — opens the task-detail
   * drawer (the card's primary click keeps its edit-dialog behavior). */
  onDetails?: (task: BoardTask) => void;
  onDragStart?: (task: BoardTask) => void;
  onDragEnd?: (task: BoardTask) => void;
}

/**
 * TaskCard — §4.8's card. 2026-10-28 rulings 4+5: the TITLE is the
 * primary label (the tsk- id lives only in the details drawer popup and
 * data/aria attributes); the layout wraps — long titles wrap INSIDE the
 * card (min-w-0 + break-words), action buttons sit in a shrink-0 cluster
 * that can never bleed into a neighboring column. Pause/waiting chips,
 * tags, the structural blocked badge (🔗 N with live dependency stage
 * chips, ruling R3), the assignee busy indicator, the attempt counter
 * with its escalation preview, and the backlog Start affordance
 * (task_start) are unchanged.
 */
export function TaskCard({ task, projectTasks, pending, refused, dimmed, highlighted, onStart, starting, kebabItems, onOpen, onDetails, workspaceId, onDragStart, onDragEnd }: TaskCardProps) {
  const [showBlockedBy, setShowBlockedBy] = useState(false);
  const anchor = prAnchorOf(task.tags);
  const depStages = (task.blocked_by ?? []).map((id) => {
    const dep = projectTasks.find((t) => t.short_id === id);
    return { id, stage: dep?.stage, title: dep?.title };
  });
  const escalateNext =
    task.attempts >= ATTEMPTS_CAP_DEFAULT &&
    (task.stage === "backlog" || task.stage === "in_progress") &&
    !task.pending_input; // 000050: a flagged row's claim lifecycle is suspended

  return (
    <div
      data-task-card={task.short_id}
      data-testid={`task-card-${task.short_id}`}
      data-stage={task.stage}
      data-dimmed={dimmed ? "true" : undefined}
      data-highlighted={highlighted ? "true" : undefined}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData("text/board-task", task.short_id);
        e.dataTransfer.effectAllowed = "move";
        onDragStart?.(task);
      }}
      onDragEnd={() => onDragEnd?.(task)}
      onClick={() => onOpen?.(task)}
      className={cn(
        "min-w-0 cursor-grab rounded-md border border-border bg-card p-2 text-left shadow-sm transition-colors hover:border-primary/40",
        pending && "opacity-60",
        refused && "border-destructive",
        dimmed && "opacity-40 saturate-50",
        highlighted && "border-primary/60 ring-1 ring-primary/40",
      )}
      aria-label={task.title}
    >
      {/* Ruling 5: title row — the title flexes and WRAPS within the
       * card (min-w-0 flex-1 break-words); the actions cluster is
       * shrink-0 so kebab/start/details stay inside the card's bounds
       * at any column width. */}
      <div className="flex items-start justify-between gap-1">
        <p
          className="mt-0.5 min-w-0 flex-1 break-words text-xs font-medium"
          title={task.title}
          data-testid={`card-title-${task.short_id}`}
        >
          {task.title}
        </p>
        <div className="flex shrink-0 items-center gap-0.5">
          {onDetails && (
            <button
              type="button"
              data-testid={`details-${task.short_id}`}
              onClick={(e) => {
                e.stopPropagation();
                onDetails(task);
              }}
              title="Story details — charter, criteria, full audit timeline"
              aria-label={`Story details ${task.short_id}`}
              className="rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
            >
              <Info className="h-3 w-3" />
            </button>
          )}
          {task.stage === "backlog" && onStart && (
            <button
              type="button"
              data-testid={`start-${task.short_id}`}
              onClick={(e) => {
                e.stopPropagation();
                onStart(task);
              }}
              disabled={starting}
              title="Story start (task_start) — manual one-off dispatch (claim + fresh worker, R9)"
              aria-label={`Start story ${task.short_id}`}
              className="rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50"
            >
              <Play className="h-3 w-3" />
            </button>
          )}
          {kebabItems && kebabItems.length > 0 && (
            <KebabMenu
              items={kebabItems}
              ariaLabel={`Story actions for ${task.short_id}`}
              testId={`task-kebab-${task.short_id}`}
            />
          )}
        </div>
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-1">
        {task.paused && (
          <Badge variant="warning" className="px-1.5 py-0 text-[10px]" title="Paused — claim candidacy off, forward writes refused (#1747)">
            paused
          </Badge>
        )}
        {task.waiting_on === "concurrency_limit" && (
          <Badge variant="muted" className="px-1.5 py-0 text-[10px]" title="Ready but the per-user/global concurrency cap is saturated (#1747)">
            waiting: concurrency
          </Badge>
        )}
        {task.is_blocked && (
          <button
            type="button"
            data-blocked-badge={task.short_id}
            data-testid={`blocked-badge-${task.short_id}`}
            onClick={(e) => {
              e.stopPropagation();
              setShowBlockedBy((v) => !v);
            }}
            className="inline-flex items-center gap-0.5 rounded bg-yellow-500/10 px-1.5 py-0.5 text-[10px] text-yellow-600 dark:text-yellow-400"
            aria-expanded={showBlockedBy}
            aria-label={`Blocked by ${depStages.length} dependencies`}
          >
            <Link2 className="h-3 w-3" /> {depStages.length}
          </button>
        )}
        {/* RC-C S2 (badge honesty): the spinner reflects LIVENESS, not
         * claim state — busy (mid-turn on a live lease) spins; idle,
         * unknown, or lapsed quiets to the distinct claimed-but-quiet
         * marker (assignee_busy is the server's derived-busy answer
         * under the lease TTL; absent = quiet, never the dishonest
         * spin of the 2026-10-30 incident). */}
        {task.stage === "in_progress" && task.assignee_session && <BusyIndicator size="sm" busy={task.assignee_busy === true} />}
        {/* 2026-10-29 owner ruling: the assignee ref is a CLICKABLE
         * session link (the card's busy area names its worker). The
         * value rides the read edge already phrase-translated (#1922)
         * — the phrase is the display text AND the route segment; the
         * cold window's raw harness id still links (never worse),
         * flagged via data-deprecated. stopPropagation: the link is
         * not a card click. */}
        {task.assignee_session &&
          (workspaceId ? (
            <Link
              to={`/chat/${encodeURIComponent(workspaceId)}/${encodeURIComponent(task.assignee_session)}`}
              data-testid={`assignee-link-${task.short_id}`}
              data-deprecated={task.assignee_session_deprecated ? "true" : undefined}
              onClick={(e) => e.stopPropagation()}
              title={
                task.assignee_session_deprecated
                  ? `${task.assignee_session} — raw session id (phrase not yet minted); opens the worker's chat view`
                  : `${task.assignee_session} — opens the worker's chat view`
              }
              className="max-w-[9rem] truncate font-mono text-[10px] text-muted-foreground underline decoration-dotted underline-offset-2 hover:text-primary"
            >
              {task.assignee_session}
            </Link>
          ) : (
            <span className="max-w-[9rem] truncate font-mono text-[10px] text-muted-foreground" title={task.assignee_session}>
              {task.assignee_session}
            </span>
          ))}
        {/* 000050: the input-wait flag rides the REAL stage — a badge
         * where the blocked thing lives; the escalated origin also marks
         * failed rows (the re-arm queue). */}
        {((task.pending_input && task.pending_origin) || task.stage === "failed") && task.pending_origin && (
          <Badge variant="warning" className="px-1.5 py-0 text-[10px]" data-testid={`pending-badge-${task.short_id}`}>
            {PENDING_ORIGIN_LABELS[task.pending_origin] ?? task.pending_origin}
          </Badge>
        )}
        {task.last_attempt_failed && (
          <Badge variant="destructive" className="px-1.5 py-0 text-[10px]" data-testid={`failed-${task.short_id}`}
            title={`Last attempt failed (${task.failure_count} consecutive — auto-return-to-ready behind doubling backoff). Never a stage: the row is ${task.stage}.`}
          >
            FAILED ×{task.failure_count}
          </Badge>
        )}
        {task.attempts > 0 && (
          <span
            className="text-[10px] text-muted-foreground"
            data-testid={`attempts-${task.short_id}`}
            title={`Claims granted (attempts). Escalates to Failed at the platform attempts cap (R6 default ${ATTEMPTS_CAP_DEFAULT}; the configured N is a deploy knob the API does not surface).`}
          >
            ⏱ {task.attempts}
          </span>
        )}
        {escalateNext && (
          <span
            className="inline-flex items-center gap-0.5 text-[10px] text-yellow-600 dark:text-yellow-400"
            data-testid={`escalation-preview-${task.short_id}`}
            title="The next claim escalates to Failed (attempts cap, R6)"
          >
            <AlertTriangle className="h-3 w-3" /> escalates next claim
          </span>
        )}
        {anchor && (
          <span className="rounded bg-muted px-1 font-mono text-[10px] text-muted-foreground" data-pr-anchor>
            {anchor}
          </span>
        )}
        {task.tags
          .filter((t) => t !== anchor)
          .slice(0, 3)
          .map((t) => (
            <span key={t} className="rounded bg-muted px-1 py-px text-[10px] text-muted-foreground">
              {t}
            </span>
          ))}
      </div>
      {showBlockedBy && depStages.length > 0 && (
        <div className="mt-1.5 flex flex-col gap-1 rounded bg-muted/50 p-1.5" data-testid={`blocked-by-${task.short_id}`}>
          {depStages.map(({ id, stage, title }) => (
            <div key={id} className="flex min-w-0 items-center justify-between gap-2" data-dep-id={id}>
              {/* Ruling 4: the dependency is named by TITLE; the tsk- id
               * rides the tooltip + data attr, not the chrome. */}
              <span className="min-w-0 flex-1 truncate text-[10px]" title={id}>
                {title ?? id}
              </span>
              {stage ? <StageChip stage={stage} /> : <span className="text-[10px] text-muted-foreground">—</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

