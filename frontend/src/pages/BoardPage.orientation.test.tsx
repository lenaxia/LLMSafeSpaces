// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor, act } from "@testing-library/react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { renderBoard, seedBoard, PROJECT_A } from "./boardTestUtils";
import { MAIN_STAGES, type BoardStage } from "../lib/boardRules";

// Owner report #3 (2026-11-17, second occurrence — the symptom survived
// the .16 mdPlus guard): "rotating the phone landscape then back to
// portrait leaves the sprint board columns at the wrong sizes — the
// portrait 95/3+5-peek layout does not re-apply after orientation
// change." The .16 reset was EVENT-DELIVERED: a rotation whose
// matchMedia change event never reaches the page (the frozen/hidden
// window, coalesced renderer delivery) leaves mdPlus stale, and the
// portrait-only focus widths render at md+ AND back — the record's own
// admitted residual ("a genuinely UNDELIVERED event still leaves
// mdPlus stale; no poll").
//
// These pins hold the two walls that make the layout re-apply
// STRUCTURALLY, with the undelivered-event world simulated exactly:
// a controllable matchMedia whose `matches` flips WITHOUT any change
// event — the honest shape of the lost event (no synthetic event
// delivery, no state poking).
//
//  1. CSS AUTHORITY — every focus width carries its stage's md:
//     overview variant, so the media query alone renders the md row
//     even with stale state (STAGE_COL_FOCUS_MD_WIDTH).
//  2. THE ROTATION CLOCKS — window resize + orientationchange re-read
//     the media state; a real rotation always fires resize, so the
//     lost event heals on the next resize frame.
//  3. POSITION OWNERSHIP — crossing back into portrait re-derives the
//     rail's snap target from live geometry (scroll restoration
//     replays stale content offsets against changed column widths).

const RAIL_ID = `stage-grid-${PROJECT_A.short_id}`;

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

/** The md: utility tokens a column's class list carries (the
 * viewport-class half of its width contract). */
function mdTokens(stage: BoardStage) {
  return (column(stage).className.match(/md:[^\s]+/g) ?? []).sort();
}

/** The overview row's md: half, read BEFORE any lens — the reference
 * every focus state must mirror (CSS authority: one md row, reached
 * through every component state). */
async function overviewMdTokens() {
  const tokens: Partial<Record<BoardStage, string[]>> = {};
  for (const stage of MAIN_STAGES) tokens[stage] = mdTokens(stage);
  return tokens;
}

/** A controllable matchMedia: `matches` flips WITHOUT dispatching any
 * change event (the lost-event world), and the listener registry is
 * real enough that DELIBERATE dispatch is still possible. */
function installRotatableMedia() {
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
  return {
    /** Rotate the viewport class with the change event LOST. */
    silentlyCrossTo: (md: boolean) => {
      mdMatches = md;
    },
    /** Rotate with normal event delivery (the healthy path). */
    crossTo: (md: boolean) => {
      mdMatches = md;
      act(() => {
        for (const fn of changeListeners) fn({ matches: md });
      });
    },
  };
}

describe("BoardPage — orientation re-apply (owner report #3: the portrait layout must re-apply on every viewport-class change, state or no state)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  // --- (1) CSS authority: the md row renders regardless of component state ---

  it("an undelivered viewport-class crossing still renders the md row: a lens left holding (lost change event) cannot keep portrait focus widths at md+", async () => {
    const media = installRotatableMedia();
    renderBoard();
    await boardReady();
    const overviewMd = await overviewMdTokens();

    fireEvent.click(chevron("in_progress"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    // The rotation whose change event never arrives: matches says md+,
    // mdPlus still says portrait — the exact owner-report residual.
    media.silentlyCrossTo(true);

    // CSS AUTHORITY: every column — focused, compressed, periphery —
    // carries its overview md: width, so the md row (24/28/24/24 by
    // stage) is what renders at md+ even from this stale state.
    for (const stage of MAIN_STAGES) {
      expect(mdTokens(stage), `${stage} keeps its md: overview width under a stale lens`).toEqual(overviewMd[stage]);
    }
    // And the portrait focus fractions stay class-present only below md:
    expect(column("in_progress").className).toContain("w-[calc(70%-0.375rem)]");
    expect(column("backlog").className).toContain("w-[calc(10%-0.375rem)]");
  });

  it("the md: halves never drift: focus widths mirror STAGE_COL_WIDTH's md row per stage (one md row, every state)", async () => {
    installRotatableMedia();
    renderBoard();
    await boardReady();
    const overviewMd = await overviewMdTokens();

    // Lens on every column in turn (each takes its turn as focused;
    // the others compress / render as periphery).
    for (const focusOn of MAIN_STAGES) {
      fireEvent.click(chevron(focusOn));
      await waitFor(() => expect(colMode(focusOn)).toBe("focused"));
      for (const stage of MAIN_STAGES) {
        expect(mdTokens(stage), `${stage} while ${focusOn} focused`).toEqual(overviewMd[stage]);
      }
      fireEvent.keyDown(column(focusOn), { key: "Escape" });
      await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
    }
  });

  // --- (2) the rotation clocks: a lost matchMedia event heals ---

  it("resize heals a lost viewport-class crossing: the lens exits and the mode returns to overview", async () => {
    const media = installRotatableMedia();
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    media.silentlyCrossTo(true);
    // Rotation always fires resize on both engines — the heal clock.
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });

    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
    for (const stage of MAIN_STAGES) expect(colMode(stage)).toBe("overview");
  });

  it("orientationchange heals a lost viewport-class crossing (the legacy iOS clock)", async () => {
    const media = installRotatableMedia();
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("in_review"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    media.silentlyCrossTo(true);
    act(() => {
      window.dispatchEvent(new Event("orientationchange"));
    });

    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
  });

  it("the clocks are idempotent: a resize that changes nothing re-renders nothing (no spurious exit)", async () => {
    const media = installRotatableMedia();
    renderBoard();
    await boardReady();

    fireEvent.click(chevron("backlog"));
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("focus"));

    // Portrait resize (keyboard show/hide, address-bar collapse): the
    // class did not cross — the lens must HOLD.
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
    expect(rail().getAttribute("data-mode")).toBe("focus");
    expect(colMode("backlog")).toBe("focused");

    // The delivered crossing still exits (the healthy path unregressed).
    media.crossTo(true);
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));
  });

  // --- (3) position ownership: the portrait crossing re-owns the snap ---

  it("crossing back into portrait re-snaps the rail to the nearest column start (scroll restoration replays stale content offsets)", async () => {
    const media = installRotatableMedia();
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await boardReady();

    // Mount at md+, rotate to portrait with the offset restore landing
    // mid-content: the rail's scrollLeft sits past every column start
    // (the browser restored the landscape-era content offset).
    media.silentlyCrossTo(true);
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));

    const railEl = rail();
    Object.defineProperty(railEl, "scrollLeft", { value: 230, configurable: true });
    Object.defineProperty(railEl, "offsetLeft", { value: 0, configurable: true });
    const starts: Partial<Record<BoardStage, number>> = { backlog: 8, in_progress: 111, in_review: 238, done: 344 };
    for (const stage of MAIN_STAGES) {
      Object.defineProperty(column(stage), "offsetLeft", { value: starts[stage], configurable: true });
    }

    scrollSpy.mockClear();
    media.silentlyCrossTo(false);
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });

    // After the width transition settles (FOCUS_RESNAP_DELAY_MS), the
    // rail re-derives its snap target: scrollLeft 230 is 108px short
    // of done (344) and 8 past in_review (238) — nearest is in_review.
    // (scrollIntoView's receiver is the target; the args are options.)
    await waitFor(() => {
      const receivers = (scrollSpy.mock.contexts ?? []) as Element[];
      expect(receivers.at(-1)?.getAttribute("data-stage-col")).toBe("in_review");
    });
  });

  it("an on-column restore is left alone: crossing with the snapport already on a column start issues no corrective scroll", async () => {
    const media = installRotatableMedia();
    const scrollSpy = vi.spyOn(Element.prototype, "scrollIntoView").mockImplementation(() => {});
    renderBoard();
    await boardReady();

    media.silentlyCrossTo(true);
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
    await waitFor(() => expect(rail().getAttribute("data-mode")).toBe("overview"));

    const railEl = rail();
    Object.defineProperty(railEl, "scrollLeft", { value: 111, configurable: true });
    Object.defineProperty(railEl, "offsetLeft", { value: 0, configurable: true });
    const starts: Partial<Record<BoardStage, number>> = { backlog: 8, in_progress: 111, in_review: 238, done: 344 };
    for (const stage of MAIN_STAGES) {
      Object.defineProperty(column(stage), "offsetLeft", { value: starts[stage], configurable: true });
    }

    scrollSpy.mockClear();
    media.silentlyCrossTo(false);
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
    // jsdom computes no padding, so the snapport origin IS scrollLeft
    // 111 == in_progress's start: the settle window closes with no
    // corrective scroll (already legal — leave it be).
    await new Promise((r) => setTimeout(r, 350));
    expect(scrollSpy).not.toHaveBeenCalled();
  });
});
