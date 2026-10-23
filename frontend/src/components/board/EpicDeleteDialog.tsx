// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../ui/Button";
import { BoardErrorNotice } from "./BoardErrorNotice";
import type { BoardEpic, BoardTask } from "../../api/board";
import type { DescribedBoardError } from "../../lib/boardRules";

interface EpicDeleteDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  epic: BoardEpic | null;
  /** The epic's task rows (live read model) — the cascade count and
   * the in-flight census derive from them. */
  epicTasks: BoardTask[];
  /** Runs the destructive write. Returns true on success (the parent
   * closes the dialog + invalidates the read model). */
  onDelete: (epic: BoardEpic, force: boolean) => Promise<boolean>;
  error: DescribedBoardError | null;
  onDismissError: () => void;
}

/**
 * EpicDeleteDialog — #1832 item 3's epic kebab → delete flow, matching
 * the API's in-flight guard (#1747): the modal surfaces the epic's
 * in-flight workers (in_progress + assignee) from the read model
 * pre-flight, the first attempt never sends force (the engine refuses
 * with epic_state while claims are live), and ONLY the rendered
 * refusal reveals the force-confirm — an acknowledge checkbox gating
 * the force delete (force aborts each in-flight worker post-commit).
 */
export function EpicDeleteDialog({ open, onOpenChange, epic, epicTasks, onDelete, error, onDismissError }: EpicDeleteDialogProps) {
  const [acknowledgeForce, setAcknowledgeForce] = useState(false);

  useEffect(() => {
    if (open) setAcknowledgeForce(false);
  }, [open, epic?.id]);

  const inFlight = epicTasks.filter((t) => t.stage === "in_progress" && t.assignee_session);
  // The refusal's force gate renders only after the API actually
  // refused with epic_state — never proactively (the engine is the
  // arbiter; the census is advisory).
  const refusedInFlight = error?.code === "epic_state";

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
          className="fixed left-1/2 top-1/2 z-50 flex w-[28rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl"
          data-testid="epic-delete-modal"
        >
          <Dialog.Title className="text-sm font-semibold">
            Delete epic <span className="font-mono text-xs">{epic?.short_id}</span>
          </Dialog.Title>
          <Dialog.Description className="text-xs text-muted-foreground">
            Destructive and permanent — “{epic?.title}” and every task under it are removed (each
            task keeps its own snapshot event in the audit log; cross-epic dependents are
            surfaced as orphans, never a refusal).
          </Dialog.Description>
          {error && <BoardErrorNotice error={error} title="Delete refused" onDismiss={onDismissError} />}

          <div className="flex flex-col gap-1.5 rounded-md bg-muted/50 p-2 text-xs">
            <span>
              <span className="font-medium">Cascade:</span>{" "}
              <span data-testid="epic-delete-cascade-count">{epicTasks.length}</span>{" "}
              task{epicTasks.length === 1 ? "" : "s"} under this epic will be deleted.
            </span>
          </div>

          {inFlight.length > 0 && (
            <div
              className="flex flex-col gap-1.5 rounded-md border border-yellow-500/40 bg-yellow-500/5 p-2 text-xs"
              data-testid="epic-delete-inflight"
            >
              <span className="font-medium text-yellow-600 dark:text-yellow-400">
                {inFlight.length} worker{inFlight.length > 1 ? "s" : ""} in flight under this epic —
                the API refuses deletion without force:
              </span>
              <ul className="ml-3 list-disc">
                {inFlight.map((t) => (
                  <li key={t.short_id} className="font-mono text-[11px]">
                    {t.short_id} <span className="font-sans text-muted-foreground">{t.title}</span>
                    <span className="ml-1 font-sans text-[10px] text-muted-foreground/70">
                      ({t.assignee_session})
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}

          {refusedInFlight && (
            <div className="flex flex-col gap-2 rounded-md border border-destructive/40 bg-destructive/5 p-2 text-xs" data-testid="epic-delete-force-gate">
              <label className="flex items-start gap-2">
                <input
                  type="checkbox"
                  data-testid="force-abort-confirm"
                  className="mt-0.5"
                  checked={acknowledgeForce}
                  onChange={(e) => setAcknowledgeForce(e.target.checked)}
                />
                <span>
                  <span className="font-medium">Force: abort the in-flight worker{inFlight.length > 1 ? "s" : ""} and delete anyway.</span>
                  <span className="block text-muted-foreground">
                    force=true deletes the epic, then aborts each in-flight worker post-commit —
                    the rows’ removal is the authority; the aborts are cleanup.
                  </span>
                </span>
              </label>
              <Button
                variant="destructive"
                size="sm"
                className="self-end"
                data-testid="force-delete-epic"
                disabled={!acknowledgeForce || !epic}
                onClick={() => {
                  if (epic) void onDelete(epic, true);
                }}
              >
                Force delete ({inFlight.length} in flight)
              </Button>
            </div>
          )}

          <div className="flex justify-end gap-2">
            <Button variant="ghost" size="sm" onClick={() => onOpenChange(false)}>Cancel</Button>
            <Button
              variant="destructive"
              size="sm"
              data-testid="confirm-delete-epic"
              disabled={!epic}
              onClick={() => {
                if (epic) void onDelete(epic, false);
              }}
            >
              Delete epic
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
