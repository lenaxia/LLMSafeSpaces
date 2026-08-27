// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// D6 (#998): hung-state recovery from persisted alerts. When a
// workspace's sessions first appear in the query cache (page load /
// reconnect), the provider seeds hungWorkspaces from the persisted
// alerts endpoint — an alert missed while disconnected must still
// surface the banner/badge.

import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  SessionActivityProvider,
  useWorkspaceHung,
} from "./SessionActivityProvider";
import type { SessionListItem } from "../api/types";

const mockGetAlerts = vi.fn();

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getAlerts: (id: string) => mockGetAlerts(id),
  },
}));

// No live SSE in this test: events are the other (tested) path in.
vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: () => {},
}));

function HungProbe({ workspaceId }: { workspaceId: string }) {
  const hung = useWorkspaceHung(workspaceId);
  return <div data-testid="hung-probe">{hung ? "hung" : "healthy"}</div>;
}

function renderProvider(sessions: SessionListItem[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["sessions", "ws-1"], sessions);
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          <HungProbe workspaceId="ws-1" />
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("SessionActivityProvider — persisted-alert recovery (#998)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("seeds hung state from persisted alerts on (re)connect", async () => {
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();
    expect(mockGetAlerts).toHaveBeenCalledWith("ws-1");
  });

  it("stays healthy when no persisted alerts exist", async () => {
    mockGetAlerts.mockResolvedValue([]);
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalledWith("ws-1"));
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("holds state when the alerts fetch fails", async () => {
    mockGetAlerts.mockRejectedValue(new Error("net down"));
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalled());
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });
});
