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
  runApi: { get: vi.fn(), cancel: vi.fn(), nodes: vi.fn() },
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
