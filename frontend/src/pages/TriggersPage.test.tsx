// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TriggersPage } from "./TriggersPage";
import type { Trigger } from "../api/workflows";

// ─── API mocks ────────────────────────────────────────────────────────────────

const mockList = vi.fn();
const mockCreate = vi.fn();
const mockDelete = vi.fn();
const mockUpdate = vi.fn();
const mockWorkspacesList = vi.fn();

vi.mock("../api/workflows", () => ({
  workflowApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn(), update: vi.fn(), delete: vi.fn(), run: vi.fn() },
  triggerApi: {
    list: () => mockList(),
    create: (data: unknown) => mockCreate(data),
    delete: (id: string) => mockDelete(id),
    update: (id: string, data: unknown) => mockUpdate(id, data),
  },
  runApi: { get: vi.fn(), cancel: vi.fn(), nodes: vi.fn() },
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: { list: () => mockWorkspacesList() },
}));

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const CRON_TRIGGER: Trigger = {
  id: "trig-1", name: "nightly-cron", enabled: true,
  sourceType: "cron", sourceConfig: { expr: "0 2 * * *" },
  workspaceId: "ws-1", prompt: "Run nightly backup",
  memoryMode: "none", captureMode: "errors_only", preserveSession: "never",
  consecutiveFailures: 0, autoDisableAfter: 10,
  nextFireAt: "2026-08-08T02:00:00Z",
  createdAt: "2026-08-01T00:00:00Z", updatedAt: "2026-08-01T00:00:00Z",
};

const WEBHOOK_TRIGGER: Trigger = {
  id: "trig-2", name: "github-hook", enabled: false,
  sourceType: "webhook", sourceConfig: {},
  workspaceId: "ws-2", prompt: "Analyze GitHub issue",
  memoryMode: "none", captureMode: "errors_only", preserveSession: "never",
  consecutiveFailures: 3, autoDisableAfter: 10,
  createdAt: "2026-08-01T00:00:00Z", updatedAt: "2026-08-01T00:00:00Z",
};

const WORKFLOW_TRIGGER: Trigger = {
  id: "trig-3", name: "wf-trigger", enabled: true,
  sourceType: "cron", sourceConfig: { expr: "0 * * * *" },
  workflowId: "wf-1",
  consecutiveFailures: 0, autoDisableAfter: 10,
  createdAt: "2026-08-01T00:00:00Z", updatedAt: "2026-08-01T00:00:00Z",
};

const ROUTINE_WITH_SCRIPT: Trigger = {
  id: "trig-4", name: "script-routine", enabled: true,
  sourceType: "cron", sourceConfig: { expr: "0 * * * *" },
  workspaceId: "ws-1", prompt: "Process data",
  agent: "custom-agent",
  scriptPath: "/scripts/fetch.sh",
  scriptArgs: ["--flag", "value"],
  scriptEnv: { FOO: "bar", BAZ: "qux" },
  memoryMode: "last_result", captureMode: "full", preserveSession: "always",
  consecutiveFailures: 0, autoDisableAfter: 10,
  createdAt: "2026-08-01T00:00:00Z", updatedAt: "2026-08-01T00:00:00Z",
};

const WORKSPACES = {
  items: [
    { id: "ws-1", name: "alpha-workspace", phase: "Active" },
    { id: "ws-2", name: "beta-workspace", phase: "Suspended" },
    { id: "ws-3", name: "gamma-workspace", phase: "Active" },
  ],
};

// ─── Helpers ──────────────────────────────────────────────────────────────────

function renderPage(path = "/triggers") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/triggers" element={<TriggersPage />} />
          <Route path="/triggers/:triggerId" element={<TriggersPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("TriggersPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockWorkspacesList.mockResolvedValue(WORKSPACES);
  });

  it("renders trigger list with cron and webhook", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER, WEBHOOK_TRIGGER]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("nightly-cron")).toBeInTheDocument();
      expect(screen.getByText("github-hook")).toBeInTheDocument();
    });
  });

  it("shows disabled badge for disabled triggers", async () => {
    mockList.mockResolvedValue([WEBHOOK_TRIGGER]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("disabled")).toBeInTheDocument();
    });
  });

  it("shows consecutive failures count", async () => {
    mockList.mockResolvedValue([WEBHOOK_TRIGGER]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/3 failures/)).toBeInTheDocument();
    });
  });

  it("renders empty state", async () => {
    mockList.mockResolvedValue([]);
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/No triggers yet/)).toBeInTheDocument();
    });
  });

  it("shows create form when + clicked", async () => {
    mockList.mockResolvedValue([]);
    renderPage();
    await waitFor(() => expect(screen.getByLabelText("New trigger")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("New trigger"));
    expect(screen.getByPlaceholderText("Trigger name")).toBeInTheDocument();
    expect(screen.getByText("Schedule")).toBeInTheDocument();
  });

  it("shows trigger detail when selected", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER, WEBHOOK_TRIGGER]);
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText("Circuit Breaker")).toBeInTheDocument();
      expect(screen.getAllByText("nightly-cron").length).toBeGreaterThan(0);
    });
  });

  it("toggles trigger enabled state", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    mockUpdate.mockResolvedValue({});
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText("Enabled")).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText("Enabled"));
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith("trig-1", { enabled: false });
    });
  });

  it("shows Target Workspace (not Target Workflow) for routine triggers", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText("Target Workspace")).toBeInTheDocument();
      expect(screen.queryByText("Target Workflow")).not.toBeInTheDocument();
    });
  });

  it("shows Target Workflow for workflow triggers", async () => {
    mockList.mockResolvedValue([WORKFLOW_TRIGGER]);
    renderPage("/triggers/trig-3");
    await waitFor(() => {
      expect(screen.getByText("Target Workflow")).toBeInTheDocument();
      expect(screen.queryByText("Target Workspace")).not.toBeInTheDocument();
    });
  });

  it("shows prompt pre-filled from trigger data for routine triggers", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText("Run nightly backup")).toBeInTheDocument();
    });
  });

  it("shows workspace name in target section for routine triggers", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText("alpha-workspace")).toBeInTheDocument();
    });
  });

  it("sends routine fields when saving target for routine trigger", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    mockUpdate.mockResolvedValue({});
    renderPage("/triggers/trig-1");
    let targetSection: HTMLElement;
    await waitFor(() => {
      targetSection = screen.getByText("Target Workspace").closest("div.rounded-lg")!;
    });
    fireEvent.click(within(targetSection!).getByText("Edit"));
    await waitFor(() => {
      expect(within(targetSection!).getByText("Save")).toBeInTheDocument();
    });
    fireEvent.click(within(targetSection!).getByText("Save"));
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith("trig-1", expect.objectContaining({
        workspaceId: "ws-1",
        prompt: "Run nightly backup",
        memoryMode: "none",
        captureMode: "errors_only",
        preserveSession: "never",
      }));
    });
  });

  it("shows memory/capture/session settings for routine triggers", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    renderPage("/triggers/trig-1");
    await waitFor(() => {
      expect(screen.getByText(/Memory:/)).toBeInTheDocument();
      expect(screen.getByText(/Capture:/)).toBeInTheDocument();
      expect(screen.getByText(/Session:/)).toBeInTheDocument();
    });
  });

  it("sends agent + script fields including parsed env when saving", async () => {
    mockList.mockResolvedValue([ROUTINE_WITH_SCRIPT]);
    mockUpdate.mockResolvedValue({});
    renderPage("/triggers/trig-4");
    let targetSection: HTMLElement;
    await waitFor(() => {
      targetSection = screen.getByText("Target Workspace").closest("div.rounded-lg")!;
    });
    fireEvent.click(within(targetSection!).getByText("Edit"));
    await waitFor(() => {
      expect(within(targetSection!).getByText("Save")).toBeInTheDocument();
    });
    fireEvent.click(within(targetSection!).getByText("Save"));
    await waitFor(() => {
      expect(mockUpdate).toHaveBeenCalledWith("trig-4", expect.objectContaining({
        workspaceId: "ws-1",
        prompt: "Process data",
        agent: "custom-agent",
        scriptPath: "/scripts/fetch.sh",
        scriptArgs: ["--flag", "value"],
        scriptEnv: { FOO: "bar", BAZ: "qux" },
        memoryMode: "last_result",
        captureMode: "full",
        preserveSession: "always",
      }));
    });
  });

  it("discards edits when cancel clicked without calling onUpdate", async () => {
    mockList.mockResolvedValue([CRON_TRIGGER]);
    mockUpdate.mockResolvedValue({});
    renderPage("/triggers/trig-1");
    let targetSection: HTMLElement;
    await waitFor(() => {
      targetSection = screen.getByText("Target Workspace").closest("div.rounded-lg")!;
    });
    fireEvent.click(within(targetSection!).getByText("Edit"));
    await waitFor(() => {
      expect(within(targetSection!).getByText("Cancel")).toBeInTheDocument();
    });
    const updateCountBefore = mockUpdate.mock.calls.length;
    fireEvent.click(within(targetSection!).getByText("Cancel"));
    await waitFor(() => {
      expect(within(targetSection!).getByText("Edit")).toBeInTheDocument();
    });
    expect(mockUpdate.mock.calls.length).toBe(updateCountBefore);
  });
});
