/**
 * Integration: real SessionActivityProvider + real Sidebar — the
 * Workers-group header spinner on the REAL wiring (no provider-module
 * mock, so the sidebar consumes the provider's actual busy map and
 * aggregation hook end-to-end). SSE-shaped events enter through the
 * captured useUserEventStream callback exactly as the provider test
 * harness does; the assertion surface is the real Sidebar tree.
 *
 * Pins the gap the mocked-factory suites cannot see: the header
 * spinner lights from a busy event for a WORKER id, stays dark for a
 * busy NON-worker id (the aggregation is worker-scoped on the real
 * path), and extinguishes on the worker's idle event.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SessionActivityProvider } from "../../providers/SessionActivityProvider";
import { AuthProvider } from "../../providers/AuthProvider";
import { Sidebar } from "../../components/layout/Sidebar";

let capturedOnEvent: ((data: unknown) => void) | undefined;

vi.mock("../../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void; onReconnect?: () => void }) => {
    capturedOnEvent = options?.onEvent;
  },
}));

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
    getSessions: vi.fn().mockResolvedValue([
      { id: "ses-user", title: "My chat", messageCount: 3, status: "idle", hasUnread: false, lastMessageAt: "2026-11-01T00:00:00Z" },
      { id: "ses-worker-a", title: "lane worker A", origin: "worker", messageCount: 5, status: "idle", hasUnread: false, lastMessageAt: "2026-11-02T00:00:00Z" },
      { id: "ses-worker-b", title: "lane worker B", origin: "worker", messageCount: 2, status: "idle", hasUnread: false, lastMessageAt: "2026-11-03T00:00:00Z" },
    ]),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "ses-user", workspaceId: "ws-1" }),
    getStatus: vi.fn().mockResolvedValue(null),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    setSessionArchived: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

function statusEvent(sessionId: string, status: string) {
  capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: sessionId, status });
}

describe("Integration: real provider + real Sidebar — Workers-group header activity spinner", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
  });

  it("stays dark for a busy NON-worker, lights on a busy worker, extinguishes on idle", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <SessionActivityProvider>
            <MemoryRouter initialEntries={["/chat/ws-1"]}>
              <Routes>
                <Route path="/chat/:workspaceId" element={<Sidebar />} />
                <Route path="/chat/:workspaceId/:sessionId" element={<Sidebar />} />
              </Routes>
            </MemoryRouter>
          </SessionActivityProvider>
        </AuthProvider>
      </QueryClientProvider>,
    );

    // The group renders collapsed with BOTH workers counted and no spinner.
    await screen.findByTestId("workers-group");
    expect(screen.getByText("2")).toBeTruthy();
    expect(screen.queryByTestId("workers-group-spinner")).toBeNull();

    // A busy USER session lands (the provider's busy map holds it —
    // the user row would badge) but the WORKERS header stays dark:
    // the real aggregation is scoped to the group's worker ids.
    act(() => statusEvent("ses-user", "busy"));
    expect(screen.queryByTestId("workers-group-spinner")).toBeNull();

    // Worker A goes busy → the header lights, group still collapsed.
    act(() => statusEvent("ses-worker-a", "busy"));
    expect(screen.getByTestId("workers-group-spinner")).toBeTruthy();

    // Worker A goes idle → the header extinguishes (worker B was
    // never busy — all idle, no spinner).
    act(() => statusEvent("ses-worker-a", "idle"));
    expect(screen.queryByTestId("workers-group-spinner")).toBeNull();
  });

  it("stays lit while ANY worker remains busy (OR on the real path)", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <SessionActivityProvider>
            <MemoryRouter initialEntries={["/chat/ws-1"]}>
              <Routes>
                <Route path="/chat/:workspaceId" element={<Sidebar />} />
                <Route path="/chat/:workspaceId/:sessionId" element={<Sidebar />} />
              </Routes>
            </MemoryRouter>
          </SessionActivityProvider>
        </AuthProvider>
      </QueryClientProvider>,
    );

    await screen.findByTestId("workers-group");
    act(() => statusEvent("ses-worker-a", "busy"));
    act(() => statusEvent("ses-worker-b", "busy"));
    expect(screen.getByTestId("workers-group-spinner")).toBeTruthy();

    // One idle does not extinguish the other's activity.
    act(() => statusEvent("ses-worker-a", "idle"));
    expect(screen.getByTestId("workers-group-spinner")).toBeTruthy();

    act(() => statusEvent("ses-worker-b", "idle"));
    expect(screen.queryByTestId("workers-group-spinner")).toBeNull();
  });
});
