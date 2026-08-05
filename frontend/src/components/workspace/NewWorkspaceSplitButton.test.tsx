import { describe, expect, it, vi, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { NewWorkspaceSplitButton } from "./NewWorkspaceSplitButton";

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    create: vi.fn().mockResolvedValue({ id: "ws-new" }),
  },
}));

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    listConfigs: vi.fn().mockResolvedValue({
      configs: [
        { id: "c1", hash: "s-ready1", name: "Python+Node", status: "ready", selection: ["python-3.12", "node-22"], scope: "member" },
        { id: "c2", hash: "s-building1", name: "Building Image", status: "building", selection: ["go-1.24"], scope: "member" },
      ],
    }),
  },
}));

// --- jsdom geometry mocking (mirrors KebabMenu.test.tsx) ---
// jsdom doesn't compute layout, so getBoundingClientRect/offsetHeight/scrollHeight
// return zeros. These mocks let us assert the component applies viewport-aware
// positioning from computeMenuPosition.

let origScrollHeight: PropertyDescriptor | undefined;
let origOffsetWidth: PropertyDescriptor | undefined;
let origInnerHeight: PropertyDescriptor | undefined;

function mockButtonRect(el: HTMLElement, rect: { top: number; bottom: number; left: number; right: number }) {
  Object.defineProperty(el, "getBoundingClientRect", { configurable: true, value: () => rect });
}

function mockMenuSize(height: number, width = 240) {
  origScrollHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  origOffsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", { configurable: true, get: () => height });
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => width });
}

function mockInnerHeight(h: number) {
  origInnerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  Object.defineProperty(window, "innerHeight", { configurable: true, value: h });
}

afterEach(() => {
  if (origScrollHeight) Object.defineProperty(HTMLElement.prototype, "scrollHeight", origScrollHeight);
  if (origOffsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", origOffsetWidth);
  if (origInnerHeight) Object.defineProperty(window, "innerHeight", origInnerHeight);
});

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
    mockInnerHeight(600);
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
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 800 });
    await user.click(screen.getByLabelText("Select workspace image"));
    const menu = await screen.findByRole("menu");
    // Clamped: 800 - 8(pad) - 240 = 552. NOT 560 (790-240, which overflows).
    expect(Number(menu.style.left.replace("px", ""))).toBeLessThanOrEqual(552);
  });

  it("applies maxHeight when the menu is taller than the viewport room", async () => {
    const user = userEvent.setup();
    mockInnerHeight(300);
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
});
