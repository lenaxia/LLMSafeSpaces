import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ModelSelector } from "./ModelSelector";
import { mockButtonRect, mockMenuSize, mockViewport, restoreMenuGeometry } from "../../test/menuGeometry";

const mockModels = {
  models: [
    { id: "anthropic/claude-sonnet-4-5", providerID: "anthropic", name: "Claude Sonnet", tier: "paid", freeTier: false, selected: true, enabled: true },
    { id: "openai/gpt-4o", providerID: "openai", name: "GPT-4o", tier: "paid", freeTier: false, selected: false, enabled: true },
    { id: "opencode/free-model", providerID: "opencode", name: "Free Model", tier: "free", freeTier: true, selected: false, enabled: true },
    { id: "disabled/model", providerID: "test", name: "Disabled", tier: "paid", freeTier: false, selected: false, enabled: false },
  ],
  currentModel: "anthropic/claude-sonnet-4-5",
};

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    listModels: vi.fn(),
    setModel: vi.fn(),
  },
}));

vi.mock("../../hooks/useUserSettings", () => ({
  useUserSetting: vi.fn((_key: string, defaultValue: unknown) => defaultValue),
}));

import { workspacesApi } from "../../api/workspaces";
import { useUserSetting } from "../../hooks/useUserSettings";

function wrapper({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

describe("ModelSelector", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(useUserSetting).mockReturnValue("");
  });

  it("renders current model name when loaded", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => expect(screen.getByText("Claude Sonnet")).toBeInTheDocument());
  });

  it("shows dropdown with all returned models on click", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("Claude Sonnet"));
    expect(screen.getByText("GPT-4o")).toBeInTheDocument();
    expect(screen.getByText("Free Model")).toBeInTheDocument();
    // The backend already filters out unavailable models before returning;
    // the frontend renders all models it receives without a secondary filter.
    // The "Disabled" model in the mock fixture is never sent by a real backend.
  });

  it("calls setModel when a model is selected", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: true });
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("GPT-4o"));
    await waitFor(() => expect(workspacesApi.setModel).toHaveBeenCalledWith("ws-1", "openai/gpt-4o"));
  });

  it("closes dropdown immediately on selection (optimistic)", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    // setModel never resolves — verifies dropdown closes before server response
    vi.mocked(workspacesApi.setModel).mockReturnValue(new Promise(() => {}));
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
    // Dropdown open: Claude Sonnet appears in the list
    const listItems = screen.getAllByText("Claude Sonnet");
    expect(listItems.length).toBeGreaterThan(1);
    fireEvent.click(screen.getByText("GPT-4o"));
    // After click the dropdown list closes: only one instance of GPT-4o
    // (the optimistic trigger label), not two (trigger + dropdown item)
    await waitFor(() => expect(screen.getAllByText("GPT-4o").length).toBe(1));
  });

  it("shows selected model immediately on click (optimistic), reverts on error", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    vi.mocked(workspacesApi.setModel).mockRejectedValue(new Error("500"));
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("GPT-4o"));
    // Immediately after click the button label should show the new model
    expect(screen.getByText("GPT-4o")).toBeInTheDocument();
    // After error resolves it reverts to the server-confirmed model
    await waitFor(() => expect(screen.getByText("Claude Sonnet")).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText(/Failed to set model/)).toBeInTheDocument());
  });

  it("renders nothing when no models available", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue({ models: [], currentModel: "" });
    const { container } = render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => expect(container.querySelector("button")).toBeNull());
  });

  it("shows tier badges", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    fireEvent.click(screen.getByText("Claude Sonnet"));
    const badges = screen.getAllByText("free");
    expect(badges.length).toBeGreaterThan(0);
  });

  it("shows error indicator when listModels fails", async () => {
    vi.mocked(workspacesApi.listModels).mockRejectedValue(new Error("503"));
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    // retry:1 in component means it takes 2 rejections before error state
    await waitFor(() => expect(screen.getByTitle("Could not load models")).toBeInTheDocument(), { timeout: 3000 });
  });

  it("shows toast when model saved with applied:false", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: false });
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    // open dropdown
    fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
    fireEvent.click(screen.getByText("GPT-4o"));
    await waitFor(() => expect(screen.getByText(/takes effect/)).toBeInTheDocument());
  });

  it("shows error toast when setModel fails", async () => {
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    vi.mocked(workspacesApi.setModel).mockRejectedValue(new Error("500"));
    render(<ModelSelector workspaceId="ws-1" />, { wrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));
    // open dropdown
    fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
    fireEvent.click(screen.getByText("GPT-4o"));
    await waitFor(() => expect(screen.getByText(/Failed to set model/)).toBeInTheDocument());
  });

  // Regression test for the "ModelSelector disappears after workspace activates" bug.
  // Root cause: invalidateQueries cleared the cache and models.length===0 during
  // the re-fetch window hit the old unconditional `return null`.
  // Fix: placeholderData:keepPreviousData keeps previous data visible during refetches.
  it("stays visible during background refetch (regression: disappear on invalidateQueries)", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const sharedWrapper = ({ children }: { children: React.ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    );

    // First fetch resolves — button appears.
    vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    render(<ModelSelector workspaceId="ws-1" />, { wrapper: sharedWrapper });
    await waitFor(() => screen.getByText("Claude Sonnet"));

    // Simulate invalidateQueries mid-flight: remove the query data from the cache
    // (React Query marks status as "loading" again) while a slow refetch is pending.
    let resolveRefetch!: (v: typeof mockModels) => void;
    vi.mocked(workspacesApi.listModels).mockReturnValueOnce(
      new Promise((res) => { resolveRefetch = res; }),
    );
    await act(async () => {
      qc.removeQueries({ queryKey: ["models", "ws-1"] });
      // Trigger a fresh fetch with the slow mock.
      void qc.fetchQuery({ queryKey: ["models", "ws-1"], queryFn: () => workspacesApi.listModels("ws-1") });
    });

    // While re-fetch is in-flight the button must still be present.
    // With the old code (no placeholderData) models===[] → return null → button gone.
    // With the fix (placeholderData:keepPreviousData) the prior data is kept while
    // fetching. The button stays visible.
    expect(screen.getByText("Claude Sonnet")).toBeInTheDocument();

    // Resolve the pending refetch — button still there.
    await act(async () => { resolveRefetch(mockModels); });
    await waitFor(() => expect(screen.getByText("Claude Sonnet")).toBeInTheDocument());
  });

  // --- US-9.16: preferredModel seeding ---

  describe("US-9.16 preferredModel seeding", () => {
    const noCurrent = {
      models: [
        { id: "anthropic/claude-sonnet-4-5", providerID: "anthropic", name: "Claude Sonnet", tier: "paid", freeTier: false, selected: false, enabled: true },
        { id: "openai/gpt-4o", providerID: "openai", name: "GPT-4o", tier: "paid", freeTier: false, selected: false, enabled: true },
      ],
      currentModel: "",
    };

    it("seeds from preferredModel when currentModel is empty and preferred is available", async () => {
      vi.mocked(workspacesApi.listModels).mockResolvedValue(noCurrent);
      vi.mocked(useUserSetting).mockReturnValue("openai/gpt-4o");
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: true });

      render(<ModelSelector workspaceId="ws-new" />, { wrapper });
      await waitFor(() => expect(workspacesApi.setModel).toHaveBeenCalledWith("ws-new", "openai/gpt-4o"));
    });

    it("does NOT seed when currentModel is already set (server value wins)", async () => {
      vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels); // currentModel: claude
      vi.mocked(useUserSetting).mockReturnValue("openai/gpt-4o");
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: true });

      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      // Give any stray effect a chance to fire
      await waitFor(() => expect(workspacesApi.setModel).not.toHaveBeenCalled(), { timeout: 500 });
    });

    it("does NOT seed when preferredModel is set but not in available models", async () => {
      vi.mocked(workspacesApi.listModels).mockResolvedValue(noCurrent);
      vi.mocked(useUserSetting).mockReturnValue("google/gemini-pro");
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "google/gemini-pro", applied: true });

      render(<ModelSelector workspaceId="ws-new" />, { wrapper });
      await waitFor(() => screen.getByText("Select model"));
      await waitFor(() => expect(workspacesApi.setModel).not.toHaveBeenCalled(), { timeout: 500 });
    });

    it("does NOT seed when preferredModel is empty", async () => {
      vi.mocked(workspacesApi.listModels).mockResolvedValue(noCurrent);
      vi.mocked(useUserSetting).mockReturnValue("");
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "x", applied: true });

      render(<ModelSelector workspaceId="ws-new" />, { wrapper });
      await waitFor(() => screen.getByText("Select model"));
      await waitFor(() => expect(workspacesApi.setModel).not.toHaveBeenCalled(), { timeout: 500 });
    });

    it("does NOT loop: seed runs at most once per workspace", async () => {
      vi.mocked(workspacesApi.listModels).mockResolvedValue(noCurrent);
      vi.mocked(useUserSetting).mockReturnValue("openai/gpt-4o");
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: true });

      render(<ModelSelector workspaceId="ws-once" />, { wrapper });
      await waitFor(() => expect(workspacesApi.setModel).toHaveBeenCalledTimes(1));
      // Wait another tick to ensure no duplicate call
      await new Promise((r) => setTimeout(r, 50));
      expect(workspacesApi.setModel).toHaveBeenCalledTimes(1);
    });
  });

  // --- Viewport-aware positioning ---
  // Regression: the composer drawer (US-67.5) moved the selector to the
  // bottom of the screen; a dropdown that opens downward renders off-screen.
  // These tests prove the dropdown and the transient toast are portaled,
  // fixed-positioned, and flip/clamp via computeMenuPosition.

  describe("viewport-aware positioning", () => {
    beforeEach(() => {
      vi.clearAllMocks();
      vi.mocked(useUserSetting).mockReturnValue("");
      vi.mocked(workspacesApi.listModels).mockResolvedValue(mockModels);
    });
    afterEach(restoreMenuGeometry);

    async function openDropdown() {
      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
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
      mockMenuSize(150, 256);
      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      mockButtonRect(screen.getByRole("button", { name: /Claude Sonnet/i }), {
        top: 580, bottom: 600, left: 300, right: 460,
      });
      fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
      const menu = screen.getByRole("menu");
      // 580 - 150 - 4 = 426 (flipped above), NOT 604 (below, off-screen).
      expect(menu.style.top).toBe("426px");
    });

    it("clamps left when the dropdown would overflow the right edge", async () => {
      mockViewport(800, 800);
      mockMenuSize(150, 256);
      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      mockButtonRect(screen.getByRole("button", { name: /Claude Sonnet/i }), {
        top: 100, bottom: 124, left: 780, right: 800,
      });
      fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
      const menu = screen.getByRole("menu");
      // Right-aligned left = 800-256 = 544; 544+256 = 800 > 800-8 → clamp to 536.
      expect(menu.style.left).toBe("536px");
    });

    it("caps dropdown height when it is taller than the viewport room", async () => {
      mockViewport(300);
      mockMenuSize(400, 256);
      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      mockButtonRect(screen.getByRole("button", { name: /Claude Sonnet/i }), {
        top: 280, bottom: 290, left: 300, right: 460,
      });
      fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
      const menu = screen.getByRole("menu");
      // Above has more room: top clamps to pad 8, maxHeight = 280-4-8 = 268.
      expect(menu.style.top).toBe("8px");
      expect(menu.style.maxHeight).toBe("268px");
    });

    it("renders the toast fixed and flipped above near the viewport bottom", async () => {
      mockViewport(600);
      mockMenuSize(40, 224);
      vi.mocked(workspacesApi.setModel).mockResolvedValue({ model: "openai/gpt-4o", applied: false });
      render(<ModelSelector workspaceId="ws-1" />, { wrapper });
      await waitFor(() => screen.getByText("Claude Sonnet"));
      mockButtonRect(screen.getByRole("button", { name: /Claude Sonnet/i }), {
        top: 580, bottom: 600, left: 300, right: 460,
      });
      fireEvent.click(screen.getByRole("button", { name: /Claude Sonnet/i }));
      fireEvent.click(screen.getByText("GPT-4o"));
      const toast = await screen.findByText(/takes effect/);
      // Fixed-positioned (not statically dropped at the end of body) and
      // flipped above the trigger: 580 - 40 - 4 = 536, NOT below (off-screen).
      expect(toast.className).toContain("fixed");
      expect(toast.style.top).toBe("536px");
    });
  });
});
