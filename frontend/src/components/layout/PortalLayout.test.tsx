import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import * as React from "react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useOutletContext } from "react-router-dom";
import { PortalLayout, type NavItem } from "./PortalLayout";

const NAV_ITEMS: NavItem[] = [
  { to: "overview", label: "Overview" },
  { to: "settings", label: "Settings" },
];

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

function renderPortal(initialPath = "/portal/overview") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route path="/portal" element={<PortalLayout title="Test Portal" backLink="/chat" navItems={NAV_ITEMS} context={null} />}>
          <Route path="overview" element={<div>overview content</div>} />
          <Route path="settings" element={<div>settings content</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("PortalLayout", () => {
  it("renders the title in the header", () => {
    renderPortal();
    expect(screen.getByText("Test Portal")).toBeInTheDocument();
  });

  it("renders a back link", () => {
    renderPortal();
    const link = screen.getByText(/Back to Chat/);
    expect(link).toHaveAttribute("href", "/chat");
  });

  it("renders all nav items", () => {
    renderPortal();
    expect(screen.getByText("Overview")).toBeInTheDocument();
    expect(screen.getByText("Settings")).toBeInTheDocument();
  });

  it("renders the outlet content for the active route", () => {
    renderPortal("/portal/overview");
    expect(screen.getByText("overview content")).toBeInTheDocument();
  });

  it("renders custom back label", () => {
    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/home" backLabel="Home" navItems={[]} context={null} />}>
            <Route path="overview" element={<div>content</div>} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByText("← Home")).toBeInTheDocument();
  });

  it("renders badges in the header when provided", () => {
    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/chat" navItems={[]} context={null} badges={<span data-testid="badge">PRO</span>} />}>
            <Route path="overview" element={<div>content</div>} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByTestId("badge")).toBeInTheDocument();
  });

  it("renders meta in the header when provided", () => {
    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/chat" navItems={[]} context={null} meta={<span data-testid="meta">5 users</span>} />}>
            <Route path="overview" element={<div>content</div>} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByTestId("meta")).toBeInTheDocument();
  });

  it("renders as full-screen without AppShell", () => {
    const { container } = renderPortal();
    const root = container.firstElementChild;
    expect(root).toHaveClass("h-dvh");
  });

  it("propagates the context prop to child routes via useOutletContext", () => {
    const ctx = { org: { id: "org-1" }, isAdmin: true };

    function ContextReader() {
      const c = useOutletContext<typeof ctx>();
      return <span data-testid="ctx">{JSON.stringify(c)}</span>;
    }

    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/chat" navItems={[]} context={ctx} />}>
            <Route path="overview" element={<ContextReader />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );

    const ctxEl = screen.getByTestId("ctx");
    expect(ctxEl.textContent).toContain("org-1");
    expect(ctxEl.textContent).toContain('"isAdmin":true');
  });

  it("renders a loading fallback in the content area while a lazy child suspends, keeping the portal nav visible", () => {
    // A child that suspends indefinitely — mirrors what React.lazy does
    // before its dynamic import resolves.
    function SuspendingChild(): React.ReactNode {
      throw new Promise(() => {});
    }

    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/chat" navItems={NAV_ITEMS} context={null} />}>
            <Route path="overview" element={<SuspendingChild />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );

    // The Suspense fallback (spinner, labelled "Loading") must be present
    // in the content area...
    expect(screen.getByLabelText("Loading")).toBeInTheDocument();
    // ...and the lazy child's never-rendered content must NOT be present.
    expect(screen.queryByText("overview content")).not.toBeInTheDocument();
    // The portal chrome (title + nav) must remain visible while the child
    // is suspended — the Suspense boundary is local to the <Outlet>, so a
    // suspending section never replaces the whole portal.
    expect(screen.getByText("T")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Settings" })).toBeInTheDocument();
  });

  it("renders a suspending child's content once the promise resolves", async () => {
    let resolveImport!: (mod: { default: React.ComponentType }) => void;
    const lazy = () => new Promise<{ default: React.ComponentType }>((resolve) => {
      resolveImport = resolve;
    });

    const LazyChild = React.lazy(lazy);

    render(
      <MemoryRouter initialEntries={["/portal/overview"]}>
        <Routes>
          <Route path="/portal" element={<PortalLayout title="T" backLink="/chat" navItems={[]} context={null} />}>
            <Route path="overview" element={<LazyChild />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );

    // Spinner shown while suspended.
    expect(screen.getByLabelText("Loading")).toBeInTheDocument();

    // Resolve the lazy import — the child content should appear.
    await act(async () => {
      resolveImport({ default: () => <span>lazy content</span> });
    });

    await waitFor(() => {
      expect(screen.getByText("lazy content")).toBeInTheDocument();
    });
    expect(screen.queryByLabelText("Loading")).not.toBeInTheDocument();
  });
});

describe("PortalLayout mobile drawer", () => {
  beforeEach(() => {
    setMobileMatchMedia(true);
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders a menu toggle button on mobile", () => {
    renderPortal();
    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
  });

  it("does not render a menu toggle on desktop", () => {
    setMobileMatchMedia(false);
    renderPortal();
    expect(screen.queryByRole("button", { name: "Open menu" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Close menu" })).not.toBeInTheDocument();
  });

  it("opens the drawer when the toggle is clicked", async () => {
    const user = userEvent.setup();
    renderPortal();
    const toggle = screen.getByRole("button", { name: "Open menu" });
    await user.click(toggle);
    expect(screen.getByRole("button", { name: "Close menu" })).toBeInTheDocument();
  });

  it("renders an overlay when the drawer is open on mobile", async () => {
    const user = userEvent.setup();
    const { container } = renderPortal();
    await user.click(screen.getByRole("button", { name: "Open menu" }));
    const overlay = container.querySelector('div[aria-hidden="true"]');
    expect(overlay).not.toBeNull();
    expect(overlay!.className).toContain("opacity-100");
  });

  it("closes the drawer when a nav item is clicked", async () => {
    const user = userEvent.setup();
    renderPortal();
    await user.click(screen.getByRole("button", { name: "Open menu" }));
    expect(screen.getByRole("button", { name: "Close menu" })).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Settings" }));
    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
  });

  it("closes the drawer when the overlay is clicked", async () => {
    const user = userEvent.setup();
    const { container } = renderPortal();
    await user.click(screen.getByRole("button", { name: "Open menu" }));
    const overlay = container.querySelector('div[aria-hidden="true"]') as HTMLElement;
    await user.click(overlay);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
    });
  });

  it("closes the drawer on navigation to another tab", async () => {
    const user = userEvent.setup();
    renderPortal();
    await user.click(screen.getByRole("button", { name: "Open menu" }));
    // Navigate via a tab link rendered directly (not through the drawer close handler path):
    // clicking the link triggers both the close handler and the route change.
    await user.click(screen.getByRole("link", { name: "Settings" }));
    expect(screen.getByRole("button", { name: "Open menu" })).toBeInTheDocument();
  });
});
