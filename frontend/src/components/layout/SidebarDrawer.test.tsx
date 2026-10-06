import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderBare as render } from "../../test/utils";
import { SidebarDrawer } from "./SidebarDrawer";
import type { CollapsibleSidebarState } from "../../hooks/useCollapsibleSidebar";

function overlayEl(container: HTMLElement): HTMLElement {
  // inset-0 (full-screen scrim) distinguishes the overlay from the
  // #1623 drag-handle strip (inset-y-0), which is also aria-hidden.
  const el = container.querySelector("div.inset-0");
  if (!el) throw new Error("overlay not rendered");
  return el as HTMLElement;
}

function setMobileMatchMedia(isMobile: boolean) {
  vi.spyOn(window, "matchMedia").mockImplementation((query) => {
    const isMinWidthQuery = query.includes("min-width");
    return {
      matches: isMinWidthQuery ? !isMobile : false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    } as unknown as MediaQueryList;
  });
}

function makeState(overrides: Partial<CollapsibleSidebarState> = {}): CollapsibleSidebarState {
  const base: CollapsibleSidebarState = {
    isMobile: true,
    open: false,
    setOpen: vi.fn(),
    close: vi.fn(),
    containerRef: { current: null },
    sidebarRef: { current: null },
    overlayRef: { current: null },
    handleRef: { current: null },
    sidebarWidth: 256,
  };
  return { ...base, ...overrides };
}

describe("SidebarDrawer", () => {
  beforeEach(() => {
    setMobileMatchMedia(true);
  });

  // ── #1623: the inset drag-handle affordance ──────────────────────────

  it("renders the drag-handle strip when mobile and closed", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: false })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const handle = container.querySelector("div.touch-none");
    expect(handle).not.toBeNull();
    expect(handle?.className).toContain("w-7");
  });

  it("positions the handle inside the safe-area inset (never at the absolute edge)", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: false })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const handle = container.querySelector("div.touch-none") as HTMLElement;
    // The positioning rides a Tailwind arbitrary-value class (jsdom's
    // CSSOM silently drops env() from inline styles).
    expect(handle.className).toContain("left-[calc(env(safe-area-inset-left,0px)+16px)]");
  });

  it("does not render the drag-handle when the sidebar is open (swipe-to-close lives on the drawer)", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: true })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    expect(container.querySelector("div.touch-none")).toBeNull();
  });

  it("does not render the drag-handle on desktop", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: false, open: false })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    expect(container.querySelector("div.touch-none")).toBeNull();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders its children", () => {
    render(
      <SidebarDrawer state={makeState()}>
        <nav data-testid="nav-content">nav</nav>
      </SidebarDrawer>,
    );
    expect(screen.getByTestId("nav-content")).toBeInTheDocument();
  });

  it("does not render an overlay on desktop", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: false })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    expect(container.querySelector('[aria-hidden="true"]')).toBeNull();
  });

  it("renders an overlay on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    expect(container.querySelector('[aria-hidden="true"]')).not.toBeNull();
  });

  it("hides overlay (pointer-events-none) when closed on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: false })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const overlay = overlayEl(container);
    expect(overlay.className).toContain("opacity-0");
    expect(overlay.className).toContain("pointer-events-none");
  });

  it("shows overlay (pointer-events-auto) when open on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: true })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const overlay = overlayEl(container);
    expect(overlay.className).toContain("opacity-100");
    expect(overlay.className).toContain("pointer-events-auto");
  });

  it("clicking the overlay calls setOpen(false)", async () => {
    const user = userEvent.setup();
    const setOpen = vi.fn();
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: true, setOpen })}>
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    await user.click(overlayEl(container));
    expect(setOpen).toHaveBeenCalledWith(false);
  });

  it("translates the drawer off-screen when closed on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: false })} ariaLabel="Sections">
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const drawer = container.querySelector('[aria-label="Sections"]') as HTMLElement;
    expect(drawer).not.toBeNull();
    expect(drawer.className).toContain("-translate-x-full");
    expect(drawer.className).toContain("fixed");
  });

  it("shows the drawer on-screen when open on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: true })} ariaLabel="Sections">
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const drawer = container.querySelector('[aria-label="Sections"]') as HTMLElement;
    expect(drawer.className).toContain("translate-x-0");
  });

  it("sets drawer width via inline style to match sidebarWidth on mobile", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: true, open: true, sidebarWidth: 192 })} ariaLabel="Sections">
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const drawer = container.querySelector('[aria-label="Sections"]') as HTMLElement;
    expect(drawer.style.width).toBe("192px");
  });

  it("uses relative positioning on desktop with default desktopClassName", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: false })} ariaLabel="Sections">
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const drawer = container.querySelector('[aria-label="Sections"]') as HTMLElement;
    expect(drawer.className).toContain("relative");
    expect(drawer.className).not.toContain("fixed");
  });

  it("honours a custom desktopClassName on desktop", () => {
    const { container } = render(
      <SidebarDrawer state={makeState({ isMobile: false })} ariaLabel="Sections" desktopClassName="relative w-48 shrink-0">
        <nav>nav</nav>
      </SidebarDrawer>,
    );
    const drawer = container.querySelector('[aria-label="Sections"]') as HTMLElement;
    expect(drawer.className).toContain("w-48");
    expect(drawer.className).toContain("shrink-0");
  });
});
