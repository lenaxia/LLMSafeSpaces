// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// D6 (#998): hung-state recovery from persisted alerts. When a
// workspace's sessions first appear in the query cache (page load /
// reconnect), the provider seeds hungWorkspaces from the persisted
// alerts endpoint — an alert missed while disconnected must still
// surface the banner/badge — but ONLY for UNRESOLVED alerts
// (resolvedAt): the feed is append-only 24h history, and resolution is
// server-side truth (resolved_at is written when the hang ends — D6
// sweep observation or leave-Active watch event, both of which also
// emit workspace.alert_resolved, or the read-side heal aging a lost
// resolution). A session that hung and recovered keeps its alerts
// forever; the resolved flag — not client-side busy reconstruction —
// decides liveness.

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

  it("seeds hung state from an UNRESOLVED alert (true recovery)", async () => {
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
        resolvedAt: null,
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "busy" } as SessionListItem]);

    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();
    expect(mockGetAlerts).toHaveBeenCalledWith("ws-1");
  });

  it("does NOT seed hung from RESOLVED history (the shipped latch)", async () => {
    // The shipped bug: a session that hung and recovered kept its
    // alerts forever in the append-only feed, and the badge seeded from
    // mere existence — latching on every page load. Resolution is now
    // server-side: resolvedAt set = history, not state.
    const resolved = new Date().toISOString();
    mockGetAlerts.mockResolvedValue([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 3154,
        createdAt: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
        resolvedAt: resolved,
      },
      {
        id: "2", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 997,
        createdAt: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
        resolvedAt: resolved,
      },
    ]);
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalledWith("ws-1"));
    // Give the resolved promise a beat to (wrongly) flip state.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText("healthy")).toBeInTheDocument();
  });

  it("a late-resolving unresolved seed latches, then workspace.alert_resolved clears it", async () => {
    // Between recovery and the sweep's next tick, an alert can still be
    // unresolved while REST says idle — the seed badges it (correct:
    // the server has not resolved it), and the sweep's
    // workspace.alert_resolved event is what clears it authoritatively.
    let resolveAlerts!: (alerts: unknown[]) => void;
    mockGetAlerts.mockReturnValue(
      new Promise((resolve) => {
        resolveAlerts = resolve;
      }),
    );
    renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

    resolveAlerts([
      {
        id: "1", workspaceId: "ws-1", sessionId: "ses-x",
        alert: "session_hung", oldestBusySeconds: 960,
        createdAt: new Date().toISOString(),
        resolvedAt: null,
      },
    ]);
    expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

    capturedOnEvent?.({ type: "workspace.alert_resolved", workspace_id: "ws-1" });
    await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
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
    // Resolve on a macrotask like a real network response, matching
    // production fetch latency.
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
                  resolvedAt: null,
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

// The straddle ordering (documented bounded false-latch in the
// UnresolvedStaleAfter doc): resolveHungs publishes the SSE clear
// BEFORE the persist commits, so a seed fetch landing in that window
// re-latches the badge from still-unresolved rows. This pins the
// behavior AND its bound: reconnect's gated re-seed clears it (the
// row heals read-side for future loads).
it("straddle: a late unresolved fetch after an alert_resolved clear re-latches until reconnect", async () => {
  let resolveAlerts!: (alerts: unknown[]) => void;
  mockGetAlerts.mockReturnValue(
    new Promise((resolve) => {
      resolveAlerts = resolve;
    }),
  );
  renderProvider([{ id: "ses-x", title: "t", status: "idle" } as SessionListItem]);

  // The sweep's resolution event arrives BEFORE the queued persist
  // commits; the in-flight alerts fetch resolves after — with rows the
  // persist has not touched yet.
  capturedOnEvent?.({ type: "workspace.alert_resolved", workspace_id: "ws-1" });
  resolveAlerts([
    {
      id: "1", workspaceId: "ws-1", sessionId: "ses-x",
      alert: "session_hung", oldestBusySeconds: 960,
      createdAt: new Date().toISOString(),
      resolvedAt: null,
    },
  ]);
  expect(await screen.findByText("hung", {}, { timeout: 2000 })).toBeInTheDocument();

  // The bound: reconnect full-clears; the gated re-seed (alerts now
  // healed server-side on the next read) does not re-add.
  mockGetAlerts.mockResolvedValue([
    {
      id: "1", workspaceId: "ws-1", sessionId: "ses-x",
      alert: "session_hung", oldestBusySeconds: 960,
      createdAt: new Date().toISOString(),
      resolvedAt: new Date().toISOString(),
    },
  ]);
  capturedOnReconnect?.();
  await waitFor(() => expect(screen.getByText("healthy")).toBeInTheDocument());
});
