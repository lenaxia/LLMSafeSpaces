import type { ReactNode } from "react";
import { Suspense } from "react";
import { Link, NavLink, Outlet } from "react-router-dom";
import { SidebarDrawer } from "./SidebarDrawer";
import { SidebarToggleButton } from "./SidebarToggleButton";
import { Spinner } from "../ui/Spinner";
import { useCollapsibleSidebar } from "../../hooks/useCollapsibleSidebar";

export interface NavItem {
  to: string;
  label: string;
}

export interface PortalLayoutProps {
  title: string;
  backLink: string;
  backLabel?: string;
  badges?: ReactNode;
  meta?: ReactNode;
  navItems: NavItem[];
  context: unknown;
}

const PORTAL_NAV_WIDTH = 192;

export function PortalLayout({
  title,
  backLink,
  backLabel = "Back to Chat",
  badges,
  meta,
  navItems,
  context,
}: PortalLayoutProps) {
  const sidebar = useCollapsibleSidebar({ sidebarWidth: PORTAL_NAV_WIDTH });

  return (
    <div
      ref={sidebar.containerRef}
      className="flex h-dvh flex-col bg-background overflow-hidden overscroll-none"
      style={{ touchAction: "pan-y" }}
    >
      {/* Top padding composes the base with the safe-area inset: viewport-fit=cover extends under the notch — the header is the topmost row (#1623). */}
      <header className="flex items-center justify-between border-b border-border px-6 pb-3 pt-[calc(0.75rem+env(safe-area-inset-top,0px))]">
        <div className="flex items-center gap-3">
          {sidebar.isMobile && (
            <SidebarToggleButton open={sidebar.open} onClick={() => sidebar.setOpen(!sidebar.open)} />
          )}
          <Link
            to={backLink}
            className="text-sm text-muted-foreground hover:text-foreground"
          >
            ← {backLabel}
          </Link>
          <span className="text-border">|</span>
          <h1 className="text-lg font-semibold">{title}</h1>
          {badges}
        </div>
        {meta && <div className="text-xs text-muted-foreground">{meta}</div>}
      </header>

      <div className="flex flex-1 overflow-hidden">
        <SidebarDrawer state={sidebar} ariaLabel="Sections" desktopClassName="relative w-48 shrink-0">
          <nav className="h-full w-full border-r border-border bg-card py-2">
            {navItems.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                onClick={() => sidebar.close()}
                className={({ isActive }) =>
                  `block px-4 py-2 text-sm ${
                    isActive
                      ? "bg-accent/10 font-medium text-accent"
                      : "text-muted-foreground hover:bg-muted hover:text-foreground"
                  }`
                }
              >
                {item.label}
              </NavLink>
            ))}
          </nav>
        </SidebarDrawer>

        <main className="flex-1 overflow-y-auto p-6">
          <Suspense
            fallback={
              <div className="flex h-full items-center justify-center">
                <Spinner size="lg" />
              </div>
            }
          >
            <Outlet context={context} />
          </Suspense>
        </main>
      </div>
    </div>
  );
}
