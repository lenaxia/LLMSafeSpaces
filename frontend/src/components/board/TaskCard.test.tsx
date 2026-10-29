// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// TaskCard.test.tsx — the 2026-10-29 owner ruling's CLICK pins (review
// round 1): the card's assignee ref is a router Link — clicking it
// NAVIGATES to /chat/:workspaceId/:sessionId (the sidebar's target),
// and the click does NOT open the card's own edit dialog (the
// stopPropagation contract, previously an untested claim in the code
// comment). The cold-window and no-workspace degrades are pinned at
// the page tier (BoardPage.test.tsx); these are the behavior pins a
// rendered href cannot prove.

import { render, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useParams } from "react-router-dom";
import { describe, it, expect, vi } from "vitest";
import { TaskCard } from "./TaskCard";
import type { BoardTask } from "../../api/board";

/** Renders the routed params — proof the SPA landed on the chat view. */
function ChatRouteProbe() {
  const { workspaceId, sessionId } = useParams();
  return <div data-testid="chat-route-probe">{`${workspaceId}/${sessionId}`}</div>;
}

const TASK: BoardTask = {
  id: "task-uuid-2",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-bbbb0002",
  title: "Implement engine",
  description: "",
  stage: "in_progress",
  pending_input: false,
  paused: false,
  project_name: "Apollo",
  epic_name: "E1",
  tags: [],
  acceptance_criteria: [],
  is_blocked: false,
  claim_incremented_attempts: false,
  last_attempt_failed: false,
  failure_count: 0,
  attempts: 1,
  stage_entered_at: "2026-10-10T00:00:00Z",
  created_at: "2026-10-10T00:00:00Z",
  updated_at: "2026-10-10T00:00:00Z",
  assignee_session: "amber-falcon-glides",
};

function renderCard(props: Partial<Parameters<typeof TaskCard>[0]> = {}) {
  return render(
    <MemoryRouter initialEntries={["/board"]}>
      <Routes>
        <Route
          path="/board"
          element={<TaskCard task={TASK} projectTasks={[]} onOpen={() => {}} onDetails={() => {}} kebabItems={[]} {...props} />}
        />
        <Route path="/chat/:workspaceId/:sessionId" element={<ChatRouteProbe />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("TaskCard — session ref click-through (2026-10-29 owner ruling)", () => {
  it("clicking the assignee link navigates to the session's chat view", () => {
    renderCard({ workspaceId: "ws-1" });
    fireEvent.click(screen.getByTestId("assignee-link-tsk-bbbb0002"));
    expect(screen.getByTestId("chat-route-probe").textContent).toBe("ws-1/amber-falcon-glides");
  });

  it("the assignee link's click does NOT open the card (stopPropagation — the link is not a card click)", () => {
    const onOpen = vi.fn();
    renderCard({ workspaceId: "ws-1", onOpen });
    fireEvent.click(screen.getByTestId("assignee-link-tsk-bbbb0002"));
    // Navigation happened (the probe rendered), the editor did not.
    expect(screen.getByTestId("chat-route-probe").textContent).toBe("ws-1/amber-falcon-glides");
    expect(onOpen).not.toHaveBeenCalled();
  });

  it("navigates through the #1922 cold window too (the raw harness id is a resolvable route segment)", () => {
    renderCard({
      workspaceId: "ws-1",
      task: { ...TASK, assignee_session: "ses_ed9183a46ffeRiNw2PddpL5dAA", assignee_session_deprecated: true },
    });
    fireEvent.click(screen.getByTestId("assignee-link-tsk-bbbb0002"));
    expect(screen.getByTestId("chat-route-probe").textContent).toBe("ws-1/ses_ed9183a46ffeRiNw2PddpL5dAA");
  });
});
