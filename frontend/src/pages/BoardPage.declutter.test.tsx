// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import {
  renderBoard,
  seedBoard,
  mockGetEpic,
  mockListProjectEpics,
  PROJECT_A,
} from "./boardTestUtils";
import type { BoardEpic } from "../api/board";
import { EPIC_LIST_MAX_VISIBLE } from "./BoardPage";

// 2026-10-28 owner rulings (board declutter + mobile layout):
//   1. no epic pills in the toolbar (pinned in scope tests);
//   2. one epic ROW per project — no nested task lists;
//   3. the epic list is CAPPED (~4 visible, overflow scrolls inside
//      the list's own region) and ordered by recent activity;
//   4. no epc-/tsk-/prj- ids in the chrome — titles are the labels;
//      ids render ONLY in the click-through metadata popups;
//   5. cards wrap and action buttons stay contained at narrow widths
//      (the responsive column collapse; layout pinned in e2e).

function epicWith(n: number, title: string, updated_at: string): BoardEpic {
  return {
    id: `epic-sort-${n}`, project_id: PROJECT_A.id, short_id: `epc-sort000${n}`,
    title, outcome: "", status: "active", tags: [], created_at: "", updated_at,
    progress_done: 0, progress_total: 0, progress_derived: false,
  };
}

describe("BoardPage — 2026-10-28 board declutter rulings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  // --- Ruling 3: capped list + activity order ---------------------------------

  it("caps the epic list at 4 visible rows (own scroll region) and renders every row", async () => {
    mockListProjectEpics.mockResolvedValue([
      epicWith(1, "Alpha old", "2026-10-01T00:00:00Z"),
      epicWith(2, "Bravo recent", "2026-10-07T00:00:00Z"),
      epicWith(3, "Charlie mid", "2026-10-05T00:00:00Z"),
      epicWith(4, "Delta mid2", "2026-10-06T00:00:00Z"),
      epicWith(5, "Echo none", ""), // quiet → alpha fallback
      epicWith(6, "Foxtrot older", "2026-10-02T00:00:00Z"),
    ]);
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    const list = lane.querySelector('[data-testid="epic-list-prj-aaaa1111"]')!;
    expect(list).not.toBeNull();
    // The cap is declared and enforced by the container's own scroll
    // region (overflow inside the list — the page never grows).
    expect(list.getAttribute("data-max-visible")).toBe(String(EPIC_LIST_MAX_VISIBLE));
    expect(list.className).toContain("overflow-y-auto");
    expect(list.className).toMatch(/max-h-\[/);
    // Every epic still renders (the cap is VISUAL — scrolling reaches
    // the rest). 6 server epics + the 2 task-derived (E1, E2).
    expect(list.querySelectorAll('[data-testid^="epic-band-"]').length).toBe(8);
  });

  it("orders the epic rows by recent activity first, alpha as the tiebreak", async () => {
    mockListProjectEpics.mockResolvedValue([
      epicWith(1, "Alpha old", "2026-10-01T00:00:00Z"),
      epicWith(2, "Bravo recent", "2026-10-07T00:00:00Z"),
      epicWith(3, "Charlie mid", "2026-10-05T00:00:00Z"),
      epicWith(4, "Delta mid2", "2026-10-06T00:00:00Z"),
      epicWith(5, "Echo none", ""),
      epicWith(6, "Foxtrot older", "2026-10-02T00:00:00Z"),
    ]);
    renderBoard();
    await screen.findByTestId("epic-band-epic-sort-2");

    const list = screen.getByTestId("swimlane-prj-aaaa1111").querySelector('[data-testid="epic-list-prj-aaaa1111"]')!;
    const titles = [...list.querySelectorAll('[data-testid^="band-title-"]')].map((el) => el.textContent);
    // Activity desc — the task-derived E1/E2 ride their rows'
    // updated_at (2026-10-10, newer than every seeded epic), then the
    // seeded epics by their own updated_at: 07, 06, 05, 02, 01 — the
    // timestamp-less Echo falls last (alpha among the quiet).
    expect(titles).toEqual([
      "E1",
      "E2",
      "Bravo recent",
      "Delta mid2",
      "Charlie mid",
      "Foxtrot older",
      "Alpha old",
      "Echo none",
    ]);
  });

  // --- Ruling 4: ids out of the chrome, into the popups -----------------------

  it("renders NO epc-/tsk-/prj- ids on lanes, epic rows, cards, or the focus select", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const lane = screen.getByTestId("swimlane-prj-aaaa1111");
    // The lane header/rows/cards are label-first: no raw short ids.
    expect(lane.textContent).not.toMatch(/(prj|epc|tsk)-/);
    // The focus select names projects by NAME (the id rides the value).
    const options = [...screen.getByTestId("project-focus").querySelectorAll("option")];
    expect(options.map((o) => o.textContent)).toEqual(["All projects", "Apollo", "Borealis"]);
    // The titles ARE the labels (epic-uuid-1 spans both lanes — scope).
    expect(screen.getByTestId("card-title-tsk-bbbb0002").textContent).toBe("Implement engine");
    expect(lane.querySelector('[data-testid="band-title-epic-uuid-1"]')!.textContent).toBe("E1");
  });

  it("the ids render ONLY in the click-through popups (task + epic drawers)", async () => {
    mockGetEpic.mockResolvedValue({
      id: "epic-uuid-2", project_id: PROJECT_A.id, short_id: "epc-drawer000",
      title: "E2", outcome: "", status: "active", tags: [],
      created_at: "", updated_at: "", progress_done: 0, progress_total: 1, progress_derived: false,
    });
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    // Task popup: the details drawer carries the tsk- id.
    fireEvent.click(screen.getByTestId("details-tsk-bbbb0002"));
    expect((await screen.findByTestId("drawer-short-id")).textContent).toContain("tsk-bbbb0002");
    fireEvent.click(screen.getByTestId("drawer-close"));

    // Epic popup: the drawer (row double-click) carries the epc- id.
    fireEvent.dblClick(screen.getByTestId("epic-band-epic-uuid-2"));
    expect((await screen.findByTestId("epic-drawer-short-id")).textContent).toContain("epc-");
    expect(screen.getByTestId("epic-drawer-title").textContent).toContain("E2");
  });

  // 2026-10-29 owner ruling: inside the ids popup, the session refs
  // (assignee + implementer) are CLICKABLE — straight through to the
  // session's chat view, the same /chat/:workspaceId/:sessionId target
  // as the sidebar.
  it("the task popup's session ids are clickable links to the chat view (assignee + implementer)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    // Assignee: the claimed row's worker session.
    fireEvent.click(screen.getByTestId("details-tsk-bbbb0002"));
    const assignee = (await screen.findByTestId("drawer-assignee")) as HTMLAnchorElement;
    expect(assignee.getAttribute("href")).toBe("/chat/ws-1/amber-falcon-glides");
    expect(assignee.textContent).toBe("amber-falcon-glides");
    fireEvent.click(screen.getByTestId("drawer-close"));

    // Implementer: the review-lifecycle row's ORIGINAL worker (#1959).
    fireEvent.click(screen.getByTestId("details-tsk-cccc0003"));
    const implementer = (await screen.findByTestId("drawer-implementer")) as HTMLAnchorElement;
    expect(implementer.getAttribute("href")).toBe("/chat/ws-1/brave-osprey-lands");
    expect(implementer.textContent).toBe("brave-osprey-lands");
    fireEvent.click(screen.getByTestId("drawer-close"));
  });

  it("names blocked-by dependencies by title (the id rides the tooltip/data attr)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");

    fireEvent.click(screen.getByTestId("blocked-badge-tsk-eeee0005"));
    const panel = await screen.findByTestId("blocked-by-tsk-eeee0005");
    expect(panel.textContent).toContain("Review drag arm");
    expect(panel.textContent).not.toContain("tsk-cccc0003");
    const depTitle = panel.querySelector("[data-dep-id='tsk-cccc0003'] span")!;
    expect(depTitle).not.toBeNull();
    expect(depTitle.getAttribute("title")).toBe("tsk-cccc0003");
  });

  // --- Ruling 5: wrap + button containment (class pins; layout pinned in e2e) -

  it("cards wrap their content: title min-w-0/break-words, actions shrink-0, card min-w-0", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-bbbb0002");

    const card = screen.getByTestId("task-card-tsk-bbbb0002");
    expect(card.className).toContain("min-w-0");
    const title = screen.getByTestId("card-title-tsk-bbbb0002");
    expect(title.className).toContain("min-w-0");
    expect(title.className).toContain("break-words");
    // The action cluster (details/start/kebab) may not shrink → the
    // buttons can never be squeezed out of the card's bounds.
    const kebab = screen.getByTestId("task-kebab-tsk-bbbb0002");
    expect(kebab.parentElement!.className).toContain("shrink-0");
  });

  it("collapses the main stage grid to 2 columns on small screens, 4 from md up", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const grid = screen.getByTestId("stage-grid-prj-aaaa1111");
    expect(grid.className).toContain("grid-cols-2");
    expect(grid.className).toContain("md:grid-cols-4");
  });

  // --- Ruling 2 companion: the click-highlight stays the epic→task path ------

  it("the epic row click-highlight still reaches the cards (the only epic→task navigation)", async () => {
    renderBoard();
    await screen.findByTestId("epic-band-epic-uuid-2");

    fireEvent.click(screen.getByTestId("epic-band-epic-uuid-2"));
    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-eeee0005").getAttribute("data-highlighted")).toBe("true");
    });
    expect(screen.getByTestId("task-card-tsk-aaaa0001").getAttribute("data-dimmed")).toBe("true");
  });
});
