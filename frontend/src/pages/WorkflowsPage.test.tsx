// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { WorkflowsPage } from "./WorkflowsPage";
import type { Workflow } from "../api/workflows";

// ─── API mocks ────────────────────────────────────────────────────────────────

const mockList = vi.fn();
const mockCreate = vi.fn();
const mockUpdate = vi.fn();
const mockDelete = vi.fn();
const mockRun = vi.fn();

vi.mock("../api/workflows", () => ({
  workflowApi: {
    list: () => mockList(),
    create: (data: unknown) => mockCreate(data),
    update: (id: string, data: unknown) => mockUpdate(id, data),
    delete: (id: string) => mockDelete(id),
    run: (id: string) => mockRun(id),
  },
  triggerApi: { list: vi.fn(), create: vi.fn(), update: vi.fn(), delete: vi.fn() },
  runApi: { get: vi.fn(), cancel: vi.fn(), nodes: vi.fn(), listForWorkflow: (...a: unknown[]) => mockListRuns(...a) },
}));

const mockListRuns = vi.fn();
const mockGetAlerts = vi.fn();
const mockGetSessions = vi.fn();

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getAlerts: (id: string) => mockGetAlerts(id),
    getSessions: (id: string) => mockGetSessions(id),
  },
}));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const WF1: Workflow = {
  id: "wf-1", ownerType: "user", name: "meeting-processor", slug: "meeting-processor",
  description: "Processes meetings", specYaml: '{"nodes":[],"edges":[]}',
  status: "active", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-02T00:00:00Z",
};

const WF2: Workflow = {
  id: "wf-2", ownerType: "user", name: "nightly-backup", slug: "nightly-backup",
  description: "", specYaml: '{"nodes":[],"edges":[]}',
  status: "draft", createdAt: "2026-01-03T00:00:00Z", updatedAt: "2026-01-03T00:00:00Z",
};

// ─── Helpers ──────────────────────────────────────────────────────────────────

function renderPage(path = "/workflows") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/workflows" element={<WorkflowsPage />} />
          <Route path="/workflows/:workflowId" element={<WorkflowsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("WorkflowsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders workflow list", async () => {
    mockList.mockResolvedValue([WF1, WF2]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("meeting-processor")).toBeInTheDocument();
      expect(screen.getByText("nightly-backup")).toBeInTheDocument();
    });
  });

  it("renders empty state when no workflows", async () => {
    mockList.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/No workflows yet/)).toBeInTheDocument();
    });
  });

  it("shows create form when + button clicked", async () => {
    mockList.mockResolvedValue([]);
    renderPage();
    await waitFor(() => expect(screen.getByLabelText("New workflow")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("New workflow"));
    expect(screen.getByPlaceholderText("Workflow name")).toBeInTheDocument();
  });

  it("creates a workflow and navigates to it", async () => {
    mockList.mockResolvedValue([]);
    mockCreate.mockResolvedValue(WF1);
    renderPage();
    await waitFor(() => expect(screen.getByLabelText("New workflow")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("New workflow"));

    fireEvent.change(screen.getByPlaceholderText("Workflow name"), { target: { value: "test" } });
    fireEvent.click(screen.getByText("Create"));

    await waitFor(() => expect(mockCreate).toHaveBeenCalled());
  });

  it("renders workflows when data loads", async () => {
    mockList.mockResolvedValue([WF1, WF2]);
    renderPage("/workflows/wf-1");
    await waitFor(() => {
      expect(screen.getByText("nightly-backup")).toBeInTheDocument();
    });
  });
});

// ─── D6 hung-alert badge (#998) ──────────────────────────────────────────────

describe("WorkflowsPage — D6 hung-alert badge (#998)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockList.mockResolvedValue([WF1]);
  });

  function runFixture(workspaceId?: string) {
    return {
      id: "run-1234567890abcdef", workflowId: "wf-1", status: "running",
      workspaceId, createdAt: "2026-08-26T00:00:00Z", updatedAt: "2026-08-26T00:00:00Z",
    };
  }

  async function openHistory() {
    renderPage("/workflows/wf-1");
    const toggle = await screen.findByRole("button", { name: /run history/i });
    fireEvent.click(toggle);
  }

  it("flags runs whose workspace has a persisted hung alert for a STILL-BUSY session", async () => {
    mockListRuns.mockResolvedValue([runFixture("ws-hung")]);
    mockGetAlerts.mockResolvedValue([
      { id: "1", workspaceId: "ws-hung", sessionId: "ses-x", alert: "session_hung", oldestBusySeconds: 960, createdAt: new Date().toISOString() },
    ]);
    mockGetSessions.mockResolvedValue([{ id: "ses-x", title: "t", status: "busy" }]);
    await openHistory();
    expect(await screen.findByTestId("workflow-hung-alert")).toBeInTheDocument();
    expect(mockGetAlerts).toHaveBeenCalledWith("ws-hung");
  });

  it("shows no badge for stale history when the alerted session recovered (idle)", async () => {
    // The feed is append-only 24h history: an alert for a session that
    // has since gone idle must not badge the run (the shipped
    // mere-existence bug, mirrored from SessionActivityProvider).
    mockListRuns.mockResolvedValue([runFixture("ws-hung")]);
    mockGetAlerts.mockResolvedValue([
      { id: "1", workspaceId: "ws-hung", sessionId: "ses-x", alert: "session_hung", oldestBusySeconds: 3154, createdAt: "2026-10-03T04:59:06Z" },
    ]);
    mockGetSessions.mockResolvedValue([{ id: "ses-x", title: "t", status: "idle" }]);
    await openHistory();
    await screen.findByText("run-1234");
    // The badge decision is async (alerts + sessions queries) — settle
    // both before asserting the negative, or the assertion runs before
    // a mere-existence page could commit the dot (vacuous pass).
    await waitFor(() => expect(mockGetAlerts).toHaveBeenCalledWith("ws-hung"));
    await waitFor(() => expect(mockGetSessions).toHaveBeenCalledWith("ws-hung"));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("workflow-hung-alert")).not.toBeInTheDocument();
  });

  it("shows no badge when the run's workspace is alert-free", async () => {
    mockListRuns.mockResolvedValue([runFixture("ws-ok")]);
    mockGetAlerts.mockResolvedValue([]);
    mockGetSessions.mockResolvedValue([]);
    await openHistory();
    await screen.findByText("run-1234");
    expect(screen.queryByTestId("workflow-hung-alert")).not.toBeInTheDocument();
  });
});

// ─── ConfirmDialog (#814) ────────────────────────────────────────────────────

describe("WorkflowsPage — ConfirmDialog delete (#814)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockList.mockResolvedValue([WF1]);
    mockDelete.mockResolvedValue(undefined);
  });

  it("opens the confirm dialog and deletes the workflow on confirm", async () => {
    renderPage("/workflows/wf-1");
    // Wait for the editor (edit mode) to mount its Delete button.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument(),
    );

    // Editor's Delete button opens the confirm dialog.
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    // ConfirmDialog opens — title visible, and a second "Delete" button (the
    // dialog's confirm) is now present. The last matching button is the dialog's.
    await waitFor(() => expect(screen.getByText("Delete workflow?")).toBeInTheDocument());
    const dialogConfirm = screen.getAllByRole("button", { name: "Delete" }).pop()!;
    fireEvent.click(dialogConfirm);

    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("wf-1"));
  });

  it("does not delete when the dialog is cancelled", async () => {
    renderPage("/workflows/wf-1");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    fireEvent.click(cancelBtn);

    expect(mockDelete).not.toHaveBeenCalled();
  });
});
