import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { ApiClientError } from "../api/client";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-new", workspaceId: "ws-1", workspacePhase: "Active", resumed: false }),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
vi.mock("../providers/SessionActivityProvider", () => ({
  useIsSessionDeleted: () => false,
  useClearPendingUnread: () => () => {},
  useClearSessionBusy: () => () => {},
  useIsSessionBusy: () => false,
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => 0,
  useWorkspaceHung: () => false,
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useDropPendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
  useWhileAwayStalenessSweep: () => {},
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }) } };
});
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

import { workspacesApi } from "../api/workspaces";

type Mock = ReturnType<typeof vi.fn>;

function makeItem(over: Record<string, unknown> = {}) {
  return {
    id: "ws-1",
    name: "Parked Workspace",
    userId: "u1",
    runtime: "base",
    storageSize: "5Gi",
    createdAt: "",
    updatedAt: "",
    phase: "Creating",
    ...over,
  };
}

function renderChat(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <TooltipProvider delayDuration={0}>
          <Routes>
            <Route path="/chat/:workspaceId" element={<ChatPage />} />
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  (workspacesApi.list as Mock).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
});

describe("ChatPage — parked-unbound first-session flow (D1/D2/D3/D4)", () => {
  it("D1: parked-unbound Creating renders the harness panel + CTA, not the few-seconds spinner copy; the CTA opens the picker and submits the bind", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "waiting for harness selection" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    const panel = await screen.findByTestId("harness-first-session-panel");
    expect(panel).toHaveTextContent(/waiting for harness selection/i);
    // The false estimate is gone: this workspace never becomes Active
    // without a bind.
    expect(screen.queryByText(/usually takes a few seconds/i)).not.toBeInTheDocument();

    // The panel-level entrance — ChatPage's main area is the only
    // mobile-visible surface (the sidebar + button is drawer-hidden).
    const cta = screen.getByTestId("first-session-cta");
    await user.click(cta);

    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /pi/i }));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "pi" });
    });
  });

  it("D1: single enabled harness — the CTA binds silently, no dialog", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "waiting for harness selection" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harnessOptions: ["opencode"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    await user.click(await screen.findByTestId("first-session-cta"));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "opencode" });
    });
  });

  it("D3: bind failure keeps the dialog open and renders the server's error class inline — no generic alert", async () => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "waiting for harness selection" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.ensureSession as Mock).mockRejectedValue(
      new ApiClientError(500, {
        error:
          "internal_error: harness_bind_not_persisted (the harness bind for workspace ws-1 was accepted but did not persist — the cluster's Workspace CRD predates 0.44.15; the operator must `kubectl apply -f helm/crds/workspace.yaml`)",
      }),
    );

    renderChat("/chat/ws-1");

    await user.click(await screen.findByTestId("first-session-cta"));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalled();
    });
    // The dialog survives the failure with the class + operator
    // remediation inline — the choice is preserved for retry.
    await waitFor(() => {
      expect(screen.getByRole("dialog")).toBeInTheDocument();
      expect(screen.getByText(/harness_bind_not_persisted/)).toBeInTheDocument();
      expect(screen.getByText(/kubectl apply -f helm\/crds\/workspace\.yaml/)).toBeInTheDocument();
    });
    expect(alertSpy).not.toHaveBeenCalled();
    alertSpy.mockRestore();
  });

  it("D3: the three bind-failure classes render distinguishable text in the still-open dialog", async () => {
    const cases: Array<[number, string, RegExp]> = [
      [500, "internal_error: harness_bind_not_persisted (spec.harness reads back empty)", /harness_bind_not_persisted/],
      [409, "harness_immutable: harness is immutable once bound for this workspace", /harness_immutable/],
      [422, "validation_error: harness \"pi\" is not enabled on this instance", /not enabled on this instance/],
    ];
    for (const [status, error, pattern] of cases) {
      const user = userEvent.setup();
      (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "waiting for harness selection" });
      (workspacesApi.list as Mock).mockResolvedValue({
        items: [makeItem({ harnessOptions: ["opencode", "pi"] })],
        pagination: { limit: 20, offset: 0, total: 1 },
      });
      (workspacesApi.ensureSession as Mock).mockRejectedValue(new ApiClientError(status, { error }));

      const { unmount } = renderChat("/chat/ws-1");
      await user.click(await screen.findByTestId("first-session-cta"));
      await user.click(screen.getByTestId("harness-picker-submit"));
      await waitFor(() => {
        expect(screen.getByText(pattern)).toBeInTheDocument();
      });
      expect(screen.getByRole("dialog")).toBeInTheDocument();
      unmount();
      vi.clearAllMocks();
      (workspacesApi.list as Mock).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    }
  });

  it("D3: dialog-less silent-bind failure alerts the honest class text, not the generic string", async () => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "waiting for harness selection" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harnessOptions: ["opencode"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.ensureSession as Mock).mockRejectedValue(
      new ApiClientError(409, { error: "harness_immutable: harness is immutable once bound for this workspace" }),
    );

    renderChat("/chat/ws-1");

    await user.click(await screen.findByTestId("first-session-cta"));

    await waitFor(() => {
      expect(alertSpy).toHaveBeenCalledWith(expect.stringContaining("harness_immutable"));
    });
    expect(alertSpy).not.toHaveBeenCalledWith("Failed to create session.");
    alertSpy.mockRestore();
  });

  it("D2: the transitioning panel renders the controller's status message for a pod-building workspace", async () => {
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating", message: "pulling workspace image" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    await waitFor(() => {
      expect(screen.getByTestId("transitioning-status-message")).toHaveTextContent("pulling workspace image");
    });
    expect(screen.queryByText(/usually takes a few seconds/i)).not.toBeInTheDocument();
  });

  it("D2 regression: a bound Creating workspace without a message keeps the generic estimate (no parked-unbound panel flash)", async () => {
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Creating" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    await waitFor(() => {
      expect(screen.getByText(/usually takes a few seconds/i)).toBeInTheDocument();
    });
    expect(screen.queryByTestId("harness-first-session-panel")).not.toBeInTheDocument();
  });

  it("D4 invariant: an Active workspace that is unbound in the listing never auto-creates — the picker decision must not be bypassed", async () => {
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ phase: "Active", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    // Latent-path pin: today the controller parks unbound workspaces in
    // Creating so this never fires — if a future path (restore, import)
    // yields unbound-Active, the harness-less auto-create stays buried.
    await waitFor(() => {
      expect(document.querySelector("textarea")).toBeInTheDocument();
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(workspacesApi.ensureSession).not.toHaveBeenCalled();
  });

  it("D4: a BOUND Active workspace auto-creates through the bind-aware ensureSession wire", async () => {
    (workspacesApi.getStatus as Mock).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as Mock).mockResolvedValue({
      items: [makeItem({ phase: "Active", harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });

    renderChat("/chat/ws-1");

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1");
    });
  });
});
