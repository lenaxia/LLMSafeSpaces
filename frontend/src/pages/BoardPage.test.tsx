// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  renderBoard,
  seedBoard,
  mockListProjects,
  defaultTasks,
  TASK_BACKLOG,
  TASK_BLOCKED,
  TASK_IN_PROGRESS,
} from "./boardTestUtils";

// The api mock registry lives in boardTestUtils (single vi.mock site;
// its fns are the per-file singletons every suite in this directory shares).

describe("BoardPage — kanban/swimlanes render (§4.8)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("renders one swimlane per project with the project short id + slot budget", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.getByTestId("swimlane-prj-aaaa1111")).toBeInTheDocument();
    expect(screen.getByTestId("swimlane-prj-bbbb2222")).toBeInTheDocument();
    expect(screen.getByText("Apollo")).toBeInTheDocument();
    expect(screen.getByText("Borealis")).toBeInTheDocument();
    // slots in use: Apollo has 1 in_progress of 2 slots (settles once tasks land)
    await waitFor(() => expect(screen.getByTestId("slots-prj-aaaa1111").textContent).toContain("1/2"));
  });

  it("renders the four through-flow stage columns", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    for (const stage of ["backlog", "in_progress", "in_review", "done"]) {
      expect(screen.getAllByTestId(`stage-drop-${stage}`).length).toBeGreaterThan(0);
    }
  });

  it("places cards in their stage columns (short id mono, title, priority)", async () => {
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-aaaa0001");
    expect(card.closest('[data-stage-drop="backlog"]')).not.toBeNull();
    expect(card.textContent).toContain("tsk-aaaa0001");
    expect(card.textContent).toContain("Write charter");
    expect(card.textContent).toContain("P2");
  });

  it("shows the assignee busy indicator only on claimed in-progress rows", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    const busyCard = screen.getByTestId("task-card-tsk-bbbb0002");
    expect(busyCard.querySelector("[data-busy]")).not.toBeNull();
    const idleCard = screen.getByTestId("task-card-tsk-aaaa0001");
    expect(idleCard.querySelector("[data-busy]")).toBeNull();
  });

  it("carries the structural blocked badge with the dependency count (ruling R3)", async () => {
    renderBoard();
    const badge = await screen.findByTestId("blocked-badge-tsk-eeee0005");
    expect(badge.textContent).toContain("1");
  });

  it("expands the blocked-by view with live dependency stage chips", async () => {
    renderBoard();
    const badge = await screen.findByTestId("blocked-badge-tsk-eeee0005");
    badge.click();
    const view = await screen.findByTestId("blocked-by-tsk-eeee0005");
    expect(view.textContent).toContain("tsk-cccc0003");
    expect(view.querySelector('[data-dep-stage="in_review"]')).not.toBeNull();
  });

  it("renders the OR4 anchor chip on the card", async () => {
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-eeee0005");
    expect(card.querySelector("[data-pr-anchor]")?.textContent).toBe("pr:1702");
  });

  it("collapses Pending Input + Cancelled side columns until toggled", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.queryByTestId("stage-drop-pending_input")).toBeNull();
    screen.getAllByTestId("side-columns-toggle")[0]!.click();
    await waitFor(() => expect(screen.getAllByTestId("stage-drop-pending_input").length).toBeGreaterThan(0));
  });

  it("shows the pending-origin badge on pending rows", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");
    screen.getAllByTestId("side-columns-toggle")[0]!.click();
    const card = await screen.findByTestId("task-card-tsk-ffff0006");
    expect(card.textContent).toContain("Escalated");
  });

  it("renders the empty state when no projects exist", async () => {
    mockListProjects.mockResolvedValue([]);
    renderBoard();
    await waitFor(() => expect(screen.getByTestId("board-empty")).toBeInTheDocument());
  });

  it("renders attempt counters on claimed rows", async () => {
    renderBoard();
    const claimed = await screen.findByTestId("task-card-tsk-bbbb0002");
    expect(claimed.textContent).toContain("⏱ 1");
    const fresh = screen.getByTestId("task-card-tsk-aaaa0001");
    expect(fresh.textContent).not.toContain("⏱");
  });

  it("keeps defaultTasks fixtures coherent (stages across two projects)", () => {
    const tasks = defaultTasks();
    expect(tasks).toContainEqual(expect.objectContaining({ short_id: TASK_BACKLOG.short_id }));
    expect(tasks).toContainEqual(expect.objectContaining({ short_id: TASK_IN_PROGRESS.short_id }));
    expect(tasks).toContainEqual(expect.objectContaining({ short_id: TASK_BLOCKED.short_id }));
  });
});
