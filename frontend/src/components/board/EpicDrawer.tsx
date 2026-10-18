// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../ui/Button";
import { Badge } from "../ui/Badge";
import { X, Play, Pause } from "lucide-react";
import type { BoardEpic, BoardProject } from "../../api/board";

interface EpicDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  epic: BoardEpic | null;
  project?: BoardProject | null;
  /** Slots in use = the project's current in_progress count (derived). */
  slotsInUse?: number;
  onActivate?: () => void;
  onPause?: () => void;
  onNewTask?: () => void;
  busy?: boolean;
}

/**
 * EpicDrawer — §4.8's epic focus: the outcome (the human's steering
 * wheel, R2) front and center, DERIVED progress (Done/(total−Cancelled)
 * — computed by board_read, never stored; a zero denominator renders
 * "—", never 0/0), activate/pause, and the worker budget (slots in use).
 */
export function EpicDrawer({ open, onOpenChange, epic, project, slotsInUse = 0, onActivate, onPause, onNewTask, busy }: EpicDrawerProps) {
  if (!epic) return null;
  const progress = epic.progress_derived
    ? `${epic.progress_done}/${epic.progress_total}`
    : "—";

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed right-0 top-0 z-50 flex h-full w-[22rem] max-w-[90vw] flex-col overflow-y-auto border-l border-border bg-background shadow-xl"
          aria-label={`Epic ${epic.short_id}`}
        >
          <div className="flex items-center justify-between border-b border-border px-4 py-3">
            <Dialog.Title className="flex items-center gap-2 text-sm font-semibold">
              Epic <span className="font-mono text-xs text-muted-foreground">{epic.short_id}</span>
              <Badge variant={epic.status === "active" ? "success" : "muted"} className="text-[10px]">{epic.status}</Badge>
            </Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close"><X className="h-4 w-4" /></Button>
            </Dialog.Close>
          </div>

          <div className="flex flex-1 flex-col gap-3 px-4 py-3 text-xs">
            <div>
              <span className="font-medium">{epic.title}</span>
              <span className="ml-2 text-muted-foreground">in {epic.project_name}</span>
            </div>

            <div className="rounded-md bg-muted/50 p-2" data-testid="epic-outcome">
              <span className="block text-[10px] uppercase tracking-wide text-muted-foreground">Outcome</span>
              <span className="text-xs">{epic.outcome || "—"}</span>
            </div>

            <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5" data-testid="epic-progress">
              <span className="text-muted-foreground">Progress (derived)</span>
              <span className="font-mono">{progress}</span>
            </div>

            {project && (
              <div className="flex items-center justify-between rounded-md border border-border px-2 py-1.5" data-testid="epic-worker-budget">
                <span className="text-muted-foreground">Worker budget</span>
                <span className="font-mono">{slotsInUse}/{project.worker_slots} slots</span>
              </div>
            )}

            {epic.tags.length > 0 && (
              <div className="flex flex-wrap gap-1">
                {epic.tags.map((t) => (
                  <span key={t} className="rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">{t}</span>
                ))}
              </div>
            )}
          </div>

          <div className="flex flex-wrap justify-end gap-2 border-t border-border px-4 py-3">
            {onNewTask && (
              <Button variant="outline" size="sm" onClick={onNewTask}>New task</Button>
            )}
            {epic.status !== "active" ? (
              onActivate && (
                <Button size="sm" onClick={onActivate} disabled={busy} data-testid="epic-activate">
                  <Play className="mr-1 h-3.5 w-3.5" /> Activate
                </Button>
              )
            ) : (
              onPause && (
                <Button variant="outline" size="sm" onClick={onPause} disabled={busy} data-testid="epic-pause">
                  <Pause className="mr-1 h-3.5 w-3.5" /> Pause
                </Button>
              )
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
