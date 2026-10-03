// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// D6 (#998): hung-state recovery from persisted alerts. When a
// workspace's sessions first appear in the query cache (page load /
// reconnect), the provider seeds hungWorkspaces from the persisted
// alerts endpoint — an alert missed while disconnected must still
// surface the banner/badge — BUT only for alerts whose session is
// STILL busy. The persisted feed is append-only 24h history: a session
// that hung and later went idle (or whose pod restarted) keeps its
// alerts forever, and the only SSE clear path (session.status idle)
// never fires for a session that was already idle when the page
// loaded. Seeding from stale history latched the badge permanently.

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

  it("seeds hung state when the alerted session is currently busy (true recovery)", async () => {
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();
    expect(mockGetAlerts).toHaveBeenCalledWith("ws-1");
  });

  it("does NOT seed hung from stale history when the alerted session is idle", async () => {
    // The shipped bug: session hung (alert persisted), pod restarted,
    // session went idle with no browser attached — on next page load
    // the idle session's stale alert latched the hung badge forever
    // (no idle SSE transition ever fires for an already-idle session).
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 3154,
        createdAt: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
      },
      {
        id: "2", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 997,
        createdAt: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalledWith("ws-1"));
    // Give the resolved promise a beat to (wrongly) flip state.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("does not seed hung when the alerted session no longer exists", async () => {
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-gone",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalledWith("ws-1"));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("healthy")).toBeInTheDocument();
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
