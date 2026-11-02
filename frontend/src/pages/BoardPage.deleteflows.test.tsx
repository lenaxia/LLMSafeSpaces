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
  mockListProjects,
  mockDeleteTask,
  mockDeleteEpic,
  mockArchiveProject,
  mockUnarchiveProject,
  boardError,
  PROJECT_A,
  PROJECT_ARCHIVED,
  TASK_IN_PROGRESS,
} from "./boardTestUtils";
import type { BoardProject } from "../api/board";

// #1832 item 3: kebab menus on project / epic / task with deletion
// flows. Project stays archive/unarchive ONLY (#1747 — project delete
// is eliminated). Task delete exposes the reconnect-dependents checkbox
// (BOTH semantics against the API). Epic delete surfaces in-flight
// workers and refuses-with-force-confirm, matching the API (force
// aborts in-flight post-commit).

const EPIC_STATE_REFUSAL = boardError(409, {
  error:
    "board: guard epic_state: claims in flight under the epic — force required (force=true deletes, then aborts each in-flight worker post-commit)",
  code: "epic_state",
});

/** Opens the task kebab on a card and clicks Delete story… */
async function openTaskDelete(shortId: string) {
  fireEvent.click(screen.getByTestId(`task-kebab-${shortId}`));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Delete story…" }));
  return screen.findByTestId("task-delete-modal");
}

/** Opens the epic kebab on a band (scoped to the Apollo swimlane — the
 * fixtures' epic-uuid-1 spans both lanes, so the band-level kebab is
 * addressed inside its lane) and clicks Delete epic…. */
async function openEpicDelete(epicId: string) {
  const lane = screen.getByTestId("swimlane-prj-aaaa1111");
  fireEvent.click(within(lane).getByTestId(`epic-kebab-${epicId}`));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Delete epic…" }));
  return screen.findByTestId("epic-delete-modal");
}

describe("BoardPage #1832 item 3 — kebab menus + delete flows", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // clearAllMocks does NOT pop queued once-implementations — a test
    // that fails before consuming its reject/resolve pair would poison
    // the next test's calls. Reset the sequenced mocks outright.
    mockDeleteEpic.mockReset();
    mockDeleteTask.mockReset();
    seedBoard();
  });

  // --- Kebabs exist at all three levels -------------------------------------

  it("renders a kebab in every project swimlane header (archive item, NO delete — #1747)", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    for (const sid of ["prj-aaaa1111", "prj-bbbb2222"]) {
      const kebab = screen.getByTestId(`project-kebab-${sid}`);
      expect(kebab).toBeInTheDocument();
      fireEvent.click(kebab);
      const menu = await screen.findByRole("menu");
      expect(within(menu).getByRole("menuitem", { name: "Archive project" })).toBeInTheDocument();
      // #1747: project delete is ELIMINATED — the menu must not offer it.
      expect(within(menu).queryByRole("menuitem", { name: /Delete project/i })).toBeNull();
      fireEvent.mouseDown(document.body); // close (outside-click — the menu listens on mousedown)
      await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    }
  });

  it("renders a kebab on every epic band header (delete item)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // Scoped to Apollo's lane — the fixture's epic-uuid-1 also renders
    // as a band in Borealis (same epic id under both projects).
    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    for (const eid of ["epic-uuid-1", "epic-uuid-2"]) {
      fireEvent.click(within(lane).getByTestId(`epic-kebab-${eid}`));
      const menu = await screen.findByRole("menu");
      expect(within(menu).getByRole("menuitem", { name: "Delete epic…" })).toBeInTheDocument();
      fireEvent.mouseDown(document.body);
      await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    }
  });

  it("renders a kebab on every task card (delete item; click does not open the editor)", async () => {
    renderBoard();
    await screen.findByTestId("task-kebab-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("task-kebab-tsk-cccc0003"));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Delete story…" })).toBeInTheDocument();

    // The kebab click must NOT have opened the task editor (stopPropagation).
    expect(screen.queryByTestId("task-title")).toBeNull();
  });

  // --- Project kebab: archive / unarchive ONLY -------------------------------

  it("Archive project calls the API with the project id and invalidates the board", async () => {
    mockArchiveProject.mockResolvedValue({ ...PROJECT_A, status: "archived" });
    const callsBefore = mockListProjects.mock.calls.length;
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    fireEvent.click(screen.getByTestId("project-kebab-prj-aaaa1111"));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Archive project" }));

    await waitFor(() => expect(mockArchiveProject).toHaveBeenCalledWith("proj-uuid-a"));
    // The read model is invalidated after the write.
    await waitFor(() => expect(mockListProjects.mock.calls.length).toBeGreaterThan(callsBefore + 1));
    expect(mockUnarchiveProject).not.toHaveBeenCalled();
  });

  it("an archived project's kebab offers Unarchive (and calls it)", async () => {
    const archived: BoardProject = PROJECT_ARCHIVED;
    seedBoard(
      defaultTasks().filter((t) => t.project_id === archived.id),
      [PROJECT_A, archived],
    );
    mockUnarchiveProject.mockResolvedValue({ ...archived, status: "active" });
    renderBoard();
    // #1964: the archived lane lives inside the collapsed bottom
    // drawer — open BOTH levels before its kebab is reachable.
    fireEvent.click(await screen.findByTestId("archived-projects-toggle"));
    fireEvent.click(await screen.findByTestId("archived-project-toggle-prj-arch3333"));
    await screen.findByTestId("swimlane-prj-arch3333");

    fireEvent.click(screen.getByTestId("project-kebab-prj-arch3333"));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unarchive project" }));

    await waitFor(() => expect(mockUnarchiveProject).toHaveBeenCalledWith("proj-uuid-arc"));
    expect(mockArchiveProject).not.toHaveBeenCalled();
  });

  // --- Task delete modal: reconnect checkbox, BOTH semantics ------------------

  it("task delete modal lists the dependents and defaults to UNCHECKED reconnect (plain unblock)", async () => {
    mockDeleteTask.mockResolvedValue({
      item_kind: "task", id: "task-uuid-3", short_id: "tsk-cccc0003",
      orphaned_dependents: ["tsk-eeee0005"],
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    await openTaskDelete("tsk-cccc0003");

    // The dependent is surfaced pre-flight (tsk-eeee0005 is blocked-by this task).
    expect(screen.getByTestId("task-delete-dependents").textContent).toContain("tsk-eeee0005");
    const box = screen.getByTestId("reconnect-dependents") as HTMLInputElement;
    expect(box.checked).toBe(false);

    fireEvent.click(screen.getByTestId("confirm-delete-task"));
    // Semantics 1 — unchecked: NO reconnect flag reaches the API.
    await waitFor(() => expect(mockDeleteTask).toHaveBeenCalledWith("tsk-cccc0003", false));
    await waitFor(() => expect(screen.queryByTestId("task-delete-modal")).toBeNull());
    // The delete result is surfaced (dependents unblocked, audit events).
    await waitFor(() =>
      expect(screen.getByTestId("board-notice").textContent).toContain("tsk-eeee0005"));
  });

  it("CHECKED reconnect sends reconnect=true to the API and surfaces the inherited edges", async () => {
    mockDeleteTask.mockResolvedValue({
      item_kind: "task", id: "task-uuid-3", short_id: "tsk-cccc0003",
      orphaned_dependents: ["tsk-eeee0005"],
      reconnected: ["tsk-eeee0005→tsk-aaaa0001"],
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    await openTaskDelete("tsk-cccc0003");
    fireEvent.click(screen.getByTestId("reconnect-dependents"));

    fireEvent.click(screen.getByTestId("confirm-delete-task"));
    // Semantics 2 — checked: reconnect=true reaches the API.
    await waitFor(() => expect(mockDeleteTask).toHaveBeenCalledWith("tsk-cccc0003", true));
    await waitFor(() => expect(screen.queryByTestId("task-delete-modal")).toBeNull());
    await waitFor(() =>
      expect(screen.getByTestId("board-notice").textContent).toContain("tsk-eeee0005→tsk-aaaa0001"));
  });

  it("a refused task delete keeps the modal open with the guard code rendered", async () => {
    mockDeleteTask.mockRejectedValue(
      boardError(409, { error: "board: guard task_paused: paused tasks refuse delete", code: "task_paused" }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    await openTaskDelete("tsk-cccc0003");
    fireEvent.click(screen.getByTestId("confirm-delete-task"));

    const modal = screen.getByTestId("task-delete-modal");
    await waitFor(() =>
      expect(modal.querySelector("[data-board-error]")?.getAttribute("data-board-error")).toBe("task_paused"));
    expect(modal).toBeInTheDocument();
  });

  // --- Epic delete modal: in-flight surfacing + refuse/force-confirm ----------

  it("epic delete surfaces in-flight workers; the refusal reveals force-confirm (API-matching)", async () => {
    // epic-uuid-1 holds tsk-bbbb0002 (in_progress + assignee) — in-flight.
    expect(TASK_IN_PROGRESS.stage).toBe("in_progress");
    mockDeleteEpic
      .mockRejectedValueOnce(EPIC_STATE_REFUSAL)
      .mockResolvedValueOnce({
        item_kind: "epic", id: "epic-uuid-1", short_id: "epic-epic-uu",
        cascade_tasks: ["tsk-aaaa0001", "tsk-bbbb0002", "tsk-cccc0003", "tsk-dddd0004"],
      });
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    await openEpicDelete("epic-uuid-1");

    // The in-flight worker is surfaced from the read model, pre-flight.
    expect(screen.getByTestId("epic-delete-inflight").textContent).toContain("tsk-bbbb0002");
    // The cascade size is honest (7 tasks carry this epic id after the
    // failed-state and escalated fixtures joined epic-uuid-1: the five
    // Apollo rows + the two Borealis rows sharing the fixture's epic id).
    expect(screen.getByTestId("epic-delete-cascade-count")).toHaveTextContent("7");

    // First attempt: NO force (matches the API's refuse-unless-force).
    fireEvent.click(screen.getByTestId("confirm-delete-epic"));
    await waitFor(() => expect(mockDeleteEpic).toHaveBeenCalledWith("epic-uuid-1", false));

    // The refusal renders; the force-confirm is revealed but gated.
    const modal = screen.getByTestId("epic-delete-modal");
    await waitFor(() =>
      expect(modal.querySelector("[data-board-error]")?.getAttribute("data-board-error")).toBe("epic_state"));
    const forceBtn = screen.getByTestId("force-delete-epic") as HTMLButtonElement;
    expect(forceBtn.disabled).toBe(true);
    fireEvent.click(screen.getByTestId("force-abort-confirm"));
    expect(forceBtn.disabled).toBe(false);

    // Force: force=true reaches the API; success closes.
    fireEvent.click(forceBtn);
    await waitFor(() => expect(mockDeleteEpic).toHaveBeenCalledWith("epic-uuid-1", true));
    await waitFor(() => expect(screen.queryByTestId("epic-delete-modal")).toBeNull());
  });

  it("epic delete with NO in-flight workers deletes without force", async () => {
    // epic-uuid-2 holds only tsk-eeee0005 (backlog) — nothing in flight.
    mockDeleteEpic.mockResolvedValue({
      item_kind: "epic", id: "epic-uuid-2", short_id: "epic-epic-uu2",
      cascade_tasks: ["tsk-eeee0005"],
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");

    await openEpicDelete("epic-uuid-2");

    expect(screen.queryByTestId("epic-delete-inflight")).toBeNull();
    expect(screen.getByTestId("epic-delete-cascade-count")).toHaveTextContent("1");
    fireEvent.click(screen.getByTestId("confirm-delete-epic"));

    await waitFor(() => expect(mockDeleteEpic).toHaveBeenCalledWith("epic-uuid-2", false));
    await waitFor(() => expect(screen.queryByTestId("epic-delete-modal")).toBeNull());
    expect(mockDeleteEpic.mock.calls[0]?.[1]).toBe(false);
  });

  it("a non-refusal epic delete error keeps the modal open (no force UI)", async () => {
    mockDeleteEpic.mockRejectedValue(boardError(404, { error: 'board: epic "epic-uuid-2" not found' }));
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");

    await openEpicDelete("epic-uuid-2");
    fireEvent.click(screen.getByTestId("confirm-delete-epic"));

    const modal = screen.getByTestId("epic-delete-modal");
    await waitFor(() => expect(modal.querySelector("[data-board-error]")).not.toBeNull());
    expect(screen.queryByTestId("force-delete-epic")).toBeNull();
    expect(modal).toBeInTheDocument();
  });

  // --- No accidental surface overlap ------------------------------------------

  it("the epic kebab does not disturb band selection (stopPropagation)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    fireEvent.click(within(lane).getByTestId("epic-kebab-epic-uuid-2"));
    await screen.findByRole("menu");
    fireEvent.mouseDown(document.body);
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

    // No selection was toggled by opening/closing the kebab menu.
    expect(screen.getByTestId("task-card-tsk-aaaa0001").getAttribute("data-dimmed")).toBeNull();
  });
});
