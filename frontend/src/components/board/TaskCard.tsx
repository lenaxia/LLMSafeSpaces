// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState } from "react";
import { Badge } from "../ui/Badge";
import { BusyIndicator } from "../ui/BusyIndicator";
import type { BadgeVariant } from "../ui/Badge";
import { Link2, Play, AlertTriangle } from "lucide-react";
import { cn } from "../../lib/utils";
import type { BoardTask } from "../../api/board";
import { STAGE_LABELS, PENDING_ORIGIN_LABELS, prAnchorOf, type BoardStage } from "../../lib/boardRules";

/** Priority chip color scale — P0 hottest. */
export function priorityVariant(priority: string): BadgeVariant {
  switch (priority) {
    case "P0":
      return "destructive";
    case "P1":
      return "warning";
    case "P3":
      return "muted";
    default:
      return "default";
  }
}

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
  /** task_start (§4.2): the manual one-off dispatch — the designed UI
   * exit for backlog rows (backlog stage moves are not enumerated; the
   * engine's claim is the path). */
  onStart?: (task: BoardTask) => void;
  /** True while a task_start for this card is in flight. */
  starting?: boolean;
  onOpen?: (task: BoardTask) => void;
  onDragStart?: (task: BoardTask) => void;
  onDragEnd?: (task: BoardTask) => void;
}

/**
 * TaskCard — §4.8's card: short ID (mono), priority chip, tags, the
 * structural blocked badge (🔗 N with live dependency stage chips, ruling
 * R3 — no evaluator, no Evaluate-now), the assignee busy indicator (the
 * #1687-hardened spinner on evidence the worker holds the claim), the
 * attempt counter with its escalation preview, and the backlog Start
 * affordance (task_start).
 */
export function TaskCard({ task, projectTasks, pending, refused, onStart, starting, onOpen, onDragStart, onDragEnd }: TaskCardProps) {
  const [showBlockedBy, setShowBlockedBy] = useState(false);
  const anchor = prAnchorOf(task.tags);
  const depStages = (task.blocked_by ?? []).map((id) => {
    const dep = projectTasks.find((t) => t.short_id === id);
    return { id, stage: dep?.stage };
  });
  const escalateNext =
    task.attempts >= ATTEMPTS_CAP_DEFAULT &&
    (task.stage === "backlog" || task.stage === "in_progress");

  return (
    <div
      data-task-card={task.short_id}
      data-testid={`task-card-${task.short_id}`}
      data-stage={task.stage}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData("text/board-task", task.short_id);
        e.dataTransfer.effectAllowed = "move";
        onDragStart?.(task);
      }}
      onDragEnd={() => onDragEnd?.(task)}
      onClick={() => onOpen?.(task)}
      className={cn(
        "cursor-grab rounded-md border border-border bg-card p-2 text-left shadow-sm transition-colors hover:border-primary/40",
        pending && "opacity-60",
        refused && "border-destructive",
      )}
      aria-label={`${task.short_id} ${task.title}`}
    >
      <div className="flex items-start justify-between gap-1">
        <span className="font-mono text-[10px] text-muted-foreground">{task.short_id}</span>
        <div className="flex items-center gap-1">
          {task.stage === "backlog" && onStart && (
            <button
              type="button"
              data-testid={`start-${task.short_id}`}
              onClick={(e) => {
                e.stopPropagation();
                onStart(task);
              }}
              disabled={starting}
              title="task_start — manual one-off dispatch (claim + fresh worker, R9)"
              aria-label={`Start task ${task.short_id}`}
              className="rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50"
            >
              <Play className="h-3 w-3" />
            </button>
          )}
          <Badge variant={priorityVariant(task.priority)} className="px-1.5 py-0 text-[10px]">
            {task.priority}
          </Badge>
        </div>
      </div>
      <p className="mt-0.5 line-clamp-2 text-xs font-medium">{task.title}</p>
      <div className="mt-1 flex flex-wrap items-center gap-1">
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
        {task.stage === "in_progress" && task.assignee_session && <BusyIndicator size="sm" />}
        {task.stage === "pending_input" && task.pending_origin && (
          <Badge variant="warning" className="px-1.5 py-0 text-[10px]">
            {PENDING_ORIGIN_LABELS[task.pending_origin] ?? task.pending_origin}
          </Badge>
        )}
        {task.attempts > 0 && (
          <span
            className="text-[10px] text-muted-foreground"
            data-testid={`attempts-${task.short_id}`}
            title={`Claims granted (attempts). Escalates to Pending Input at the platform attempts cap (R6 default ${ATTEMPTS_CAP_DEFAULT}; the configured N is a deploy knob the API does not surface).`}
          >
            ⏱ {task.attempts}
          </span>
        )}
        {escalateNext && (
          <span
            className="inline-flex items-center gap-0.5 text-[10px] text-yellow-600 dark:text-yellow-400"
            data-testid={`escalation-preview-${task.short_id}`}
            title="The next claim escalates to Pending Input (attempts cap, R6)"
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
          {depStages.map(({ id, stage }) => (
            <div key={id} className="flex items-center justify-between gap-2">
              <span className="font-mono text-[10px]">{id}</span>
              {stage ? <StageChip stage={stage} /> : <span className="text-[10px] text-muted-foreground">—</span>}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

