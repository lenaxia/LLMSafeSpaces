// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, fireEvent, waitFor } from "@testing-library/react";
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

vi.mock("../api/workflows", () => ({
  workflowApi: { list: vi.fn(), create: vi.fn(), update: vi.fn(), delete: vi.fn(), run: vi.fn() },
  triggerApi: {
    list: () => mockList(),
    create: (data: unknown) => mockCreate(data),
    delete: (id: string) => mockDelete(id),
    update: (id: string, data: unknown) => mockUpdate(id, data),
  },
  runApi: { get: vi.fn(), cancel: vi.fn(), nodes: vi.fn() },
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
});
