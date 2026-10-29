// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

  // Board config lives ON the board (owner ruling 2026-10-28 — "alongside
  // the projects and on the board itself"): defaults reachable from the
  // header, every project's config reachable from its swimlane, all in
  // one interaction.
  it("carries a Board defaults affordance opening the defaults dialog from the board view", async () => {
    const user = userEvent.setup();
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    await user.click(screen.getByTestId("board-defaults-button"));
    expect(await screen.findByTestId("board-defaults-modal")).toBeInTheDocument();
    // The persona editor is right there (both tiers share it).
    expect(screen.getByTestId("cfg-personas-user")).toBeInTheDocument();
  });

  it("?config=defaults auto-opens the defaults dialog (the /settings/board-config redirect target)", async () => {
    renderBoard("/board?config=defaults");
    expect(await screen.findByTestId("board-defaults-modal")).toBeInTheDocument();
  });

  it("carries a per-project Configure affordance on every swimlane opening the project's config", async () => {
    const user = userEvent.setup();
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    await user.click(screen.getByTestId("project-config-prj-aaaa1111"));
    expect(await screen.findByTestId("project-config-modal-prj-aaaa1111")).toBeInTheDocument();
    // Worker slots + the project-tier persona editor ride the panel.
    expect(screen.getByTestId("cfg-slots-prj-aaaa1111")).toBeInTheDocument();
    expect(screen.getByTestId("cfg-personas-prj-aaaa1111")).toBeInTheDocument();
  });

  it("renders one swimlane per project, named by title (ruling 4: no raw ids)", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.getByTestId("swimlane-prj-aaaa1111")).toBeInTheDocument();
    expect(screen.getByTestId("swimlane-prj-bbbb2222")).toBeInTheDocument();
    // The lane header is the project NAME — the prj- id never renders
    // in the chrome (2026-10-28 ruling 4); scope to the lane since the
    // focus select's options carry the same names.
    expect(screen.getByTestId("swimlane-prj-aaaa1111").textContent).toContain("Apollo");
    expect(screen.getByTestId("swimlane-prj-bbbb2222").textContent).toContain("Borealis");
    expect(screen.getByTestId("swimlane-prj-aaaa1111").textContent).not.toContain("prj-aaaa1111");
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

  it("places cards in their stage columns (title is the label — no raw id, ruling 4)", async () => {
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-aaaa0001");
    expect(card.closest('[data-stage-drop="backlog"]')).not.toBeNull();
    expect(card.textContent).toContain("Write charter");
    expect(card.textContent).not.toContain("tsk-aaaa0001");
  });

  it("filters to failed-attempt rows via the #1813 FAILED toggle", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    // The filters live in the drawer (owner ruling 2026-10-29) — open it.
    fireEvent.click(screen.getByTestId("board-filter-button"));
    const toggle = screen.getByTestId("failed-filter-toggle");
    fireEvent.click(toggle);
    // Only the failed fixture survives; the clean backlog row is gone.
    expect(screen.getByTestId("task-card-tsk-ffff0009")).toBeDefined();
    expect(screen.queryByTestId("task-card-tsk-aaaa0001")).toBeNull();
    // Toggling off restores.
    fireEvent.click(toggle);
    expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeDefined();
  });

  it("carries the #1813 failed-state overlay badge (FAILED ×n — never a stage)", async () => {
    renderBoard();
    const badge = await screen.findByTestId("failed-tsk-ffff0009");
    expect(badge.textContent).toContain("FAILED ×2");
    // The overlay is an ATTRIBUTE, not a stage: the failed row still
    // sits in its stage column (backlog) with the ordinary card shape.
    const card = screen.getByTestId("task-card-tsk-ffff0009");
    expect(card.getAttribute("data-stage")).toBe("backlog");
  });

  it("shows the assignee busy indicator only on claimed in-progress rows", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");
    const busyCard = screen.getByTestId("task-card-tsk-bbbb0002");
    expect(busyCard.querySelector("[data-busy]")).not.toBeNull();
    const idleCard = screen.getByTestId("task-card-tsk-aaaa0001");
    expect(idleCard.querySelector("[data-busy]")).toBeNull();
  });

  // 2026-10-29 owner ruling: the card's assignee area clicks straight
  // through to the session's chat view (/chat/:workspaceId/:sessionId
  // — the same target as the sidebar). The read edge already serves
  // the #1922 phrase, so the display text IS the route segment.
  it("links the card's assignee ref to the session's chat view (phrase display, /chat target)", async () => {
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-bbbb0002");
    const link = card.querySelector('[data-testid="assignee-link-tsk-bbbb0002"]') as HTMLAnchorElement | null;
    expect(link).not.toBeNull();
    expect(link!.getAttribute("href")).toBe("/chat/ws-1/amber-falcon-glides");
    expect(link!.textContent).toBe("amber-falcon-glides");
    // Unclaimed rows carry no link at all.
    expect(screen.getByTestId("task-card-tsk-aaaa0001").querySelector('[data-testid^="assignee-link-"]')).toBeNull();
  });

  it("keeps the assignee link valid through the #1922 cold window (raw id + deprecation flag — never a broken link)", async () => {
    const tasks = defaultTasks().map((t) =>
      t.short_id === "tsk-bbbb0002"
        ? { ...t, assignee_session: "ses_ed9183a46ffeRiNw2PddpL5dAA", assignee_session_deprecated: true }
        : t,
    );
    seedBoard(tasks);
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-bbbb0002");
    const link = card.querySelector('[data-testid="assignee-link-tsk-bbbb0002"]') as HTMLAnchorElement | null;
    expect(link).not.toBeNull();
    expect(link!.getAttribute("href")).toBe("/chat/ws-1/ses_ed9183a46ffeRiNw2PddpL5dAA");
    expect(link!.textContent).toContain("ses_ed9183a46ffeRiNw2PddpL5dAA");
    expect(link!.getAttribute("data-deprecated")).toBe("true");
  });

  it("carries the structural blocked badge with the dependency count (ruling R3)", async () => {
    renderBoard();
    const badge = await screen.findByTestId("blocked-badge-tsk-eeee0005");
    expect(badge.textContent).toContain("1");
  });

  it("expands the blocked-by view with live dependency stage chips (dep named by title)", async () => {
    renderBoard();
    const badge = await screen.findByTestId("blocked-badge-tsk-eeee0005");
    badge.click();
    const view = await screen.findByTestId("blocked-by-tsk-eeee0005");
    // Ruling 4: the dependency is named by its TITLE (id in tooltip).
    expect(view.textContent).toContain("Review drag arm");
    expect(view.textContent).not.toContain("tsk-cccc0003");
    expect(view.querySelector('[data-dep-stage="in_review"]')).not.toBeNull();
  });

  it("renders the OR4 anchor chip on the card", async () => {
    renderBoard();
    const card = await screen.findByTestId("task-card-tsk-eeee0005");
    expect(card.querySelector("[data-pr-anchor]")?.textContent).toBe("pr:1702");
  });

  it("collapses the Failed side column until toggled", async () => {
    renderBoard();
    await screen.findByTestId("swimlane-prj-aaaa1111");
    expect(screen.queryByTestId("stage-drop-failed")).toBeNull();
    screen.getAllByTestId("side-columns-toggle")[0]!.click();
    await waitFor(() => expect(screen.getAllByTestId("stage-drop-failed").length).toBeGreaterThan(0));
  });

  it("shows the pending badge on the REAL stage (000050: a flag, never a costume column)", async () => {
    renderBoard();
    // The flagged in_progress row wears its badge in the work column…
    const flagged = await screen.findByTestId("task-card-tsk-ffff0006");
    expect(flagged.dataset.stage).toBe("in_progress");
    expect(flagged.textContent).toContain("Worker question");
    // …and the escalated FAILED row carries the re-arm marker in the
    // side column.
    screen.getAllByTestId("side-columns-toggle")[0]!.click();
    const escalated = await screen.findByTestId("task-card-tsk-ffff0008");
    expect(escalated.textContent).toContain("Escalated");
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
