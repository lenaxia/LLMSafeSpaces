// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import * as Dialog from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { Button } from "../ui/Button";
import { StoryCard } from "./StoryCard";
import type { BoardProject, BoardTask } from "../../api/board";
import { sortDoneNewestFirst } from "../../lib/boardRules";

interface DoneDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The lane whose Done column opened the drawer (null = closed). */
  project: BoardProject | null;
  /** The lane's done stories under the column's own filter context —
   * the drawer is the column's overflow surface, so its universe is
   * exactly what the column would render (uncapped). */
  tasks: BoardTask[];
  /** Same-project rows for the cards' blocked-by chips (deps are
   * within-project by construction, ruling R4). */
  allProjectTasks: BoardTask[];
  /** The project's target workspace id — the cards' assignee session
   * links' chat-route segment (2026-10-29 owner ruling). */
  workspaceId?: string;
  /** The swimlane's selected epic — cards highlight/dim as on the
   * board (display-only scope, kept coherent with the column). */
  highlightEpicId?: string | null;
  onOpenCard: (t: BoardTask) => void;
  onCardDetails: (t: BoardTask) => void;
  /** #1832 item 3: opens the task delete modal (card kebab). */
  onCardDelete: (t: BoardTask) => void;
}

/**
 * DoneDrawer — the Done column's overflow surface (owner ask
 * 2026-11-13): the column caps its inline cards at
 * DONE_COLUMN_MAX_VISIBLE newest-done stories; this drawer lists the
 * FULL set in its own scroll region (the page never grows with the
 * done count). The StoryDrawer shape (Radix right-side drawer): focus
 * trap + Escape + focus return come from the existing dialog stack —
 * no new overlay system. Rows are the board's own StoryCards, so every
 * column affordance (edit, details, delete) works in place.
 */
export function DoneDrawer({
  open,
  onOpenChange,
  project,
  tasks,
  allProjectTasks,
  workspaceId,
  highlightEpicId = null,
  onOpenCard,
  onCardDetails,
  onCardDelete,
}: DoneDrawerProps) {
  if (!project) return null;
  const ordered = sortDoneNewestFirst(tasks);
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed right-0 top-0 z-50 flex h-full w-[26rem] max-w-[90vw] flex-col border-l border-border bg-background shadow-xl"
          aria-label={`Done stories in ${project.name}`}
        >
          <div className="flex items-start justify-between gap-2 border-b border-border px-4 py-3">
            <Dialog.Title className="flex flex-col gap-0.5 text-sm font-semibold" data-testid="done-drawer-title">
              Done — {project.name}
              <span className="text-[10px] font-normal text-muted-foreground" data-testid="done-drawer-count">
                {ordered.length} {ordered.length === 1 ? "story" : "stories"}, newest first
              </span>
            </Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close" data-testid="done-drawer-close">
                <X className="h-4 w-4" />
              </Button>
            </Dialog.Close>
          </div>
          {/* The full set scrolls INSIDE this region — the drawer is
           * the column's overflow surface, so hundreds of done stories
           * scroll here instead of pushing the board layout. */}
          <div className="flex flex-1 flex-col gap-1.5 overflow-y-auto px-3 py-3" data-testid="done-drawer-list">
            {ordered.map((t) => (
              <StoryCard
                key={t.short_id}
                task={t}
                projectTasks={allProjectTasks}
                workspaceId={workspaceId}
                dimmed={highlightEpicId !== null && t.epic_id !== highlightEpicId}
                highlighted={highlightEpicId !== null && t.epic_id === highlightEpicId}
                onOpen={onOpenCard}
                onDetails={onCardDetails}
                kebabItems={[
                  {
                    label: "Delete story…",
                    destructive: true,
                    onClick: () => onCardDelete(t),
                  },
                ]}
              />
            ))}
            {ordered.length === 0 && (
              <span className="px-1 text-[10px] text-muted-foreground/60" data-testid="done-drawer-empty">
                No done stories —
              </span>
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
