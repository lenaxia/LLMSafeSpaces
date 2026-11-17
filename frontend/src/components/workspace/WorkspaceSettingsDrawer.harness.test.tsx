import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import { WorkspaceSettingsDrawer } from "./WorkspaceSettingsDrawer";
import type { WorkspaceListItem } from "../../api/types";
import { render } from "../../test/utils";

vi.mock("../../api/secrets", () => ({
  secretsApi: { list: vi.fn().mockResolvedValue({ secrets: [] }) },
}));

vi.mock("../../api/prompts", () => ({
  promptsApi: { getOrg: vi.fn().mockResolvedValue({ allowUserPrompt: true }) },
}));

vi.mock("../../api/client", () => ({
  api: {
    get: vi.fn().mockResolvedValue({}),
    put: vi.fn().mockResolvedValue(undefined),
  },
  ApiClientError: class extends Error {},
}));

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

function renderDrawer(workspace: WorkspaceListItem) {
  render(
    <WorkspaceSettingsDrawer workspace={workspace} open onOpenChange={vi.fn()} />,
  );
}

describe("WorkspaceSettingsDrawer — the read-only harness surface", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("bound workspace: badge + permanence note, informational only", () => {
    renderDrawer(makeWorkspace({ harness: "pi" }));

    expect(screen.getByTestId("harness-settings-row")).toBeInTheDocument();
    const badge = screen.getByTestId("harness-badge");
    expect(badge).toHaveTextContent("pi");
    expect(screen.getByText(/permanent for this workspace/i)).toBeInTheDocument();
  });

  it("unbound workspace: the not-chosen-yet note points at the first session", () => {
    renderDrawer(makeWorkspace({ harness: undefined }));

    expect(screen.getByTestId("harness-unbound-settings")).toHaveTextContent(
      /pick a harness when starting this workspace's first session/i,
    );
    expect(screen.queryByTestId("harness-badge")).not.toBeInTheDocument();
  });

  it("ZERO switch affordances: no harness control exists anywhere on the surface", () => {
    renderDrawer(makeWorkspace({ harness: "pi", harnessOptions: ["opencode", "pi"] }));

    const row = screen.getByTestId("harness-settings-row");
    expect(screen.queryByRole("button", { name: /harness/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    expect(screen.queryByRole("radio")).not.toBeInTheDocument();
    expect(row.querySelector("button, select, input")).toBeNull();
  });
});
