import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { RoleSelector } from "./RoleSelector";
import { agentRolesApi, type AgentRole } from "../../api/agentRoles";
import { mockButtonRect, mockMenuSize, mockViewport, restoreMenuGeometry } from "../../test/menuGeometry";

function role(id: string, name: string, description = ""): AgentRole {
  return {
    id, scope: "platform", name, slug: id, description, isDefault: false,
    config: {}, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
  };
}

vi.mock("../../api/agentRoles", () => ({
  agentRolesApi: {
    listPlatform: vi.fn(),
    listOrg: vi.fn(),
    getWorkspaceRole: vi.fn(),
    setWorkspaceRole: vi.fn(),
    clearWorkspaceRole: vi.fn(),
  },
}));

vi.mock("../../api/prompts", () => ({
  promptsApi: {
    getOrg: vi.fn(),
  },
}));

import { promptsApi } from "../../api/prompts";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

describe("RoleSelector", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(agentRolesApi.listPlatform).mockResolvedValue([role("p1", "Reviewer", "Reviews code"), role("p2", "Coder")]);
    vi.mocked(agentRolesApi.listOrg).mockResolvedValue([]);
    vi.mocked(agentRolesApi.getWorkspaceRole).mockResolvedValue(role("p1", "Reviewer", "Reviews code"));
  });

  it("renders the current role name", async () => {
    render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => expect(screen.getByText("Reviewer")).toBeInTheDocument());
  });

  it("lists all roles on open and offers platform-default reset when a role is set", async () => {
    render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Reviewer"));
    fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
    expect(screen.getByText("Coder")).toBeInTheDocument();
    expect(screen.getByText("Use platform default")).toBeInTheDocument();
  });

  it("hides the platform-default option when no role is set", async () => {
    vi.mocked(agentRolesApi.getWorkspaceRole).mockResolvedValue(null);
    render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByRole("button", { name: /Default/i }));
    fireEvent.click(screen.getByRole("button", { name: /Default/i }));
    expect(screen.getByText("Coder")).toBeInTheDocument();
    expect(screen.queryByText("Use platform default")).not.toBeInTheDocument();
  });

  it("calls setWorkspaceRole on selection", async () => {
    vi.mocked(agentRolesApi.setWorkspaceRole).mockResolvedValue({ status: "ok" });
    render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Reviewer"));
    fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
    fireEvent.click(screen.getByText("Coder"));
    await waitFor(() => expect(agentRolesApi.setWorkspaceRole).toHaveBeenCalledWith("ws-1", "p2"));
  });

  it("calls clearWorkspaceRole when platform default is chosen", async () => {
    vi.mocked(agentRolesApi.clearWorkspaceRole).mockResolvedValue({ status: "ok" });
    render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Reviewer"));
    fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
    fireEvent.click(screen.getByText("Use platform default"));
    await waitFor(() => expect(agentRolesApi.clearWorkspaceRole).toHaveBeenCalledWith("ws-1"));
  });

  it("shows the locked state when the org disallows user prompts", async () => {
    vi.mocked(promptsApi.getOrg).mockResolvedValue({ prompt: "", allowUserPrompt: false });
    render(<RoleSelector workspaceId="ws-1" orgId="org-1" />, { wrapper });
    await waitFor(() => expect(screen.getByTitle("Your org admin manages agent roles")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /Reviewer/i })).not.toBeInTheDocument();
  });

  it("renders nothing when there are no roles and no current role", async () => {
    vi.mocked(agentRolesApi.listPlatform).mockResolvedValue([]);
    vi.mocked(agentRolesApi.getWorkspaceRole).mockResolvedValue(null);
    const { container } = render(<RoleSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => expect(container.querySelector("button")).toBeNull());
  });

  // --- Viewport-aware positioning ---
  // Regression: the composer drawer (US-67.5) moved the selector to the
  // bottom of the screen; a dropdown that opens downward renders off-screen.

  describe("viewport-aware positioning", () => {
    afterEach(restoreMenuGeometry);

    async function openDropdown() {
      render(<RoleSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Reviewer"));
      fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
    }

    it("portals the dropdown to document.body with fixed positioning", async () => {
      await openDropdown();
      const menu = screen.getByRole("menu");
      expect(menu.parentElement).toBe(document.body);
      expect(menu.className).toContain("fixed");
      expect(menu.className).toContain("z-50");
    });

    it("flips above when the trigger is near the viewport bottom", async () => {
      mockViewport(600);
      mockMenuSize(150, 224);
      render(<RoleSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Reviewer"));
      mockButtonRect(screen.getByRole("button", { name: /Reviewer/i }), {
        top: 580, bottom: 600, left: 300, right: 440,
      });
      fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
      const menu = screen.getByRole("menu");
      // 580 - 150 - 4 = 426 (flipped above), NOT 604 (below, off-screen).
      expect(menu.style.top).toBe("426px");
    });

    it("clamps left when the dropdown would overflow the right edge", async () => {
      mockViewport(800, 800);
      mockMenuSize(150, 224);
      render(<RoleSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Reviewer"));
      mockButtonRect(screen.getByRole("button", { name: /Reviewer/i }), {
        top: 100, bottom: 124, left: 780, right: 800,
      });
      fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
      const menu = screen.getByRole("menu");
      // Right-aligned left = 800-224 = 576; 576+224 = 800 > 792 → clamp to 568.
      expect(menu.style.left).toBe("568px");
    });

    it("caps dropdown height when it is taller than the viewport room", async () => {
      mockViewport(300);
      mockMenuSize(400, 224);
      render(<RoleSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Reviewer"));
      mockButtonRect(screen.getByRole("button", { name: /Reviewer/i }), {
        top: 280, bottom: 290, left: 300, right: 440,
      });
      fireEvent.click(screen.getByRole("button", { name: /Reviewer/i }));
      const menu = screen.getByRole("menu");
      expect(menu.style.top).toBe("8px");
      expect(menu.style.maxHeight).toBe("268px");
    });
  });
});
