import { describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AppShell } from "./AppShell";
import { AuthProvider } from "../../providers/AuthProvider";

vi.mock("../../api/auth", () => ({
  authApi: {
    me: vi.fn().mockResolvedValue({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }),
  },
}));

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
  },
}));

vi.mock("../../providers/SessionActivityProvider", () => ({
  useIsSessionAborted: () => false,
  useClearPendingUnread: () => () => {},
  useIsSessionBusy: () => false,
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => 0,
  useWorkspaceHung: () => false,
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

function setMobileMatchMedia() {
  return vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    matches: !query.includes("min-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  } as unknown as MediaQueryList));
}

// ── #1648 pin: the app shell box must fit the VISUAL viewport ──────────
//
// jsdom cannot run Tailwind or lay out flexbox, so "effective height"
// cannot be measured with getBoundingClientRect. The pin instead does
// what a mobile browser does when it resolves the shell root's height
// utility, against a viewport model that encodes the #1648 regression:
//
//   - viewport-fit=cover + safe-area insets INSIDE the shell (#1626),
//     so the mobile header row (pt-[calc(...+env(safe-area-inset-top))])
//     is present in the rendered tree;
//   - browser chrome shown, so the LARGE viewport (what 100vh resolves
//     to) is TALLER than the visual viewport (what 100dvh resolves to,
//     and what window.innerHeight reports).
//
// h-screen resolves to the large viewport → shell box taller than the
// visible screen → with overflow-hidden one end (header or composer) is
// permanently clipped. h-dvh resolves to the visual viewport → fits.
// Red at f9a74a19 (h-screen); green with the dvh fix.

// iPhone Safari-class geometry while browser chrome is shown: ~120px
// URL-bar/toolbar band between the visual and large viewports.
const CHROME_BAND_PX = 120;
const VISUAL_VIEWPORT_PX = 660;
const LARGE_VIEWPORT_PX = VISUAL_VIEWPORT_PX + CHROME_BAND_PX;

function mockMobileVisualViewport() {
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    value: VISUAL_VIEWPORT_PX,
    writable: true,
  });
}

// Resolve a viewport-percentage height utility the way a mobile browser
// does with chrome shown. Returns the px height the shell box would get,
// or null when no full-viewport height utility is present at all (which
// would also break the fixed shell layout).
function resolveViewportHeight(className: string): number | null {
  // Large-viewport units: resolve taller than the visible screen.
  if (/(^|\s)h-screen(\s|$)/.test(className)) return LARGE_VIEWPORT_PX;
  if (/(^|\s)h-\[100vh\](\s|$)/.test(className) || /(^|\s)h-\[100lvh\](\s|$)/.test(className)) return LARGE_VIEWPORT_PX;
  if (/(^|\s)h-lvh(\s|$)/.test(className)) return LARGE_VIEWPORT_PX;
  // Dynamic/small-viewport units: track the visual viewport.
  if (/(^|\s)h-dvh(\s|$)/.test(className)) return VISUAL_VIEWPORT_PX;
  if (/(^|\s)h-\[100dvh\](\s|$)/.test(className)) return VISUAL_VIEWPORT_PX;
  if (/(^|\s)h-svh(\s|$)/.test(className)) return VISUAL_VIEWPORT_PX;
  return null;
}

function renderShell() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(
    [{
      path: "/",
      element: <AppShell />,
      children: [{ path: "chat", element: <div>Chat</div> }],
    }],
    { initialEntries: ["/chat"] },
  );
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <RouterProvider router={router} />
      </AuthProvider>
    </QueryClientProvider>,
  );
}

function shellRoot(): HTMLElement {
  // overscroll-none is unique to the shell roots (AppShell/PortalLayout),
  // and the selector is deliberately independent of the height utility in
  // force so the pin keeps working across the fix.
  const el = document.querySelector("div.overscroll-none");
  if (!el) throw new Error("app shell root (div.overscroll-none) not rendered");
  return el as HTMLElement;
}

describe("AppShell mobile viewport fit (#1648)", () => {
  it("shell box resolves to the visual viewport, not the large viewport", async () => {
    const spy = setMobileMatchMedia();
    mockMobileVisualViewport();
    renderShell();

    // Regression preconditions actually hold: the mobile header row is
    // rendered with the safe-area inset composed INSIDE the shell (#1626).
    const header = document.querySelector(".border-b.px-3");
    expect(header).not.toBeNull();
    expect(header!.className).toMatch(/pt-\[calc\(0\.5rem\+env\(safe-area-inset-top,0px\)\)\]/);

    const root = shellRoot();
    const resolved = resolveViewportHeight(root.className);
    expect(resolved).not.toBeNull();
    expect(resolved).toBe(VISUAL_VIEWPORT_PX);
    spy.mockRestore();
  });

  it("shell box height never exceeds the visual viewport (window.innerHeight)", async () => {
    const spy = setMobileMatchMedia();
    mockMobileVisualViewport();
    renderShell();

    const resolved = resolveViewportHeight(shellRoot().className);
    expect(resolved).not.toBeNull();
    // The core #1648 contract: with browser chrome shown and safe-area
    // insets live inside the box, the box itself must fit the screen the
    // user can actually see. h-screen resolves to innerHeight + chrome
    // band → red; dvh resolves to innerHeight → green.
    expect(resolved!).toBeLessThanOrEqual(window.innerHeight);
    spy.mockRestore();
  });

  it("keeps the shell a fixed-height clipping context (overflow-hidden, not min-height)", async () => {
    const spy = setMobileMatchMedia();
    mockMobileVisualViewport();
    renderShell();

    const cls = shellRoot().className;
    // The shell must stay a height-constrained clipping context: a
    // min-height shell would let the composer scroll out of view instead
    // of pinning header+content+composer into one visible column.
    expect(cls).toMatch(/(^|\s)overflow-hidden(\s|$)/);
    expect(cls).not.toMatch(/min-h-/);
    spy.mockRestore();
  });

  it("box interior covers the nonzero safe-area insets and still leaves content room", async () => {
    const spy = setMobileMatchMedia();
    mockMobileVisualViewport();
    renderShell();

    // Why NOT "box + insets ≤ innerHeight" (the issue's shorthand): the
    // #1626 insets are interior paddings — paid FROM the box (header
    // pt-calc, Composer pb-calc), never added to it. The literal sum
    // would demand box ≤ innerHeight − insets, i.e. a shell SHORTER than
    // the screen, which is neither what the fix does nor what "fit the
    // visible screen" means under viewport-fit=cover (the inset bands are
    // inside innerHeight). The coherent invariant: the box fits the
    // visual viewport (previous test) AND the box's interior can absorb
    // both inset bands while leaving usable content space.
    const INSET_TOP = 47;    // notch band, nonzero per the regression
    const INSET_BOTTOM = 34; // home-indicator band
    const MIN_USABLE_CONTENT = 200;
    const box = resolveViewportHeight(shellRoot().className)!;
    expect(box).toBeGreaterThanOrEqual(INSET_TOP + INSET_BOTTOM + MIN_USABLE_CONTENT);
    // And the insets are genuinely nonzero in the rendered tree — the
    // header composes the top inset (#1623 pin covers the full contract).
    const header = document.querySelector(".border-b.px-3") as HTMLElement | null;
    expect(header?.className).toMatch(/env\(safe-area-inset-top/);
    spy.mockRestore();
  });
});

describe("AppShell keyboard guard wiring (#1648 criterion 4)", () => {
  it("mounts useOskViewportGuard: a visual-viewport shrink activates the override on <html>", async () => {
    const spy = setMobileMatchMedia();
    mockMobileVisualViewport();
    // jsdom has no visualViewport; install one where the keyboard is open
    // (visual 400 ≪ layout 660) and pump the resize the hook listens to.
    const listeners = new Set<() => void>();
    const state = { height: 660, offsetTop: 0 };
    Object.defineProperty(window, "visualViewport", {
      configurable: true,
      value: {
        get height() { return state.height; },
        get offsetTop() { return state.offsetTop; },
        addEventListener: (_: string, fn: () => void) => listeners.add(fn),
        removeEventListener: (_: string, fn: () => void) => listeners.delete(fn),
      },
      writable: true,
    });

    renderShell();
    act(() => { state.height = 400; listeners.forEach((fn) => fn()); });

    // The [data-osk-open] rule in index.css switches .h-dvh shells to
    // this tracked height while the keyboard heuristic holds.
    expect(document.documentElement.dataset.oskOpen).toBe("true");
    expect(document.documentElement.style.getPropertyValue("--shell-visual-height")).toBe("400px");

    delete (window as { visualViewport?: unknown }).visualViewport;
    delete document.documentElement.dataset.oskOpen;
    document.documentElement.style.removeProperty("--shell-visual-height");
    spy.mockRestore();
  });
});
