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
  PROJECT_A,
  PROJECT_B,
} from "./boardTestUtils";
import type { BoardProject } from "../api/board";

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

    // Derived progress rides the band header (E1: 1 of 4 done).
    expect(bandE1.textContent).toContain("1/4");

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
