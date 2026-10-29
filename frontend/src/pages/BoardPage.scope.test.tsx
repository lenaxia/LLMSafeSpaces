// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  renderBoard,
  seedBoard,
  mockListProjectTasks,
} from "./boardTestUtils";

// §4.8 scope switcher: swimlane focus (project), tag/failed filters,
// search with did-you-mean (0066 shape over loaded rows).
// 2026-10-28 ruling 1: the epic pills are GONE from the toolbar — the
// real filters (stage overlay, tag facets) live in the collapsible
// filter drawer (owner ruling 2026-10-29).

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
    // #1964 r1: the tasks read is always the FULL fan-out (drawer
    // lanes need every lane's rows regardless of focus) — focus is a
    // view filter over the cached board, so every lane's REST call
    // (the focused one included) has fired.
    expect(mockListProjectTasks).toHaveBeenCalledWith("proj-uuid-b", undefined);
    expect(mockListProjectTasks).toHaveBeenCalledWith("proj-uuid-a", undefined);
  });

  // 2026-10-28 ruling 1: the epic pills are removed from the toolbar —
  // and the owner ruling 2026-10-29 folds the REAL filters (FAILED
  // overlay, tag facets) into the collapsible filter drawer: with the
  // drawer closed NOTHING renders inline, and no epic renders as a
  // pill anywhere in the header.
  it("renders NO pills inline; the real filters live in the filter drawer", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    expect(screen.queryByTestId("epic-chip-all")).toBeNull();
    expect(screen.queryByText("E1", { selector: "button" })).toBeNull();
    expect(screen.queryByText("E2", { selector: "button" })).toBeNull();
    // Drawer closed by default: no filter pills inline, no drawer in
    // the DOM (zero height — the board starts right below the toolbar).
    expect(screen.queryByTestId("board-filter-drawer")).toBeNull();
    expect(screen.queryByTestId("failed-filter-toggle")).toBeNull();
    expect(screen.queryByTestId("tag-filter-pr:1702")).toBeNull();
    // The funnel button is the toolbar's only filter affordance, and
    // with nothing applied it carries NO count badge.
    expect(screen.getByTestId("board-filter-button")).toBeInTheDocument();
    expect(screen.queryByTestId("board-filter-count")).toBeNull();

    // Opening the drawer reveals the real filters.
    fireEvent.click(screen.getByTestId("board-filter-button"));
    expect(screen.getByTestId("board-filter-drawer")).toBeInTheDocument();
    expect(screen.getByTestId("failed-filter-toggle")).toBeInTheDocument();
    expect(screen.getByTestId("tag-filter-pr:1702")).toBeInTheDocument();
  });

  it("tag chips filter by tag", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");
    fireEvent.click(screen.getByTestId("board-filter-button"));
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

  it("renders did-you-mean suggestions on a near-miss id search (title labels, 0066 shape)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "tsk-aaaa0009" } });
    const miss = await screen.findByTestId("board-search-miss");
    expect(miss.textContent).toContain("No match");
    // 2026-10-28 ruling 4: the suggestion is labeled by TITLE (one edit
    // from tsk-aaaa0001 → suggested); the id never renders in the chrome.
    expect(miss.textContent).toContain("Write charter");
    expect(miss.textContent).not.toContain("tsk-aaaa0001");

    // clicking the suggestion applies it (searches by the title)
    fireEvent.click(screen.getByText("Write charter"));
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
