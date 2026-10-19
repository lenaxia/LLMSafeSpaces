// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  renderBoard,
  seedBoard,
  mockListProjectTasks,
} from "./boardTestUtils";

// §4.8 scope switcher: swimlane focus (project), epic chips, tag filters,
// search with did-you-mean (0066 shape over loaded rows).

describe("BoardPage — scope switcher (project/epic focus)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("focuses one project's swimlane", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.getByTestId("swimlane-prj-bbbb2222")).toBeInTheDocument();

    fireEvent.change(screen.getByTestId("project-focus"), { target: { value: "proj-uuid-b" } });
    await waitFor(() => expect(screen.queryByTestId("swimlane-prj-aaaa1111")).toBeNull());
    expect(screen.getByTestId("swimlane-prj-bbbb2222")).toBeInTheDocument();
    // focused fetch is the single-project REST call
    expect(mockListProjectTasks).toHaveBeenCalledWith("proj-uuid-b", undefined);
  });

  it("epic chips filter within the focused scope and reset with All epics", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    expect(screen.getByTestId("task-card-tsk-eeee0005")).toBeInTheDocument();

    // Both epic chips are visible in the All-projects scope.
    const chipE1 = screen.getByText("E1");
    fireEvent.click(chipE1);
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-eeee0005")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("epic-chip-all"));
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-eeee0005")).toBeInTheDocument());
  });

  it("derives epic chips from task rows (display inheritance, R2)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    expect(screen.getByText("E1")).toBeInTheDocument();
    expect(screen.getByText("E2")).toBeInTheDocument();
  });

  it("tag chips filter by tag", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");
    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-eeee0005")).toBeInTheDocument();
  });

  it("search filters on short id, title, and tags", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "tsk-bbbb0002" } });
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-bbbb0002")).toBeInTheDocument();

    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "Review drag arm" } });
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-cccc0003")).toBeInTheDocument());
    expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull();

    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "pr:1702" } });
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-eeee0005")).toBeInTheDocument());
    expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull();
  });

  it("renders did-you-mean suggestions on a near-miss short id search (0066 shape)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "tsk-aaaa0009" } });
    const miss = await screen.findByTestId("board-search-miss");
    expect(miss.textContent).toContain("No match");
    // one edit from tsk-aaaa0001 → suggested
    expect(miss.textContent).toContain("tsk-aaaa0001");

    // clicking the suggestion applies it
    fireEvent.click(screen.getByText("tsk-aaaa0001"));
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeInTheDocument());
    expect(screen.queryByTestId("board-search-miss")).toBeNull();
  });

  it("shows no miss panel on far misses with no suggestions", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "zzzz-nothing-here" } });
    const miss = await screen.findByTestId("board-search-miss");
    expect(miss.textContent).not.toContain("Did you mean");
  });
});
