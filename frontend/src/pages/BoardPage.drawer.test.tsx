// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// BoardPage.drawer.test.tsx — #1832 item 4 red-first pins for the
// drawer's open affordance: the task card's details control opens the
// task-detail drawer (and ONLY the drawer — the card's primary click
// keeps its existing edit-dialog behavior; PR-C scope untouched), the
// drawer fetches that task's audit timeline, and closing it returns to
// the board. RED until the wiring exists.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import {
  mockTaskEvents,
  renderBoard,
  seedBoard,
  TASK_IN_PROGRESS,
  boardError,
} from "./boardTestUtils";

function timelineFor() {
  return [
    {
      id: "e1", project_id: "proj-uuid-a", item_kind: "task", item_id: "task-uuid-2",
      action: "create_task", actor: { kind: "user", surface: "rest" },
      payload: { task_id: "task-uuid-2" }, created_at: "2026-10-22T09:00:00Z",
    },
    {
      id: "e2", project_id: "proj-uuid-a", item_kind: "task", item_id: "task-uuid-2",
      action: "claim", actor: { kind: "system", surface: "engine" },
      payload: { task_id: "task-uuid-2", worker: "sess-1", attempts: 1 }, created_at: "2026-10-22T10:00:00Z",
    },
  ];
}

describe("BoardPage task-detail drawer (#1832 item 4)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
    mockTaskEvents.mockImplementation(async () => timelineFor());
  });

  it("opens the drawer from the card's details affordance and loads its timeline", async () => {
    renderBoard();
    fireEvent.click(await screen.findByTestId(`details-${TASK_IN_PROGRESS.short_id}`));

    // The drawer is up with the header + timeline content.
    const title = await screen.findByTestId("drawer-title");
    expect(title.textContent).toContain(TASK_IN_PROGRESS.title);
    expect(mockTaskEvents).toHaveBeenCalledWith(TASK_IN_PROGRESS.short_id, expect.anything());
    await screen.findByTestId("timeline-evt-e2");
  });

  it("keeps the card's primary click on the edit dialog (PR-C scope untouched)", async () => {
    renderBoard();
    fireEvent.click(await screen.findByTestId(`task-card-${TASK_IN_PROGRESS.short_id}`));
    // The edit dialog opens (its title field) — NOT the drawer.
    await screen.findByTestId("task-title");
    expect(screen.queryByTestId("drawer-title")).toBeNull();
  });

  it("closes the drawer without disturbing the board", async () => {
    renderBoard();
    fireEvent.click(await screen.findByTestId(`details-${TASK_IN_PROGRESS.short_id}`));
    await screen.findByTestId("drawer-title");
    fireEvent.click(screen.getByTestId("drawer-close"));
    await waitFor(() => expect(screen.queryByTestId("drawer-title")).toBeNull());
    expect(await screen.findByTestId(`task-card-${TASK_IN_PROGRESS.short_id}`)).toBeTruthy();
  });

  it("drawer timeline error renders in place (board stays usable)", async () => {
    mockTaskEvents.mockRejectedValue(boardError(500, { error: "internal", message: "timeline fetch failed" }));
    renderBoard();
    fireEvent.click(await screen.findByTestId(`details-${TASK_IN_PROGRESS.short_id}`));
    expect(await screen.findByTestId("timeline-error")).toBeTruthy();
    expect(await screen.findByTestId(`task-card-${TASK_IN_PROGRESS.short_id}`)).toBeTruthy();
  });
});
