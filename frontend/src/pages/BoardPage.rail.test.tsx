// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { renderBoard, seedBoard, mockListProjects, mockListProjectTasks, PROJECT_A, defaultTasks, TASK_DONE } from "./boardTestUtils";
import { MAIN_STAGES, STAGE_LABELS, type BoardStage } from "../lib/boardRules";

// Owner direction 2026-11-15: the board is a PIPELINE — pipeline state
// reads left-to-right; the old 2x2 grid put in_review under in_progress,
// destroying the flow axis. The 1x4 rail: one row, all four columns,
// scroll-snap per column (snap points ARE the navigation). Portrait:
// the three active columns share 95% of the rail; done's edge peeks in
// the final 5% (carousel edge-peek — terminal reference, discoverable
// without real estate). Landscape/desktop (md+): all four visible.
// Empty columns keep their width — fixed-basis wrappers, never
// content-driven, so the layout cannot jump as stages drain.

const RAIL = `stage-grid-${PROJECT_A.short_id}`;

function column(stage: BoardStage) {
  return screen.getAllByTestId(`stage-col-${stage}`)[0]!;
}

/** The portrait width weights: the ACTIVE trio (backlog, in_progress,
 * in_review) shares 95% of the rail so done's edge holds the last 5%;
 * in_progress runs widest (titles + assignee + lease chips are the
 * densest column). Each active column cedes 0.375rem so the trio's
 * two gutters + done's gutter (3 × gap-2 = 1.5rem) never eat the
 * peek. done renders as a FULL-WIDTH final page: at a share width its
 * snap point (and in_progress's) would sit past the rail's max scroll
 * and mandatory snap would freeze at the clamp — the full page keeps
 * every column's snap-start reachable. */
const PORTRAIT_WIDTH: Record<BoardStage, string> = {
  backlog: "w-[calc(29%-0.375rem)]",
  in_progress: "w-[calc(36%-0.375rem)]",
  in_review: "w-[calc(30%-0.375rem)]",
  done: "w-[calc(100%+0.75rem)]",
  failed: "",
};

/** Landscape/desktop: all four columns visible in one row — 24/28/24/24
 * splits to 100% minus the shared gutter allowance, with min-widths
 * holding the floor (the scroll container absorbs narrower windows). */
const LANDSCAPE_WIDTH: Record<BoardStage, string> = {
  backlog: "md:w-[calc(24%-0.375rem)]",
  in_progress: "md:w-[calc(28%-0.375rem)]",
  in_review: "md:w-[calc(24%-0.375rem)]",
  done: "md:w-[calc(24%-0.375rem)]",
  failed: "",
};

describe("BoardPage — the 1x4 pipeline rail (owner direction 2026-11-15)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("renders one horizontal scroll-snap rail: flex row, snap-mandatory, all four columns in MAIN_STAGES order", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const rail = screen.getByTestId(RAIL);
    // One row: the flex rail replaces the 2x2 grid; overflow scrolls
    // horizontally (never the page), snapping per column.
    expect(rail.className).toContain("flex");
    expect(rail.className).toContain("snap-x");
    expect(rail.className).toContain("snap-mandatory");
    expect(rail.className).toContain("overflow-x-auto");
    // r1: the snapport insets to the content box (snap points land at
    // the resting column origin, not the border box).
    expect(rail.className).toContain("scroll-px-2");
    expect(rail.className).not.toContain("grid-cols-2");
    // The flow axis: MAIN_STAGES order is the DOM order.
    const cols = rail.querySelectorAll("[data-stage-col]");
    expect(Array.from(cols).map((c) => c.getAttribute("data-stage-col"))).toEqual(MAIN_STAGES);
    // Every column is a snap point and refuses to shrink.
    for (const stage of MAIN_STAGES) {
      expect(column(stage).className).toContain("snap-start");
      expect(column(stage).className).toContain("shrink-0");
    }
  });

  it("portrait: the active trio shares 95% (29/36/30) and done's edge carries the continuation affordance", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const pct = (stage: BoardStage) => Number(column(stage).className.match(/w-\[calc\((\d+)%/)?.[1]);
    expect(pct("backlog")).toBe(29);
    expect(pct("in_progress")).toBe(36);
    expect(pct("in_review")).toBe(30);
    // 29 + 36 + 30 = 95: the done column's leading 5% is the peek.
    expect(pct("backlog") + pct("in_progress") + pct("in_review")).toBe(95);
    // Done itself renders wide enough to be a real snap target.
    expect(pct("done")).toBeGreaterThanOrEqual(30);
    // Exact class pins (the e2e spec measures the real 95/5 geometry).
    for (const stage of MAIN_STAGES) {
      expect(column(stage).className).toContain(PORTRAIT_WIDTH[stage]);
    }
  });

  it("landscape/desktop: all four columns carry md widths that split one row (24/28/24/24) with min-width floors", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    for (const stage of MAIN_STAGES) {
      expect(column(stage).className).toContain(LANDSCAPE_WIDTH[stage]);
      // The md floor: 7.5rem across the rail, 8rem for in_progress
      // (titles + assignee + lease chips are the densest column). cn()
      // is tailwind-merge — the per-stage floor REPLACES the base one,
      // so each column asserts exactly its own.
      const floor = stage === "in_progress" ? "md:min-w-[8rem]" : "md:min-w-[7.5rem]";
      expect(column(stage).className).toContain(floor);
    }
    const pct = (stage: BoardStage) => Number(column(stage).className.match(/md:w-\[calc\((\d+)%/)?.[1]);
    expect(pct("backlog") + pct("in_progress") + pct("in_review") + pct("done")).toBe(100);
  });

  // --- #2021 a11y discipline: arrow keys navigate columns, focus visible ---

  it("roving tabindex: the active column is the rail's only tab stop; ArrowRight/ArrowLeft move focus and scroll column-to-column", async () => {
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // Initial: backlog is the tab stop, the rest are arrow-reachable.
    expect(column("backlog").tabIndex).toBe(0);
    for (const stage of ["in_progress", "in_review", "done"] as BoardStage[]) {
      expect(column(stage).tabIndex).toBe(-1);
    }

    // ArrowRight: backlog → in_progress (focus + snap-scroll to its start).
    column("backlog").focus();
    expect(document.activeElement).toBe(column("backlog"));
    fireEvent.keyDown(column("backlog"), { key: "ArrowRight" });
    await waitFor(() => expect(document.activeElement).toBe(column("in_progress")));
    expect(column("in_progress").tabIndex).toBe(0);
    expect(column("backlog").tabIndex).toBe(-1);
    expect(scrollSpy).toHaveBeenLastCalledWith({ behavior: "smooth", inline: "start", block: "nearest" });
    expect(scrollSpy.mock.instances.at(-1)).toBe(column("in_progress"));

    // …through review to done; the ends clamp (no wrap, no crash).
    fireEvent.keyDown(column("in_progress"), { key: "ArrowRight" });
    await waitFor(() => expect(document.activeElement).toBe(column("in_review")));
    fireEvent.keyDown(column("in_review"), { key: "ArrowRight" });
    await waitFor(() => expect(document.activeElement).toBe(column("done")));
    fireEvent.keyDown(column("done"), { key: "ArrowRight" });
    expect(document.activeElement).toBe(column("done"));
    fireEvent.keyDown(column("done"), { key: "ArrowLeft" });
    await waitFor(() => expect(document.activeElement).toBe(column("in_review")));
    scrollSpy.mockRestore();
  });

  it("a11y tree: the rail is a labelled region; each column is a named group with a visible focus ring class", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const rail = screen.getByTestId(RAIL);
    expect(rail.getAttribute("role")).toBe("region");
    expect(rail.getAttribute("aria-label")).toContain("Pipeline stages");
    for (const stage of MAIN_STAGES) {
      const col = column(stage);
      expect(col.getAttribute("role")).toBe("group");
      expect(col.getAttribute("aria-label")).toBe(`${STAGE_LABELS[stage]} column`);
      // #2021: focus is VISIBLE on the keyboard path.
      expect(col.className).toContain("focus-visible:outline-2");
    }
  });

  it("arrow keys inside an OPEN kebab menu drive the menu, not the rail (r1: the portal guard)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    // Focus the in_progress column (the card's lane), open its kebab —
    // the menu renders through a document.body portal, but its React
    // keydowns still propagate through the column's tree.
    column("in_progress").focus();
    expect(document.activeElement).toBe(column("in_progress"));
    fireEvent.click(screen.getByTestId("task-kebab-tsk-bbbb0002"));
    const item = (await screen.findAllByRole("menuitem"))[0]!;

    // ArrowRight on the open menu: the rail must NOT steal the key —
    // focus stays put and the menu stays open for its own key handling.
    fireEvent.keyDown(item, { key: "ArrowRight" });
    expect(document.activeElement).toBe(column("in_progress"));
    expect(screen.getAllByRole("menuitem").length).toBeGreaterThan(0);
    // Sanity: the same key on the column itself (no menu in the event
    // path) still navigates — the guard scopes to menu subtrees only.
    fireEvent.click(screen.getByTestId("task-kebab-tsk-bbbb0002")); // close
    fireEvent.keyDown(column("in_progress"), { key: "ArrowRight" });
    await waitFor(() => expect(document.activeElement).toBe(column("in_review")));
  });

  // --- Empty-column width retention across the drain states ---

  it("an all-done board keeps all four columns at full width (skeleton cells, no collapse)", async () => {
    // Only done rows: backlog/in_progress/in_review drain to skeletons —
    // their wrappers must keep the exact same width classes.
    const doneRow = { ...TASK_DONE, project_id: PROJECT_A.id };
    seedBoard([doneRow]);
    renderBoard();
    await screen.findByTestId("task-card-tsk-dddd0004");

    for (const stage of MAIN_STAGES) {
      const col = column(stage);
      expect(col.className).toContain("shrink-0");
      expect(col.className).toContain(PORTRAIT_WIDTH[stage]);
      expect(col.querySelector('[data-stage-drop="' + stage + '"]')).not.toBeNull();
    }
    // The drained columns render the skeleton affordance, not nothing.
    expect(column("backlog").textContent).toContain("—");
    expect(column("in_progress").textContent).toContain("—");
  });

  it("a fully drained board (no stories anywhere) still renders four equal-footing columns", async () => {
    mockListProjects.mockResolvedValue([PROJECT_A]);
    mockListProjectTasks.mockResolvedValue([]);
    renderBoard();
    await screen.findByTestId(RAIL);

    const cols = screen.getByTestId(RAIL).querySelectorAll("[data-stage-col]");
    expect(cols).toHaveLength(4);
    for (const col of Array.from(cols)) {
      expect(col.className).toContain("shrink-0");
      expect(col.className).toMatch(/w-\[calc\(\d+%/);
    }
  });

  // --- Regression guards: the rail changes CHROME, not behavior ---

  it("the done column keeps its cap + show-all affordance inside the rail (the drawer opens from there)", async () => {
    const many = Array.from({ length: 13 }, (_, i) => ({
      ...TASK_DONE,
      id: `task-done-${i}`,
      short_id: `tsk-done${String(i + 1).padStart(4, "0")}`,
      title: `Done story ${i + 1}`,
      project_id: PROJECT_A.id,
    }));
    const tasks = defaultTasks().filter((t) => t.project_id !== PROJECT_A.id || t.stage !== "done").concat(many);
    seedBoard(tasks);
    renderBoard();
    await screen.findByTestId(`done-show-all-${PROJECT_A.short_id}`);

    // The capped column: 10 inline cards inside the done column.
    expect(column("done").querySelectorAll("[data-task-card]")).toHaveLength(10);
    fireEvent.click(screen.getByTestId(`done-show-all-${PROJECT_A.short_id}`));
    await screen.findByTestId("done-drawer-list");
  });

  it("kebab actions and card interactions are unchanged inside the rail (drag arms, kebab, details)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    // Card kebab still renders + opens (the interaction surface rides
    // the card, not the grid geometry).
    fireEvent.click(screen.getByTestId("task-kebab-tsk-bbbb0002"));
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-bbbb0002").textContent).toBeTruthy());
    // Drag arms still present on every column cell.
    for (const stage of MAIN_STAGES) {
      expect(column(stage).querySelector(`[data-stage-drop="${stage}"]`)).not.toBeNull();
    }
  });
});
