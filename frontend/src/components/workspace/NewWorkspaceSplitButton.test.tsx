import { describe, expect, it, vi, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { NewWorkspaceSplitButton } from "./NewWorkspaceSplitButton";
import { mockButtonRect, mockMenuSize, mockViewport, restoreMenuGeometry } from "../../test/menuGeometry";

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    create: vi.fn().mockResolvedValue({ id: "ws-new" }),
  },
}));

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    listConfigs: vi.fn().mockResolvedValue([
        { id: "c1", hash: "s-ready1", name: "Python+Node", status: "ready", selection: ["python-3.12", "node-22"], scope: "member" },
        { id: "c2", hash: "s-building1", name: "Building Image", status: "building", selection: ["go-1.24"], scope: "member" },
      ]),
  },
}));

// --- jsdom geometry mocking (shared: src/test/menuGeometry.ts) ---
// jsdom doesn't compute layout, so getBoundingClientRect/offsetHeight/scrollHeight
// return zeros. These mocks let us assert the component applies viewport-aware
// positioning from computeMenuPosition.

afterEach(restoreMenuGeometry);

describe("NewWorkspaceSplitButton", () => {
  it("renders + and arrow buttons", () => {
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);
    expect(screen.getByLabelText("New workspace (default image)")).toBeInTheDocument();
    expect(screen.getByLabelText("Select workspace image")).toBeInTheDocument();
  });

  it("launches default on + click", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("New workspace (default image)"));
    expect(onCreated).toHaveBeenCalledWith("ws-new");
  });

  it("shows the update pill on stale ready configs in the launch picker (#928)", async () => {
    // Re-mock listConfigs for this test: one stale ready config
    const { imageFactoryApi } = await import("../../api/imageFactory");
    vi.mocked(imageFactoryApi.listConfigs).mockResolvedValueOnce([
      {
        id: "c9", hash: "s-stale1", name: "Stale Stack", status: "ready", selection: ["ffmpeg"], scope: "member",
        updatesAvailable: {
          kind: "base_migration",
          currentBaseName: "bookworm",
          currentBaseVersion: "0.6.0",
          latestBaseVersion: "0.9.0",
          defaultBaseName: "trixie",
          defaultBaseVersion: "0.1.0",
        },
      },
    ] as never);
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    expect(await screen.findByText("Stale Stack")).toBeInTheDocument();
    const pill = screen.getByTitle(/New base available: trixie/i);
    expect(pill).toBeInTheDocument();
    expect(pill.textContent).toBe("↻");
    // Fresh ready configs from prior tests are absent (mockResolvedValueOnce)
  });

  it("opens popup showing ready + building configs with pills", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));

    // Ready config with green pill
    expect(await screen.findByText("Python+Node")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();

    // Building config with yellow pill
    expect(screen.getByText("Building Image")).toBeInTheDocument();
    // "Building" appears in both the section header and the pill
    expect(screen.getAllByText("Building").length).toBeGreaterThanOrEqual(1);
  });

  it("launches workspace when ready config clicked", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const readyConfig = await screen.findByText("Python+Node");
    await user.click(readyConfig);

    expect(onCreated).toHaveBeenCalledWith("ws-new");
  });

  it("building configs are not clickable", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const buildingConfig = await screen.findByText("Building Image");
    await user.click(buildingConfig);

    // Building config click should NOT trigger creation
    expect(onCreated).not.toHaveBeenCalled();
  });

  // --- Regression: viewport-aware positioning (#652) ---
  // Pre-fix the popup was hardcoded to absolute right-0 top-full and
  // overflowed the screen edge. These tests prove the popup is now portaled
  // and positioned via computeMenuPosition (flip/clamp/maxHeight).

  it("portals the popup to document.body (escapes ancestor clipping)", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const menu = await screen.findByRole("menu");

    // The portaled menu is a direct child of document.body, not nested
    // inside the component's relative container.
    expect(menu.parentElement).toBe(document.body);
    expect(menu.className).toContain("fixed");
  });

  it("flips above when the trigger is near the viewport bottom", async () => {
    const user = userEvent.setup();
    mockViewport(600);
    mockMenuSize(150);
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);
    mockButtonRect(screen.getByLabelText("Select workspace image"), {
      top: 580, bottom: 600, left: 300, right: 320,
    });
    await user.click(screen.getByLabelText("Select workspace image"));
    const menu = await screen.findByRole("menu");
    // 580 - 150 - 4 = 426 (flipped above), NOT 604 (below, which overflows).
    expect(menu.style.top).toBe("426px");
  });

  it("clamps left when the menu would overflow the right edge", async () => {
    const user = userEvent.setup();
    mockMenuSize(150, 240);
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);
    // Trigger near the right edge: right=780, viewport width=800.
    // Left would be 780-240=540, which fits. But push it further: right=790.
    // 790-240=550, +240=790 < 800-8=792, still fits. Push to right=795:
    // 795-240=555, +240=795 > 792 → clamp to 792-240=552.
    mockButtonRect(screen.getByLabelText("Select workspace image"), {
      top: 100, bottom: 124, left: 780, right: 800,
    });
    // Override innerWidth for the clamp calculation.
    mockViewport(800, 800);
    await user.click(screen.getByLabelText("Select workspace image"));
    const menu = await screen.findByRole("menu");
    // Clamped: 800 - 8(pad) - 240 = 552. NOT 560 (790-240, which overflows).
    expect(Number(menu.style.left.replace("px", ""))).toBeLessThanOrEqual(552);
  });

  it("applies maxHeight when the menu is taller than the viewport room", async () => {
    const user = userEvent.setup();
    mockViewport(300);
    mockMenuSize(400);
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);
    mockButtonRect(screen.getByLabelText("Select workspace image"), {
      top: 280, bottom: 290, left: 300, right: 320,
    });
    await user.click(screen.getByLabelText("Select workspace image"));
    const menu = await screen.findByRole("menu");
    // Tall menu near the bottom → height is capped.
    expect(menu.style.maxHeight).not.toBe("");
    expect(Number(menu.style.maxHeight.replace("px", ""))).toBeGreaterThan(0);
  });

  it("closes the popup when clicking outside", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    expect(await screen.findByRole("menu")).toBeInTheDocument();

    await user.click(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  // --- Creation-error notice: edge-aware, anchored to the control ---
  // Regression: the error div used to be absolute-positioned below the
  // control (off-screen when the control is near the bottom edge), and a
  // broken intermediate version rendered it fixed at the viewport top-left
  // because it reused the popup's unmeasured position state.

  it("renders creation errors fixed and anchored to the control, flipping above near the bottom edge", async () => {
    const { workspacesApi } = await import("../../api/workspaces");
    vi.mocked(workspacesApi.create).mockRejectedValueOnce(new Error("quota exceeded"));
    mockViewport(600);
    mockMenuSize(40, 224);
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    // Anchor the measurement to the control container (parent of the + button).
    mockButtonRect(screen.getByLabelText("New workspace (default image)").parentElement!, {
      top: 540, bottom: 560, left: 300, right: 460,
    });
    await user.click(screen.getByLabelText("New workspace (default image)"));
    const error = await screen.findByText("quota exceeded");
    expect(error.className).toContain("fixed");
    // Flipped above: 540 - 40 - 4 = 496. NOT below (560+4, off-screen) and
    // NOT 0 (the unmeasured top-left regression).
    expect(error.style.top).toBe("496px");
    // Right-aligned to the control: 460 - 224 = 236.
    expect(error.style.left).toBe("236px");
  });

  it("renders creation errors below the control when there is room", async () => {
    const { workspacesApi } = await import("../../api/workspaces");
    vi.mocked(workspacesApi.create).mockRejectedValueOnce(new Error("quota exceeded"));
    mockViewport(800);
    mockMenuSize(40, 224);
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    mockButtonRect(screen.getByLabelText("New workspace (default image)").parentElement!, {
      top: 100, bottom: 124, left: 300, right: 460,
    });
    await user.click(screen.getByLabelText("New workspace (default image)"));
    const error = await screen.findByText("quota exceeded");
    // Default placement below: 124 + 4 = 128.
    expect(error.style.top).toBe("128px");
  });
});
