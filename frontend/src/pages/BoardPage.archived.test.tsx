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
  mockArchiveProject,
  mockUnarchiveProject,
  PROJECT_A,
  PROJECT_B,
  PROJECT_ARCHIVED,
  TASK_DONE,
  TASK_BACKLOG,
} from "./boardTestUtils";
import type { BoardProject, BoardTask } from "../api/board";

// #1964: archived projects are hidden from the board's active surface
// and live in a collapsed Archived drawer at the board's end — the
// drawer itself collapsed by default, each project inside a collapsed
// name-only drawer (progressive disclosure; the #1627 deliberate-
// friction posture). History stays reachable inside the drawer; the
// unarchive flow keeps working from there.

/** A history row belonging to the ARCHIVED lane (proj-uuid-arc). */
const ARCHIVED_TASK: BoardTask = {
  ...TASK_DONE,
  id: "task-uuid-arc1",
  project_id: "proj-uuid-arc",
  epic_id: "epic-uuid-1",
  short_id: "tsk-arc00001",
  title: "Ancient history",
};

const ARCHIVED_SEED: BoardProject[] = [PROJECT_A, PROJECT_ARCHIVED];

function seedArchivedBoard() {
  return seedBoard([...defaultTasks(), ARCHIVED_TASK], ARCHIVED_SEED);
}

/** Opens both drawer levels: the Archived section, then the project's
 * own drawer (name-only until that second click). */
async function expandArchivedLane(shortId: string) {
  fireEvent.click(await screen.findByTestId("archived-projects-toggle"));
  await screen.findByTestId(`archived-project-${shortId}`);
  fireEvent.click(screen.getByTestId(`archived-project-toggle-${shortId}`));
  return screen.findByTestId(`swimlane-${shortId}`);
}

describe("BoardPage #1964 — archived projects drawer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedArchivedBoard();
  });

  it("an archived project renders NO swimlane on the active surface (drawer carries it instead)", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    expect(screen.queryByTestId("swimlane-prj-arch3333")).toBeNull();
    const drawer = await screen.findByTestId("archived-projects-drawer");
    expect(within(drawer).getByTestId("archived-projects-count").textContent).toBe("1");
  });

  it("no drawer renders when nothing is archived", async () => {
    seedBoard(defaultTasks(), [PROJECT_A, PROJECT_B]);
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    expect(screen.queryByTestId("archived-projects-drawer")).toBeNull();
  });

  it("archived projects are absent from the project-focus dropdown", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    const select = screen.getByTestId("project-focus") as HTMLSelectElement;
    const values = Array.from(select.options).map((o) => o.value);
    expect(values).toEqual(["all", "proj-uuid-a"]);
  });

  it("both drawer levels stay collapsed by default — the project name appears only after the first click", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    const toggle = screen.getByTestId("archived-projects-toggle");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    // Collapsed outer drawer: not even the project NAME is on screen.
    expect(screen.queryByText("Archived Lane")).toBeNull();

    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");

    // Name-only row: the project drawer shows the name and nothing of
    // its lane (no slots, no swimlane) until its own click.
    const row = screen.getByTestId("archived-project-prj-arch3333");
    expect(within(row).getByText("Archived Lane")).toBeInTheDocument();
    const rowToggle = within(row).getByTestId("archived-project-toggle-prj-arch3333");
    expect(rowToggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByTestId("swimlane-prj-arch3333")).toBeNull();
    expect(screen.queryByTestId("slots-prj-arch3333")).toBeNull();
  });

  it("expanding a project drawer reveals its history (the full swimlane renders inside)", async () => {
    renderBoard();
    await expandArchivedLane("prj-arch3333");

    // History reachable: the archived lane's done task renders inside
    // (the epic band row AND the Done stage column both carry it).
    const lane = screen.getByTestId("swimlane-prj-arch3333");
    expect(within(lane).getAllByText("Ancient history").length).toBeGreaterThan(0);
  });

  it("unarchive from inside the drawer returns the lane to the active surface", async () => {
    mockUnarchiveProject.mockResolvedValue({ ...PROJECT_ARCHIVED, status: "active" });
    // Stateful read: the initial load carries the archived seed; every
    // post-invalidation refetch lands the lane as active (the write's
    // authoritative echo) — race-free regardless of refetch timing.
    let listCalls = 0;
    mockListProjects.mockImplementation(async () => {
      listCalls += 1;
      return listCalls <= 1 ? ARCHIVED_SEED : [PROJECT_A, { ...PROJECT_ARCHIVED, status: "active" }];
    });
    renderBoard();
    await expandArchivedLane("prj-arch3333");

    fireEvent.click(screen.getByTestId("project-kebab-prj-arch3333"));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Unarchive project" }));

    await waitFor(() => expect(mockUnarchiveProject).toHaveBeenCalledWith("proj-uuid-arc"));
    // The refetch lands it as active: swimlane on the surface, drawer gone.
    await screen.findByTestId("swimlane-prj-arch3333");
    await waitFor(() => expect(screen.queryByTestId("archived-projects-drawer")).toBeNull());
    expect(mockArchiveProject).not.toHaveBeenCalled();
  });

  it("archiving the focused lane resets focus and files the project into the drawer", async () => {
    mockArchiveProject.mockResolvedValue({ ...PROJECT_B, status: "archived" });
    let listCalls = 0;
    mockListProjects.mockImplementation(async () => {
      listCalls += 1;
      return listCalls <= 1
        ? [PROJECT_A, PROJECT_B]
        : [PROJECT_A, { ...PROJECT_B, status: "archived" }];
    });
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    fireEvent.change(screen.getByTestId("project-focus"), { target: { value: "proj-uuid-b" } });
    await screen.findByTestId("swimlane-prj-bbbb2222");

    fireEvent.click(screen.getByTestId("project-kebab-prj-bbbb2222"));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Archive project" }));
    await waitFor(() => expect(mockArchiveProject).toHaveBeenCalledWith("proj-uuid-b"));

    await screen.findByTestId("archived-projects-drawer");
    await waitFor(() => expect(screen.queryByTestId("swimlane-prj-bbbb2222")).toBeNull());
    // Focus reset to All — the scope never points at a hidden lane.
    expect((screen.getByTestId("project-focus") as HTMLSelectElement).value).toBe("all");
    expect(mockUnarchiveProject).not.toHaveBeenCalled();
  });

  it("the active surface derivations ignore archived lanes (epic/tag chips, search)", async () => {
    // An archived-lane task whose tag and epic would otherwise surface.
    const tagged: BoardTask = {
      ...TASK_BACKLOG,
      id: "task-uuid-arc2",
      project_id: "proj-uuid-arc",
      short_id: "tsk-arc00002",
      title: "Zephyr relic",
      tags: ["pr:1964"],
    };
    seedBoard([...defaultTasks(), tagged], ARCHIVED_SEED);
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");

    expect(screen.queryByTestId("tag-filter-pr:1964")).toBeNull();
    // The archived lane's epic never graduates to a chip (E2 exists on
    // the active lane via TASK_BLOCKED; the drawer lane shares E1 only).
    // Search: a term matching ONLY the archived row is an honest miss.
    fireEvent.change(screen.getByTestId("board-search"), { target: { value: "Zephyr relic" } });
    await screen.findByTestId("board-search-miss");
    expect(screen.queryByTestId("band-task-epic-uuid-1-tsk-arc00002")).toBeNull();
  });
});
