// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1646 layer 3: the sidebar sessions query gets a 60s refetchInterval
// floor (+ explicit refetchOnWindowFocus) so the session LIST and its
// unread reconcile keep converging even with zero SSE events and zero
// window focus. Against the pre-fix code this pin is RED: the query
// fetches once on mount and never again.

import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";

vi.mock("../../api/auth", () => ({
  authApi: {
    me: vi.fn().mockResolvedValue({ id: "u1", username: "alice", email: "a@b.com", role: "user", active: true }),
  },
}));

vi.mock("../../api/orgs", () => ({
  orgsApi: {
    list: vi.fn().mockResolvedValue([]),
  },
}));

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    list: vi.fn().mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    }),
    create: vi.fn().mockResolvedValue({ id: "ws-new", name: "new-ws" }),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-1", workspaceId: "ws-1" }),
    getSessions: vi.fn().mockResolvedValue([
      { id: "sess-1", title: "t", messageCount: 0, status: "idle", hasUnread: false },
    ]),
    getStatus: vi.fn().mockResolvedValue(null),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    setSessionArchived: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";

describe("Sidebar — sessions query refetch floor (#1646)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("refetches the sessions query on the 60s interval floor with no events and no focus", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
            <Sidebar />
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>,
    );

    // Initial mount: workspace list renders, the expanded workspace's
    // sessions query fetches once.
    await vi.advanceTimersByTimeAsync(1_000);
    const initial = (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mock.calls.filter(
      (c: unknown[]) => c[0] === "ws-1",
    ).length;
    expect(initial).toBeGreaterThanOrEqual(1);

    // 61s of nothing: no SSE events, no window focus. The floor must
    // have refetched the mounted query at least once more.
    await vi.advanceTimersByTimeAsync(61_000);
    const after = (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mock.calls.filter(
      (c: unknown[]) => c[0] === "ws-1",
    ).length;
    expect(after).toBeGreaterThanOrEqual(initial + 1);
  });
});
