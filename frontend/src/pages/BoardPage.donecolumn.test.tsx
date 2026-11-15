// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// BoardPage.donecolumn.test.tsx — red-first pins for the Done column's
// cap + drawer (owner ask 2026-11-13: hundreds of completed stories
// render inline, pushing the board layout). The hybrid shape:
//   (a) >N done stories → the column renders exactly N newest-done
//       inline + a "Show all N done" affordance opening a scrollable
//       drawer with the FULL set;
//   (b) ≤N done stories → renders as today (all inline, no affordance);
//   (c) newest-done first ordering pinned in BOTH the capped preview
//       and the drawer (the server list arrives created_at-asc — the
//       recency order is applied view-side).

import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor, within } from "@testing-library/react";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import { renderBoard, seedBoard, PROJECT_A, PROJECT_B } from "./boardTestUtils";
import type { BoardTask } from "../api/board";
import { DONE_COLUMN_MAX_VISIBLE } from "./BoardPage";

/** A done story done at `enteredAt` (stage_entered_at = when the row
 * entered Done — the recency clock). Titles carry the day so order
 * assertions read humanly. */
function doneTask(n: number, enteredAt: string, projectId = PROJECT_A.id): BoardTask {
  return {
    id: `task-uuid-done-${n}`,
    project_id: projectId,
    epic_id: "epic-uuid-1",
    short_id: `tsk-done${String(n).padStart(4, "0")}`,
    title: `Done ${enteredAt.slice(5, 10)} #${n}`,
    description: "",
    stage: "done",
    pending_input: false,
    paused: false,
    project_name: projectId === PROJECT_A.id ? "Apollo" : "Borealis",
    epic_name: "E1",
    tags: [],
    acceptance_criteria: [],
    is_blocked: false,
    attempts: 0,
    claim_incremented_attempts: false,
    last_attempt_failed: false,
    failure_count: 0,
    stage_entered_at: enteredAt,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: enteredAt,
  };
}

/** 13 done stories entered on staggered days 01..13 — the >N fixture.
 * `tsk-done0013` is the NEWEST done (must render first). */
function manyDone(): BoardTask[] {
  return Array.from({ length: 13 }, (_, i) => doneTask(i + 1, `2026-10-${String(i + 1).padStart(2, "0")}T00:00:00Z`));
}

/** The done cell's inline card ids, in DOM order, scoped to a lane. */
function laneDoneCardIds(laneShortId: string): string[] {
  const lane = screen.getByTestId(`swimlane-${laneShortId}`);
  const cell = lane.querySelector('[data-testid="stage-drop-done"]')!;
  expect(cell).not.toBeNull();
  return [...cell.querySelectorAll("[data-task-card]")].map((el) => el.getAttribute("data-task-card")!);
}

describe("BoardPage — Done column cap + drawer (owner ask 2026-11-13)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // --- Pin (a): >N → exactly N inline + the affordance ------------------------

  it("renders exactly DONE_COLUMN_MAX_VISIBLE inline cards + the 'Show all N done' affordance when more exist", async () => {
    seedBoard([...manyDone(), { ...doneTask(90, "2026-10-05T00:00:00Z", PROJECT_B.id) }]);
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");

    // The cap: exactly 10 inline (13 exist) — the column's height stays
    // in line with the other through-flow columns.
    const ids = laneDoneCardIds(PROJECT_A.short_id);
    expect(ids).toHaveLength(DONE_COLUMN_MAX_VISIBLE);

    // The affordance names the FULL count (the column's badge keeps the
    // lane's truth — the cap is visual, nothing is dropped).
    const affordance = screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`);
    expect(affordance.textContent).toContain(`Show all 13 done`);

    // The header count badge stays the full 13 (never the capped 10).
    const lane = screen.getByTestId(`swimlane-${PROJECT_A.short_id}`);
    const cell = lane.querySelector('[data-testid="stage-drop-done"]')!;
    const headerCount = [...cell.querySelectorAll("span")].find((s) => s.textContent === "13");
    expect(headerCount).not.toBeUndefined();

    // Other lanes are independent: lane B has 1 done — no affordance.
    expect(laneDoneCardIds(PROJECT_B.short_id)).toHaveLength(1);
    expect(screen.queryByTestId(`done-show-all-${PROJECT_B.short_id}`)).toBeNull();
  });

  // --- Pin (c): newest-done first in the capped preview -----------------------

  it("orders the inline preview newest-done first (stage_entered_at desc)", async () => {
    seedBoard(manyDone());
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");

    const ids = laneDoneCardIds(PROJECT_A.short_id);
    // The 10 NEWEST (days 13..04), newest first — days 01..03 live only
    // behind the affordance.
    expect(ids).toEqual([
      "tsk-done0013", "tsk-done0012", "tsk-done0011", "tsk-done0010", "tsk-done0009",
      "tsk-done0008", "tsk-done0007", "tsk-done0006", "tsk-done0005", "tsk-done0004",
    ]);
  });

  // --- Pin (a)+(c): the drawer reveals the FULL set, scrollable, newest-first -

  it("the affordance opens the drawer: every done story, newest first, in its own scroll region", async () => {
    seedBoard(manyDone());
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");

    fireEvent.click(screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`));

    expect((await screen.findByTestId("done-drawer-title")).textContent).toContain("Apollo");
    expect(screen.getByTestId("done-drawer-count").textContent).toContain("13");

    const list = screen.getByTestId("done-drawer-list");
    expect(list.className).toContain("overflow-y-auto");
    const ids = [...list.querySelectorAll("[data-task-card]")].map((el) => el.getAttribute("data-task-card")!);
    expect(ids).toHaveLength(13);
    expect(ids[0]).toBe("tsk-done0013");
    expect(ids[12]).toBe("tsk-done0001");
  });

  it("closing the drawer returns to the board; Escape closes it (overlay conventions)", async () => {
    seedBoard(manyDone());
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");
    fireEvent.click(screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`));
    await screen.findByTestId("done-drawer-list");

    // Radix Dialog: Escape dismisses (the same convention as the story
    // drawer / filter drawer).
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("done-drawer-list")).toBeNull());

    // Reopen via the affordance, then the explicit close button.
    fireEvent.click(screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`));
    await screen.findByTestId("done-drawer-list");
    fireEvent.click(screen.getByTestId("done-drawer-close"));
    await waitFor(() => expect(screen.queryByTestId("done-drawer-list")).toBeNull());
    // The board is intact.
    expect(await screen.findByTestId("task-card-tsk-done0013")).toBeTruthy();
  });

  it("the affordance is a keyboard-reachable native button", async () => {
    seedBoard(manyDone());
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");

    const affordance = screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`);
    expect(affordance.tagName).toBe("BUTTON");
    affordance.focus();
    expect(document.activeElement).toBe(affordance);
    expect(affordance.getAttribute("aria-haspopup")).toBe("dialog");
  });

  it("drawer cards keep the board affordances: details opens the story drawer above, closing returns to the list", async () => {
    seedBoard(manyDone());
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0013");
    fireEvent.click(screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`));
    await screen.findByTestId("done-drawer-list");

    // The newest done story renders BOTH inline (top of the capped
    // preview) and in the drawer — act on the drawer's copy.
    const drawerList = screen.getByTestId("done-drawer-list");
    fireEvent.click(within(drawerList).getByTestId("details-tsk-done0013"));
    // The story-detail drawer opens ABOVE the done drawer (later-opened
    // overlay stacks above) with the newest done story's title.
    expect((await screen.findByTestId("drawer-title")).textContent).toContain("Done 10-13 #13");
    expect(screen.getByTestId("done-drawer-list")).toBeTruthy();

    fireEvent.click(screen.getByTestId("drawer-close"));
    await waitFor(() => expect(screen.queryByTestId("drawer-title")).toBeNull());
    // Back on the done drawer's list — the return-to-list path.
    expect(screen.getByTestId("done-drawer-list")).toBeTruthy();
  });

  // --- Pin (b): ≤N → renders as today (regression pin) ------------------------

  it("at N done stories renders ALL inline with no affordance (the cap's complement)", async () => {
    const ten = manyDone().slice(0, DONE_COLUMN_MAX_VISIBLE);
    seedBoard(ten);
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0010");

    expect(laneDoneCardIds(PROJECT_A.short_id)).toHaveLength(DONE_COLUMN_MAX_VISIBLE);
    expect(screen.queryByTestId(`done-show-all-${PROJECT_A.short_id}`)).toBeNull();
  });

  it("below N renders every done story inline, newest first, no affordance (regression pin)", async () => {
    // 3 done stories + the default backlog/in_progress/review rows so
    // the lane renders its through-flow columns normally.
    seedBoard([
      doneTask(1, "2026-10-01T00:00:00Z"),
      doneTask(2, "2026-10-11T00:00:00Z"),
      doneTask(3, "2026-10-06T00:00:00Z"),
    ]);
    renderBoard();
    await screen.findByTestId("task-card-tsk-done0002");

    const ids = laneDoneCardIds(PROJECT_A.short_id);
    expect(ids).toHaveLength(3);
    // Newest-done first even below the cap (ordering is the column's,
    // not the cap's).
    expect(ids).toEqual(["tsk-done0002", "tsk-done0003", "tsk-done0001"]);
    expect(screen.queryByTestId(`done-show-all-${PROJECT_A.short_id}`)).toBeNull();
  });
});
