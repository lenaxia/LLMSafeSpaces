// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor, within } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import {
  renderBoard,
  seedBoard,
  defaultTasks,
  mockListProjectEpics,
  mockPauseTask,
  mockResumeTask,
  mockRearmTask,
  mockMoveStage,
  mockUpdateTask,
  mockActivateEpic,
  mockPauseEpic,
  mockArchiveEpic,
  mockUnarchiveEpic,
  boardError,
  TASK_BACKLOG,
  TASK_IN_PROGRESS,
  TASK_IN_REVIEW,
  TASK_DONE,
  TASK_PENDING,
  TASK_ESCALATED,
  TASK_PAUSED,
  PROJECT_A,
} from "./boardTestUtils";
import type { BoardEpic } from "../api/board";

// The story card kebab's per-entity action surface (owner ask
// 2026-11-14): every item is CONDITIONAL on the row's state — the
// matrix below mirrors buildStoryKebabItems 1:1 — and every action
// fires its REAL endpoint with the right params (optimistic flip +
// rollback on refusal). Epic bands carry the lifecycle matrix
// (activate/pause/archive/unarchive per the engine's allowed-map).

const epicRow = (id: string, shortId: string, title: string, status: string): BoardEpic => ({
  id, project_id: PROJECT_A.id, short_id: shortId, title, outcome: "", status,
  tags: [], created_at: "", updated_at: "", progress_done: 0, progress_total: 1, progress_derived: true,
});

const EPIC_DRAFT = epicRow("epic-uuid-d1", "epc-d1d11111", "Draft lane", "draft");
const EPIC_ACTIVE = epicRow("epic-uuid-a2", "epc-a2a22222", "Active lane", "active");
const EPIC_PAUSED = epicRow("epic-uuid-p3", "epc-p3p33333", "Paused lane", "paused");
const EPIC_COMPLETED = epicRow("epic-uuid-c4", "epc-c4c44444", "Completed lane", "completed");
const EPIC_ARCHIVED = epicRow("epic-uuid-r5", "epc-r5r55555", "Archived lane", "archived");

/** Serves epic rows from a mutable array — the post-action refetch must
 * carry the server's post-write truth (knownEpics' bridge expires when
 * the list re-lands, by design). */
function serveEpics(rows: BoardEpic[]) {
  mockListProjectEpics.mockImplementation(async (pid: string) => rows.filter((e) => e.project_id === pid));
}

/** Opens the story kebab and returns the open menu element. */
async function openStoryMenu(shortId: string): Promise<HTMLElement> {
  fireEvent.click(await screen.findByTestId(`task-kebab-${shortId}`));
  return screen.findByRole("menu");
}

/** Opens an epic band kebab and returns the open menu element. */
async function openEpicMenu(epicId: string): Promise<HTMLElement> {
  fireEvent.click(await screen.findByTestId(`epic-kebab-${epicId}`));
  return screen.findByRole("menu");
}

function labelsOf(menu: HTMLElement): string[] {
  return within(menu).getAllByRole("menuitem").map((el: HTMLElement) => el.textContent ?? "");
}

describe("BoardPage story card kebab — the state matrix", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard([...defaultTasks(), TASK_PAUSED]);
    serveEpics([]);
  });

  it("backlog: Pause + criteria + Delete only — no stage moves, no Resume, no Re-arm", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_BACKLOG.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Pause story");
    expect(labels).toContain("Delete story…");
    expect(labels).not.toContain("Resume story");
    expect(labels).not.toContain("Re-arm story (reset attempts)");
    expect(labels).not.toContain("Move to failed…");
    expect(labels).not.toContain("Requeue to backlog");
    expect(labels).not.toContain("Resume work (re-claim)");
  });

  it("in_progress: Pause + Move to failed… + criteria + Delete", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Pause story");
    expect(labels).toContain("Move to failed…");
    expect(labels).toContain("Delete story…");
    // Worker-owned transitions are NEVER offered (the kebab is the
    // manager surface): no forward exit, no review targets.
    expect(labels.some((l) => l.toLowerCase().includes("review"))).toBe(false);
    expect(labels).not.toContain("Requeue to backlog");
    expect(labels).not.toContain("Re-arm story (reset attempts)");
  });

  it("in_review: Pause + Move to failed… (the self-fail arm) — no bounce item", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_REVIEW.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Pause story");
    expect(labels).toContain("Move to failed…");
    expect(labels.some((l) => l.toLowerCase().includes("in progress"))).toBe(false);
  });

  it("pending_input: the two flag exits — Requeue + Resume work, never Move to failed", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_PENDING.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Pause story");
    expect(labels).toContain("Requeue to backlog");
    expect(labels).toContain("Resume work (re-claim)");
    expect(labels).not.toContain("Move to failed…");
  });

  it("paused: Resume replaces Pause", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_PAUSED.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Resume story");
    expect(labels).not.toContain("Pause story");
  });

  it("failed+escalated: Re-arm offered", async () => {
    renderBoard();
    // The Failed side column starts collapsed — expand it first.
    fireEvent.click(within(await screen.findByTestId("swimlane-prj-bbbb2222")).getByTestId("side-columns-toggle"));
    const menu = await openStoryMenu(TASK_ESCALATED.short_id);
    expect(labelsOf(menu)).toContain("Re-arm story (reset attempts)");
  });

  it("done is TERMINAL: no lifecycle, no stage items — criteria + Delete only", async () => {
    renderBoard();
    const menu = await openStoryMenu(TASK_DONE.short_id);
    const labels = labelsOf(menu);
    expect(labels).toContain("Delete story…");
    expect(labels).not.toContain("Pause story");
    expect(labels).not.toContain("Resume story");
    expect(labels).not.toContain("Move to failed…");
    expect(labels).not.toContain("Requeue to backlog");
    expect(labels).not.toContain("Resume work (re-claim)");
    expect(labels).not.toContain("Re-arm story (reset attempts)");
  });
});

describe("BoardPage story card kebab — actions fire their real endpoints", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard([...defaultTasks(), TASK_PAUSED]);
    serveEpics([]);
  });

  it("Pause fires pauseTask and flips the paused badge optimistically", async () => {
    let release!: (t: unknown) => void;
    mockPauseTask.mockImplementation(
      (id: string) =>
        new Promise((res) => {
          expect(id).toBe(TASK_IN_PROGRESS.short_id);
          release = res;
        }),
    );
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Pause story" }));

    // Optimistic: the badge lands BEFORE the server answers.
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-bbbb0002").textContent).toContain("paused"));

    release({ ...TASK_IN_PROGRESS, paused: true });
    await waitFor(() => expect(mockPauseTask).toHaveBeenCalledWith("tsk-bbbb0002"));
    await screen.findByTestId("board-notice");
  });

  it("Pause refusal rolls the badge back and renders the page-level guard error", async () => {
    mockPauseTask.mockRejectedValue(
      boardError(409, { error: "board: guard paused: terminal rows refuse pause", code: "paused" }),
    );
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Pause story" }));

    await waitFor(() => expect(mockPauseTask).toHaveBeenCalled());
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("paused");
    // Rollback: the optimistic badge is gone.
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-bbbb0002").textContent).not.toContain("paused"));
  });

  it("Resume fires resumeTask on a paused row", async () => {
    mockResumeTask.mockResolvedValue({ ...TASK_PAUSED, paused: false });
    renderBoard();
    const menu = await openStoryMenu(TASK_PAUSED.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Resume story" }));

    await waitFor(() => expect(mockResumeTask).toHaveBeenCalledWith("tsk-99990009"));
  });

  it("Re-arm fires rearmTask (not move-stage) on the failed/escalated row", async () => {
    mockRearmTask.mockResolvedValue({ ...TASK_ESCALATED, stage: "backlog", attempts: 0 });
    renderBoard();
    // The Failed side column starts collapsed — expand it first.
    fireEvent.click(within(await screen.findByTestId("swimlane-prj-bbbb2222")).getByTestId("side-columns-toggle"));
    const menu = await openStoryMenu(TASK_ESCALATED.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Re-arm story (reset attempts)" }));

    await waitFor(() => expect(mockRearmTask).toHaveBeenCalledWith("tsk-ffff0008"));
    expect(mockMoveStage).not.toHaveBeenCalled();
  });

  it("Requeue fires moveStage(backlog) — the pending_input re-queue exit", async () => {
    mockMoveStage.mockResolvedValue({ ...TASK_PENDING, pending_input: false, stage: "backlog" });
    renderBoard();
    const menu = await openStoryMenu(TASK_PENDING.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Requeue to backlog" }));

    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-ffff0006", "backlog", undefined));
  });

  it("Resume work fires moveStage(in_progress) — the re-claim exit", async () => {
    mockMoveStage.mockResolvedValue({ ...TASK_PENDING, pending_input: false });
    renderBoard();
    const menu = await openStoryMenu(TASK_PENDING.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Resume work (re-claim)" }));

    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-ffff0006", "in_progress", undefined));
  });

  it("Move to failed fires moveStage(failed) on a stuck in_progress row", async () => {
    mockMoveStage.mockResolvedValue({ ...TASK_IN_PROGRESS, stage: "failed" });
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Move to failed…" }));

    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-bbbb0002", "failed", undefined));
    // The refused-drag lane owns failures: no page alert on success.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("criterion toggle fires updateTask with the {index, met} two-way form, both directions", async () => {
    mockUpdateTask.mockImplementation(async (_id: string, input: { setCriteriaMet?: { index: number; met: boolean } }) => ({
      acceptance_criteria: [],
      mutated_criterion: input.setCriteriaMet,
    }));
    renderBoard();

    // Unmet → met (index 1).
    let menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: /^○ 2\./ }));
    await waitFor(() => expect(mockUpdateTask).toHaveBeenCalledWith("tsk-bbbb0002", { setCriteriaMet: { index: 1, met: true } }));

    // Met → unmet (index 0) — the #1837 P2b both-ways pin.
    mockUpdateTask.mockClear();
    menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: /^✓ 1\./ }));
    await waitFor(() => expect(mockUpdateTask).toHaveBeenCalledWith("tsk-bbbb0002", { setCriteriaMet: { index: 0, met: false } }));
  });

  it("criterion toggle refusal rolls the glyph back with the page-level error", async () => {
    mockUpdateTask.mockRejectedValue(
      boardError(400, { error: "board: input: setCriteriaMet index 5 out of bounds", code: "input" }),
    );
    renderBoard();
    const menu = await openStoryMenu(TASK_IN_PROGRESS.short_id);
    fireEvent.click(within(menu).getByRole("menuitem", { name: /^○ 2\./ }));

    await waitFor(() => expect(mockUpdateTask).toHaveBeenCalled());
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("input");
    // Rollback: the kebab still shows the ORIGINAL unmet glyph.
    fireEvent.click(await screen.findByTestId(`task-kebab-${TASK_IN_PROGRESS.short_id}`));
    const reopened = within(screen.getByRole("menu"));
    expect(reopened.getByRole("menuitem", { name: /^○ 2\./ })).toBeInTheDocument();
  });
});

describe("BoardPage epic band kebab — the lifecycle matrix", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard(defaultTasks().filter((t) => t.project_id === PROJECT_A.id), [PROJECT_A]);
    serveEpics([EPIC_DRAFT, EPIC_ACTIVE, EPIC_PAUSED, EPIC_COMPLETED, EPIC_ARCHIVED]);
  });

  it("draft: Activate + Archive (+ Delete); activating fires activateEpic with the short id and flips the badge", async () => {
    const activated = { ...EPIC_DRAFT, status: "active" };
    mockActivateEpic.mockImplementation(async () => {
      serveEpics([activated, EPIC_ACTIVE, EPIC_PAUSED, EPIC_COMPLETED, EPIC_ARCHIVED]);
      return { epic: activated, initial_ready_set: ["tsk-aaaa0001"] };
    });
    renderBoard();
    const band = within(await screen.findByTestId("swimlane-prj-aaaa1111"));
    expect(band.getByTestId("band-title-epic-uuid-d1").textContent).toBe("Draft lane");

    const menu = await openEpicMenu("epic-uuid-d1");
    const labels = labelsOf(menu);
    expect(labels).toContain("Activate epic");
    expect(labels).toContain("Archive epic");
    expect(labels).not.toContain("Pause epic");
    expect(labels).not.toContain("Unarchive epic");

    fireEvent.click(within(menu).getByRole("menuitem", { name: "Activate epic" }));
    await waitFor(() => expect(mockActivateEpic).toHaveBeenCalledWith("epc-d1d11111"));
    // The draft BADGE leaves (the title stays — same epic, new status).
    const lane = within(await screen.findByTestId("swimlane-prj-aaaa1111"));
    await waitFor(() => expect(lane.queryByText("draft")).toBeNull());
    expect(lane.getByTestId("band-title-epic-uuid-d1").textContent).toBe("Draft lane");
  });

  it("active: Pause + Archive; pausing fires pauseEpic", async () => {
    const paused = { ...EPIC_ACTIVE, status: "paused" };
    mockPauseEpic.mockImplementation(async () => {
      serveEpics([EPIC_DRAFT, paused, EPIC_PAUSED, EPIC_COMPLETED, EPIC_ARCHIVED]);
      return paused;
    });
    renderBoard();
    const menu = await openEpicMenu("epic-uuid-a2");
    const labels = labelsOf(menu);
    expect(labels).toContain("Pause epic");
    expect(labels).toContain("Archive epic");
    expect(labels).not.toContain("Activate epic");

    fireEvent.click(within(menu).getByRole("menuitem", { name: "Pause epic" }));
    await waitFor(() => expect(mockPauseEpic).toHaveBeenCalledWith("epc-a2a22222"));
  });

  it("paused/completed: Activate + Archive (the re-open and its storage lever)", async () => {
    renderBoard();
    let menu = await openEpicMenu("epic-uuid-p3");
    expect(labelsOf(menu)).toEqual(expect.arrayContaining(["Activate epic", "Archive epic"]));
    // The portal closes on outside MOUSEDOWN (click alone doesn't fire it).
    fireEvent.mouseDown(document.body);
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    menu = await openEpicMenu("epic-uuid-c4");
    expect(labelsOf(menu)).toEqual(expect.arrayContaining(["Activate epic", "Archive epic"]));
  });

  it("archived: Unarchive ONLY (+ Delete) — no Pause, no Archive; fires unarchiveEpic", async () => {
    const restored = { ...EPIC_ARCHIVED, status: "active" };
    mockUnarchiveEpic.mockImplementation(async () => {
      serveEpics([EPIC_DRAFT, EPIC_ACTIVE, EPIC_PAUSED, EPIC_COMPLETED, restored]);
      return restored;
    });
    renderBoard();
    const menu = await openEpicMenu("epic-uuid-r5");
    const labels = labelsOf(menu);
    expect(labels).toContain("Unarchive epic");
    expect(labels).not.toContain("Pause epic");
    expect(labels).not.toContain("Archive epic");
    expect(labels).not.toContain("Activate epic");

    fireEvent.click(within(menu).getByRole("menuitem", { name: "Unarchive epic" }));
    await waitFor(() => expect(mockUnarchiveEpic).toHaveBeenCalledWith("epc-r5r55555"));
  });

  it("Archive on an active epic fires archiveEpic", async () => {
    const archived = { ...EPIC_ACTIVE, status: "archived" };
    mockArchiveEpic.mockImplementation(async () => {
      serveEpics([EPIC_DRAFT, archived, EPIC_PAUSED, EPIC_COMPLETED, EPIC_ARCHIVED]);
      return archived;
    });
    renderBoard();
    const menu = await openEpicMenu("epic-uuid-a2");
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Archive epic" }));

    await waitFor(() => expect(mockArchiveEpic).toHaveBeenCalledWith("epc-a2a22222"));
  });

  it("lifecycle refusal renders the page-level guard error and keeps the state", async () => {
    mockActivateEpic.mockRejectedValue(
      boardError(409, { error: "board: guard epic_state: 2 cross-epic dependenc(ies) enter inactive epics — acknowledge…", code: "epic_state" }),
    );
    renderBoard();
    const menu = await openEpicMenu("epic-uuid-d1");
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Activate epic" }));

    await waitFor(() => expect(mockActivateEpic).toHaveBeenCalled());
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("epic_state");
    // State stays: the band still reads draft.
    expect(within(screen.getByTestId("swimlane-prj-aaaa1111")).getByText("Draft lane")).toBeInTheDocument();
  });
});
