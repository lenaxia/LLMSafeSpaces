// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  renderBoard,
  seedBoard,
  mockMoveStage,
  makeDataTransfer,
  boardError,
  defaultTasks,
  TASK_IN_REVIEW,
} from "./boardTestUtils";

// §4.6/§4.8: drag = move_stage optimistic + server-arbitrated snap-back.
// The engine's guard outcomes (409 + code: done_guard, stage_transition,
// claim_token) render verbatim — the server is the single arbiter.

function dragCardTo(shortId: string, stage: string) {
  const dt = makeDataTransfer();
  fireEvent.dragStart(screen.getByTestId(`task-card-${shortId}`), { dataTransfer: dt });
  fireEvent.drop(screen.getAllByTestId(`stage-drop-${stage}`)[0]!, { dataTransfer: dt });
}

describe("BoardPage — drag: optimistic, server-arbitrated, honest guards", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("optimistically moves the card and confirms on the server's row", async () => {
    // Deferred mutation: hold the server answer so the OPTIMISTIC phase
    // is observable (optimistic survival — the #1687 pin class).
    let resolveMove!: (t: typeof TASK_IN_REVIEW) => void;
    mockMoveStage.mockImplementation(
      () => new Promise((res) => { resolveMove = (t) => res(t); }),
    );
    const movedTasks = seedBoard();
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    dragCardTo("tsk-cccc0003", "in_progress");

    // Optimistic: the card appears in the target column while pending.
    await waitFor(() => {
      const moved = screen.getByTestId("task-card-tsk-cccc0003");
      expect(moved.closest('[data-stage-drop="in_progress"]')).not.toBeNull();
    });
    expect(mockMoveStage).toHaveBeenCalledWith("tsk-cccc0003", "in_progress", undefined);

    // Server answers: the authoritative row (in_progress) confirms the
    // optimistic placement — no snap-back.
    const updated = { ...TASK_IN_REVIEW, stage: "in_progress" };
    const refreshed = movedTasks.map((t) => (t.short_id === updated.short_id ? updated : t));
    seedBoard(refreshed);
    resolveMove(updated);
    await waitFor(() => {
      const settled = screen.getByTestId("task-card-tsk-cccc0003");
      expect(settled.closest('[data-stage-drop="in_progress"]')).not.toBeNull();
      expect(settled.getAttribute("data-stage")).toBe("in_progress");
    });
  });

  it("snaps back and renders the guard code when the server refuses (done_guard)", async () => {
    mockMoveStage.mockRejectedValue(
      boardError(409, { error: "board: guard done_guard: all acceptance criteria must be met before done (ruling R2)", code: "done_guard" }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    dragCardTo("tsk-cccc0003", "in_progress");
    // refuse comes from the mocked mutation
    await waitFor(() => expect(mockMoveStage).toHaveBeenCalled());
    await waitFor(() => {
      const snapped = screen.getByTestId("task-card-tsk-cccc0003");
      expect(snapped.closest('[data-stage-drop="in_review"]')).not.toBeNull();
    });

    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("done_guard");
    expect(notice.textContent).toContain("done_guard");
    expect(notice.textContent).toContain("snap");
  });

  it("renders the stage_transition guard honestly on an unenumerated drop", async () => {
    mockMoveStage.mockRejectedValue(
      boardError(409, { error: "board: guard stage_transition: stage transition backlog→in_progress is not enumerated in the machine", code: "stage_transition" }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    dragCardTo("tsk-aaaa0001", "in_progress");
    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-aaaa0001", "in_progress", undefined));
    await waitFor(() => {
      const snapped = screen.getByTestId("task-card-tsk-aaaa0001");
      expect(snapped.closest('[data-stage-drop="backlog"]')).not.toBeNull();
    });
    const notice = screen.getByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("stage_transition");
  });

  it("renders the claim_token guard when dragging a worker-held row", async () => {
    mockMoveStage.mockRejectedValue(
      boardError(409, { error: "board: guard claim_token: stage writes on a claimed task require its live claim token", code: "claim_token" }),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    dragCardTo("tsk-bbbb0002", "in_review");
    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-bbbb0002", "in_review", undefined));
    const notice = await screen.findByRole("alert");
    expect(notice.getAttribute("data-board-error")).toBe("claim_token");
    // snapped back to in_progress
    const snapped = screen.getByTestId("task-card-tsk-bbbb0002");
    expect(snapped.closest('[data-stage-drop="in_progress"]')).not.toBeNull();
  });

  it("prompts for the evidence URL on a → done drop and threads it to the call", async () => {
    mockMoveStage.mockResolvedValue({ ...TASK_IN_REVIEW, stage: "done" });
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    dragCardTo("tsk-cccc0003", "done");
    // No move yet: the evidence prompt intercepts.
    expect(mockMoveStage).not.toHaveBeenCalled();
    const prompt = await screen.findByTestId("evidence-url");
    fireEvent.change(prompt, { target: { value: "https://github.com/lenaxia/LLMSafeSpaces/pull/1728" } });
    fireEvent.click(screen.getByTestId("confirm-done"));

    await waitFor(() =>
      expect(mockMoveStage).toHaveBeenCalledWith("tsk-cccc0003", "done", "https://github.com/lenaxia/LLMSafeSpaces/pull/1728"),
    );
  });

  it("disables the Done confirm until an evidence URL is present (required on → done, R2)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    dragCardTo("tsk-cccc0003", "done");
    const confirm = await screen.findByTestId("confirm-done");
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
  });

  it("previews unmet criteria in the done prompt, without vetoing the server's call", async () => {
    mockMoveStage.mockResolvedValue(TASK_IN_REVIEW);
    seedBoard(
      defaultTasks().map((t) =>
        t.short_id === TASK_IN_REVIEW.short_id
          ? { ...t, acceptance_criteria: [{ text: "tests green", met: false }] }
          : t,
      ),
    );
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");

    dragCardTo("tsk-cccc0003", "done");
    const readiness = await screen.findByTestId("done-readiness-view");
    expect(readiness.textContent).toContain("tests green");
    expect(readiness.textContent).toContain("done_guard");
    // The move is still offered — the engine arbitrates, the client mirrors.
    fireEvent.change(screen.getByTestId("evidence-url"), { target: { value: "https://pr/1" } });
    fireEvent.click(screen.getByTestId("confirm-done"));
    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-cccc0003", "done", "https://pr/1"));
  });

  it("drops onto the collapsed side columns too (pending_input exit)", async () => {
    const seeded = seedBoard();
    mockMoveStage.mockImplementation(async () => {
      // The server's authoritative row lands pending_input; the reseed
      // (query invalidation) must reflect it or the card snaps back.
      const updated = { ...TASK_IN_REVIEW, stage: "pending_input", pending_origin: "worker" };
      seedBoard(seeded.map((t) => (t.short_id === updated.short_id ? updated : t)));
      return updated;
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");
    screen.getAllByTestId("side-columns-toggle")[0]!.click();
    await screen.findAllByTestId("stage-drop-pending_input");

    dragCardTo("tsk-cccc0003", "pending_input");
    await waitFor(() => expect(mockMoveStage).toHaveBeenCalledWith("tsk-cccc0003", "pending_input", undefined));
    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-cccc0003").closest('[data-stage-drop="pending_input"]')).not.toBeNull();
    });
  });

  it("ignores a no-op drop onto the card's own stage", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-cccc0003");
    dragCardTo("tsk-cccc0003", "in_review");
    expect(mockMoveStage).not.toHaveBeenCalled();
  });
});
