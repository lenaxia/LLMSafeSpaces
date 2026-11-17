// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueries, useMutation, useQueryClient } from "@tanstack/react-query";
import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../components/ui/Button";
import { Input } from "../components/ui/Input";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { TagInput } from "../components/ui/TagInput";
import { StoryDeleteDialog, describeStoryDeleteResult } from "../components/board/StoryDeleteDialog";
import { EpicDeleteDialog } from "../components/board/EpicDeleteDialog";
import { KebabMenu, type KebabMenuItem } from "../components/ui/KebabMenu";
import { BoardErrorNotice } from "../components/board/BoardErrorNotice";
import { StoryCard } from "../components/board/StoryCard";
import { StoryDialog } from "../components/board/StoryDialog";
import { StoryDrawer } from "../components/board/StoryDrawer";
import { DoneDrawer } from "../components/board/DoneDrawer";
import { BoardDefaultsDialog, ProjectConfigTrigger } from "../components/board/BoardConfigPanels";
import { EpicDrawer } from "../components/board/EpicDrawer";
import { BoardHelpDrawer } from "../components/board/BoardHelpDrawer";
import { Archive, HelpCircle, KanbanSquare, ChevronRight, ChevronsLeftRight, ChevronsRightLeft, ListFilter, Settings2 } from "lucide-react";
import { useSearchParams } from "react-router-dom";
import { cn } from "../lib/utils";
import { boardApi, type BoardEpic, type BoardProject, type BoardTask, type BoardEventPayload } from "../api/board";
import { subscribeBoardEvents } from "../lib/boardFeed";
import { workspacesApi } from "../api/workspaces";
import { ApiClientError } from "../api/client";
import {
  MAIN_STAGES,
  SIDE_STAGES,
  STAGE_BACKLOG,
  STAGE_DONE,
  STAGE_IN_PROGRESS,
  STAGE_IN_REVIEW,
  STAGE_LABELS,
  isManagerDragAllowed,
  stageDragKey,
  doneReadiness,
  sortDoneNewestFirst,
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

/** Kebab labels live in a w-40 portal menu — criterion text truncates
 * (the full text renders in the edit dialog and the details drawer;
 * the index keeps the item identifiable). */
function kebabTruncate(text: string, max = 26): string {
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

/** 2026-10-28 ruling 3: the visible-row cap for a project's epic list —
 * overflow scrolls inside the list's own region (never page growth). */
export const EPIC_LIST_MAX_VISIBLE = 4;

/** Owner ask 2026-11-13: the Done column's inline-render cap — beyond
 * this many done stories the column renders exactly this many (the
 * newest) plus the "Show all N done" affordance; the full set lives in
 * the DoneDrawer's own scroll region. 10 keeps the terminal column's
 * height in line with the other through-flow columns; the cap is
 * visual — nothing is dropped, and ≤N renders as before (no
 * affordance). */
export const DONE_COLUMN_MAX_VISIBLE = 10;

/** 2026-10-28 ruling 3: an epic's ACTIVITY clock — the newest of the
 * epic's own updated_at and its task rows' updated_at (both already in
 * the view layer). 0 = no parseable timestamp (quiet/derived rows) →
 * falls to the alpha tiebreak in the sort. */
function epicActivity(epic: BoardEpic, laneTasks: BoardTask[]): number {
  let latest = Date.parse(epic.updated_at ?? "") || 0;
  for (const t of laneTasks) {
    if (t.epic_id !== epic.id) continue;
    const at = Date.parse(t.updated_at ?? "") || 0;
    if (at > latest) latest = at;
  }
  return latest;
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

  // --- Scope state (§4.8: swimlane focus + search) ----------------
  // 2026-10-28 ruling 1: the epic pills are GONE from the toolbar —
  // epic association is discovered via the epic rows' click-highlight
  // (ruling 2), so there is no focusEpic state to keep.
  const [focusProject, setFocusProject] = useState<string>("all");
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState<string | null>(null);
  // #1813: the failed-state overlay filter (client-side, the tagFilter
  // pattern — the server ?failed= param serves programmatic callers).
  const [failedOnly, setFailedOnly] = useState(false);
  // Owner ruling 2026-10-29 ("those pills need to be in a collapsed
  // drawer"): the filter pills live INSIDE a collapsible drawer opened
  // by the toolbar's filter button — no pills render inline in the
  // toolbar. Closed = not rendered = zero height (mobile: the swim
  // lanes start immediately below the toolbar, nothing overlaps).
  const [filterDrawerOpen, setFilterDrawerOpen] = useState(false);
  // The "How the sprint board works" help drawer (owner ruling
  // 2026-10-30): opened from the toolbar's HelpCircle affordance AND
  // the empty-board first-run state — static prose, no data dependency.
  const [helpOpen, setHelpOpen] = useState(false);
  const [sideCollapsed, setSideCollapsed] = useState(true);
  // #1964: archived projects' two-level drawer — the #1627
  // Archived-group deliberate-friction posture: BOTH levels are
  // collapsed by default and open only on a human click; nothing
  // auto-expands them (not even an archive landing mid-session).
  const [archivedDrawerExpanded, setArchivedDrawerExpanded] = useState(false);
  // Board config ON the board (owner ruling 2026-10-28): the defaults
  // dialog's open state. The /settings/board-config redirect rides
  // ?config=defaults to auto-open it once (muscle memory lands on the
  // config, not on a bare board).
  const [searchParams, setSearchParams] = useSearchParams();
  const [defaultsOpen, setDefaultsOpen] = useState(searchParams.get("config") === "defaults");
  useEffect(() => {
    if (searchParams.get("config") === "defaults") {
      setSearchParams({}, { replace: true });
    }
  }, [searchParams, setSearchParams]);
  const [expandedArchivedProjects, setExpandedArchivedProjects] = useState<ReadonlySet<string>>(new Set());
  const toggleArchivedProject = useCallback((projectId: string) => {
    setExpandedArchivedProjects((prev) => {
      const next = new Set(prev);
      if (next.has(projectId)) next.delete(projectId);
      else next.add(projectId);
      return next;
    });
  }, []);

  // --- Data ---------------------------------------------------------------------
  const projectsQuery = useQuery({
    queryKey: ["board", "projects"],
    queryFn: () => boardApi.listProjects(),
  });
  const projects = useMemo(() => projectsQuery.data ?? [], [projectsQuery.data]);

  // #1964: the active/archived partition — archived projects are
  // HIDDEN from the board's active surface (swimlanes, focus
  // dropdown, epic/tag chips, search) and live only in the collapsed
  // Archived drawer at the board's end. History stays reachable there;
  // the archive semantics themselves (#1755) are untouched. The tasks/
  // epics fan-outs still fetch every lane: drawer content needs them.
  const activeProjects = useMemo(() => projects.filter((p) => p.status !== "archived"), [projects]);
  const archivedProjects = useMemo(() => projects.filter((p) => p.status === "archived"), [projects]);
  const activeProjectIds = useMemo(() => new Set(activeProjects.map((p) => p.id)), [activeProjects]);
  const archivedProjectIds = useMemo(() => new Set(archivedProjects.map((p) => p.id)), [archivedProjects]);

  // #1964 r2 (review finding): the drawer's expansion state is
  // click-owned but must never OUTLIVE its object — a project that
  // leaves the drawer (unarchived, here or cross-context) loses its
  // expanded row: re-archiving starts name-only again (criterion 3's
  // "until clicked" holds on EVERY entry, not just the first). The
  // id-set prune returns the same Set reference when nothing changed
  // (no re-render loop).
  useEffect(() => {
    setExpandedArchivedProjects((prev) => {
      if (prev.size === 0) return prev;
      const next = new Set([...prev].filter((id) => archivedProjectIds.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [archivedProjectIds]);

  // …and a drawer that EMPTIES collapses: a later archive (in-session
  // or riding a cross-context refetch) never lands pre-expanded —
  // nothing auto-expands the drawer, not even an archive arriving
  // mid-session (the file's founding posture, criterion 2).
  useEffect(() => {
    if (archivedProjects.length === 0) setArchivedDrawerExpanded(false);
  }, [archivedProjects.length]);

  // #1964 r1 (review finding 2): the focused lane can leave the
  // active surface without THIS session's hand — another tab's
  // archive rides the board.event ["board"] invalidation (or a
  // window-focus refetch), the lane drops out of activeProjects,
  // and a stale focus id would strand the board: visibleProjects
  // = [] while other active lanes exist, the scope select matching
  // no option, no hint, no recovery. Reset scope to All the moment
  // the focus id stops naming an active lane.
  useEffect(() => {
    if (focusProject !== "all" && !activeProjectIds.has(focusProject)) {
      setFocusProject("all");
    }
  }, [focusProject, activeProjectIds]);

  // #1964 r1 (review finding 1): the tasks read is ALWAYS the full
  // fan-out, never a focused single-lane fetch — the Archived drawer
  // renders any lane's history regardless of the scope switcher's
  // focus, and a focused read starved drawer lanes of their rows
  // (band headers rendered, every cell stayed empty). The epics
  // fan-out below already fetches every lane for the same reason;
  // focus stays a pure VIEW filter (visibleProjects), and a cached
  // full board makes switching focus instant (no refetch, no flash).
  const tasksQuery = useQuery({
    queryKey: ["board", "tasks"],
    queryFn: async () => {
      const lists = await Promise.all(projects.map((p) => boardApi.listProjectTasks(p.id)));
      return lists.flat();
    },
    enabled: projectsQuery.isSuccess && projects.length > 0,
    refetchInterval: BOARD_REFETCH_MS,
  });
  const tasks = useMemo(() => tasksQuery.data ?? [], [tasksQuery.data]);

  // #1832 r2 (review finding 1): the ListEpics REST seam —
  // GET /me/projects/:id/epics (router → ListProjectEpics →
  // pkg/board Service.ListEpics, #1699's engine read). The epic LIST
  // is the source of existence for bands and the create-task picker:
  // zero-task epics (created last session, or via the API) live here,
  // never in task rows.
  const epicsQueries = useQueries({
    queries: projects.map((p) => ({
      queryKey: ["board", "epics", p.id],
      queryFn: () => boardApi.listProjectEpics(p.id),
      enabled: projectsQuery.isSuccess && projects.length > 0,
      refetchInterval: BOARD_REFETCH_MS,
    })),
  });
  const serverEpicsByProject = useMemo(() => {
    const m = new Map<string, BoardEpic[]>();
    projects.forEach((p, i) => m.set(p.id, epicsQueries[i]?.data ?? []));
    return m;
  }, [projects, epicsQueries]);
  // When each lane's list last ARRIVED (0 = never) — the clock the
  // session-known bridge is gated against (below).
  const epicsArrivedAtByProject = useMemo(() => {
    const m = new Map<string, number>();
    projects.forEach((p, i) => {
      const at = epicsQueries[i]?.dataUpdatedAt ?? 0;
      if (at > 0) m.set(p.id, at);
    });
    return m;
  }, [projects, epicsQueries]);

  // Read-path failures are NOT emptiness (r2): a failed projects/tasks/
  // epics fetch renders an error notice — the empty-board state is
  // reserved for a successful read of zero projects (the "no swallowed
  // errors" posture extended from action paths to read paths).
  const boardReadError = useMemo(() => {
    if (projectsQuery.isError) return toBoardError(projectsQuery.error);
    if (tasksQuery.isError) return toBoardError(tasksQuery.error);
    const failedEpics = epicsQueries.find((q) => q.isError);
    if (failedEpics) return toBoardError(failedEpics.error);
    return null;
  }, [projectsQuery.isError, projectsQuery.error, tasksQuery.isError, tasksQuery.error, epicsQueries]);

  // #1832: epics per lane. Precedence, per id:
  //   1. the SERVER LIST, once it has arrived for the lane — the
  //      engine's truth for existence, progress, and metadata; a known
  //      copy stops applying the moment a list response (re)lands
  //      (whether it carries the epic or not — server deletion wins);
  //   2. task-row derivation — the mid-load fallback (an epic visible
  //      in freshly-fetched task rows renders before its list response
  //      lands) and the honest floor when the list lags the rows;
  //   3. this session's created/opened epics (knownEpics) — the
  //      POST→refetch bridge ONLY: each carries knownAt and applies
  //      until the lane's NEXT list arrival, never past it (r3: the
  //      create response's 0/0 must not shadow the refetched 0/1).
  type KnownEpic = BoardEpic & { knownAt: number };
  const [knownEpics, setKnownEpics] = useState<KnownEpic[]>([]);
  const rememberEpic = useCallback((epic: BoardEpic) => {
    setKnownEpics((prev) => [...prev.filter((e) => e.id !== epic.id), { ...epic, knownAt: Date.now() }]);
  }, []);
  const epicsByProject = useMemo(() => {
    const byProject = new Map<string, Map<string, BoardEpic>>();
    const laneOf = (projectId: string) => {
      let m = byProject.get(projectId);
      if (!m) {
        m = new Map();
        byProject.set(projectId, m);
      }
      return m;
    };
    const put = (epic: BoardEpic) => laneOf(epic.project_id).set(epic.id, epic);
    for (const t of tasks) {
      const lane = laneOf(t.project_id);
      if (!lane.has(t.epic_id)) {
        const rows = tasks.filter((x) => x.project_id === t.project_id && x.epic_id === t.epic_id);
        lane.set(t.epic_id, {
          id: t.epic_id,
          project_id: t.project_id,
          short_id: `epic-${t.epic_id.slice(0, 8)}`,
          title: t.epic_name,
          outcome: "",
          status: "active",
          tags: [],
          created_at: "",
          updated_at: "",
          progress_done: rows.filter((x) => x.stage === "done").length,
          progress_total: rows.length,
          progress_derived: true,
        });
      }
    }
    for (const [, epics] of serverEpicsByProject) for (const e of epics) put(e);
    for (const e of knownEpics) {
      // The bridge applies only while the lane's list has NOT arrived
      // since the epic was learned (server truth supersedes, r3).
      if (e.knownAt > (epicsArrivedAtByProject.get(e.project_id) ?? 0)) put(e);
    }
    return new Map([...byProject.entries()].map(([k, v]) => [k, [...v.values()]]));
  }, [tasks, serverEpicsByProject, knownEpics, epicsArrivedAtByProject]);

  // 2026-10-28 ruling 1: the flat epic list + active-scope filter that
  // fed the toolbar's epic pills are REMOVED with the pills — epics
  // render per-project as rows (ruling 2/3) and remain discoverable via
  // the click-highlight.

  const visibleProjects = useMemo(
    () => (focusProject === "all" ? activeProjects : activeProjects.filter((p) => p.id === focusProject)),
    [activeProjects, focusProject],
  );

  // #1964: the ACTIVE surface's derivations (tag chips, search hits)
  // draw from active lanes only — archived lanes are drawer content,
  // never toolbar state.
  const surfaceTasks = useMemo(
    () => tasks.filter((t) => activeProjectIds.has(t.project_id)),
    [tasks, activeProjectIds],
  );

  const allTags = useMemo(() => {
    const counts = new Map<string, number>();
    for (const t of surfaceTasks) for (const tag of t.tags) counts.set(tag, (counts.get(tag) ?? 0) + 1);
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 12).map(([tag]) => tag);
  }, [surfaceTasks]);

  // #1964 r1 (review minor): a live tag filter must keep its chip —
  // allTags draws from the ACTIVE surface only, so once the tag's
  // lane archives (this session or cross-context) the chip vanishes
  // from the toolbar while filteredTasks kept applying it: rows
  // silently hidden with no visible chip to clear. Drop the filter
  // with its chip.
  useEffect(() => {
    if (tagFilter !== null && !allTags.includes(tagFilter)) {
      setTagFilter(null);
    }
  }, [tagFilter, allTags]);

  // The filter drawer's keyboard exit: Escape collapses it (the funnel
  // button's toggle is the pointer path; both leave the filters applied).
  useEffect(() => {
    if (!filterDrawerOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setFilterDrawerOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [filterDrawerOpen]);

  // --- Filtering ------------------------------------------------------------------
  // #1964: search arbitrates over the ACTIVE surface (surfaceTasks) —
  // archived rows render only inside the collapsed drawer, so a term
  // matching history alone is an honest miss on the board.
  const searchMiss = useMemo(() => {
    if (!search.trim()) return null;
    const q = search.trim().toLowerCase();
    const hit = surfaceTasks.some(
      (t) => t.short_id.toLowerCase().includes(q) || t.title.toLowerCase().includes(q) || t.tags.some((tag) => tag.toLowerCase().includes(q)),
    );
    return hit ? null : q;
  }, [search, surfaceTasks]);

  // 2026-10-28 ruling 4: did-you-mean chips carry the TITLE (ids stay
  // out of the chrome); the suggestion fills the search with the title
  // — the same row the id would have found, addressed humanly.
  const searchSuggestions = useMemo(
    () =>
      searchMiss
        ? suggestBoardItems(
            searchMiss,
            surfaceTasks.map((t) => ({ shortId: t.short_id, title: t.title })),
          )
        : [],
    [searchMiss, surfaceTasks],
  );

  const filteredTasks = useMemo(() => {
    const q = search.trim().toLowerCase();
    return tasks.filter((t) => {
      if (tagFilter && !t.tags.includes(tagFilter)) return false;
      if (failedOnly && !t.last_attempt_failed) return false;
      if (q) {
        return (
          t.short_id.toLowerCase().includes(q) ||
          t.title.toLowerCase().includes(q) ||
          t.tags.some((tag) => tag.toLowerCase().includes(q))
        );
      }
      return true;
    });
  }, [tasks, tagFilter, failedOnly, search]);

  // Owner ruling 2026-10-29: active filters stay VISIBLE with the pills
  // folded away — the filter button's count badge carries the number of
  // live filters (the FAILED overlay + the tag facet; search and the
  // project focus are not drawer filters and never count). 0 = no badge.
  const activeFilterCount = (failedOnly ? 1 : 0) + (tagFilter !== null ? 1 : 0);

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
    async (task: BoardTask, toStage: string, evidence?: string, via?: () => Promise<unknown>) => {
      setDragError(null);
      setRefusedShortId(null);
      setOptimistic((prev) => ({ ...prev, [task.short_id]: { shortId: task.short_id, fromStage: task.stage, toStage } }));
      try {
        // `via` swaps the wire call while keeping the optimistic stage
        // placement + snap-back machinery (the kebab's Re-arm rides
        // POST /rearm, not move-stage — same visual outcome: failed →
        // backlog).
        await (via ? via() : moveStageMutation.mutateAsync({ shortId: task.short_id, toStage, evidence }));
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

  // #1832 item 1: the New-epic dialog carries ITS OWN project context
  // (the swimlane header button sets it directly — the global toolbar
  // button sets it from the focus dropdown; null = closed).
  const [createEpicTarget, setCreateEpicTarget] = useState<string | null>(null);
  const [epicTitle, setEpicTitle] = useState("");
  const [epicOutcome, setEpicOutcome] = useState("");
  const [epicTags, setEpicTags] = useState<string[]>([]);

  // #1832 item 2: the swimlane's selected epic (projectId → epicId) —
  // highlight/dim + task-creation scope. 2026-10-28 ruling 2: the epic
  // rows carry no nested task lists anymore, so the click-highlight is
  // the ONLY epic→task navigation (kept intact); and the New-task epic
  // picker (opened when no epic is selected).
  const [selectedEpicByProject, setSelectedEpicByProject] = useState<Record<string, string>>({});
  const [taskPickerProject, setTaskPickerProject] = useState<BoardProject | null>(null);

  const [taskDialog, setTaskDialog] = useState<{ mode: "create"; epic: BoardEpic } | { mode: "edit"; task: BoardTask } | null>(null);
  const [dialogError, setDialogError] = useState<DescribedBoardError | null>(null);
  const [epicDrawer, setEpicDrawer] = useState<BoardEpic | null>(null);
  // #1832 item 4: the task-detail drawer (charter + criteria + full
  // audit timeline) — opened from the card's details affordance.
  // #1832 item 3: kebab delete flows — the modals' targets + their own
  // error channel (a refusal renders INSIDE the modal, which stays open
  // for a retry; page-level notices carry the successes).
  const [taskDeleteTarget, setTaskDeleteTarget] = useState<BoardTask | null>(null);
  const [epicDeleteTarget, setEpicDeleteTarget] = useState<BoardEpic | null>(null);
  const [deleteError, setDeleteError] = useState<DescribedBoardError | null>(null);

  const [taskDrawer, setTaskDrawer] = useState<BoardTask | null>(null);

  // Owner ask 2026-11-13: the Done column's show-all drawer — which
  // lane's done set it carries (null = closed). Per-mount state, no
  // persistence (the board re-opens collapsed; there is no UI-prefs
  // pattern to ride).
  const [doneDrawerProjectId, setDoneDrawerProjectId] = useState<string | null>(null);

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
      rememberEpic(epic);
      setCreateEpicTarget(null);
      setEpicTitle("");
      setEpicOutcome("");
      setEpicTags([]);
      void queryClient.invalidateQueries({ queryKey: ["board"] });
    },
    onError: (err) => setCreateError(toBoardError(err)),
  });

  // #1832 item 2: the swimlane's LIVE selected epic — derived, so a
  // selection whose epic vanished from the read model reads as none
  // (never a stale scope for creation).
  const selectedEpicOf = useCallback(
    (projectId: string): BoardEpic | null => {
      const sel = selectedEpicByProject[projectId];
      if (!sel) return null;
      return epicsByProject.get(projectId)?.find((e) => e.id === sel) ?? null;
    },
    [selectedEpicByProject, epicsByProject],
  );

  const toggleEpicSelection = useCallback((projectId: string, epicId: string) => {
    setSelectedEpicByProject((prev) => {
      const next = { ...prev };
      if (next[projectId] === epicId) delete next[projectId];
      else next[projectId] = epicId;
      return next;
    });
  }, []);

  // #1832 item 1+2: the swimlane's New task — the SELECTED epic scopes
  // creation (New task lands inside it); with no selection the epic
  // picker opens (never a dead/greyed path).
  const openSwimlaneNewTask = useCallback(
    (project: BoardProject) => {
      const epic = selectedEpicOf(project.id);
      if (epic) {
        setTaskDialog({ mode: "create", epic });
        return;
      }
      setTaskPickerProject(project);
    },
    [selectedEpicOf],
  );

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
    // Activate's 200 is the {epic, initial_ready_set} wrapper (hurl
    // board.hurl) — unwrap here so downstream state holds a true row.
    mutationFn: async ({ epic, action }: { epic: BoardEpic; action: "activate" | "pause" }) => {
      const idOrShort = epic.short_id.startsWith("epc-") ? epic.short_id : epic.id;
      return action === "activate" ? (await boardApi.activateEpic(idOrShort)).epic : boardApi.pauseEpic(idOrShort);
    },
    onSuccess: (epic) => {
      setDrawerError(null);
      rememberEpic(epic);
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
      // 2026-10-28 ruling 4: notices lead with the human title, not the id.
      setPageNotice(
        claim.escalated
          ? `${task.title}: attempts cap reached — escalated to Failed (no grant).`
          : `${task.title}: fresh worker dispatched into the project's target workspace.`,
      );
    },
    onError: (err) => setPageError(toBoardError(err)),
    onSettled: (_d, _e, task) => setStartingShortId((cur) => (cur === task.short_id ? null : cur)),
  });

  // #1832 item 3 — task kebab → delete. The reconnect checkbox rides
  // through verbatim (#1747's optional flag): unchecked = dependents
  // simply unblocked (audited), checked = they inherit the deleted
  // task's direct upstream edges (dedup + cycle-guarded server-side).
  const handleTaskDelete = useCallback(
    async (task: BoardTask, reconnect: boolean): Promise<boolean> => {
      setDeleteError(null);
      try {
        const result = await boardApi.deleteTask(task.short_id, reconnect);
        setTaskDeleteTarget(null);
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        setPageNotice(describeStoryDeleteResult(result));
        return true;
      } catch (err) {
        setDeleteError(toBoardError(err));
        return false;
      }
    },
    [queryClient],
  );

  // #1832 item 3 — epic kebab → delete. The first attempt NEVER sends
  // force (the engine refuses with epic_state while claims are live —
  // the refusal reveals the force-confirm in the modal; force aborts
  // each in-flight worker post-commit).
  const handleEpicDelete = useCallback(
    async (epic: BoardEpic, force: boolean): Promise<boolean> => {
      setDeleteError(null);
      const idOrShort = epic.short_id.startsWith("epc-") ? epic.short_id : epic.id;
      try {
        const result = await boardApi.deleteEpic(idOrShort, force);
        setEpicDeleteTarget(null);
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        const cascade = result.cascade_tasks ?? [];
        const orphans = result.orphaned_dependents ?? [];
        setPageNotice(
          `${epic.title} deleted — ${cascade.length} stor${cascade.length === 1 ? "y" : "ies"} cascaded` +
            (orphans.length > 0 ? `; ${orphans.length} cross-epic dependent${orphans.length === 1 ? "" : "s"} orphaned (audited)` : ""),
        );
        return true;
      } catch (err) {
        setDeleteError(toBoardError(err));
        return false;
      }
    },
    [queryClient],
  );

  // #1832 item 3 — project kebab: archive/unarchive ONLY (#1747 —
  // project delete is eliminated; the menu never offers it).
  const handleProjectLifecycle = useCallback(
    async (project: BoardProject, action: "archive" | "unarchive"): Promise<void> => {
      setPageError(null);
      try {
        await (action === "archive" ? boardApi.archiveProject(project.id) : boardApi.unarchiveProject(project.id));
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        // #1964: archiving the focused lane would leave the scope
        // pointed at a project the active surface no longer renders —
        // reset to All (the archived lane lives in the drawer now).
        if (action === "archive" && focusProject === project.id) {
          setFocusProject("all");
        }
        // 2026-10-28 ruling 4: the notice names the project, not its id.
        setPageNotice(`${project.name}: ${action === "archive" ? "archived (claims into it stopped)" : "unarchived (active again)"}.`);
      } catch (err) {
        setPageError(toBoardError(err));
      }
    },
    [queryClient, focusProject],
  );

  // §4.8 activity feed: subscribe to the root stream's board.event fan-
  // out (boardFeed). Display-only — the ["board"] invalidation in the
  // same branch stays the authoritative update path.
  useEffect(() => {
    return subscribeBoardEvents((evt) => {
      setBoardActivity((prev) => [evt, ...prev].slice(0, 50));
    });
  }, []);

  // Epic lifecycle on the band kebab (owner ask 2026-11-14; the
  // activate/pause arm mirrors the drawer pattern — server-confirmed
  // rememberEpic flip + ["board"] invalidate, page notice naming the
  // epic, page-level BoardErrorNotice on failure (the drawer is not
  // open; drawerError would be invisible here)). No confirmation:
  // none of these writes is destructive (archive is reversible
  // storage) and the drawer's own buttons never confirm.
  const handleEpicLifecycle = useCallback(
    async (epic: BoardEpic, action: "activate" | "pause"): Promise<void> => {
      setPageError(null);
      const idOrShort = epic.short_id.startsWith("epc-") ? epic.short_id : epic.id;
      try {
        const updated =
          action === "activate" ? (await boardApi.activateEpic(idOrShort)).epic : await boardApi.pauseEpic(idOrShort);
        rememberEpic(updated);
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        setPageNotice(
          action === "activate"
            ? `${epic.title}: activated — ready stories start feeding to fresh workers.`
            : `${epic.title}: paused (in-flight leases run out; new claims stopped).`,
        );
      } catch (err) {
        setPageError(toBoardError(err));
      }
    },
    [queryClient, rememberEpic],
  );

  // Archive/unarchive — the epic's reversible-storage lever, same shape
  // as handleProjectLifecycle's archive arm. Unarchive always restores
  // ACTIVE (the engine's resumable default — the epic seam has no
  // separate resume endpoint; activate is the un-pause path).
  const handleEpicArchive = useCallback(
    async (epic: BoardEpic, action: "archive" | "unarchive"): Promise<void> => {
      setPageError(null);
      const idOrShort = epic.short_id.startsWith("epc-") ? epic.short_id : epic.id;
      try {
        const updated = action === "archive" ? await boardApi.archiveEpic(idOrShort) : await boardApi.unarchiveEpic(idOrShort);
        rememberEpic(updated);
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        setPageNotice(
          action === "archive"
            ? `${epic.title}: archived — it leaves the claimable set (unarchive restores it).`
            : `${epic.title}: unarchived — active again; ready stories start feeding.`,
        );
      } catch (err) {
        setPageError(toBoardError(err));
      }
    },
    [queryClient, rememberEpic],
  );

  // The epic band kebab's item list per the engine's lifecycle
  // allowed-map (service.go epicLifecycle): active ⟵
  // {draft,paused,completed,archived} (unarchive owns the archived
  // source — its dedicated endpoint), paused ⟵ {active}, archived ⟵
  // {draft,active,paused,completed}. There is NO epic resume endpoint
  // (activate un-pauses) — the inventory's "Resume" maps onto Activate.
  const buildEpicKebabItems = useCallback(
    (epic: BoardEpic): KebabMenuItem[] => {
      if (epic.status === "archived") {
        return [
          { section: "Lifecycle", label: "Unarchive epic", onClick: () => void handleEpicArchive(epic, "unarchive") },
          {
            label: "Delete epic…",
            destructive: true,
            onClick: () => {
              setDeleteError(null);
              setEpicDeleteTarget(epic);
            },
          },
        ];
      }
      return [
        ...(epic.status === "active"
          ? [{ section: "Lifecycle", label: "Pause epic", onClick: () => void handleEpicLifecycle(epic, "pause") }]
          : [{ section: "Lifecycle", label: "Activate epic", onClick: () => void handleEpicLifecycle(epic, "activate") }]),
        { section: "Lifecycle", label: "Archive epic", onClick: () => void handleEpicArchive(epic, "archive") },
        {
          label: "Delete epic…",
          destructive: true,
          onClick: () => {
            setDeleteError(null);
            setEpicDeleteTarget(epic);
          },
        },
      ];
    },
    [handleEpicArchive, handleEpicLifecycle],
  );

  // --- Story card kebab: the per-entity action surface (owner ask
  // 2026-11-14) ----------------------------------------------------------
  // Non-stage writes (pause/resume, the criteria toggles) get their OWN
  // lightweight optimism: a per-card override map flips the visible
  // badge/criterion immediately, the override is CLEARED once the
  // invalidated read lands the server's authoritative row (r2), and a
  // refusal rolls it back and renders the page-level notice (the drag
  // lane owns stage optimism — the kebab's stage items reuse runMove
  // wholesale).
  const [cardOverrides, setCardOverrides] = useState<Record<string, { paused?: boolean; acceptance_criteria?: BoardTask["acceptance_criteria"] }>>({});

  const applyCardOverrides = useCallback(
    (t: BoardTask): BoardTask => {
      const o = cardOverrides[t.short_id];
      if (!o) return t;
      return { ...t, ...(o.paused === undefined ? {} : { paused: o.paused }), ...(o.acceptance_criteria ? { acceptance_criteria: o.acceptance_criteria } : {}) };
    },
    [cardOverrides],
  );

  /** Retires ONE field's optimism (r2 stale-card-override-mask): the
   * success path calls it once the invalidated read has landed the
   * authoritative row, the refusal path calls it to roll back — either
   * way the override can never outlive the write it optimizes, so a
   * later authoritative change (dialog criteria edit, second tab, SSE
   * refetch) always renders. Dropping the entry when its last field
   * leaves keeps the map from masking rows it no longer speaks for. */
  const clearCardOverride = useCallback((shortId: string, field: "paused" | "acceptance_criteria") => {
    setCardOverrides((prev) => {
      const o = prev[shortId];
      if (!o || o[field] === undefined) return prev;
      const retained = { ...o };
      delete retained[field];
      if (retained.paused === undefined && !retained.acceptance_criteria) {
        const next = { ...prev };
        delete next[shortId];
        return next;
      }
      return { ...prev, [shortId]: retained };
    });
  }, []);

  /** Pause/resume (#1747 lever): the paused badge flips optimistically;
   * failure rolls it back and renders at page level (the drag lane's
   * snap-back is stage-specific — this is the flag's equivalent). */
  const handleStoryPauseResume = useCallback(
    async (task: BoardTask, action: "pause" | "resume"): Promise<void> => {
      setPageError(null);
      const target = action === "pause";
      setCardOverrides((prev) => ({ ...prev, [task.short_id]: { ...prev[task.short_id], paused: target } }));
      try {
        await (action === "pause" ? boardApi.pauseTask(task.short_id) : boardApi.resumeTask(task.short_id));
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        // The invalidated read has landed the server's row — retire
        // this write's optimism so later authoritative changes render.
        clearCardOverride(task.short_id, "paused");
        setPageNotice(
          action === "pause"
            ? `${task.title}: paused — claim candidacy off; the in-flight worker halts at its turn's boundary.`
            : `${task.title}: resumed — a survived lease continues; a lapsed one re-enters the FIFO pool.`,
        );
      } catch (err) {
        clearCardOverride(task.short_id, "paused");
        setPageError(toBoardError(err));
      }
    },
    [clearCardOverride, queryClient],
  );

  /** Per-criterion check-off toggle (#1837 P2b's TWO-WAY wire form
   * {index, met}): the glyph flips optimistically; failure rolls it
   * back with the page-level notice. Never rides acceptanceCriteria —
   * the indexed write cannot clobber a concurrent edit's array. */
  const handleCriterionToggle = useCallback(
    async (task: BoardTask, index: number): Promise<void> => {
      setPageError(null);
      const current = applyCardOverrides(task).acceptance_criteria;
      const criterion = current[index];
      if (!criterion) return;
      const met = !criterion.met;
      const nextCriteria = current.map((c, i) => (i === index ? { ...c, met } : c));
      setCardOverrides((prev) => ({ ...prev, [task.short_id]: { ...prev[task.short_id], acceptance_criteria: nextCriteria } }));
      try {
        await boardApi.updateTask(task.short_id, { setCriteriaMet: { index, met } });
        await queryClient.invalidateQueries({ queryKey: ["board"] });
        // The invalidated read has landed the server's row — retire
        // this write's optimism so later authoritative changes render.
        clearCardOverride(task.short_id, "acceptance_criteria");
        setPageNotice(`${task.title}: criterion ${index + 1} ${met ? "met" : "unmet"}.`);
      } catch (err) {
        clearCardOverride(task.short_id, "acceptance_criteria");
        setPageError(toBoardError(err));
      }
    },
    [applyCardOverrides, clearCardOverride, queryClient],
  );

  // The state matrix, pinned by BoardPage.kebab.test.tsx:
  //   backlog        → Pause, criteria, Delete
  //   in_progress    → Pause, Move to failed…, criteria, Delete
  //   in_review      → Pause, Move to failed…, criteria, Delete
  //   pending_input  → Pause, Requeue / Resume work, criteria, Delete
  //   paused         → Resume, criteria, Delete
  //   failed+esc     → Pause, Re-arm, criteria, Delete
  //   done           → criteria, Delete ONLY (terminal: no stage items)
  // Worker-owned transitions (→in_review, →done, the review bounce) are
  // deliberately absent — the kebab is the manager surface; the drag
  // affordance mirror (isManagerDragAllowed) keeps its own superset.
  const buildStoryKebabItems = useCallback(
    (task: BoardTask): KebabMenuItem[] => {
      const items: KebabMenuItem[] = [];
      const effective = applyCardOverrides(task);
      const terminal = task.stage === STAGE_DONE;
      if (effective.paused) {
        items.push({ section: "Lifecycle", label: "Resume story", onClick: () => void handleStoryPauseResume(task, "resume") });
      } else if (!terminal) {
        items.push({ section: "Lifecycle", label: "Pause story", onClick: () => void handleStoryPauseResume(task, "pause") });
      }
      if (task.stage === "failed" && task.pending_origin === "escalated") {
        items.push({
          section: "Lifecycle",
          label: "Re-arm story (reset attempts)",
          onClick: () => void runMove(task, "backlog", undefined, () => boardApi.rearmTask(task.short_id)),
        });
      }
      if (!terminal) {
        if (task.pending_input) {
          // 000050: the flag's two manager exits — the ONE attempts
          // reset (the human re-queue is a fresh judgment).
          items.push({ section: "Stage", label: "Requeue to backlog", onClick: () => void runMove(task, "backlog") });
          items.push({ section: "Stage", label: "Resume work (re-claim)", onClick: () => void runMove(task, "in_progress") });
        } else if (task.stage === "in_progress" || task.stage === "in_review") {
          // stuck → failed: the failure terminus. A CLAIMED in_progress
          // row additionally demands its live token (the engine's
          // claim-token guard) — the honest 409 renders; in_review and
          // unclaimed rows move.
          items.push({ section: "Stage", label: "Move to failed…", onClick: () => void runMove(task, "failed") });
        }
      }
      effective.acceptance_criteria.forEach((c, i) => {
        items.push({
          section: "Criteria",
          label: `${c.met ? "✓" : "○"} ${i + 1}. ${kebabTruncate(c.text)}`,
          onClick: () => void handleCriterionToggle(task, i),
        });
      });
      items.push({
        label: "Delete story…",
        destructive: true,
        onClick: () => {
          setDeleteError(null);
          setTaskDeleteTarget(task);
        },
      });
      return items;
    },
    [applyCardOverrides, handleCriterionToggle, handleStoryPauseResume, runMove],
  );

  // Open the epic drawer with the authoritative row (derived progress).
  // Failures render at PAGE level (r1: they previously landed in
  // dialogError, invisible with no StoryDialog open).
  const openEpicDrawer = useCallback(
    async (epicId: string) => {
      try {
        const epic = await boardApi.getEpic(epicId);
        rememberEpic(epic);
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

  // Owner ask 2026-11-13: the Done drawer's lane — derived, so a lane
  // that vanishes (archived/deleted mid-view, here or cross-context)
  // reads as closed rather than stranding a drawer over nothing (the
  // #1964 r1 stale-focus posture).
  const doneDrawerProject = doneDrawerProjectId === null ? null : projects.find((p) => p.id === doneDrawerProjectId) ?? null;

  if (projectsQuery.isLoading) {
    return (
      <div className="flex h-full items-center justify-center" data-testid="board-loading">
        <Spinner size="lg" />
      </div>
    );
  }

  const slotsInUse = (projectId: string) =>
    tasks.filter((t) => t.project_id === projectId && t.stage === "in_progress").length;

  // #1964: ONE swimlane renderer for both surfaces — the active
  // board's lanes and the Archived drawer's expanded projects render
  // identically (the drawer is the same lane, one click deeper;
  // history stays reachable — purely presentation, the #1755
  // archive semantics are untouched).
  const renderSwimlane = (project: BoardProject) => {
    const projectTasks = filteredTasks.filter((t) => t.project_id === project.id);
    const laneAllTasks = tasks.filter((t) => t.project_id === project.id);
    const laneEpics = epicsByProject.get(project.id) ?? [];
    // Ruling 3: activity-first order (alpha tiebreak) — computed once
    // per lane render; the scroll cap does the visible-window cut.
    const epicActivityById = new Map<string, number>();
    for (const e of laneEpics) epicActivityById.set(e.id, epicActivity(e, laneAllTasks));
    const sortedLaneEpics = [...laneEpics].sort(
      (a, b) =>
        (epicActivityById.get(b.id) ?? 0) - (epicActivityById.get(a.id) ?? 0) || a.title.localeCompare(b.title),
    );
    // The swimlane's selection (derived — a vanished epic reads
    // as unselected) drives highlight/dim on the kanban cards
    // and scopes the header's New task.
    const selectedEpic = selectedEpicOf(project.id);
    const highlightEpicId = selectedEpic?.id ?? null;
    return (
      <section key={project.id} data-swimlane={project.short_id} data-testid={`swimlane-${project.short_id}`} className="rounded-lg border border-border">
        <header className="flex flex-wrap items-center gap-2 border-b border-border px-3 py-1.5">
          <span className="text-xs font-semibold">{project.name}</span>
          <span className="text-[10px] text-muted-foreground" data-testid={`slots-${project.short_id}`}>
            {slotsInUse(project.id)}/{project.worker_slots} slots
          </span>
          {project.status === "archived" && <Badge variant="muted" className="text-[10px]">archived</Badge>}
          {project.slot_cooldown_until && new Date(project.slot_cooldown_until) > new Date() && (
            <Badge variant="warning" className="text-[10px]" title="Provider throttle: new claims paused">
              slot cooldown
            </Badge>
          )}
          {/* #1832 item 1: contextual creation — the swimlane IS
            * the selection (New epic / New task land here, no
            * dropdown needed; New task honors the selected epic
            * band, else the picker). The Configure gear is the
            * project's config entry point ON the board (owner
            * ruling 2026-10-28 — "alongside the projects"). */}
          <div className="ml-auto flex items-center gap-1">
            <ProjectConfigTrigger project={project} />
            <Button
              variant="outline"
              size="sm"
              className="h-6 px-2 text-[11px]"
              onClick={() => { setCreateEpicTarget(project.id); setCreateError(null); }}
              data-testid={`new-epic-${project.short_id}`}
              title={`New epic in ${project.name}`}
            >
              New epic
            </Button>
            <Button
              variant="outline"
              size="sm"
              className="h-6 px-2 text-[11px]"
              onClick={() => openSwimlaneNewTask(project)}
              data-testid={`new-task-${project.short_id}`}
              title={selectedEpic ? `New story in ${selectedEpic.title}` : `New story in ${project.name} — pick an epic`}
            >
              New story
            </Button>
            {/* #1832 item 3: project kebab — archive/unarchive
             * ONLY (#1747: project delete is eliminated; this
             * menu never grows a delete item). */}
            <KebabMenu
              ariaLabel={`Project actions for ${project.name}`}
              testId={`project-kebab-${project.short_id}`}
              items={[
                project.status === "archived"
                  ? {
                      label: "Unarchive project",
                      onClick: () => void handleProjectLifecycle(project, "unarchive"),
                    }
                  : {
                      label: "Archive project",
                      onClick: () => void handleProjectLifecycle(project, "archive"),
                    },
              ]}
            />
          </div>
        </header>
        {/* 2026-10-28 rulings 2+3: ONE ROW per epic — title, status,
         * counts at most; NO nested task list underneath (the row's
         * click highlights the epic's task cards across the columns —
         * now the ONLY epic→task navigation). The list is CAPPED at
         * EPIC_LIST_MAX_VISIBLE rows; overflow scrolls INSIDE this
         * container (its own region, never page growth), ordered by
         * recent activity first (the newest of the epic's own
         * updated_at and its task rows' updated_at — data the view
         * layer already holds; alpha is the tiebreak/quiet fallback). */}
        <div className="border-b border-border px-2 py-1.5" data-testid={`epic-bands-${project.short_id}`}>
          {laneEpics.length === 0 ? (
            <span className="px-1 text-[10px] text-muted-foreground/70">No epics yet — use New epic above.</span>
          ) : (
            <div
              // 8.5rem = 4 × 1.75rem rows + 3 × gap-1 + slack: the cap
              // fits exactly 4 rows with headroom (zoom/min-font row
              // growth clips nothing) while >4 rows scroll INSIDE this
              // region — pinned in tests/e2e/board.spec.ts.
              className="flex max-h-[8.5rem] flex-col gap-1 overflow-y-auto"
              data-testid={`epic-list-${project.short_id}`}
              data-max-visible={EPIC_LIST_MAX_VISIBLE}
            >
              {sortedLaneEpics.map((epic) => {
                const selected = selectedEpic?.id === epic.id;
                return (
                  <div
                    key={epic.id}
                    data-testid={`epic-band-${epic.id}`}
                    data-selected={selected ? "true" : undefined}
                    data-epic-short-id={epic.short_id}
                    className={cn(
                      "flex min-h-[1.75rem] cursor-pointer select-none items-center gap-1.5 rounded-md border px-2 py-1 transition-colors",
                      selected ? "border-primary/50 bg-primary/5" : "border-border bg-muted/20 hover:border-primary/30",
                    )}
                    onClick={() => toggleEpicSelection(project.id, epic.id)}
                    onDoubleClick={() => void openEpicDrawer(epic.id)}
                    title={`${epic.title} — click to highlight its stories; double-click for the epic drawer`}
                  >
                    {/* Ruling 4: the TITLE is the label — the epc- id
                     * never renders here (it lives in the click-through
                     * drawer; data attrs keep it for tests). */}
                    <span className="min-w-0 flex-1 truncate text-[11px] font-medium" data-testid={`band-title-${epic.id}`}>
                      {epic.title}
                    </span>
                    <span className="shrink-0 text-[10px] text-muted-foreground" data-testid={`band-progress-${epic.id}`}>
                      {epic.progress_done}/{epic.progress_total}
                    </span>
                    {/* The band's status badges (owner ask 2026-11-14:
                     * every non-active state is legible on the row —
                     * the kebab's conditional items key on the same
                     * status). */}
                    {epic.status === "paused" && <Badge variant="warning" className="shrink-0 px-1.5 py-0 text-[10px]">paused</Badge>}
                    {epic.status === "draft" && <Badge variant="muted" className="shrink-0 px-1.5 py-0 text-[10px]">draft</Badge>}
                    {epic.status === "completed" && <Badge variant="default" className="shrink-0 px-1.5 py-0 text-[10px]">completed</Badge>}
                    {epic.status === "archived" && <Badge variant="muted" className="shrink-0 px-1.5 py-0 text-[10px]">archived</Badge>}
                    {selected && <Badge variant="default" className="shrink-0 px-1.5 py-0 text-[10px]">selected</Badge>}
                    {/* #1832 item 3 + owner ask 2026-11-14: epic kebab —
                     * the lifecycle surface (activate/pause,
                     * archive/unarchive per the engine's allowed-map)
                     * plus the delete flow (in-flight surfacing +
                     * refuse/force-confirm inside the modal). The
                     * trigger stops propagation: the kebab never
                     * toggles the row's selection. */}
                    <span className="shrink-0">
                      <KebabMenu
                        ariaLabel={`Epic actions for ${epic.title}`}
                        testId={`epic-kebab-${epic.id}`}
                        items={buildEpicKebabItems(epic)}
                      />
                    </span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
        {/* Owner direction 2026-11-15: a board is a PIPELINE — pipeline
         * state reads left-to-right, and the old 2x2 grid put
         * in_review UNDER in_progress, destroying the flow axis. The
         * 1x4 rail: one row, all four columns, scroll-snap per column
         * (the snap points ARE the navigation; #2021 gives arrow keys
         * + visible focus the same path). Geometry confirmation before
         * the grid removal: nothing in the board depends on the 2x2
         * geometry — drag/drop and the card interactions are
         * event-target based (data-stage-drop closest() lookups, kebab
         * stopPropagation), never grid-position based, and the column
         * cells are DOM-independent siblings either way. */}
        <StageRail
          laneShortId={project.short_id}
          onDropStage={handleDrop}
          cellProps={{
            projectTasks,
            allProjectTasks: laneAllTasks,
            workspaceId: project.target_workspace_id,
            stageOf,
            optimistic,
            refusedShortId,
            draggingStage,
            highlightEpicId,
            onOpenCard: (t) => setTaskDialog({ mode: "edit", task: t }),
            onCardDetails: (t) => setTaskDrawer(t),
            onCardDragStart: (t) => setDraggingStage(stageDragKey(t)),
            onCardDragEnd: () => setDraggingStage(null),
            onCardStart: (t) => {
              setStartingShortId(t.short_id);
              startTaskMutation.mutate(t);
            },
            storyKebabItems: buildStoryKebabItems,
            decorateTask: applyCardOverrides,
            startingShortId,
            laneShortId: project.short_id,
            onShowAllDone: () => setDoneDrawerProjectId(project.id),
          }}
        />
        {/* Side column: Failed (collapsible, §4.8; pending_input rows
         * stay in their real stage wearing the badge) */}
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
                  allProjectTasks={laneAllTasks}
                  workspaceId={project.target_workspace_id}
                  stageOf={stageOf}
                  optimistic={optimistic}
                  refusedShortId={refusedShortId}
                  draggingStage={draggingStage}
                  highlightEpicId={highlightEpicId}
                  onDrop={handleDrop(stage)}
                  onOpenCard={(t) => setTaskDialog({ mode: "edit", task: t })}
                  onCardDetails={(t) => setTaskDrawer(t)}
                  onCardDragStart={(t) => setDraggingStage(stageDragKey(t))}
                  onCardDragEnd={() => setDraggingStage(null)}
                  onCardStart={(t) => {
                    setStartingShortId(t.short_id);
                    startTaskMutation.mutate(t);
                  }}
                  storyKebabItems={buildStoryKebabItems}
                  decorateTask={applyCardOverrides}
                  startingShortId={startingShortId}
                />
              ))}
            </div>
          )}
        </div>
      </section>
    );
  };

  return (
    <div className="flex h-full flex-col overflow-hidden" data-page="board">
      {/* --- Scope switcher (project focus + filter drawer + search) ------------ */}
      <div className="flex flex-col gap-2 border-b border-border px-4 py-2" data-testid="board-toolbar">
        <div className="flex flex-wrap items-center gap-2">
          <KanbanSquare className="h-4 w-4 text-muted-foreground" aria-hidden />
          <h1 className="text-sm font-semibold">Sprint Board</h1>
          <select
            aria-label="Project focus"
            value={focusProject}
            onChange={(e) => {
              setFocusProject(e.target.value);
            }}
            className="h-8 rounded-md border border-border bg-background px-2 text-sm"
            data-testid="project-focus"
          >
            <option value="all">All projects</option>
            {/* #1964: archived projects never list here — the drawer is
             * their only surface (focus is the ACTIVE board's scope).
             * Ruling 4: the option label is the project NAME (the id
             * lives in the option's value, never its text). */}
            {activeProjects.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search title, tag or id…"
            className="h-8 w-56"
            aria-label="Search stories"
            data-testid="board-search"
          />
          <div className="ml-auto flex items-center gap-2">
            {/* Owner ruling 2026-10-29 ("I think those pills need to be in
             * a collapsed drawer. They eat up a lot of screen real estate
             * esp on mobile."): the funnel button replaces the inline pill
             * row — it opens the filter drawer below, and the count badge
             * keeps active filters VISIBLE while the drawer is closed. */}
            <Button
              variant="outline"
              size="sm"
              onClick={() => setFilterDrawerOpen((v) => !v)}
              aria-expanded={filterDrawerOpen}
              aria-controls="board-filter-drawer"
              data-testid="board-filter-button"
              title="Filters (stage overlay, tags)"
            >
              <ListFilter className="h-4 w-4" aria-hidden />
              Filters
              {activeFilterCount > 0 && (
                <span
                  className="ml-1 inline-flex h-4 min-w-4 items-center justify-center rounded-full bg-primary px-1 text-[10px] font-semibold leading-none text-primary-foreground"
                  data-testid="board-filter-count"
                >
                  {activeFilterCount}
                </span>
              )}
            </Button>
            {/* Board config lives ON the board (owner ruling
             * 2026-10-28: "There are no configuration options for the
             * board" — relocated out of Settings): the defaults dialog
             * carries review column, auto-review, worker slots, and
             * the stage-persona editor; each swimlane header carries
             * its project's Configure gear. */}
            <Button
              variant="outline"
              size="sm"
              onClick={() => setDefaultsOpen(true)}
              data-testid="board-defaults-button"
            >
              <Settings2 className="h-4 w-4" aria-hidden />
              Board defaults
            </Button>
            {/* #1832 item 1: the global button stays, but the swimlane
             * headers carry the contextual path (never greyed-only). */}
            <Button
              variant="outline"
              size="sm"
              onClick={() => { setCreateEpicTarget(focusProject !== "all" ? focusProject : null); setCreateError(null); }}
              disabled={focusProject === "all" || projects.length === 0}
              data-testid="new-epic-button"
            >
              New epic
            </Button>
            <Button variant="outline" size="sm" onClick={() => { setShowCreateProject(true); setCreateError(null); }} data-testid="new-project-button">
              New project
            </Button>
            {/* Owner ruling 2026-10-30: a help affordance on the board —
             * opens the "How the sprint board works" drawer (what it is,
             * the recommended workflow, worked examples). Icon-only, so
             * the accessible name carries the full intent. */}
            <Button
              variant="outline"
              size="sm"
              className="h-8 w-8 px-0"
              onClick={() => setHelpOpen(true)}
              aria-label="Learn more: how the sprint board works"
              title="Learn more: how the sprint board works"
              data-testid="board-help-button"
            >
              <HelpCircle className="h-4 w-4" aria-hidden />
            </Button>
          </div>
        </div>
        {/* Owner ruling 2026-10-29: the filter pills live in this
         * COLLAPSIBLE drawer (opened by the toolbar's funnel button) —
         * no pills inline in the toolbar. 2026-10-28 ruling 1 stays
         * true a fortiori: no epic pills anywhere; the real filters
         * (the #1813 FAILED overlay, tag facets) render HERE, and epic
         * association is discovered via the epic rows'
         * click-highlight. Closed = not rendered = ZERO height — on
         * mobile the board content starts immediately below the
         * toolbar (nothing overlaps the swim lanes). */}
        {filterDrawerOpen && (
          <div
            id="board-filter-drawer"
            role="group"
            aria-label="Board filters"
            className="flex flex-wrap items-center gap-1.5 rounded-md border border-border bg-muted/30 px-2 py-1.5"
            data-testid="board-filter-drawer"
          >
            <button
              onClick={() => setFailedOnly(!failedOnly)}
              className={cn(
                "rounded px-1.5 py-0.5 text-[10px] transition-colors",
                failedOnly ? "bg-destructive/15 text-destructive" : "bg-muted text-muted-foreground hover:bg-accent",
              )}
              data-testid="failed-filter-toggle"
              title="#1813: show only stories whose last worker attempt failed (the overlay — never a stage)"
            >
              FAILED
            </button>
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
            {/* The drawer's own reset affordance — with the pills folded
             * away this is where a stale filter gets cleared (disabled
             * at zero active filters; the count badge mirrors it). */}
            <Button
              variant="ghost"
              size="sm"
              className="ml-auto h-6 px-2 text-[11px]"
              onClick={() => {
                setFailedOnly(false);
                setTagFilter(null);
              }}
              disabled={activeFilterCount === 0}
              data-testid="board-filter-clear"
            >
              Clear all
            </Button>
          </div>
        )}
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
                        key={s.shortId}
                        type="button"
                        onClick={() => setSearch(s.title)}
                        data-suggest-short-id={s.shortId}
                        className="mr-1 rounded bg-accent px-1.5 py-0.5 text-[10px]"
                      >
                        {s.title}
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
            {/* Owner ruling 2026-10-30: the first-run moment carries the
             * help entry point — same drawer, discoverable before any
             * project exists. */}
            <Button
              variant="ghost"
              size="sm"
              className="text-muted-foreground"
              onClick={() => setHelpOpen(true)}
              aria-label="Learn more: how the sprint board works"
              data-testid="board-empty-learn-more"
            >
              <HelpCircle className="mr-1 h-3.5 w-3.5" aria-hidden />
              How the sprint board works
            </Button>
          </div>
        ) : projectsQuery.isSuccess && activeProjects.length === 0 ? (
          // #1964: every project archived — the active surface is empty
          // but the board itself is not; point at the drawer below.
          <div className="flex flex-col items-center gap-2 p-6 text-sm text-muted-foreground" data-testid="board-no-active-projects">
            <span>No active projects — archived history sits in the drawer below.</span>
          </div>
        ) : null}
        <div className="flex flex-col gap-4">
          {visibleProjects.map(renderSwimlane)}
          {/* #1964: the Archived drawer — at the board's end. Collapsed
           * by default (deliberate friction, the #1627 Archived-group
           * ruling); each archived project inside is itself a collapsed
           * drawer showing ONLY its name until clicked (progressive
           * disclosure). Expanding a project renders its full swimlane:
           * history stays reachable and the lane kebab keeps Unarchive —
           * purely presentation. */}
          {archivedProjects.length > 0 && (
            <section className="rounded-lg border border-border" data-testid="archived-projects-drawer">
              <button
                type="button"
                onClick={() => setArchivedDrawerExpanded((v) => !v)}
                className="flex w-full items-center gap-2 px-3 py-1.5 text-left"
                aria-expanded={archivedDrawerExpanded}
                aria-label={archivedDrawerExpanded ? "Collapse archived projects" : "Expand archived projects"}
                data-testid="archived-projects-toggle"
              >
                <ChevronRight
                  className={cn("h-3 w-3 text-muted-foreground transition-transform", archivedDrawerExpanded && "rotate-90")}
                />
                <Archive className="h-3 w-3 text-muted-foreground" aria-hidden="true" />
                <span className="text-xs font-semibold text-muted-foreground">Archived</span>
                <span className="text-[10px] text-muted-foreground/60" data-testid="archived-projects-count">
                  {archivedProjects.length}
                </span>
              </button>
              {archivedDrawerExpanded && (
                <div className="flex flex-col gap-2 border-t border-border p-2">
                  {archivedProjects.map((project) => {
                    const expanded = expandedArchivedProjects.has(project.id);
                    return (
                      <div
                        key={project.id}
                        className="rounded-md border border-border"
                        data-testid={`archived-project-${project.short_id}`}
                      >
                        {/* Name ONLY until clicked — the project's
                         * history renders exclusively inside this
                         * drawer, never on the active surface. */}
                        <button
                          type="button"
                          onClick={() => toggleArchivedProject(project.id)}
                          className="flex w-full items-center gap-2 px-2 py-1 text-left"
                          aria-expanded={expanded}
                          aria-label={expanded ? `Collapse ${project.name}` : `Expand ${project.name}`}
                          data-testid={`archived-project-toggle-${project.short_id}`}
                        >
                          <ChevronRight
                            className={cn("h-3 w-3 text-muted-foreground transition-transform", expanded && "rotate-90")}
                          />
                          <span className="text-xs font-medium">{project.name}</span>
                        </button>
                        {expanded && <div className="border-t border-border">{renderSwimlane(project)}</div>}
                      </div>
                    );
                  })}
                </div>
              )}
            </section>
          )}
        </div>
      </div>

      {/* --- Done drawer (owner ask 2026-11-13) — the Done column's
       * overflow surface: the full done set of ONE lane, newest-first,
       * scrolling in its own region. Rendered BEFORE the dialogs so a
       * drawer/dialog opened later stacks above it (Radix portals
       * append in open order — the details drawer / edit dialog /
       * delete modal all return to this list on close). -------- */}
      <DoneDrawer
        open={doneDrawerProject !== null}
        onOpenChange={(open) => { if (!open) setDoneDrawerProjectId(null); }}
        project={doneDrawerProject}
        tasks={
          doneDrawerProject
            ? filteredTasks.filter((t) => t.project_id === doneDrawerProject.id && stageOf(t) === STAGE_DONE)
            : []
        }
        allProjectTasks={doneDrawerProject ? tasks.filter((t) => t.project_id === doneDrawerProject.id) : []}
        workspaceId={doneDrawerProject?.target_workspace_id}
        highlightEpicId={doneDrawerProject ? selectedEpicOf(doneDrawerProject.id)?.id ?? null : null}
        onOpenCard={(t) => setTaskDialog({ mode: "edit", task: t })}
        onCardDetails={(t) => setTaskDrawer(t)}
        onCardDelete={(t) => {
          setDeleteError(null);
          setTaskDeleteTarget(t);
        }}
      />

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

      {/* --- New epic dialog (target = the swimlane or focus context) -------- */}
      <Dialog.Root open={createEpicTarget !== null} onOpenChange={(open) => { if (!open) setCreateEpicTarget(null); }}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl">
            <Dialog.Title className="text-sm font-semibold">
              New epic in {projects.find((p) => p.id === createEpicTarget)?.name}
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
              <Button variant="ghost" size="sm" onClick={() => setCreateEpicTarget(null)}>Cancel</Button>
              <Button
                size="sm"
                disabled={!epicTitle.trim() || createEpicMutation.isPending}
                onClick={() => createEpicTarget && createEpicMutation.mutate(createEpicTarget)}
                data-testid="epic-create"
              >
                Create
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      {/* --- Epic picker (New task with no band selected, #1832 item 2) ------ */}
      <Dialog.Root open={taskPickerProject !== null} onOpenChange={(open) => { if (!open) setTaskPickerProject(null); }}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col gap-3 rounded-md border border-border bg-background p-4 shadow-xl">
            <Dialog.Title className="text-sm font-semibold">
              New story in {taskPickerProject?.name} — pick an epic
            </Dialog.Title>
            <div className="flex flex-col gap-1" data-testid="epic-picker">
              {(taskPickerProject ? epicsByProject.get(taskPickerProject.id) ?? [] : []).map((epic) => (
                <button
                  key={epic.id}
                  type="button"
                  data-testid={`epic-picker-option-${epic.id}`}
                  onClick={() => {
                    const p = taskPickerProject;
                    setTaskPickerProject(null);
                    if (p) setTaskDialog({ mode: "create", epic });
                  }}
                  className="flex items-center justify-between gap-2 rounded-md border border-border px-3 py-1.5 text-left text-xs hover:border-primary/40 hover:bg-accent"
                >
                  <span className="font-medium">{epic.title}</span>
                  <span className="text-[10px] text-muted-foreground">
                    {epic.progress_done}/{epic.progress_total} done
                  </span>
                </button>
              ))}
              {taskPickerProject && (epicsByProject.get(taskPickerProject.id) ?? []).length === 0 && (
                <div className="flex flex-col gap-2" data-testid="epic-picker-empty">
                  <span className="text-xs text-muted-foreground">
                    No epics in {taskPickerProject.name} yet — a story needs its epic.
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    className="self-start"
                    data-testid="epic-picker-create-epic"
                    onClick={() => {
                      const p = taskPickerProject;
                      setTaskPickerProject(null);
                      if (p) {
                        setCreateEpicTarget(p.id);
                        setCreateError(null);
                      }
                    }}
                  >
                    Create epic in {taskPickerProject.name}
                  </Button>
                </div>
              )}
            </div>
            <div className="flex justify-end">
              <Button variant="ghost" size="sm" onClick={() => setTaskPickerProject(null)}>Cancel</Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      {/* --- Task create/edit ------------------------------------------------------ */}
      <StoryDialog
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

      {/* --- Task-detail drawer (#1832 item 4) ------------------------------------ */}
      <StoryDrawer
        open={taskDrawer !== null}
        onOpenChange={(open) => {
          if (!open) setTaskDrawer(null);
        }}
        task={taskDrawer}
        workspaceId={taskDrawer ? projects.find((p) => p.id === taskDrawer.project_id)?.target_workspace_id : undefined}
        projectTasks={taskDrawer ? tasks.filter((t) => t.project_id === taskDrawer.project_id) : []}
        onEdit={
          taskDrawer
            ? (t) => {
                setTaskDialog({ mode: "edit", task: t });
                setTaskDrawer(null);
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
              {/* Ruling 4: the prompt names the TASK, not its id (the
               * short id stays in the details drawer popup). */}
              Move <span className="font-medium">{donePrompt?.task.title}</span> to Done
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

      {/* --- Task delete (kebab → modal with the reconnect checkbox) -------- */}
      <StoryDeleteDialog
        open={taskDeleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setTaskDeleteTarget(null);
            setDeleteError(null);
          }
        }}
        task={taskDeleteTarget}
        projectTasks={taskDeleteTarget ? tasks.filter((t) => t.project_id === taskDeleteTarget.project_id) : []}
        onDelete={handleTaskDelete}
        error={deleteError}
        onDismissError={() => setDeleteError(null)}
      />

      {/* --- Epic delete (kebab → in-flight surfacing + force-confirm) ------ */}
      <EpicDeleteDialog
        open={epicDeleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) {
            setEpicDeleteTarget(null);
            setDeleteError(null);
          }
        }}
        epic={epicDeleteTarget}
        epicTasks={epicDeleteTarget ? tasks.filter((t) => t.epic_id === epicDeleteTarget.id) : []}
        onDelete={handleEpicDelete}
        error={deleteError}
        onDismissError={() => setDeleteError(null)}
      />

      {/* --- Board defaults (the relocated Settings tab, owner ruling
       * 2026-10-28 — config ON the board) --------------------------------- */}
      <BoardDefaultsDialog open={defaultsOpen} onOpenChange={setDefaultsOpen} />

      {/* --- Help drawer ("How the sprint board works", owner ruling
       * 2026-10-30) -------------------------------------------------------- */}
      <BoardHelpDrawer open={helpOpen} onOpenChange={setHelpOpen} />
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

/** Owner direction 2026-11-15 — the 1x4 pipeline rail's column
 * geometry. PORTRAIT (the default view): the active trio — backlog,
 * in_progress, in_review — shares 95% of the rail's width (29/36/30;
 * in_progress runs widest: titles + assignee + lease chips are the
 * densest column), so the done column's leading edge keeps the final
 * 5% visible — the continuation affordance (carousel edge-peek: done
 * is discoverable without dedicating real estate; most of the time it
 * is terminal reference, hence the existing Done cap + drawer). The
 * active trio cedes 0.375rem per column so the three gap-2 gutters
 * (1.5rem) are funded without eating the peek. DONE renders as a
 * FULL-WIDTH final page (100% + 0.75rem): with done at a share-width,
 * its snap point — and even in_progress's — sits past the rail's
 * maximum scroll (max scroll = last offset + doneWidth − railWidth),
 * and mandatory snap FREEZES at the clamp instead of settling; the
 * full page keeps every column's snap-start reachable, so swipe and
 * arrow keys truly move column-to-column. The resting view is
 * unchanged — done still peeks at 5%; its width only shows once you
 * navigate to it. LANDSCAPE/DESKTOP doubles as the md+ rule: 24/28/
 * 24/24 splits the full row — all four columns visible — over
 * min-width floors that no longer overwhelm a real md-era rail (the
 * board's left panel narrows the rail well below the viewport);
 * narrower rails scroll, never squeeze the cards. Widths live on
 * fixed-basis shrink-0 wrappers, never on content: EMPTY COLUMNS KEEP
 * THEIR WIDTH (skeleton cells), so the layout cannot jump as stages
 * drain. (failed never rides the rail — it is the §4.8 side column —
 * hence the Partial.) */
const STAGE_COL_WIDTH: Partial<Record<BoardStage, string>> = {
  [STAGE_BACKLOG]: "w-[calc(29%-0.375rem)] md:w-[calc(24%-0.375rem)]",
  [STAGE_IN_PROGRESS]: "w-[calc(36%-0.375rem)] md:min-w-[8rem] md:w-[calc(28%-0.375rem)]",
  [STAGE_IN_REVIEW]: "w-[calc(30%-0.375rem)] md:w-[calc(24%-0.375rem)]",
  [STAGE_DONE]: "w-[calc(100%+0.75rem)] md:w-[calc(24%-0.375rem)]",
};

/** Focus-column mode (owner direction 2026-11-15 fast-follow) —
 * PORTRAIT ONLY: the focused column takes 70% of the rail's width
 * (the column you're working gets the width), the compressed
 * neighbors shrink to 10% stubs — enough for a tappable header (the
 * compressed header is itself a transit affordance; the carousel
 * feel). Landscape/desktop has NO focus mode: the chevrons are
 * md:hidden and a matchMedia guard force-exits on the portrait→md
 * crossing, so these classes never render at md+ widths. The
 * compressed floor is 3rem (below the 4.5rem overview floor — a
 * compressed column is a navigation stub, not a reading surface; its
 * cards are hidden, only the header stays).
 *
 * DONE keeps its FULL-PAGE width in focus mode (the overview
 * treatment) — the reachability invariant the rail already carries: a
 * column's snap-start is only reachable when the widths from it to
 * the end cover the rail (start_last ≤ maxScroll ⟺ w_last ≥ rail
 * width). At 70/10/10/10 the content totals ~100% and every column
 * past the first sits beyond max scroll — mandatory snap FREEZES at
 * the clamp and the carousel dies (the same lesson that made done a
 * full page in overview). Focused done IS the full page. The
 * full-page, non-focused done renders as "periphery": cards visible,
 * chevron still transits. */
const STAGE_COL_FOCUS_WIDTH = {
  focused: "w-[calc(70%-0.375rem)]",
  focusedDone: "w-[calc(100%+0.75rem)]",
  compressed: "w-[calc(10%-0.375rem)]",
};

/** The md: HALF of every focus width — the rail's CSS AUTHORITY over
 * viewport class (owner report #3, 2026-11-17: "rotation leaves the
 * columns at the wrong sizes — the portrait layout does not re-apply").
 * The .16 guard made the lens exit depend on a DELIVERED matchMedia
 * change event; an event lost to the renderer's rotation timing (the
 * frozen/hidden-page window, WebKit's coalesced delivery) leaves
 * mdPlus stale and the portrait-only focus widths rendered at md+ AND
 * back — the exact symptom. These variants give the media query the
 * final word INDEPENDENT of component state: at md+ every column
 * renders its overview md width whether the lens state is stale or
 * not (the md row is 24/28/24/24 by STAGE — the map values mirror
 * STAGE_COL_WIDTH's md: halves exactly, min-width floors included, so
 * tailwind-merge resolves focus and overview columns to the identical
 * md geometry; the orientation test pins the equality so the two maps
 * cannot drift). */
const STAGE_COL_FOCUS_MD_WIDTH: Partial<Record<BoardStage, string>> = {
  [STAGE_BACKLOG]: "md:w-[calc(24%-0.375rem)]",
  [STAGE_IN_PROGRESS]: "md:min-w-[8rem] md:w-[calc(28%-0.375rem)]",
  [STAGE_IN_REVIEW]: "md:w-[calc(24%-0.375rem)]",
  [STAGE_DONE]: "md:w-[calc(24%-0.375rem)]",
};

/** One discrete transition per mode change: the width reflow animates
 * over FOCUS_TRANSITION_MS; the explicit re-snap that follows it (the
 * expanded column fully in view) is scheduled just past the transition
 * so it targets the SETTLED geometry, never a moving snap area. */
const FOCUS_TRANSITION_MS = 200;
const FOCUS_RESNAP_DELAY_MS = FOCUS_TRANSITION_MS + 20;

/** The wrapper every column wears: snap-start makes each column a snap
 * point (the snap points ARE the navigation — swipe and arrow keys
 * land column-to-column); shrink-0 + the min-width floor keep the
 * column's width through every drain state. The PORTRAIT floor is
 * 4.5rem — deliberately BELOW the calc width at every real phone
 * width (≥320px viewports), so it never disturbs the exact 95/5
 * split; it only guards degenerate sub-phone narrowness. md:grow
 * fills the row at landscape/desktop so the fourth column never
 * leaves a ragged edge (grow only distributes free space — it cannot
 * compress a shrink-0 column); the focus-visible ring is the #2021
 * keyboard-parity affordance (arrow keys navigate columns, focus
 * visible). */
const STAGE_COL_BASE_CLASS =
  "flex min-w-[4.5rem] shrink-0 snap-start flex-col rounded-md outline-none transition-[width] duration-200 ease-out focus-visible:outline-2 focus-visible:outline-primary/60 focus-visible:-outline-offset-2 md:min-w-[7.5rem] md:grow";

interface StageRailProps {
  /** The swimlane's short id — the rail's testid keeps the
   * stage-grid-<lane> name the e2e suite already pins. */
  laneShortId: string;
  /** Everything StageCell needs except the per-column stage and drop
   * handler (both derive from the mapped stage). */
  cellProps: Omit<StageCellProps, "stage" | "onDrop">;
  /** Builds the per-column drop handler (the engine's arbiter surface
   * — unchanged by the rail; drops are event-target based). */
  onDropStage: (stage: BoardStage) => (e: React.DragEvent) => void;
}

/** StageRail — the 1x4 pipeline (owner direction 2026-11-15): all four
 * through-flow columns on ONE row inside a horizontal scroll-snap
 * container, left-to-right in MAIN_STAGES order. Keyboard parity per
 * the #2021 a11y discipline: the columns are a roving-tabindex group —
 * exactly one column is the rail's tab stop; ArrowLeft/ArrowRight move
 * focus column-to-column and snap-scroll the target into view.
 *
 * Focus-column mode (owner direction 2026-11-15 fast-follow, portrait
 * only): FOCUS FOLLOWS SNAP-SETTLED, NEVER LIVE SWIPE POSITION — no
 * scroll event ever mutates geometry; the ONLY width-changing moments
 * are the user-initiated mode actions (chevron/Enter/Escape/pinch) and
 * the snap-settled transit (the native scrollend fires once scrolling
 * has fully stopped, so an in-flight gesture can never meet a reflow —
 * the frozen-geometry pin). Every mode change animates as ONE discrete
 * width transition, then explicitly re-snaps past the transition so
 * the expanded column is fully in view. OVERVIEW IS THE DEFAULT AND
 * THE BOARD: every mount starts in overview, focus is component state
 * only — never persisted, never restored. */
function StageRail({ laneShortId, cellProps, onDropStage }: StageRailProps) {
  const [activeIndex, setActiveIndex] = useState(0);
  // null = overview (the default on every mount); a number = focus
  // mode on MAIN_STAGES[index].
  const [focusedIndex, setFocusedIndex] = useState<number | null>(null);
  const [modeNotice, setModeNotice] = useState("");
  const colRefs = useRef<Array<HTMLDivElement | null>>([]);
  const railRef = useRef<HTMLDivElement | null>(null);
  // The scrollend listener and the pinch handlers read live state
  // without re-subscribing — the ref mirrors focusedIndex.
  const focusedRef = useRef<number | null>(null);
  focusedRef.current = focusedIndex;
  // mdPlus mirrored for the callbacks (enter guarded at md+ — the
  // chevrons are md:hidden; keyboard Enter mirrors the affordance).
  const mdPlusRef = useRef(false);
  const resnapTimer = useRef<number | undefined>(undefined);
  const pinchStartDist = useRef<number | null>(null);

  useEffect(() => () => window.clearTimeout(resnapTimer.current), []);

  const scrollColumnToStart = useCallback((index: number) => {
    colRefs.current[index]?.scrollIntoView({ behavior: "smooth", inline: "start", block: "nearest" });
  }, []);

  /** Enter focus on a column — or TRANSIT to it when focus mode is
   * already active (the same state write; both are one user action).
   * Discrete states, one transition: the width classes change here,
   * and the explicit re-snap is scheduled just past the transition so
   * it targets the settled geometry. Portrait-only: a no-op at md+
   * (the mode does not exist there — keyboard Enter mirrors the
   * md:hidden chevron, so it cannot strand a lens across a rotation). */
  const applyFocus = useCallback(
    (index: number) => {
      if (mdPlusRef.current) return;
      setFocusedIndex(index);
      setActiveIndex(index);
      setModeNotice(`Focused ${STAGE_LABELS[MAIN_STAGES[index]!]} column`);
      colRefs.current[index]?.focus({ preventScroll: true });
      window.clearTimeout(resnapTimer.current);
      resnapTimer.current = window.setTimeout(() => scrollColumnToStart(index), FOCUS_RESNAP_DELAY_MS);
    },
    [scrollColumnToStart],
  );

  /** Exit to overview. The overview RE-CENTERS on the column just
   * left — the re-snap targets it under the restored overview
   * geometry (no jump to position zero). */
  const exitFocus = useCallback(() => {
    const last = focusedRef.current;
    if (last === null) return;
    setFocusedIndex(null);
    setModeNotice(`Overview — ${STAGE_LABELS[MAIN_STAGES[last]!]} column centered`);
    window.clearTimeout(resnapTimer.current);
    resnapTimer.current = window.setTimeout(() => scrollColumnToStart(last), FOCUS_RESNAP_DELAY_MS);
  }, [scrollColumnToStart]);

  const focusColumn = useCallback(
    (index: number) => {
      setActiveIndex(index);
      const el = colRefs.current[index];
      if (el) {
        el.focus({ preventScroll: true });
        el.scrollIntoView({ behavior: "smooth", inline: "start", block: "nearest" });
      }
      // Arrows in focus mode TRANSIT the lens to the adjacent column
      // (the carousel feel); in overview they only move the roving
      // stop. Swiping never changes mode — these are key transits.
      if (focusedRef.current !== null && focusedRef.current !== index) applyFocus(index);
    },
    [applyFocus],
  );

  // Snap-settled transit: the native scrollend (fired only once
  // scrolling — gesture AND snap animation — has fully stopped) is
  // the one scroll-derived moment allowed to move focus. The settled
  // column is the one whose start sits nearest the snapport origin.
  // Settling back on the already-focused column is a no-op.
  useEffect(() => {
    const rail = railRef.current;
    if (!rail) return;
    const onScrollEnd = () => {
      const focused = focusedRef.current;
      if (focused === null) return;
      const padLeft = parseFloat(getComputedStyle(rail).paddingLeft) || 0;
      const snapportStart = rail.scrollLeft + padLeft;
      let best = focused;
      let bestDist = Infinity;
      colRefs.current.forEach((el, i) => {
        if (!el) return;
        // Content offsets (offsetLeft is layout-stable — unlike rects,
        // it never moves under a scroll).
        const start = el.offsetLeft - rail.offsetLeft;
        const dist = Math.abs(start - snapportStart);
        if (dist < bestDist) {
          bestDist = dist;
          best = i;
        }
      });
      if (best !== focused) applyFocus(best);
    };
    rail.addEventListener("scrollend", onScrollEnd);
    return () => rail.removeEventListener("scrollend", onScrollEnd);
  }, [applyFocus]);

  // Landscape/desktop has NO focus mode — and the reset must not
  // depend on a single addEventListener change event (owner report #2,
  // 2026-11-17: a lens that survives the rotation leaves BOTH
  // orientations at the wrong sizes). mdPlus is STATE, re-read at
  // mount and re-read again on EVERY clock that can observe a
  // viewport-class crossing (owner report #3, same day: the .16
  // single-clock shape still left the md row at the mercy of one
  // DELIVERED matchMedia change — a rotation whose change event is
  // lost to the renderer's timing (the frozen/hidden-page window,
  // coalesced delivery on rotation) leaves mdPlus stale and the
  // portrait layout stranded at md+ and back). Three clocks, one
  // idempotent read: the modern addEventListener path, the legacy
  // addListener arm (pre-Safari-14), AND window resize +
  // orientationchange — every real rotation fires resize on both
  // engines, so a lost mq event heals on the very next resize frame.
  // The RENDER additionally treats md+ as "no lens can show"
  // (effectiveFocused), and the focus widths carry their own md:
  // variants (STAGE_COL_FOCUS_MD_WIDTH) so even a stale state cannot
  // render portrait geometry at md+. No poll: the clocks are the
  // events browsers guarantee alongside a viewport change.
  const [mdPlus, setMdPlus] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia?.("(min-width: 768px)");
    if (!mq) return;
    const apply = () => setMdPlus(mq.matches);
    apply();
    const offs: Array<() => void> = [];
    if (typeof mq.addEventListener === "function") {
      mq.addEventListener("change", apply);
      offs.push(() => mq.removeEventListener("change", apply));
    } else if (typeof mq.addListener === "function") {
      // Legacy Safari (< 14): the deprecated addListener path.
      mq.addListener(apply);
      offs.push(() => mq.removeListener(apply));
    }
    // The rotation clocks (owner report #3): the viewport-class truth
    // re-reads the media state on resize and orientationchange — the
    // heal for an undelivered matchMedia change. addEventListener's
    // passive default is fine (the handler only writes state).
    window.addEventListener("resize", apply);
    window.addEventListener("orientationchange", apply);
    offs.push(() => {
      window.removeEventListener("resize", apply);
      window.removeEventListener("orientationchange", apply);
    });
    return () => {
      offs.forEach((off) => off());
    };
  }, []);
  // The render-level guard: at md+ the effective lens is ALWAYS null.
  const effectiveFocused = mdPlus ? null : focusedIndex;
  mdPlusRef.current = mdPlus;
  // The crossing effect: flips only (the ref gates the mount run, and
  // setMdPlus bails on unchanged values so this runs on crossings,
  // not on every clock tick). Two arms:
  //  - crossing INTO md+ exits an open lens (chevrons are md:hidden —
  //    there is no way back into focus from md+);
  //  - crossing INTO portrait RE-OWNERS the rail's scroll position:
  //    the browser's rotation scroll-restoration replays the old
  //    CONTENT offset against the CHANGED column widths (the md row
  //    ran in between), which lands the snapport off every column's
  //    valid snap range — nothing in CSS re-snaps a restored scroll.
  //    The rail re-derives its snap target from live geometry once
  //    the width transition has settled (FOCUS_RESNAP_DELAY_MS), the
  //    same "target the settled geometry, never a moving snap area"
  //    contract every other mode change honors.
  const prevMdPlus = useRef(false);
  useEffect(() => {
    const crossed = prevMdPlus.current !== mdPlus;
    prevMdPlus.current = mdPlus;
    if (!crossed) return;
    if (mdPlus) {
      if (focusedRef.current !== null) exitFocus();
      return;
    }
    window.clearTimeout(resnapTimer.current);
    resnapTimer.current = window.setTimeout(() => {
      const rail = railRef.current;
      if (!rail) return;
      const padLeft = parseFloat(getComputedStyle(rail).paddingLeft) || 0;
      const snapportStart = rail.scrollLeft + padLeft;
      let best = 0;
      let bestDist = Infinity;
      colRefs.current.forEach((el, i) => {
        if (!el) return;
        // Content offsets (layout-stable under scrolls, live after the
        // re-layout — the same measurement the scrollend transit uses).
        const start = el.offsetLeft - rail.offsetLeft;
        const dist = Math.abs(start - snapportStart);
        if (dist < bestDist) {
          bestDist = dist;
          best = i;
        }
      });
      // Already on the column's start: nothing to correct.
      if (bestDist > 1) scrollColumnToStart(best);
    }, FOCUS_RESNAP_DELAY_MS);
  }, [mdPlus, exitFocus, scrollColumnToStart]);

  /** Pinch-out exits focus (zoom-out muscle memory): two touches whose
   * spread grows by >25% mid-gesture — once per gesture. */
  const pinchSpread = (e: React.TouchEvent) =>
    Math.hypot(e.touches[0]!.clientX - e.touches[1]!.clientX, e.touches[0]!.clientY - e.touches[1]!.clientY);

  const handleColumnKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>, index: number) => {
      // Overlays own their keys (r1): menus and dialogs render through
      // portals but their React keydowns still propagate through the
      // column's tree — Escape inside one must reach the overlay, not
      // exit the lens.
      const origin = e.target as Element | null;
      const inOverlay = origin?.closest?.('[role="menu"], [role="dialog"]');
      if (e.key === "Escape") {
        if (!inOverlay && focusedRef.current !== null) exitFocus();
        return;
      }
      // Enter on the column itself (never on a card or button inside
      // it) enters focus on the active column — arrows + Enter.
      if (e.key === "Enter" && e.target === e.currentTarget && focusedRef.current === null) {
        e.preventDefault();
        applyFocus(index);
        return;
      }
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      if (origin?.closest?.('[role="menu"]')) return;
      e.preventDefault();
      const next = e.key === "ArrowRight" ? Math.min(index + 1, MAIN_STAGES.length - 1) : Math.max(index - 1, 0);
      if (next === index) return;
      focusColumn(next);
    },
    [applyFocus, exitFocus, focusColumn],
  );

  const columnMode = (index: number): "overview" | "focused" | "compressed" | "periphery" => {
    if (effectiveFocused === null) return "overview";
    if (index === effectiveFocused) return "focused";
    // The non-focused done column keeps its full page (reachability)
    // — periphery: full geometry, cards visible, chevron transits.
    return MAIN_STAGES[index] === STAGE_DONE ? "periphery" : "compressed";
  };

  return (
    <div
      // scroll-px-2 (r1): CSS scroll-snap aligns snap areas to the
      // SCROLLPORT (the border box), not the content box — without the
      // matching scroll-padding, a snapped column lands flush with the
      // rail's border, 8px left of its at-rest position (measured:
      // settle at 112 for a content-offset-104 column). scroll-px-2
      // insets the snapport to the content box so snap points land
      // exactly where the resting column starts; scrollIntoView's
      // inline:"start" obeys the same inset.
      className="flex snap-x snap-mandatory gap-2 overflow-x-auto scroll-px-2 p-2"
      data-testid={`stage-grid-${laneShortId}`}
      data-stage-rail="true"
      data-mode={effectiveFocused === null ? "overview" : "focus"}
      role="region"
      aria-label="Pipeline stages — use the left and right arrow keys to move between columns; Enter focuses a column; Escape returns to overview"
      ref={railRef}
      onTouchStart={(e) => {
        if (focusedRef.current === null || e.touches.length !== 2) return;
        pinchStartDist.current = pinchSpread(e);
      }}
      onTouchMove={(e) => {
        if (pinchStartDist.current === null || e.touches.length !== 2) return;
        if (pinchSpread(e) > pinchStartDist.current * 1.25) {
          pinchStartDist.current = null;
          exitFocus();
        }
      }}
      onTouchEnd={() => {
        pinchStartDist.current = null;
      }}
      onTouchCancel={() => {
        pinchStartDist.current = null;
      }}
    >
      {/* Mode changes announced to screen readers (#2021 a11y
       * discipline): a polite live region carrying the lens state.
       * sr-only is absolutely positioned — it never participates in
       * the flex row. */}
      <span data-testid={`rail-mode-notice-${laneShortId}`} role="status" aria-live="polite" className="sr-only">
        {modeNotice}
      </span>
      {MAIN_STAGES.map((stage, index) => {
        const colMode = columnMode(index);
        return (
          <div
            key={stage}
            ref={(el) => {
              colRefs.current[index] = el;
            }}
            tabIndex={index === activeIndex ? 0 : -1}
            role="group"
            aria-label={`${STAGE_LABELS[stage]} column`}
            data-testid={`stage-col-${stage}`}
            data-stage-col={stage}
            data-col-state={colMode}
            onKeyDown={(e) => handleColumnKeyDown(e, index)}
            className={cn(
              STAGE_COL_BASE_CLASS,
              effectiveFocused === null
                ? STAGE_COL_WIDTH[stage]
                : index === effectiveFocused
                  ? stage === STAGE_DONE
                    ? STAGE_COL_FOCUS_WIDTH.focusedDone
                    : STAGE_COL_FOCUS_WIDTH.focused
                  : stage === STAGE_DONE
                    ? STAGE_COL_WIDTH[STAGE_DONE]
                    : STAGE_COL_FOCUS_WIDTH.compressed,
              // The CSS viewport-class authority: a column in ANY focus
              // state still carries its overview md: width, so the md
              // row renders correctly even when the mdPlus state behind
              // effectiveFocused is stale (owner report #3 — the lens
              // must never be able to hold portrait geometry at md+).
              effectiveFocused !== null && STAGE_COL_FOCUS_MD_WIDTH[stage],
              // The compressed floor replaces the 4.5rem overview floor
              // (cn is tailwind-merge): a compressed column is a stub.
              effectiveFocused !== null && index !== effectiveFocused && stage !== STAGE_DONE && "min-w-[3rem]",
            )}
          >
            <StageCell
              stage={stage}
              onDrop={onDropStage(stage)}
              {...cellProps}
              colMode={colMode}
              onFocusToggle={
                effectiveFocused === null
                  ? () => applyFocus(index)
                  : index === effectiveFocused
                    ? exitFocus
                    : () => applyFocus(index)
              }
            />
          </div>
        );
      })}
    </div>
  );
}

interface StageCellProps {
  stage: BoardStage;
  projectTasks: BoardTask[];
  allProjectTasks: BoardTask[];
  /** The swimlane project's target workspace — the cards' assignee
   * session links' chat-route segment (2026-10-29 owner ruling). */
  workspaceId?: string;
  stageOf: (t: BoardTask) => BoardStage;
  optimistic: OptimisticMap;
  refusedShortId: string | null;
  /** The in-flight drag's source stage — gates the affordance highlight
   * (the manager-surface mirror); the drop still attempts (the engine
   * arbitrates and its 409 renders). */
  draggingStage: string | null;
  /** #1832 item 2: the swimlane's selected epic id — cards in OTHER
   * epics dim, cards in this epic highlight (display-only scope). */
  highlightEpicId: string | null;
  onDrop: (e: React.DragEvent) => void;
  onOpenCard: (t: BoardTask) => void;
  onCardDetails: (t: BoardTask) => void;
  onCardDragStart: (t: BoardTask) => void;
  onCardDragEnd: () => void;
  onCardStart: (t: BoardTask) => void;
  /** The card kebab's full item list (owner ask 2026-11-14) — built in
   * page scope so every action closes over the real handlers; includes
   * Delete (the former inline item). */
  storyKebabItems: (t: BoardTask) => KebabMenuItem[];
  /** Applies the kebab actions' per-card optimistic overrides (paused
   * badge, criterion glyphs) ahead of the server's row. */
  decorateTask: (t: BoardTask) => BoardTask;
  startingShortId: string | null;
  /** Owner ask 2026-11-13: the lane's short id — scopes the Done
   * column's show-all affordance (one per swimlane). */
  laneShortId?: string;
  /** Owner ask 2026-11-13: opens the Done drawer for this lane (the
   * Done column's overflow surface). Invoked only by the done branch. */
  onShowAllDone?: () => void;
  /** Focus-column mode (owner direction 2026-11-15 fast-follow,
   * PORTRAIT only): the column's lens state. undefined = no affordance
   * (the side columns and any non-rail caller); "overview" renders the
   * expand chevron, "focused" the collapsing one, "compressed" the
   * stub (cards hidden, header + chevron remain the transit tap), and
   * "periphery" the non-focused done's full page (reachability keeps
   * it wide; cards visible, chevron still transits). */
  colMode?: "overview" | "focused" | "compressed" | "periphery";
  /** The one-action mode lever: chevron tap in every state, plus the
   * compressed header's tap-to-transit. */
  onFocusToggle?: () => void;
}

function StageCell({
  stage,
  projectTasks,
  allProjectTasks,
  workspaceId,
  stageOf,
  optimistic,
  refusedShortId,
  draggingStage,
  highlightEpicId,
  onDrop,
  onOpenCard,
  onCardDetails,
  onCardDragStart,
  onCardDragEnd,
  onCardStart,
  storyKebabItems,
  decorateTask,
  startingShortId,
  laneShortId,
  onShowAllDone,
  colMode,
  onFocusToggle,
}: StageCellProps) {
  const [dragOver, setDragOver] = useState(false);
  // Focus-column mode: a compressed neighbor is a navigation stub —
  // its header row itself transits (the bigger tap target on a thin
  // column); the focused column's header never toggles (only its
  // chevron does — cards keep their click targets).
  const compressed = colMode === "compressed";
  const cards = projectTasks.filter((t) => stageOf(t) === stage);
  // Owner ask 2026-11-13 — the Done column's render set: NEWEST-DONE
  // first (the server list arrives created_at-asc, reads.go; the
  // recency order is applied view-side), capped at
  // DONE_COLUMN_MAX_VISIBLE inline cards; the rest live one click
  // deeper in the show-all drawer. Every other column keeps the
  // server's list order verbatim; at ≤N the done column renders every
  // card (no cap, no affordance — the regression pin).
  let visibleCards = cards;
  let doneOverflow = 0;
  if (stage === STAGE_DONE) {
    const ordered = sortDoneNewestFirst(cards);
    doneOverflow = Math.max(0, ordered.length - DONE_COLUMN_MAX_VISIBLE);
    visibleCards = doneOverflow > 0 ? ordered.slice(0, DONE_COLUMN_MAX_VISIBLE) : ordered;
  }
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
      <div
        data-stage-header={stage}
        className={cn("flex items-center justify-between gap-1 px-0.5", compressed && "cursor-pointer")}
        onClick={compressed ? onFocusToggle : undefined}
      >
        <span className={cn("truncate text-[10px] font-medium uppercase tracking-wide text-muted-foreground", draggingStage !== null && !landsHere && "line-through opacity-60")}>
          {STAGE_LABELS[stage]}
        </span>
        <span className="shrink-0 text-[10px] text-muted-foreground">{cards.length}</span>
        {/* The focus affordance (fast-follow): a visible expand chevron
         * in the header — discoverable, never a hidden gesture. Portrait
         * only (md:hidden): landscape/desktop shows all four columns,
         * there is nothing to expand. aria-expanded names the lens state
         * of THIS column (#2021). */}
        {colMode != null && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onFocusToggle?.();
            }}
            aria-expanded={colMode === "focused"}
            aria-label={colMode === "focused" ? `Exit focus — return to overview from the ${STAGE_LABELS[stage]} column` : `Focus the ${STAGE_LABELS[stage]} column`}
            title={colMode === "focused" ? "Exit to overview" : "Focus this column"}
            data-testid={`col-focus-toggle-${stage}`}
            className="shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-primary/60 md:hidden"
          >
            {colMode === "focused" ? (
              <ChevronsRightLeft className="h-3.5 w-3.5" aria-hidden />
            ) : (
              <ChevronsLeftRight className="h-3.5 w-3.5" aria-hidden />
            )}
          </button>
        )}
      </div>
      {/* Compressed neighbors hide their cards (a ~3rem stub cannot
       * render a readable card; the header carries the column). The
       * drop zone itself stays armed — drops still arbitrate. */}
      <div className={cn("flex flex-col gap-1.5", compressed && "hidden")}>
        {visibleCards.map((t) => (
          <StoryCard
            key={t.short_id}
            task={decorateTask(t)}
            projectTasks={allProjectTasks}
            workspaceId={workspaceId}
            pending={optimistic[t.short_id] !== undefined}
            refused={refusedShortId === t.short_id}
            dimmed={highlightEpicId !== null && t.epic_id !== highlightEpicId}
            highlighted={highlightEpicId !== null && t.epic_id === highlightEpicId}
            starting={startingShortId === t.short_id}
            onStart={onCardStart}
            onOpen={onOpenCard}
            onDetails={onCardDetails}
            kebabItems={storyKebabItems(t)}
            onDragStart={onCardDragStart}
            onDragEnd={onCardDragEnd}
          />
        ))}
      {/* Owner ask 2026-11-13: the Done column's collapsed affordance —
       * exactly N inline + this control; a native button (keyboard
       * reachable, aria-haspopup names the dialog it opens). The count
       * is the lane's FULL done set (the header badge stays the same
       * truth — the cap is visual, nothing is dropped). */}
      {doneOverflow > 0 && (
        <button
          type="button"
          onClick={() => onShowAllDone?.()}
          aria-haspopup="dialog"
          title={`Open the done drawer — every completed story in this lane, newest first`}
          data-testid={`done-show-all-${laneShortId}`}
          className="rounded-md border border-border bg-muted/40 px-2 py-1 text-[10px] font-medium text-muted-foreground transition-colors hover:border-primary/40 hover:text-foreground"
        >
          Show all {cards.length} done
        </button>
      )}
      {cards.length === 0 && <span className="px-1 text-[10px] text-muted-foreground/60">—</span>}
      </div>
    </div>
  );
}
