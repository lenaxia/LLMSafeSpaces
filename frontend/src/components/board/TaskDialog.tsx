// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../ui/Button";
import { Input } from "../ui/Input";
import { TagInput } from "../ui/TagInput";
import { BoardErrorNotice } from "./BoardErrorNotice";
import { Plus, X, Check, Link2Off } from "lucide-react";
import type { BoardAcceptanceCriterion, BoardEpic, BoardTask } from "../../api/board";
import { validateBoardTags, doneReadiness, type DescribedBoardError } from "../../lib/boardRules";

interface TaskDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The epic a NEW task lands in; undefined ⇒ edit mode (task set). */
  epic?: BoardEpic | null;
  task?: BoardTask | null;
  /** Same-project rows for dependency picking (deps are within-project, R4). */
  projectTasks: BoardTask[];
  /** Resolves true on success (the dialog closes); false/throw on
   * failure (the dialog STAYS open — the parent has rendered the error
   * into `error`; this contract pins that behaviour and prevents the
   * unhandled-rejection smell of a bare rethrow). */
  onSubmit: (fields: {
    title: string;
    description: string;
    tags: string[];
    acceptanceCriteria: BoardAcceptanceCriterion[];
  }) => Promise<boolean>;
  /** Dependency writes are their own calls (409+suggestions shape).
   * Resolves true on success; a throw means the typed input is KEPT
   * (r1: the failed id is recoverable, not cleared). */
  onAddDependency?: (dependsOn: string) => Promise<boolean>;
  onRemoveDependency?: (dependsOnShortId: string) => Promise<void>;
  onDelete?: () => Promise<boolean>;
  /** Rendered inline when a submit/dependency call fails. */
  error?: DescribedBoardError | null;
  onDismissError?: () => void;
}

/**
 * TaskDialog — the create/edit surface (§4.8): title/description (the
 * description seeds the worker charter, R2), tags with the OR4
 * pr:N anchor validated client-side (^pr:[1-9][0-9]*$), acceptance
 * criteria with reviewer check-off (Done preparation — the engine's Done
 * guard consumes the flags; Done itself is only ever the server's call),
 * and the structural dependency editor (add/remove, the ONLY blocking
 * mechanism — R3/R4).
 */
export function TaskDialog({
  open,
  onOpenChange,
  epic,
  task,
  projectTasks,
  onSubmit,
  onAddDependency,
  onRemoveDependency,
  onDelete,
  error,
  onDismissError,
}: TaskDialogProps) {
  const editing = !!task;
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
    const [tags, setTags] = useState<string[]>([]);
  const [criteria, setCriteria] = useState<BoardAcceptanceCriterion[]>([]);
  const [newCriterion, setNewCriterion] = useState("");
  const [depInput, setDepInput] = useState("");
  const [tagErrors, setTagErrors] = useState<string[]>([]);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!open) return;
    setTitle(task?.title ?? "");
    setDescription(task?.description ?? "");
    setTags(task?.tags ?? []);
    setCriteria(task?.acceptance_criteria ? task.acceptance_criteria.map((c) => ({ ...c })) : []);
    setDepInput("");
    setNewCriterion("");
    setTagErrors([]);
  }, [open, task]);

  const readiness = doneReadiness({ acceptance_criteria: criteria });

  function handleTagsChange(next: string[]) {
    setTags(next);
    setTagErrors(validateBoardTags(next).map((e) => `${e.tag}: ${e.reason}`));
  }

  async function handleSubmit() {
    if (!title.trim() || tagErrors.length > 0) return;
    setSubmitting(true);
    try {
      const ok = await onSubmit({
        title: title.trim(),
        description,
        tags,
        acceptanceCriteria: criteria,
      });
      if (ok) onOpenChange(false);
    } catch {
      // The parent's error surface (the in-dialog notice) owns the
      // message; the dialog stays open with the fields intact.
    } finally {
      setSubmitting(false);
    }
  }

  const depCandidates = projectTasks.filter(
    (t) => t.short_id !== task?.short_id && !(task?.blocked_by ?? []).includes(t.short_id),
  );

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed left-1/2 top-1/2 z-50 flex max-h-[85vh] w-[32rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-md border border-border bg-background shadow-xl"
          aria-label={editing ? `Edit task ${task?.short_id}` : "New task"}
        >
          <div className="flex items-center justify-between border-b border-border px-4 py-3">
            <Dialog.Title className="text-sm font-semibold">
              {editing ? (
                <>
                  Task <span className="font-mono text-xs text-muted-foreground">{task?.short_id}</span>
                  {epic === null && task && <span className="ml-2 text-xs text-muted-foreground">{task.epic_name}</span>}
                </>
              ) : (
                <>New task {epic && <span className="ml-2 text-xs text-muted-foreground">in {epic.title}</span>}</>
              )}
            </Dialog.Title>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close">
                <X className="h-4 w-4" />
              </Button>
            </Dialog.Close>
          </div>

          <div className="flex flex-1 flex-col gap-3 overflow-y-auto px-4 py-3">
            {error && <BoardErrorNotice error={error} onDismiss={onDismissError} onSuggestionPick={(s) => setDepInput(s)} />}

            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Title</span>
              <Input value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Task title" data-testid="task-title" />
            </label>

            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Description <span className="text-muted-foreground">(markdown — seeds the worker charter)</span></span>
              <textarea
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                rows={3}
                className="w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                placeholder="What the fresh worker executes…"
                data-testid="task-description"
              />
            </label>

            <div className="flex gap-3">
              <div className="flex flex-1 flex-col gap-1 text-xs">
                <span className="font-medium">Tags</span>
                <TagInput value={tags} onChange={handleTagsChange} placeholder="pr:123, backend…" id="task-tags" />
                <span className="text-[10px] text-muted-foreground">
                  PR-drift anchors: explicit <code className="font-mono">pr:N</code> tag only (ruling OR4)
                </span>
                {tagErrors.length > 0 && (
                  <div className="flex flex-col gap-0.5" data-testid="tag-validation-errors" role="alert">
                    {tagErrors.map((e) => (
                      <span key={e} className="text-[10px] text-destructive">{e}</span>
                    ))}
                  </div>
                )}
              </div>
            </div>

            <div className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Acceptance criteria <span className="text-muted-foreground">(Done = ALL met + evidence, R2)</span></span>
              {criteria.length === 0 && <span className="text-[10px] text-muted-foreground">None — Done then needs only the evidence URL.</span>}
              {criteria.map((c, i) => (
                <div key={i} className="flex items-center gap-2">
                  <button
                    type="button"
                    role="checkbox"
                    aria-checked={c.met}
                    aria-label={`Criterion met: ${c.text}`}
                    data-testid={`criterion-${i}`}
                    onClick={() => setCriteria((prev) => prev.map((x, j) => (j === i ? { ...x, met: !x.met } : x)))}
                    className={c.met ? "text-green-600" : "text-muted-foreground"}
                  >
                    <Check className="h-4 w-4" />
                  </button>
                  <span className={c.met ? "text-xs line-through opacity-60" : "text-xs"}>{c.text}</span>
                  <button
                    type="button"
                    onClick={() => setCriteria((prev) => prev.filter((_, j) => j !== i))}
                    aria-label={`Remove criterion ${c.text}`}
                    className="ml-auto text-muted-foreground hover:text-destructive"
                  >
                    <X className="h-3.5 w-3.5" />
                  </button>
                </div>
              ))}
              <div className="flex gap-1">
                <Input
                  value={newCriterion}
                  onChange={(e) => setNewCriterion(e.target.value)}
                  placeholder="New criterion…"
                  className="h-8"
                  data-testid="new-criterion"
                />
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    const t = newCriterion.trim();
                    if (t) {
                      setCriteria((prev) => [...prev, { text: t, met: false }]);
                      setNewCriterion("");
                    }
                  }}
                  aria-label="Add criterion"
                >
                  <Plus className="h-3.5 w-3.5" />
                </Button>
              </div>
              {editing && !readiness.allMet && criteria.length > 0 && (
                <span className="text-[10px] text-muted-foreground" data-testid="done-readiness">
                  {readiness.unmet.length} unmet criteri{readiness.unmet.length === 1 ? "on" : "a"} — the Done guard will refuse until all are checked.
                </span>
              )}
            </div>

            {editing && task && (
              <div className="flex flex-col gap-1 text-xs">
                <span className="font-medium">Depends on <span className="text-muted-foreground">(structural — the only blocking mechanism, R3/R4)</span></span>
                {(task.blocked_by ?? []).length === 0 && <span className="text-[10px] text-muted-foreground">No dependencies.</span>}
                {(task.blocked_by ?? []).map((dep) => {
                  const depTask = projectTasks.find((t) => t.short_id === dep);
                  return (
                    <div key={dep} className="flex items-center justify-between gap-2 rounded bg-muted/50 px-2 py-1">
                      <span className="font-mono text-[10px]">{dep}</span>
                      <span className="truncate text-[10px] text-muted-foreground">{depTask?.title ?? ""}</span>
                      <button
                        type="button"
                        onClick={() => onRemoveDependency?.(dep)}
                        aria-label={`Remove dependency ${dep}`}
                        className="text-muted-foreground hover:text-destructive"
                        data-testid={`remove-dep-${dep}`}
                      >
                        <Link2Off className="h-3.5 w-3.5" />
                      </button>
                    </div>
                  );
                })}
                {onAddDependency && (
                  <div className="flex gap-1">
                    <Input
                      value={depInput}
                      onChange={(e) => setDepInput(e.target.value)}
                      placeholder="tsk-… (short id)"
                      className="h-8 font-mono"
                      data-testid="dep-input"
                    />
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={!depInput.trim()}
                      onClick={async () => {
                        const v = depInput.trim();
                        if (!v) return;
                        try {
                          const ok = await onAddDependency(v);
                          if (ok) setDepInput("");
                        } catch {
                          // keep the typed id — the notice shows why it failed
                        }
                      }}
                      data-testid="add-dependency"
                    >
                      <Plus className="h-3.5 w-3.5" /> Dep
                    </Button>
                  </div>
                )}
                {depCandidates.length > 0 && depInput.trim() === "" && (
                  <span className="text-[10px] text-muted-foreground">
                    e.g. {depCandidates.slice(0, 3).map((t) => t.short_id).join(", ")}
                  </span>
                )}
              </div>
            )}
          </div>

          <div className="flex items-center justify-between gap-2 border-t border-border px-4 py-3">
            {editing && onDelete ? (
              <Button
                variant="destructive"
                size="sm"
                data-testid="delete-task"
                title="Destructive: removes the task; the audit keeps the full snapshot (#1747)"
                onClick={async () => {
                  try {
                    const ok = await onDelete();
                    if (ok) onOpenChange(false);
                  } catch {
                    // stay open — the notice carries the guard outcome
                  }
                }}
              >
                Delete task
              </Button>
            ) : (
              <span />
            )}
            <div className="flex gap-2">
              <Button variant="ghost" size="sm" onClick={() => onOpenChange(false)}>Close</Button>
              <Button
                size="sm"
                onClick={handleSubmit}
                disabled={!title.trim() || tagErrors.length > 0 || submitting}
                data-testid="task-save"
              >
                {submitting ? "Saving…" : editing ? "Save" : "Create"}
              </Button>
            </div>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
