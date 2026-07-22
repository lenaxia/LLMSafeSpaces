import { afterEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { KebabMenu, computeMenuPosition } from "./KebabMenu";

describe("KebabMenu", () => {
  let origOffsetHeight: PropertyDescriptor | undefined;
  let origOffsetWidth: PropertyDescriptor | undefined;
  let origInnerHeight: PropertyDescriptor | undefined;

  afterEach(() => {
    const proto = HTMLElement.prototype as unknown as Record<string, unknown>;
    if (origOffsetHeight) Object.defineProperty(HTMLElement.prototype, "offsetHeight", origOffsetHeight);
    else delete proto.offsetHeight;
    if (origOffsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", origOffsetWidth);
    else delete proto.offsetWidth;
    if (origInnerHeight) Object.defineProperty(window, "innerHeight", origInnerHeight);
    origOffsetHeight = origOffsetWidth = origInnerHeight = undefined;
  });
  it("renders trigger button", () => {
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} />);
    expect(screen.getByLabelText("Actions")).toBeInTheDocument();
  });

  it("shows menu items when clicked", async () => {
    const user = userEvent.setup();
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} />);
    await user.click(screen.getByLabelText("Actions"));
    expect(screen.getByRole("menuitem", { name: "Action" })).toBeInTheDocument();
  });

  it("calls onClick when menu item is clicked", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<KebabMenu items={[{ label: "Action", onClick }]} />);
    await user.click(screen.getByLabelText("Actions"));
    await user.click(screen.getByRole("menuitem", { name: "Action" }));
    expect(onClick).toHaveBeenCalled();
  });

  it("closes menu after item is clicked", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<KebabMenu items={[{ label: "Action", onClick }]} />);
    await user.click(screen.getByLabelText("Actions"));
    await user.click(screen.getByRole("menuitem", { name: "Action" }));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("applies destructive style", async () => {
    const user = userEvent.setup();
    render(<KebabMenu items={[{ label: "Delete", onClick: vi.fn(), destructive: true }]} />);
    await user.click(screen.getByLabelText("Actions"));
    const item = screen.getByRole("menuitem", { name: "Delete" });
    expect(item.className).toContain("text-destructive");
  });

  it("disables menu item", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<KebabMenu items={[{ label: "Action", onClick, disabled: true }]} />);
    await user.click(screen.getByLabelText("Actions"));
    const item = screen.getByRole("menuitem", { name: "Action" });
    expect(item).toBeDisabled();
  });

  it("closes menu when clicking outside", async () => {
    const user = userEvent.setup();
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} />);
    await user.click(screen.getByLabelText("Actions"));
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.click(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  // --- Viewport-aware positioning (integration: component wiring) ---
  //
  // jsdom doesn't compute layout, so getBoundingClientRect/offsetHeight
  // return zeros. These tests mock the geometry on the rendered elements
  // and assert the menu's computed style reflects the pure function's
  // flip/clamp/maxHeight output — proving the wiring from DOM measurement
  // through computeMenuPosition to the applied style.

  function mockButtonRect(el: HTMLElement, rect: { top: number; bottom: number; left: number; right: number }) {
    Object.defineProperty(el, "getBoundingClientRect", { configurable: true, value: () => rect });
  }

  // offsetHeight/offsetWidth are read on the menu element (menuRef.current),
  // which only exists after open. Override the prototype so the value is in
  // place before the layout effect runs on the first open render. Originals
  // are restored in afterEach.
  function mockMenuSize(height: number, width = 160) {
    origOffsetHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
    origOffsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth");
    Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, get: () => height });
    Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, get: () => width });
  }

  function mockInnerHeight(h: number) {
    origInnerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
    Object.defineProperty(window, "innerHeight", { configurable: true, value: h });
  }

  it("applies flipped-above top when the trigger is near the viewport bottom", async () => {
    const user = userEvent.setup();
    mockInnerHeight(600);
    mockMenuSize(150);
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} align="left" />);
    mockButtonRect(screen.getByLabelText("Actions"), { top: 580, bottom: 600, left: 50, right: 90 });
    await user.click(screen.getByLabelText("Actions"));
    const menu = screen.getByRole("menu");
    // 580 - 150 - 4 = 426 (flipped above), NOT 604 (below, which overflows).
    expect(menu.style.top).toBe("426px");
  });

  it("applies default below top when there is room", async () => {
    const user = userEvent.setup();
    mockInnerHeight(800);
    mockMenuSize(150);
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} align="left" />);
    mockButtonRect(screen.getByLabelText("Actions"), { top: 100, bottom: 124, left: 50, right: 90 });
    await user.click(screen.getByLabelText("Actions"));
    const menu = screen.getByRole("menu");
    expect(menu.style.top).toBe("128px"); // 124 + 4
  });

  it("applies maxHeight when the menu is taller than the viewport room", async () => {
    const user = userEvent.setup();
    mockInnerHeight(300);
    mockMenuSize(400);
    render(<KebabMenu items={[{ label: "Action", onClick: vi.fn() }]} align="left" />);
    mockButtonRect(screen.getByLabelText("Actions"), { top: 280, bottom: 290, left: 50, right: 90 });
    await user.click(screen.getByLabelText("Actions"));
    const menu = screen.getByRole("menu");
    // Tall menu near the bottom → height is capped to the available room.
    expect(menu.style.maxHeight).not.toBe("");
    expect(Number(menu.style.maxHeight.replace("px", ""))).toBeGreaterThan(0);
  });

  // --- Section grouping ---

  it("renders a labelled divider for a section header", async () => {
    const user = userEvent.setup();
    render(
      <KebabMenu
        items={[
          { label: "Rename", onClick: vi.fn() },
          { label: "Suspend", onClick: vi.fn(), section: "Lifecycle" },
        ]}
      />,
    );
    await user.click(screen.getByLabelText("Actions"));
    expect(screen.getByText("Lifecycle")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Suspend" })).toBeInTheDocument();
  });

  it("does not render a section header when no item declares a section", async () => {
    const user = userEvent.setup();
    render(
      <KebabMenu
        items={[
          { label: "Rename", onClick: vi.fn() },
          { label: "Delete", onClick: vi.fn(), destructive: true },
        ]}
      />,
    );
    await user.click(screen.getByLabelText("Actions"));
    // Legacy two-phase layout: no section header text present.
    expect(screen.queryByText("Lifecycle")).not.toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Rename" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Delete" })).toBeInTheDocument();
  });

  it("keeps destructive styling within a section", async () => {
    const user = userEvent.setup();
    render(
      <KebabMenu
        items={[
          { label: "Refresh compute", onClick: vi.fn(), section: "Lifecycle" },
          { label: "Delete", onClick: vi.fn(), section: "Lifecycle", destructive: true },
        ]}
      />,
    );
    await user.click(screen.getByLabelText("Actions"));
    const deleteItem = screen.getByRole("menuitem", { name: "Delete" });
    expect(deleteItem.className).toContain("text-destructive");
    expect(screen.getByRole("menuitem", { name: "Refresh compute" }).className).not.toContain(
      "text-destructive",
    );
  });

  it("renders a header on each section change (multi-section)", async () => {
    const user = userEvent.setup();
    render(
      <KebabMenu
        items={[
          { label: "Rename", onClick: vi.fn() },
          { label: "Suspend", onClick: vi.fn(), section: "Lifecycle" },
          { label: "Delete", onClick: vi.fn(), section: "Lifecycle", destructive: true },
          { label: "Export", onClick: vi.fn(), section: "Advanced" },
        ]}
      />,
    );
    await user.click(screen.getByLabelText("Actions"));
    // One header per named section; the unsectioned "Rename" has no header.
    const lifecycleHeaders = screen.getAllByText("Lifecycle");
    const advancedHeaders = screen.getAllByText("Advanced");
    expect(lifecycleHeaders).toHaveLength(1);
    expect(advancedHeaders).toHaveLength(1);
  });

  it("calls onClick and closes the menu for an item in sectioned mode", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(
      <KebabMenu
        items={[
          { label: "Rename", onClick: vi.fn() },
          { label: "Suspend", onClick, section: "Lifecycle" },
        ]}
      />,
    );
    await user.click(screen.getByLabelText("Actions"));
    await user.click(screen.getByRole("menuitem", { name: "Suspend" }));
    expect(onClick).toHaveBeenCalledOnce();
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});

// ── Viewport-aware positioning ──────────────────────────────────────────
//
// Regression coverage for the kebab menu that opened off the bottom of the
// screen when triggered from the last sidebar item. The positioning logic
// is extracted into a pure function so it can be tested deterministically
// without jsdom layout (which doesn't compute geometry).

describe("computeMenuPosition (viewport-aware)", () => {
  const VW = 1280;
  const VH = 800;
  const MENU_W = 160;
  const MENU_H = 150;

  it("opens below the button when there is room (default)", () => {
    const btn = { top: 100, bottom: 124, left: 50, right: 90 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    expect(r.top).toBe(128); // btn.bottom + 4
    expect(r.left).toBe(50);
    expect(r.maxHeight).toBeUndefined();
  });

  it("flips above when not enough room below but enough above", () => {
    // Button near the bottom: only 20px below, plenty above.
    const btn = { top: 700, bottom: 724, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    // spaceBelow = 800-724-4 = 72 (< 150); spaceAbove = 700-4 = 696 (>= 150) → flip above.
    expect(r.top).toBe(700 - MENU_H - 4); // 546
    expect(r.maxHeight).toBeUndefined();
  });

  it("opens above and clamps to PAD when above has more room and the menu is taller than both sides", () => {
    // Both sides smaller than menu; above is larger → pick above, clamp top to PAD.
    const btn = { top: 420, bottom: 440, left: 50, right: 90 };
    // spaceBelow = 800-440-4 = 356; spaceAbove = 420-4 = 416. Above is larger → clamp.
    const r = computeMenuPosition(btn, { width: MENU_W, height: 700 }, { width: VW, height: VH }, "left");
    expect(r.maxHeight).toBeDefined();
    expect(r.top).toBe(8); // clamped to PAD
  });

  it("caps height when the menu is taller than the viewport on the chosen side (open below)", () => {
    // Tiny space below, huge menu, below is the larger side.
    const btn = { top: 100, bottom: 120, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: 900 }, { width: VW, height: VH }, "left");
    // spaceBelow = 800-120-4 = 676 (larger than spaceAbove=96) → below, capped.
    expect(r.top).toBe(124);
    expect(r.maxHeight).toBe(800 - 8 - 124); // vh - PAD - top
  });

  it("clamps left so a left-aligned menu near the right edge stays on screen", () => {
    const btn = { top: 100, bottom: 124, left: 1200, right: 1240 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    // left would be 1200, but 1200+160 = 1360 > 1280-8 → clamp to 1280-8-160 = 1112.
    expect(r.left).toBe(VW - 8 - MENU_W);
  });

  it("clamps left for a right-aligned menu that would overflow", () => {
    const btn = { top: 100, bottom: 124, left: 1260, right: 1280 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "right");
    // right-aligned: left = 1280-160 = 1120; 1120+160 = 1280 <= 1272? no, > 1272 → clamp.
    expect(r.left).toBe(VW - 8 - MENU_W);
  });

  it("never returns a negative left", () => {
    const btn = { top: 100, bottom: 124, left: -50, right: -10 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    expect(r.left).toBe(8); // PAD
  });

  it("never returns a top above the PAD margin", () => {
    // Button near the bottom; huge menu. Above is the larger side, so the
    // menu opens upward and its top clamps to PAD.
    const btn = { top: 780, bottom: 790, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: 900 }, { width: VW, height: VH }, "left");
    expect(r.top).toBe(8); // clamped to PAD
    expect(r.maxHeight).toBe(780 - 4 - 8);
  });

  it("respects the right align anchor when it fits", () => {
    const btn = { top: 100, bottom: 124, left: 100, right: 260 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "right");
    expect(r.left).toBe(260 - MENU_W); // right edges align
  });
});
