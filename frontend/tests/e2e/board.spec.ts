/**
 * #1702 — Task Board UI e2e (design 0067 §4.8, Lane 4).
 *
 * Rows:
 *   1. Board renders: swimlanes = projects, columns = stages, cards
 *      titled by their human title (2026-10-28 ruling 4: no raw ids in
 *      the chrome) with pause-waiting chips / tags / blocked badge.
 *   2. Drag = server-arbitrated move_stage: optimistic move + confirm;
 *      refused drag (409 + guard code) snaps back with the code rendered.
 *   3. Done drop collects the evidence URL (required on → done, R2).
 *   4. SSE live update: a board.event on the user stream moves the card
 *      (fetch monkey-patch streams frames — route.fulfill cannot stream;
 *      the known limitation documented in chat-scroll-tail.spec.ts).
 *   5. Scope switcher: project focus + real filters + search did-you-mean
 *      (2026-10-28 ruling 1: the epic pills are gone — the epic ROWS'
 *      click-highlight is the epic→task path).
 *   6. OR4 pr:N tag validation on the task editor.
 *
 * All backend APIs are mocked via Playwright route interception, the SSE
 * endpoints through the established fetch monkey-patch (per-test emitters).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-board-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";
const PROJECT_B = "prj-bbbb2222";

// --- fixtures -------------------------------------------------------------------

type E2ETask = {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
  stage: string;
  /** 000050: the awaiting-input FLAG riding the real stage (the wire
   * never carries pending_input as a stage anymore). */
  pending_input?: boolean;
  pending_origin?: string;
  paused: boolean;
  project_name: string;
  epic_name: string;
  tags: string[];
  acceptance_criteria: Array<{ text: string; met: boolean }>;
  is_blocked: boolean;
  blocked_by?: string[];
  assignee_session?: string;
  attempts: number;
  claim_incremented_attempts: boolean;
  /** #1813: the failed-state overlay's row flag (drives the FAILED
   * drawer filter and the card's FAILED badge). */
  last_attempt_failed?: boolean;
  failure_count?: number;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
};

function task(partial: Partial<E2ETask> & { short_id: string; title: string; stage: string; project_id: string; epic_id: string }): E2ETask {
  return {
    id: `uuid-${partial.short_id}`,
    description: "",
    paused: false,
    project_name: partial.project_id === PROJECT_A ? "Apollo" : "Borealis",
    epic_name: partial.epic_id === "epic-1" ? "E1" : "E2",
    tags: [],
    acceptance_criteria: [],
    is_blocked: false,
    attempts: 0,
    claim_incremented_attempts: false,
    last_attempt_failed: false,
    failure_count: 0,
    stage_entered_at: "2026-10-10T00:00:00Z",
    created_at: "2026-10-10T00:00:00Z",
    updated_at: "2026-10-10T00:00:00Z",
    ...partial,
  };
}

interface BoardState {
  tasks: E2ETask[];
  /** #1832 r2: epics that exist ONLY on the server (zero task rows). */
  epics: E2EEpic[];
  /** Per-test board overrides (e.g. a third project for the picker row). */
  projects?: Array<{ id: string; short_id: string; name: string; worker_slots: number; user_id?: string; target_workspace_id?: string; tags?: string[]; status?: string; created_at?: string; updated_at?: string }>;
  /** Unhappy-path levers: force the two creation POSTs to 500. */
  failEpicCreate?: boolean;
  failTaskCreate?: boolean;
}

type E2EEpic = {
  id: string;
  project_id: string;
  short_id: string;
  title: string;
  outcome: string;
  status: string;
  tags: string[];
  created_at: string;
  updated_at: string;
  progress_done: number;
  progress_total: number;
  progress_derived: boolean;
};

/** #1831 wire contract: the wire addresses epics by their DERIVED short
 * id (epc-<compact-id>). The ONE derivation, shared by the epic-list
 * mapper and the epic-delete route's guard arms — two inline copies
 * would drift and silently unmatch the guards (r2 review note). */
function wireEpicShortId(id: string): string {
  return `epc-${id.replace(/-/g, "").padEnd(8, "0")}`;
}

/** Server-side ListEpics: the task-backed epics derived from rows (the
 * read the UI used to guess client-side) — progress scoped PER PROJECT
 * (a same-id epic spanning projects is one row per project, matching
 * the engine's per-project list). */
function derivedEpicsFromTasks(tasks: E2ETask[]): E2EEpic[] {
  const byKey = new Map<string, E2EEpic>();
  for (const t of tasks) {
    const key = `${t.project_id}/${t.epic_id}`;
    const existing = byKey.get(key);
    if (existing) {
      existing.progress_total += 1;
      if (t.stage === "done") existing.progress_done += 1;
      continue;
    }
    byKey.set(key, {
      id: t.epic_id, project_id: t.project_id, short_id: wireEpicShortId(t.epic_id),
      title: t.epic_name, outcome: "", status: "active", tags: [],
      created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z",
      progress_done: t.stage === "done" ? 1 : 0, progress_total: 1, progress_derived: false,
    });
  }
  return [...byKey.values()];
}

function freshState(): BoardState {
  return {
    projects: [
      { id: PROJECT_A, short_id: PROJECT_A, user_id: "u1", name: "Apollo", target_workspace_id: WORKSPACE_ID, worker_slots: 2, tags: [], status: "active", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" },
      { id: PROJECT_B, short_id: PROJECT_B, user_id: "u1", name: "Borealis", target_workspace_id: WORKSPACE_ID, worker_slots: 1, tags: [], status: "active", created_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:00Z" },
    ],
    epics: [],
    tasks: [
      task({ short_id: "tsk-aaaa0001", title: "Write charter", stage: "backlog", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({
        short_id: "tsk-bbbb0002", title: "Implement engine", stage: "in_progress", project_id: PROJECT_A, epic_id: "epic-1",
        attempts: 1, acceptance_criteria: [{ text: "tests green", met: false }, { text: "docs", met: true }],
      }),
      task({ short_id: "tsk-cccc0003", title: "Review drag arm", stage: "in_review", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({ short_id: "tsk-dddd0004", title: "Ship design doc", stage: "done", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({ short_id: "tsk-eeee0005", title: "Blocked follow-up", stage: "backlog", project_id: PROJECT_A, epic_id: "epic-2", is_blocked: true, blocked_by: ["tsk-cccc0003"], tags: ["pr:1702"] }),
      task({ short_id: "tsk-ffff0006", title: "Waiting on human", stage: "in_progress", pending_input: true, pending_origin: "worker", project_id: PROJECT_B, epic_id: "epic-1", attempts: 3 }),
    ],
  };
}

// --- SSE emitters (the chat-scroll-tail fetch monkey-patch shape) ----------------

async function installSSEEmitters(page: Page) {
  await page.addInitScript(() => {
    const enc = new TextEncoder();
    const userCtrls = new Set<ReadableStreamDefaultController<Uint8Array>>();
    (window as unknown as Record<string, unknown>).__user = (frame: string) => {
      for (const c of userCtrls) {
        try {
          c.enqueue(enc.encode(frame));
        } catch {
          /* cancelled connection */
        }
      }
    };
    const origFetch = window.fetch.bind(window);
    window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/events")) {
        let ctl: ReadableStreamDefaultController<Uint8Array> | null = null;
        const stream = new ReadableStream<Uint8Array>({
          start(c) {
            ctl = c;
            userCtrls.add(c);
          },
          cancel: () => {
            if (ctl) userCtrls.delete(ctl);
          },
        });
        void stream;
        return new Response(stream, {
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
        });
      }
      return origFetch(input as RequestInfo, init);
    };
  });
}

// --- REST mocks -------------------------------------------------------------------

async function setupBoardMocks(page: Page, state: BoardState) {
  const j = (body: unknown, status = 200) => ({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill(j({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true })));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill(j({ registrationEnabled: false, oidcEnabled: false })));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") {
      void r.continue();
      return;
    }
    void r.fulfill(j({
      items: [{ id: WORKSPACE_ID, name: "Board E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    }));
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill(j({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } })));

  // Board REST surface (#1699): projects list + per-project tasks +
  // move-stage. The move-stage mock mutates the shared state so the
  // post-invalidation refetch returns the authoritative row.
  await page.route(`${API}/me/projects`, (r: Route) =>
    r.fulfill(j({
      projects: state.projects ?? [
        { id: PROJECT_A, short_id: PROJECT_A, user_id: "u1", name: "Apollo", target_workspace_id: WORKSPACE_ID, worker_slots: 2, tags: [], status: "active", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" },
        { id: PROJECT_B, short_id: PROJECT_B, user_id: "u1", name: "Borealis", target_workspace_id: WORKSPACE_ID, worker_slots: 1, tags: [], status: "active", created_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:00Z" },
      ],
    })));
  // #1832 item 3: project kebab — archive/unarchive ONLY (#1747:
  // project delete is eliminated; no DELETE route exists at all).
  for (const lifecycle of ["archive", "unarchive"]) {
    await page.route(`${API}/me/projects/*/${lifecycle}`, (r: Route) => {
      if (r.request().method() !== "POST") {
        void r.continue();
        return;
      }
      const url = new URL(r.request().url());
      const pid = url.pathname.split("/")[url.pathname.split("/").indexOf("projects") + 1] ?? "";
      const project = (state.projects ?? []).find((p) => p.id === pid || p.short_id === pid);
      if (!project) {
        void r.fulfill(j({ error: `board: project "${pid}" not found` }, 404));
        return;
      }
      project.status = lifecycle === "archive" ? "archived" : "active";
      void r.fulfill(j(project));
    });
  }
  // #1832 item 3: epic delete — refuses while claims are in flight
  // under the epic unless ?force=true (force deletes, then aborts each
  // in-flight worker post-commit). Matches pkg/board/delete.go.
  await page.route(`${API}/me/epics/*`, async (r: Route) => {
    if (r.request().method() !== "DELETE") {
      void r.continue();
      return;
    }
    const url = new URL(r.request().url());
    const epicId = url.pathname.split("/epics/")[1]?.split("/")[0] ?? "";
    const force = url.searchParams.get("force") === "true";
    // #1831 wire contract: the wire addresses epics by their derived
    // short id — the ONE shared derivation maps the fixture's epic ids
    // to wire ids so the guard arms see the fixture's rows.
    const under = state.tasks.filter((t) => wireEpicShortId(t.epic_id) === epicId);
    const inFlight = under.filter((t) => t.stage === "in_progress" && t.assignee_session);
    if (inFlight.length > 0 && !force) {
      void r.fulfill(j({
        error: "board: guard epic_state: claims in flight under the epic — force required (force=true deletes, then aborts each in-flight worker post-commit)",
        code: "epic_state",
      }, 409));
      return;
    }
    // Cross-epic dependents outside the blast radius are orphaned.
    const cascade = under.map((t) => t.short_id);
    const orphaned: string[] = [];
    for (const t of under) {
      for (const dep of state.tasks.filter((x) => x.epic_id !== epicId && (x.blocked_by ?? []).includes(t.short_id))) {
        dep.blocked_by = (dep.blocked_by ?? []).filter((id) => id !== t.short_id);
        dep.is_blocked = (dep.blocked_by ?? []).length > 0;
        orphaned.push(dep.short_id);
      }
    }
    state.tasks = state.tasks.filter((t) => wireEpicShortId(t.epic_id) !== epicId);
    void r.fulfill(j({
      item_kind: "epic", id: epicId, short_id: `epc-${epicId.slice(-6)}`,
      cascade_tasks: cascade, orphaned_dependents: orphaned,
    }));
  });

  await page.route(`${API}/me/projects/*/tasks*`, (r: Route) => {
    const url = new URL(r.request().url());
    const pid = url.pathname.split("/").pop() === "tasks" ? url.pathname.split("/")[url.pathname.split("/").indexOf("projects") + 1] : "";
    void r.fulfill(j({ tasks: state.tasks.filter((t) => t.project_id === pid) }));
  });
  // #1832 item 1: contextual epic creation — POST /me/projects/:id/epics.
  // The route echoes the project the button carried (asserted per-test).
  // #1832 r2 finding 1: GET /me/projects/:id/epics — the server's epic
  // LIST (task-backed rows derived server-side + this state's zero-task
  // epics): the read that keeps zero-task epics visible after reload.
  await page.route(`${API}/me/projects/*/epics`, async (r: Route) => {
    const url = new URL(r.request().url());
    const pid = url.pathname.split("/")[url.pathname.split("/").indexOf("projects") + 1] ?? "";
    if (r.request().method() === "GET") {
      // Faithful to ListEpics: zero-task epics from state ride AFTER the
      // task-derived rows, so live row-derived progress (the engine's
      // own derivation) WINS for any epic that has tasks (r3 finding 2).
      const byId = new Map<string, E2EEpic>();
      for (const e of state.epics) if (e.project_id === pid) byId.set(e.id, e);
      for (const e of derivedEpicsFromTasks(state.tasks)) if (e.project_id === pid) byId.set(e.id, e);
      void r.fulfill(j({ epics: [...byId.values()] }));
      return;
    }
    if (r.request().method() !== "POST") {
      void r.continue();
      return;
    }
    if (state.failEpicCreate) {
      void r.fulfill(j({ error: "board: epic store unavailable", code: "internal" }, 500));
      return;
    }
    const body = r.request().postDataJSON() as { title?: string; outcome?: string };
    void r.fulfill(j({
      id: `epic-created-${pid}`, project_id: pid, short_id: `epc-new${pid.slice(-4)}`,
      title: body.title ?? "", outcome: body.outcome ?? "", status: "draft", tags: [],
      created_at: "2026-10-22T00:00:00Z", updated_at: "2026-10-22T00:00:00Z",
      progress_done: 0, progress_total: 0, progress_derived: false,
    }));
    // The created epic becomes server truth: zero task rows, but it
    // EXISTS — the reload read must keep serving it.
    state.epics.push({
      id: `epic-created-${pid}`, project_id: pid, short_id: `epc-new${pid.slice(-4)}`,
      title: body.title ?? "", outcome: body.outcome ?? "", status: "draft", tags: [],
      created_at: "2026-10-22T00:00:00Z", updated_at: "2026-10-22T00:00:00Z",
      progress_done: 0, progress_total: 0, progress_derived: false,
    });
  });
  // #1832 item 2: epic-scoped task creation — POST /me/epics/:id/tasks.
  // Appends to the shared state so the post-invalidation refetch shows
  // the new row in the backlog column and beneath the epic band.
  await page.route(`${API}/me/epics/*/tasks`, async (r: Route) => {
    if (r.request().method() !== "POST") {
      void r.continue();
      return;
    }
    if (state.failTaskCreate) {
      void r.fulfill(j({ error: "board: task store unavailable", code: "internal" }, 500));
      return;
    }
    const epicId = r.request().url().split("/epics/")[1]?.split("/")[0] ?? "";
    const body = r.request().postDataJSON() as { title?: string };
    const parent = state.tasks.find((t) => t.epic_id === epicId);
    // #1832 r2: a zero-task epic has no parent ROW — its project rides
    // the server's epic list (state.epics), the honest create path.
    const parentEpic = state.epics.find((e) => e.id === epicId);
    if (!parent && !parentEpic) {
      void r.fulfill(j({ error: `board: epic "${epicId}" not found` }, 404));
      return;
    }
    const projectId = parent?.project_id ?? parentEpic!.project_id;
    const projectName =
      parent?.project_name ??
      state.projects?.find((p) => p.id === projectId)?.name ??
      (projectId === PROJECT_A ? "Apollo" : "Borealis");
    const row = task({
      short_id: `tsk-new${String(state.tasks.length + 1).padStart(4, "0")}`,
      title: body.title ?? "New task",
      stage: "backlog",
      project_id: projectId,
      epic_id: epicId,
      epic_name: parent?.epic_name ?? parentEpic!.title,
      project_name: projectName,
    });
    state.tasks.push(row);
    void r.fulfill(j(row));
  });
  await page.route(`${API}/me/tasks/*/move-stage`, async (r: Route) => {
    const shortId = r.request().url().split("/tasks/")[1]?.split("/")[0];
    const body = r.request().postDataJSON() as { toStage: string; evidenceUrl?: string };
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (!t) {
      void r.fulfill(j({ error: `board: task "${shortId}" not found` }, 404));
      return;
    }
    // The Done guard, honestly: criteria unmet or no evidence → 409.
    if (body.toStage === "done" && (body.evidenceUrl === "" || t.acceptance_criteria.some((c) => !c.met))) {
      void r.fulfill(j({ error: "board: guard done_guard: all acceptance criteria must be met before done (ruling R2)", code: "done_guard" }, 409));
      return;
    }
    t.stage = body.toStage;
    void r.fulfill(j(t));
  });

  await page.route(`${API}/me/tasks/*`, async (r: Route) => {
    // #1747: cancel is eliminated — DELETE removes the row; the delete
    // result lists orphaned dependents (surfaced, never a refusal).
    // #1832 item 3: ?reconnect=true lets orphans inherit the deleted
    // node's direct upstream edges (pairs surface in the result).
    if (r.request().method() !== "DELETE") {
      void r.continue();
      return;
    }
    const url = new URL(r.request().url());
    const shortId = url.pathname.split("/tasks/")[1]?.split("/")[0] ?? "";
    const reconnect = url.searchParams.get("reconnect") === "true";
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (!t) {
      void r.fulfill(j({ error: `board: task "${shortId}" not found` }, 404));
      return;
    }
    const upstream = t.blocked_by ?? [];
    const orphanRows = state.tasks.filter((x) => (x.blocked_by ?? []).includes(shortId));
    const reconnected: string[] = [];
    for (const orphan of orphanRows) {
      if (reconnect) {
        for (const up of upstream) {
          if (up !== orphan.short_id && !(orphan.blocked_by ?? []).includes(up)) {
            orphan.blocked_by = [...(orphan.blocked_by ?? []), up];
            reconnected.push(`${orphan.short_id}→${up}`);
          }
        }
      } else {
        orphan.blocked_by = (orphan.blocked_by ?? []).filter((id) => id !== shortId);
      }
      orphan.is_blocked = (orphan.blocked_by ?? []).length > 0;
    }
    state.tasks = state.tasks.filter((x) => x.short_id !== shortId);
    void r.fulfill(j({
      item_kind: "task", id: t.id, short_id: t.short_id,
      orphaned_dependents: orphanRows.map((o) => o.short_id),
      ...(reconnect ? { reconnected } : {}),
    }));
  });
  await page.route(`${API}/me/tasks/*/start`, async (r: Route) => {
    const shortId = r.request().url().split("/tasks/")[1]?.split("/")[0];
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (!t) {
      void r.fulfill(j({ error: `board: task "${shortId}" not found` }, 404));
      return;
    }
    t.stage = "in_progress";
    t.attempts += 1;
    void r.fulfill(j({
      task_id: t.id, short_id: t.short_id, project_id: t.project_id, epic_id: t.epic_id,
      workspace_id: WORKSPACE_ID, claim_token: "tok-e2e", lease_expires_at: "2026-10-18T23:30:00Z",
      escalated: false,
    }));
  });

  await page.route(`${API}/me/tasks/*/dependencies`, (r: Route) => {
    void r.fulfill(j({
      error: "board: dependency cycle: tsk-aaaa0001 → tsk-cccc0003 → tsk-aaaa0001 (did you mean: [tsk-cccc0003]?)",
      code: "cycle",
      suggestions: ["tsk-cccc0003"],
    }, 409));
  });

  // Platform stream — empty body then reconnects are fine.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
}

async function openBoard(page: Page, init?: Partial<BoardState>) {
  const base = freshState();
  const state: BoardState = { tasks: base.tasks, projects: base.projects, epics: [], ...init };
  await installSSEEmitters(page);
  await setupBoardMocks(page, state);
  await page.goto("/board");
  await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
  return state;
}

// --- rows -------------------------------------------------------------------------

test.describe("Task Board UI (#1702)", () => {
  test("renders swimlanes per project, stage columns, and cards (§4.8)", async ({ page }) => {
    await openBoard(page);

    await expect(page.getByTestId(`swimlane-${PROJECT_B}`)).toBeVisible();
    const card = page.getByTestId("task-card-tsk-aaaa0001");
    await expect(card).toBeVisible();
    // 2026-10-28 ruling 4: the TITLE is the card's label — the raw
    // tsk- id never renders in the chrome (only the click-through
    // drawers carry it).
    await expect(card).toContainText("Write charter");
    await expect(card).not.toContainText("tsk-aaaa0001");

    // Stage columns in the swimlane grid.
    const laneA = page.getByTestId(`swimlane-${PROJECT_A}`);
    await expect(laneA.locator('[data-stage-drop="backlog"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="in_progress"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="in_review"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="done"]')).toBeVisible();

    // Blocked badge (structural, ruling R3) + the OR4 anchor chip.
    await expect(page.getByTestId("blocked-badge-tsk-eeee0005")).toContainText("1");
    await expect(page.getByTestId("task-card-tsk-eeee0005").locator("[data-pr-anchor]")).toHaveText("pr:1702");

    // Slots in the swimlane header (1 in_progress of 2).
    await expect(page.getByTestId(`slots-${PROJECT_A}`)).toContainText("1/2");
  });

  test("drag moves a card via move-stage (optimistic + confirmed)", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage).toBe("in_review");

    await page.dragAndDrop(
      '[data-testid="task-card-tsk-cccc0003"]',
      `[data-swimlane="${PROJECT_A}"] [data-stage-drop="in_progress"]`,
    );

    // Authoritative state after the invalidation refetch.
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "in_progress", { timeout: 8000 });
  });

  test("refused drag (409 done_guard) snaps back and renders the guard code", async ({ page }) => {
    const state = await openBoard(page);
    // tsk-bbbb0002 has an unmet criterion: the Done guard must refuse.
    const claimed = state.tasks.find((t) => t.short_id === "tsk-bbbb0002")!;
    expect(claimed.stage).toBe("in_progress");
    expect(claimed.acceptance_criteria.some((c) => !c.met)).toBe(true);

    // Make it reviewable first (in_progress → in_review is token-guarded
    // for the manager surface; move the row server-side to in_review to
    // reach the done-guarded drag arm deterministically).
    claimed.stage = "in_review";
    await page.reload();
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_review");

    await page.dragAndDrop(
      '[data-testid="task-card-tsk-bbbb0002"]',
      `[data-swimlane="${PROJECT_A}"] [data-stage-drop="done"]`,
    );

    // Evidence prompt: fill it so the guard's criteria leg is what fires.
    const evidence = page.getByTestId("evidence-url");
    await expect(evidence).toBeVisible();
    // The prompt previews the unmet criterion without vetoing the call.
    await expect(page.getByTestId("done-readiness-view")).toContainText("tests green");
    await evidence.fill("https://github.com/lenaxia/LLMSafeSpaces/pull/1728");
    await page.getByTestId("confirm-done").click();

    // The engine's honest 409: code rendered, card snapped back.
    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "done_guard", { timeout: 8000 });
    await expect(notice).toContainText("done_guard");
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_review");
  });

  test("a board.event on the user stream moves the card (SSE live update)", async ({ page }) => {
    const state = await openBoard(page);
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "in_review");

    // The engine moved the row (worker's token-guarded write); the event
    // streams and the read model refetches.
    state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage = "done";
    await page.evaluate(() => {
      const emit = (window as unknown as { __user: (f: string) => void }).__user;
      emit(`data: ${JSON.stringify({
        type: "board.event",
        project_id: "prj-aaaa1111",
        id: "evt-e2e-1",
        item_kind: "task",
        item_id: "uuid-tsk-cccc0003",
        action: "move_stage",
        payload: { to_stage: "done" },
      })}\n\n`);
    });

    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "done", { timeout: 8000 });
  });

  test("scope switcher: project focus, NO epic pills, search did-you-mean titles", async ({ page }) => {
    await openBoard(page);

    // Project focus (2026-10-28 ruling 4: options are named by project
    // NAME — the prj- id rides the option's value, never its label).
    await page.getByTestId("project-focus").selectOption({ label: "Borealis" });
    await expect(page.getByTestId(`swimlane-${PROJECT_B}`)).toBeVisible();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toHaveCount(0);

    // Back to all projects.
    await page.getByTestId("project-focus").selectOption({ value: "all" });
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    // 2026-10-28 ruling 1 + owner ruling 2026-10-29: the epic pills are
    // GONE and the REAL filters (the #1813 FAILED overlay, tag facets)
    // live in the COLLAPSIBLE filter drawer — nothing inline.
    await expect(page.getByTestId("epic-chip-all")).toHaveCount(0);
    await expect(page.locator('button[data-testid^="epic-chip-"]')).toHaveCount(0);
    // Closed drawer: no pills anywhere inline…
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
    await expect(page.getByTestId("failed-filter-toggle")).toHaveCount(0);
    // …the funnel button opens it and the real filters are inside.
    await page.getByTestId("board-filter-button").click();
    await expect(page.getByTestId("failed-filter-toggle")).toBeVisible();
    await page.getByTestId("board-filter-button").click();
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
    // Epic association stays reachable: the epic ROW selects → its
    // cards highlight (pinned in the swimlane block below).
    await page.getByTestId("epic-band-epic-2").click();
    await expect(page.getByTestId("task-card-tsk-eeee0005")).toHaveAttribute("data-highlighted", "true");
    await page.getByTestId("epic-band-epic-2").click();

    // Search near-miss → did-you-mean suggestion (0066 shape,
    // 2026-10-28 ruling 4: the suggestion is labeled by TITLE).
    await page.getByTestId("board-search").fill("tsk-aaaa0009");
    const miss = page.getByTestId("board-search-miss");
    await expect(miss).toContainText("No match");
    await expect(miss).toContainText("Write charter");
    await expect(miss).not.toContainText("tsk-aaaa0001");
    await miss.getByRole("button", { name: "Write charter" }).click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();
  });

  test("OR4 pr:N validation on the task editor", async ({ page }) => {
    await openBoard(page);

    await page.getByTestId("task-card-tsk-aaaa0001").click();
    const tags = page.locator("#task-tags");
    await tags.click();
    await tags.fill("pr:0");
    await tags.press("Enter");

    await expect(page.getByTestId("tag-validation-errors")).toContainText("pr:0");
    await expect(page.getByTestId("task-save")).toBeDisabled();
  });

  test("delete arm: destructive write removes the card (#1747 — cancel is eliminated)", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage).toBe("in_review");

    await page.getByTestId("task-card-tsk-cccc0003").click();
    await page.getByTestId("delete-task").click();

    // The destructive write + invalidation: the dialog closes and the
    // card is GONE (delete removes the object; the audit keeps the
    // snapshot — no Cancelled column exists anymore).
    await expect(page.getByTestId("delete-task")).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveCount(0, { timeout: 8000 });
  });

  test("reclaim arm: a streamed reclaim event converges the row to Backlog (lease lapse)", async ({ page }) => {
    const state = await openBoard(page);
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_progress");

    // RC1 reclaimed the lapsed lease server-side; the board.event streams.
    state.tasks.find((t) => t.short_id === "tsk-bbbb0002")!.stage = "backlog";
    await page.evaluate(() => {
      const emit = (window as unknown as { __user: (f: string) => void }).__user;
      emit(`data: ${JSON.stringify({
        type: "board.event",
        project_id: "prj-aaaa1111",
        id: "evt-e2e-reclaim",
        item_kind: "task",
        item_id: "uuid-tsk-bbbb0002",
        action: "reclaim",
        payload: { to_stage: "backlog" },
      })}\n\n`);
    });

    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "backlog", { timeout: 8000 });
  });

  test("start arm: task_start dispatches a backlog card and renders the claim notice", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-aaaa0001")!.stage).toBe("backlog");

    await page.getByTestId("start-tsk-aaaa0001").click();

    await expect(page.getByTestId("board-notice")).toContainText("fresh worker dispatched", { timeout: 8000 });
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveAttribute("data-stage", "in_progress", { timeout: 8000 });
  });

  test("refusal arm: delete 409 keeps the dialog open with the guard code rendered", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    // Override delete to refuse (the in-flight-needs-force class, #1747).
    await page.route(`${API}/me/tasks/*`, (r: Route) => {
      if (r.request().method() !== "DELETE") {
        void r.continue();
        return;
      }
      void r.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "board: guard epic_state: claims in flight — force required", code: "epic_state" }) });
    });
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("task-card-tsk-cccc0003").click();
    await page.getByTestId("delete-task").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "epic_state", { timeout: 8000 });
    // The dialog STAYS open (the fields survive for a retry).
    await expect(page.getByTestId("delete-task")).toBeVisible();
    await expect(page.getByTestId("task-title")).toBeVisible();
  });

  test("refusal arm: dependency cycle 409 renders suggestions and the chip fills the input", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("task-card-tsk-cccc0003").click();
    const dep = page.getByTestId("dep-input");
    await dep.fill("tsk-aaaa0001");
    await page.getByTestId("add-dependency").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "cycle", { timeout: 8000 });
    // The typed id is preserved and the suggestion chip fills the input.
    await expect(dep).toHaveValue("tsk-aaaa0001");
    const chip = page.getByTestId("board-error-suggestions").locator("button");
    await expect(chip).toHaveText("tsk-cccc0003");
    await chip.click();
    await expect(dep).toHaveValue("tsk-cccc0003");
  });

  test("refusal arm: task_start refusal renders at page level with the guard code", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    await page.route(`${API}/me/tasks/*/start`, (r: Route) =>
      r.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "board: guard epic_state: paused epics refuse task_start", code: "epic_state" }) }));
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("start-tsk-aaaa0001").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "epic_state", { timeout: 8000 });
    await expect(notice).toContainText("epic_state");
  });
});

test.describe("#1832 items 1-2 — swimlane creation buttons + collapsible epic bands", () => {
  test("every swimlane header carries enabled New epic / New task buttons (contextual, no dropdown)", async ({ page }) => {
    await openBoard(page);

    for (const pid of [PROJECT_A, PROJECT_B]) {
      await expect(page.getByTestId(`new-epic-${pid}`)).toBeVisible();
      await expect(page.getByTestId(`new-task-${pid}`)).toBeVisible();
      await expect(page.getByTestId(`new-epic-${pid}`)).toBeEnabled();
      await expect(page.getByTestId(`new-task-${pid}`)).toBeEnabled();
    }
    // The global New epic stays greyed at All-projects scope — the
    // swimlane buttons are the contextual path (#1832 item 1).
    await expect(page.getByTestId("new-epic-button")).toBeDisabled();
  });

  test("New epic from the Borealis swimlane header targets Borealis (no focus needed)", async ({ page }) => {
    await openBoard(page);

    await page.getByTestId(`new-epic-${PROJECT_B}`).click();
    await expect(page.getByText("New epic in Borealis")).toBeVisible();
    await page.getByTestId("epic-title").fill("Borealis next phase");
    const reqP = page.waitForRequest((r) => r.method() === "POST" && /\/me\/projects\/prj-bbbb2222\/epics$/.test(r.url()));
    await page.getByTestId("epic-create").click();

    // The POST rode the SWIMLANE's project (not a dropdown selection).
    const req = await reqP;
    expect(req.url()).toContain(`/me/projects/${PROJECT_B}/epics`);
    await expect(page.getByTestId("epic-title")).toHaveCount(0, { timeout: 8000 });
  });

  test("one epic ROW per epic in the swimlane — no nested task list, no collapse toggle", async ({ page }) => {
    await openBoard(page);

    const lane = page.getByTestId(`swimlane-${PROJECT_A}`);
    const rowE1 = lane.getByTestId("epic-band-epic-1");
    await expect(rowE1).toBeVisible();
    await expect(rowE1).toContainText("1/4");
    await expect(rowE1).toContainText("E1");
    await expect(lane.getByTestId("epic-band-epic-2")).toBeVisible();

    // 2026-10-28 ruling 2: NO task rows rendered underneath the epic
    // rows (the tasks live in the stage columns; the row's click
    // highlights its cards — pinned in the next test), and no collapse
    // toggle exists anymore (nothing to fold).
    await expect(lane.locator('[data-testid^="band-task-"]')).toHaveCount(0);
    await expect(lane.locator('[data-testid^="band-toggle-"]')).toHaveCount(0);
  });

  test("clicking an epic row highlights its tasks and dims the swimlane's others", async ({ page }) => {
    await openBoard(page);

    // Borealis's only task is FLAGGED (000050: pending_input is a badge
    // on the real stage, never a side column) — its card sits in the
    // main in_progress lane wearing the awaiting-input badge.
    await expect(page.getByTestId("task-card-tsk-ffff0006")).toBeVisible();

    await page.getByTestId("epic-band-epic-2").click();
    await expect(page.getByTestId("task-card-tsk-eeee0005")).toHaveAttribute("data-highlighted", "true");
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveAttribute("data-dimmed", "true");
    await expect(page.getByTestId("task-card-tsk-dddd0004")).toHaveAttribute("data-dimmed", "true");
    // Other swimlane untouched (selection is per-project).
    await expect(page.getByTestId("task-card-tsk-ffff0006")).not.toHaveAttribute("data-dimmed");

    // Click again → deselect: dims and highlights clear.
    await page.getByTestId("epic-band-epic-2").click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).not.toHaveAttribute("data-dimmed");
    await expect(page.getByTestId("task-card-tsk-eeee0005")).not.toHaveAttribute("data-highlighted");
  });

  test("the selected epic scopes New task (POST lands on that epic's route)", async ({ page }) => {
    const state = await openBoard(page);

    await page.getByTestId("epic-band-epic-2").click();
    await page.getByTestId(`new-task-${PROJECT_A}`).click();

    // The dialog names the selected epic as the landing zone.
    await expect(page.getByText("New task in E2", { exact: true })).toBeVisible();
    await page.getByTestId("task-title").fill("Scoped follow-up");
    await page.getByTestId("task-description").fill("seeded charter (#1849: required at create)");
    await page.getByTestId("new-criterion").fill("c1");
    await page.getByLabel("Add criterion").click();
    const reqP = page.waitForRequest((r) => r.method() === "POST" && /\/me\/epics\/epic-2\/tasks$/.test(r.url()));
    await page.getByTestId("task-save").click();
    const req = await reqP;
    expect(req.postDataJSON()).toMatchObject({ title: "Scoped follow-up" });

    // Authoritative row after the invalidation refetch: card in Backlog
    // (the epic ROW carries no nested task list anymore — the card IS
    // the task's rendered home).
    await expect(page.getByTestId("task-card-tsk-new0007")).toHaveAttribute("data-stage", "backlog", { timeout: 8000 });
    expect(state.tasks.some((t) => t.title === "Scoped follow-up")).toBe(true);
  });

  test("New task with no selection opens the epic picker; a pick scopes the dialog", async ({ page }) => {
    await openBoard(page);

    await page.getByTestId(`new-task-${PROJECT_A}`).click();
    await expect(page.getByTestId("epic-picker")).toBeVisible();
    await page.getByTestId("epic-picker-option-epic-2").click();

    await expect(page.getByText("New task in E2", { exact: true })).toBeVisible();
    await page.getByTestId("task-title").fill("Picked task");
    await page.getByTestId("task-description").fill("seeded charter");
    await page.getByTestId("new-criterion").fill("c1");
    await page.getByLabel("Add criterion").click();
    const reqP = page.waitForRequest((r) => r.method() === "POST" && /\/me\/epics\/epic-2\/tasks$/.test(r.url()));
    await page.getByTestId("task-save").click();
    const req = await reqP;
    expect(req.postDataJSON()).toMatchObject({ title: "Picked task" });
  });
});

test.describe("#1832 r2 — server-listed epics, unhappy paths, picker escape hatch", () => {
  test("a zero-task epic created this session survives a reload and takes a task", async ({ page }) => {
    const state = await openBoard(page);

    // Create an epic in Borealis via the swimlane button (no task rows).
    await page.getByTestId(`new-epic-${PROJECT_B}`).click();
    await page.getByTestId("epic-title").fill("Reload survivor");
    await page.getByTestId("epic-create").click();
    await expect(page.getByTestId("epic-band-epic-created-prj-bbbb2222")).toBeVisible({ timeout: 8000 });

    // THE reload: session state is gone — the band must survive on the
    // server's epic list alone (GET /me/projects/:id/epics).
    await page.reload();
    await expect(page.getByTestId(`swimlane-${PROJECT_B}`)).toBeVisible();
    const band = page.getByTestId("epic-band-epic-created-prj-bbbb2222");
    await expect(band).toBeVisible({ timeout: 8000 });
    await expect(band).toContainText("Reload survivor");
    await expect(band).toContainText("0/0");

    // The picker lists it (no duplicate-create invitation) and creation
    // lands inside it.
    await page.getByTestId(`new-task-${PROJECT_B}`).click();
    await expect(page.getByTestId("epic-picker")).toBeVisible();
    await expect(page.getByTestId("epic-picker-option-epic-created-prj-bbbb2222")).toBeVisible();
    await page.getByTestId("epic-picker-option-epic-created-prj-bbbb2222").click();

    await expect(page.getByText("New task in Reload survivor", { exact: true })).toBeVisible();
    await page.getByTestId("task-title").fill("First of its name");
    await page.getByTestId("task-description").fill("seeded charter");
    await page.getByTestId("new-criterion").fill("c1");
    await page.getByLabel("Add criterion").click();
    await page.getByTestId("task-save").click();

    // The authoritative row lands (no parent task existed — the project
    // rides the epic list, then the card renders in its column).
    await expect(page.getByTestId("task-card-tsk-new0007")).toHaveAttribute("data-stage", "backlog", { timeout: 8000 });
    // r3 finding 1+2: the epic row's progress must follow the refetched
    // server list (row-derived 0 done / 1 total) — the session's stale
    // 0/0 create response must not survive the list's arrival.
    await expect(band).toContainText("0/1", { timeout: 8000 });
    expect(state.tasks.some((t) => t.title === "First of its name")).toBe(true);
  });

  test("a failed epics list read renders the board load error; Retry recovers (r3)", async ({ page }) => {
    await openBoard(page);

    // Override the list route: the app's query client retries once
    // (providers/QueryClientProvider), so fail PROJECT_A's first TWO
    // GETs (initial + retry — scoped to ONE lane so the sibling lane's
    // concurrent GET doesn't consume a failure) before deferring to the
    // normal handler (Playwright: later routes win; fallback defers).
    let failuresLeft = 2;
    await page.route(`${API}/me/projects/*/epics`, async (r: Route) => {
      const isProjectA = r.request().url().includes(`/me/projects/${PROJECT_A}/epics`);
      if (failuresLeft > 0 && isProjectA && r.request().method() === "GET") {
        failuresLeft -= 1;
        void r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "board: epics store unavailable", code: "internal" }) });
        return;
      }
      await r.fallback();
    });

    await page.reload();
    await expect(page.getByTestId("board-read-error")).toBeVisible({ timeout: 8000 });
    await expect(page.locator("[data-board-error]").first()).toBeVisible();

    // Retry invalidates and re-reads — the fallback now serves the good
    // list; the board renders.
    await page.getByTestId("board-retry").click();
    await expect(page.getByTestId("swimlane-prj-aaaa1111")).toBeVisible({ timeout: 8000 });
    await expect(page.getByTestId("board-read-error")).toHaveCount(0);
  });

  test("a failed New-epic POST renders the error in the still-open dialog (target + fields preserved)", async ({ page }) => {
    await openBoard(page, { failEpicCreate: true });

    await page.getByTestId(`new-epic-${PROJECT_B}`).click();
    await expect(page.getByText("New epic in Borealis")).toBeVisible();
    await page.getByTestId("epic-title").fill("Doomed epic");
    await page.getByTestId("epic-create").click();

    // 500 → the engine's error renders inside the dialog; the dialog
    // STAYS open with the swimlane target and the drafted title.
    await expect(page.locator("[data-board-error]").first()).toBeVisible({ timeout: 8000 });
    await expect(page.getByText("New epic in Borealis")).toBeVisible();
    await expect(page.getByTestId("epic-title")).toHaveValue("Doomed epic");
  });

  test("a failed New-task POST renders the error in the still-open TaskDialog (fields preserved)", async ({ page }) => {
    await openBoard(page, { failTaskCreate: true });

    await page.getByTestId("epic-band-epic-2").click();
    await page.getByTestId(`new-task-${PROJECT_A}`).click();
    await expect(page.getByText("New task in E2", { exact: true })).toBeVisible();
    await page.getByTestId("task-title").fill("Doomed task");
    await page.getByTestId("task-description").fill("seeded charter");
    await page.getByTestId("new-criterion").fill("c1");
    await page.getByLabel("Add criterion").click();
    await page.getByTestId("task-save").click();

    await expect(page.locator("[data-board-error]").first()).toBeVisible({ timeout: 8000 });
    await expect(page.getByText("New task in E2", { exact: true })).toBeVisible();
    await expect(page.getByTestId("task-title")).toHaveValue("Doomed task");
  });

  test("an empty project's picker offers the create-epic escape hatch (e2e)", async ({ page }) => {
    await openBoard(page, {
      projects: [
        { id: PROJECT_A, short_id: PROJECT_A, name: "Apollo", worker_slots: 2 },
        { id: PROJECT_B, short_id: PROJECT_B, name: "Borealis", worker_slots: 1 },
        { id: "prj-cccc3333", short_id: "prj-cccc3333", name: "Ceres", worker_slots: 1 },
      ],
    });
    await expect(page.getByTestId("swimlane-prj-cccc3333")).toBeVisible();

    await page.getByTestId("new-task-prj-cccc3333").click();
    await expect(page.getByTestId("epic-picker-empty")).toBeVisible();
    await page.getByTestId("epic-picker-create-epic").click();
    await expect(page.getByText("New epic in Ceres")).toBeVisible();
  });
});

test.describe("#1832 item 3 — kebab menus + delete flows", () => {
  test("project kebab: archive then unarchive (NO delete item — #1747; #1964 archived drawer)", async ({ page }) => {
    await openBoard(page);

    await page.getByTestId(`project-kebab-${PROJECT_A}`).click();
    const menu = page.getByRole("menu");
    await expect(menu.getByRole("menuitem", { name: "Archive project" })).toBeVisible();
    // #1747: project delete is eliminated — the menu must not offer it.
    await expect(menu.getByRole("menuitem", { name: /Delete project/i })).toHaveCount(0);

    const reqP = page.waitForRequest((r) => r.method() === "POST" && r.url().endsWith(`/me/projects/${PROJECT_A}/archive`));
    await menu.getByRole("menuitem", { name: "Archive project" }).click();
    const req = await reqP;
    expect(req.url()).toContain(`/me/projects/${PROJECT_A}/archive`);

    // #1964: post-invalidation the archived lane leaves the active
    // surface for the collapsed Archived drawer at the board's end.
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("archived-projects-drawer")).toBeVisible();

    // Open BOTH levels — the drawer, then the project's name-only row:
    // its swimlane (history) renders inside, archived badge included.
    await page.getByTestId("archived-projects-toggle").click();
    await page.getByTestId(`archived-project-toggle-${PROJECT_A}`).click();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`).locator("text=archived").first()).toBeVisible();

    // Unarchive from the kebab INSIDE the drawer restores the lane to
    // the active surface and empties the drawer.
    await page.getByTestId(`project-kebab-${PROJECT_A}`).click();
    await page.getByRole("menuitem", { name: "Unarchive project" }).click();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`).locator("text=archived")).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("archived-projects-drawer")).toHaveCount(0, { timeout: 8000 });
  });

  test("project kebab: failed lifecycle call surfaces the page error; board stays consistent, retry recovers", async ({ page }) => {
    const state = await openBoard(page);

    // The failure injection rides the lifecycle mock's own 404 arm (the
    // `project not found` branch): the project vanishes server-side
    // between the board's read and the lifecycle write. The handler
    // reads state.projects at request time, and the failed path never
    // invalidates — the UI keeps its cached read, so the write 404s
    // while the board keeps rendering (#1860 mandatory gate: the
    // handleProjectLifecycle catch branch, BoardPage.tsx setPageError).
    const apollo = state.projects!.find((p) => p.id === PROJECT_A)!;
    state.projects = state.projects!.filter((p) => p.id !== PROJECT_A);

    await page.getByTestId(`project-kebab-${PROJECT_A}`).click();
    await page.getByRole("menuitem", { name: "Archive project" }).click();

    // The error is SURFACED, not swallowed: the page-level notice
    // renders the engine's message verbatim (no guard code on this
    // arm → the "error" class attribute).
    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "error", { timeout: 8000 });
    await expect(notice).toContainText(`board: project "${PROJECT_A}" not found`);

    // Board state consistent: no crash, the swimlane and its cards
    // survive on the cached read, and the archived badge did NOT land
    // (the write never succeeded).
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`).locator("text=archived")).toHaveCount(0);
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();

    // Recovery: the project is back server-side; a retry archives it,
    // clears the page error — and #1964: the post-invalidation lane
    // leaves the active surface for the collapsed Archived drawer.
    state.projects = [...(state.projects ?? []), { ...apollo }];
    await page.getByTestId(`project-kebab-${PROJECT_A}`).click();
    await page.getByRole("menuitem", { name: "Archive project" }).click();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("archived-projects-drawer")).toBeVisible();
    await expect(page.locator("[data-board-error]")).toHaveCount(0);
  });

  test("task delete modal: UNCHECKED reconnect sends plain DELETE (dependents simply unblocked)", async ({ page }) => {
    const state = await openBoard(page);

    await page.getByTestId("task-kebab-tsk-cccc0003").click();
    await page.getByRole("menuitem", { name: "Delete task…" }).click();

    const modal = page.getByTestId("task-delete-modal");
    await expect(modal).toBeVisible();
    // The dependent (tsk-eeee0005 is blocked-by this task) is surfaced.
    await expect(modal.getByTestId("task-delete-dependents")).toContainText("tsk-eeee0005");
    await expect(modal.getByTestId("reconnect-dependents")).not.toBeChecked();

    const reqP = page.waitForRequest((r) => r.method() === "DELETE" && /\/me\/tasks\/tsk-cccc0003$/.test(r.url()));
    await modal.getByTestId("confirm-delete-task").click();
    const req = await reqP;
    // Semantics 1 — unchecked: NO reconnect flag on the wire.
    expect(new URL(req.url()).searchParams.get("reconnect")).toBeNull();

    // The card is gone after the invalidation refetch.
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveCount(0, { timeout: 8000 });
    expect(state.tasks.some((t) => t.short_id === "tsk-cccc0003")).toBe(false);
  });

  test("task delete modal: CHECKED reconnect sends ?reconnect=true (inherit upstream edges)", async ({ page }) => {
    const state = await openBoard(page);

    await page.getByTestId("task-kebab-tsk-cccc0003").click();
    await page.getByRole("menuitem", { name: "Delete task…" }).click();
    const modal = page.getByTestId("task-delete-modal");
    await modal.getByTestId("reconnect-dependents").check();

    const reqP = page.waitForRequest((r) => r.method() === "DELETE" && /\/me\/tasks\/tsk-cccc0003/.test(r.url()));
    await modal.getByTestId("confirm-delete-task").click();
    const req = await reqP;
    // Semantics 2 — checked: reconnect=true on the wire.
    expect(new URL(req.url()).searchParams.get("reconnect")).toBe("true");

    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveCount(0, { timeout: 8000 });
    expect(state.tasks.some((t) => t.short_id === "tsk-cccc0003")).toBe(false);
  });

  test("epic delete: in-flight surfaced, refusal reveals force-confirm; force cascades", async ({ page }) => {
    // A live claim under epic-1: tsk-bbbb0002 in_progress + assignee.
    // Set BEFORE goto — the browser's query cache holds parsed copies,
    // so a post-load Node-side mutation never reaches the UI.
    const state = freshState();
    state.tasks.find((t) => t.short_id === "tsk-bbbb0002")!.assignee_session = `${WORKSPACE_ID}/sess-bbbb`;
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    await page.goto("/board");

    await page.getByTestId("task-card-tsk-aaaa0001").waitFor();
    // Scoped to Apollo's swimlane — the fixture's epic-1 id also renders
    // as a band in Borealis (tsk-ffff0006 shares the epic id).
    await page.getByTestId(`swimlane-${PROJECT_A}`).getByTestId("epic-kebab-epic-1").click();
    await page.getByRole("menuitem", { name: "Delete epic…" }).click();

    const modal = page.getByTestId("epic-delete-modal");
    await expect(modal).toBeVisible();
    // In-flight worker surfaced pre-flight + honest cascade count
    // (5 tasks carry epic-1: 4 in Apollo + Borealis's pending row).
    await expect(modal.getByTestId("epic-delete-inflight")).toContainText("tsk-bbbb0002");
    await expect(modal.getByTestId("epic-delete-cascade-count")).toHaveText("5");

    // First attempt: NO force — the API refuses (epic_state).
    const refuseP = page.waitForRequest((r) => r.method() === "DELETE" && /\/me\/epics\/epc-epic1000$/.test(r.url()));
    await modal.getByTestId("confirm-delete-epic").click();
    const refuseReq = await refuseP;
    expect(new URL(refuseReq.url()).searchParams.get("force")).toBeNull();
    await expect(modal.locator("[data-board-error]")).toHaveAttribute("data-board-error", "epic_state");

    // Force-confirm: gated behind the acknowledge checkbox.
    const forceBtn = modal.getByTestId("force-delete-epic");
    await expect(forceBtn).toBeDisabled();
    await modal.getByTestId("force-abort-confirm").check();
    await expect(forceBtn).toBeEnabled();

    const forceP = page.waitForRequest((r) => r.method() === "DELETE" && /\/me\/epics\/epc-epic1000\?force=true$/.test(r.url()));
    await forceBtn.click();
    const forceReq = await forceP;
    expect(new URL(forceReq.url()).searchParams.get("force")).toBe("true");

    // Cascade: modal closes; epic-1's cards + band vanish on refetch.
    await expect(modal).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveCount(0, { timeout: 8000 });
    await expect(page.getByTestId("epic-band-epic-1")).toHaveCount(0, { timeout: 8000 });
    // epic-2 survives (scoped blast radius).
    await expect(page.getByTestId("task-card-tsk-eeee0005")).toBeVisible();
    expect(state.tasks.some((t) => t.epic_id === "epic-1")).toBe(false);
  });
});

// --- 2026-10-28 owner rulings: capped epic list + mobile layout ------------------

test.describe("2026-10-28 board declutter — capped epic list, mobile columns/wrap", () => {
  function zeroTaskEpic(n: number, title: string, updatedAt: string): E2EEpic {
    return {
      id: `epic-capped-${n}`, project_id: PROJECT_A, short_id: `epc-cap000${n}`,
      title, outcome: "", status: "active", tags: [],
      created_at: "2026-10-01T00:00:00Z", updated_at: updatedAt,
      progress_done: 0, progress_total: 0, progress_derived: false,
    };
  }

  test("epic list caps visible rows and scrolls internally (not page growth), activity-first order", async ({ page }) => {
    // 7 zero-task server epics + the 2 task-derived rows (E1, E2) = 9
    // rows; the visible window is 4 — the rest scroll INSIDE the list.
    const state = await openBoard(page, {
      epics: [
        zeroTaskEpic(1, "Alpha quiet", "2026-10-01T00:00:00Z"),
        zeroTaskEpic(2, "Bravo busy", "2026-10-07T00:00:00Z"),
        zeroTaskEpic(3, "Charlie mid", "2026-10-05T00:00:00Z"),
        zeroTaskEpic(4, "Delta late", "2026-10-06T00:00:00Z"),
        zeroTaskEpic(5, "Echo quiet", "2026-10-01T00:00:00Z"),
        zeroTaskEpic(6, "Foxtrot older", "2026-10-02T00:00:00Z"),
        zeroTaskEpic(7, "Hotel busy2", "2026-10-08T00:00:00Z"),
      ],
    });
    expect(state.epics.length).toBe(7);

    const lane = page.getByTestId(`swimlane-${PROJECT_A}`);
    const list = lane.getByTestId(`epic-list-${PROJECT_A}`);
    await expect(list).toBeVisible();

    // Every row renders (the cap is VISUAL — scrolling reaches the rest).
    await expect(list.locator('[data-testid^="epic-band-"]')).toHaveCount(9);

    // The list is its OWN scroll region: overflow clips vertically and
    // the content exceeds the capped client window.
    const metrics = await list.evaluate((el) => {
      const cs = getComputedStyle(el);
      return {
        overflowY: cs.overflowY,
        scrollHeight: el.scrollHeight,
        clientHeight: el.clientHeight,
        boxHeight: el.getBoundingClientRect().height,
      };
    });
    expect(["auto", "scroll"]).toContain(metrics.overflowY);
    expect(metrics.scrollHeight).toBeGreaterThan(metrics.clientHeight);
    // ~4 visible rows: the capped window stays a bounded region (the
    // PAGE never grows with the epic count).
    expect(metrics.boxHeight).toBeLessThan(150);

    // Activity-first order (ruling 3): the task-derived E1/E2 ride
    // their rows' 2026-10-10 activity, then the seeded epics by their
    // own updated_at desc (Hotel 10-08, Bravo 10-07, Delta 10-06,
    // Charlie 10-05, Foxtrot 10-02), quiet rows alpha-last.
    const titles = await list.locator('[data-testid^="band-title-"]').allTextContents();
    expect(titles).toEqual([
      "E1", "E2", "Hotel busy2", "Bravo busy", "Delta late", "Charlie mid", "Foxtrot older", "Alpha quiet", "Echo quiet",
    ]);
  });

  test("epic list at exactly the cap shows all 4 rows fully visible — no internal scroll", async ({ page }) => {
    // The cap's complement (ruling 3 + review r1): >4 rows must scroll
    // (pinned above); exactly 4 rows must fit with ZERO internal
    // scrolling — every row fully visible, scrollHeight === the client
    // window. Guards the zero-slack regression class (a max-h shaved
    // to exactly 4×row+3×gap clips the 4th row under any row growth).
    const state = await openBoard(page, {
      epics: [
        zeroTaskEpic(1, "Alpha quiet", "2026-10-01T00:00:00Z"),
        zeroTaskEpic(2, "Bravo busy", "2026-10-07T00:00:00Z"),
      ],
    });
    expect(state.epics.length).toBe(2);

    const lane = page.getByTestId(`swimlane-${PROJECT_A}`);
    const list = lane.getByTestId(`epic-list-${PROJECT_A}`);
    await expect(list).toBeVisible();

    // E1, E2 (task-derived) + Alpha + Bravo = exactly EPIC_LIST_MAX_VISIBLE rows.
    await expect(list.locator('[data-testid^="epic-band-"]')).toHaveCount(4);

    const metrics = await list.evaluate((el) => {
      const cs = getComputedStyle(el);
      return {
        overflowY: cs.overflowY,
        scrollHeight: el.scrollHeight,
        clientHeight: el.clientHeight,
      };
    });
    expect(["auto", "scroll"]).toContain(metrics.overflowY);
    expect(metrics.scrollHeight).toBe(metrics.clientHeight);

    // The LAST row's box sits entirely inside the list's client
    // window (fully visible, not clipped by the cap).
    const lastRow = list.locator('[data-testid^="epic-band-"]').nth(3);
    await expect(lastRow).toBeVisible();
    const listBox = await list.evaluate((el) => {
      const b = el.getBoundingClientRect();
      return { top: b.top, bottom: b.bottom };
    });
    const rowBox = await lastRow.evaluate((el) => {
      const b = el.getBoundingClientRect();
      return { top: b.top, bottom: b.bottom };
    });
    expect(rowBox.top).toBeGreaterThanOrEqual(listBox.top - 0.5);
    expect(rowBox.bottom).toBeLessThanOrEqual(listBox.bottom + 0.5);
  });

  test("mobile: 2-column grid, long titles wrap INSIDE the card, action buttons never bleed", async ({ page }) => {
    // Ruling 5: 4 columns cannot wrap cleanly at mobile widths — the
    // grid collapses to 2 columns on small screens (4 from md up), the
    // card wraps its own content, and the kebab/start cluster is
    // shrink-0 (contained inside the card's bounds).
    await page.setViewportSize({ width: 375, height: 667 });
    const longTitle =
      "Implement the reconciliation sweep for the lease-busy guard with a deliberately long unbroken token " +
      "supercalifragilisticexpialidocious-reconciliation-sweep-implementation-title so word-wrap is exercised";
    const base = freshState();
    base.tasks.push(task({
      short_id: "tsk-long0001", title: longTitle, stage: "backlog",
      project_id: PROJECT_A, epic_id: "epic-1",
    }));
    await openBoard(page, { tasks: base.tasks });

    const grid = page.getByTestId(`stage-grid-${PROJECT_A}`);
    await expect(grid).toBeVisible();

    // 2 columns at 375px …
    const colsMobile = await grid.evaluate((el) => getComputedStyle(el).gridTemplateColumns.split(" ").length);
    expect(colsMobile).toBe(2);
    // … and 4 from md up (768px).
    await page.setViewportSize({ width: 768, height: 900 });
    await expect(grid).toBeVisible();
    const colsMd = await grid.evaluate((el) => getComputedStyle(el).gridTemplateColumns.split(" ").length);
    expect(colsMd).toBe(4);
    await page.setViewportSize({ width: 375, height: 667 });

    // No horizontal page overflow at the narrow width.
    const docOverflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(docOverflow).toBeLessThanOrEqual(1);

    // The long-title card: the title WRAPS (multi-line height) and
    // stays inside the card, the card inside its column cell.
    const card = page.getByTestId("task-card-tsk-long0001");
    await expect(card).toBeVisible();
    const title = card.getByTestId("card-title-tsk-long0001");
    await expect(title).toBeVisible();
    const boxes = await card.evaluate((el) => {
      const cardBox = el.getBoundingClientRect();
      const titleEl = el.querySelector('[data-testid="card-title-tsk-long0001"]')!;
      const titleBox = titleEl.getBoundingClientRect();
      const kebabEl = el.querySelector('[data-testid="task-kebab-tsk-long0001"]')!;
      const kebabBox = kebabEl.getBoundingClientRect();
      const cellBox = (el.closest('[data-stage-drop]') as HTMLElement)!.getBoundingClientRect();
      return {
        titleRight: titleBox.right, titleHeight: titleBox.height,
        cardRight: cardBox.right, cardLeft: cardBox.left,
        kebabRight: kebabBox.right, kebabLeft: kebabBox.left,
        cellRight: cellBox.right, cellLeft: cellBox.left,
      };
    });
    // Title wraps to multiple lines (a 170+ char title in a ~160px
    // column cannot fit one line) — wrapped, not clipped/overflowed.
    expect(boxes.titleHeight).toBeGreaterThan(32);
    // Containment: title, kebab and the card itself all stay inside the
    // column cell's bounds (no bleed into the neighbor column).
    expect(boxes.titleRight).toBeLessThanOrEqual(boxes.cardRight + 0.5);
    expect(boxes.cardRight).toBeLessThanOrEqual(boxes.cellRight + 0.5);
    expect(boxes.cardLeft).toBeGreaterThanOrEqual(boxes.cellLeft - 0.5);
    expect(boxes.kebabRight).toBeLessThanOrEqual(boxes.cardRight + 0.5);
    expect(boxes.kebabLeft).toBeGreaterThanOrEqual(boxes.cardLeft - 0.5);
  });
});

// --- 2026-10-29 owner ruling: the filter pills collapse into a drawer ------------

test.describe("2026-10-29 filter drawer — pills collapsed, count badge, clear-all, mobile zero-height", () => {
  test("funnel button opens/closes the drawer; count badge tracks active filters; Clear all resets", async ({ page }) => {
    // A failed row so the FAILED overlay arm has a survivor to show.
    const base = freshState();
    base.tasks.push(task({
      short_id: "tsk-fail0010", title: "Crash looping", stage: "backlog",
      project_id: PROJECT_A, epic_id: "epic-2",
      attempts: 2, last_attempt_failed: true, failure_count: 2,
    }));
    await openBoard(page, { tasks: base.tasks });

    // Closed by default: drawer absent from the DOM, no count badge.
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
    await expect(page.getByTestId("board-filter-count")).toHaveCount(0);

    // Open: the real filters (FAILED overlay, tag facets) live INSIDE.
    await page.getByTestId("board-filter-button").click();
    await expect(page.getByTestId("board-filter-button")).toHaveAttribute("aria-expanded", "true");
    const drawer = page.getByTestId("board-filter-drawer");
    await expect(drawer).toBeVisible();
    await expect(drawer.getByTestId("failed-filter-toggle")).toBeVisible();
    await expect(drawer.getByTestId("tag-filter-pr:1702")).toBeVisible();

    // Apply the tag facet: the board filters identically to the old
    // inline pill, and the badge shows exactly one active filter.
    await drawer.getByTestId("tag-filter-pr:1702").click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveCount(0);
    await expect(page.getByTestId("task-card-tsk-eeee0005")).toBeVisible();
    await expect(page.getByTestId("board-filter-count")).toHaveText("1");

    // A second filter (the FAILED overlay) stacks: badge reads 2.
    await drawer.getByTestId("failed-filter-toggle").click();
    await expect(page.getByTestId("board-filter-count")).toHaveText("2");

    // Clear all — INSIDE the drawer: every row returns, badge drops.
    await drawer.getByTestId("board-filter-clear").click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();
    await expect(page.getByTestId("task-card-tsk-fail0010")).toBeVisible();
    await expect(page.getByTestId("board-filter-count")).toHaveCount(0);

    // Toggle close: the drawer leaves the DOM (zero height), and the
    // button's aria-expanded flips back.
    await page.getByTestId("board-filter-button").click();
    await expect(page.getByTestId("board-filter-button")).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
  });

  test("mobile: closed drawer is zero-height — no inline pills, board content starts immediately below the toolbar", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 667 });
    await openBoard(page);

    // No pills inline at the narrow viewport: while closed, neither the
    // drawer nor any of its controls render.
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
    await expect(page.getByTestId("failed-filter-toggle")).toHaveCount(0);
    await expect(page.locator('button[data-testid^="tag-filter-"]')).toHaveCount(0);

    // The board content starts IMMEDIATELY below the toolbar — no
    // reserved strip where the pill row used to live (±1px tolerance
    // for the toolbar's border rounding).
    const toolbarToGrid = () => page.evaluate(() => {
      const toolbar = document.querySelector('[data-testid="board-toolbar"]')!.getBoundingClientRect();
      const grid = document.querySelector('[data-testid="board-grid"]')!.getBoundingClientRect();
      return grid.top - toolbar.bottom;
    });
    expect(await toolbarToGrid()).toBeLessThanOrEqual(1);
    expect(await toolbarToGrid()).toBeGreaterThanOrEqual(-1);

    // The funnel button is reachable at 375px; the drawer opens as a
    // slide-down panel IN FLOW — it never overlaps the swim lanes.
    await page.getByTestId("board-filter-button").click();
    const drawer = page.getByTestId("board-filter-drawer");
    await expect(drawer).toBeVisible();
    const overlap = await page.evaluate(() => {
      const drawerBox = document.querySelector('[data-testid="board-filter-drawer"]')!.getBoundingClientRect();
      const lane = document.querySelector('[data-testid="swimlane-prj-aaaa1111"]')!.getBoundingClientRect();
      return Math.max(0, drawerBox.bottom - lane.top);
    });
    expect(overlap).toBeLessThanOrEqual(0.5);

    // A filter applied inside the mobile drawer still filters.
    await drawer.getByTestId("tag-filter-pr:1702").click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveCount(0);
    await expect(page.getByTestId("board-filter-count")).toHaveText("1");

    // Closing restores the zero-height posture byte-identically: the
    // drawer leaves the DOM and the content re-adjoints the toolbar
    // (the applied filter and its badge survive the collapse).
    await page.getByTestId("board-filter-button").click();
    await expect(page.getByTestId("board-filter-drawer")).toHaveCount(0);
    expect(await toolbarToGrid()).toBeLessThanOrEqual(1);
    expect(await toolbarToGrid()).toBeGreaterThanOrEqual(-1);
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveCount(0);
    await expect(page.getByTestId("board-filter-count")).toHaveText("1");
  });
});
