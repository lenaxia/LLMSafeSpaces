// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import {
  renderBoard,
  seedBoard,
} from "./boardTestUtils";

// Owner ruling 2026-10-29 (verbatim): "I think those pills need to be
// in a collapsed drawer. They eat up a lot of screen real estate esp
// on mobile." — the filter pills (the #1813 FAILED overlay, the tag
// facets) live inside a collapsible drawer opened by the toolbar's
// funnel button; active filters stay VISIBLE via the button's count
// badge; a Clear-all affordance lives INSIDE the drawer.

describe("BoardPage — filter drawer (owner ruling 2026-10-29)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("opens and closes via the funnel button (toggled aria-expanded, drawer in/out of the DOM)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const button = screen.getByTestId("board-filter-button");
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByTestId("board-filter-drawer")).toBeNull();

    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByTestId("board-filter-drawer")).toBeInTheDocument();
    expect(screen.getByTestId("failed-filter-toggle")).toBeInTheDocument();
    expect(screen.getByTestId("tag-filter-pr:1702")).toBeInTheDocument();

    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("false");
    // Closed = NOT RENDERED (zero height — no overlap with the swim
    // lanes; the board content starts immediately below the toolbar).
    expect(screen.queryByTestId("board-filter-drawer")).toBeNull();
  });

  it("Escape collapses the drawer; applied filters SURVIVE the collapse", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-filter-button"));
    fireEvent.click(screen.getByTestId("failed-filter-toggle"));
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());

    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByTestId("board-filter-drawer")).toBeNull();
    // Collapsing the drawer is a chrome action, not a filter reset:
    // the FAILED overlay still filters (and the badge still shows it).
    expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull();
    expect(screen.getByTestId("board-filter-count").textContent).toBe("1");
  });

  it("no count badge at zero active filters; 1 for FAILED; 2 for FAILED + tag", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // Zero: no badge at all.
    expect(screen.queryByTestId("board-filter-count")).toBeNull();

    fireEvent.click(screen.getByTestId("board-filter-button"));
    fireEvent.click(screen.getByTestId("failed-filter-toggle"));
    await waitFor(() => expect(screen.getByTestId("board-filter-count").textContent).toBe("1"));

    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    await waitFor(() => expect(screen.getByTestId("board-filter-count").textContent).toBe("2"));

    // Untoggle the tag facet: back to 1 (the badge tracks the LIVE
    // count, not a session history).
    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    await waitFor(() => expect(screen.getByTestId("board-filter-count").textContent).toBe("1"));
  });

  it("Clear all (inside the drawer) resets every filter and the board un-filters", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-filter-button"));
    fireEvent.click(screen.getByTestId("failed-filter-toggle"));
    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    // FAILED + tag: with no failed row carrying pr:1702 the board is
    // empty under both filters.
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-ffff0009")).toBeNull());
    expect(screen.getByTestId("board-filter-count").textContent).toBe("2");

    fireEvent.click(screen.getByTestId("board-filter-clear"));
    // Every row returns; the badge drops with the filters.
    await waitFor(() => expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeInTheDocument());
    expect(screen.getByTestId("task-card-tsk-ffff0009")).toBeInTheDocument();
    expect(screen.queryByTestId("board-filter-count")).toBeNull();
    // The selections themselves reset (not just the rows): reopening —
    // or reading the live controls — shows FAILED off, tag unselected.
    expect(screen.getByTestId("failed-filter-toggle").className).not.toContain("bg-destructive");
  });

  it("Clear all is disabled at zero active filters", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-filter-button"));
    expect(screen.getByTestId("board-filter-clear")).toBeDisabled();
  });

  it("filters stay functional inside the drawer (identical filtering to the old inline pills)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-eeee0005");

    // Tag facet: only the tagged row survives.
    fireEvent.click(screen.getByTestId("board-filter-button"));
    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-eeee0005")).toBeInTheDocument();

    // FAILED overlay: only the last-attempt-failed row survives.
    fireEvent.click(screen.getByTestId("board-filter-clear"));
    fireEvent.click(screen.getByTestId("failed-filter-toggle"));
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());
    expect(screen.getByTestId("task-card-tsk-ffff0009")).toBeInTheDocument();
  });

  it("the drawer reopens with the current selections rendered", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    // Apply the tag facet, then collapse the drawer.
    fireEvent.click(screen.getByTestId("board-filter-button"));
    fireEvent.click(screen.getByTestId("tag-filter-pr:1702"));
    await waitFor(() => expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull());
    fireEvent.click(screen.getByTestId("board-filter-button"));

    // Reopen: the drawer reveals the CURRENT selection (the tag facet
    // still in its selected state) plus the live count badge.
    fireEvent.click(screen.getByTestId("board-filter-button"));
    expect(screen.getByTestId("tag-filter-pr:1702").className).toContain("bg-primary");
    expect(screen.getByTestId("board-filter-count").textContent).toBe("1");
  });
});
