// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor, act } from "@testing-library/react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { renderBoard, seedBoard, PROJECT_A, defaultTasks } from "./boardTestUtils";
import { MAIN_STAGES, type BoardStage } from "../lib/boardRules";

// Owner direction 2026-11-15 fast-follow: FOCUS-COLUMN MODE, portrait
// only — the focused column takes the width (70%), neighbors compress
// (10% stubs, header + chevron remain tappable transit affordances).
// Focus follows SNAP-SETTLED, never live swipe position: no scroll
// event ever mutates geometry; the native scrollend (scrolling fully
// stopped) is the one scroll-derived transit moment. Overview is the
// default and the board — every mount starts there, focus is never
// persisted. Every mode change is user-initiated and reversible in one
// action: chevron/Enter enter, swipe-settle/compressed-header/arrows
// transit, chevron/Escape/pinch-out exit (exit re-centers the overview
// on the column just left). Landscape/desktop: no affordances, and the
// md crossing force-exits the lens.

const RAIL_ID = `stage-grid-${PROJECT_A.short_id}`;
const NOTICE_ID = `rail-mode-notice-${PROJECT_A.short_id}`;

function column(stage: BoardStage) {
  return screen.getAllByTestId(`stage-col-${stage}`)[0]!;
}

function rail() {
  return screen.getByTestId(RAIL_ID);
}

function chevron(stage: BoardStage) {
  return column(stage).querySelector(`[data-testid="col-focus-toggle-${stage}"]`) as HTMLButtonElement;
}

function colMode(stage: BoardStage) {
  return column(stage).getAttribute("data-col-state");
}

async function boardReady() {
  await screen.findByTestId("task-card-tsk-aaaa0001");
}

/** jsdom lays nothing out — the scrollend handler's nearest-column
 * geometry is supplied per test: the rail's scrollLeft and every
 * column's offsetLeft (content offsets; offsetLeft is layout-stable,
 * so the numbers are frame-independent). */
function mockRailGeometry(offsets: Partial<Record<BoardStage, number>> & { scrollLeft?: number }) {
  const railEl = rail();
  Object.defineProperty(railEl, "scrollLeft", { value: offsets.scrollLeft ?? 0, configurable: true });
  Object.defineProperty(railEl, "offsetLeft", { value: 0, configurable: true });
  for (const stage of MAIN_STAGES) {
    const left = offsets[stage];
    if (left === undefined) continue;
    Object.defineProperty(column(stage), "offsetLeft", { value: left, configurable: true });
  }
}

function fireScrollEnd() {
  rail().dispatchEvent(new Event("scrollend", { bubbles: true }));
}

describe("BoardPage — focus-column mode (owner direction 2026-11-15 fast-follow, portrait only)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  // --- (c) overview is the default and the board; focus never persisted ---

  it("every mount starts in overview: rail data-mode=overview, every column overview widths, chevrons collapsed", async () => {
    renderBoard();
    await boardReady();

    expect(rail().getAttribute("data-mode")).toBe("overview");
    for (const stage of MAIN_STAGES) {
      expect(colMode(stage)).toBe("overview");
      // The overview width pins (29/36/30/full-page done) still own
      // the classes — no focus width leaks into the default state.
      expect(column(stage).className).toMatch(/w-\[calc\((29|36|30|100)/);
      expect(chevron(stage).getAttribute("aria-expanded")).toBe("false");
    }
    // No mode notice until a mode change happens.
    expect(screen.getByTestId(NOTICE_ID).textContent).toBe("");
  });

  it("focus is ephemeral: no focus-flavored storage write, and a remount starts in overview again", async () => {
    const setItemSpy = vi.spyOn(Storage.prototype, "setItem");
    const { unmount } = renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    // Nothing about the lens ever touches storage (mode is component
    // state, never persisted — overview is the board).
    for (const call of setItemSpy.mock.calls) {
      expect(String(call[0])).not.toMatch(/focus|col-state|data-mode|rail/i);
    }

    unmount();
    renderBoard();
    await boardReady();
    expect(rail().getAttribute("data-mode")).toBe("overview");
    expect(colMode("in_progress")).toBe("overview");
  });

  // --- (a) enter: header chevron / arrows + Enter — each ONE user action ---

  it("enter via the header chevron: focused column takes 70%, neighbors compress to 10% stubs, aria-expanded tracks the lens", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));

    expect(rail().getAttribute("data-mode")).toBe("focus");
    expect(colMode("in_progress")).toBe("focused");
    expect(column("in_progress").className).toContain("w-[calc(70%-0.375rem)]");
    for (const stage of ["backlog", "in_review"] as BoardStage[]) {
      expect(colMode(stage)).toBe("compressed");
      expect(column(stage).className).toContain("w-[calc(10%-0.375rem)]");
      // The stub floor replaces the 4.5rem overview floor (cn is
      // tailwind-merge).
      expect(column(stage).className).toContain("min-w-[3rem]");
      expect(column(stage).className).not.toContain("min-w-[4.5rem]");
      expect(chevron(stage).getAttribute("aria-expanded")).toBe("false");
    }
    // The non-focused done column keeps its FULL PAGE (reachability:
    // its snap point must stay within max scroll) — periphery: wide,
    // cards visible, chevron still transits.
    expect(colMode("done")).toBe("periphery");
    expect(column("done").className).toContain("w-[calc(100%+0.75rem)]");
    expect(chevron("done").getAttribute("aria-expanded")).toBe("false");
    expect(chevron("in_progress").getAttribute("aria-expanded")).toBe("true");
    // The mode change is announced (#2021).
    expect(screen.getByTestId(NOTICE_ID).textContent).toContain("Focused In Progress column");
  });

  it("enter via keyboard: Enter on the roving-stopped column enters focus there; Enter on a CARD does not", async () => {
    renderBoard();
    await boardReady();

    column("in_progress").focus();
    fireEvent.keyDown(column("in_progress"), { key: "Enter" });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));
    expect(colMode("in_progress")).toBe("focused");

    // Exit, then prove the card guard: Enter with the event target a
    // card inside the column is the card's key, not the lens's.
    fireEvent.keyDown(column("in_progress"), { key: "Escape" });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
    fireEvent.keyDown(screen.getByTestId("task-card-tsk-aaaa0001"), { key: "Enter" });
    expect(rail().getAttribute("data-mode")).toBe("overview");
  });

  // --- (a) transit: compressed-header tap / chevron / arrows — one action ---

  it("transit via compressed-header tap: the tapped neighbor becomes the focused column", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));

    const backlogHeader = column("backlog").querySelector('[data-stage-header="backlog"]')!;
    fireEvent.click(backlogHeader);
    await waitFor(() => expect(colMode("backlog")).toBe("focused"));
    expect(colMode("in_progress")).toBe("compressed");
    expect(screen.getByTestId(NOTICE_ID).textContent).toContain("Focused Backlog column");
  });

  it("transit via a compressed neighbor's chevron: same one-action path", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(colMode("backlog")).toBe("focused"));
    fireEvent.click(chevron("in_review"));
    await waitFor(() => expect(colMode("in_review")).toBe("focused"));
    expect(colMode("backlog")).toBe("compressed");
  });

  it("arrow keys TRANSIT in focus mode (the carousel feel): ArrowRight moves the lens and DOM focus to the adjacent column", async () => {
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(colMode("backlog")).toBe("focused"));

    fireEvent.keyDown(column("backlog"), { key: "ArrowRight" });
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));
    expect(document.activeElement).toBe(column("in_progress"));
    expect(colMode("backlog")).toBe("compressed");
    scrollSpy.mockRestore();
  });

  // --- (b) focus follows snap-settled, never live swipe; frozen mid-gesture ---

  it("geometry is FROZEN during an in-flight gesture: scroll events never reflow; only the settled scrollend transits", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));
    const before = column("in_progress").className;

    // The gesture: mid-flight scroll events with the finger resting on
    // done — widths must not move (a live reflow would drag the snap
    // target under the finger).
    mockRailGeometry({ scrollLeft: 300, backlog: 0, in_progress: 100, in_review: 200, done: 300 });
    fireEvent.scroll(rail());
    fireEvent.scroll(rail());
    expect(rail().getAttribute("data-mode")).toBe("focus");
    expect(colMode("in_progress")).toBe("focused");
    expect(column("in_progress").className).toBe(before);
    expect(colMode("done")).toBe("periphery");

    // The settle: scrollend lands the lens on the snapped column…
    fireScrollEnd();
    await waitFor(() => expect(colMode("done")).toBe("focused"));
    expect(colMode("in_progress")).toBe("compressed");
    expect(screen.getByTestId(NOTICE_ID).textContent).toContain("Focused Done column");
  });

  it("snap-settled transit re-snaps the expanded column into view after the reflow", async () => {
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));
    scrollSpy.mockClear();

    mockRailGeometry({ scrollLeft: 200, backlog: 0, in_progress: 100, in_review: 200, done: 300 });
    fireScrollEnd();
    await waitFor(() => expect(colMode("in_review")).toBe("focused"));
    // …and the explicit re-snap targets the newly expanded column (the
    // timer lands just past the 200ms width transition).
    await waitFor(() => {
      expect(scrollSpy.mock.instances.at(-1)).toBe(column("in_review"));
      expect(scrollSpy).toHaveBeenLastCalledWith({ behavior: "smooth", inline: "start", block: "nearest" });
    });
    scrollSpy.mockRestore();
  });

  it("settling back on the already-focused column is a no-op (no reflow, no notice churn)", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));
    const noticeBefore = screen.getByTestId(NOTICE_ID).textContent;

    mockRailGeometry({ scrollLeft: 100, backlog: 0, in_progress: 100, in_review: 200, done: 300 });
    fireScrollEnd();
    // No state change to wait for — the assert is immediate stability.
    expect(colMode("in_progress")).toBe("focused");
    expect(screen.getByTestId(NOTICE_ID).textContent).toBe(noticeBefore);
  });

  it("scrollend in OVERVIEW moves nothing: swiping never changes mode, only position within it", async () => {
    renderBoard();
    await boardReady();

    mockRailGeometry({ scrollLeft: 300, backlog: 0, in_progress: 100, in_review: 200, done: 300 });
    fireScrollEnd();
    expect(rail().getAttribute("data-mode")).toBe("overview");
    for (const stage of MAIN_STAGES) expect(colMode(stage)).toBe("overview");
  });

  // --- (a) exit: chevron / Escape / pinch-out — each ONE action; (d) re-center ---

  it("exit via the same chevron (toggle symmetry): overview restores and RE-CENTERS on the column just left", async () => {
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_review"));
    await waitFor(() => expect(colMode("in_review")).toBe("focused"));
    scrollSpy.mockClear();

    fireEvent.click(chevron("in_review"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
    for (const stage of MAIN_STAGES) expect(colMode(stage)).toBe("overview");
    // The overview re-centers on the LAST-FOCUSED column — no jump to
    // position zero.
    expect(screen.getByTestId(NOTICE_ID).textContent).toContain("Overview — In Review column centered");
    await waitFor(() => expect(scrollSpy.mock.instances.at(-1)).toBe(column("in_review")));
    scrollSpy.mockRestore();
  });

  it("exit via Escape (the desktop reflex): one keypress returns the overview", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("done"));
    await waitFor(() => expect(colMode("done")).toBe("focused"));
    fireEvent.keyDown(column("done"), { key: "Escape" });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
    expect(screen.getByTestId(NOTICE_ID).textContent).toContain("Overview — Done column centered");
  });

  it("Escape inside an OPEN menu reaches the menu, not the lens (the r1 overlay guard)", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));

    // Open the card kebab (portal menu) and press Escape there.
    fireEvent.click(screen.getByTestId("task-kebab-tsk-bbbb0002"));
    const item = (await screen.findAllByRole("menuitem"))[0]!;
    fireEvent.keyDown(item, { key: "Escape" });
    expect(rail().getAttribute("data-mode")).toBe("focus");

    // …while the same key on the column itself exits.
    fireEvent.keyDown(column("in_progress"), { key: "Escape" });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
  });

  it("exit via pinch-out (zoom-out muscle memory): a spreading two-finger gesture exits — a small pinch does not", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));

    // Two fingers at spread 40px; a 45px move stays under the 25%
    // threshold — the lens holds.
    fireEvent.touchStart(rail(), { touches: [{ identifier: 1, clientX: 100, clientY: 50 }, { identifier: 2, clientX: 140, clientY: 50 }] });
    fireEvent.touchMove(rail(), { touches: [{ identifier: 1, clientX: 100, clientY: 50 }, { identifier: 2, clientX: 145, clientY: 50 }] });
    expect(rail().getAttribute("data-mode")).toBe("focus");

    // Spread to 60px (+50%): one pinch-out, one exit.
    fireEvent.touchMove(rail(), { touches: [{ identifier: 1, clientX: 100, clientY: 50 }, { identifier: 2, clientX: 160, clientY: 50 }] });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
  });

  // --- (e) landscape/desktop: no focus mode ---

  it("the chevron affordance is portrait-only (md:hidden); the md crossing force-exits an open lens", async () => {
    let mdMatches = false;
    const changeListeners: Array<(ev: { matches: boolean }) => void> = [];
    vi.stubGlobal(
      "matchMedia",
      (query: string) => ({
        get matches() {
          return mdMatches;
        },
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: (_t: string, fn: (ev: { matches: boolean }) => void) => changeListeners.push(fn),
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }),
    );

    renderBoard();
    await boardReady();

    for (const stage of MAIN_STAGES) {
      expect(chevron(stage).className).toContain("md:hidden");
    }

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    // Rotate to landscape (md+): the lens must leave with the affordances.
    mdMatches = true;
    act(() => {
      for (const fn of changeListeners) fn({ matches: true });
    });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
  });

  // --- compressed stubs + the focused reading surface ---

  it("compressed neighbors hide their cards (navigation stubs); the focused column keeps its cards interactive", async () => {
    renderBoard();
    await boardReady();

    // Overview: every lane-A stage card visible.
    expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(colMode("backlog")).toBe("focused"));

    // in_progress's card is inside a hidden stub wrapper (Tailwind's
    // .hidden is class-only in jsdom — the wrapper class is the truth
    // component tests can pin; the e2e suite measures real geometry).
    const busyCard = screen.getByTestId("task-card-tsk-bbbb0002");
    const stubWrapper = busyCard.closest(".hidden");
    expect(stubWrapper).not.toBeNull();
    expect(stubWrapper!.classList.contains("hidden")).toBe(true);
    // The focused column's card stays rendered on the open surface.
    expect(screen.getByTestId("task-card-tsk-aaaa0001").closest(".hidden")).toBeNull();
  });

  // --- #2021: the announcement channel ---

  it("the mode announcer is a polite live region (role=status, sr-only)", async () => {
    renderBoard();
    await boardReady();

    const notice = screen.getByTestId(NOTICE_ID);
    expect(notice.getAttribute("role")).toBe("status");
    expect(notice.getAttribute("aria-live")).toBe("polite");
    expect(notice.className).toContain("sr-only");
  });

  // --- regression: the untouched surfaces ---

  it("the Done column focuses like any other — focused done IS the full page (its overview width)", async () => {
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("done"));
    await waitFor(() => expect(colMode("done")).toBe("focused"));
    // Focused done = the full final page (reachability keeps the last
    // column ≥ the rail width — same as its overview treatment).
    expect(column("done").className).toContain("w-[calc(100%+0.75rem)]");
    for (const stage of ["backlog", "in_progress", "in_review"] as BoardStage[]) {
      expect(colMode(stage)).toBe("compressed");
    }
  });

  it("the drain regression holds: a fully-drained board still enters focus (empty columns compress as stubs)", async () => {
    const doneOnly = defaultTasks().map((t) => ({ ...t, project_id: PROJECT_A.id, stage: "done" as const }));
    seedBoard(doneOnly);
    renderBoard();
    await screen.findByTestId(RAIL_ID);

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(colMode("in_progress")).toBe("focused"));
    expect(colMode("done")).toBe("periphery");
  });
});
