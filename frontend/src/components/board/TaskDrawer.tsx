// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import * as Dialog from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Button } from "../ui/Button";
import { BoardErrorNotice } from "./BoardErrorNotice";
import { X, Pencil, History } from "lucide-react";
import { boardApi, type BoardAuditEvent, type BoardTask, type BoardVerdict, type BoardVerdictFinding } from "../../api/board";
import { ApiClientError } from "../../api/client";
import { STAGE_LABELS, describeBoardError, type BoardStage, type DescribedBoardError } from "../../lib/boardRules";

/** Mirrors the service's TaskEventsDefaultLimit — the drawer always asks
 * for the full default window (the read is bounded server-side). */
const TASK_EVENTS_LIMIT = 200;

/** The action vocabulary, display-side (the engine's committed actions —
 * engine.go's recordEventTx calls; the reconciler's system rows ride the
 * same vocabulary with deterministic ids). */
const ACTION_LABELS: Record<string, string> = {
  create_task: "story created",
  update_task: "story updated",
  delete_task: "story deleted",
  move_stage: "stage moved",
  task_start: "manual dispatch (task_start)",
  claim: "claim granted",
  escalate: "escalated to failed (attempts cap)",
  reclaim: "lease reclaimed",
  pending_stage: "flagged pending input",
  pending_input_flag: "pending-input flag set/cleared",
  nudge: "lease nudge sent",
  wedge: "wedge verdict (failed)",
  rearm: "re-armed to backlog",
  resume: "resumed",
  drift_done: "drift corrected (done)",
  drift_review: "drift corrected (review)",
  drift_reentry: "drift corrected (re-entry)",
  add_dependency: "dependency added",
  remove_dependency: "dependency removed",
  unblocked_by_delete: "unblocked by delete",
  heartbeat: "lease heartbeat",
  report_throttle: "throttled (429 requeue)",
  pause_task: "story paused",
  resume_task: "story resumed",
  import_plan: "plan imported",
};

function label(action: string): string {
  return ACTION_LABELS[action] ?? action;
}

/** Deterministic UTC HH:MM — the timeline's "when" column. */
function hhmm(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toISOString().slice(11, 16);
}

function asRecord(v: unknown): Record<string, unknown> | null {
  return typeof v === "object" && v !== null ? (v as Record<string, unknown>) : null;
}

function asVerdict(v: unknown): BoardVerdict | null {
  const rec = asRecord(v);
  if (!rec || typeof rec.verdict !== "string") return null;
  return rec as unknown as BoardVerdict;
}

interface TaskDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  task: BoardTask | null;
  /** Same-project rows for the blocked-by live stage chips (deps are
   * within-project by construction, ruling R4). */
  projectTasks?: BoardTask[];
  /** The edit affordance — hands the task back for the edit dialog. */
  onEdit?: (task: BoardTask) => void;
  /** The project's target workspace id — the session links' first chat
   * route segment (2026-10-29 owner ruling: the drawer's assignee /
   * implementer fields click straight through to the session's chat
   * view). Without it the fields render as plain text (never a broken
   * link). */
  workspaceId?: string;
}

/** The chat-route session link — the drawer's one shared renderer for
 * the assignee and implementer refs (owner ruling 2026-10-29: the ids
 * popup is where ids live, and ids that name sessions are CLICKABLE).
 * The value rides the read edge already phrase-translated (#1922) —
 * the phrase is both the display text and the route segment; the cold
 * window's raw harness id links too (still resolvable — never worse),
 * flagged via data-deprecated for the UI, never a broken href. An
 * archived/deleted session renders the link all the same: the chat
 * page owns the not-found state. */
function SessionLink({
  sessionId,
  workspaceId,
  deprecated,
  testId,
  label,
}: {
  sessionId: string;
  workspaceId?: string;
  deprecated?: boolean;
  testId: string;
  label: string;
}) {
  if (!sessionId) return null;
  if (!workspaceId) {
    return (
      <span data-testid={testId} title={sessionId}>
        {sessionId}
      </span>
    );
  }
  return (
    <Link
      to={`/chat/${encodeURIComponent(workspaceId)}/${encodeURIComponent(sessionId)}`}
      data-testid={testId}
      data-deprecated={deprecated ? "true" : undefined}
      title={
        deprecated
          ? `${sessionId} — raw session id (phrase not yet minted); opens the session's chat view`
          : `${label} ${sessionId} — opens the session's chat view`
      }
      className="max-w-[11rem] truncate font-mono text-primary underline decoration-dotted underline-offset-2 hover:text-primary/80"
    >
      {sessionId}
    </Link>
  );
}

/** One timeline row's action-specific payload arm. */
function EventDetail({ evt }: { evt: BoardAuditEvent }) {
  const p = asRecord(evt.payload) ?? {};
  switch (evt.action) {
    case "claim":
      return (
        <span className="font-mono text-[10px] text-muted-foreground">
          worker {String(p.worker ?? "—")} · attempt {String(p.attempts ?? "—")}
        </span>
      );
    case "task_start":
      return (
        <span className="font-mono text-[10px] text-muted-foreground">
          attempt {String(p.attempts ?? "—")} · fresh worker spawned (R9)
        </span>
      );
    case "move_stage":
      return (
        <span className="font-mono text-[10px] text-muted-foreground">
          {String(p.from_stage ?? "?")} → {String(p.resulting_stage ?? p.requested_to ?? "?")}
          {p.reclaimed === true ? " (reclaimed)" : ""}
        </span>
      );
    case "add_dependency":
      return <span className="font-mono text-[10px] text-muted-foreground">depends on {String(p.depends_on ?? "—")}</span>;
    case "remove_dependency":
      return <span className="font-mono text-[10px] text-muted-foreground">removed {String(p.removed ?? "—")}</span>;
    case "unblocked_by_delete":
      return <span className="font-mono text-[10px] text-muted-foreground">dependency {String(p.of ?? "—")} deleted</span>;
    case "delete_task": {
      const orphans = Array.isArray(p.orphaned_dependents) ? p.orphaned_dependents.length : 0;
      return (
        <span className="font-mono text-[10px] text-muted-foreground">
          {orphans > 0 ? `${orphans} orphaned dependent(s) unblocked` : "no dependents"}
        </span>
      );
    }
    default:
      return null;
  }
}

/** PR-B's structured verdict, rendered when it rides the move payload. */
function VerdictView({ verdict }: { verdict: BoardVerdict }) {
  return (
    <div className="mt-0.5 flex flex-col gap-0.5 rounded bg-muted/60 px-1.5 py-1" data-verdict={verdict.verdict}>
      <span className="text-[10px] font-medium">
        review verdict: <span className="font-mono">{verdict.verdict}</span>
        {verdict.reviewer ? <span className="ml-1 font-mono text-muted-foreground">by {verdict.reviewer}</span> : null}
      </span>
      {(verdict.findings ?? []).map((f: BoardVerdictFinding, i: number) => (
        <span key={i} data-finding data-severity={f.severity} data-criterion-index={String(f.criterion_index ?? "")} className="pl-2 text-[10px] text-muted-foreground">
          {f.pointer ? <span className="font-mono">{f.pointer}</span> : null}
          {f.note ? <span className="ml-1">— {f.note}</span> : null}
        </span>
      ))}
    </div>
  );
}

/**
 * TaskDrawer — #1832 item 4: the task-detail drawer. Header (title,
 * stage, claim/lease, blocked-by with live dep stages, attempts), the
 * charter + acceptance criteria with met state, and the FULL audit
 * timeline (the events read — stage transitions, claims who/when, review
 * verdicts when present, dependency-graph changes, orphan/unblock).
 */
export function TaskDrawer({ open, onOpenChange, task, projectTasks = [], onEdit, workspaceId }: TaskDrawerProps) {
  const eventsQuery = useQuery({
    queryKey: ["board", "task-events", task?.short_id ?? null],
    queryFn: () => boardApi.taskEvents(task!.short_id, TASK_EVENTS_LIMIT),
    enabled: open && task !== null,
  });

  if (!task) return null;

  const deps = (task.blocked_by ?? []).map((id) => {
    const dep = projectTasks.find((t) => t.short_id === id);
    return { id, stage: dep?.stage };
  });
  const events = eventsQuery.data ?? [];
  const error: DescribedBoardError | null = eventsQuery.error
    ? eventsQuery.error instanceof ApiClientError
      ? describeBoardError(eventsQuery.error)
      : { message: eventsQuery.error.message, suggestions: [] }
    : null;

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed right-0 top-0 z-50 flex h-full w-[26rem] max-w-[90vw] flex-col overflow-y-auto border-l border-border bg-background shadow-xl"
          aria-label={`Story ${task.short_id}`}
        >
          <div className="flex items-start justify-between gap-2 border-b border-border px-4 py-3">
            <Dialog.Title className="flex flex-col gap-0.5 text-sm font-semibold" data-testid="drawer-title">
              {task.title}
              <span className="font-mono text-[10px] font-normal text-muted-foreground" data-testid="drawer-short-id">
                {task.short_id}
              </span>
            </Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close" data-testid="drawer-close">
                <X className="h-4 w-4" />
              </Button>
            </Dialog.Close>
          </div>

          <div className="flex flex-1 flex-col gap-3 px-4 py-3 text-xs">
            {/* --- Header info: stage, claim/lease, attempts ------------------- */}
            <div className="grid grid-cols-2 gap-1.5">
              <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5">
                <span className="text-muted-foreground">Stage</span>
                <span className="font-medium" data-testid="drawer-stage">
                  {STAGE_LABELS[task.stage as BoardStage] ?? task.stage}
                </span>
              </div>
              <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5">
                <span className="text-muted-foreground">Attempts</span>
                <span className="font-mono" data-testid="drawer-attempts">
                  {task.attempts}
                </span>
              </div>
            </div>
            <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5">
              <span className="text-muted-foreground">Claim / lease</span>
              <span className="flex items-center gap-2 font-mono text-[10px]">
                {task.assignee_session ? (
                  <SessionLink
                    sessionId={task.assignee_session}
                    workspaceId={workspaceId}
                    deprecated={task.assignee_session_deprecated}
                    testId="drawer-assignee"
                    label="Worker"
                  />
                ) : null}
                <span data-testid="drawer-lease">
                  {task.lease_expires_at ? `until ${hhmm(task.lease_expires_at)}` : "unclaimed —"}
                </span>
              </span>
            </div>
            {/* 2026-10-29 owner ruling: the #1959 implementer identity is
             * on the read edge — render it as the second clickable
             * session ref (during review the assignee is the reviewer;
             * this names the session that did the work). */}
            {task.implementer_session && (
              <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5">
                <span className="text-muted-foreground" title="The session that moved this row to review (the original worker; during review the assignee is the reviewer)">
                  Implementer
                </span>
                <SessionLink
                  sessionId={task.implementer_session}
                  workspaceId={workspaceId}
                  deprecated={task.implementer_session_deprecated}
                  testId="drawer-implementer"
                  label="Implementer"
                />
              </div>
            )}
            {deps.length > 0 && (
              <div className="flex flex-col gap-1 rounded-md border border-border px-2 py-1.5" data-testid="drawer-blocked-by">
                <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Blocked by</span>
                {deps.map(({ id, stage }) => (
                  <div key={id} className="flex items-center justify-between gap-2">
                    <span className="font-mono text-[10px]">{id}</span>
                    {stage ? (
                      <span data-dep-stage={stage} className="rounded bg-muted px-1 py-px text-[10px] text-muted-foreground">
                        {STAGE_LABELS[stage as BoardStage] ?? stage}
                      </span>
                    ) : (
                      <span className="text-[10px] text-muted-foreground">—</span>
                    )}
                  </div>
                ))}
              </div>
            )}

            {/* --- Charter + acceptance criteria -------------------------------- */}
            <div className="flex flex-col gap-1">
              <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Charter</span>
              <div className="whitespace-pre-wrap rounded-md bg-muted/50 p-2" data-testid="drawer-charter">
                {task.description || <span className="text-muted-foreground">No charter (description empty).</span>}
              </div>
            </div>
            <div className="flex flex-col gap-1" data-testid="drawer-criteria">
              <span className="text-[10px] uppercase tracking-wide text-muted-foreground">Acceptance criteria</span>
              {task.acceptance_criteria.length === 0 ? (
                <span className="text-muted-foreground">None.</span>
              ) : (
                task.acceptance_criteria.map((c, i) => (
                  <div
                    key={i}
                    data-testid={`drawer-criterion-${i}`}
                    data-met={String(c.met)}
                    className="flex items-start gap-1.5 rounded px-1 py-0.5"
                  >
                    <span className={c.met ? "text-green-600 dark:text-green-400" : "text-muted-foreground"}>
                      {c.met ? "✔" : "○"}
                    </span>
                    <span className={c.met ? "" : "text-muted-foreground"}>{c.text}</span>
                  </div>
                ))
              )}
            </div>

            {/* --- The full audit timeline --------------------------------------- */}
            <div className="flex flex-col gap-1.5">
              <span className="flex items-center gap-1 text-[10px] uppercase tracking-wide text-muted-foreground">
                <History className="h-3 w-3" /> Audit timeline
              </span>
              {eventsQuery.isLoading ? (
                <span className="text-[10px] text-muted-foreground" data-testid="timeline-loading">
                  Loading timeline…
                </span>
              ) : error ? (
                <div data-testid="timeline-error">
                  <BoardErrorNotice error={{ message: error.message, suggestions: [] }} title="Timeline failed to load" />
                  <Button
                    variant="outline"
                    size="sm"
                    className="mt-1"
                    data-testid="timeline-retry"
                    onClick={() => void eventsQuery.refetch()}
                  >
                    Retry
                  </Button>
                </div>
              ) : events.length === 0 ? (
                <span className="text-[10px] text-muted-foreground" data-testid="timeline-empty">
                  No events recorded yet.
                </span>
              ) : (
                <ol className="flex flex-col gap-1" data-testid="timeline-list">
                  {events.map((evt) => {
                    const verdict = asVerdict(asRecord(evt.payload ?? {})?.verdict);
                    return (
                      <li
                        key={evt.id}
                        data-testid={`timeline-evt-${evt.id}`}
                        className="flex flex-col gap-0.5 rounded bg-muted/40 px-2 py-1"
                      >
                        <div className="flex items-center justify-between gap-2">
                          <span className="text-[11px] font-medium">{label(evt.action)}</span>
                          <span className="font-mono text-[10px] text-muted-foreground" title={evt.created_at}>
                            {hhmm(evt.created_at)}
                          </span>
                        </div>
                        <EventDetail evt={evt} />
                        {verdict ? <VerdictView verdict={verdict} /> : null}
                      </li>
                    );
                  })}
                </ol>
              )}
            </div>
          </div>

          {onEdit && (
            <div className="flex justify-end border-t border-border px-4 py-3">
              <Button variant="outline" size="sm" onClick={() => onEdit(task)} data-testid="drawer-edit">
                <Pencil className="mr-1 h-3.5 w-3.5" /> Edit story
              </Button>
            </div>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
