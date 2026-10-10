import { useEffect, useRef } from "react";
import { Outlet, useLocation, useMatches } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { SidebarDrawer } from "./SidebarDrawer";
import { SidebarToggleButton } from "./SidebarToggleButton";
import { useCollapsibleSidebar } from "../../hooks/useCollapsibleSidebar";
import { useOskViewportGuard } from "../../hooks/useOskViewportGuard";
import { SessionActivityProvider } from "../../providers/SessionActivityProvider";

export function AppShell() {
  const mainRef = useRef<HTMLElement>(null);
  const sidebar = useCollapsibleSidebar();
  const location = useLocation();
  const matches = useMatches();
  const isInitialMount = useRef(true);
  useOskViewportGuard();

  useEffect(() => {
    if (isInitialMount.current) {
      isInitialMount.current = false;
      const hasSession = matches.some((m) => m.params?.sessionId);
      if (sidebar.isMobile && !hasSession) {
        sidebar.setOpen(true);
      }
    }
    mainRef.current?.focus();
    // matches (useMatches) and the sidebar state object are unstable across renders;
    // this effect must only re-run on pathname change, so deps are intentionally minimal.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.pathname]);

  return (
    <SessionActivityProvider>
      <div
        ref={sidebar.containerRef}
        // #1648: h-dvh (100dvh) is the DYNAMIC viewport — resizes as browser
        // chrome collapses/expands. h-screen (100vh, the LARGE viewport) made
        // this overflow-hidden shell taller than the visible screen with
        // viewport-fit=cover + safe-area insets (#1626): header OR composer
        // permanently off-screen on mobile. dvh does NOT cover the on-screen
        // keyboard — that lane is interactive-widget=resizes-content
        // (index.html) + the useOskViewportGuard override.
        className="flex h-dvh overflow-hidden overscroll-none"
        style={{ touchAction: "pan-y" }}
      >
        <a
          href="#main-content"
          className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-background focus:p-2 focus:text-foreground"
        >
          Skip to content
        </a>

        <SidebarDrawer state={sidebar}>
          <Sidebar onNavigate={() => sidebar.close()} />
        </SidebarDrawer>

        <div className="flex flex-1 flex-col overflow-hidden">
          {sidebar.isMobile && (
            // The top padding COMPOSES the base with the safe-area inset
            // (calc, not replace): viewport-fit=cover extends the page
            // under the notch; this header is the topmost content row.
            <div className="flex items-center border-b border-border px-3 pb-2 pt-[calc(0.5rem+env(safe-area-inset-top,0px))]">
              <SidebarToggleButton open={sidebar.open} onClick={() => sidebar.setOpen(!sidebar.open)} />
              <span className="ml-2 text-sm font-semibold">Safe Space</span>
            </div>
          )}

          <main
            id="main-content"
            ref={mainRef}
            className="flex-1 overflow-hidden"
            tabIndex={-1}
            aria-label="Main content"
          >
            <Outlet />
          </main>
        </div>
      </div>
    </SessionActivityProvider>
  );
}
