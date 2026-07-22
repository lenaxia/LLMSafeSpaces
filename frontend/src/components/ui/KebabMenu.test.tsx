import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { KebabMenu, computeMenuPosition } from "./KebabMenu";

describe("KebabMenu", () => {
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

  it("keeps opening below when spaceBelow >= spaceAbove even if neither fits fully", () => {
    // Both sides smaller than menu, but below is larger.
    const btn = { top: 420, bottom: 440, left: 0, right: 40 };
    // spaceBelow = 800-440-4 = 356; spaceAbove = 420-4 = 416. Above is larger → pick above, clamp.
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
