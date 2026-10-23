// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../ui/Button";
import { BoardErrorNotice } from "./BoardErrorNotice";
import type { BoardDeleteResult, BoardTask } from "../../api/board";
import type { DescribedBoardError } from "../../lib/boardRules";

interface TaskDeleteDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  task: BoardTask | null;
  /** Same-project rows — the dependents preview derives from
   * blocked_by references to this task (live read model). */
  projectTasks: BoardTask[];
  /** Runs the destructive write. Returns true on success (the parent
   * closes the dialog + invalidates the read model). */
  onDelete: (task: BoardTask, reconnect: boolean) => Promise<boolean>;
  error: DescribedBoardError | null;
  onDismissError: () => void;
}

/**
 * TaskDeleteDialog — #1832 item 3's task kebab → delete flow. The
 * confirm modal exposes the 'reconnect dependents to upstream'
 * checkbox (#1747's optional flag): unchecked, dependents are simply
 * unblocked (each gets an unblocked_by_delete audit event); checked,
 * they inherit the deleted task's direct upstream edges (dedup +
 * cycle-guarded server-side). The dependents are surfaced pre-flight
 * from the read model so the choice is informed.
 */
export function TaskDeleteDialog({ open, onOpenChange, task, projectTasks, onDelete, error, onDismissError }: TaskDeleteDialogProps) {
  // The useState(false) initializer is a formality: the re-arm effect
  // below is the default's real authority (it fires on every open AND
  // every task switch, covering the dialog-reused-same-mount case the
  // initializer cannot). The double-written default is deliberate —
  // mutation note M1 from the #1843 adjudication.
  const [reconnect, setReconnect] = useState(false);

  // Re-arm the checkbox per invocation (each delete starts unchecked —
  // plain-unblock is the #1747 default the API itself ships).
  useEffect(() => {
    if (open) setReconnect(false);
  }, [open, task?.short_id]);

  const dependents = task
    ? projectTasks.filter((t) => t.short_id !== task.short_id && (t.blocked_by ?? []).includes(task.short_id))
    : [];

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (!next) onDismissError();
        onOpenChange(next);
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed left-1/2 top-1/2 z-50 flex w-[26rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl"
          data-testid="task-delete-modal"
        >
          <Dialog.Title className="text-sm font-semibold">
            Delete task <span className="font-mono text-xs">{task?.short_id}</span>
          </Dialog.Title>
          <Dialog.Description className="text-xs text-muted-foreground">
            Destructive and permanent — the object is removed (the audit log keeps the full
            final-state snapshot). “{task?.title}”
          </Dialog.Description>
          {error && <BoardErrorNotice error={error} title="Delete refused" onDismiss={onDismissError} />}

          <div className="flex flex-col gap-1.5 rounded-md bg-muted/50 p-2 text-xs" data-testid="task-delete-dependents">
            {dependents.length === 0 ? (
              <span className="text-muted-foreground">No dependents — this task blocks nothing else.</span>
            ) : (
              <>
                <span className="font-medium">
                  {dependents.length} dependent{dependents.length > 1 ? "s" : ""} lose their edge on this task:
                </span>
                <ul className="ml-3 list-disc">
                  {dependents.map((d) => (
                    <li key={d.short_id} className="font-mono text-[11px]">
                      {d.short_id} <span className="font-sans text-muted-foreground">{d.title}</span>
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>

          <label className="flex items-start gap-2 rounded-md border border-border p-2 text-xs">
            <input
              type="checkbox"
              data-testid="reconnect-dependents"
              className="mt-0.5"
              checked={reconnect}
              onChange={(e) => setReconnect(e.target.checked)}
            />
            <span>
              <span className="font-medium">Reconnect dependents to upstream</span>
              <span className="block text-muted-foreground">
                Checked: each dependent inherits this task’s direct upstream edges (deduplicated,
                cycle-guarded). Unchecked: dependents are simply unblocked — each gets an
                unblocked_by_delete audit event.
              </span>
            </span>
          </label>

          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button
              variant="destructive"
              size="sm"
              data-testid="confirm-delete-task"
              disabled={!task}
              onClick={() => {
                if (task) void onDelete(task, reconnect);
              }}
            >
              Delete task
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** Renders the delete result as a page notice (orphaned vs reconnected). */
export function describeTaskDeleteResult(result: BoardDeleteResult): string {
  const base = `${result.short_id} deleted`;
  const orphans = result.orphaned_dependents ?? [];
  const reconnected = result.reconnected ?? [];
  if (reconnected.length > 0) {
    return `${base} — ${orphans.length} dependent${orphans.length === 1 ? "" : "s"} reconnected: ${reconnected.join(", ")}`;
  }
  if (orphans.length > 0) {
    return `${base} — ${orphans.length} dependent${orphans.length === 1 ? "" : "s"} unblocked (audit events recorded): ${orphans.join(", ")}`;
  }
  return `${base}.`;
}
