import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { WorkspaceSettingsDrawer } from "./WorkspaceSettingsDrawer";
import type { WorkspaceListItem } from "../../api/types";

vi.mock("../../api/secrets", () => ({
  secretsApi: {
    list: vi.fn(),
  },
}));

vi.mock("../../api/client", () => ({
  api: {
    get: vi.fn(),
    put: vi.fn(),
    patch: vi.fn(),
  },
}));

vi.mock("../../api/prompts", () => ({
  promptsApi: {
    getOrg: vi.fn(),
  },
}));

import { secretsApi } from "../../api/secrets";
import { api } from "../../api/client";
import { promptsApi } from "../../api/prompts";


const mockWorkspace: WorkspaceListItem = {
  id: "ws-1",
  name: "Test Workspace",
  userId: "user-1",
  runtime: "base",
  storageSize: "5Gi",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  phase: "Active",
  maxActiveSessions: 5,
};

describe("WorkspaceSettingsDrawer", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (api.get as ReturnType<typeof vi.fn>).mockResolvedValue({ bindings: [] });
    (api.put as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (api.patch as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("renders when open", () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );
    expect(screen.getByText("Workspace Settings")).toBeInTheDocument();
    expect(screen.getByText("Test Workspace")).toBeInTheDocument();
  });

  it("does not render when closed", () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={false}
        onOpenChange={vi.fn()}
      />,
    );
    expect(screen.queryByText("Workspace Settings")).not.toBeInTheDocument();
  });

  it("does not show auto-suspend controls (removed in US-47.2)", () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );
    expect(screen.queryByText("Auto-Suspend")).not.toBeInTheDocument();
    expect(screen.queryByText("Idle Timeout (min)")).not.toBeInTheDocument();
  });

  it("shows error when binding save fails", async () => {
    (secretsApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      secrets: [{ id: "s1", name: "key", type: "llm-provider" }],
    });
    (api.get as ReturnType<typeof vi.fn>).mockResolvedValue({ bindings: [] });
    (api.put as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("Network error"));
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => expect(screen.getByText("key")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("key"));
    fireEvent.click(screen.getByText("Save"));

    await waitFor(() => {
      expect(screen.getByText("Network error")).toBeInTheDocument();
    });
  });

  it("closes drawer on successful save", async () => {
    const onOpenChange = vi.fn();
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={onOpenChange}
      />,
    );

    fireEvent.click(screen.getByText("Save"));

    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("sends devPreview toggle state on save", async () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => expect(screen.getByText("Dev Preview")).toBeInTheDocument());

    fireEvent.click(screen.getByLabelText("Enable dev preview tunnel"));
    fireEvent.click(screen.getByText("Save"));

    await waitFor(() => {
      expect(api.put).toHaveBeenCalledWith(`/workspaces/ws-1/dev-preview`, {
        enabled: true,
      });
    });
  });

  it("shows open-preview link when dev preview is enabled and workspace is Active", async () => {
    (api.get as ReturnType<typeof vi.fn>).mockImplementation((path: string) => {
      if (path.includes("/prompt")) return Promise.resolve({ prompt: "" });
      if (path.includes("/bindings")) return Promise.resolve({ bindings: [] });
      if (path === `/workspaces/ws-1`) return Promise.resolve({ devPreviewEnabled: true });
      return Promise.resolve({});
    });
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => expect(screen.getByText("Open preview")).toBeInTheDocument());
    expect(screen.getByText("Open preview").closest("a")).toHaveAttribute(
      "href",
      expect.stringContaining(`/api/v1/workspaces/ws-1/dev-preview/5173/`),
    );
  });

  it("disables save button while saving", async () => {
    (secretsApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      secrets: [{ id: "s1", name: "key", type: "llm-provider" }],
    });
    (api.get as ReturnType<typeof vi.fn>).mockResolvedValue({ bindings: [] });
    (api.put as ReturnType<typeof vi.fn>).mockImplementation(() => new Promise(() => {}));
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => expect(screen.getByText("key")).toBeInTheDocument());
    fireEvent.click(screen.getByLabelText("key"));
    fireEvent.click(screen.getByText("Save"));
    expect(screen.getByText("Saving...")).toBeInTheDocument();
  });

  it("drawer content has max-w-full to prevent overflow on narrow screens", () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );
    const content = screen.getByText("Workspace Settings").closest("[class*='max-w-full']");
    expect(content).not.toBeNull();
  });
});

describe("WorkspaceSettingsDrawer – secret-type grouping (US-44.9)", () => {
  const listMock = secretsApi.list as ReturnType<typeof vi.fn>;
  const getMock = api.get as ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    getMock.mockResolvedValue({ bindings: [] });
  });

  async function renderWithSecrets(secrets: Array<{ id: string; name: string; type: string }>) {
    listMock.mockResolvedValue({ secrets });
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByText("Attached Secrets")).toBeInTheDocument());
  }

  it("renders llm-provider secrets under the LLM Providers group (regression: previously silently dropped)", async () => {
    await renderWithSecrets([
      { id: "lp-1", name: "my-anthropic", type: "llm-provider" },
    ]);

    expect(screen.getByText(/LLM Providers/)).toBeInTheDocument();
    expect(screen.getByText("my-anthropic")).toBeInTheDocument();
  });

  it("labels api-key secrets as legacy, not as LLM Providers (regression: previously mislabeled)", async () => {
    await renderWithSecrets([
      { id: "ak-1", name: "old-key", type: "api-key" },
    ]);

    expect(screen.getByText(/API Keys \(legacy\)/)).toBeInTheDocument();
    expect(screen.queryByText(/LLM Providers/)).not.toBeInTheDocument();
    expect(screen.getByText("old-key")).toBeInTheDocument();
  });

  it("renders llm-provider and api-key secrets as separate groups when both exist", async () => {
    await renderWithSecrets([
      { id: "lp-1", name: "my-anthropic", type: "llm-provider" },
      { id: "ak-1", name: "old-key", type: "api-key" },
    ]);

    expect(screen.getByText(/LLM Providers/)).toBeInTheDocument();
    expect(screen.getByText(/API Keys \(legacy\)/)).toBeInTheDocument();
    expect(screen.getByText("my-anthropic")).toBeInTheDocument();
    expect(screen.getByText("old-key")).toBeInTheDocument();
  });
});

// LLMSafeSpaces#477: when the workspace belongs to an org and that org has
// disabled member prompt customization (allow_user_prompt:false), the
// drawer's "Custom Instructions" textarea must be replaced with a locked
// message. Previously the API's WorkspaceListItem dropped OrgID, so the
// frontend always saw `workspace.orgId === undefined`, skipped the org
// policy fetch, and rendered the editable textarea unconditionally. Users
// could type a custom prompt, hit Save, get a confusing generic "Save
// failed" toast (the backend correctly returned 403), and lose their
// edits on reload.
describe("WorkspaceSettingsDrawer – org prompt-customization lock (#477)", () => {
  const orgScopedWorkspace: WorkspaceListItem = {
    ...mockWorkspace,
    orgId: "org-acme",
  };

  const getOrgMock = promptsApi.getOrg as ReturnType<typeof vi.fn>;
  const apiGetMock = api.get as ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    (secretsApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ secrets: [] });
    apiGetMock.mockResolvedValue({ bindings: [] });
  });

  it("renders the locked message when the org has disabled member prompt customization", async () => {
    getOrgMock.mockResolvedValue({ prompt: "", allowUserPrompt: false });
    render(
      <WorkspaceSettingsDrawer
        workspace={orgScopedWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => {
      expect(
        screen.getByText(/Managed by your organization\. Contact your admin to request changes\./),
      ).toBeInTheDocument();
    });
    // And the editable textarea MUST be hidden — its presence is the
    // observable symptom of #477. If this fires, the lock check was
    // skipped (regression of either the OrgID propagation or the lock
    // UI itself).
    expect(
      screen.queryByPlaceholderText(/Focus on test coverage this session/),
    ).not.toBeInTheDocument();
  });

  it("renders the editable textarea when the org allows member prompt customization", async () => {
    getOrgMock.mockResolvedValue({ prompt: "", allowUserPrompt: true });
    render(
      <WorkspaceSettingsDrawer
        workspace={orgScopedWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => {
      expect(
        screen.getByPlaceholderText(/Focus on test coverage this session/),
      ).toBeInTheDocument();
    });
    expect(
      screen.queryByText(/Managed by your organization/),
    ).not.toBeInTheDocument();
  });

  it("renders the editable textarea for personal workspaces (no orgId)", async () => {
    // mockWorkspace has no orgId — personal workspace, no org policy to
    // consult.
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => {
      expect(
        screen.getByPlaceholderText(/Focus on test coverage this session/),
      ).toBeInTheDocument();
    });
    expect(getOrgMock).not.toHaveBeenCalled();
  });

  it("locks the textarea (fails closed) when the org policy fetch fails — defense in depth", async () => {
    // If we can't determine the org's allow_user_prompt policy, default
    // to LOCKED. Better UX than "type away, your save will silently 403"
    // — the user sees the lock message and can retry / reload.
    getOrgMock.mockRejectedValue(new Error("network blip"));
    render(
      <WorkspaceSettingsDrawer
        workspace={orgScopedWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() => {
      expect(
        screen.getByText(/Managed by your organization\. Contact your admin to request changes\./),
      ).toBeInTheDocument();
    });
  });
});

// LLMSafeSpaces#480: the workspace user's "Custom Instructions" helper
// text leaked internal prompt-chain terminology ("Appended after your
// role's system prompt"). End users don't need to know about the role
// or system-prompt layering. These tests assert the rewritten copy
// renders AND the leaky terms are absent.
describe("WorkspaceSettingsDrawer – Custom Instructions copy hides jargon (#480)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (secretsApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ secrets: [] });
    (api.get as ReturnType<typeof vi.fn>).mockResolvedValue({ bindings: [] });
  });

  it("renders the rewritten helper text (no 'role's system prompt' or 'Appended')", async () => {
    render(
      <WorkspaceSettingsDrawer
        workspace={mockWorkspace}
        open={true}
        onOpenChange={vi.fn()}
      />,
    );

    await waitFor(() =>
      expect(
        screen.getByText(/Instructions specific to this workspace/),
      ).toBeInTheDocument(),
    );

    // The leaky old strings must be absent.
    expect(screen.queryByText(/role's system prompt/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Appended/)).not.toBeInTheDocument();
  });
});
