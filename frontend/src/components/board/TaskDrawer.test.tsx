// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// TaskDrawer.test.tsx — #1832 item 4 red-first pins: the task-detail
// drawer renders the header (charter summary: stage, claim/lease,
// blocked-by, attempts), the charter + acceptance criteria with met
// state, and the FULL audit timeline from the events read (stage
// transitions, claims who/when, review verdicts when present,
// dependency-graph changes, orphan/unblock) — plus empty + error
// states. RED until the component exists.

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { TaskDrawer } from "./TaskDrawer";
import type { BoardTask, BoardAuditEvent } from "../../api/board";
import { ApiClientError } from "../../api/client";
import type { ApiError } from "../../api/types";

vi.mock("../../api/board", () => ({
  boardApi: {
    taskEvents: (id: string, limit?: number) => mockTaskEvents(id, limit),
  },
}));

const mockTaskEvents = vi.fn();

const baseTask = {
  description: "",
  paused: false,
  project_name: "Apollo",
  epic_name: "E1",
  tags: [] as string[],
  acceptance_criteria: [] as Array<{ text: string; met: boolean }>,
  is_blocked: false,
  claim_incremented_attempts: false,
  last_attempt_failed: false,
  failure_count: 0,
  stage_entered_at: "2026-10-10T00:00:00Z",
  created_at: "2026-10-10T00:00:00Z",
  updated_at: "2026-10-10T00:00:00Z",
  attempts: 0,
};

const TASK: BoardTask = {
  ...baseTask,
  id: "task-uuid-2",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-bbbb0002",
  title: "Implement engine",
  description: "## Charter\nShip the engine slice. Worktree wt-x, branch feat/x.",
  stage: "in_progress",
  assignee_session: "ws-1/sess-9",
  lease_expires_at: "2026-10-22T12:34:56Z",
  attempts: 2,
  acceptance_criteria: [
    { text: "tests green", met: true },
    { text: "docs updated", met: false },
  ],
  is_blocked: true,
  blocked_by: ["tsk-cccc0003"],
};

const DEP: BoardTask = {
  ...baseTask,
  id: "task-uuid-3",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-cccc0003",
  title: "Dependency task",
  stage: "in_review",
};

function evt(partial: Partial<BoardAuditEvent> & { id: string; action: string }): BoardAuditEvent {
  return {
    project_id: "proj-uuid-a",
    item_kind: "task",
    item_id: "task-uuid-2",
    created_at: "2026-10-22T10:00:00Z",
    ...partial,
  } as BoardAuditEvent;
}

/** The four #1832 item-4 timeline classes over one synthetic history. */
function fullTimeline(): BoardAuditEvent[] {
  return [
    evt({ id: "e1", action: "create_task", created_at: "2026-10-22T09:00:00Z", payload: { task_id: "task-uuid-2" } }),
    evt({
      id: "e2", action: "claim", created_at: "2026-10-22T10:00:00Z",
      actor: { kind: "system", surface: "engine" },
      payload: { task_id: "task-uuid-2", worker: "sess-9", attempts: 1 },
    }),
    evt({
      id: "e3", action: "move_stage", created_at: "2026-10-22T11:00:00Z",
      actor: { kind: "user", user_id: "u1", surface: "rest" },
      payload: { task_id: "task-uuid-2", from_stage: "backlog", requested_to: "in_progress", resulting_stage: "in_progress", reclaimed: false },
    }),
    evt({
      id: "e4", action: "add_dependency", created_at: "2026-10-22T11:30:00Z",
      payload: { task_id: "task-uuid-2", depends_on: "tsk-cccc0003" },
    }),
    evt({
      id: "e5", action: "move_stage", created_at: "2026-10-22T12:00:00Z",
      actor: { kind: "user", surface: "rest" },
      payload: {
        task_id: "task-uuid-2", from_stage: "in_review", requested_to: "in_progress", resulting_stage: "in_progress", reclaimed: false,
        // PR-B's structured verdict rides the review-bounce move (when present).
        verdict: {
          verdict: "fail",
          reviewer: "reviewer-session-1",
          timestamp: "2026-10-22T12:00:00Z",
          findings: [
            { severity: "major", pointer: "src/x.go:41", criterion_index: 0, note: "red tests not green" },
            { severity: "minor", pointer: "docs/y.md", criterion_index: 1, note: "worklog missing" },
          ],
        },
      },
    }),
    evt({
      id: "e6", action: "unblocked_by_delete", created_at: "2026-10-22T13:00:00Z",
      payload: { of: "tsk-dddd0004", orphan: "tsk-bbbb0002" },
    }),
  ];
}

function renderDrawer(props: Partial<Parameters<typeof TaskDrawer>[0]> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <TaskDrawer open onOpenChange={() => {}} task={TASK} projectTasks={[TASK, DEP]} {...props} />
    </QueryClientProvider>,
  );
}

describe("TaskDrawer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockTaskEvents.mockResolvedValue(fullTimeline());
  });

  // --- Header: charter summary fields -------------------------------------

  it("renders the header fields: title, short id, stage, attempts, claim/lease, blocked-by with dep stages", async () => {
    renderDrawer();
    expect(screen.getByTestId("drawer-title").textContent).toContain("Implement engine");
    expect(screen.getByTestId("drawer-short-id").textContent).toBe("tsk-bbbb0002");
    expect(screen.getByTestId("drawer-stage").textContent).toMatch(/In progress/i);
    expect(screen.getByTestId("drawer-attempts").textContent).toContain("2");
    // Claim/lease state: who holds it and until when.
    expect(screen.getByTestId("drawer-assignee").textContent).toContain("sess-9");
    expect(screen.getByTestId("drawer-lease").textContent).toContain("12:34");
    // Blocked-by carries the dependency WITH its live stage.
    const dep = screen.getByTestId("drawer-blocked-by");
    expect(dep.textContent).toContain("tsk-cccc0003");
    expect(dep.querySelector('[data-dep-stage="in_review"]')).toBeTruthy();
  });

  it("renders claim-free state without an assignee block (unclaimed rows)", () => {
    renderDrawer({ task: { ...TASK, assignee_session: undefined, lease_expires_at: undefined, stage: "backlog", attempts: 0, is_blocked: false, blocked_by: undefined } });
    expect(screen.queryByTestId("drawer-assignee")).toBeNull();
    expect(screen.getByTestId("drawer-lease").textContent).toMatch(/unclaimed|—/i);
  });

  // --- Charter + criteria ---------------------------------------------------

  it("renders the charter (description) and acceptance criteria with met state", () => {
    renderDrawer();
    expect(screen.getByTestId("drawer-charter").textContent).toContain("Ship the engine slice");
    const met = screen.getByTestId("drawer-criterion-0");
    const unmet = screen.getByTestId("drawer-criterion-1");
    expect(met.getAttribute("data-met")).toBe("true");
    expect(met.textContent).toContain("tests green");
    expect(unmet.getAttribute("data-met")).toBe("false");
  });

  it("renders explicit empty states for a charter-less task", () => {
    renderDrawer({ task: { ...TASK, description: "", acceptance_criteria: [] } });
    expect(screen.getByTestId("drawer-charter").textContent).toMatch(/no charter/i);
    expect(screen.getByTestId("drawer-criteria").textContent).toMatch(/none/i);
  });

  // --- The full audit timeline ----------------------------------------------

  it("renders the timeline chronologically with every #1832 item-4 event class", async () => {
    renderDrawer();
    await screen.findByTestId("timeline-evt-e1");
    const ids = ["e1", "e2", "e3", "e4", "e5", "e6"];
    const rows = ids.map((id) => screen.getByTestId(`timeline-evt-${id}`));
    // Document order is chronological (oldest first).
    for (let i = 1; i < rows.length; i += 1) {
      const prev = rows[i - 1]!;
      expect(prev.compareDocumentPosition(rows[i]!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
    // Stage transition rendered from the payload.
    expect(rows[2]!.textContent).toContain("backlog");
    expect(rows[2]!.textContent).toContain("in_progress");
    // Claim row: who + when (worker identity from the payload).
    expect(rows[1]!.textContent).toContain("sess-9");
    expect(rows[1]!.textContent).toContain("10:00");
    // Dependency-graph change.
    expect(rows[3]!.textContent).toContain("tsk-cccc0003");
    // Orphan/unblock event.
    expect(rows[5]!.textContent).toContain("tsk-dddd0004");
  });

  it("renders a structured review verdict payload when present", async () => {
    renderDrawer();
    const verdictRow = await screen.findByTestId("timeline-evt-e5");
    expect(verdictRow.textContent).toContain("fail");
    expect(verdictRow.textContent).toContain("reviewer-session-1");
    const findings = verdictRow.querySelectorAll("[data-finding]");
    expect(findings).toHaveLength(2);
    expect(findings[0]!.getAttribute("data-severity")).toBe("major");
    expect(findings[0]!.textContent).toContain("src/x.go:41");
    expect(findings[0]!.getAttribute("data-criterion-index")).toBe("0");
  });

  it("renders the empty state when the timeline read returns no events", async () => {
    mockTaskEvents.mockResolvedValue([]);
    renderDrawer();
    expect(await screen.findByTestId("timeline-empty")).toBeTruthy();
  });

  it("renders the error state with retry on a failed timeline read", async () => {
    mockTaskEvents.mockRejectedValue(new ApiClientError(500, { message: "boom" } as ApiError));
    renderDrawer();
    expect(await screen.findByTestId("timeline-error")).toBeTruthy();
    mockTaskEvents.mockResolvedValue(fullTimeline());
    fireEvent.click(screen.getByTestId("timeline-retry"));
    await screen.findByTestId("timeline-evt-e1");
    await waitFor(() => expect(mockTaskEvents).toHaveBeenCalledTimes(2));
  });

  it("fetches the task's events on open (short id key)", async () => {
    renderDrawer();
    await screen.findByTestId("timeline-evt-e1");
    expect(mockTaskEvents).toHaveBeenCalledWith("tsk-bbbb0002", expect.anything());
  });

  it("offers the edit affordance through onEdit", () => {
    const onEdit = vi.fn();
    renderDrawer({ onEdit });
    fireEvent.click(screen.getByTestId("drawer-edit"));
    expect(onEdit).toHaveBeenCalledWith(TASK);
  });

  it("renders nothing when no task is set", () => {
    const qc = new QueryClient();
    const { container } = render(
      <QueryClientProvider client={qc}>
        <TaskDrawer open onOpenChange={() => {}} task={null} />
      </QueryClientProvider>,
    );
    expect(container.textContent).toBe("");
  });
});
