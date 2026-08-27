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
