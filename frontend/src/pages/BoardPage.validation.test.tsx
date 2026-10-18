// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  renderBoard,
  seedBoard,
  mockCreateProject,
  mockCreateEpic,
  mockCreateTask,
  mockUpdateTask,
  mockAddDependency,
  mockGetEpic,
  boardError,
  PROJECT_A,
  TASK_BLOCKED,
} from "./boardTestUtils";

// OR4 (owner ruling): the PR-drift anchor is the EXPLICIT pr:N tag form
// (^pr:[1-9][0-9]*$) — create/edit inputs validate client-side; the
// engine's DependencyError 409s (code + suggestions, R4a) render verbatim.

function typeIntoTagInput(values: string[]) {
  const input = document.querySelector("#task-tags") as HTMLInputElement | null;
  expect(input).not.toBeNull();
  for (const v of values) {
    fireEvent.change(input!, { target: { value: v } });
    fireEvent.keyDown(input!, { key: "Enter" });
  }
}

describe("BoardPage — OR4 pr:N tag validation (create/edit surfaces)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  async function openCreateTaskDialog() {
    mockGetEpic.mockResolvedValue({
      id: "epic-uuid-1", project_id: PROJECT_A.id, short_id: "epc-eeee1111", title: "E1",
      outcome: "Ship it", status: "active", priority: "P2", tags: [], created_at: "", updated_at: "",
      progress_done: 1, progress_total: 4, progress_derived: true,
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    // Open the edit dialog of an existing task (tags input present).
    fireEvent.click(screen.getByTestId("task-card-tsk-aaaa0001"));
    await screen.findByTestId("task-title");
  }

  it("accepts a well-formed anchor and submits it (tags ride the wire)", async () => {
    mockUpdateTask.mockResolvedValue({ ...TASK_BLOCKED });
    await openCreateTaskDialog();
    typeIntoTagInput(["pr:1728"]);
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() =>
      expect(mockUpdateTask).toHaveBeenCalledWith(
        "tsk-aaaa0001",
        expect.objectContaining({ tags: ["pr:1728"] }),
      ),
    );
  });

  it("rejects pr:0 with the OR4 reason and blocks submit", async () => {
    await openCreateTaskDialog();
    typeIntoTagInput(["pr:0"]);
    const errors = await screen.findByTestId("tag-validation-errors");
    expect(errors.textContent).toContain("pr:0");
    expect(errors.textContent).toContain("OR4");
    const save = screen.getByTestId("task-save") as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    expect(mockUpdateTask).not.toHaveBeenCalled();
  });

  it("rejects leading zeros (pr:01)", async () => {
    await openCreateTaskDialog();
    typeIntoTagInput(["pr:01"]);
    expect(await screen.findByTestId("tag-validation-errors")).toBeTruthy();
  });

  it("rejects case near-misses (PR:12)", async () => {
    await openCreateTaskDialog();
    typeIntoTagInput(["PR:12"]);
    const errors = await screen.findByTestId("tag-validation-errors");
    expect(errors.textContent).toContain("lowercase");
  });

  it("rejects non-numeric anchors (pr:seven)", async () => {
    await openCreateTaskDialog();
    typeIntoTagInput(["pr:seven"]);
    expect(await screen.findByTestId("tag-validation-errors")).toBeTruthy();
  });

  it("keeps free-form tags untouched", async () => {
    mockUpdateTask.mockResolvedValue({});
    await openCreateTaskDialog();
    typeIntoTagInput(["urgent"]);
    await waitFor(() => expect(screen.queryByTestId("tag-validation-errors")).toBeNull());
  });

  it("project creation validates the same OR4 form", async () => {
    mockCreateProject.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    fireEvent.click(screen.getByTestId("new-project-button"));
    fireEvent.change(screen.getByTestId("project-name"), { target: { value: "New" } });
    fireEvent.change(screen.getByTestId("project-workspace"), { target: { value: "ws-1" } });
    const tagInput = document.querySelector("#project-tags") as HTMLInputElement;
    fireEvent.change(tagInput, { target: { value: "pr:0" } });
    fireEvent.keyDown(tagInput, { key: "Enter" });
    const errors = await screen.findByTestId("project-tag-errors");
    expect(errors.textContent).toContain("pr:0");
    expect((screen.getByTestId("project-create") as HTMLButtonElement).disabled).toBe(true);
    expect(mockCreateProject).not.toHaveBeenCalled();
  });

  it("project creation submits name/workspace/slots (R7/R8 surface)", async () => {
    mockCreateProject.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    fireEvent.click(screen.getByTestId("new-project-button"));
    fireEvent.change(screen.getByTestId("project-name"), { target: { value: "New" } });
    fireEvent.change(screen.getByTestId("project-workspace"), { target: { value: "ws-1" } });
    fireEvent.change(screen.getByTestId("project-slots"), { target: { value: "3" } });
    fireEvent.click(screen.getByTestId("project-create"));
    await waitFor(() =>
      expect(mockCreateProject).toHaveBeenCalledWith(
        expect.objectContaining({ name: "New", targetWorkspaceId: "ws-1", workerSlots: 3 }),
      ),
    );
  });

  it("renders the dependency 409 with guard code + did-you-mean suggestions (R4a)", async () => {
    await openCreateTaskDialog();
    mockAddDependency.mockRejectedValue(
      boardError(409, {
        error: "board: dependency cycle: tsk-aaaa0001 → tsk-cccc0003 → tsk-aaaa0001 (did you mean: [tsk-cccc0003]?)",
        code: "cycle",
        suggestions: ["tsk-cccc0003"],
      }),
    );
    fireEvent.change(screen.getByTestId("dep-input"), { target: { value: "tsk-cccc0003" } });
    fireEvent.click(screen.getByTestId("add-dependency"));
    const alert = await screen.findByRole("alert");
    expect(alert.getAttribute("data-board-error")).toBe("cycle");
    const suggestions = screen.getByTestId("board-error-suggestions");
    expect(suggestions.textContent).toContain("tsk-cccc0003");
    // Clicking the suggestion chip fills the input.
    const chip = screen.getByTestId("board-error-suggestions").querySelector("button");
    expect(chip?.textContent).toBe("tsk-cccc0003");
    fireEvent.click(chip!);
    await waitFor(() => expect((screen.getByTestId("dep-input") as HTMLInputElement).value).toBe("tsk-cccc0003"));
  });

  it("renders 404 suggestions on dependency add misses", async () => {
    await openCreateTaskDialog();
    mockAddDependency.mockRejectedValue(
      boardError(404, { error: 'board: task "tsk-nope9999" not found (did you mean: [tsk-cccc0003]?)', suggestions: ["tsk-cccc0003"] }),
    );
    fireEvent.change(screen.getByTestId("dep-input"), { target: { value: "tsk-nope9999" } });
    fireEvent.click(screen.getByTestId("add-dependency"));
    await screen.findByTestId("board-error-suggestions");
  });

  it("epic creation threads title/outcome/priority (the steering wheel)", async () => {
    mockCreateEpic.mockResolvedValue({
      id: "epic-new", project_id: PROJECT_A.id, short_id: "epc-new11111", title: "Epic Next",
      outcome: "Ship board UI", status: "draft", priority: "P1", tags: [], created_at: "", updated_at: "",
      progress_done: 0, progress_total: 0, progress_derived: false,
    });
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    fireEvent.change(screen.getByTestId("project-focus"), { target: { value: "proj-uuid-a" } });
    fireEvent.click(screen.getByTestId("new-epic-button"));
    fireEvent.change(screen.getByTestId("epic-title"), { target: { value: "Epic Next" } });
    fireEvent.change(screen.getByTestId("epic-outcome"), { target: { value: "Ship board UI" } });
    fireEvent.click(screen.getByTestId("epic-create"));
    await waitFor(() => expect(mockCreateEpic).toHaveBeenCalledWith("proj-uuid-a", expect.objectContaining({ title: "Epic Next", outcome: "Ship board UI" })));
  });

  it("task creation rides the epic route (create mode)", async () => {
    mockGetEpic.mockResolvedValue({
      id: "epic-uuid-2", project_id: PROJECT_A.id, short_id: "epc-ffff2222", title: "E2",
      outcome: "", status: "active", priority: "P2", tags: [], created_at: "", updated_at: "",
      progress_done: 0, progress_total: 1, progress_derived: true,
    });
    mockCreateTask.mockResolvedValue({});
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");
    // dblclick the E2 chip opens the drawer; use the drawer's New task.
    fireEvent.dblClick(screen.getByText("E2"));
    const drawer = await screen.findByTestId("epic-outcome");
    expect(drawer).toBeInTheDocument();
    fireEvent.click(screen.getByText("New task"));
    const title = await screen.findByTestId("task-title");
    fireEvent.change(title, { target: { value: "Fresh task" } });
    fireEvent.change(screen.getByTestId("new-criterion"), { target: { value: "criterion one" } });
    fireEvent.click(screen.getByLabelText("Add criterion"));
    fireEvent.click(screen.getByTestId("task-save"));
    await waitFor(() =>
      expect(mockCreateTask).toHaveBeenCalledWith(
        "epic-uuid-2",
        expect.objectContaining({ title: "Fresh task", acceptanceCriteria: [{ text: "criterion one", met: false }] }),
      ),
    );
  });

  it("requires a title before save", async () => {
    await openCreateTaskDialog();
    fireEvent.change(screen.getByTestId("task-title"), { target: { value: "" } });
    expect((screen.getByTestId("task-save") as HTMLButtonElement).disabled).toBe(true);
  });
});
