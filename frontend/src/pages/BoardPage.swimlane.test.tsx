// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import {
  renderBoard,
  seedBoard,
  defaultTasks,
  mockCreateEpic,
  mockCreateTask,
  mockGetEpic,
  mockListProjectEpics,
  mockListProjectTasks,
  mockMoveStage,
  makeDataTransfer,
  boardError,
  PROJECT_A,
  PROJECT_B,
} from "./boardTestUtils";
import type { BoardEpic, BoardProject } from "../api/board";

// #1832 items 1-2: contextual creation in each project swimlane header
// (New epic / New task — no greyed-only global path) + collapsible epic
// bands inside the swimlane with tasks grouped beneath; clicking a band
// highlights its tasks and dims others; the selected epic scopes task
// creation.

describe("BoardPage #1832 — swimlane creation buttons + epic bands", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  // --- Item 1: contextual header buttons -----------------------------------

  it("renders enabled New epic / New task buttons in EVERY swimlane header (All-projects scope)", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    for (const sid of ["prj-aaaa1111", "prj-bbbb2222"]) {
      const epicBtn = screen.getByTestId(`new-epic-${sid}`) as HTMLButtonElement;
      const taskBtn = screen.getByTestId(`new-task-${sid}`) as HTMLButtonElement;
      expect(epicBtn).toBeInTheDocument();
      expect(taskBtn).toBeInTheDocument();
      // Contextual = enabled with NO dropdown selection (the global
      // new-epic is greyed at focus=all; the swimlane path must not be).
      expect(epicBtn.disabled).toBe(false);
      expect(taskBtn.disabled).toBe(false);
    }
    // The greyed-until-selection global path still exists but is NOT the
    // only one (the swimlane buttons above are the contextual path).
    expect((screen.getByTestId("new-epic-button") as HTMLButtonElement).disabled).toBe(true);
  });

  it("New epic from a swimlane header scopes creation to THAT project (no dropdown needed)", async () => {
    mockCreateEpic.mockResolvedValue({
      id: "epic-uuid-new", project_id: PROJECT_B.id, short_id: "epc-new11111", title: "Borealis epic",
      outcome: "", status: "draft", tags: [], created_at: "", updated_at: "",
      progress_done: 0, progress_total: 0, progress_derived: false,
    });
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    // focus stays "all" — the swimlane carries the context.
    fireEvent.click(screen.getByTestId("new-epic-prj-bbbb2222"));
    const title = await screen.findByTestId("epic-title");
    expect(title).toBeInTheDocument();
    // The dialog names the target project (Borealis, not Apollo).
    expect(screen.getByText("New epic in Borealis")).toBeInTheDocument();

    fireEvent.change(title, { target: { value: "Borealis epic" } });
    fireEvent.click(screen.getByTestId("epic-create"));
    await waitFor(() => expect(mockCreateEpic).toHaveBeenCalledWith("proj-uuid-b", expect.objectContaining({ title: "Borealis epic" })));
  });

  // --- Item 2: collapsible epic bands ----------------------------------------

  it("renders one collapsible band per epic inside the swimlane, tasks grouped beneath", async () => {
    renderBoard();
    // Wait for the read model (a card) before querying derived bands.
    await screen.findByTestId("task-card-tsk-aaaa0001");
    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    const bandE1 = lane.querySelector('[data-testid="epic-band-epic-uuid-1"]')!;
    expect(bandE1).not.toBeNull();
    expect(screen.getByTestId("epic-band-epic-uuid-2")).toBeInTheDocument();

    // Tasks grouped beneath their band (Apollo: E1 → 4 tasks, E2 → 1).
    for (const sid of ["tsk-aaaa0001", "tsk-bbbb0002", "tsk-cccc0003", "tsk-dddd0004"]) {
      expect(lane.querySelector(`[data-testid="band-task-epic-uuid-1-${sid}"]`)).not.toBeNull();
    }
    expect(lane.querySelector('[data-testid="band-task-epic-uuid-2-tsk-eeee0005"]')).not.toBeNull();
    expect(lane.querySelector('[data-testid="band-task-epic-uuid-1-tsk-eeee0005"]')).toBeNull();

    // Derived progress rides the band header (E1: 1 of 5 done). Failed
    // tasks COUNT: the backend epic progress (pkg/board/reads.go) is
    // count(*) FILTER (done) / count(*) with no failure exclusion —
    // failure is an attribute (#1823), not a stage — so the local
    // derivation must match the server's number exactly (r3: server
    // truth and derived truth may not disagree).
    expect(bandE1.textContent).toContain("1/5");

    // Collapse hides the band's tasks (header stays); expand restores.
    fireEvent.click(lane.querySelector('[data-testid="band-toggle-epic-uuid-1"]')!);
    await waitFor(() => expect(lane.querySelector('[data-testid="band-task-epic-uuid-1-tsk-aaaa0001"]')).toBeNull());
    expect(bandE1).toBeInTheDocument();
    fireEvent.click(lane.querySelector('[data-testid="band-toggle-epic-uuid-1"]')!);
    await waitFor(() => expect(lane.querySelector('[data-testid="band-task-epic-uuid-1-tsk-aaaa0001"]')).not.toBeNull());
  });

  it("clicking a band highlights its tasks and dims the swimlane's others (other swimlanes untouched)", async () => {
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");
    // Borealis's only task lives in the collapsed side columns — open
    // them so its card is queryable.
    fireEvent.click(screen.getAllByTestId("side-columns-toggle")[1]!);
    await screen.findByTestId("task-card-tsk-ffff0006");

    // Select E2 in Apollo's swimlane.
    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-eeee0005").getAttribute("data-highlighted")).toBe("true");
    });
    for (const sid of ["tsk-aaaa0001", "tsk-bbbb0002", "tsk-cccc0003", "tsk-dddd0004"]) {
      expect(screen.getByTestId(`task-card-${sid}`).getAttribute("data-dimmed")).toBe("true");
    }
    // The band itself carries the selection state.
    expect(screen.getByTestId("epic-band-epic-uuid-2").getAttribute("data-selected")).toBe("true");
    // Borealis has no selection — its card is neither dimmed nor highlighted.
    expect(screen.getByTestId("task-card-tsk-ffff0006").getAttribute("data-dimmed")).toBeNull();
    expect(screen.getByTestId("task-card-tsk-ffff0006").getAttribute("data-highlighted")).toBeNull();

    // Click again → deselect: no dims or highlights anywhere.
    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-aaaa0001").getAttribute("data-dimmed")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-eeee0005").getAttribute("data-highlighted")).toBeNull();
  });

  // --- Item 2b: the selected epic scopes task creation ------------------------

  it("New task from the swimlane header lands inside the SELECTED epic", async () => {
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");

    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));

    // The dialog names the selected epic as the landing zone (the
    // "in <epic>" span inside the dialog title).
    const title = await screen.findByTestId("task-title");
    expect(screen.getByText("in E2")).toBeInTheDocument();
    fireEvent.change(title, { target: { value: "Scoped task" } });
    fireEvent.click(screen.getByTestId("task-save"));

    await waitFor(() => expect(mockCreateTask).toHaveBeenCalledWith("epic-uuid-2", expect.objectContaining({ title: "Scoped task" })));
  });

  it("New task with NO selection opens the epic picker; choosing an epic scopes the dialog", async () => {
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    const picker = await screen.findByTestId("epic-picker");
    expect(picker).toBeInTheDocument();
    // The picker offers the swimlane's epics.
    fireEvent.click(screen.getByTestId("epic-picker-option-epic-uuid-2"));

    const title = await screen.findByTestId("task-title");
    expect(screen.getByText("in E2")).toBeInTheDocument();
    fireEvent.change(title, { target: { value: "Picked task" } });
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() => expect(mockCreateTask).toHaveBeenCalledWith("epic-uuid-2", expect.objectContaining({ title: "Picked task" })));
  });

  it("the epic picker offers creation when the project has no epics yet", async () => {
    const emptyProject: BoardProject = {
      id: "proj-uuid-c",
      short_id: "prj-cccc3333",
      user_id: "u1",
      name: "Ceres",
      target_workspace_id: "ws-1",
      worker_slots: 1,
      tags: [],
      status: "active",
      created_at: "2026-10-03T00:00:00Z",
      updated_at: "2026-10-03T00:00:00Z",
    };
    seedBoard(defaultTasks(), [PROJECT_A, PROJECT_B, emptyProject]);
    renderBoard();
    await screen.findByTestId("swimlane-prj-cccc3333");

    fireEvent.click(screen.getByTestId("new-task-prj-cccc3333"));
    const empty = await screen.findByTestId("epic-picker-empty");
    expect(empty.textContent).toContain("No epics");
    // The picker's escape hatch opens epic creation scoped to the project.
    fireEvent.click(screen.getByTestId("epic-picker-create-epic"));
    await screen.findByTestId("epic-title");
    expect(screen.getByText("New epic in Ceres")).toBeInTheDocument();
  });
});

// #1832 r2 — the review round's findings: epics sourced from
// GET /me/projects/:id/epics (zero-task epics survive reload and stay
// pickable), the filtered-vs-empty band distinction, unhappy-path
// coverage for the new creation flows, and reversion guards for the
// r1 fixes (band-header hit surface, seedBoard exact-pid routing).
describe("BoardPage #1832 r2 — server epics, unhappy paths, reversion guards", () => {
  // Server truth: a REAL epic (created last session, never a task row).
  const ZERO_TASK_EPIC: BoardEpic = {
    id: "epic-uuid-zero", project_id: PROJECT_A.id, short_id: "epc-zero0000",
    title: "Quiet epic", outcome: "", status: "active", tags: [],
    created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z",
    progress_done: 0, progress_total: 0, progress_derived: false,
  };

  function dragCardTo(shortId: string, stage: string) {
    const dt = makeDataTransfer();
    fireEvent.dragStart(screen.getByTestId(`task-card-${shortId}`), { dataTransfer: dt });
    fireEvent.drop(screen.getAllByTestId(`stage-drop-${stage}`)[0]!, { dataTransfer: dt });
  }

  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  // --- Finding 1: server-listed epics --------------------------------------

  it("renders a zero-task epic from GET /me/projects/:id/epics (band after reload, picker, creation)", async () => {
    mockListProjectEpics.mockResolvedValue([ZERO_TASK_EPIC]);
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // A fresh mount IS the reload: the epic lives only in the server
    // list (no task row, nothing session-created) — its band must render.
    const band = await screen.findByTestId("epic-band-epic-uuid-zero");
    expect(band).toBeInTheDocument();
    expect(band.textContent).toContain("Quiet epic");
    expect(band.textContent).toContain("0/0");

    // The picker offers it (no duplicate-create invitation).
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    expect(await screen.findByTestId("epic-picker-option-epic-uuid-zero")).toBeInTheDocument();

    // Creation lands inside it.
    fireEvent.click(screen.getByTestId("epic-picker-option-epic-uuid-zero"));
    const title = await screen.findByTestId("task-title");
    expect(screen.getByText("in Quiet epic")).toBeInTheDocument();
    fireEvent.change(title, { target: { value: "First task" } });
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() => expect(mockCreateTask).toHaveBeenCalledWith("epic-uuid-zero", expect.objectContaining({ title: "First task" })));
  });

  it("an epics read failure is a load error, never a silent empty lane (r2 posture)", async () => {
    mockListProjectEpics.mockRejectedValue(new Error("503 from epics"));
    renderBoard();
    const err = await screen.findByTestId("board-read-error");
    expect(err.querySelector("[data-board-error]")?.textContent).toContain("503 from epics");
    expect(screen.queryByTestId("board-empty")).toBeNull();
  });

  it("a selected epic that vanishes from the read model falls back to the picker (never a stale scope)", async () => {
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");

    // Select E2, then make the read model drop it (refetch without the
    // E2 row — a drag's invalidation drives the refetch).
    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    expect(screen.getByTestId("epic-band-epic-uuid-2").getAttribute("data-selected")).toBe("true");

    const withoutE2 = defaultTasks().filter((t) => t.id !== "task-uuid-5");
    mockListProjectTasks.mockImplementation(async (pid: string) => withoutE2.filter((t) => t.project_id === pid));
    mockMoveStage.mockResolvedValue({});
    dragCardTo("tsk-aaaa0001", "in_progress");
    await waitFor(() => expect(screen.queryByTestId("epic-band-epic-uuid-2")).toBeNull());

    // New task must open the PICKER (no selection), never a create
    // dialog scoped to the vanished epic.
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    expect(await screen.findByTestId("epic-picker")).toBeInTheDocument();
    expect(mockCreateTask).not.toHaveBeenCalled();
  });

  // --- Finding 2: filtered bands distinguish empty from filtered-out -------

  it("a band whose tasks are all filtered out says so — not 'No tasks yet.'", async () => {
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");

    // E2's only task (tsk-eeee0005) is in the lane; search for E1's
    // task titles so E2's row is filtered out of the lane.
    const search = screen.getByTestId("board-search");
    fireEvent.change(search, { target: { value: "Write charter" } });

    const bandE2 = screen.getByTestId("epic-band-epic-uuid-2");
    await waitFor(() => expect(bandE2.querySelector('[data-testid="band-filtered-empty-epic-uuid-2"]')).not.toBeNull());
    expect(bandE2.querySelector('[data-testid="band-empty-epic-uuid-2"]')).toBeNull();
  });

  it("a taskless epic's band keeps 'No tasks yet.' when no filter hides anything", async () => {
    mockListProjectEpics.mockResolvedValue([ZERO_TASK_EPIC]);
    renderBoard();
    const band = await screen.findByTestId("epic-band-epic-uuid-zero");
    expect(band.querySelector('[data-testid="band-empty-epic-uuid-zero"]')).not.toBeNull();
    expect(band.querySelector('[data-testid="band-filtered-empty-epic-uuid-zero"]')).toBeNull();
  });

  // --- Finding 3a: unhappy paths for the new creation flows -----------------

  it("a failed swimlane New-epic POST keeps the dialog open with the error and the swimlane target", async () => {
    mockCreateEpic.mockRejectedValue(boardError(409, { error: "board: epic title already exists", code: "epic_title" }));
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    fireEvent.click(screen.getByTestId("new-epic-prj-bbbb2222"));
    const title = await screen.findByTestId("epic-title");
    fireEvent.change(title, { target: { value: "Dup" } });
    fireEvent.click(screen.getByTestId("epic-create"));

    // The error renders INSIDE the still-open dialog, target + fields preserved.
    const notice = await screen.findByText(/epic title already exists/);
    expect(notice).toBeInTheDocument();
    expect(screen.getByText("New epic in Borealis")).toBeInTheDocument();
    expect((screen.getByTestId("epic-title") as HTMLInputElement).value).toBe("Dup");
  });

  it("a failed New-task POST keeps TaskDialog open with the error and preserved fields", async () => {
    mockCreateTask.mockRejectedValue(boardError(409, { error: "board: guard task_guard: criteria required", code: "task_guard" }));
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");

    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    const title = await screen.findByTestId("task-title");
    fireEvent.change(title, { target: { value: "Doomed task" } });
    fireEvent.click(screen.getByTestId("task-save"));

    await screen.findByText(/criteria required/);
    expect(screen.getByText("in E2")).toBeInTheDocument();
    expect((screen.getByTestId("task-title") as HTMLInputElement).value).toBe("Doomed task");
  });

  // --- Finding 3b: reversion guards for the r1 fixes ------------------------

  it("band container and task-row clicks never select (selection is the header row only)", async () => {
    renderBoard();
    // epic-uuid-1 spans BOTH lanes (same-id epic across projects) —
    // scope every epic-uuid-1 query to Apollo's swimlane.
    const lane = await screen.findByTestId("swimlane-prj-aaaa1111");
    await waitFor(() => expect(lane.querySelector('[data-testid="epic-band-epic-uuid-1"]')).not.toBeNull());

    // A task-row click opens the task dialog AND leaves the band
    // unselected (a container-level onClick would select on the bubbling
    // click — the exact hit-testing bug the r1 fix moved to the header).
    fireEvent.click(lane.querySelector('[data-testid="band-task-epic-uuid-1-tsk-aaaa0001"]')!);
    await screen.findByTestId("task-title");
    expect(lane.querySelector('[data-testid="epic-band-epic-uuid-1"]')!.getAttribute("data-selected")).toBeNull();

    // The toggle collapses without selecting.
    fireEvent.click(screen.getByTestId("band-toggle-epic-uuid-2"));
    await waitFor(() => expect(screen.queryByTestId("band-task-epic-uuid-2-tsk-eeee0005")).toBeNull());
    expect(screen.getByTestId("epic-band-epic-uuid-2").getAttribute("data-selected")).toBeNull();

    // A click on the band CONTAINER itself (outside the header row)
    // never selects.
    fireEvent.click(lane.querySelector('[data-testid="epic-band-epic-uuid-1"]')!);
    expect(lane.querySelector('[data-testid="epic-band-epic-uuid-1"]')!.getAttribute("data-selected")).toBeNull();
  });

  it("each swimlane renders ONLY its own project's task rows (seedBoard exact-pid guard)", async () => {
    const emptyProject: BoardProject = {
      id: "proj-uuid-c", short_id: "prj-cccc3333", user_id: "u1", name: "Ceres",
      target_workspace_id: "ws-1", worker_slots: 1, tags: [], status: "active",
      created_at: "2026-10-03T00:00:00Z", updated_at: "2026-10-03T00:00:00Z",
    };
    seedBoard(defaultTasks(), [PROJECT_A, PROJECT_B, emptyProject]);
    renderBoard();
    const laneC = await screen.findByTestId("swimlane-prj-cccc3333");

    // Ceres owns zero tasks: NO task rows may render in its lane, and
    // its bands area must not grow task-derived bands from another
    // project's rows (the pre-r1 mock returned Apollo's tasks for any
    // pid ≠ proj-uuid-b — exactly this leak).
    await screen.findByTestId("swimlane-prj-aaaa1111");
    await waitFor(() => expect(laneC.querySelectorAll('[data-testid^="band-task-"]').length).toBe(0));
    expect(laneC.querySelectorAll('[data-testid^="epic-band-"]').length).toBe(0);
    expect(laneC.querySelectorAll('[data-testid^="task-card-"]').length).toBe(0);
  });
});

// #1832 r3 — the second review round's finding: the session's
// knownEpics must never override the server list once it (re)arrives.
// The known copy bridges ONLY the POST→refetch window; a successful
// list response is authoritative for existence, progress, and metadata.
describe("BoardPage #1832 r3 — server truth supersedes session-known epics", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("band progress follows the refetched server list after task creation (not the stale POST copy)", async () => {
    // Server list: initially without the epic; after the create-task
    // invalidation the refetch reports it WITH the fresh row count.
    mockListProjectEpics
      .mockResolvedValueOnce([])
      .mockResolvedValue([{
        id: "epic-uuid-live", project_id: PROJECT_A.id, short_id: "epc-live0000",
        title: "Live epic", outcome: "", status: "active", tags: [],
        created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z",
        progress_done: 0, progress_total: 1, progress_derived: false,
      }]);
    mockCreateEpic.mockResolvedValue({
      id: "epic-uuid-live", project_id: PROJECT_A.id, short_id: "epc-live0000",
      title: "Live epic", outcome: "", status: "active", tags: [],
      created_at: "", updated_at: "", progress_done: 0, progress_total: 0, progress_derived: false,
    });
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    // Create the epic (swimlane button) — the POST copy (0/0) bridges
    // until the refetch lands.
    fireEvent.click(screen.getByTestId("new-epic-prj-aaaa1111"));
    const epicTitle = await screen.findByTestId("epic-title");
    fireEvent.change(epicTitle, { target: { value: "Live epic" } });
    fireEvent.click(screen.getByTestId("epic-create"));
    const band = await screen.findByTestId("epic-band-epic-uuid-live");
    expect(band.textContent).toContain("Live epic");

    // Select it and create a task inside (success → invalidation → the
    // epics list refetch reports the task: 0 done / 1 total).
    fireEvent.click(screen.getByTestId("band-header-epic-uuid-live"));
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    const taskTitle = await screen.findByTestId("task-title");
    expect(screen.getByText("in Live epic")).toBeInTheDocument();
    fireEvent.change(taskTitle, { target: { value: "First" } });
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() => expect(mockCreateTask).toHaveBeenCalled());

    // The band must show the SERVER's 1-total progress — the session's
    // stale 0/0 POST copy must not survive the list's arrival.
    await waitFor(() => expect(band.textContent).toContain("0/1"), { timeout: 4000 });
  });

  it("a session-known epic follows the server list for metadata and deletion", async () => {
    // The drawer GET mints the known copy (paused); the list refetch —
    // triggered by the create-task invalidation — is authoritative.
    mockGetEpic.mockResolvedValue({
      id: "epic-uuid-follow", project_id: PROJECT_A.id, short_id: "epc-follow00",
      title: "Follow epic", outcome: "", status: "paused", tags: [],
      created_at: "", updated_at: "", progress_done: 0, progress_total: 0, progress_derived: false,
    });
    mockListProjectEpics.mockResolvedValue([]);
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // Open the drawer via the epic chip's double-click (adds the known
    // copy for an epic the task-derived model doesn't carry) — the band
    // then renders from the known bridge.
    fireEvent.dblClick(screen.getByText("E1", { selector: "button" }));
    await screen.findByTestId("epic-status");
    // The known bridge renders its band while the list hasn't arrived.
    await screen.findByTestId("epic-band-epic-uuid-follow");

    // Trigger the invalidation (any successful board write).
    fireEvent.click(screen.getByTestId("band-header-epic-uuid-2"));
    fireEvent.click(screen.getByTestId("new-task-prj-aaaa1111"));
    const t = await screen.findByTestId("task-title");
    fireEvent.change(t, { target: { value: "Trigger" } });
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() => expect(mockCreateTask).toHaveBeenCalled());

    // The server list (arrived, without the epic) is authoritative:
    // the session-known paused copy must not keep its band alive.
    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    await waitFor(() => expect(lane.querySelector('[data-testid="epic-band-epic-uuid-follow"]')).toBeNull(), { timeout: 4000 });
  });
});
