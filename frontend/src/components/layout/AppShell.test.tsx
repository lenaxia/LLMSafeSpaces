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

// ── #1623 gesture integration: layouts → hook → drawer ────────────────
//
// The load-bearing seam (r1 review finding 1: deleting ref={handleRef}
// left every unit test green). These drive REAL touch events through the
// full AppShell tree — the container listeners, the rendered handle
// strip, and the drawer's open state — so unwiring the ref, moving the
// strip, or breaking the recognition logic all fail HERE.

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

function dispatchTouchOn(target: Element, type: string, touches: { clientX: number; clientY: number }[], changed?: typeof touches) {
  const mk = (list: typeof touches) =>
    list.map((t) => new Touch({ identifier: 0, target, clientX: t.clientX, clientY: t.clientY }));
  target.dispatchEvent(
    new TouchEvent(type, {
      touches: mk(touches),
      changedTouches: mk(changed ?? touches),
      bubbles: true,
      cancelable: true,
    }),
  );
}

describe("AppShell gesture integration (#1623)", () => {
  it("opens the drawer on a handle-zone swipe through the full tree", async () => {
    const spy = setMobileMatchMedia();
    // A session in the URL keeps the drawer closed at mount.
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    expect(await screen.findByRole("button", { name: "Open menu" })).toBeInTheDocument();

    const handle = document.querySelector("div.touch-none");
    if (!handle) throw new Error("the drag-handle strip must render when mobile and closed");
    // jsdom has no layout: pin the strip's rect at the shipped geometry
    // (28px wide, 16px in from the safe-area inset).
    vi.spyOn(handle as HTMLElement, "getBoundingClientRect").mockReturnValue({
      left: 16, top: 0, width: 28, height: 800, right: 44, bottom: 800, x: 16, y: 0,
    } as DOMRect);

    dispatchTouchOn(handle, "touchstart", [{ clientX: 30, clientY: 300 }]);
    dispatchTouchOn(handle, "touchmove", [{ clientX: 140, clientY: 300 }]);
    dispatchTouchOn(handle, "touchend", [], [{ clientX: 140, clientY: 300 }]);

    expect(await screen.findByRole("button", { name: "Close menu" })).toBeInTheDocument();
    spy.mockRestore();
  });

  it("does not open on an absolute-edge swipe through the full tree (the OS back-gesture zone)", async () => {
    const spy = setMobileMatchMedia();
    renderWithDataRouter("/chat/ws-1/sess-1", <div>Chat</div>);
    expect(await screen.findByRole("button", { name: "Open menu" })).toBeInTheDocument();

    const handle = document.querySelector("div.touch-none") as HTMLElement;
    vi.spyOn(handle, "getBoundingClientRect").mockReturnValue({
      left: 16, top: 0, width: 28, height: 800, right: 44, bottom: 800, x: 16, y: 0,
    } as DOMRect);

    // The touch target is the container surface at the absolute edge.
    const container = handle.closest("div")?.parentElement ?? document.body;
    dispatchTouchOn(container, "touchstart", [{ clientX: 10, clientY: 300 }]);
    dispatchTouchOn(container, "touchmove", [{ clientX: 140, clientY: 300 }]);
    dispatchTouchOn(container, "touchend", [], [{ clientX: 140, clientY: 300 }]);

    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
    spy.mockRestore();
  });
});
