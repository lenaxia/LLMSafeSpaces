// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../components/ui/Button";
import { Input } from "../components/ui/Input";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { TagInput } from "../components/ui/TagInput";
import { BoardErrorNotice } from "../components/board/BoardErrorNotice";
import { TaskCard } from "../components/board/TaskCard";
import { TaskDialog } from "../components/board/TaskDialog";
import { EpicDrawer } from "../components/board/EpicDrawer";
import { KanbanSquare, ChevronRight } from "lucide-react";
import { cn } from "../lib/utils";
import { boardApi, type BoardEpic, type BoardTask, type BoardEventPayload } from "../api/board";
import { subscribeBoardEvents } from "../lib/boardFeed";
import { workspacesApi } from "../api/workspaces";
import { ApiClientError } from "../api/client";
import {
  MAIN_STAGES,
  SIDE_STAGES,
  STAGE_LABELS,
  isManagerDragAllowed,
  doneReadiness,
  describeBoardError,
  suggestBoardItems,
  validateBoardTags,
  type BoardStage,
  type DescribedBoardError,
} from "../lib/boardRules";

/** The reconciler's default sweep cadence doubles as the board's periodic
 * reconciliation belt (§4.6 ii: "periodically reconciles … so a
 * silently-dead stream never leaves a stale board") — the version-stamp
 * check's stand-in until a board-read version seam exists. */
const BOARD_REFETCH_MS = 30_000;

function toBoardError(err: unknown): DescribedBoardError {
  if (err instanceof ApiClientError) return describeBoardError(err);
  return { message: err instanceof Error ? err.message : "Board request failed", suggestions: [] };
}

interface OptimisticDrag {
  shortId: string;
  fromStage: string;
  toStage: string;
}

/** In-flight optimistic moves keyed by short id — concurrent drags are
 * independent slots (r1: the single-slot shape made a second drag
 * visually snap the first back regardless of its server outcome). */
type OptimisticMap = Record<string, OptimisticDrag>;

export function BoardPage() {
  const queryClient = useQueryClient();

  // --- Scope state (§4.8: swimlane focus + epic chips + search) ----------------
  const [focusProject, setFocusProject] = useState<string>("all");
  const [focusEpic, setFocusEpic] = useState<string>("all");
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState<string | null>(null);
  const [sideCollapsed, setSideCollapsed] = useState(true);

  // --- Data ---------------------------------------------------------------------
  const projectsQuery = useQuery({
    queryKey: ["board", "projects"],
    queryFn: () => boardApi.listProjects(),
  });
  const projects = useMemo(() => projectsQuery.data ?? [], [projectsQuery.data]);

  const tasksQuery = useQuery({
    queryKey: ["board", "tasks", focusProject],
    queryFn: async () => {
      if (focusProject !== "all") {
        return boardApi.listProjectTasks(focusProject);
      }
      const lists = await Promise.all(projects.map((p) => boardApi.listProjectTasks(p.id)));
      return lists.flat();
    },
    enabled: projectsQuery.isSuccess && projects.length > 0,
    refetchInterval: BOARD_REFETCH_MS,
  });
  const tasks = useMemo(() => tasksQuery.data ?? [], [tasksQuery.data]);

  // Read-path failures are NOT emptiness (r2): a failed projects/tasks
  // fetch renders an error notice — the empty-board state is reserved
  // for a successful read of zero projects (the "no swallowed errors"
  // posture extended from action paths to read paths).
  const boardReadError = useMemo(() => {
    if (projectsQuery.isError) return toBoardError(projectsQuery.error);
    if (tasksQuery.isError) return toBoardError(tasksQuery.error);
    return null;
  }, [projectsQuery.isError, projectsQuery.error, tasksQuery.isError, tasksQuery.error]);

  // Epics are derived from task rows (display inheritance, R2) plus the
  // epics this session created/opened — the ListEpics REST seam does not
  // exist yet (#1699 stopped it on the engine gap); this bridge keeps the
  // epic chips + create-task picker honest over live data.
  const [knownEpics, setKnownEpics] = useState<BoardEpic[]>([]);
  const derivedEpics = useMemo(() => {
    const byId = new Map<string, BoardEpic>();
    for (const t of tasks) {
      if (!byId.has(t.epic_id)) {
        byId.set(t.epic_id, {
          id: t.epic_id,
          project_id: t.project_id,
          short_id: `epic-${t.epic_id.slice(0, 8)}`,
          title: t.epic_name,
          outcome: "",
          status: "active",
          tags: [],
          created_at: "",
          updated_at: "",
          progress_done: tasks.filter((x) => x.epic_id === t.epic_id && x.stage === "done").length,
          progress_total: tasks.filter((x) => x.epic_id === t.epic_id).length,
          progress_derived: tasks.some((x) => x.epic_id === t.epic_id),
        });
      }
    }
    for (const e of knownEpics) byId.set(e.id, e);
    return [...byId.values()];
  }, [tasks, knownEpics]);

  const scopedEpics = useMemo(
    () => derivedEpics.filter((e) => focusProject === "all" || e.project_id === focusProject),
    [derivedEpics, focusProject],
  );

  const visibleProjects = useMemo(
    () => (focusProject === "all" ? projects : projects.filter((p) => p.id === focusProject)),
    [projects, focusProject],
  );

  const allTags = useMemo(() => {
    const counts = new Map<string, number>();
    for (const t of tasks) for (const tag of t.tags) counts.set(tag, (counts.get(tag) ?? 0) + 1);
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 12).map(([tag]) => tag);
  }, [tasks]);

  // --- Filtering ------------------------------------------------------------------
  const searchMiss = useMemo(() => {
    if (!search.trim()) return null;
    const q = search.trim().toLowerCase();
    const hit = tasks.some(
      (t) => t.short_id.toLowerCase().includes(q) || t.title.toLowerCase().includes(q) || t.tags.some((tag) => tag.toLowerCase().includes(q)),
    );
    return hit ? null : q;
  }, [search, tasks]);

  const searchSuggestions = useMemo(
    () =>
      searchMiss
        ? suggestBoardItems(
            searchMiss,
            tasks.map((t) => ({ shortId: t.short_id, title: t.title })),
          ).map((s) => s.shortId)
        : [],
    [searchMiss, tasks],
  );

  const filteredTasks = useMemo(() => {
    const q = search.trim().toLowerCase();
    return tasks.filter((t) => {
      if (focusEpic !== "all" && t.epic_id !== focusEpic) return false;
      if (tagFilter && !t.tags.includes(tagFilter)) return false;
      if (q) {
        return (
          t.short_id.toLowerCase().includes(q) ||
          t.title.toLowerCase().includes(q) ||
          t.tags.some((tag) => tag.toLowerCase().includes(q))
        );
      }
      return true;
    });
  }, [tasks, focusEpic, tagFilter, search]);

  // --- Optimistic drag (§4.6: optimistic, server-arbitrated snap-back) -----------
  const [optimistic, setOptimistic] = useState<OptimisticMap>({});
  const [dragError, setDragError] = useState<DescribedBoardError | null>(null);
  const [refusedShortId, setRefusedShortId] = useState<string | null>(null);
  // The dragged card's CURRENT stage — drives the affordance mirror
  // (§4.8/§4.1): cells the manager-surface map does not enumerate render
  // as non-landing (no highlight), while the DROP itself still attempts
  // and renders the engine's honest 409 (the server is the arbiter).
  const [draggingStage, setDraggingStage] = useState<string | null>(null);
  // task_start in flight (per card).
  const [startingShortId, setStartingShortId] = useState<string | null>(null);
  // Page-level notices (r1: openEpicDrawer/task_start failures have no
  // dialog to render inside).
  const [pageError, setPageError] = useState<DescribedBoardError | null>(null);
  const [pageNotice, setPageNotice] = useState<string | null>(null);
  // Epic lifecycle failure — renders inside the drawer (r1).
  const [drawerError, setDrawerError] = useState<DescribedBoardError | null>(null);
  // §4.8 activity feed buffer: board.event payloads (display-only; the
  // snapshot refetch remains the authoritative update path).
  const [boardActivity, setBoardActivity] = useState<BoardEventPayload[]>([]);
  // The done-evidence prompt: dropping onto Done asks for the evidence
  // URL first (required on → done, R2); the server's done_guard still
  // arbitrates criteria.
  const [donePrompt, setDonePrompt] = useState<{ task: BoardTask } | null>(null);
  const [evidenceUrl, setEvidenceUrl] = useState("");

  const moveStageMutation = useMutation({
    mutationFn: ({ shortId, toStage, evidence }: { shortId: string; toStage: string; evidence?: string }) =>
      boardApi.moveStage(shortId, toStage, evidence),
  });

  const runMove = useCallback(
    async (task: BoardTask, toStage: string, evidence?: string) => {
      setDragError(null);
      setRefusedShortId(null);
      setOptimistic((prev) => ({ ...prev, [task.short_id]: { shortId: task.short_id, fromStage: task.stage, toStage } }));
      try {
        await moveStageMutation.mutateAsync({ shortId: task.short_id, toStage, evidence });
        // Authoritative row: invalidate the board read model (the SSE
        // board.event would do the same; the mutation path does not wait
        // for the stream).
        await queryClient.invalidateQueries({ queryKey: ["board"] });
      } catch (err) {
        setDragError(toBoardError(err));
        setRefusedShortId(task.short_id);
      } finally {
        setOptimistic((prev) => {
          const next = { ...prev };
          delete next[task.short_id];
          return next;
        });
      }
    },
    [moveStageMutation, queryClient],
  );

  const handleDrop = useCallback(
    (stage: string) => (e: React.DragEvent) => {
      e.preventDefault();
      const shortId = e.dataTransfer.getData("text/board-task");
      if (!shortId) return;
      const task = tasks.find((t) => t.short_id === shortId);
      if (!task || task.stage === stage) return;
      if (stage === "done") {
        // Collect the evidence URL first; criteria remain the server's
        // done_guard call (rendered verbatim when it refuses).
        setEvidenceUrl("");
        setDonePrompt({ task: { ...task, stage: task.stage } });
        return;
      }
      void runMove(task, stage);
    },
    [tasks, runMove],
  );

  const stageOf = useCallback(
    (t: BoardTask): BoardStage => {
      const opt = optimistic[t.short_id];
      if (opt) return opt.toStage as BoardStage;
      return t.stage as BoardStage;
    },
    [optimistic],
  );

  // --- Dialogs ---------------------------------------------------------------------
  const [showCreateProject, setShowCreateProject] = useState(false);

  const workspacesQuery = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
    // r3: fetch only once the projects read has SETTLED and the list is
    // genuinely needed — the empty-board prefetch or the New-project
    // dialog — never speculatively mid-load (the dialog must not depend
    // on the Sidebar's fetch having populated the shared cache), and its
    // failure must be a rendered error, not a silently empty select.
    enabled:
      projectsQuery.isSuccess &&
      (projects.length === 0 || showCreateProject),
  });

  const [projName, setProjName] = useState("");
  const [projWorkspace, setProjWorkspace] = useState("");
  const [projSlots, setProjSlots] = useState(2);
  const [projTags, setProjTags] = useState<string[]>([]);
  const [projTagErrors, setProjTagErrors] = useState<string[]>([]);
  const [createError, setCreateError] = useState<DescribedBoardError | null>(null);

  const [showCreateEpic, setShowCreateEpic] = useState(false);
  const [epicTitle, setEpicTitle] = useState("");
  const [epicOutcome, setEpicOutcome] = useState("");
  const [epicTags, setEpicTags] = useState<string[]>([]);

  const [taskDialog, setTaskDialog] = useState<{ mode: "create"; epic: BoardEpic } | { mode: "edit"; task: BoardTask } | null>(null);
  const [dialogError, setDialogError] = useState<DescribedBoardError | null>(null);
  const [epicDrawer, setEpicDrawer] = useState<BoardEpic | null>(null);

  const createProjectMutation = useMutation({
    mutationFn: () =>
      boardApi.createProject({ name: projName.trim(), targetWorkspaceId: projWorkspace, workerSlots: projSlots, tags: projTags }),
    onSuccess: () => {
      setShowCreateProject(false);
      setProjName("");
      setProjTags([]);
      setProjTagErrors([]);
      void queryClient.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (err) => setCreateError(toBoardError(err)),
  });

  const createEpicMutation = useMutation({
    mutationFn: (projectId: string) =>
      boardApi.createEpic(projectId, { title: epicTitle.trim(), outcome: epicOutcome, tags: epicTags }),
    onSuccess: (epic) => {
      setKnownEpics((prev) => [...prev.filter((e) => e.id !== epic.id), epic]);
      setShowCreateEpic(false);
      setEpicTitle("");
      setEpicOutcome("");
      setEpicTags([]);
      void queryClient.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (err) => setCreateError(toBoardError(err)),
  });

  const submitTask = useCallback(
    async (fields: { title: string; description: string; tags: string[]; acceptanceCriteria: Array<{ text: string; met: boolean }> }): Promise<boolean> => {
      setDialogError(null);
      try {
        if (taskDialog?.mode === "create") {
          await boardApi.createTask(taskDialog.epic.id, fields);
        } else if (taskDialog?.mode === "edit") {
          await boardApi.updateTask(taskDialog.task.short_id, fields);
        }
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        return true;
      } catch (err) {
        setDialogError(toBoardError(err));
        return false;
      }
    },
    [taskDialog, queryClient],
  );

  const lifecycleMutation = useMutation({
    mutationFn: async ({ epic, action }: { epic: BoardEpic; action: "activate" | "pause" }) =>
      action === "activate" ? boardApi.activateEpic(epic.short_id.startsWith("epc-") ? epic.short_id : epic.id) : boardApi.pauseEpic(epic.short_id.startsWith("epc-") ? epic.short_id : epic.id),
    onSuccess: (epic) => {
      setDrawerError(null);
      setKnownEpics((prev) => [...prev.filter((e) => e.id !== epic.id), epic]);
      setEpicDrawer(epic);
      void queryClient.invalidateQueries({ queryKey: ["board"] });
    },
    // r1: a failed activate/pause is a rendered notice, never a swallow.
    onError: (err) => setDrawerError(toBoardError(err)),
  });

  // task_start (§4.2): the manual one-off dispatch — the designed UI exit
  // for backlog rows. Renders the engine's Claim honestly (escalated =
  // the attempts cap withheld the grant).
  const startTaskMutation = useMutation({
    mutationFn: (task: BoardTask) => boardApi.startTask(task.short_id),
    onSuccess: async (claim, task) => {
      await queryClient.invalidateQueries({ queryKey: ["board"] });
      setPageNotice(
        claim.escalated
          ? `${task.short_id}: attempts cap reached — escalated to Pending Input (no grant).`
          : `${task.short_id}: fresh worker dispatched into the project's target workspace.`,
      );
    },
    onError: (err) => setPageError(toBoardError(err)),
    onSettled: (_d, _e, task) => setStartingShortId((cur) => (cur === task.short_id ? null : cur)),
  });

  // §4.8 activity feed: subscribe to the root stream's board.event fan-
  // out (boardFeed). Display-only — the ["board"] invalidation in the
  // same branch stays the authoritative update path.
  useEffect(() => {
    return subscribeBoardEvents((evt) => {
      setBoardActivity((prev) => [evt, ...prev].slice(0, 50));
    });
  }, []);

  // Open the epic drawer with the authoritative row (derived progress).
  // Failures render at PAGE level (r1: they previously landed in
  // dialogError, invisible with no TaskDialog open).
  const openEpicDrawer = useCallback(
    async (epicId: string) => {
      try {
        const epic = await boardApi.getEpic(epicId);
        setKnownEpics((prev) => [...prev.filter((e) => e.id !== epic.id), epic]);
        setDrawerError(null);
        setEpicDrawer(epic);
      } catch (err) {
        setPageError(toBoardError(err));
      }
    },
    [],
  );

  const taskDialogTask = taskDialog?.mode === "edit" ? taskDialog.task : null;
  const taskDialogEpic = taskDialog?.mode === "create" ? taskDialog.epic : null;

  if (projectsQuery.isLoading) {
    return (
      <div className="flex h-full items-center justify-center" data-testid="board-loading">
        <Spinner size="lg" />
      </div>
    );
  }

  const slotsInUse = (projectId: string) =>
    tasks.filter((t) => t.project_id === projectId && t.stage === "in_progress").length;

  return (
    <div className="flex h-full flex-col overflow-hidden" data-page="board">
      {/* --- Scope switcher (project focus + epic chips + search) --------------- */}
      <div className="flex flex-col gap-2 border-b border-border px-4 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <KanbanSquare className="h-4 w-4 text-muted-foreground" aria-hidden />
          <h1 className="text-sm font-semibold">Board</h1>
          <select
            aria-label="Project focus"
            value={focusProject}
            onChange={(e) => {
              setFocusProject(e.target.value);
              setFocusEpic("all");
            }}
            className="h-8 rounded-md border border-border bg-background px-2 text-sm"
            data-testid="project-focus"
          >
            <option value="all">All projects</option>
            {projects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name} ({p.short_id}){p.status === "archived" ? " — archived" : ""}
              </option>
            ))}
          </select>
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search short id, title, tag…"
            className="h-8 w-56"
            aria-label="Search tasks"
            data-testid="board-search"
          />
          <div className="ml-auto flex items-center gap-2">
            <Button variant="outline" size="sm" onClick={() => { setShowCreateEpic(true); setCreateError(null); }} disabled={focusProject === "all" || projects.length === 0} data-testid="new-epic-button">
              New epic
            </Button>
            <Button variant="outline" size="sm" onClick={() => { setShowCreateProject(true); setCreateError(null); }} data-testid="new-project-button">
              New project
            </Button>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <button
            onClick={() => setFocusEpic("all")}
            className={cn(
              "rounded-full px-2 py-0.5 text-[11px] transition-colors",
              focusEpic === "all" ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground hover:bg-accent",
            )}
            data-testid="epic-chip-all"
          >
            All epics
          </button>
          {scopedEpics.map((e) => (
            <button
              key={e.id}
              onClick={() => (focusEpic === e.id ? setFocusEpic("all") : setFocusEpic(e.id))}
              onDoubleClick={() => void openEpicDrawer(e.id)}
              title={`${e.title} — double-click for the epic drawer`}
              className={cn(
                "rounded-full px-2 py-0.5 text-[11px] transition-colors",
                focusEpic === e.id ? "bg-primary/10 text-primary" : "bg-muted text-muted-foreground hover:bg-accent",
              )}
              data-testid={`epic-chip-${e.short_id}`}
            >
              {e.title}
            </button>
          ))}
          {allTags.length > 0 && <span className="mx-1 text-border">|</span>}
          {allTags.map((tag) => (
            <button
              key={tag}
              onClick={() => setTagFilter(tagFilter === tag ? null : tag)}
              className={cn(
                "rounded px-1.5 py-0.5 font-mono text-[10px] transition-colors",
                tagFilter === tag ? "bg-primary/15 text-primary" : "bg-muted text-muted-foreground hover:bg-accent",
              )}
              data-testid={`tag-filter-${tag}`}
            >
              {tag}
            </button>
          ))}
        </div>
        {(searchMiss || dragError || pageError || pageNotice) && (
          <div className="flex flex-col gap-1">
            {dragError && <BoardErrorNotice error={dragError} title="Move refused — card snapped back" onDismiss={() => setDragError(null)} />}
            {pageError && <BoardErrorNotice error={pageError} onDismiss={() => setPageError(null)} />}
            {pageNotice && (
              <div className="flex items-center justify-between rounded-md border border-border bg-muted/40 px-3 py-1.5 text-xs" data-testid="board-notice">
                <span>{pageNotice}</span>
                <button type="button" onClick={() => setPageNotice(null)} aria-label="Dismiss notice" className="text-muted-foreground hover:text-foreground">×</button>
              </div>
            )}
            {searchMiss && (
              <div className="rounded-md border border-border bg-muted/40 px-3 py-1.5 text-xs" data-testid="board-search-miss">
                <span>No match for “{search}”.</span>
                {searchSuggestions.length > 0 && (
                  <span className="ml-1">
                    Did you mean:{" "}
                    {searchSuggestions.map((s) => (
                      <button
                        key={s}
                        type="button"
                        onClick={() => setSearch(s)}
                        className="mr-1 rounded bg-accent px-1.5 py-0.5 font-mono text-[10px]"
                      >
                        {s}
                      </button>
                    ))}
                  </span>
                )}
              </div>
            )}
          </div>
        )}
      </div>

      {/* --- Kanban: swimlanes = projects, columns = stages ---------------------- */}
      <div className="flex-1 overflow-auto p-3" data-testid="board-grid">
        {boardReadError ? (
          <div className="flex h-full items-center justify-center p-4" data-testid="board-read-error">
            <div className="w-full max-w-md">
              <BoardErrorNotice
                error={boardReadError}
                title="Board failed to load"
                onDismiss={undefined}
              />
              <div className="mt-2 flex justify-end">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => void queryClient.invalidateQueries({ queryKey: ["board"] })}
                  data-testid="board-retry"
                >
                  Retry
                </Button>
              </div>
            </div>
          </div>
        ) : projectsQuery.isSuccess && projects.length === 0 ? (
          <div className="flex h-full flex-col items-center justify-center gap-2 text-sm text-muted-foreground" data-testid="board-empty">
            <span>No boards yet.</span>
            <Button size="sm" onClick={() => setShowCreateProject(true)}>Create your first project</Button>
          </div>
        ) : null}
        <div className="flex flex-col gap-4">
          {visibleProjects.map((project) => {
            const projectTasks = filteredTasks.filter((t) => t.project_id === project.id);
            return (
              <section key={project.id} data-swimlane={project.short_id} data-testid={`swimlane-${project.short_id}`} className="rounded-lg border border-border">
                <header className="flex items-center gap-2 border-b border-border px-3 py-1.5">
                  <span className="text-xs font-semibold">{project.name}</span>
                  <span className="font-mono text-[10px] text-muted-foreground">{project.short_id}</span>
                  <span className="text-[10px] text-muted-foreground" data-testid={`slots-${project.short_id}`}>
                    {slotsInUse(project.id)}/{project.worker_slots} slots
                  </span>
                  {project.status === "archived" && <Badge variant="muted" className="text-[10px]">archived</Badge>}
                  {project.slot_cooldown_until && new Date(project.slot_cooldown_until) > new Date() && (
                    <Badge variant="warning" className="text-[10px]" title="Provider throttle: new claims paused">
                      slot cooldown
                    </Badge>
                  )}
                </header>
                <div className="grid grid-cols-4 gap-2 p-2">
                  {MAIN_STAGES.map((stage) => (
                    <StageCell
                      key={stage}
                      stage={stage}
                      projectTasks={projectTasks}
                      allProjectTasks={tasks.filter((t) => t.project_id === project.id)}
                      stageOf={stageOf}
                      optimistic={optimistic}
                      refusedShortId={refusedShortId}
                      draggingStage={draggingStage}
                      onDrop={handleDrop(stage)}
                      onOpenCard={(t) => setTaskDialog({ mode: "edit", task: t })}
                      onCardDragStart={(t) => setDraggingStage(t.stage)}
                      onCardDragEnd={() => setDraggingStage(null)}
                      onCardStart={(t) => {
                        setStartingShortId(t.short_id);
                        startTaskMutation.mutate(t);
                      }}
                      startingShortId={startingShortId}
                    />
                  ))}
                </div>
                {/* Side columns: Pending Input + Cancelled (collapsible, §4.8) */}
                <div className="border-t border-border px-2 py-1.5">
                  <button
                    onClick={() => setSideCollapsed((v) => !v)}
                    className="flex items-center gap-1 text-[11px] text-muted-foreground"
                    aria-expanded={!sideCollapsed}
                    data-testid="side-columns-toggle"
                  >
                    <ChevronRight className={cn("h-3 w-3 transition-transform", !sideCollapsed && "rotate-90")} />
                    {SIDE_STAGES.map((s) => `${STAGE_LABELS[s]} ${projectTasks.filter((t) => stageOf(t) === s).length}`).join(" · ")}
                  </button>
                  {!sideCollapsed && (
                    <div className="mt-1.5 grid grid-cols-2 gap-2">
                      {SIDE_STAGES.map((stage) => (
                        <StageCell
                          key={stage}
                          stage={stage}
                          projectTasks={projectTasks}
                          allProjectTasks={tasks.filter((t) => t.project_id === project.id)}
                          stageOf={stageOf}
                          optimistic={optimistic}
                          refusedShortId={refusedShortId}
                          draggingStage={draggingStage}
                          onDrop={handleDrop(stage)}
                          onOpenCard={(t) => setTaskDialog({ mode: "edit", task: t })}
                          onCardDragStart={(t) => setDraggingStage(t.stage)}
                          onCardDragEnd={() => setDraggingStage(null)}
                          onCardStart={(t) => {
                            setStartingShortId(t.short_id);
                            startTaskMutation.mutate(t);
                          }}
                          startingShortId={startingShortId}
                        />
                      ))}
                    </div>
                  )}
                </div>
              </section>
            );
          })}
        </div>
      </div>

      {/* --- New project dialog ------------------------------------------------- */}
      <Dialog.Root open={showCreateProject} onOpenChange={setShowCreateProject}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl">
            <Dialog.Title className="text-sm font-semibold">New project</Dialog.Title>
            {createError && <BoardErrorNotice error={createError} onDismiss={() => setCreateError(null)} />}
            {workspacesQuery.isError && (
              <div data-testid="workspaces-load-error">
                <BoardErrorNotice error={toBoardError(workspacesQuery.error)} title="Target workspaces failed to load" />
                <Button
                  variant="outline"
                  size="sm"
                  className="mt-1"
                  data-testid="workspaces-retry"
                  onClick={() => void queryClient.invalidateQueries({ queryKey: ["workspaces"] })}
                >
                  Retry workspaces
                </Button>
              </div>
            )}
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Name</span>
              <Input value={projName} onChange={(e) => setProjName(e.target.value)} data-testid="project-name" />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Target workspace <span className="text-muted-foreground">(one per project, R8)</span></span>
              <select
                value={projWorkspace}
                onChange={(e) => setProjWorkspace(e.target.value)}
                className="h-9 rounded-md border border-input bg-background px-2 text-sm"
                data-testid="project-workspace"
              >
                <option value="">Select workspace…</option>
                {(workspacesQuery.data?.items ?? []).map((ws) => (
                  <option key={ws.id} value={ws.id}>{ws.name}</option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Worker slots</span>
              <Input
                type="number"
                min={1}
                value={projSlots}
                onChange={(e) => setProjSlots(Number(e.target.value) || 1)}
                data-testid="project-slots"
              />
            </label>
            <div className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Tags</span>
              <TagInput
                value={projTags}
                onChange={(next) => {
                  setProjTags(next);
                  setProjTagErrors(validateBoardTags(next).map((e) => `${e.tag}: ${e.reason}`));
                }}
                id="project-tags"
              />
              {projTagErrors.length > 0 && (
                <div role="alert" data-testid="project-tag-errors">
                  {projTagErrors.map((e) => (
                    <span key={e} className="block text-[10px] text-destructive">{e}</span>
                  ))}
                </div>
              )}
            </div>
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setShowCreateProject(false)}>Cancel</Button>
              <Button
                size="sm"
                disabled={!projName.trim() || !projWorkspace || projTagErrors.length > 0 || createProjectMutation.isPending}
                onClick={() => createProjectMutation.mutate()}
                data-testid="project-create"
              >
                Create
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      {/* --- New epic dialog ------------------------------------------------------ */}
      <Dialog.Root open={showCreateEpic} onOpenChange={setShowCreateEpic}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl">
            <Dialog.Title className="text-sm font-semibold">
              New epic in {projects.find((p) => p.id === focusProject)?.name}
            </Dialog.Title>
            {createError && <BoardErrorNotice error={createError} onDismiss={() => setCreateError(null)} />}
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Title</span>
              <Input value={epicTitle} onChange={(e) => setEpicTitle(e.target.value)} data-testid="epic-title" />
            </label>
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Outcome <span className="text-muted-foreground">(the steering wheel)</span></span>
              <Input value={epicOutcome} onChange={(e) => setEpicOutcome(e.target.value)} data-testid="epic-outcome" />
            </label>
            <div className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Tags</span>
              <TagInput value={epicTags} onChange={setEpicTags} id="epic-tags" />
            </div>
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setShowCreateEpic(false)}>Cancel</Button>
              <Button
                size="sm"
                disabled={!epicTitle.trim() || createEpicMutation.isPending}
                onClick={() => focusProject !== "all" && createEpicMutation.mutate(focusProject)}
                data-testid="epic-create"
              >
                Create
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      {/* --- Task create/edit ------------------------------------------------------ */}
      <TaskDialog
        open={taskDialog !== null}
        onOpenChange={(open) => {
          if (!open) {
            setTaskDialog(null);
            setDialogError(null);
          }
        }}
        epic={taskDialogEpic}
        task={taskDialogTask}
        projectTasks={taskDialogTask ? tasks.filter((t) => t.project_id === taskDialogTask.project_id) : []}
        onSubmit={submitTask}
        error={dialogError}
        onDismissError={() => setDialogError(null)}
        onAddDependency={
          taskDialogTask
            ? async (dependsOn): Promise<boolean> => {
                setDialogError(null);
                try {
                  await boardApi.addDependency(taskDialogTask.short_id, dependsOn);
                  await queryClient.invalidateQueries({ queryKey: ["board"] });
                  return true;
                } catch (err) {
                  setDialogError(toBoardError(err));
                  return false;
                }
              }
            : undefined
        }
        onRemoveDependency={
          taskDialogTask
            ? async (dep) => {
                setDialogError(null);
                try {
                  await boardApi.removeDependency(taskDialogTask.short_id, dep);
                  await queryClient.invalidateQueries({ queryKey: ["board"] });
                } catch (err) {
                  setDialogError(toBoardError(err));
                }
              }
            : undefined
        }
        onDelete={
          taskDialogTask
            ? async (): Promise<boolean> => {
                setDialogError(null);
                try {
                  await boardApi.deleteTask(taskDialogTask.short_id);
                  await queryClient.invalidateQueries({ queryKey: ["board"] });
                  return true;
                } catch (err) {
                  setDialogError(toBoardError(err));
                  return false;
                }
              }
            : undefined
        }
      />

      {/* --- Epic drawer ------------------------------------------------------------ */}
      <EpicDrawer
        open={epicDrawer !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEpicDrawer(null);
            setDrawerError(null);
          }
        }}
        epic={epicDrawer}
        project={epicDrawer ? projects.find((p) => p.id === epicDrawer.project_id) ?? null : null}
        slotsInUse={epicDrawer ? slotsInUse(epicDrawer.project_id) : 0}
        busy={lifecycleMutation.isPending}
        error={drawerError}
        onDismissError={() => setDrawerError(null)}
        // §4.8: the SSE activity feed filtered to this epic's tasks
        // (item_id is the task UUID the event carried).
        activity={
          epicDrawer
            ? boardActivity.filter((evt) =>
                evt.item_kind === "task" &&
                tasks.some((t) => t.epic_id === epicDrawer.id && (t.id === evt.item_id || t.short_id === evt.payload?.short_id)))
            : []
        }
        onActivate={epicDrawer ? () => lifecycleMutation.mutate({ epic: epicDrawer, action: "activate" }) : undefined}
        onPause={epicDrawer ? () => lifecycleMutation.mutate({ epic: epicDrawer, action: "pause" }) : undefined}
        onNewTask={
          epicDrawer
            ? () => {
                setTaskDialog({ mode: "create", epic: epicDrawer });
                setEpicDrawer(null);
              }
            : undefined
        }
      />

      {/* --- Done-evidence prompt ---------------------------------------------------- */}
      <Dialog.Root open={donePrompt !== null} onOpenChange={(open) => { if (!open) setDonePrompt(null); }}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl">
            <Dialog.Title className="text-sm font-semibold">
              Move <span className="font-mono text-xs">{donePrompt?.task.short_id}</span> to Done
            </Dialog.Title>
            {donePrompt && <DoneReadinessView task={donePrompt.task} />}
            <label className="flex flex-col gap-1 text-xs">
              <span className="font-medium">Evidence URL <span className="text-muted-foreground">(required on → done, R2)</span></span>
              <Input
                value={evidenceUrl}
                onChange={(e) => setEvidenceUrl(e.target.value)}
                placeholder="https://github.com/…/pull/123"
                data-testid="evidence-url"
              />
            </label>
            <div className="flex justify-end gap-2">
              <Button variant="ghost" size="sm" onClick={() => setDonePrompt(null)}>Cancel</Button>
              <Button
                size="sm"
                disabled={!evidenceUrl.trim()}
                onClick={() => {
                  if (!donePrompt) return;
                  const t = donePrompt.task;
                  setDonePrompt(null);
                  void runMove(t, "done", evidenceUrl.trim());
                }}
                data-testid="confirm-done"
              >
                Move to Done
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </div>
  );
}

function DoneReadinessView({ task }: { task: BoardTask }) {
  const readiness = doneReadiness(task);
  return (
    <div className="flex flex-col gap-1 rounded-md bg-muted/50 p-2 text-xs" data-testid="done-readiness-view">
      {task.acceptance_criteria.length === 0 ? (
        <span className="text-muted-foreground">No acceptance criteria — Done needs only the evidence URL.</span>
      ) : readiness.allMet ? (
        <span className="text-green-600 dark:text-green-400">All {task.acceptance_criteria.length} criteria met.</span>
      ) : (
        <>
          <span className="text-destructive">
            {readiness.unmet.length} of {task.acceptance_criteria.length} criteria unmet — the server's Done guard will refuse this move (code: done_guard):
          </span>
          <ul className="ml-3 list-disc">
            {readiness.unmet.map((u) => (
              <li key={u}>{u}</li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

interface StageCellProps {
  stage: BoardStage;
  projectTasks: BoardTask[];
  allProjectTasks: BoardTask[];
  stageOf: (t: BoardTask) => BoardStage;
  optimistic: OptimisticMap;
  refusedShortId: string | null;
  /** The in-flight drag's source stage — gates the affordance highlight
   * (the manager-surface mirror); the drop still attempts (the engine
   * arbitrates and its 409 renders). */
  draggingStage: string | null;
  onDrop: (e: React.DragEvent) => void;
  onOpenCard: (t: BoardTask) => void;
  onCardDragStart: (t: BoardTask) => void;
  onCardDragEnd: () => void;
  onCardStart: (t: BoardTask) => void;
  startingShortId: string | null;
}

function StageCell({
  stage,
  projectTasks,
  allProjectTasks,
  stageOf,
  optimistic,
  refusedShortId,
  draggingStage,
  onDrop,
  onOpenCard,
  onCardDragStart,
  onCardDragEnd,
  onCardStart,
  startingShortId,
}: StageCellProps) {
  const [dragOver, setDragOver] = useState(false);
  const cards = projectTasks.filter((t) => stageOf(t) === stage);
  // §4.8 affordance mirror (r1): cells the manager-surface map does not
  // enumerate for the dragged source render as non-landing — no primary
  // highlight, struck-through header — while the drop STILL attempts so
  // the engine's guard outcome (409 + code) is what the user sees.
  const landsHere = draggingStage === null || isManagerDragAllowed(draggingStage, stage);
  return (
    <div
      data-stage-drop={stage}
      data-testid={`stage-drop-${stage}`}
      data-droppable={landsHere ? "true" : "false"}
      onDragOver={(e) => {
        e.preventDefault();
        e.dataTransfer.dropEffect = landsHere ? "move" : "copy";
        setDragOver(true);
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={(e) => {
        setDragOver(false);
        onDrop(e);
      }}
      className={cn(
        "flex min-h-[6rem] flex-col gap-1.5 rounded-md bg-muted/30 p-1.5",
        dragOver && landsHere && "outline-2 outline-dashed outline-primary/50",
        dragOver && !landsHere && "outline-2 outline-dashed outline-muted-foreground/30",
      )}
      aria-label={`${STAGE_LABELS[stage]} column`}
    >
      <div className="flex items-center justify-between px-0.5">
        <span className={cn("text-[10px] font-medium uppercase tracking-wide text-muted-foreground", draggingStage !== null && !landsHere && "line-through opacity-60")}>
          {STAGE_LABELS[stage]}
        </span>
        <span className="text-[10px] text-muted-foreground">{cards.length}</span>
      </div>
      {cards.map((t) => (
        <TaskCard
          key={t.short_id}
          task={t}
          projectTasks={allProjectTasks}
          pending={optimistic[t.short_id] !== undefined}
          refused={refusedShortId === t.short_id}
          starting={startingShortId === t.short_id}
          onStart={onCardStart}
          onOpen={onOpenCard}
          onDragStart={onCardDragStart}
          onDragEnd={onCardDragEnd}
        />
      ))}
      {cards.length === 0 && <span className="px-1 text-[10px] text-muted-foreground/60">—</span>}
    </div>
  );
}
