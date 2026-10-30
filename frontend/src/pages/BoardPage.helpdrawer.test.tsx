// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST (its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board).
import { renderBoard, seedBoard, mockListProjects } from "./boardTestUtils";

// Owner ruling 2026-10-30: "Add a help/info/learn more button which
// opens a drawer explaining to users how to use the board, and what
// the recommended workflow is. Include specific examples such as if
// writing code, doing research, writing a story, doing data
// analytics, data post-processing, etc." — the toolbar HelpCircle
// affordance (plus the empty-board first-run entry point) opens the
// "How the sprint board works" right-side drawer: five sections, six
// worked examples, infra-neutral copy (artifacts and verification
// outcomes — never tooling or vendors).

const ALL_SECTION_HEADINGS = [
  "board-help-heading-what",
  "board-help-heading-workflow",
  "board-help-heading-criteria",
  "board-help-heading-examples",
  "board-help-heading-when-not",
] as const;

const ALL_EXAMPLES = [
  "board-help-example-code",
  "board-help-example-research",
  "board-help-example-creative",
  "board-help-example-analytics",
  "board-help-example-post-processing",
  "board-help-example-batch",
] as const;

describe("BoardPage — help drawer (owner ruling 2026-10-30)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
  });

  it("renders the toolbar affordance with its accessible label", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    const button = screen.getByTestId("board-help-button");
    expect(button).toBeInTheDocument();
    // The icon-only button's accessible name carries the full intent.
    expect(button.getAttribute("aria-label")).toBe("Learn more: how the sprint board works");
    // Closed = not rendered (no portal content).
    expect(screen.queryByTestId("board-help-drawer")).toBeNull();
  });

  it("opens the drawer from the toolbar; Escape closes it", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-help-button"));
    const drawer = await screen.findByTestId("board-help-drawer");
    expect(drawer).toBeInTheDocument();
    expect(screen.getByTestId("board-help-title").textContent).toBe("How the sprint board works");

    // Escape closes (the Radix dialog contract — same keyboard exit as
    // every drawer in the app).
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("board-help-drawer")).toBeNull());
    // The board itself is untouched underneath.
    expect(screen.getByTestId("task-card-tsk-aaaa0001")).toBeInTheDocument();
  });

  it("carries all five sections and the six worked examples", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-help-button"));
    await screen.findByTestId("board-help-drawer");

    for (const heading of ALL_SECTION_HEADINGS) {
      expect(screen.getByTestId(heading)).toBeInTheDocument();
    }
    for (const example of ALL_EXAMPLES) {
      expect(screen.getByTestId(example)).toBeInTheDocument();
    }
    // The section headings say what they say (the pin is the TEXT, not
    // just the node).
    expect(screen.getByTestId("board-help-heading-what").textContent).toBe("What it is");
    expect(screen.getByTestId("board-help-heading-workflow").textContent).toBe(
      "The recommended workflow",
    );
    expect(screen.getByTestId("board-help-heading-criteria").textContent).toBe(
      "Writing good acceptance criteria",
    );
    expect(screen.getByTestId("board-help-heading-examples").textContent).toBe("Worked examples");
    expect(screen.getByTestId("board-help-heading-when-not").textContent).toBe(
      "When not to use the board",
    );
  });

  it("the empty-board first-run state carries the learn-more entry point; it opens the same drawer", async () => {
    mockListProjects.mockResolvedValue([]);
    renderBoard();
    await screen.findByTestId("board-empty");

    const entry = screen.getByTestId("board-empty-learn-more");
    expect(entry.getAttribute("aria-label")).toBe("Learn more: how the sprint board works");
    // The toolbar affordance is ALSO present on the empty board (the
    // toolbar renders regardless of project count).
    expect(screen.getByTestId("board-help-button")).toBeInTheDocument();

    fireEvent.click(entry);
    expect(await screen.findByTestId("board-help-drawer")).toBeInTheDocument();
    expect(screen.getByTestId("board-help-title").textContent).toBe("How the sprint board works");
  });

  it("the drawer closes via its Close button", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-help-button"));
    await screen.findByTestId("board-help-drawer");
    fireEvent.click(screen.getByTestId("board-help-close"));
    await waitFor(() => expect(screen.queryByTestId("board-help-drawer")).toBeNull());
  });

  it("vocabulary: the drawer copy never says 'task board' (the Sprint Board/story rename coordination pin)", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-help-button"));
    await screen.findByTestId("board-help-drawer");

    // The WHOLE drawer (title + description + body) — never "task board".
    const text = screen.getByTestId("board-help-drawer").textContent ?? "";
    expect(text).not.toMatch(/task board/i);
    // The copy speaks in stories (the rename lane's vocabulary).
    expect(text).toMatch(/stories/);
  });

  it("infra-neutral: the drawer copy names no tooling or vendors — artifacts and verification outcomes only", async () => {
    renderBoard();
    await screen.findByTestId("task-card-tsk-aaaa0001");

    fireEvent.click(screen.getByTestId("board-help-button"));
    await screen.findByTestId("board-help-drawer");

    const text = (screen.getByTestId("board-help-drawer").textContent ?? "").toLowerCase();
    // The binding rule: "the suite passes", not "GitHub Actions passes";
    // "the review pass", not a named reviewer/CI/chat vendor. The list
    // pins the obvious candidates — the rule itself is broader.
    const banned = [
      "github",
      "gitlab",
      "actions",
      "jenkins",
      "circleci",
      "jira",
      "slack",
      "notion",
      "excel",
      "google",
      "openai",
      "copilot",
    ];
    for (const vendor of banned) {
      expect(text).not.toContain(vendor);
    }
  });
});
