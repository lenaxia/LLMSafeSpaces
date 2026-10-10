import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { render } from "@testing-library/react";
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

function renderWithDataRouter(initialPath: string, childElement: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(
    [{
      path: "/",
      element: <AppShell />,
      children: [
        { path: "chat", element: childElement },
        { path: "chat/:workspaceId", element: childElement },
        { path: "chat/:workspaceId/:sessionId", element: childElement },
      ],
    }],
    { initialEntries: [initialPath] },
  );
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <RouterProvider router={router} />
      </AuthProvider>
    </QueryClientProvider>,
  );
}

function setDesktopMatchMedia() {
  return vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    matches: query.includes("min-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  } as unknown as MediaQueryList));
}

describe("AppShell", () => {
  it("renders sidebar and outlet", async () => {
    renderWithDataRouter("/chat", <div>Chat Content</div>);
    const titles = await screen.findAllByText("Safe Space");
    expect(titles.length).toBeGreaterThan(0);
    expect(screen.getByText("Chat Content")).toBeInTheDocument();
  });

  it("has skip-to-content link", async () => {
    renderWithDataRouter("/chat", <div>Content</div>);
    const skipLink = await screen.findByText("Skip to content");
    expect(skipLink).toHaveAttribute("href", "#main-content");
  });

  it("has main content landmark", async () => {
    renderWithDataRouter("/chat", <div>Content</div>);
    const main = await screen.findByRole("main");
    expect(main).toHaveAttribute("id", "main-content");
  });
});

describe("AppShell mobile drawer auto-open", () => {
  it("auto-opens the sidebar on initial mount when mobile and no session in URL", async () => {
    renderWithDataRouter("/chat", <div>Chat</div>);
    const toggle = await screen.findByRole("button", { name: "Close menu" });
    expect(toggle).toBeInTheDocument();
  });

  it("does not auto-open when a session is in the URL", async () => {
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    const toggle = await screen.findByRole("button", { name: "Open menu" });
    expect(toggle).toBeInTheDocument();
  });

  it("does not auto-open on desktop (no toggle rendered)", async () => {
    const spy = setDesktopMatchMedia();
    renderWithDataRouter("/chat", <div>Chat</div>);
    await screen.findByText("Chat");
    expect(screen.queryByRole("button", { name: /menu/i })).not.toBeInTheDocument();
    spy.mockRestore();
  });
});

// ── #1629 gesture integration: layouts → hook → drawer ────────────────
//
// The load-bearing seam (the #1626 r1 lesson: unwiring a ref left every
// unit test green). These drive REAL touch events through the full
// AppShell tree — the container listeners (attached via
// sidebar.containerRef on the h-dvh root div) and the drawer's open
// state — so removing the ref wiring, the EDGE_ZONE recognition, or the
// touchstart claim all fail HERE.

function setMobileMatchMedia() {
  return vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    matches: !query.includes("min-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  } as unknown as MediaQueryList));
}

function dispatchTouchOn(target: Element, type: string, touches: { clientX: number; clientY: number }[], changed?: typeof touches) {
  const mk = (list: typeof touches) =>
    list.map((t) => new Touch({ identifier: 0, target, clientX: t.clientX, clientY: t.clientY }));
  const event = new TouchEvent(type, {
    touches: mk(touches),
    changedTouches: mk(changed ?? touches),
    bubbles: true,
    cancelable: true,
  });
  target.dispatchEvent(event);
  return event;
}

describe("AppShell safe-area padding composition (#1623 r4)", () => {
  it("mobile header keeps its base padding and composes the safe-area inset additively", async () => {
    const spy = setMobileMatchMedia();
    renderWithDataRouter("/chat", <div>Chat</div>);
    const header = await screen.findByRole("banner", { hidden: true }).catch(() => null);
    const headerEl =
      header ?? document.querySelector(".border-b.px-3");
    expect(headerEl).not.toBeNull();
    const cls = (headerEl as HTMLElement).className;
    expect(cls).toContain("pb-2");
    expect(cls).toMatch(/pt-\[calc\(0\.5rem\+env\(safe-area-inset-top,0px\)\)\]/);
    expect(cls).not.toMatch(/(?<!calc\(0\.5rem\+)env\(safe-area-inset-top/);
    spy.mockRestore();
  });
});

describe("AppShell gesture integration (#1629)", () => {
  function gestureContainer(): HTMLElement {
    const el = document.querySelector(".h-dvh.overscroll-none");
    if (!el) throw new Error("gesture container (h-dvh root div) not rendered");
    return el as HTMLElement;
  }

  it("opens the drawer on a full-edge swipe through the full tree", async () => {
    const spy = setMobileMatchMedia();
    // A session in the URL keeps the drawer closed at mount.
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    expect(await screen.findByRole("button", { name: "Open menu" })).toBeInTheDocument();

    const container = gestureContainer();
    const startEvent = dispatchTouchOn(container, "touchstart", [{ clientX: 10, clientY: 300 }]);
    dispatchTouchOn(container, "touchmove", [{ clientX: 140, clientY: 300 }]);
    dispatchTouchOn(container, "touchend", [], [{ clientX: 140, clientY: 300 }]);

    expect(startEvent.defaultPrevented).toBe(true);
    expect(await screen.findByRole("button", { name: "Close menu" })).toBeInTheDocument();
    spy.mockRestore();
  });

  it("does not open on a swipe starting outside the edge zone, and never claims its touchstart", async () => {
    const spy = setMobileMatchMedia();
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    expect(await screen.findByRole("button", { name: "Open menu" })).toBeInTheDocument();

    const container = gestureContainer();
    const startEvent = dispatchTouchOn(container, "touchstart", [{ clientX: 60, clientY: 300 }]);
    dispatchTouchOn(container, "touchmove", [{ clientX: 160, clientY: 300 }]);
    dispatchTouchOn(container, "touchend", [], [{ clientX: 160, clientY: 300 }]);

    expect(startEvent.defaultPrevented).toBe(false);
    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
    spy.mockRestore();
  });

  it("never claims a touchstart that lands on the hamburger inside the edge band (click suppression guard, the #1626 r2 lesson)", async () => {
    const spy = setMobileMatchMedia();
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    const toggle = await screen.findByRole("button", { name: "Open menu" });

    // The button's left half is inside the 30px edge band.
    const startEvent = dispatchTouchOn(toggle, "touchstart", [{ clientX: 15, clientY: 300 }]);
    dispatchTouchOn(toggle, "touchmove", [{ clientX: 140, clientY: 300 }]);
    dispatchTouchOn(toggle, "touchend", [], [{ clientX: 140, clientY: 300 }]);

    expect(startEvent.defaultPrevented).toBe(false);
    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
    spy.mockRestore();
  });
});

describe("AppShell root gesture surface (#1629)", () => {
  // The CSS half of the fix: overscroll-behavior-x: none on the root
  // scroller (html/body — pinned in styles/index.test.ts) removes the
  // browser's swipe-back at the platform level; the container-level
  // overscroll-none + pan-y (shipped since 77850fc9/#117) is the inner
  // defense-in-depth layer. Pinned so it can't silently regress.
  it("keeps the container's overscroll containment and pan-y touch-action", () => {
    const spy = setMobileMatchMedia();
    renderWithDataRouter("/chat", <div>Chat</div>);
    const container = document.querySelector(".h-dvh.overscroll-none") as HTMLElement;
    expect(container).not.toBeNull();
    expect(container.style.touchAction).toBe("pan-y");
    spy.mockRestore();
  });
});
