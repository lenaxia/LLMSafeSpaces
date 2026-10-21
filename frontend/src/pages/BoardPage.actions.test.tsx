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
  mockListProjects,
  mockWorkspacesList,
  mockStartTask,
  mockCancelTask,
  mockActivateEpic,
  mockPauseEpic,
  mockGetEpic,
  mockUpdateTask,
  mockAddDependency,
  boardError,
  makeDataTransfer,
  TASK_IN_PROGRESS,
  PROJECT_A,
} from "./boardTestUtils";
import { emitBoardEvent } from "../lib/boardFeed";
import { act } from "@testing-library/react";

function boardEvent(action: string, itemId: string, extra: Record<string, unknown> = {}) {
  return {
    type: "board.event" as const,
    project_id: "proj-uuid-a",
    id: `evt-${action}-${itemId}-${Math.random().toString(36).slice(2, 6)}`,
    item_kind: "task",
    item_id: itemId,
    action,
    payload: extra,
  };
}

function dragCardTo(shortId: string, stage: string) {
  const dt = makeDataTransfer();
  fireEvent.dragStart(screen.getByTestId(`task-card-${shortId}`), { dataTransfer: dt });
  fireEvent.drop(screen.getAllByTestId(`stage-drop-${stage}`)[0]!, { dataTransfer: dt });
}

const EPIC_ROW = {
  id: "epic-uuid-1", project_id: PROJECT_A.id, short_id: "epc-eeee1111", title: "E1",
  outcome: "Ship it", status: "active", priority: "P2", tags: [], created_at: "", updated_at: "",
  progress_done: 1, progress_total: 4, progress_derived: true,
};

describe("BoardPage r1 — cancel / lifecycle / start / feed / concurrency", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  // --- Cancel flow (design §4.8's cancel arm; previously untested) ---------

  it("cancels a task from the dialog: terminal write + invalidation + card lands Cancelled", async () => {
    const seeded = seedBoard();
    mockCancelTask.mockImplementation(async () => {
      const updated = { ...TASK_IN_PROGRESS, stage: "cancelled" };
      seedBoard(seeded.map((t) => (t.short_id === updated.short_id ? updated : t)));
      return updated;
    });
    renderBoard();
    fireEvent.click(await screen.findByTestId("task-card-tsk-bbbb0002"));
    fireEvent.click(await screen.findByTestId("cancel-task"));

    await waitFor(() => expect(mockCancelTask).toHaveBeenCalledWith("tsk-bbbb0002", undefined));
    // dialog closed on success
    await waitFor(() => expect(screen.queryByTestId("cancel-task")).toBeNull());
    // card visible in the Cancelled side column
    fireEvent.click(screen.getAllByTestId("side-columns-toggle")[0]!);
    await waitFor(() =>
      expect(screen.getByTestId("task-card-tsk-bbbb0002").closest('[data-stage-drop="cancelled"]')).not.toBeNull(),
    );
  });

  it("cancel failure renders the guard code and the dialog STAYS open", async () => {
    mockCancelTask.mockRejectedValue(
      boardError(409, { error: "board: guard stage_transition: already terminal", code: "stage_transition" }),
    );
    renderBoard();
    fireEvent.click(await screen.findByTestId("task-card-tsk-bbbb0002"));
    fireEvent.click(await screen.findByTestId("cancel-task"));

    await waitFor(() => expect(mockCancelTask).toHaveBeenCalled());
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("stage_transition");
    expect(screen.getByTestId("cancel-task")).toBeInTheDocument();
  });

  // --- Epic lifecycle (activate/pause; the r1 onError path) ------------------

  it("activates a paused epic from the drawer: status flips + board invalidates", async () => {
    mockGetEpic.mockResolvedValue({ ...EPIC_ROW, status: "paused" });
    mockActivateEpic.mockResolvedValue({ ...EPIC_ROW, status: "active" });
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.dblClick(screen.getByText("E1"));

    await screen.findByTestId("epic-outcome");
    expect(screen.getByTestId("epic-status").textContent).toBe("paused");
    fireEvent.click(screen.getByTestId("epic-activate"));

    await waitFor(() => expect(mockActivateEpic).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByTestId("epic-status").textContent).toBe("active"));
  });

  it("pauses an active epic from the drawer", async () => {
    mockGetEpic.mockResolvedValue({ ...EPIC_ROW, status: "active" });
    mockPauseEpic.mockResolvedValue({ ...EPIC_ROW, status: "paused" });
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.dblClick(screen.getByText("E1"));
    await screen.findByTestId("epic-outcome");
    fireEvent.click(screen.getByTestId("epic-pause"));
    await waitFor(() => expect(mockPauseEpic).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByTestId("epic-status").textContent).toBe("paused"));
  });

  it("renders a FAILED activate inside the drawer (r1: the swallow)", async () => {
    mockGetEpic.mockResolvedValue({ ...EPIC_ROW, status: "paused" });
    mockActivateEpic.mockRejectedValue(
      boardError(409, { error: "board: guard epic_state: paused→active requires…", code: "epic_state" }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.dblClick(screen.getByText("E1"));
    await screen.findByTestId("epic-outcome");
    fireEvent.click(screen.getByTestId("epic-activate"));

    await waitFor(() => expect(mockActivateEpic).toHaveBeenCalled());
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("epic_state");
    expect(screen.getByTestId("epic-status").textContent).toBe("paused");
  });

  // --- task_start (the backlog exit the engine enumerates) -------------------

  it("starts a backlog task (manual dispatch) and renders the dispatched notice", async () => {
    mockStartTask.mockResolvedValue({
      task_id: "task-uuid-1", short_id: "tsk-aaaa0001", project_id: PROJECT_A.id, epic_id: "epic-uuid-1",
      workspace_id: "ws-1", claim_token: "tok", lease_expires_at: "2026-10-18T23:00:00Z", escalated: false,
    });
    renderBoard();
    fireEvent.click(await screen.findByTestId("start-tsk-aaaa0001"));

    await waitFor(() => expect(mockStartTask).toHaveBeenCalledWith("tsk-aaaa0001"));
    const notice = await screen.findByTestId("board-notice");
    expect(notice.textContent).toContain("fresh worker dispatched");
  });

  it("renders the escalation outcome honestly when the attempts cap withholds the grant", async () => {
    mockStartTask.mockResolvedValue({
      task_id: "task-uuid-6", short_id: "tsk-ffff0006", project_id: "proj-uuid-b", epic_id: "epic-uuid-1",
      workspace_id: "ws-1", escalated: true,
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    // Borealis' backlog-adjacent pending row is not startable; use Apollo's backlog row after attempts bump
    fireEvent.click(screen.getByTestId("start-tsk-aaaa0001"));
    await waitFor(() => expect(mockStartTask).toHaveBeenCalled());
    const notice = await screen.findByTestId("board-notice");
    expect(notice.textContent).toContain("escalated");
  });

  it("task_start failure renders at page level with the guard code", async () => {
    mockStartTask.mockRejectedValue(
      boardError(409, { error: "board: guard epic_state: paused epics refuse task_start", code: "epic_state" }),
    );
    renderBoard();
    fireEvent.click(await screen.findByTestId("start-tsk-aaaa0001"));
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("epic_state");
  });

  // --- Attempt escalation preview (§4.8 attempts/N) ---------------------------

  it("shows the escalation preview on a capped backlog row (attempts ≥ default N)", async () => {
    seedBoard(defaultTasks().map((t) => (t.short_id === "tsk-aaaa0001" ? { ...t, attempts: 3 } : t)));
    renderBoard();
    const preview = await screen.findByTestId("escalation-preview-tsk-aaaa0001");
    expect(preview.textContent).toContain("escalates next claim");
  });

  it("hides the preview below the cap and on pending rows (the engine already escalated)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    expect(screen.queryByTestId("escalation-preview-tsk-aaaa0001")).toBeNull();
    // tsk-ffff0006: attempts 3 but pending_input — origin badge, no preview
    fireEvent.click(screen.getAllByTestId("side-columns-toggle")[0]!);
    await screen.findByTestId("task-card-tsk-ffff0006");
    expect(screen.queryByTestId("escalation-preview-tsk-ffff0006")).toBeNull();
  });

  // --- submitTask failure (dialog stays open; no unhandled rejection) --------

  it("keeps the edit dialog open with the notice inside on a failed save", async () => {
    mockUpdateTask.mockRejectedValue(
      boardError(409, { error: "board: guard claim_token: stage writes on a claimed task require its live claim token", code: "claim_token" }),
    );
    renderBoard();
    fireEvent.click(await screen.findByTestId("task-card-tsk-bbbb0002"));
    fireEvent.change(await screen.findByTestId("task-title"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByTestId("task-save"));

    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("claim_token");
    expect(screen.getByTestId("task-title")).toBeInTheDocument();
    expect((screen.getByTestId("task-title") as HTMLInputElement).value).toBe("Renamed");
  });

  // --- Dependency input preservation (r1) --------------------------------------

  it("preserves the typed dependency id when the add fails", async () => {
    mockAddDependency.mockRejectedValue(
      boardError(409, { error: "board: dependency cycle: …", code: "cycle", suggestions: ["tsk-cccc0003"] }),
    );
    renderBoard();
    fireEvent.click(await screen.findByTestId("task-card-tsk-aaaa0001"));
    const dep = await screen.findByTestId("dep-input");
    fireEvent.change(dep, { target: { value: "tsk-cccc0003" } });
    fireEvent.click(screen.getByTestId("add-dependency"));

    await waitFor(() => expect(mockAddDependency).toHaveBeenCalled());
    await waitFor(() => expect((screen.getByTestId("dep-input") as HTMLInputElement).value).toBe("tsk-cccc0003"));
  });

  // --- Concurrent drags (r1: per-slot optimistic state) ------------------------

  it("keeps two in-flight optimistic moves independent", async () => {
    const resolvers: Array<(t: unknown) => void> = [];
    const { mockMoveStage } = vi.hoisted(() => ({ mockMoveStage: vi.fn() }));
    void mockMoveStage;
    // use the shared mock with deferred promises
    const { mockMoveStage: shared } = await import("./boardTestUtils");
    shared.mockImplementation(
      () => new Promise((res) => { resolvers.push(res); }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");
    await screen.findByTestId("task-card-tsk-dddd0004");

    dragCardTo("tsk-cccc0003", "in_progress");
    dragCardTo("tsk-dddd0004", "in_review");

    // BOTH optimistic placements live simultaneously.
    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-cccc0003").closest('[data-stage-drop="in_progress"]')).not.toBeNull();
      expect(screen.getByTestId("task-card-tsk-dddd0004").closest('[data-stage-drop="in_review"]')).not.toBeNull();
    });

    // Resolve only the first: the second's optimistic placement survives.
    await act(async () => { resolvers[0]!({}); });
    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-dddd0004").closest('[data-stage-drop="in_review"]')).not.toBeNull();
    });
  });

  // --- Epic drawer activity feed (§4.8) -------------------------------------------

  it("feeds the drawer board events for the epic's tasks only", async () => {
    mockGetEpic.mockResolvedValue(EPIC_ROW);
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.dblClick(screen.getByText("E1"));
    await screen.findByTestId("epic-activity");

    await act(async () => {
      emitBoardEvent(boardEvent("move_stage", "task-uuid-1", { short_id: "tsk-aaaa0001" })); // E1's task
      emitBoardEvent(boardEvent("move_stage", "task-uuid-5", { short_id: "tsk-eeee0005" })); // E2's task
      emitBoardEvent({ ...boardEvent("epic_pause", "epic-uuid-2"), item_kind: "epic" }); // not a task
    });

    const items = screen.getAllByTestId("epic-activity-item");
    expect(items.length).toBe(1);
    expect(items[0]!.textContent).toContain("tsk-aaaa0001");
  });

  // --- Read-path failures are not emptiness (r2) ------------------------------------

  it("renders a load-error notice (not the empty state) when listProjects rejects", async () => {
    mockListProjects.mockRejectedValue(new Error("network down"));
    renderBoard();
    const err = await screen.findByTestId("board-read-error");
    expect(err.querySelector('[data-board-error]')?.textContent).toContain("network down");
    expect(screen.queryByTestId("board-empty")).toBeNull();
  });

  it("renders a load-error notice when the tasks fetch rejects (projects fine)", async () => {
    const { mockListProjectTasks } = await import("./boardTestUtils");
    seedBoard();
    mockListProjectTasks.mockRejectedValue(new Error("503 from tasks"));
    renderBoard();
    const err = await screen.findByTestId("board-read-error");
    expect(err.querySelector('[data-board-error]')?.textContent).toContain("503 from tasks");
    expect(screen.queryByTestId("board-empty")).toBeNull();
  });

  it("recovers via Retry (invalidates and re-renders rows)", async () => {
    mockListProjects.mockRejectedValueOnce(new Error("network blip"));
    seedBoard();
    renderBoard();
    await screen.findByTestId("board-read-error");
    fireEvent.click(screen.getByTestId("board-retry"));
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.queryByTestId("board-read-error")).toBeNull();
  });

  it("surfaces a workspaces-list failure inside the New-project dialog (r3: not a silent empty select)", async () => {
    mockWorkspacesList.mockRejectedValue(new Error("503 workspaces"));
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    fireEvent.click(screen.getByTestId("new-project-button"));

    const err = await screen.findByTestId("workspaces-load-error");
    expect(err.querySelector('[data-board-error]')?.textContent).toContain("503 workspaces");
    // The select is present but the failure is rendered beside it — not silent.
    expect(screen.getByTestId("project-workspace")).toBeInTheDocument();
  });

  it("retries the workspaces fetch from the dialog", async () => {
    mockWorkspacesList.mockRejectedValueOnce(new Error("503 workspaces"));
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    fireEvent.click(screen.getByTestId("new-project-button"));
    await screen.findByTestId("workspaces-load-error");

    fireEvent.click(screen.getByTestId("workspaces-retry"));
    await waitFor(() => expect(screen.queryByTestId("workspaces-load-error")).toBeNull());
    // The select now carries the loaded workspace.
    const select = screen.getByTestId("project-workspace") as HTMLSelectElement;
    await waitFor(() => expect(select.querySelector('option[value="ws-1"]')).not.toBeNull());
  });

  it("keeps the empty state for a SUCCESSFUL read of zero projects", async () => {
    mockListProjects.mockResolvedValue([]);
    renderBoard();
    await screen.findByTestId("board-empty");
    expect(screen.queryByTestId("board-read-error")).toBeNull();
  });

  // --- openEpicDrawer failure at page level (r1) -----------------------------------

  it("renders a getEpic 404 at page level (with suggestions) when the drawer fails to open", async () => {
    mockGetEpic.mockRejectedValue(
      boardError(404, { error: 'board: epic "epc-nope9999" not found (did you mean: [epc-eeee1111]?)', suggestions: ["epc-eeee1111"] }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    fireEvent.dblClick(screen.getByText("E1"));

    const notice = await screen.findByRole("alert");
    expect(notice.textContent).toContain("not found");
    expect(screen.getByTestId("board-error-suggestions").textContent).toContain("epc-eeee1111");
  });
});
