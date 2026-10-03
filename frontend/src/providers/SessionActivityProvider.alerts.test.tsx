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
    getSessions: (id: string) => Promise.resolve(sessionsFixtures[id] ?? []),
  },
}));

let sessionsFixtures: Record<string, SessionListItem[]> = {};

// SSE handlers are captured so the race/reconnect paths (the two
// residual permanent-latch windows) can be driven deterministically.
let capturedOnEvent: ((data: unknown) => void) | undefined;
let capturedOnReconnect: (() => void) | undefined;
vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void; onReconnect?: () => void }) => {
    capturedOnEvent = options?.onEvent;
    capturedOnReconnect = options?.onReconnect;
  },
}));

function HungProbe({ workspaceId }: { workspaceId: string }) {
  const hung = useWorkspaceHung(workspaceId);
  return <div data-testid="hung-probe">{hung ? "hung" : "healthy"}</div>;
}

function renderProvider(sessions: SessionListItem[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["sessions", "ws-1"], sessions);
  const result = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          <HungProbe workspaceId="ws-1" />
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { qc, ...result };
}

describe("SessionActivityProvider — persisted-alert recovery (#998)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    sessionsFixtures = {};
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  const fireHungAlertSse = () =>
    capturedOnEvent?.({
      type: "workspace.alert",
      workspace_id: "ws-1",
      data: { alert: "session_hung" },
    });
  const fireIdleSse = (sessionId: string) =>
    capturedOnEvent?.({ type: "session.status", status: "idle", workspace_id: "ws-1", session_id: sessionId });

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

  it("a late-resolving alerts fetch does not re-latch the badge after an idle SSE clear", async () => {
    // Review round-1 finding: seed runs while REST says busy, the
    // fetch is in flight, an idle SSE clears the (live-latched) badge,
    // and the fetch then resolves — against the seed-time snapshot it
    // would re-latch permanently (no future idle fires). The gate
    // must re-validate against live busy state.
    let resolveAlerts!: (alerts: unknown[]) => void;
    mockGetAlerts.mockReturnValue(
      new Promise((resolve) => {
        resolveAlerts = resolve;
      }),
    );
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    // Badge latched live while the fetch is in flight...
    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();
    // ...the session recovers (idle SSE clears badge AND busy state)...
    fireIdleSse("ses-x");
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
    // ...and THEN the alerts response lands with the stale history.
    resolveAlerts([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
      },
    ]);
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("reconnect drops a live-latched badge for a session that recovered while disconnected", async () => {
    // Review round-1 finding: onReconnect cleared busy seeding but
    // not hungWorkspaces — a session that recovered while the tab was
    // offline kept the badge (the gated re-seed correctly declines to
    // re-add it, but nothing cleared the stale entry).
    mockGetAlerts.mockResolvedValue([]);
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnReconnect?.();
    // The gated re-seed re-adds the badge only when the session is
    // still busy — here REST says idle, so it stays cleared.
    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("reconnect re-seeds the badge when the session is still genuinely busy", async () => {
    // Resolve on a macrotask like a real network response: the busy
    // re-seed (triggered by the same seedBusy pass) must have committed
    // to busySessionsRef before the gate reads it — the documented
    // ordering the ref-sync comment relies on.
    mockGetAlerts.mockImplementation(
      () =>
        new Promise((resolve) =>
          setTimeout(
            () =>
              resolve([
                {
                  id: "1", workspaceId: "ws-1", sessionId: "ses-x",
                  alert: "session_hung", oldestBusySeconds: 960,
                  createdAt: new Date().toISOString(),
                },
              ]),
            50,
          ),
        ),
    );
    const { qc } = renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnReconnect?.();
    // The clear must commit before the re-seed could run.
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());

    // In production the re-seed fires when the sessions cache updates
    // after reconnect (seededRef was cleared) — drive exactly that.
    qc.setQueryData(["sessions", "ws-1"], [{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();
  });

  it.each(["aborted", "deleted"] as const)(
    "session.status=%s clears a live-latched badge (terminal events end the hang)",
    async (status) => {
      mockGetAlerts.mockResolvedValue([]);
      renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

      fireHungAlertSse();
      expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

      capturedOnEvent?.({ type: "session.status", status, workspace_id: "ws-1", session_id: "ses-x" });
      await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
    },
  );

  it("agent_died clears a live-latched badge", async () => {
    mockGetAlerts.mockResolvedValue([]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnEvent?.({ type: "agent_died", workspace_id: "ws-1" });
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
  });

  it("workspace.phase non-active clears a live-latched badge", async () => {
    mockGetAlerts.mockResolvedValue([]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnEvent?.({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
  });

  it("resync clears hung state (dropped events include the idle clear)", async () => {
    mockGetAlerts.mockResolvedValue([]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    fireHungAlertSse();
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnEvent?.({ type: "resync" });
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
  });
});
