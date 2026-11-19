import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";
import { ApiClientError } from "../../api/client";
import type { WorkspaceListItem } from "../../api/types";

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
    list: vi.fn(),
    create: vi.fn().mockResolvedValue({ id: "ws-new", name: "new-ws" }),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-1", workspaceId: "ws-1", resumed: false }),
    getSessions: vi.fn(),
    getStatus: vi.fn().mockResolvedValue(null),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";

type ListMock = ReturnType<typeof vi.fn>;

function makeWorkspace(over: Partial<WorkspaceListItem> = {}): WorkspaceListItem {
  return {
    id: "ws-1",
    name: "My Workspace",
    userId: "u1",
    runtime: "base",
    storageSize: "5Gi",
    createdAt: "",
    updatedAt: "",
    phase: "Active",
    ...over,
  };
}

function renderSidebar() {
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <MemoryRouter initialEntries={["/chat"]}>
          <Sidebar />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  );
}

let qc: QueryClient;

describe("Sidebar — first-session harness picker (bind-once)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  it("picker opens on the FIRST session of an unbound multi-option workspace; submit binds the chosen harness", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const newChat = await screen.findByRole("button", { name: "New chat" });
    await user.click(newChat);

    const dialog = screen.getByRole("dialog");
    expect(dialog).toBeInTheDocument();
    expect(screen.getByText(/cannot be changed later/i)).toBeInTheDocument();
    // The default is preselected inside the modal.
    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveAttribute("aria-checked", "true");

    await user.click(screen.getByRole("radio", { name: /pi/i }));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "pi" });
    });
  });

  it("picker appears exactly once: a BOUND workspace never opens it — the call carries no harness", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([
      { id: "sess-1", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", {});
    });
  });

  it("single enabled harness: binds silently with NO modal", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "opencode" });
    });
  });

  it("unbound workspace with zero sessions renders the choose-a-harness hint; bound does not", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [
        makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] }),
        makeWorkspace({ id: "ws-2", name: "Bound", harness: "pi", harnessOptions: ["opencode", "pi"] }),
      ],
      pagination: { limit: 20, offset: 0, total: 2 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const hint = await screen.findByTestId("harness-unbound-hint");
    expect(hint).toHaveTextContent(/choose a harness when starting your first session/i);
    expect(screen.getAllByTestId("harness-unbound-hint")).toHaveLength(1);
  });

  it("bound workspace renders the read-only harness badge in its header row", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ name: "Pi Workspace", harness: "pi" })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const badge = await screen.findByTestId("harness-badge");
    expect(badge).toHaveTextContent("pi");
    expect(badge).toHaveAttribute("title", "Harness: pi — permanent for this workspace");
  });

  it("unbound workspace renders no badge", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    await screen.findByText("My Workspace");
    expect(screen.queryByTestId("harness-badge")).not.toBeInTheDocument();
  });

  // #2092 ruling 3: an unbound workspace whose binds persistently fail
  // (cluster CRD predates the harness field) must surface the degraded
  // hint INSTEAD of the unbounded Creating phase text — the honest
  // state is operator-actionable, not "in progress".
  it("bindBlocked: renders the CRD-blocked hint, never the bare Creating phase", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"], bindBlocked: true })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const hint = await screen.findByTestId("bind-blocked-hint");
    expect(hint).toHaveTextContent(/blocked on cluster CRD — operator action required/i);
    // The plain phase span is REPLACED, not accompanied: "Creating"
    // would read as progress on a workspace that can never converge.
    expect(screen.queryByText("Creating")).not.toBeInTheDocument();
  });

  // Contrast pin: without bindBlocked the Creating phase renders
  // exactly as before (the healthy unbound wait is unchanged).
  it("without bindBlocked the plain Creating phase still renders", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    expect(await screen.findByText("Creating")).toBeInTheDocument();
    expect(screen.queryByTestId("bind-blocked-hint")).not.toBeInTheDocument();
  });
});

describe("Sidebar — bind-failure error-class fidelity (D3)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  it("picker submit failure keeps the dialog open with the server's class inline — no generic alert", async () => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);
    (workspacesApi.ensureSession as ListMock).mockRejectedValue(
      new ApiClientError(500, {
        error:
          "internal_error: harness_bind_not_persisted (the harness bind for workspace ws-1 was accepted but did not persist — the cluster's Workspace CRD predates 0.44.15; the operator must `kubectl apply -f helm/crds/workspace.yaml`)",
      }),
    );

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalled();
    });
    // The dialog survives with the class + operator remediation inline;
    // the choice is preserved for retry after the CRD lands.
    await waitFor(() => {
      expect(screen.getByRole("dialog")).toBeInTheDocument();
      expect(screen.getByText(/harness_bind_not_persisted/)).toBeInTheDocument();
      expect(screen.getByText(/kubectl apply -f helm\/crds\/workspace\.yaml/)).toBeInTheDocument();
    });
    expect(alertSpy).not.toHaveBeenCalled();
    alertSpy.mockRestore();
  });

  // The audit's born-red repro: the three failure classes must be
  // distinguishable in the surfaced text (on the skewed cluster the
  // generic alert hid the exact operator remediation).
  it.each([
    [500, "internal_error: harness_bind_not_persisted (spec.harness reads back empty)", /harness_bind_not_persisted/],
    [409, "harness_immutable: harness is immutable once bound for this workspace", /harness_immutable/],
    [422, "validation_error: harness \"pi\" is not enabled on this instance", /not enabled on this instance/],
  ])("class %i renders distinct inline text", async (_status, error, pattern) => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);
    (workspacesApi.ensureSession as ListMock).mockRejectedValue(new ApiClientError(_status, { error }));

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(screen.getByText(pattern)).toBeInTheDocument();
    });
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(alertSpy).not.toHaveBeenCalled();
    alertSpy.mockRestore();
  });

  it("single-option silent-bind failure alerts the honest class text, not the generic string", async () => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);
    (workspacesApi.ensureSession as ListMock).mockRejectedValue(
      new ApiClientError(500, {
        error: "internal_error: harness_bind_not_persisted (spec.harness reads back empty — apply helm/crds/workspace.yaml)",
      }),
    );

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));

    await waitFor(() => {
      expect(alertSpy).toHaveBeenCalledWith(expect.stringContaining("harness_bind_not_persisted"));
    });
    expect(alertSpy).not.toHaveBeenCalledWith("Failed to create session.");
    alertSpy.mockRestore();
  });

  // Review r1 F2 pin: a failure the open dialog rendered inline is
  // consumed with the dialog — canceling after reading it must not
  // re-alert the same message.
  it("cancel after an inline picker failure does not re-alert on dialog close", async () => {
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);
    (workspacesApi.ensureSession as ListMock).mockRejectedValue(
      new ApiClientError(409, { error: "harness_immutable: harness is immutable once bound for this workspace" }),
    );

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));
    await user.click(screen.getByTestId("harness-picker-submit"));
    await waitFor(() => {
      expect(screen.getByText(/harness_immutable/)).toBeInTheDocument();
    });

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(alertSpy).not.toHaveBeenCalled();
    alertSpy.mockRestore();
  });
});

describe("Sidebar — empty-state first-session CTA (D5)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  it("unbound workspace with zero sessions renders the CTA; it opens the picker", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const cta = await screen.findByTestId("empty-state-new-session");
    expect(cta).toHaveTextContent(/choose harness & start/i);
    await user.click(cta);

    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("bound workspace with zero sessions: the CTA creates directly (no picker)", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const cta = await screen.findByTestId("empty-state-new-session");
    expect(cta).toHaveTextContent(/start a session/i);
    await user.click(cta);

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", {});
    });
  });

  it("the CTA is disabled while a create is in flight (double-fire guard)", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);
    (workspacesApi.ensureSession as ListMock).mockImplementation(() => new Promise(() => {}));

    renderSidebar();

    const cta = await screen.findByTestId("empty-state-new-session");
    await user.click(cta);

    await waitFor(() => {
      expect(screen.getByTestId("empty-state-new-session")).toBeDisabled();
    });
  });

  it("a suspended workspace hides the empty-state CTA (it cannot create sessions)", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Suspended", harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    await screen.findByText(/no sessions yet/i);
    expect(screen.queryByTestId("empty-state-new-session")).not.toBeInTheDocument();
  });
});
